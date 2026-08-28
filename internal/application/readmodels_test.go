package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

func TestRequirementPageUsesOpaqueBoundedCursor(t *testing.T) {
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	for i := 0; i < 3; i++ {
		if _, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "page-" + string(rune('a'+i)), RequirementID: "req-" + string(rune('a'+i)), Text: "original"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.ListRequirementsPage(ctx, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Requirements) != 2 || first.NextCursor == "" {
		t.Fatalf("first=%#v", first)
	}
	if strings.Contains(first.NextCursor, "req-") {
		t.Fatalf("cursor leaked key: %q", first.NextCursor)
	}
	second, err := svc.ListRequirementsPage(ctx, first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Requirements) != 1 || second.NextCursor != "" {
		t.Fatalf("second=%#v", second)
	}
	if _, err := svc.ListRequirementsPage(ctx, "not-a-cursor", 2); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestRequirementDetailIncludesExecutionAndNextAction(t *testing.T) {
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	if _, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "detail", RequirementID: "r-detail", Text: "original request"}); err != nil {
		t.Fatal(err)
	}
	inc, _ := domain.NewIncrementID("inc-detail")
	rid, _ := domain.NewRequirementID("r-detail")
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: rid, Status: domain.IncrementReady, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	v, ok, err := svc.GetRequirementDetail(ctx, "r-detail")
	if err != nil || !ok {
		t.Fatalf("detail %v %v", ok, err)
	}
	if v.OriginalText != "original request" || v.NextAction != "run next increment" || len(v.Increments) != 1 {
		t.Fatalf("detail=%#v", v)
	}
}

func TestControlReadModelDoesNotInferObservation(t *testing.T) {
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	if _, err := svc.Control(ctx, application.ControlRequest{RequestID: "control-read", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "i"}, Mode: domain.ControlPauseIntake}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListControls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Requested || rows[0].Acknowledged || rows[0].Effective || rows[0].Verified {
		t.Fatalf("progress=%#v", rows)
	}
	if _, err := svc.Heartbeat(application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner", RunnerID: "r"}), application.HeartbeatRequest{RequestID: "observe", ControlRevision: rows[0].Revision}); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.ListControls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Acknowledged || rows[0].Effective || rows[0].Verified {
		t.Fatalf("ack progress=%#v", rows)
	}
}

func TestHeartbeatPersistsBoundedRunnerObservation(t *testing.T) {
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner", RunnerID: "r"})
	_, err = svc.Heartbeat(ctx, application.HeartbeatRequest{RequestID: "hb", ControlRevision: 3, Processes: []domain.ProcessObservation{{ProcessID: "p", State: "running", At: clock{}.Now()}}})
	if err != nil {
		t.Fatal(err)
	}
	var got domain.RunnerObservation
	err = st.Transact(context.Background(), func(u application.UnitOfWork) error {
		var ok bool
		got, ok, err = u.RunnerObservation(context.Background(), "r")
		if !ok {
			return errors.New("observation missing")
		}
		return err
	})
	if err != nil || got.AppliedRevision != 3 || len(got.Processes) != 1 {
		t.Fatalf("observation=%#v err=%v", got, err)
	}
}

// ===========================================================================
// V2-073: the capture time on the read surface.
// ===========================================================================
//
// A5/A6/A11. The three properties proven here are that the read path has no
// source for the value other than the Requirement record itself, that a
// legacy record's absent capture time is reported as absent rather than as
// the zero instant, and that the export record is unchanged so no export
// digest moves.

// captureFixtureClock is an injected clock whose instant is chosen per test,
// so "reading at a different time" is a parameter rather than a wall-clock
// event. It exists because clock{} in service_test.go is fixed.
type captureFixtureClock struct{ at time.Time }

func (c captureFixtureClock) Now() time.Time { return c.at }

