package application_test

// Acceptance tests for V2-069, the Runner version report.
//
// Determinism is acceptance here: there is no fixed sleep, no wall-clock
// timer and no goroutine anywhere in this file. Every instant comes from an
// injected clock, and the one "retry" is a deterministic re-invocation of the
// transaction callback rather than a real contention race.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

const (
	testDigestA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testDigestB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// countingClock advances one second per call and records how many times it
// was consulted. An advancing clock is what makes "the stored instant is the
// transaction's authority time, captured once" a falsifiable claim: a second
// read of the clock would produce a different value.
type countingClock struct {
	mu    sync.Mutex
	base  time.Time
	calls int
}

func (c *countingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.base.Add(time.Duration(c.calls-1) * time.Second)
}
func (c *countingClock) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fixedClock is a frozen instant that can be moved forward by the test
// itself, which is how the staleness window is crossed without waiting.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fixedClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func runnerService(t *testing.T, clk application.Clock) (*application.Service, *memory.Store) {
	t.Helper()
	st := memory.New()
	s, err := application.NewServiceWithConfig(st, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func storedReport(t *testing.T, st *memory.Store, runnerID string) (application.RunnerVersionReport, bool) {
	t.Helper()
	var out application.RunnerVersionReport
	var found bool
	ctx := context.Background()
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		v, ok, e := u.RunnerVersionReport(ctx, runnerID)
		out, found = v, ok
		return e
	}); err != nil {
		t.Fatal(err)
	}
	return out, found
}

func enumeratedReports(t *testing.T, st *memory.Store, limit int) ([]application.RunnerVersionReport, bool) {
	t.Helper()
	var rows []application.RunnerVersionReport
	var truncated bool
	ctx := context.Background()
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		v, more, e := u.RunnerVersionReports(ctx, limit)
		rows, truncated = v, more
		return e
	}); err != nil {
		t.Fatal(err)
	}
	return rows, truncated
}

func validInput() *application.RunnerVersionInput {
	return &application.RunnerVersionInput{Version: "1.2.3", BinarySHA256: testDigestA, SchemaMin: 2, SchemaMax: 7}
}

// ===========================================================================
// A3 / A11 (d11): the shape is closed and minimal
// ===========================================================================

// credentialDenyList is copied by value from internal/domain's
// source_guard_test.go. internal/domain's test is not imported: a test in one
// package cannot import another package's test file, and copying the list is
// what the Work Order asks for. The bare word "token" is deliberately absent
// for the same reason it is absent there: FencingToken and its relatives are
// legitimate non-secret concepts.
var credentialDenyList = []string{
	"password",
	"passwd",
	"credential",
	"credentials",
	"secret",
	"apikey",
	"privatekey",
	"accesstoken",
	"refreshtoken",
	"bearer",
	"sessioncookie",
	"authorization",
	"rawprompt",
	"rawproviderout",
}

// forbiddenReportFieldTokens is the second deny list: field names that would
// widen the report past the version-and-compatibility question, or would let
// a Runner clock or a machine identifier beyond the authenticated RunnerID
// into the record.
var forbiddenReportFieldTokens = []string{
	"message", "detail", "output", "text", "hostname", "host", "ip", "path", "root", "env", "timestamp",
}

func normalizeFieldName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", ""))
}

func matchesAny(name string, list []string) (string, bool) {
	normalized := normalizeFieldName(name)
	for _, entry := range list {
		if strings.Contains(normalized, entry) {
			return entry, true
		}
	}
	return "", false
}

// TestRunnerReportFieldMatchersAreVerified checks both matchers against a
// known-positive and a known-negative before the scan below trusts either.
func TestRunnerReportFieldMatchersAreVerified(t *testing.T) {
	for _, positive := range []string{"Password", "APIKey", "ClientSecret", "RawPrompt", "raw_provider_output"} {
		if _, ok := matchesAny(positive, credentialDenyList); !ok {
			t.Fatalf("known-positive %q did not match the credential deny list", positive)
		}
	}
	for _, negative := range []string{"FencingToken", "BinarySHA256", "SchemaMin", "ReportedAt"} {
		if entry, ok := matchesAny(negative, credentialDenyList); ok {
			t.Fatalf("known-negative %q matched the credential deny list on %q", negative, entry)
		}
	}
	for _, positive := range []string{"Hostname", "Message", "InstallPath", "RawOutput", "reported_timestamp", "Env", "IP"} {
		if _, ok := matchesAny(positive, forbiddenReportFieldTokens); !ok {
			t.Fatalf("known-positive %q did not match the forbidden report field list", positive)
		}
	}
	for _, negative := range []string{"Version", "BinarySHA256", "SchemaMin", "SchemaMax", "ReportedAt", "RunnerID", "ReportState", "IntersectionState", "Truncated", "RunnerCount"} {
		if entry, ok := matchesAny(negative, forbiddenReportFieldTokens); ok {
			t.Fatalf("known-negative %q matched the forbidden report field list on %q", negative, entry)
		}
	}
}

// applicationPackageFiles parses every .go file in this package directory,
// test files included, and fails outright on a zero-file scan so no assertion
// below can pass vacuously.
func applicationPackageFiles(t *testing.T, includeTests bool) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if !includeTests && strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		out[e.Name()] = file
	}
	if len(out) == 0 {
		t.Fatal("scanned zero .go files; the working directory is not internal/application or the scan is broken")
	}
	return out
}

// structFieldNames returns the declared Go field names and json tag names of
// the named struct types, and reports which names it did not find.
func structFieldNames(t *testing.T, files map[string]*ast.File, want []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, target := range want {
				if spec.Name.Name != target {
					continue
				}
				names := []string{}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						names = append(names, name.Name)
					}
					if field.Tag != nil {
						tag, err := strconv.Unquote(field.Tag.Value)
						if err == nil {
							if json := jsonTagFieldName(tag); json != "" {
								names = append(names, json)
							}
						}
					}
				}
				out[target] = names
			}
			return true
		})
	}
	return out
}

