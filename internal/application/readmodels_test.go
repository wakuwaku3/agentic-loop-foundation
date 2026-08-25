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
	want := map[string]bool{"Capture": true, "Plan": true}
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