// storeWithRequirement writes one Requirement directly through the store,
// bypassing Capture, so a capture time (or the absence of one) can be set
// exactly. No event is recorded, which is what makes the event-log assertions
// below decisive.
func storeWithRequirement(t *testing.T, r domain.Requirement) *memory.Store {
	t.Helper()
	st := memory.New()
	ctx := context.Background()
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, r, 0)
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func serviceOn(t *testing.T, st *memory.Store, at time.Time) *application.Service {
	t.Helper()
	s, err := application.NewServiceWithConfig(st, captureFixtureClock{at: at}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func recordedRequirement(t *testing.T, id string, capturedAt time.Time) domain.Requirement {
	t.Helper()
	rid, err := domain.NewRequirementID(id)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Requirement{ID: rid, Status: domain.RequirementCaptured, Version: 1, CapturedAt: capturedAt}
}

// TestCaptureTimeDoesNotDependOnTheReadTime is A5's first assertion: reading
// the same Requirement at two different injected clock values produces
// byte-identical capture times, so the ordering input cannot depend on when
// somebody refreshed a view.
func TestCaptureTimeDoesNotDependOnTheReadTime(t *testing.T) {
	capturedAt := time.Unix(1_650_000_000, 987_654_321).UTC()
	st := storeWithRequirement(t, recordedRequirement(t, "requirement-read-time", capturedAt))
	ctx := owner(context.Background())

	early := serviceOn(t, st, time.Unix(1_700_000_000, 0).UTC())
	late := serviceOn(t, st, time.Unix(1_900_000_000, 0).UTC())

	a, ok, err := early.GetRequirementDetail(ctx, "requirement-read-time")
	if err != nil || !ok {
		t.Fatalf("early read: ok=%v err=%v", ok, err)
	}
	b, ok, err := late.GetRequirementDetail(ctx, "requirement-read-time")
	if err != nil || !ok {
		t.Fatalf("late read: ok=%v err=%v", ok, err)
	}
	if a.CapturedAt == nil || b.CapturedAt == nil {
		t.Fatalf("captured_at was omitted for a recorded Requirement: early=%v late=%v", a.CapturedAt, b.CapturedAt)
	}
	// Byte-identical: the marshalled bytes of the two reads must match, which
	// is stronger than comparing the instants.
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ja) != string(jb) {
		t.Fatalf("two reads at different clock values produced different bytes:\n%s\n%s", ja, jb)
	}
	if !a.CapturedAt.Equal(capturedAt) {
		t.Fatalf("captured_at = %v, want the stored %v", *a.CapturedAt, capturedAt)
	}
	// Neither read time appears in the response at all.
	for _, readTime := range []time.Time{time.Unix(1_700_000_000, 0).UTC(), time.Unix(1_900_000_000, 0).UTC()} {
		if a.CapturedAt.Equal(readTime) {
			t.Fatalf("captured_at equals a read time (%v); the value was derived from the clock", readTime)
		}
	}
}

// TestCaptureTimeIsNotDerivedFromTheEventLog is A5's decisive behavioural
// check: the same Requirement read from a store whose event log is empty
// reports the identical capture time, so nothing on the read path scans
// events for it.
func TestCaptureTimeIsNotDerivedFromTheEventLog(t *testing.T) {
	ctx := owner(context.Background())
	// A store populated by Capture: it has one requirement.captured event.
	withEvents := memory.New()
	svc := serviceOn(t, withEvents, time.Unix(1_650_000_000, 987_654_321).UTC())
	out, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "capture-event-log", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(withEvents.Events()) == 0 {
		t.Fatal("the populated store has no events; the comparison below would prove nothing")
	}
	stored, ok := withEvents.Requirement(out.RequirementID)
	if !ok {
		t.Fatal("requirement missing")
	}
	before, ok, err := svc.GetRequirementDetail(ctx, out.RequirementID)
	if err != nil || !ok {
		t.Fatalf("read with events: ok=%v err=%v", ok, err)
	}

	// The identical record in a store with an EMPTY event log.
	emptied := storeWithRequirement(t, stored)
	if got := len(emptied.Events()); got != 0 {
		t.Fatalf("the emptied store has %d events, want 0", got)
	}
	after, ok, err := serviceOn(t, emptied, time.Unix(1_650_000_000, 987_654_321).UTC()).GetRequirementDetail(ctx, out.RequirementID)
	if err != nil || !ok {
		t.Fatalf("read without events: ok=%v err=%v", ok, err)
	}
	if after.CapturedAt == nil {
		t.Fatal("captured_at disappeared once the event log was empty; the read path derives it from events")
	}
	if !before.CapturedAt.Equal(*after.CapturedAt) {
		t.Fatalf("captured_at changed with an empty event log: %v -> %v", *before.CapturedAt, *after.CapturedAt)
	}
}

// TestLegacyRequirementOmitsCapturedAtEntirely is A6: the read view omits the
// key rather than emitting 0001-01-01T00:00:00Z, checked by scanning the
// marshalled bytes for the key AND for the literal zero date.
func TestLegacyRequirementOmitsCapturedAtEntirely(t *testing.T) {
	ctx := owner(context.Background())
	legacy := recordedRequirement(t, "requirement-legacy", time.Time{})
	if legacy.CaptureRecorded() {
		t.Fatal("the fixture is not a legacy record")
	}
	st := storeWithRequirement(t, legacy)
	svc := serviceOn(t, st, time.Unix(1_700_000_000, 0).UTC())

	detail, ok, err := svc.GetRequirementDetail(ctx, "requirement-legacy")
	if err != nil || !ok {
		t.Fatalf("detail read: ok=%v err=%v", ok, err)
	}
	if detail.CapturedAt != nil {
		t.Fatalf("RequirementDetailView reported captured_at %v for a legacy record", *detail.CapturedAt)
	}
	page, err := svc.ListRequirementsPage(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requirements) != 1 {
		t.Fatalf("requirements = %d, want 1", len(page.Requirements))
	}
	if page.Requirements[0].CapturedAt != nil {
		t.Fatalf("RequirementView reported captured_at %v for a legacy record", *page.Requirements[0].CapturedAt)
	}
	for name, value := range map[string]any{"RequirementDetailView": detail, "RequirementView": page.Requirements[0], "RequirementPage": page} {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "captured_at") {
			t.Fatalf("%s marshalled the captured_at key for a legacy record: %s", name, b)
		}
		if strings.Contains(string(b), "0001-01-01") {
			t.Fatalf("%s marshalled the zero instant for a legacy record: %s", name, b)
		}
	}

	// A recorded record does carry the key, so the absence above is a
	// statement about the value and not about the marshaller.
	recorded := storeWithRequirement(t, recordedRequirement(t, "requirement-recorded", time.Unix(1_650_000_000, 0).UTC()))
	got, ok, err := serviceOn(t, recorded, time.Unix(1_700_000_000, 0).UTC()).GetRequirementDetail(ctx, "requirement-recorded")
	if err != nil || !ok {
		t.Fatalf("recorded read: ok=%v err=%v", ok, err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "captured_at") {
		t.Fatalf("a recorded Requirement did not marshal captured_at: %s", b)
	}
}

// TestCapturedAtViewReturnsACopyNotAPointerIntoStoredState is A6's aliasing
// assertion: mutating through the returned pointer and re-reading must not
// change what the store reports.
func TestCapturedAtViewReturnsACopyNotAPointerIntoStoredState(t *testing.T) {
	ctx := owner(context.Background())
	capturedAt := time.Unix(1_650_000_000, 0).UTC()
	st := storeWithRequirement(t, recordedRequirement(t, "requirement-alias", capturedAt))
	svc := serviceOn(t, st, time.Unix(1_700_000_000, 0).UTC())

	view, ok, err := svc.GetRequirementDetail(ctx, "requirement-alias")
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	if view.CapturedAt == nil {
		t.Fatal("captured_at was omitted for a recorded Requirement")
	}
	*view.CapturedAt = time.Unix(1, 0).UTC()

	again, ok, err := svc.GetRequirementDetail(ctx, "requirement-alias")
	if err != nil || !ok {
		t.Fatalf("re-read: ok=%v err=%v", ok, err)
	}
	if again.CapturedAt == nil || !again.CapturedAt.Equal(capturedAt) {
		t.Fatalf("stored state was mutated through the returned pointer: re-read = %v, want %v", again.CapturedAt, capturedAt)
	}
	stored, _ := st.Requirement("requirement-alias")
	if !stored.CapturedAt.Equal(capturedAt) {
		t.Fatalf("the stored Requirement was mutated through the returned pointer: %v", stored.CapturedAt)
	}
}

// TestExportRequirementRecordIsUnchangedByTheCaptureTime is A11: the export
// record's marshalled shape -- and therefore its sha256 digest -- is
// byte-identical for the same input whether or not a capture time is
// recorded, so no export digest moves.
func TestExportRequirementRecordIsUnchangedByTheCaptureTime(t *testing.T) {
	ctx := owner(context.Background())
	legacy := storeWithRequirement(t, recordedRequirement(t, "requirement-export", time.Time{}))
	recorded := storeWithRequirement(t, recordedRequirement(t, "requirement-export", time.Unix(1_650_000_000, 0).UTC()))

	digest := func(st *memory.Store) string {
		records, err := serviceOn(t, st, time.Unix(1_700_000_000, 0).UTC()).Export(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range records {
			if rec.Kind != "requirement" {
				continue
			}
			b, err := json.Marshal(rec.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "captured_at") || strings.Contains(string(b), "CapturedAt") {
				t.Fatalf("the export record carries a capture time: %s", b)
			}
			return rec.Digest + " " + string(b)
		}
		t.Fatal("no requirement export record was produced")
		return ""
	}
	if a, b := digest(legacy), digest(recorded); a != b {
		t.Fatalf("the export record moved when a capture time was recorded:\n%s\n%s", a, b)
	}
	// The ExportRequirement field list is pinned, so a later edit that adds
	// the value to the export has to change this assertion deliberately.
	typ := reflect.TypeOf(application.ExportRequirement{})
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, typ.Field(i).Name)
	}
	want := []string{"RequirementID", "Status", "Version", "IncrementIDs", "RequestedBy"}
	if len(fields) != len(want) {
		t.Fatalf("ExportRequirement fields = %v, want exactly %v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("ExportRequirement fields = %v, want exactly %v", fields, want)
		}
	}
}