func jsonTagFieldName(tag string) string {
	const key = `json:"`
	i := strings.Index(tag, key)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""

	}
	value := rest[:j]
	if k := strings.Index(value, ","); k >= 0 {
		value = value[:k]
	}
	if value == "-" {
		return ""
	}
	return value
}

// TestRunnerVersionShapeIsClosedAndMinimal is A3: the request DTO, the stored
// record and the two read-model types carry the four reported coordinates,
// the already-authenticated RunnerID, reported_at, the two closed enums and
// the enumeration bound, and nothing else. Every field name and every json
// tag is checked against both deny lists.
func TestRunnerVersionShapeIsClosedAndMinimal(t *testing.T) {
	files := applicationPackageFiles(t, false)
	targets := []string{"RunnerVersionInput", "RunnerVersionReport", "RunnerVersionView", "RunnerVersionListView"}
	found := structFieldNames(t, files, targets)
	for _, target := range targets {
		names, ok := found[target]
		if !ok {
			t.Fatalf("the AST scan did not find type %s; the scan is broken or the type was renamed", target)
		}
		if len(names) == 0 {
			t.Fatalf("%s has no fields; the scan is broken", target)
		}
		for _, name := range names {
			if entry, bad := matchesAny(name, credentialDenyList); bad {
				t.Fatalf("%s.%s matches the credential deny list on %q", target, name, entry)
			}
			if entry, bad := matchesAny(name, forbiddenReportFieldTokens); bad {
				t.Fatalf("%s.%s matches the forbidden report field list on %q", target, name, entry)
			}
		}
	}
	// The request DTO carries exactly the four reported coordinates, and in
	// particular no timestamp field of any spelling.
	if got := found["RunnerVersionInput"]; !reflect.DeepEqual(got, []string{"Version", "BinarySHA256", "SchemaMin", "SchemaMax"}) {
		t.Fatalf("RunnerVersionInput fields = %v, want exactly the four reported coordinates", got)
	}
	// No contract, bundle or provenance coordinate appears anywhere, not even
	// as an optional placeholder.
	for _, forbidden := range []string{"contract_release", "contract_digest", "runner_api_min", "runner_api_max", "bundle_digest", "candidate_id", "key_id", "algorithm", "ContractRelease", "ContractDigest", "RunnerAPIMin", "RunnerAPIMax", "BundleDigest", "CandidateID", "SigningKeyID", "Algorithm"} {
		for _, target := range targets {
			for _, name := range found[target] {
				if name == forbidden {
					t.Fatalf("%s declares %q; no code in this repository fills it from a Runner", target, forbidden)
				}
			}
		}
	}
	t.Logf("scanned %d files; %s", len(files), found)
}

// ===========================================================================
// A12: the two shapes are pinned without an import
// ===========================================================================

// TestApplicationImportsNeitherUpdateNorScheduler is A12 and A16: the semver
// and digest shapes are re-declared as literals precisely so that no
// application-to-update edge exists, in production or in test code.
func TestApplicationImportsNeitherUpdateNorScheduler(t *testing.T) {
	const prefix = "github.com/takushi/agentic-loop-foundation/v2/internal/"
	forbidden := []string{prefix + "update", prefix + "scheduler"}
	files := applicationPackageFiles(t, true)
	total := 0
	for name, file := range files {
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			total++
			for _, bad := range forbidden {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports %q; only V2-045 may declare that component edge", name, path)
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("scanned zero import paths across the package's files")
	}
	t.Logf("import scan covered %d files and %d import paths", len(files), total)
}

// TestRunnerVersionShapesArePinnedToTheUpdatePatterns is the mitigation named
// in dp-v2-069 d12: because internal/application must not import
// internal/update, the semver and 64-hex shapes are re-declared here, the two
// declarations can drift, and this table is the only thing that will catch
// it. Every verdict below is the verdict internal/update/update.go:64 and :65
// give for the same literal.
func TestRunnerVersionShapesArePinnedToTheUpdatePatterns(t *testing.T) {
	versionAccepted := []string{"0.1.0", "1.2.3", "10.20.30", "0.1.0-dev", "1.0.0-rc.1", "2.0.0-alpha-3"}
	versionRejected := []string{"", "1.2", "v1.2.3", "1.2.3-RC1", "1.2.3.4", "1.2.3-", " 1.2.3", "1.2.3 ", "01.2.3-A"}
	for _, ok := range versionAccepted {
		if err := (application.RunnerVersionInput{Version: ok, BinarySHA256: testDigestA, SchemaMin: 1, SchemaMax: 1}).Validate(); err != nil {
			t.Errorf("version %q was rejected: %v", ok, err)
		}
	}
	for _, bad := range versionRejected {
		if err := (application.RunnerVersionInput{Version: bad, BinarySHA256: testDigestA, SchemaMin: 1, SchemaMax: 1}).Validate(); err == nil {
			t.Errorf("version %q was accepted", bad)
		}
	}
	digestAccepted := []string{testDigestA, testDigestB, strings.Repeat("0", 64), strings.Repeat("f", 64)}
	digestRejected := []string{
		"",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.ToUpper(testDigestA),
		strings.Repeat("g", 64),
		"sha256:" + testDigestA,
	}
	for _, ok := range digestAccepted {
		if err := (application.RunnerVersionInput{Version: "1.2.3", BinarySHA256: ok, SchemaMin: 1, SchemaMax: 1}).Validate(); err != nil {
			t.Errorf("digest %q was rejected: %v", ok, err)
		}
	}
	for _, bad := range digestRejected {
		if err := (application.RunnerVersionInput{Version: "1.2.3", BinarySHA256: bad, SchemaMin: 1, SchemaMax: 1}).Validate(); err == nil {
			t.Errorf("digest %q was accepted", bad)
		}
	}
	t.Logf("pinned %d accepted and %d rejected version literals, %d accepted and %d rejected digest literals",
		len(versionAccepted), len(versionRejected), len(digestAccepted), len(digestRejected))
}

// TestRunnerVersionIntervalValidationIsShapeOnly completes A4's shape table
// on the interval endpoints.
func TestRunnerVersionIntervalValidationIsShapeOnly(t *testing.T) {
	cases := []struct {
		name     string
		min, max int
		accept   bool
	}{
		{"the smallest legal interval", 1, 1, true},
		{"a wide legal interval", 1, application.MaxRunnerSchemaBound, true},
		{"schema_min at the ceiling", application.MaxRunnerSchemaBound, application.MaxRunnerSchemaBound, true},
		{"schema_min below one", 0, 4, false},
		{"a negative schema_min", -1, 4, false},
		{"schema_max below schema_min", 5, 4, false},
		{"schema_max above the ceiling", 1, application.MaxRunnerSchemaBound + 1, false},
		{"schema_min above the ceiling", application.MaxRunnerSchemaBound + 1, application.MaxRunnerSchemaBound + 1, false},
	}
	for _, c := range cases {
		err := (application.RunnerVersionInput{Version: "1.2.3", BinarySHA256: testDigestA, SchemaMin: c.min, SchemaMax: c.max}).Validate()
		if c.accept && err != nil {
			t.Errorf("%s: [%d,%d] was rejected: %v", c.name, c.min, c.max, err)
		}
		if !c.accept && err == nil {
			t.Errorf("%s: [%d,%d] was accepted", c.name, c.min, c.max)
		}
		if !c.accept && err != nil && !errors.Is(err, application.ErrRunnerVersionReportInvalid) {
			t.Errorf("%s: refusal is not a shape refusal: %v", c.name, err)
		}
	}
}

// ===========================================================================
// A6(g): both enums are closed in both directions
// ===========================================================================

// enumConstantsAndCases parses runner_version.go and returns, for the named
// type, the constant names declared with that type and the identifier names
// appearing as case values in that type's Valid() switch.
func enumConstantsAndCases(t *testing.T, typeName string) (constants, cases []string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runner_version.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse runner_version.go: %v", err)
	}
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || value.Type == nil {
					continue
				}
				ident, ok := value.Type.(*ast.Ident)
				if !ok || ident.Name != typeName {
					continue
				}
				for _, name := range value.Names {
					constants = append(constants, name.Name)
				}
			}
			continue
		}
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Valid" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recv, ok := fn.Recv.List[0].Type.(*ast.Ident)
		if !ok || recv.Name != typeName {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if ident, ok := expr.(*ast.Ident); ok {
					cases = append(cases, ident.Name)
				}
			}
			return true
		})
	}
	sort.Strings(constants)
	sort.Strings(cases)
	return constants, cases
}

