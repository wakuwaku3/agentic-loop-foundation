package application_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// loop returns a context whose Caller represents the Loop deciding on its
// own, mirroring how owner()/runner() already model the other two
// authenticated principals in this package's tests.
func loop(ctx context.Context, subject string) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleScheduler, Subject: subject})
}

// TestCaptureRecordsOwnerRequestedBy proves an owner-issued Requirement
// intake is attributed to the owner, using the same authenticated-session
// subject callerActor already resolves for every other owner-only command
// (local dev session subject today, IAP subject in production).
func TestCaptureRecordsOwnerRequestedBy(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	out, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-owner", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestedBy.ActorType != domain.ActorTypeOwner || out.RequestedBy.Subject == "" {
		t.Fatalf("capture response requested_by = %+v, want owner with a non-empty subject", out.RequestedBy)
	}
	detail, ok, err := s.GetRequirementDetail(owner(context.Background()), out.RequirementID)
	if err != nil || !ok {
		t.Fatalf("detail lookup failed: ok=%v err=%v", ok, err)
	}
	if detail.RequestedBy == nil || detail.RequestedBy.ActorType != domain.ActorTypeOwner {
		t.Fatalf("stored requirement requested_by = %+v, want owner", detail.RequestedBy)
	}
}

// TestCaptureRecordsLoopRequestedBy proves that when the Loop itself is the
// origin (RoleScheduler), the record is distinguishable from an owner
// request: same operation, different actor_type, and the subject carries the
// Loop's own component identifier rather than an owner session subject.
func TestCaptureRecordsLoopRequestedBy(t *testing.T) {
	s, _ := service()
	out, err := s.Capture(loop(context.Background(), "reconciler.self-intake"), application.CaptureRequest{RequestID: "cap-loop", Text: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestedBy.ActorType != domain.ActorTypeLoop || out.RequestedBy.Subject != "reconciler.self-intake" {
		t.Fatalf("capture response requested_by = %+v, want loop/reconciler.self-intake", out.RequestedBy)
	}
	detail, ok, err := s.GetRequirementDetail(owner(context.Background()), out.RequirementID)
	if err != nil || !ok {
		t.Fatalf("detail lookup failed: ok=%v err=%v", ok, err)
	}
	if detail.RequestedBy == nil || detail.RequestedBy.ActorType != domain.ActorTypeLoop || detail.RequestedBy.Subject != "reconciler.self-intake" {
		t.Fatalf("stored requirement requested_by = %+v, want loop/reconciler.self-intake", detail.RequestedBy)
	}
}

// TestCaptureStillRejectsRunnerRole proves widening Capture to accept the
// Loop's own RoleScheduler did not also open it to RoleRunner: a claimed
// Runner session remains unable to intake Requirements.
func TestCaptureStillRejectsRunnerRole(t *testing.T) {
	s, _ := service()
	if _, err := s.Capture(runner(context.Background(), "r1"), application.CaptureRequest{RequestID: "cap-runner"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
}

// TestControlRecordsOwnerAndLoopRequestedBy proves both origins are
// distinguishable for Control Intents too. domain.ControlIntent itself
// (internal/domain/control.go) is unmodified M1-closed surface, so this also
// exercises the side-table path (SaveControlRequestedBy/ControlRequestedBy)
// that carries the value across the same transaction as SaveControl.
func TestControlRecordsOwnerAndLoopRequestedBy(t *testing.T) {
	s, _ := service()
	now := clock{}.Now()
	ownerOut, err := s.Control(owner(context.Background()), application.ControlRequest{RequestID: "ctl-owner", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlPauseIntake, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if ownerOut.RequestedBy.ActorType != domain.ActorTypeOwner {
		t.Fatalf("owner control requested_by = %+v", ownerOut.RequestedBy)
	}
	loopOut, err := s.Control(loop(context.Background(), "reconciler.runaway-guard"), application.ControlRequest{RequestID: "ctl-loop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlAllow, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if loopOut.RequestedBy.ActorType != domain.ActorTypeLoop || loopOut.RequestedBy.Subject != "reconciler.runaway-guard" {
		t.Fatalf("loop control requested_by = %+v", loopOut.RequestedBy)
	}
	rows, err := s.ListControls(owner(context.Background()), 25)
	if err != nil {
		t.Fatal(err)
	}
	found := map[domain.Revision]*domain.RequestedBy{}
	for i := range rows {
		found[rows[i].Revision] = rows[i].RequestedBy
	}
	if rb := found[ownerOut.Revision]; rb == nil || rb.ActorType != domain.ActorTypeOwner {
		t.Fatalf("listed control revision %d requested_by = %+v, want owner", ownerOut.Revision, rb)
	}
	if rb := found[loopOut.Revision]; rb == nil || rb.ActorType != domain.ActorTypeLoop || rb.Subject != "reconciler.runaway-guard" {
		t.Fatalf("listed control revision %d requested_by = %+v, want loop/reconciler.runaway-guard", loopOut.Revision, rb)
	}
}

// TestLegacyRequirementWithoutRequestedByStillReads proves a pre-existing
// record saved before this field existed (RequestedBy is its Go zero value)
// remains valid and readable: Validate accepts it, and the owner read model
// omits requested_by entirely rather than surfacing an empty placeholder.
func TestLegacyRequirementWithoutRequestedByStillReads(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	legacy := domain.Requirement{ID: domain.RequirementID("legacy-1"), Status: domain.RequirementCaptured, Version: 1}
	if err := domain.Validate(legacy); err != nil {
		t.Fatalf("legacy record (no requested_by) should validate: %v", err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveRequirement(ctx, legacy, 0); err != nil {
			return err
		}
		return u.SaveRequirementText(ctx, "legacy-1", "pre-existing")
	}); err != nil {
		t.Fatal(err)
	}
	detail, ok, err := s.GetRequirementDetail(owner(ctx), "legacy-1")
	if err != nil || !ok {
		t.Fatalf("legacy requirement should still be readable: ok=%v err=%v", ok, err)
	}
	if detail.RequestedBy != nil {
		t.Fatalf("legacy requirement should have no requested_by, got %+v", detail.RequestedBy)
	}
	page, err := s.ListRequirementsPage(owner(ctx), "", 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Requirements {
		if r.RequirementID == "legacy-1" && r.RequestedBy != nil {
			t.Fatalf("legacy requirement in page should have no requested_by, got %+v", r.RequestedBy)
		}
	}
}

// TestRequestedByActorTypeIsAClosedEnum proves the domain layer rejects an
// actor_type outside {owner, loop} while continuing to accept the absent
// (legacy) value, using domain.Validate directly since RequestedBy's
// closed-enum guard lives in internal/domain/model.go.
func TestRequestedByActorTypeIsAClosedEnum(t *testing.T) {
	base := domain.Requirement{ID: domain.RequirementID("r-enum"), Status: domain.RequirementCaptured, Version: 1}

	base.RequestedBy = domain.RequestedBy{}
	if err := domain.Validate(base); err != nil {
		t.Fatalf("absent requested_by should validate: %v", err)
	}

	base.RequestedBy = domain.RequestedBy{ActorType: domain.ActorTypeOwner, Subject: "s"}
	if err := domain.Validate(base); err != nil {
		t.Fatalf("owner requested_by should validate: %v", err)
	}

	base.RequestedBy = domain.RequestedBy{ActorType: domain.ActorTypeLoop, Subject: "s"}
	if err := domain.Validate(base); err != nil {
		t.Fatalf("loop requested_by should validate: %v", err)
	}

	base.RequestedBy = domain.RequestedBy{ActorType: domain.ActorType("superuser"), Subject: "s"}
	if err := domain.Validate(base); err == nil {
		t.Fatal("an actor_type outside {owner, loop} should be rejected")
	}
}

// ===========================================================================
// V2-086: the scheduler becomes a role a real caller can be.
//
// The measurement this task repairs is a COUNT, so it is asserted as a count
// rather than described. At the parent commit the number of non-test sites in
// the whole repository that construct an application.Caller carrying
// RoleScheduler was ZERO -- every non-test appearance of the role was on the
// accepting side -- so domain.ActorTypeLoop was unreachable for a
// Requirement's, a Repository's, a link's and a Control Intent's requester and
// for the human-input answerer, because internal/application/caller.go's
// requestedBy is the sole producer of that value for all five. After this task
// the count is exactly ONE, and it is the return inside
// application.LoopCaller: the single sanctioned producer.
//
// The scan is repo-wide and mechanical rather than a grep in a comment,
// because the property it defends is "no OTHER package invents this identity
// for itself" and a reviewer cannot keep checking that. It walks the module
// from the repository root, parses every non-test *.go file with go/parser,
// and reports every composite literal of type Caller whose Role field names
// RoleScheduler, in either spelling (unqualified inside internal/application,
// package-qualified everywhere else). A zero-file scan is a Fatal so no
// assertion here can pass vacuously, and the matcher is exercised against a
// known-positive and a known-negative first.
// ===========================================================================

// schedulerProducerSite is one measured producer of a RoleScheduler Caller.
type schedulerProducerSite struct {
	path string // slash-separated, relative to the repository root
	line int
}

// schedulerProducerScanSkipDirs are the directories the walk does not enter.
// None of them holds Go source that is compiled into this module.
var schedulerProducerScanSkipDirs = map[string]bool{
	".git": true, "build": true, "node_modules": true, ".agents": true,
}

// callerLiteralNamesScheduler reports whether a composite literal is a Caller
// literal whose Role field is RoleScheduler. Both the type and the constant are
// matched in either spelling: bare inside internal/application, and through a
// package qualifier from anywhere else.
func callerLiteralNamesScheduler(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		if t.Name != "Caller" {
			return false
		}
	case *ast.SelectorExpr:
		if t.Sel == nil || t.Sel.Name != "Caller" {
			return false
		}
	default:
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Role" {
			continue
		}
		switch v := kv.Value.(type) {
		case *ast.Ident:
			if v.Name == "RoleScheduler" {
				return true
			}
		case *ast.SelectorExpr:
			if v.Sel != nil && v.Sel.Name == "RoleScheduler" {
				return true
			}
		}
	}
	return false
}

// scanSchedulerProducers walks the repository root and returns every non-test
// producer site, plus the number of files it actually parsed.
func scanSchedulerProducers(t *testing.T, root string) ([]schedulerProducerSite, int) {
	t.Helper()
	sites := []schedulerProducerSite{}
	parsed := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if schedulerProducerScanSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		parsed++
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			t.Fatalf("relative path of %s: %v", path, rerr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !callerLiteralNamesScheduler(lit) {
				return true
			}
			sites = append(sites, schedulerProducerSite{path: filepath.ToSlash(rel), line: fset.Position(lit.Pos()).Line})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return sites, parsed
}

// TestExactlyOneSanctionedSchedulerCallerProducerExists is V2-086 A4's
// measurement: 0 producers before this task, exactly 1 after, and the one is
// inside internal/application. Any other package inventing a scheduler
// identity for itself -- the defect internal/runner/orchestrator.go committed
// with RoleOwner -- fails here by name and line.
func TestExactlyOneSanctionedSchedulerCallerProducerExists(t *testing.T) {
	// The matcher is verified before the scan trusts it: both spellings of a
	// positive, and the two shapes that must NOT match.
	const controls = `package control

type Role string
type Caller struct {
	Role    Role
	Subject string
}

const (
	RoleOwner     Role = "owner"
	RoleScheduler Role = "scheduler"
)

var bare = Caller{Role: RoleScheduler, Subject: "a"}
var qualified = application.Caller{Role: application.RoleScheduler, Subject: "b"}
var owner = Caller{Role: RoleOwner, Subject: "c"}
var notACaller = Session{Role: RoleScheduler}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic_scheduler_producers.go", controls, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	matched := []string{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if ok && callerLiteralNamesScheduler(lit) {
			matched = append(matched, strconv.Itoa(fset.Position(lit.Pos()).Line))
		}
		return true
	})
	if len(matched) != 2 {
		t.Fatalf("matcher control: matched %d literals at lines %v, want exactly the two RoleScheduler Caller literals", len(matched), matched)
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	sites, parsed := scanSchedulerProducers(t, root)
	const wantMinParsedFiles = 40
	if parsed < wantMinParsedFiles {
		t.Fatalf("the scan parsed %d non-test .go files, want at least %d; the walk is not seeing the module and every assertion below would pass vacuously", parsed, wantMinParsedFiles)
	}
	const sanctioned = "internal/application/caller.go"
	outside := []schedulerProducerSite{}
	inside := []schedulerProducerSite{}
	for _, s := range sites {
		if s.path == sanctioned {
			inside = append(inside, s)
			continue
		}
		outside = append(outside, s)
	}
	for _, s := range outside {
		t.Errorf("%s:%d constructs a Caller with RoleScheduler; application.LoopCaller in %s is the only sanctioned producer of a scheduler identity", s.path, s.line, sanctioned)
	}
	if len(inside) != 1 {
		t.Fatalf("the scan parsed %d non-test files and found %d RoleScheduler Caller literals in %s (all sites: %v), want exactly 1 -- the return inside LoopCaller. Zero means no transport or in-process caller can be the scheduler at all, which is the defect V2-086 repairs; more than one means the single sanctioned producer was duplicated", parsed, len(inside), sanctioned, sites)
	}
	t.Logf("scheduler-producer scan parsed %d non-test .go files: 1 sanctioned producer at %s:%d and 0 anywhere else", parsed, inside[0].path, inside[0].line)
}

// TestLoopCallerIsTheOnlyProducerAndRefusesAnEmptySubject is V2-086 A4's
// refusal proof and its positive case in one run.
//
// The refusal is asserted, not described: a guard with no refusal test is not a
// guard. An empty subject, a subject that is only spaces and a subject that is
// only tabs and newlines each return ErrUnauthenticated and the Caller zero
// value, so no partially-formed scheduler identity can escape the constructor.
// The positive case then shows the value is not decorative: the same subject
// drives Capture and the stored Requirement reads domain.ActorTypeLoop, which
// no transport could produce before this task.
func TestLoopCallerIsTheOnlyProducerAndRefusesAnEmptySubject(t *testing.T) {
	for _, subject := range []string{"", " ", "   ", "\t", "\n", " \t\r\n "} {
		caller, err := application.LoopCaller(subject)
		if !errors.Is(err, application.ErrUnauthenticated) {
			t.Fatalf("LoopCaller(%q) error = %v, want ErrUnauthenticated", subject, err)
		}
		if caller != (application.Caller{}) {
			t.Fatalf("LoopCaller(%q) returned %#v alongside its error; a refused identity must be the zero value", subject, caller)
		}
	}

	caller, err := application.LoopCaller("reconciler.self-intake")
	if err != nil {
		t.Fatalf("LoopCaller with a real subject: %v", err)
	}
	if caller.Role != application.RoleScheduler {
		t.Fatalf("LoopCaller role = %q, want %q", caller.Role, application.RoleScheduler)
	}
	if caller.Subject != "reconciler.self-intake" {
		t.Fatalf("LoopCaller subject = %q, want the argument as given", caller.Subject)
	}
	if caller.RunnerID != "" {
		t.Fatalf("LoopCaller invented a RunnerID %q; the Loop is not a runner", caller.RunnerID)
	}

	// And it is a caller the commands that name the role actually accept, with
	// the attribution the domain distinguishes.
	s, _ := service()
	out, err := s.Capture(application.ContextWithCaller(context.Background(), caller), application.CaptureRequest{RequestID: "loopcaller-capture", Text: "the Loop's own intake"})
	if err != nil {
		t.Fatalf("Capture as the sanctioned loop caller: %v", err)
	}
	if out.RequestedBy.ActorType != domain.ActorTypeLoop || out.RequestedBy.Subject != "reconciler.self-intake" {
		t.Fatalf("capture response requested_by = %+v, want loop/reconciler.self-intake", out.RequestedBy)
	}
	detail, ok, err := s.GetRequirementDetail(owner(context.Background()), out.RequirementID)
	if err != nil || !ok {
		t.Fatalf("detail lookup failed: ok=%v err=%v", ok, err)
	}
	if detail.RequestedBy == nil || detail.RequestedBy.ActorType != domain.ActorTypeLoop {
		t.Fatalf("stored requirement requested_by = %+v, want loop", detail.RequestedBy)
	}
}

// TestRequestedByStaysAClosedRefusalForEveryOtherRole is V2-086 A13. Nothing in
// this task widens a closed set, and the way that is kept true is by asserting
// the set: requestedBy admits exactly RoleOwner and RoleScheduler, and its
// default arm is still a refusal, so an unlisted member still fails. The
// unlisted members checked are RoleRunner -- which askedBy DOES admit, so the
// two mappings must not be confused -- the zero Role, and an invented one.
func TestRequestedByStaysAClosedRefusalForEveryOtherRole(t *testing.T) {
	s, _ := service()
	admitted := map[application.Role]domain.ActorType{
		application.RoleOwner:     domain.ActorTypeOwner,
		application.RoleScheduler: domain.ActorTypeLoop,
	}
	n := 0
	for role, want := range admitted {
		n++
		ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: role, Subject: "subject-" + string(role)})
		out, err := s.Capture(ctx, application.CaptureRequest{RequestID: "closed-set-" + string(role), Text: "x"})
		if err != nil {
			t.Fatalf("Capture as %q: %v", role, err)
		}
		if out.RequestedBy.ActorType != want {
			t.Fatalf("Capture as %q recorded actor_type %q, want %q", role, out.RequestedBy.ActorType, want)
		}
	}
	if n != 2 {
		t.Fatalf("the admitted-role table has %d entries, want exactly 2", n)
	}
	for _, role := range []application.Role{application.RoleRunner, application.Role(""), application.Role("superuser")} {
		ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: role, Subject: "subject"})
		if _, err := s.Capture(ctx, application.CaptureRequest{RequestID: "closed-set-refused-" + string(role), Text: "x"}); !errors.Is(err, application.ErrForbidden) {
			t.Fatalf("Capture as %q returned %v, want ErrForbidden; the requester attribution set must stay closed", role, err)
		}
	}
}