// ===========================================================================
// V2-073 A5: the source of the value, read out of the AST rather than trusted.
// ===========================================================================

// predeclaredIdentifiers are the names a function body may mention without
// naming an imported package or a package-level declaration.
var predeclaredIdentifiers = map[string]bool{
	"nil": true, "true": true, "false": true, "iota": true,
	"len": true, "cap": true, "append": true, "make": true, "new": true,
	"copy": true, "delete": true, "panic": true, "recover": true, "print": true, "println": true,
	"error": true, "string": true, "int": true, "int64": true, "bool": true, "byte": true, "rune": true,
}

// parseApplicationFile parses one file of this package by name. `go test`
// always runs with the package directory as its working directory, so the
// relative name is correct and a wrong one fails loudly rather than silently
// scanning nothing.
func parseApplicationFile(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return fset, file
}

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name != nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// externalNamesInBody returns every identifier a function body mentions that
// is neither one of its own parameters, nor declared inside the body, nor
// predeclared, nor a field name (a selector's Sel or a composite-literal
// key). Anything left is a reference OUT of the function: an imported package
// such as `time`, a package-level variable, or another function. For
// capturedAtView the set must be empty, which is what "its only source is the
// Requirement record" means mechanically.
func externalNamesInBody(fn *ast.FuncDecl) []string {
	local := map[string]bool{}
	if fn.Recv != nil {
		for _, f := range fn.Recv.List {
			for _, n := range f.Names {
				local[n.Name] = true
			}
		}
	}
	if fn.Type.Params != nil {
		for _, f := range fn.Type.Params.List {
			for _, n := range f.Names {
				local[n.Name] = true
			}
		}
	}
	skip := map[token.Pos]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						local[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, n := range x.Names {
				local[n.Name] = true
			}
		case *ast.SelectorExpr:
			if x.Sel != nil {
				skip[x.Sel.Pos()] = true
			}
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok {
				skip[id.Pos()] = true
			}
		}
		return true
	})
	seen := map[string]bool{}
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || skip[id.Pos()] || local[id.Name] || predeclaredIdentifiers[id.Name] {
			return true
		}
		if !seen[id.Name] {
			seen[id.Name] = true
			out = append(out, id.Name)
		}
		return true
	})
	sort.Strings(out)
	return out
}

// TestCapturedAtViewReadsOnlyTheRequirementRecord is A5's AST assertion. The
// matcher is verified FIRST against a known-negative (a helper that reads only
// its parameter) and a known-positive (the same helper with a clock read),
// because a scan that can never report anything would pass vacuously.
func TestCapturedAtViewReadsOnlyTheRequirementRecord(t *testing.T) {
	parseHelper := func(src string) *ast.FuncDecl {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "control.go", "package application\n"+src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse control: %v", err)
		}
		fn := findFunc(f, "control")
		if fn == nil {
			t.Fatal("the control helper was not parsed")
		}
		return fn
	}
	clean := parseHelper("func control(r domain.Requirement) *time.Time {\n\tif !r.CaptureRecorded() {\n\t\treturn nil\n\t}\n\tv := r.CapturedAt\n\treturn &v\n}\n")
	if got := externalNamesInBody(clean); len(got) != 0 {
		t.Fatalf("negative control failed: a helper that reads only its parameter reported external names %v", got)
	}
	dirty := parseHelper("func control(r domain.Requirement) *time.Time {\n\tv := time.Now()\n\treturn &v\n}\n")
	got := externalNamesInBody(dirty)
	found := false
	for _, name := range got {
		if name == "time" {
			found = true
		}
	}
	if !found {
		t.Fatalf("positive control failed: a helper that reads the wall clock reported external names %v, which does not include \"time\"", got)
	}

	_, file := parseApplicationFile(t, "readmodels.go")
	fn := findFunc(file, "capturedAtView")
	if fn == nil {
		t.Fatal("capturedAtView was not found in readmodels.go; this scan proves nothing")
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		t.Fatal("capturedAtView must take exactly one parameter, the Requirement record")
	}
	sel, ok := fn.Type.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != "Requirement" {
		t.Fatalf("capturedAtView's parameter type is %T, want domain.Requirement", fn.Type.Params.List[0].Type)
	}
	if names := externalNamesInBody(fn); len(names) != 0 {
		t.Fatalf("capturedAtView reaches outside its parameter: %v. Its only source must be the Requirement record -- no clock, no event scan, no package-level state.", names)
	}
}