// TestRunnerEnumsAreClosed is A6(g). It fails if a constant is added without a
// case, and it fails if a case is added without a constant, for report_state
// and intersection_state both. The matcher is verified first against a
// synthetic type whose switch is deliberately missing one case.
func TestRunnerEnumsAreClosed(t *testing.T) {
	src := `package application
type Synthetic string
const (
	SyntheticA Synthetic = "a"
	SyntheticB Synthetic = "b"
)
func (s Synthetic) Valid() bool {
	switch s {
	case SyntheticA:
		return true
	}
	return false
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var syntheticConstants, syntheticCases []string
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if ident, ok := value.Type.(*ast.Ident); ok && ident.Name == "Synthetic" {
					for _, name := range value.Names {
						syntheticConstants = append(syntheticConstants, name.Name)
					}
				}
			}
		}
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Valid" {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if clause, ok := n.(*ast.CaseClause); ok {
					for _, expr := range clause.List {
						if ident, ok := expr.(*ast.Ident); ok {
							syntheticCases = append(syntheticCases, ident.Name)
						}
					}
				}
				return true
			})
		}
	}
	if len(syntheticConstants) != 2 || len(syntheticCases) != 1 {
		t.Fatalf("positive control failed to parse: constants=%v cases=%v", syntheticConstants, syntheticCases)
	}

	for _, typeName := range []string{"RunnerReportState", "SchemaIntersectionState"} {
		constants, cases := enumConstantsAndCases(t, typeName)
		if len(constants) == 0 || len(cases) == 0 {
			t.Fatalf("%s: constants=%v cases=%v; the scan found nothing and would pass vacuously", typeName, constants, cases)
		}
		if !reflect.DeepEqual(constants, cases) {
			t.Fatalf("%s: constants %v and Valid() cases %v are not the same set", typeName, constants, cases)
		}
	}
	// And the runtime side agrees: every declared value is Valid and nothing
	// else is.
	for _, v := range []application.RunnerReportState{application.RunnerReportReported, application.RunnerReportNotReported, application.RunnerReportStale} {
		if !v.Valid() {
			t.Fatalf("report state %q is declared but not Valid", v)
		}
	}
	for _, v := range []application.RunnerReportState{"", "compatible", "ok", "healthy", "valid"} {
		if v.Valid() {
			t.Fatalf("report state %q is Valid but not declared", v)
		}
	}
	for _, v := range []application.SchemaIntersectionState{application.SchemaIntersectionNonEmpty, application.SchemaIntersectionEmpty, application.SchemaIntersectionUnknown} {
		if !v.Valid() {
			t.Fatalf("intersection state %q is declared but not Valid", v)
		}
	}
	for _, v := range []application.SchemaIntersectionState{"", "compatible", "ok", "healthy", "valid"} {
		if v.Valid() {
			t.Fatalf("intersection state %q is Valid but not declared", v)
		}
	}
}

// ===========================================================================
// A5: the Runner clock cannot enter the record
// ===========================================================================

// TestReportedAtIsTheTransactionAuthorityTime is A5: the stored reported_at is
// byte-identical to the At of the event this same heartbeat recorded, and it
// is compared against that event rather than against a fresh clock read.
func TestReportedAtIsTheTransactionAuthorityTime(t *testing.T) {
	clk := &countingClock{base: time.Unix(1700000000, 0).UTC()}
	s, st := runnerService(t, clk)
	ctx := runner(context.Background(), "runner-1")
	if _, err := s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-1", RunnerVersion: validInput()}); err != nil {
		t.Fatal(err)
	}
	report, ok := storedReport(t, st, "runner-1")
	if !ok {
		t.Fatal("no report was stored")
	}
	events := st.Events()
	if len(events) != 1 {
		t.Fatalf("events=%d, want exactly the one heartbeat event", len(events))
	}
	want := events[0].At
	if !report.ReportedAt.Equal(want) {
		t.Fatalf("reported_at %s is not the recorded event's At %s", report.ReportedAt, want)
	}
	if report.ReportedAt.Format(time.RFC3339Nano) != want.Format(time.RFC3339Nano) {
		t.Fatalf("reported_at %q and the event's At %q are not byte-identical",
			report.ReportedAt.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
	// The clock advances on every call, so a value taken from any other read
	// of the clock would differ from the event's At. That is what makes the
	// equality above meaningful rather than incidental.
	if clk.Calls() < 2 {
		t.Fatalf("the clock was consulted %d times; the advancing-clock control is not in force", clk.Calls())
	}
}

// replayTransactor re-invokes the transaction callback the way Firestore
// re-invokes it after an aborted attempt: the first attempt's staged writes
// are discarded and the callback runs again on a fresh unit of work with the
// same context, and therefore the same authority time. It is deterministic --
// no goroutine, no contention, no sleep.
type replayTransactor struct {
	inner    application.Transactor
	attempts int
	calls    int
}

var errDiscardedAttempt = errors.New("discarded transaction attempt")

func (t *replayTransactor) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	t.calls++
	for i := 1; i < t.attempts; i++ {
		if err := t.inner.Transact(ctx, func(u application.UnitOfWork) error {
			if e := fn(u); e != nil {
				return e
			}
			return errDiscardedAttempt
		}); err != nil && !errors.Is(err, errDiscardedAttempt) {
			return err
		}
	}
	return t.inner.Transact(ctx, fn)
}

// TestReportedAtIsStableAcrossATransactionRetry is A5's retry half, driven
// deterministically at the application boundary. The real Firestore
// contention retry cannot be forced without a concurrent writer, which A15
// forbids; the property under test is the one that matters and is the one the
// adapter inherits -- the authority time is captured once, before the
// callback, so re-running the callback cannot change it.
func TestReportedAtIsStableAcrossATransactionRetry(t *testing.T) {
	clk := &countingClock{base: time.Unix(1700000000, 0).UTC()}
	st := memory.New()
	tx := &replayTransactor{inner: st, attempts: 3}
	s, err := application.NewServiceWithConfig(tx, clk, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	before := clk.Calls()
	if _, err := s.Heartbeat(runner(context.Background(), "runner-1"), application.HeartbeatRequest{RequestID: "hb-retry", RunnerVersion: validInput()}); err != nil {
		t.Fatal(err)
	}
	// Exactly two clock reads for one Heartbeat, whatever the attempt count:
	// one for the request's own authority instant and one for the
	// transaction's. A third would mean an attempt re-read the clock.
	if got := clk.Calls() - before; got != 2 {
		t.Fatalf("the clock was consulted %d times across %d attempts, want 2", got, tx.attempts)
	}
	report, ok := storedReport(t, st, "runner-1")
	if !ok {
		t.Fatal("no report survived the retried transaction")
	}
	events := st.Events()
	if len(events) != 1 {
		t.Fatalf("events=%d, want exactly one; a discarded attempt leaked", len(events))
	}
	if !report.ReportedAt.Equal(events[0].At) {
		t.Fatalf("reported_at %s is not the surviving event's At %s", report.ReportedAt, events[0].At)
	}
}

// ===========================================================================
// A4: a partial object is refused and stores nothing
// ===========================================================================

// TestPartialRunnerVersionObjectIsRefusedAndStoresNothing is the load-bearing
// half of A4. In particular an object carrying schema_max with no schema_min
// must be refused rather than stored as an interval starting at zero.
func TestPartialRunnerVersionObjectIsRefusedAndStoresNothing(t *testing.T) {
	full := *validInput()
	subsets := []struct {
		name  string
		input application.RunnerVersionInput
	}{
		{"schema_max alone", application.RunnerVersionInput{SchemaMax: 7}},
		{"schema_min alone", application.RunnerVersionInput{SchemaMin: 2}},
		{"version alone", application.RunnerVersionInput{Version: full.Version}},
		{"digest alone", application.RunnerVersionInput{BinarySHA256: full.BinarySHA256}},
		{"the interval without the binary", application.RunnerVersionInput{SchemaMin: 2, SchemaMax: 7}},
		{"the binary without the interval", application.RunnerVersionInput{Version: full.Version, BinarySHA256: full.BinarySHA256}},
		{"everything but schema_min", application.RunnerVersionInput{Version: full.Version, BinarySHA256: full.BinarySHA256, SchemaMax: 7}},
		{"everything but the digest", application.RunnerVersionInput{Version: full.Version, SchemaMin: 2, SchemaMax: 7}},
		{"the wholly empty object", application.RunnerVersionInput{}},
	}
	for _, c := range subsets {
		s, st := runnerService(t, clock{})
		input := c.input
		_, err := s.Heartbeat(runner(context.Background(), "runner-1"), application.HeartbeatRequest{RequestID: "hb", RunnerVersion: &input})
		if err == nil {
			t.Fatalf("%s: a partial report was accepted", c.name)
		}
		if !errors.Is(err, application.ErrRunnerVersionReportInvalid) {
			t.Fatalf("%s: refusal is not a shape refusal: %v", c.name, err)
		}
		if report, ok := storedReport(t, st, "runner-1"); ok {
			t.Fatalf("%s: a refused report was stored as %#v", c.name, report)
		}
		if rows, _ := enumeratedReports(t, st, application.MaxRunnerVersionReports); len(rows) != 0 {
			t.Fatalf("%s: the refused heartbeat left %d enumerated rows", c.name, len(rows))
		}
		if len(st.Events()) != 0 || len(st.Outbox()) != 0 {
			t.Fatalf("%s: the refused heartbeat recorded events=%d outbox=%d", c.name, len(st.Events()), len(st.Outbox()))
		}
	}
	t.Logf("refused %d strict subsets of the four-field object, each storing nothing", len(subsets))
}

// TestHeartbeatWithoutAReportStoresNothingAndErasesNothing is the other half
// of A4 and the reason the report is a side table rather than a field on
// domain.RunnerObservation: a heartbeat that omits the object must not
// overwrite a previously reported version with zero values.
func TestHeartbeatWithoutAReportStoresNothingAndErasesNothing(t *testing.T) {
	s, st := runnerService(t, clock{})
	ctx := runner(context.Background(), "runner-1")

	// A report-less heartbeat from a Runner that has never reported stores no
	// report at all.
	if _, err := s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-1"}); err != nil {
		t.Fatal(err)
	}
	if report, ok := storedReport(t, st, "runner-1"); ok {
		t.Fatalf("a heartbeat carrying no runner_version stored %#v", report)
	}
	// The Runner is nevertheless known, and enumerates as not-reported.
	rows, truncated := enumeratedReports(t, st, application.MaxRunnerVersionReports)
	if truncated || len(rows) != 1 || rows[0].RunnerID != "runner-1" || rows[0].Reported() {
		t.Fatalf("enumeration = %#v truncated=%v", rows, truncated)
	}

	// Now it reports, and then heartbeats again without reporting.
	if _, err := s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-2", RunnerVersion: validInput()}); err != nil {
		t.Fatal(err)
	}
	reported, ok := storedReport(t, st, "runner-1")
	if !ok {
		t.Fatal("the report was not stored")
	}
	if _, err := s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-3"}); err != nil {
		t.Fatal(err)
	}
	after, ok := storedReport(t, st, "runner-1")
	if !ok {
		t.Fatal("a report-less heartbeat erased the stored report")
	}
	if after != reported {
		t.Fatalf("a report-less heartbeat rewrote the report: %#v then %#v", reported, after)
	}
}

// ===========================================================================
// A6: three states, no default interval, explicit unknown
// ===========================================================================

func marshalView(t *testing.T, v application.RunnerVersionListView) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func runnerRows(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	raw, ok := doc["runners"].([]any)
	if !ok {
		t.Fatalf("the marshalled view has no runners array: %v", doc)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("runner row is not an object: %v", r)
		}
		out = append(out, row)
	}
	return out
}

// TestRunnerViewHasThreeStatesAndNoDefaultInterval is A6(a), (b) and (c).
func TestRunnerViewHasThreeStatesAndNoDefaultInterval(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	clk := &fixedClock{now: base}
	s, st := runnerService(t, clk)
	owned := owner(context.Background())

	// (a) a Runner with no report at all.
	if _, err := s.Heartbeat(runner(context.Background(), "runner-silent"), application.HeartbeatRequest{RequestID: "hb-silent"}); err != nil {
		t.Fatal(err)
	}
	doc := marshalView(t, mustRunners(t, s, owned))
	rows := runnerRows(t, doc)
	if len(rows) != 1 || rows[0]["report_state"] != string(application.RunnerReportNotReported) {
		t.Fatalf("row = %v", rows)
	}
	for _, absent := range []string{"version", "binary_sha256", "schema_min", "schema_max", "reported_at"} {
		if _, present := rows[0][absent]; present {
			t.Fatalf("a not-reported row carries %q = %v; there must be no interval and no coordinate at all", absent, rows[0][absent])
		}
	}
	if doc["intersection_state"] != string(application.SchemaIntersectionUnknown) {
		t.Fatalf("intersection_state = %v with an unreported Runner", doc["intersection_state"])
	}

	// (b) one valid report, echoed exactly.
	if _, err := s.Heartbeat(runner(context.Background(), "runner-silent"), application.HeartbeatRequest{RequestID: "hb-report", RunnerVersion: validInput()}); err != nil {
		t.Fatal(err)
	}
	rows = runnerRows(t, marshalView(t, mustRunners(t, s, owned)))
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0]["report_state"] != string(application.RunnerReportReported) ||
		rows[0]["version"] != "1.2.3" ||
		rows[0]["binary_sha256"] != testDigestA ||
		rows[0]["schema_min"] != float64(2) ||
		rows[0]["schema_max"] != float64(7) {
		t.Fatalf("the four coordinates were not echoed exactly: %v", rows[0])
	}
	reportedAt, _ := rows[0]["reported_at"].(string)
	if reportedAt == "" {
		t.Fatalf("a reported row carries no reported_at: %v", rows[0])
	}

	// (c) the same report, read after the staleness window: stale, with the
	// previously reported coordinates preserved rather than refreshed or
	// dropped.
	clk.Set(base.Add(application.RunnerVersionReportStaleAfter + time.Second))
	stale := runnerRows(t, marshalView(t, mustRunners(t, s, owned)))
	if len(stale) != 1 || stale[0]["report_state"] != string(application.RunnerReportStale) {
		t.Fatalf("row after the staleness window = %v", stale)
	}
	for _, key := range []string{"version", "binary_sha256", "schema_min", "schema_max", "reported_at"} {
		if stale[0][key] != rows[0][key] {
			t.Fatalf("stale row changed %q from %v to %v", key, rows[0][key], stale[0][key])
		}
	}
	// A report exactly at the boundary is not yet stale.
	clk.Set(base.Add(application.RunnerVersionReportStaleAfter))
	edge := runnerRows(t, marshalView(t, mustRunners(t, s, owned)))
	if edge[0]["report_state"] != string(application.RunnerReportReported) {
		t.Fatalf("the boundary instant is reported as %v", edge[0]["report_state"])
	}
	_ = st
}

func mustRunners(t *testing.T, s *application.Service, ctx context.Context) application.RunnerVersionListView {
	t.Helper()
	out, err := s.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSchemaIntersectionStates is A6(d), (e) and (f).
func TestSchemaIntersectionStates(t *testing.T) {
	report := func(min, max int) *application.RunnerVersionInput {
		return &application.RunnerVersionInput{Version: "1.2.3", BinarySHA256: testDigestA, SchemaMin: min, SchemaMax: max}
	}
	cases := []struct {
		name      string
		reports   map[string]*application.RunnerVersionInput
		wantState application.SchemaIntersectionState
		wantMin   int
		wantMax   int
	}{
		{
			name:      "no Runner is known at all",
			reports:   map[string]*application.RunnerVersionInput{},
			wantState: application.SchemaIntersectionUnknown,
		},
		{
			name:      "exactly one of two Runners has reported",
			reports:   map[string]*application.RunnerVersionInput{"runner-a": report(2, 7), "runner-b": nil},
			wantState: application.SchemaIntersectionUnknown,
		},
		{
			name:      "two Runners report overlapping intervals",
			reports:   map[string]*application.RunnerVersionInput{"runner-a": report(2, 7), "runner-b": report(4, 9)},
			wantState: application.SchemaIntersectionNonEmpty,
			wantMin:   4,
			wantMax:   7,
		},
		{
			name:      "two Runners report disjoint intervals",
			reports:   map[string]*application.RunnerVersionInput{"runner-a": report(1, 3), "runner-b": report(5, 9)},
			wantState: application.SchemaIntersectionEmpty,
		},
		{
			name:      "one Runner reports",
			reports:   map[string]*application.RunnerVersionInput{"runner-a": report(3, 3)},
			wantState: application.SchemaIntersectionNonEmpty,
			wantMin:   3,
			wantMax:   3,
		},
	}
	for _, c := range cases {
		s, _ := runnerService(t, clock{})
		names := make([]string, 0, len(c.reports))
		for id := range c.reports {
			names = append(names, id)
		}
		sort.Strings(names)
		for i, id := range names {
			if _, err := s.Heartbeat(runner(context.Background(), id), application.HeartbeatRequest{RequestID: fmt.Sprintf("hb-%s-%d", id, i), RunnerVersion: c.reports[id]}); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		}
		view := mustRunners(t, s, owner(context.Background()))
		if view.IntersectionState != c.wantState {
			t.Fatalf("%s: intersection_state = %q, want %q", c.name, view.IntersectionState, c.wantState)
		}
		doc := marshalView(t, view)
		if c.wantState != application.SchemaIntersectionNonEmpty {
			for _, key := range []string{"intersection_schema_min", "intersection_schema_max"} {
				if _, present := doc[key]; present {
					t.Fatalf("%s: a %s intersection carries %q; only a non-empty intersection is an interval", c.name, c.wantState, key)
				}
			}
			continue
		}
		if doc["intersection_schema_min"] != float64(c.wantMin) || doc["intersection_schema_max"] != float64(c.wantMax) {
			t.Fatalf("%s: endpoints = %v,%v want %d,%d (max of minima, min of maxima)",
				c.name, doc["intersection_schema_min"], doc["intersection_schema_max"], c.wantMin, c.wantMax)
		}
	}
}

// TestStaleReportMakesTheIntersectionUnknown: a Runner that reported and then
// went quiet is not in state reported, so the intersection cannot be stated.
func TestStaleReportMakesTheIntersectionUnknown(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	clk := &fixedClock{now: base}
	s, _ := runnerService(t, clk)
	if _, err := s.Heartbeat(runner(context.Background(), "runner-a"), application.HeartbeatRequest{RequestID: "hb-a", RunnerVersion: validInput()}); err != nil {
		t.Fatal(err)
	}
	if got := mustRunners(t, s, owner(context.Background())).IntersectionState; got != application.SchemaIntersectionNonEmpty {
		t.Fatalf("fresh report gives %q", got)
	}
	clk.Set(base.Add(application.RunnerVersionReportStaleAfter + time.Nanosecond))
	if got := mustRunners(t, s, owner(context.Background())).IntersectionState; got != application.SchemaIntersectionUnknown {
		t.Fatalf("stale report gives %q, want unknown", got)
	}
}

// TestRunnerViewNeverSpellsCompatibleWithoutUnknown is A6(h). Every key and
// every enum value in the marshalled view is walked.
func TestRunnerViewNeverSpellsCompatibleWithoutUnknown(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	views := []application.RunnerVersionListView{}
	clk := &fixedClock{now: base}
	s, _ := runnerService(t, clk)
	for i, id := range []string{"runner-a", "runner-b", "runner-c"} {
		var input *application.RunnerVersionInput
		if i != 2 {
			input = validInput()
		}
		if _, err := s.Heartbeat(runner(context.Background(), id), application.HeartbeatRequest{RequestID: fmt.Sprintf("hb-%d", i), RunnerVersion: input}); err != nil {
			t.Fatal(err)
		}
		views = append(views, mustRunners(t, s, owner(context.Background())))
	}
	clk.Set(base.Add(2 * application.RunnerVersionReportStaleAfter))
	views = append(views, mustRunners(t, s, owner(context.Background())))

	forbidden := []string{"compatible", "ok", "healthy", "valid"}
	keys := map[string]bool{}
	values := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				keys[k] = true
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		case string:
			values[t] = true
		}
	}
	for _, view := range views {
		walk(marshalView(t, view))
	}
	if len(keys) == 0 || len(values) == 0 {
		t.Fatalf("the walk found keys=%d values=%d; it is not inspecting the document", len(keys), len(values))
	}
	for key := range keys {
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(key), bad) {
				t.Fatalf("field name %q spells %q", key, bad)
			}
		}
	}
	// A state value may not spell any of the four words, and unknown must
	// actually be reachable as a sibling of the states that do appear.
	for value := range values {
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(value), bad) {
				t.Fatalf("enum value %q spells %q", value, bad)
			}
		}
	}
	if !values[string(application.SchemaIntersectionUnknown)] {
		t.Fatalf("unknown never appeared among the marshalled values %v; it must be a sibling value of intersection_state", values)
	}
	if !values[string(application.RunnerReportNotReported)] || !values[string(application.RunnerReportStale)] {
		t.Fatalf("not-reported and stale did not both appear among %v", values)
	}
	t.Logf("walked %d distinct keys and %d distinct string values", len(keys), len(values))
}

// ===========================================================================
// A7: the report is never authority over reads
// ===========================================================================

// reportIdentifiers are the names no admission path may mention.
var reportIdentifiers = []string{
	"RunnerVersionReport",
	"RunnerVersionReports",
	"SaveRunnerVersionReport",
	"RunnerVersionReportRepository",
	"RunnerVersionInput",
	"RunnerVersionView",
	"RunnerVersionListView",
	"RunnerReportState",
	"SchemaIntersectionState",
}

func namesReportIdentifier(n ast.Node) []string {
	found := []string{}
	ast.Inspect(n, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		for _, name := range reportIdentifiers {
			if ident.Name == name {
				found = append(found, name)
			}
		}
		return true
	})
	return found
}

// TestAdmissionPathDoesNotReadTheReport is A7's mechanical half inside
// internal/application: the whole caller-admission file -- CallerFromContext,
// callerActor, runnerCaller and requestedBy -- mentions no report type and no
// report port. The matcher is verified against a known-positive fixture
// first, and a zero-declaration scan fails outright.
func TestAdmissionPathDoesNotReadTheReport(t *testing.T) {
	positive := `package application
func runnerCaller() { var r RunnerVersionReport; _ = r }
`
	file, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if len(namesReportIdentifier(file)) == 0 {
		t.Fatal("positive control: a synthetic admission function naming RunnerVersionReport was not flagged")
	}

	fset := token.NewFileSet()
	admission, err := parser.ParseFile(fset, "caller.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse caller.go: %v", err)
	}
	declarations := 0
	for _, decl := range admission.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		declarations++
		if hits := namesReportIdentifier(fn); len(hits) != 0 {
			t.Fatalf("caller.go: %s names %v; no admission path may read a Runner's self-claim", fn.Name.Name, hits)
		}
	}
	if declarations == 0 {
		t.Fatal("scanned zero function declarations in caller.go; the scan is broken")
	}
	// The three admission functions the Work Order names must actually be in
	// the file that was scanned, or the scan proves nothing.
	names := map[string]bool{}
	for _, decl := range admission.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			names[fn.Name.Name] = true
		}
	}
	for _, want := range []string{"callerActor", "runnerCaller", "CallerFromContext"} {
		if !names[want] {
			t.Fatalf("caller.go does not declare %s; the admission path moved and this guard is vacuous", want)
		}
	}
	t.Logf("scanned caller.go: %d function declarations, none naming a report identifier", declarations)
}

// runnerOperationOutcome is one runner-facing operation's whole observable
// result: the response value and the error, compared by value across the two
// arms of the behavioural proof.
type runnerOperationOutcome struct {
	name     string
	response string
	err      string
}

// driveRunnerLifecycle runs every runner-facing operation once and returns
// each one's outcome. reportInterval, when non-nil, is posted first as this
// Runner's version report.
func driveRunnerLifecycle(t *testing.T, report *application.RunnerVersionInput) []runnerOperationOutcome {
	t.Helper()
	s, _ := runnerService(t, clock{})
	ctx := context.Background()
	own := owner(ctx)
	run := runner(ctx, "runner-1")
	render := func(name string, v any, err error) runnerOperationOutcome {
		out := runnerOperationOutcome{name: name}
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		out.response = string(b)
		if err != nil {
			out.err = err.Error()
		}
		return out
	}

	// The report, or an equally-shaped heartbeat that carries none, so both
	// arms consume the same identifiers from the deterministic generator.
	hb0, hb0err := s.Heartbeat(run, application.HeartbeatRequest{RequestID: "hb-0", RunnerVersion: report})
	if hb0err != nil {
		t.Fatalf("the report-carrying heartbeat itself failed: %v", hb0err)
	}
	_ = hb0

	captured, err := s.Capture(own, application.CaptureRequest{RequestID: "cap", Text: "work"})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := s.Plan(own, application.PlanRequest{RequestID: "plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prepare(own, application.PrepareRequest{RequestID: "prep", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version}); err != nil {
		t.Fatal(err)
	}

	outcomes := []runnerOperationOutcome{}
	claim, claimErr := s.Claim(run, application.ClaimRequest{RequestID: "claim", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	outcomes = append(outcomes, render("claims:acquire", claim, claimErr))
	if claimErr != nil {
		t.Fatalf("claim failed: %v", claimErr)
	}
	permit, permitErr := s.Permit(run, application.PermitRequest{RequestID: "permit", Kind: domain.PermitExternalEffect, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	outcomes = append(outcomes, render("permits:check", permit, permitErr))
	start, startErr := s.Start(run, application.StartRequest{RequestID: "start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	outcomes = append(outcomes, render("executions:start", start, startErr))
	checkpoint, checkpointErr := s.Checkpoint(run, application.CheckpointRequest{RequestID: "checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken})
	outcomes = append(outcomes, render("checkpoints", checkpoint, checkpointErr))
	beat, beatErr := s.Heartbeat(run, application.HeartbeatRequest{RequestID: "hb-1"})
	outcomes = append(outcomes, render("heartbeat", beat, beatErr))
	accepted, acceptErr := s.AcceptResult(run, application.AcceptResultRequest{RequestID: "result", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: start.Version, FencingToken: claim.FencingToken, Succeeded: true})
	outcomes = append(outcomes, render("executions/result", accepted, acceptErr))
	return outcomes
}

// TestAnAbsurdReportChangesNoRunnerOperation is A7's behavioural half, which
// matters more than the scan: a scan can be satisfied by indirection, while a
// Runner that keeps working after reporting an interval no plausible
// canonical schema lies in demonstrates the absence of the coupling.
func TestAnAbsurdReportChangesNoRunnerOperation(t *testing.T) {
	absurd := &application.RunnerVersionInput{
		Version:      "1.2.3",
		BinarySHA256: testDigestA,
		SchemaMin:    application.MaxRunnerSchemaBound,
		SchemaMax:    application.MaxRunnerSchemaBound,
	}
	reported := driveRunnerLifecycle(t, absurd)
	silent := driveRunnerLifecycle(t, nil)
	if len(reported) != 6 || len(silent) != 6 {
		t.Fatalf("expected six runner-facing operations, got %d and %d", len(reported), len(silent))
	}
	for i := range reported {
		if reported[i] != silent[i] {
			t.Fatalf("%s differed: reported=%+v silent=%+v", reported[i].name, reported[i], silent[i])
		}
		if reported[i].err != "" {
			t.Fatalf("%s failed in both arms: %s", reported[i].name, reported[i].err)
		}
		t.Logf("%s: identical in both arms, response=%s", reported[i].name, reported[i].response)
	}
}

// ===========================================================================
// A8: the enumeration is bounded and read-only
// ===========================================================================

// TestRunnersEnumerationIsBoundedAndPerformsNoWrite is A8 at the application
// boundary: the bound truncates, the order is fixed, and one read records no
// event and enqueues no outbox item.
func TestRunnersEnumerationIsBoundedAndPerformsNoWrite(t *testing.T) {
	s, st := runnerService(t, clock{})
	ctx := context.Background()
	total := application.MaxRunnerVersionReports + 5
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("runner-%03d", i)
		if _, err := s.Heartbeat(runner(ctx, id), application.HeartbeatRequest{RequestID: "hb-" + id, RunnerVersion: validInput()}); err != nil {
			t.Fatal(err)
		}
	}
	eventsBefore := len(st.Events())
	outboxBefore := len(st.Outbox())
	view := mustRunners(t, s, owner(ctx))
	if view.RunnerCount != application.MaxRunnerVersionReports || !view.Truncated {
		t.Fatalf("runner_count=%d truncated=%v, want %d and true", view.RunnerCount, view.Truncated, application.MaxRunnerVersionReports)
	}
	if view.IntersectionState != application.SchemaIntersectionUnknown {
		t.Fatalf("a truncated enumeration reports intersection_state %q; it did not see every machine", view.IntersectionState)
	}
	for i := 1; i < len(view.Runners); i++ {
		if view.Runners[i-1].RunnerID >= view.Runners[i].RunnerID {
			t.Fatalf("the order is not fixed ascending: %q then %q", view.Runners[i-1].RunnerID, view.Runners[i].RunnerID)
		}
	}
	if view.Runners[0].RunnerID != "runner-000" {
		t.Fatalf("first row = %q", view.Runners[0].RunnerID)
	}
	if got, want := len(st.Events()), eventsBefore; got != want {
		t.Fatalf("one read recorded %d new events", got-want)
	}
	if got, want := len(st.Outbox()), outboxBefore; got != want {
		t.Fatalf("one read enqueued %d outbox items", got-want)
	}
	// A second read is byte-identical: the read is a projection, not a
	// mutation that would change what the next read sees.
	again := mustRunners(t, s, owner(ctx))
	if !reflect.DeepEqual(view, again) {
		t.Fatal("two consecutive reads of the same state disagree")
	}
	t.Logf("stored %d reports, enumerated %d, truncated=%v", total, view.RunnerCount, view.Truncated)
}

// TestRunnersRequiresTheOwnerRole keeps the read on the owner side of the
// boundary at the application level too, not only at the transport.
func TestRunnersRequiresTheOwnerRole(t *testing.T) {
	s, _ := runnerService(t, clock{})
	if _, err := s.Runners(context.Background()); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("unauthenticated read: %v", err)
	}
	if _, err := s.Runners(runner(context.Background(), "runner-1")); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner-role read: %v", err)
	}
	if _, err := s.Runners(owner(context.Background())); err != nil {
		t.Fatalf("owner read: %v", err)
	}
}

// ===========================================================================
// A11: the idempotency consequence, asserted not assumed
// ===========================================================================

// TestHeartbeatReplayWithAChangedReportIsAConflict asserts the consequence
// dp-v2-069 d2 records rather than hides. It follows from
// requestFingerprint("heartbeat", req) covering the whole request, and it is
// existing semantics rather than new behaviour: any changed field of a
// replayed heartbeat has always produced this conflict.
func TestHeartbeatReplayWithAChangedReportIsAConflict(t *testing.T) {
	s, st := runnerService(t, clock{})
	ctx := runner(context.Background(), "runner-1")
	first, err := s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-same", RunnerVersion: validInput()})
	if err != nil {
		t.Fatal(err)
	}
	// An identical replay restores the prior response.
	again, err := s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-same", RunnerVersion: validInput()})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("an identical replay changed the response: %#v then %#v", first, again)
	}
	stored, ok := storedReport(t, st, "runner-1")
	if !ok {
		t.Fatal("no report stored")
	}
	eventsBefore := len(st.Events())

	// The same request_id with a different report is a conflict.
	changed := &application.RunnerVersionInput{Version: "1.2.4", BinarySHA256: testDigestB, SchemaMin: 3, SchemaMax: 8}
	if _, err = s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-same", RunnerVersion: changed}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("a replay with a changed report returned %v, want an idempotency conflict", err)
	}
	// And the conflict stored nothing new.
	after, ok := storedReport(t, st, "runner-1")
	if !ok || after != stored {
		t.Fatalf("the conflict changed the stored report: %#v then %#v", stored, after)
	}
	if len(st.Events()) != eventsBefore {
		t.Fatalf("the conflict recorded %d new events", len(st.Events())-eventsBefore)
	}
	// Dropping the report from a replayed request_id is the same conflict, for
	// the same reason.
	if _, err = s.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb-same"}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("a replay that dropped the report returned %v, want an idempotency conflict", err)
	}
}

// ===========================================================================
// The shared behavioural table, run against the memory adapter through the
// UnitOfWork port. internal/store/memory and internal/store/firestore run the
// very same cases; see RunnerVersionReportCases.
// ===========================================================================

func TestRunnerVersionReportCasesAreARealTable(t *testing.T) {
	cases := application.RunnerVersionReportCases()
	if len(cases) < 5 {
		t.Fatalf("the shared table has %d cases; it is too small to be the behavioural contract", len(cases))
	}
	names := map[string]bool{}
	sawTruncation := false
	sawUnreported := false
	for _, c := range cases {
		if names[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		names[c.Name] = true
		if c.WantTruncated {
			sawTruncation = true
		}
		for _, want := range c.Want {
			if !want.Reported() {
				sawUnreported = true
			}
		}
	}
	if !sawTruncation || !sawUnreported {
		t.Fatalf("the table must cover truncation (%v) and a known-but-unreported Runner (%v)", sawTruncation, sawUnreported)
	}
}