// TestCapturedAtIsAssignedOnlyInsideCaptureFromTheAuthorityTime is A5's last
// assertion: no code path in this package assigns CapturedAt from a clock
// read outside Capture. The scan is over the AST of every non-test file in the
// package, so a write added anywhere fails here.
func TestCapturedAtIsAssignedOnlyInsideCaptureFromTheAuthorityTime(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("the scan found no files; the working directory is not internal/application")
	}
	sort.Strings(matches)
	type site struct {
		file, function, value string
	}
	var sites []site
	scanned := 0
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		_, file := parseApplicationFile(t, name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "CapturedAt" {
						sites = append(sites, site{name, fn.Name.Name, exprName(x.Value)})
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if s, ok := lhs.(*ast.SelectorExpr); ok && s.Sel != nil && s.Sel.Name == "CapturedAt" {
							sites = append(sites, site{name, fn.Name.Name, "assignment"})
						}
					}
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("the scan skipped every file")
	}
	// Two kinds of site are legitimate and they are separated by their value
	// expression, not by their file: a READ-MODEL projection sets the view
	// struct's own CapturedAt field from capturedAtView, and exactly one
	// DOMAIN write sets domain.Requirement.CapturedAt. Anything else -- a
	// second domain write, or a projection whose value is not capturedAtView
	// -- fails here.
	var domainWrites []site
	for _, s := range sites {
		if s.value == "capturedAtView" {
			if s.file != "readmodels.go" {
				t.Fatalf("a read-model projection of the capture time lives in %s, want readmodels.go", s.file)
			}
			continue
		}
		domainWrites = append(domainWrites, s)
	}
	if len(domainWrites) != 1 {
		t.Fatalf("domain.Requirement.CapturedAt is written at %d sites, want exactly 1 (Capture): %+v (all sites: %+v)", len(domainWrites), domainWrites, sites)
	}
	if domainWrites[0].file != "service.go" || domainWrites[0].function != "Capture" {
		t.Fatalf("CapturedAt is written in %s/%s, want service.go/Capture", domainWrites[0].file, domainWrites[0].function)
	}
	if domainWrites[0].value != "capturedAt" {
		t.Fatalf("CapturedAt is set from %q, want the local capturedAt bound to the transaction authority time", domainWrites[0].value)
	}
	if len(sites) != 3 {
		t.Fatalf("CapturedAt is written at %d sites in total, want 3 (two read-model projections plus the one domain write): %+v", len(sites), sites)
	}

	// capturedAt itself comes from transactionAuthorityTime, and the mutate
	// callback -- the part Firestore may retry -- reads no clock at all.
	_, service := parseApplicationFile(t, "service.go")
	capture := findFunc(service, "Capture")
	if capture == nil {
		t.Fatal("Capture was not found in service.go")
	}
	source := ""
	ast.Inspect(capture.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			return true
		}
		id, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || id.Name != "capturedAt" || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		source = exprName(call.Fun)
		return false
	})
	if source != "transactionAuthorityTime" {
		t.Fatalf("capturedAt is bound from %q, want transactionAuthorityTime -- the accessor Service.record also uses", source)
	}
	clockReads := 0
	ast.Inspect(capture.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		ast.Inspect(lit.Body, func(inner ast.Node) bool {
			sel, ok := inner.(*ast.SelectorExpr)
			if ok && sel.Sel != nil && sel.Sel.Name == "Now" {
				clockReads++
			}
			return true
		})
		return true
	})
	if clockReads != 0 {
		t.Fatalf("Capture's transaction callback reads a clock %d time(s); Firestore may retry the callback, so the capture time would not be retry-stable", clockReads)
	}
}

// exprName renders the simple name of an expression used as a value or a
// callee, or a placeholder when it is not a plain identifier or selector.
func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if x.Sel != nil {
			return x.Sel.Name
		}
	case *ast.CallExpr:
		return exprName(x.Fun)
	case *ast.UnaryExpr:
		return exprName(x.X)
	}
	return "non-identifier"
}

// TestTheServiceWritesARequirementOnlyFromCaptureAndPlan pins the set of
// service paths a Requirement's stored value can pass through, which is what
// makes "the capture time is unchanged by every lifecycle transition applied
// through the service" (A10) an exhaustive statement rather than a sample.
// The store-adapter tables in internal/store/memory and
// internal/store/firestore drive exactly these two.
func TestTheServiceWritesARequirementOnlyFromCaptureAndPlan(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("the scan found no files; the working directory is not internal/application")
	}
	sort.Strings(matches)
	writers := map[string]bool{}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		_, file := parseApplicationFile(t, name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "SaveRequirement" {
					writers[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	// V2-065 widened this closed set from two to four, and the widening is
	// the measurement rather than a weakening: RequestHumanInput and
	// AnswerHumanInput are the first two application commands that move a
	// Requirement through domain.DecideRequirement, so they necessarily
	// persist a Requirement. The property this test exists to make
	// exhaustive is unaffected -- domain.DecideRequirement opens with
	// `next := current`, so neither command touches CapturedAt, and the
	// store-adapter tables still drive Capture and Plan. The set stays
	// closed: a fifth writer still fails here.
	// V2-082 widens the closed set from four to five, on the same terms: it
	// adds Service.StartFraming, the one command that issues the
	// captured->framing transition, so it necessarily persists a Requirement.
	// The property this test makes exhaustive is again unaffected --
	// domain.DecideRequirement opens with `next := current`, so StartFraming
	// does not touch CapturedAt either, and the store-adapter tables still
	// drive Capture and Plan. The set stays closed: a sixth writer still fails
	// here.
	// V2-084 widens the closed set from five to seven, on the same terms, and
	// each added entry has its own reason:
	//   - CompleteFraming issues domain.RequirementReadyCommand from framing and
	//     therefore necessarily persists a Requirement.
	//   - Claim issues domain.RequirementStart for the claimed Increment's own
	//     parent when that parent is in ready, inside the claim's existing
	//     transaction, and therefore necessarily persists a Requirement too.
	// The property this test makes exhaustive is again unaffected:
	// domain.DecideRequirement opens with `next := current`, so neither new
	// writer touches CapturedAt, and the store-adapter tables still drive
	// Capture and Plan. Nothing else in this guard changes and the set stays
	// closed: an eighth writer still fails here.
	// V2-090 widens the closed set from seven to TEN, on the same terms, and
	// the three added entries share one reason: PauseRequirement,
	// ResumeRequirement and CancelRequirement each issue exactly one
	// domain.RequirementCommand -- pause, resume and cancel respectively --
	// through domain.DecideRequirement, so each necessarily persists a
	// Requirement. They are the owner triple docs/product/user-facing-spec.md
	// :201 names, and before them `paused` was a source status in exactly ONE of
	// DecideRequirement's ten branches, so a pause would have had no exit.
	//
	// The property this test makes exhaustive is again unaffected in the sense
	// that matters here -- domain.DecideRequirement opens with `next := current`,
	// so no new writer touches CapturedAt -- but ONE thing about it has changed
	// and is recorded rather than glossed: V2-090's pause, resume and cancel
	// branches are the FIRST branches in the domain that write a field other
	// than Status and Version, namely Requirement.PausedFrom.
	// internal/domain/capture_time_test.go's table over every command kind still
	// asserts CapturedAt survives all ten, and
	// internal/domain/invariant_model_test.go's requirementsEqual now compares
	// PausedFrom so the new field cannot move unobserved.
	//
	// The store-adapter tables still drive Capture and Plan. Nothing else in
	// this guard changes and the set stays CLOSED: an eleventh writer still fails
	// here.
	// V2-091 widens the closed set from ten to TWELVE, under v2-task-dag.md
	// 12.12, on the same terms. Two entries are added, both in the ONE new file
	// internal/application/loop.go, and they share one reason: each issues
	// exactly one domain.RequirementCommand through domain.DecideRequirement and
	// therefore necessarily persists a Requirement.
	//   - loopRequirementTransition persists the waiting and the recovering
	//     transitions, neither of which had any non-test issuer at all before
	//     this task.
	//   - loopCompleteRequirement persists the completed value, and it stages
	//     exactly ONE SaveRequirement for the evaluate-then-complete pair on
	//     purpose: two writes describing one atomic step would leave a
	//     half-state if a store flushed the first and refused the second.
	// The property this test makes exhaustive is unaffected:
	// domain.DecideRequirement opens with `next := current` and
	// domain.CompleteRequirementFromRelease assigns only Status, Version and
	// StableSnapshot, so neither new writer touches CapturedAt. The
	// store-adapter tables still drive Capture and Plan. Nothing else in this
	// guard changes and the set stays CLOSED: a THIRTEENTH writer still fails
	// here.
	want := map[string]bool{"Capture": true, "Plan": true, "RequestHumanInput": true, "AnswerHumanInput": true, "StartFraming": true, "CompleteFraming": true, "Claim": true, "PauseRequirement": true, "ResumeRequirement": true, "CancelRequirement": true, "loopRequirementTransition": true, "loopCompleteRequirement": true}
	if len(writers) != len(want) {
		t.Fatalf("the service writes a Requirement from %v, want exactly %v", keysSorted(writers), keysSorted(want))
	}
	for name := range want {
		if !writers[name] {
			t.Fatalf("the service writes a Requirement from %v, want exactly %v", keysSorted(writers), keysSorted(want))
		}
	}
}

func keysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTheRequirementDetailReportsTheExitOnlyWhileThePauseHoldsIt is V2-090 A16.
//
// The read-model change is ONE additive optional field. A way out that is
// visible only in the response to the pause is still a trap for anyone who
// closed the tab, so the exit is on the detail too -- and it follows the
// "omitted entirely" discipline repository_id and captured_at already use
// (contracts/openapi/openapi-v1.yaml declares all three that way), asserted
// here at the JSON level rather than only on the Go value, because "absent" and
// "present and empty" are different answers to the owner.
//
// nextAction is asserted to be BYTE-UNCHANGED in the only way a test can assert
// it: the field is read and compared against the value the same fixture
// produced before this task, "plan increments" for a Requirement with no
// Increments. A paused branch is deliberately NOT added and deliberately NOT
// asserted -- see the evidence record's named finding.
func TestTheRequirementDetailReportsTheExitOnlyWhileThePauseHoldsIt(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())
	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a16:capture", Text: "a requirement whose exit must be visible"})
	if err != nil {
		t.Fatal(err)
	}
	detailJSON := func(t *testing.T) (application.RequirementDetailView, map[string]any) {
		t.Helper()
		view, found, err := svc.GetRequirementDetail(ctx, captured.RequirementID)
		if err != nil || !found {
			t.Fatalf("detail: found=%v err=%v", found, err)
		}
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return view, body
	}

	// Not paused: the key is absent ENTIRELY, not present and empty.
	view, body := detailJSON(t)
	if _, present := body["resumes_to"]; present {
		t.Fatalf("a captured Requirement's detail carries resumes_to: %v", body["resumes_to"])
	}
	if view.NextAction != "plan increments" {
		t.Fatalf("next_action for a Requirement with no Increments = %q, want %q; nextAction must be byte-unchanged", view.NextAction, "plan increments")
	}

	seeded := seedRequirementStatus(t, st, captured.RequirementID, domain.RequirementReady)
	paused, err := svc.PauseRequirement(ctx, application.PauseRequirementRequest{RequestID: "a16:pause", RequirementID: captured.RequirementID, ExpectedVersion: seeded})
	if err != nil {
		t.Fatal(err)
	}
	view, body = detailJSON(t)
	if got, _ := body["resumes_to"].(string); got != string(domain.RequirementReady) {
		t.Fatalf("the paused detail's resumes_to = %v, want ready", body["resumes_to"])
	}
	if view.ResumesTo != domain.RequirementReady {
		t.Fatalf("the paused detail's ResumesTo = %q, want ready", view.ResumesTo)
	}
	// The pause did not change nextAction, and did not change the other
	// additive fields either.
	if view.NextAction != "plan increments" {
		t.Fatalf("next_action after a pause = %q; nextAction has no paused branch and must not gain one here", view.NextAction)
	}

	if _, err = svc.ResumeRequirement(ctx, application.ResumeRequirementRequest{RequestID: "a16:resume", RequirementID: captured.RequirementID, ExpectedVersion: paused.Version}); err != nil {
		t.Fatal(err)
	}
	_, body = detailJSON(t)
	if _, present := body["resumes_to"]; present {
		t.Fatalf("a resumed Requirement's detail still carries resumes_to: %v", body["resumes_to"])
	}

	// A paused Requirement with NO memory -- the shape a record written before
	// the field existed has -- reads with the key ABSENT rather than as an
	// invented status. Absent means absent.
	forgotten := memory.New()
	svc2, err := application.NewServiceWithConfig(forgotten, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := svc2.Capture(ctx, application.CaptureRequest{RequestID: "a16:legacy", Text: "a paused record with no remembered origin"})
	if err != nil {
		t.Fatal(err)
	}
	seedRequirementStatus(t, forgotten, legacy.RequirementID, domain.RequirementPaused)
	view2, found, err := svc2.GetRequirementDetail(ctx, legacy.RequirementID)
	if err != nil || !found {
		t.Fatalf("legacy detail: found=%v err=%v", found, err)
	}
	if view2.Status != domain.RequirementPaused {
		t.Fatalf("the legacy record is %q, want paused", view2.Status)
	}
	if view2.ResumesTo != "" {
		t.Fatalf("a paused record with no memory reports resumes_to=%q; absent must stay absent", view2.ResumesTo)
	}
	raw, err := json.Marshal(view2)
	if err != nil {
		t.Fatal(err)
	}
	var legacyBody map[string]any
	if err := json.Unmarshal(raw, &legacyBody); err != nil {
		t.Fatal(err)
	}
	if _, present := legacyBody["resumes_to"]; present {
		t.Fatalf("a paused record with no memory carries resumes_to: %v", legacyBody["resumes_to"])
	}
}

// ===========================================================================
// V2-095 A6/A8: the Backlog list projection and the repository_id filter.
//
// Every test below is a table test over values written straight into the
// in-memory store and read back through the Service. NO CLOCK IS READ by any
// assertion: the three new confirmation items (per-row Increment status,
// next_action, and the Preview/Stable reflection) and the filter are pure
// functions of stored values, so a clock would be a sign the design was
// departed from. There is no sleep, no timer, no goroutine and no randomness.
// ===========================================================================

// listFixture writes a Requirement, its Increments, their Executions, its text
// and its Repository link directly through the store, bypassing every command,
// so a projection can be driven from an exactly-known state.
// listFixtureAssignedAt is the fixed, injected instant every fixture link
// carries. It is a constant and not a clock read: nothing in these table tests
// may depend on when they run.
var listFixtureAssignedAt = time.Unix(1_699_000_000, 0).UTC()

type listFixture struct {
	requirementID string
	status        domain.RequirementStatus
	snapshot      domain.StableReleaseSnapshot
	increments    []domain.Increment
	executions    []domain.Execution
	text          string
	repositoryID  string
}

func writeListFixtures(t *testing.T, fixtures ...listFixture) *memory.Store {
	t.Helper()
	st := memory.New()
	ctx := context.Background()
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		for _, f := range fixtures {
			rid, err := domain.NewRequirementID(f.requirementID)
			if err != nil {
				return err
			}
			incIDs := make([]domain.IncrementID, 0, len(f.increments))
			for _, inc := range f.increments {
				incIDs = append(incIDs, inc.ID)
			}
			r := domain.Requirement{ID: rid, Status: f.status, Version: 1, Increments: incIDs, StableSnapshot: f.snapshot}
			if err := u.SaveRequirement(ctx, r, 0); err != nil {
				return err
			}
			if f.text != "" {
				if err := u.SaveRequirementText(ctx, f.requirementID, f.text); err != nil {
					return err
				}
			}
			for _, inc := range f.increments {
				if err := u.SaveIncrement(ctx, inc, 0); err != nil {
					return err
				}
			}
			for _, e := range f.executions {
				if err := u.SaveExecution(ctx, e, 0); err != nil {
					return err
				}
			}
			if f.repositoryID != "" {
				repoID, err := domain.NewRepositoryID(f.repositoryID)
				if err != nil {
					return err
				}
				if err := u.SaveRequirementRepositoryLink(ctx, domain.RequirementRepositoryLink{RequirementID: rid, RepositoryID: repoID, AssignedAt: listFixtureAssignedAt}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func listIncrement(t *testing.T, id, requirementID string, status domain.IncrementStatus) domain.Increment {
	t.Helper()
	incID, err := domain.NewIncrementID(id)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := domain.NewRequirementID(requirementID)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Increment{ID: incID, RequirementID: rid, Status: status, Version: 1}
}

func listExecution(t *testing.T, id, incrementID string, status domain.ExecutionStatus) domain.Execution {
	t.Helper()
	eid, err := domain.NewExecutionID(id)
	if err != nil {
		t.Fatal(err)
	}
	incID, err := domain.NewIncrementID(incrementID)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Execution{ID: eid, IncrementID: incID, Status: status, Version: 1}
}

func listRowByID(t *testing.T, page application.RequirementPage, id string) application.RequirementView {
	t.Helper()
	for _, row := range page.Requirements {
		if row.RequirementID == id {
			return row
		}
	}
	t.Fatalf("the page carries no row for %s: %+v", id, page.Requirements)
	return application.RequirementView{}
}

// TestBacklogRowCarriesIncrementStatusNextActionAndReleaseReflection is A6's
// table: the three declared confirmation items no surface carried, asserted
// per row against a known state, and asserted to agree with the DETAIL view's
// next_action so the two cannot drift.
func TestBacklogRowCarriesIncrementStatusNextActionAndReleaseReflection(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	for _, tc := range []struct {
		name           string
		fixture        listFixture
		wantStatuses   map[string]domain.IncrementStatus
		wantNextAction string
		wantObserved   bool
	}{
		{
			name: "a running Execution makes the next action monitor execution",
			fixture: listFixture{
				requirementID: "req-running", status: domain.RequirementActive, text: "running",
				increments: []domain.Increment{listIncrement(t, "inc-running", "req-running", domain.IncrementExecuting)},
				executions: []domain.Execution{listExecution(t, "exe-running", "inc-running", domain.ExecutionRunning)},
			},
			wantStatuses:   map[string]domain.IncrementStatus{"inc-running": domain.IncrementExecuting},
			wantNextAction: "monitor execution",
		},
		{
			name: "a failed Increment makes the next action review failed increment",
			fixture: listFixture{
				requirementID: "req-failed", status: domain.RequirementActive, text: "failed",
				increments: []domain.Increment{listIncrement(t, "inc-failed", "req-failed", domain.IncrementFailed)},
			},
			wantStatuses:   map[string]domain.IncrementStatus{"inc-failed": domain.IncrementFailed},
			wantNextAction: "review failed increment",
		},
		{
			name: "no Increment at all makes the next action plan increments",
			fixture: listFixture{
				requirementID: "req-bare", status: domain.RequirementReady, text: "bare",
			},
			wantStatuses:   map[string]domain.IncrementStatus{},
			wantNextAction: "plan increments",
		},
		{
			name: "a completed Requirement has no next action",
			fixture: listFixture{
				requirementID: "req-done", status: domain.RequirementCompleted, text: "done",
				increments: []domain.Increment{listIncrement(t, "inc-done", "req-done", domain.IncrementIntegrated)},
			},
			wantStatuses:   map[string]domain.IncrementStatus{"inc-done": domain.IncrementIntegrated},
			wantNextAction: "none",
		},
		{
			name: "a recorded Stable snapshot is reported as observed",
			fixture: listFixture{
				requirementID: "req-released", status: domain.RequirementCompleted, text: "released",
				snapshot: domain.StableReleaseSnapshot{ReleaseID: "release-7", ReleaseVersion: 3, BundleDigest: "bundle-7", EvidenceDigest: "evidence-7"},
			},
			wantStatuses:   map[string]domain.IncrementStatus{},
			wantNextAction: "none",
			wantObserved:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := writeListFixtures(t, tc.fixture)
			svc := serviceOn(t, st, at)
			ctx := owner(context.Background())
			page, err := svc.ListRequirementsPage(ctx, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			row := listRowByID(t, page, tc.fixture.requirementID)

			if len(row.Increments) != len(tc.wantStatuses) {
				t.Fatalf("the row carries %d increments, want %d: %+v", len(row.Increments), len(tc.wantStatuses), row.Increments)
			}
			for _, got := range row.Increments {
				want, declared := tc.wantStatuses[got.IncrementID]
				if !declared {
					t.Fatalf("the row reports an increment %q the fixture never wrote", got.IncrementID)
				}
				if got.Status != want {
					t.Fatalf("increment %s reports status %q, want %q", got.IncrementID, got.Status, want)
				}
			}
			if row.IncrementsTruncated {
				t.Fatalf("a row with %d increments reports truncation", len(row.Increments))
			}
			if row.NextAction != tc.wantNextAction {
				t.Fatalf("next_action = %q, want %q", row.NextAction, tc.wantNextAction)
			}
			if row.ReleaseReflection.Observed != tc.wantObserved {
				t.Fatalf("release_reflection.observed = %v, want %v", row.ReleaseReflection.Observed, tc.wantObserved)
			}
			if row.ReleaseReflection.Reason == "" {
				t.Fatal("release_reflection carries no reason; an absence with no reason is indistinguishable from a value nobody looked for")
			}
			if !tc.wantObserved {
				// An unobserved reflection reports NO identifier at all. This
				// is the assertion that stops a zero snapshot from being read
				// as a real release.
				if row.ReleaseReflection.ReleaseID != "" || row.ReleaseReflection.BundleDigest != "" ||
					row.ReleaseReflection.EvidenceDigest != "" || row.ReleaseReflection.ReleaseVersion != 0 {
					t.Fatalf("an unobserved release reflection carries identifiers: %+v", row.ReleaseReflection)
				}
			} else if row.ReleaseReflection.ReleaseID == "" || row.ReleaseReflection.BundleDigest == "" {
				t.Fatalf("an observed release reflection carries no identifiers: %+v", row.ReleaseReflection)
			}

			// The list's next_action and the DETAIL view's next_action are the
			// same function over the same state, so they must agree. Two
			// variants would drift and the drift would show as a Backlog row
			// advising something the detail page does not.
			detail, found, err := svc.GetRequirementDetail(ctx, tc.fixture.requirementID)
			if err != nil || !found {
				t.Fatalf("detail read: found=%v err=%v", found, err)
			}
			if detail.NextAction != row.NextAction {
				t.Fatalf("the list reports next_action %q and the detail view reports %q for the same Requirement", row.NextAction, detail.NextAction)
			}
		})
	}
}

// TestBacklogPageBoundsIncrementIdsAcrossThePageAndSaysSo is v8's truncation
// table: a page whose Increments exceed the cap must SAY it was bounded rather
// than silently drop rows.
func TestBacklogPageBoundsIncrementIdsAcrossThePageAndSaysSo(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	// Two Requirements, each holding MaxPageSize Increments, so the page-wide
	// budget of MaxPageSize ids is exhausted by the first row alone.
	var fixtures []listFixture
	for _, tag := range []string{"a", "b"} {
		f := listFixture{requirementID: "req-" + tag, status: domain.RequirementActive, text: "many " + tag}
		for i := 0; i < application.MaxPageSize; i++ {
			f.increments = append(f.increments, listIncrement(t, "inc-"+tag+"-"+strconv.Itoa(i), "req-"+tag, domain.IncrementReady))
		}
		fixtures = append(fixtures, f)
	}
	st := writeListFixtures(t, fixtures...)
	svc := serviceOn(t, st, at)
	ctx := owner(context.Background())
	page, err := svc.ListRequirementsPage(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated {
		t.Fatal("a page whose increments exceed the page-wide cap did not report truncation")
	}
	total := 0
	truncatedRows := 0
	for _, row := range page.Requirements {
		total += len(row.Increments)
		if row.IncrementsTruncated {
			truncatedRows++
		}
		// increment_ids is UNCHANGED in meaning: it still carries every id the
		// aggregate holds, whether or not that row's statuses were read.
		if len(row.IncrementIDs) != application.MaxPageSize {
			t.Fatalf("row %s reports %d increment_ids, want the aggregate's full %d", row.RequirementID, len(row.IncrementIDs), application.MaxPageSize)
		}
	}
	if total > application.MaxPageSize {
		t.Fatalf("the page read %d increment statuses, above the cap of %d", total, application.MaxPageSize)
	}
	if truncatedRows == 0 {
		t.Fatal("the page reports truncation but no row says which one was cut")
	}
	t.Logf("page-wide increment budget bound the answer: %d statuses read across %d rows, %d rows marked truncated, page truncated=%v",
		total, len(page.Requirements), truncatedRows, page.Truncated)

	// The positive half: a page inside the budget reports no truncation at all,
	// so the flag is not simply always set.
	small := writeListFixtures(t, listFixture{
		requirementID: "req-small", status: domain.RequirementActive, text: "small",
		increments: []domain.Increment{listIncrement(t, "inc-small", "req-small", domain.IncrementReady)},
	})
	smallPage, err := serviceOn(t, small, at).ListRequirementsPage(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if smallPage.Truncated {
		t.Fatalf("a page of one Requirement with one Increment reported truncation: %+v", smallPage)
	}
}

// TestBacklogRepositoryFilterUsesTheExistingPortAndNeverFallsBack is A8 and
// escalation E22-7: an unknown repository id returns an EMPTY list and never
// the unfiltered page, the parameter composes with page_size and cursor, and
// a filtered page never contains an unlinked Requirement.
func TestBacklogRepositoryFilterUsesTheExistingPortAndNeverFallsBack(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	st := writeListFixtures(t,
		listFixture{requirementID: "req-linked-1", status: domain.RequirementReady, text: "linked one", repositoryID: "repo-alpha"},
		listFixture{requirementID: "req-linked-2", status: domain.RequirementReady, text: "linked two", repositoryID: "repo-alpha"},
		listFixture{requirementID: "req-other", status: domain.RequirementReady, text: "other repo", repositoryID: "repo-beta"},
		listFixture{requirementID: "req-unlinked", status: domain.RequirementReady, text: "no link"},
	)
	svc := serviceOn(t, st, at)
	ctx := owner(context.Background())

	unfiltered, err := svc.ListRequirementsPage(ctx, "", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Requirements) != 4 {
		t.Fatalf("the unfiltered page carries %d rows, want 4", len(unfiltered.Requirements))
	}
	if unfiltered.Filter != nil {
		t.Fatalf("an unfiltered page reported a filter object: %+v", unfiltered.Filter)
	}

	filtered, err := svc.ListRequirementsPageFiltered(ctx, "", 25, "repo-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Requirements) != 2 {
		t.Fatalf("the filtered page carries %d rows, want 2: %+v", len(filtered.Requirements), filtered.Requirements)
	}
	if len(filtered.Requirements) == len(unfiltered.Requirements) {
		t.Fatal("the filtered and unfiltered pages carry the same number of rows; the parameter is being ignored (E22-7)")
	}
	for _, row := range filtered.Requirements {
		if row.RepositoryID != "repo-alpha" {
			t.Fatalf("the filtered page carries %s, linked to %q, not repo-alpha", row.RequirementID, row.RepositoryID)
		}
	}
	if filtered.Filter == nil || filtered.Filter.RepositoryID != "repo-alpha" {
		t.Fatalf("the filtered page does not report its filter: %+v", filtered.Filter)
	}
	if filtered.Filter.LinkedIDsRead != 2 || filtered.Filter.LinkedIDsBounded {
		t.Fatalf("the filter report does not surface the port's bound truthfully: %+v", filtered.Filter)
	}
	if filtered.Filter.Bound != application.MaxPageSize || filtered.Filter.Reason == "" {
		t.Fatalf("the filter report hides the bound it was called with: %+v", filtered.Filter)
	}

	// AN UNKNOWN REPOSITORY ID RETURNS AN EMPTY LIST, NOT THE UNFILTERED PAGE.
	// This is the exact defect E22-7 measured.
	unknown, err := svc.ListRequirementsPageFiltered(ctx, "", 25, "repo-does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Requirements) != 0 {
		t.Fatalf("an unknown repository id returned %d rows, want 0: %+v", len(unknown.Requirements), unknown.Requirements)
	}
	if unknown.Requirements == nil {
		t.Fatal("an empty filtered page must marshal as [] and not null")
	}
	if unknown.NextCursor != "" {
		t.Fatalf("an empty filtered page issued a cursor: %q", unknown.NextCursor)
	}

	// COMPOSITION WITH page_size AND cursor.
	firstOf, err := svc.ListRequirementsPageFiltered(ctx, "", 1, "repo-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(firstOf.Requirements) != 1 || firstOf.NextCursor == "" {
		t.Fatalf("page_size=1 over a two-Requirement Repository: rows=%d cursor=%q", len(firstOf.Requirements), firstOf.NextCursor)
	}
	secondOf, err := svc.ListRequirementsPageFiltered(ctx, firstOf.NextCursor, 1, "repo-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(secondOf.Requirements) != 1 || secondOf.NextCursor != "" {
		t.Fatalf("the second filtered page: rows=%d cursor=%q", len(secondOf.Requirements), secondOf.NextCursor)
	}
	walked := map[string]int{
		firstOf.Requirements[0].RequirementID:  1,
		secondOf.Requirements[0].RequirementID: 1,
	}
	if len(walked) != 2 {
		t.Fatalf("the two filtered pages covered the same Requirement twice: %v", walked)
	}

	// A MALFORMED VALUE IS A 400-SHAPED CALLER FAULT, NOT A 500.
	for _, bad := range []string{" ", "\t", strings.Repeat("x", application.MaxRepositoryFilterID+1)} {
		_, err := svc.ListRequirementsPageFiltered(ctx, "", 25, bad)
		if err == nil {
			t.Fatalf("a malformed repository_id %q was accepted", bad)
		}
		if !errors.Is(err, application.ErrInvalidRequest) {
			t.Fatalf("a malformed repository_id %q produced %v, which is not a caller fault", bad, err)
		}
	}
}
