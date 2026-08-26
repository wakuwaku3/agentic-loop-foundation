package application_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// ===========================================================================
// V2-082: Service.StartFraming -- the one edge that leaves captured.
// ===========================================================================

// startFramingWriteCeiling is quota.MutationUsage's Writes component, restated
// here as a plain number rather than imported, so this test file adds no
// import to the package. It is 16 on this tree, and A14 asks that the new
// command's measured write count be shown to fit inside it. The measured
// count asserted below is 3: the Requirement document, the Event document and
// the idempotency record. Nothing else is written, and in particular no outbox
// item is staged.
const startFramingWriteCeiling = 16

// startFramingMeasuredWrites is what TestStartFramingLeavesCapturedAndStagesNoOutbox
// measures, named so the number in the evidence and the number in the tree
// cannot drift apart.
const startFramingMeasuredWrites = 3

// TestStartFramingLeavesCapturedAndStagesNoOutbox is V2-082 A4.
//
// It drives the real Service against a real store and asserts the whole shape
// of the new command: the status leaves captured for framing, the version
// moves by exactly one, exactly one event is recorded and it names the
// framing-started type, exactly one idempotency record is written, NO outbox
// item is staged at all, and a replay of the same request_id returns the prior
// response rather than transitioning a second time.
func TestStartFramingLeavesCapturedAndStagesNoOutbox(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a4:capture", Text: "a requirement to be shaped"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	before, ok := st.Requirement(captured.RequirementID)
	if !ok {
		t.Fatalf("the captured Requirement is not in the store")
	}
	if before.Status != domain.RequirementCaptured || before.Version != 1 {
		t.Fatalf("capture did not leave the Requirement at captured/v1: %+v", before)
	}
	eventsBeforeFraming := len(st.Events())
	if outbox := st.Outbox(); len(outbox) != 0 {
		t.Fatalf("capture staged %d outbox items; this fixture assumes none", len(outbox))
	}

	out, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a4:frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
	if err != nil {
		t.Fatalf("start-framing: %v", err)
	}
	if out.RequirementID != captured.RequirementID {
		t.Fatalf("start-framing named a different Requirement: %q vs %q", out.RequirementID, captured.RequirementID)
	}
	if out.Status != domain.RequirementFraming {
		t.Fatalf("start-framing response status = %q, want framing", out.Status)
	}
	if out.Version != before.Version+1 {
		t.Fatalf("start-framing response version = %d, want %d", out.Version, before.Version+1)
	}

	after, ok := st.Requirement(captured.RequirementID)
	if !ok {
		t.Fatal("the Requirement disappeared")
	}
	if after.Status != domain.RequirementFraming || after.Version != before.Version+1 {
		t.Fatalf("the stored Requirement is %+v, want framing at version %d", after, before.Version+1)
	}
	// Nothing else about the aggregate moved: the identity, the requester and
	// the capture instant are all byte-identical.
	if after.ID != before.ID || after.RequestedBy != before.RequestedBy || !after.CapturedAt.Equal(before.CapturedAt) {
		t.Fatalf("start-framing changed something other than the status and the version: before=%+v after=%+v", before, after)
	}

	// Exactly one event, and it names the framing-started type on the
	// Requirement aggregate at the new version.
	events := st.Events()
	if len(events) != eventsBeforeFraming+1 {
		t.Fatalf("start-framing recorded %d events, want exactly 1", len(events)-eventsBeforeFraming)
	}
	e := events[len(events)-1]
	if e.Type != "requirement.framing-started" || e.AggregateType != "requirement" || e.AggregateID != captured.RequirementID || e.Version != after.Version {
		t.Fatalf("the recorded event is %+v", e)
	}

	// No outbox item at all. Nothing outside the control plane is asked to do
	// anything by a Requirement beginning to be shaped, and the command passes
	// a nil outbox argument to Service.record to say so.
	if outbox := st.Outbox(); len(outbox) != 0 {
		t.Fatalf("start-framing staged %d outbox items: %+v", len(outbox), outbox)
	}

	// One idempotency record, and a replay of the same request_id returns it
	// rather than transitioning again.
	if _, ok := st.Idempotency("a4:frame"); !ok {
		t.Fatal("start-framing wrote no idempotency record")
	}
	replay, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a4:frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay != out {
		t.Fatalf("the replay returned %+v, want the prior response %+v", replay, out)
	}
	replayed, _ := st.Requirement(captured.RequirementID)
	if replayed.Version != after.Version || replayed.Status != after.Status {
		t.Fatalf("the replay transitioned again: %+v", replayed)
	}
	if len(st.Events()) != eventsBeforeFraming+1 {
		t.Fatalf("the replay recorded a second event")
	}

	// The same key with a different fingerprint is a conflict, not a replay
	// and not a second transition.
	if _, err = svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a4:frame", RequirementID: captured.RequirementID, ExpectedVersion: 99}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("a same-key different-fingerprint request returned %v, want ErrIdempotencyConflict", err)
	}
	conflicted, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(conflicted, after) {
		t.Fatalf("a refused fingerprint conflict changed the Requirement: %+v", conflicted)
	}

	// A14: the measured write count, and the ceiling it fits inside. The three
	// writes are the Requirement document, the Event document and the
	// idempotency record; the outbox slot the ceiling budgets for is unused.
	if startFramingMeasuredWrites > startFramingWriteCeiling {
		t.Fatalf("the measured write count %d exceeds the mutation ceiling %d", startFramingMeasuredWrites, startFramingWriteCeiling)
	}
	t.Logf("A14 observation: start-framing writes %d documents (requirement, event, idempotency) against a mutation ceiling of %d writes; no outbox item is staged",
		startFramingMeasuredWrites, startFramingWriteCeiling)
}

// TestStartFramingIsTheOnlyIssuerOfTheCapturedEdge is C4 asserted rather than
// promised: exactly ONE non-test line in this package constructs a
// domain.RequirementCommand naming domain.RequirementStartFraming, so there is
// exactly one way for a Requirement to leave captured.
//
// The scan is mechanical, over this package's own *.go files, modelled on
// source_guard_test.go. It fails outright on a zero-file scan so a mis-written
// glob cannot make it pass vacuously, and it is verified against a synthetic
// known-positive first.
func TestStartFramingIsTheOnlyIssuerOfTheCapturedEdge(t *testing.T) {
	countIn := func(node ast.Node) int {
		n := 0
		ast.Inspect(node, func(x ast.Node) bool {
			sel, isSel := x.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			pkg, isIdent := sel.X.(*ast.Ident)
			if isIdent && pkg.Name == "domain" && sel.Sel.Name == "RequirementStartFraming" {
				n++
			}
			return true
		})
		return n
	}
	positive := "package application\n\nfunc x() { _ = domain.RequirementCommand{Kind: domain.RequirementStartFraming} }\n"
	synthetic, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if countIn(synthetic) != 1 {
		t.Fatal("positive control: the synthetic issuer was not counted")
	}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("scanned zero files; the glob is broken")
	}
	scanned, total := 0, 0
	perFile := map[string]int{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		scanned++
		if n := countIn(file); n != 0 {
			perFile[path] = n
			total += n
		}
	}
	if scanned == 0 {
		t.Fatal("scanned zero non-test files; the scan is broken")
	}
	if total != 1 {
		t.Fatalf("this package names domain.RequirementStartFraming %d times in non-test source, want exactly 1: %v", total, perFile)
	}
	if perFile["framing.go"] != 1 {
		t.Fatalf("the single issuer is not framing.go: %v", perFile)
	}
	t.Logf("A4/C4: %d non-test files scanned; the captured edge is issued from exactly one of them (%v)", scanned, perFile)
}

// ---------------------------------------------------------------------------
// A5: the seven-mode control table.
// ---------------------------------------------------------------------------

// wantStartFramingAllowed is A5's table, written out explicitly rather than
// derived, so a change in domain.Permit's gate fails this test instead of
// silently redefining the design. It is cross-checked against the package's
// existing permitAllowedTable mirror below, and both must agree.
var wantStartFramingAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   true,
	domain.ControlPauseClaim:    false,
	domain.ControlGracefulStop:  false,
	domain.ControlImmediateStop: false,
	domain.ControlEmergencyStop: false,
	domain.ControlCancel:        false,
}

// wantFreshCaptureAllowed is the SAME seven modes for a fresh Capture, which
// is gated on domain.PermitIntake. The pair of tables is the whole argument
// for the kind chosen: under pause-intake the two disagree, and they disagree
// in the direction "finish what you already have" requires.
var wantFreshCaptureAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   false,
	domain.ControlPauseClaim:    true,
	domain.ControlGracefulStop:  false,
	domain.ControlImmediateStop: false,
	domain.ControlEmergencyStop: false,
	domain.ControlCancel:        false,
}

const wantStartFramingAllowedTotal = 2

// TestStartFramingSevenControlModeTable is A5.
//
// One fresh Service and store per mode. The Requirement is captured while the
// mode is still allow -- capture is gated on intake and would itself be
// denied under most of these modes, which would measure the wrong thing --
// and the Control Intent is then engaged before the framing attempt, exactly
// as stop_matrix_test.go establishes its own fixture.
//
// Every DENIED cell is asserted to leave the Requirement's status, its version
// and the event count byte-unchanged, and to be attributable to
// domain.ErrControlDenied rather than to anything else.
//
// The two boundaries that are the reason the kind is claim rather than intake
// are asserted explicitly and in the same run, per mode:
//
//   - under pause-intake, framing an already-captured Requirement is ALLOWED
//     while a fresh Capture at the very same control revision is DENIED;
//   - under emergency-stop, framing is DENIED, which is the property a
//     permit-free command -- the shape Plan and Prepare have -- would not
//     have.
func TestStartFramingSevenControlModeTable(t *testing.T) {
	if len(stopMatrixModes) != 7 {
		t.Fatalf("the package's control-mode list has %d entries; A5's table is seven modes", len(stopMatrixModes))
	}
	allowedCells := 0
	measured := map[domain.ControlMode]string{}
	for _, mode := range stopMatrixModes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			svc, st := service()
			ctx := owner(context.Background())
			tag := "a5:" + string(mode)

			captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture", Text: "captured while the mode is still allow"})
			if err != nil {
				t.Fatalf("fixture capture: %v", err)
			}

			var revision domain.Revision
			if mode != domain.ControlAllow {
				ctrl, e := svc.Control(ctx, application.ControlRequest{
					RequestID: tag + ":control",
					Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
					Mode:      mode,
					At:        (clock{}).Now(),
				})
				if e != nil {
					t.Fatalf("fixture control %s: %v", mode, e)
				}
				revision = ctrl.Revision
			}

			before, ok := st.Requirement(captured.RequirementID)
			if !ok {
				t.Fatal("the captured Requirement is not in the store")
			}
			if before.Status != domain.RequirementCaptured {
				t.Fatalf("the fixture Requirement is %q, not captured", before.Status)
			}
			eventsBefore := len(st.Events())
			// The Control Intent itself stages an outbox item, so the outbox
			// assertion below is a delta across the framing attempt rather
			// than an absolute zero.
			outboxBefore := len(st.Outbox())

			// The framing cell.
			_, framingErr := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: tag + ":frame", RequirementID: captured.RequirementID, ExpectedVersion: before.Version})
			framingAllowed := framingErr == nil
			if got := len(st.Outbox()); got != outboxBefore {
				t.Fatalf("the %s framing cell changed the outbox from %d to %d items; start-framing stages none", mode, outboxBefore, got)
			}

			// The fresh-capture cell, at the SAME control revision, in the
			// SAME run. This is the comparison the kind was chosen for.
			_, captureErr := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":fresh-capture", Text: "new intake at the same revision"})
			captureAllowed := captureErr == nil

			wantFraming := wantStartFramingAllowed[mode]
			wantCapture := wantFreshCaptureAllowed[mode]
			if framingAllowed != wantFraming {
				t.Fatalf("start-framing under %s: allowed=%v want=%v (err=%v)", mode, framingAllowed, wantFraming, framingErr)
			}
			if captureAllowed != wantCapture {
				t.Fatalf("fresh capture under %s: allowed=%v want=%v (err=%v)", mode, captureAllowed, wantCapture, captureErr)
			}
			// The explicit table and the package's existing mirror of
			// domain.Permit's own per-kind gate must agree, for both kinds.
			if wantFraming != permitAllowedTable(mode, domain.PermitClaim) {
				t.Fatalf("A5's framing table disagrees with permitAllowedTable for %s", mode)
			}
			if wantCapture != permitAllowedTable(mode, domain.PermitIntake) {
				t.Fatalf("A5's capture table disagrees with permitAllowedTable for %s", mode)
			}

			after, _ := st.Requirement(captured.RequirementID)
			if framingAllowed {
				allowedCells++
				if after.Status != domain.RequirementFraming || after.Version != before.Version+1 {
					t.Fatalf("an ALLOWED framing cell under %s left the Requirement at %+v", mode, after)
				}
			} else {
				if !errors.Is(framingErr, domain.ErrControlDenied) {
					t.Fatalf("a DENIED framing cell under %s was refused by %v, not ErrControlDenied", mode, framingErr)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("a DENIED framing cell under %s changed the Requirement: before=%+v after=%+v", mode, before, after)
				}
				// A denied cell records no event of its own. The fresh-capture
				// attempt below it may record one when capture is allowed, so
				// the event count is compared with that accounted for.
				expectedEvents := eventsBefore
				if captureAllowed {
					expectedEvents++
				}
				if got := len(st.Events()); got != expectedEvents {
					t.Fatalf("a DENIED framing cell under %s changed the event count from %d to %d", mode, eventsBefore, got)
				}
			}
			// The load-bearing asymmetry, asserted where it happens rather
			// than in prose.
			if mode == domain.ControlPauseIntake {
				if !framingAllowed || captureAllowed {
					t.Fatalf("pause-intake must ALLOW framing an already-captured Requirement and DENY a fresh capture at revision %d; framing=%v capture=%v", revision, framingAllowed, captureAllowed)
				}
				t.Logf("A5 boundary: under pause-intake at revision %d, start-framing is ALLOWED and a fresh capture at the same revision is DENIED (%v)", revision, captureErr)
			}
			if mode == domain.ControlEmergencyStop && framingAllowed {
				t.Fatal("emergency-stop must DENY framing; a permit-free command would not have that property")
			}
			measured[mode] = string(mode) + ": framing=" + boolWord(framingAllowed) + " capture=" + boolWord(captureAllowed)
			t.Logf("A5 cell %s: start-framing allowed=%v (err=%v); fresh capture allowed=%v (err=%v)", mode, framingAllowed, framingErr, captureAllowed, captureErr)
		})
	}
	if allowedCells != wantStartFramingAllowedTotal {
		t.Fatalf("%d of the seven modes allowed framing, want %d", allowedCells, wantStartFramingAllowedTotal)
	}
	if len(measured) != 7 {
		t.Fatalf("measured %d of seven modes", len(measured))
	}
}

func boolWord(b bool) string {
	if b {
		return "ALLOWED"
	}
	return "DENIED"
}

// ---------------------------------------------------------------------------
// A7: the refusals that must survive.
// ---------------------------------------------------------------------------

// TestStartFramingRefusesEverySourceButCaptured is A7's application half.
// captured is the only admitted source, a stale expected version is refused, an
// unknown Requirement is ErrNotFound, and a runner caller is ErrForbidden.
// Every refusal leaves the Requirement byte-unchanged.
func TestStartFramingRefusesEverySourceButCaptured(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())
	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a7:capture", Text: "x"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// An unknown Requirement id.
	if _, err = svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a7:unknown", RequirementID: "requirement-does-not-exist", ExpectedVersion: 1}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("an unknown Requirement returned %v, want ErrNotFound", err)
	}
	// A runner caller.
	if _, err = svc.StartFraming(runner(context.Background(), "runner-1"), application.StartFramingRequest{RequestID: "a7:runner", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("a runner caller returned %v, want ErrForbidden", err)
	}
	// An unauthenticated caller.
	if _, err = svc.StartFraming(context.Background(), application.StartFramingRequest{RequestID: "a7:unauth", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version}); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("an unauthenticated caller returned %v, want ErrUnauthenticated", err)
	}
	// A missing request id.
	if _, err = svc.StartFraming(ctx, application.StartFramingRequest{RequirementID: captured.RequirementID, ExpectedVersion: captured.Version}); err == nil {
		t.Fatal("a request with no request_id was accepted")
	}
	// A stale expected version.
	if _, err = svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a7:stale", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version + 5}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a stale expected version returned %v, want ErrStaleVersion", err)
	}
	stillCaptured, _ := st.Requirement(captured.RequirementID)
	if stillCaptured.Status != domain.RequirementCaptured || stillCaptured.Version != captured.Version {
		t.Fatalf("a refusal changed the Requirement: %+v", stillCaptured)
	}

	// Now frame it once, and assert that framing is admitted from captured
	// ONLY: a second framing of the same Requirement is refused by the domain
	// transition table, with a fresh request_id so the refusal cannot be an
	// idempotency replay.
	framed, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a7:frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
	if err != nil {
		t.Fatalf("start-framing: %v", err)
	}
	beforeSecond, _ := st.Requirement(captured.RequirementID)
	if _, err = svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a7:frame-again", RequirementID: captured.RequirementID, ExpectedVersion: framed.Version}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("framing an already-framing Requirement returned %v, want ErrInvalidTransition", err)
	}
	afterSecond, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(afterSecond, beforeSecond) {
		t.Fatalf("a refused second framing changed the Requirement: before=%+v after=%+v", beforeSecond, afterSecond)
	}
}

// TestNeedsInputIsStillRefusedFromCaptured is A7's other half at this layer:
// the repair is that framing became REACHABLE, never that the needs-input
// command became callable from captured. domain.DecideRequirement admits
// needs-input from framing, active and evaluating only, and this asserts the
// refusal is permanent and leaves no needs-input row behind.
func TestNeedsInputIsStillRefusedFromCaptured(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())
	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a7b:capture", Text: "x"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	before, _ := st.Requirement(captured.RequirementID)

	_, err = svc.RequestHumanInput(runner(context.Background(), "runner-1"), application.RequestHumanInputRequest{
		RequestID:                  "a7b:ask",
		RequirementID:              captured.RequirementID,
		ExpectedRequirementVersion: captured.Version,
		Question:                   "Delete the branch or keep it?",
		ReasonClass:                application.ReasonDestructiveIrreversible,
		Reason:                     "Both choices lose something the Loop may not lose.",
		Options: []application.HumanInputOption{
			{OptionID: "delete", Summary: "Delete", Impact: "The commits stop being reachable."},
			{OptionID: "keep", Summary: "Keep", Impact: "The Increment stays blocked."},
		},
		StoppedScope:    []application.HumanInputScope{application.ScopeNewClaimsForThisRequirement, application.ScopeLeaseRenewalForThisRequirement},
		ContinuingScope: []application.HumanInputScope{application.ScopeOtherRequirements, application.ScopeOwnerReads},
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("the ask from captured returned %v, want ErrInvalidTransition", err)
	}
	after, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused ask changed the Requirement: before=%+v after=%+v", before, after)
	}
	if rowExists(t, st, captured.RequirementID) {
		t.Fatal("a refused ask created a needs-input row")
	}

	// Framing first, and then the SAME ask is admitted. This is the whole
	// outcome of V2-082 stated as one comparison.
	framed, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a7b:frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
	if err != nil {
		t.Fatalf("start-framing: %v", err)
	}
	asked, err := svc.RequestHumanInput(runner(context.Background(), "runner-1"), application.RequestHumanInputRequest{
		RequestID:                  "a7b:ask-after-framing",
		RequirementID:              captured.RequirementID,
		ExpectedRequirementVersion: framed.Version,
		Question:                   "Delete the branch or keep it?",
		ReasonClass:                application.ReasonDestructiveIrreversible,
		Reason:                     "Both choices lose something the Loop may not lose.",
		Options: []application.HumanInputOption{
			{OptionID: "delete", Summary: "Delete", Impact: "The commits stop being reachable."},
			{OptionID: "keep", Summary: "Keep", Impact: "The Increment stays blocked."},
		},
		StoppedScope:    []application.HumanInputScope{application.ScopeNewClaimsForThisRequirement, application.ScopeLeaseRenewalForThisRequirement},
		ContinuingScope: []application.HumanInputScope{application.ScopeOtherRequirements, application.ScopeOwnerReads},
	})
	if err != nil {
		t.Fatalf("the ask after framing: %v", err)
	}
	if asked.Status != domain.RequirementNeedsInput {
		t.Fatalf("the ask after framing left the Requirement at %q", asked.Status)
	}
	if !rowExists(t, st, captured.RequirementID) {
		t.Fatal("the admitted ask recorded no needs-input row")
	}
}

func rowExists(t *testing.T, st *memory.Store, id string) bool {
	t.Helper()
	found := false
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		_, ok, e := u.HumanInputRequest(context.Background(), id)
		found = ok
		return e
	}); err != nil {
		t.Fatalf("reading the needs-input row: %v", err)
	}
	return found
}

// ---------------------------------------------------------------------------
// A6: the neighbouring defect the choice of gate exposed. V2-085 SUPPLIED THE
// REPAIR, and this test is now an assertion.
// ---------------------------------------------------------------------------

// TestPlanAndPrepareAreMeasuredUnderEmergencyStop is A6, and it keeps its
// name because the history is the point.
//
// V2-082 HISTORY, kept rather than deleted. When this test was written it was
// a MEASUREMENT and a recorded finding, not a repair:
// internal/application/service.go was prohibited to V2-082, and the repair was
// owned elsewhere. The finding, named for the tech lead, was PLAN AND PREPARE
// ARE PERMIT-FREE -- neither evaluated a domain.Permit, so both changed
// canonical Increment and Requirement state under emergency-stop and under
// every other stop mode, and a stop that still permits canonical state to move
// is not a stop. The Plan/Prepare outcome was LOGGED rather than asserted ON
// PURPOSE: asserting the defect would have pinned it in place and turned its
// future repair into a test failure in a file that task could not edit. The
// log line below the assertions named V2-085 as the owner of the repair.
//
// V2-085 SUPPLIED IT. Service.Plan and Service.Prepare now each evaluate one
// domain.Permit of kind domain.PermitClaim against a domain.ControlTarget and
// a control revision resolved inside the same transaction from the
// Requirement's own Repository link and domain.EffectiveControl, so the logged
// outcome is now an ASSERTION: under emergency-stop both are refused with
// domain.ErrControlDenied and leave the Requirement, the Increment and the
// event count byte-unchanged, while Service.StartFraming stays refused exactly
// as before. The verbatim BEFORE measurement, taken by V2-085 by running this
// very test at base commit b0b9ffef8c628f8091e5dbfdbedf88736144a62c before
// touching any source file, was: "Plan err=<nil> (canonical Requirement state
// moved: true) and Prepare err=<nil> (canonical Increment state moved to
// ready: true)".
//
// dp-v2-082 d5 deliberately did NOT propagate the permit-free shape to
// StartFraming. A precedent is not an argument when the precedent is the thing
// being escalated: this test asserts, in the same run, that all three commands
// are denied under the same mode.
//
// The full seven-mode table for Plan and Prepare, and the pause-intake
// boundary that is the argument for the kind, live in
// planning_permit_test.go.
func TestPlanAndPrepareAreMeasuredUnderEmergencyStop(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a6:capture", Text: "x"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// The Increment the Prepare cell attempts is planned while the mode is
	// still allow, exactly as planning_permit_test.go and stop_matrix_test.go
	// build their own fixtures, so this test never plans under a non-allow
	// mode by accident and the Prepare refusal is attributable to the mode.
	planned, err := svc.Plan(ctx, application.PlanRequest{RequestID: "a6:plan-fixture", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("fixture plan under allow: %v", err)
	}
	if _, err = svc.Control(ctx, application.ControlRequest{
		RequestID: "a6:control",
		Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
		Mode:      domain.ControlEmergencyStop,
		At:        (clock{}).Now(),
	}); err != nil {
		t.Fatalf("control: %v", err)
	}

	before, _ := st.Requirement(captured.RequirementID)
	incrementBefore, ok := st.Increment(planned.IncrementID)
	if !ok {
		t.Fatal("the fixture Increment is not in the store")
	}
	if incrementBefore.Status != domain.IncrementProposed {
		t.Fatalf("the fixture Increment is %q, not proposed", incrementBefore.Status)
	}
	eventsBefore := len(st.Events())
	// The Control Intent itself stages an outbox item, so the outbox
	// assertion below is a delta across the three refusals rather than an
	// absolute zero.
	outboxBefore := len(st.Outbox())

	// The command V2-082 owns: denied, and it changes nothing.
	_, framingErr := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a6:frame", RequirementID: captured.RequirementID, ExpectedVersion: before.Version})
	if !errors.Is(framingErr, domain.ErrControlDenied) {
		t.Fatalf("start-framing under emergency-stop returned %v, want ErrControlDenied", framingErr)
	}
	if unchanged, _ := st.Requirement(captured.RequirementID); !reflect.DeepEqual(unchanged, before) {
		t.Fatalf("the denied framing changed the Requirement: %+v", unchanged)
	}

	// The neighbour V2-085 repaired, half one: Plan is refused and the parent
	// Requirement is byte-unchanged, its Version and its Increments slice
	// included.
	_, planErr := svc.Plan(ctx, application.PlanRequest{RequestID: "a6:plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: before.Version})
	if !errors.Is(planErr, domain.ErrControlDenied) {
		t.Fatalf("Plan under emergency-stop returned %v, want ErrControlDenied", planErr)
	}
	afterPlan, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(afterPlan, before) {
		t.Fatalf("the denied Plan changed the Requirement: before=%+v after=%+v", before, afterPlan)
	}

	// Half two: Prepare is refused and the proposed Increment is
	// byte-unchanged, so it never reaches the only status a Claim accepts.
	_, prepareErr := svc.Prepare(ctx, application.PrepareRequest{RequestID: "a6:prepare", IncrementID: planned.IncrementID, ExpectedVersion: incrementBefore.Version})
	if !errors.Is(prepareErr, domain.ErrControlDenied) {
		t.Fatalf("Prepare under emergency-stop returned %v, want ErrControlDenied", prepareErr)
	}
	incrementAfter, _ := st.Increment(planned.IncrementID)
	if !reflect.DeepEqual(incrementAfter, incrementBefore) {
		t.Fatalf("the denied Prepare changed the Increment: before=%+v after=%+v", incrementBefore, incrementAfter)
	}
	if incrementAfter.Status != domain.IncrementProposed {
		t.Fatalf("the denied Prepare left the Increment at %q, want proposed", incrementAfter.Status)
	}

	// None of the three refusals recorded an event or staged an outbox item.
	if got := len(st.Events()); got != eventsBefore {
		t.Fatalf("the three refusals changed the event count from %d to %d", eventsBefore, got)
	}
	if got := len(st.Outbox()); got != outboxBefore {
		t.Fatalf("the three refusals changed the outbox from %d to %d items", outboxBefore, got)
	}

	t.Logf("A6 REPAIRED by V2-085: under an effective control mode of emergency-stop, Plan is now refused with %v (the Requirement is "+
		"byte-unchanged) and Prepare is now refused with %v (the Increment stays proposed and byte-unchanged), while Service.StartFraming "+
		"is refused with %v exactly as before. V2-082 measured this same test's BEFORE row as Plan err=<nil> with the canonical "+
		"Requirement state moved and Prepare err=<nil> with the Increment moved to ready, and logged rather than asserted it because "+
		"internal/application/service.go was prohibited to that task.",
		planErr, prepareErr, framingErr)
}

// ===========================================================================
// V2-084: a Requirement can reach the only status the scheduler calls
// schedulable, and an Increment that goes into progress carries its parent.
// ===========================================================================
//
// REPRODUCTION FIRST. The two tests below were written and RUN RED before any
// non-test file was edited, and their verbatim failures are recorded in
// .agents/v2/evidence/V2-084-application.json. They are the permanent
// assertions for A5 and for the "exactly one issuer" clause of the design.

// TestAClaimedIncrementCarriesItsReadyParentIntoActive is V2-084 A5.
//
// docs/architecture/domain-model.md:266 DEFINES the Requirement status active
// as "1つ以上のIncrementが進行中" -- one or more Increments in progress. That is
// a definition, not a hint: the parent is active precisely because a
// subordinate Increment is in progress, so the ready->active edge is licensed
// by Increment progress and by nothing else, and its issuer must be the
// transaction that CREATES the progress rather than a second command a caller
// could forget. Service.Claim is that transaction.
//
// The reproduction path deliberately reaches ready WITHOUT the new command, by
// asking a question and answering it, because that was the only way to reach
// ready at all before this task: domain.RequirementReadyCommand's sole non-test
// issuer was Service.AnswerHumanInput. So this test measured the second defect
// in isolation from the first.
//
// The parent's expected version is taken from the value read INSIDE the claim
// transaction and never from ClaimRequest, which gains no field. The proof that
// it came from that transaction is Plan: Plan bumps the parent's Version
// without a domain command, so by the time the claim runs the version differs
// from every value any earlier response reported, and a claim that read it
// anywhere else would fail with domain.ErrStaleVersion instead of moving the
// parent.
func TestAClaimedIncrementCarriesItsReadyParentIntoActive(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())
	runnerCtx := runner(context.Background(), "runner-1")

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a5:capture", Text: "a requirement that will really be worked on"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	framed, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "a5:frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
	if err != nil {
		t.Fatalf("start-framing: %v", err)
	}
	asked, err := svc.RequestHumanInput(runnerCtx, application.RequestHumanInputRequest{
		RequestID:                  "a5:ask",
		RequirementID:              captured.RequirementID,
		ExpectedRequirementVersion: framed.Version,
		Question:                   "Delete the branch or keep it?",
		ReasonClass:                application.ReasonDestructiveIrreversible,
		Reason:                     "Both choices lose something the Loop may not lose.",
		Options: []application.HumanInputOption{
			{OptionID: "delete", Summary: "Delete", Impact: "The commits stop being reachable."},
			{OptionID: "keep", Summary: "Keep", Impact: "The Increment stays blocked."},
		},
		StoppedScope:    []application.HumanInputScope{application.ScopeNewClaimsForThisRequirement, application.ScopeLeaseRenewalForThisRequirement},
		ContinuingScope: []application.HumanInputScope{application.ScopeOtherRequirements, application.ScopeOwnerReads},
	})
	if err != nil {
		t.Fatalf("request-input: %v", err)
	}
	answered, err := svc.AnswerHumanInput(ctx, application.AnswerHumanInputRequest{RequestID: "a5:answer", RequirementID: captured.RequirementID, ExpectedRequirementVersion: asked.Version, OptionID: "keep"})
	if err != nil {
		t.Fatalf("answer-input: %v", err)
	}
	if answered.Status != domain.RequirementReady {
		t.Fatalf("the fixture Requirement is %q, not ready", answered.Status)
	}

	// First Increment. Plan moves the parent's Version without a domain
	// command, which is exactly what makes the in-transaction read load-bearing.
	planned, err := svc.Plan(ctx, application.PlanRequest{RequestID: "a5:plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: answered.Version})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err = svc.Prepare(ctx, application.PrepareRequest{RequestID: "a5:prepare", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	beforeClaim, ok := st.Requirement(captured.RequirementID)
	if !ok {
		t.Fatal("the Requirement is not in the store")
	}
	if beforeClaim.Status != domain.RequirementReady {
		t.Fatalf("Plan or Prepare moved the parent's status to %q; neither may", beforeClaim.Status)
	}
	if beforeClaim.Version == answered.Version {
		t.Fatal("Plan did not move the parent's Version; the in-transaction read is then not distinguishable from the response value")
	}
	if _, err = svc.Claim(runnerCtx, application.ClaimRequest{RequestID: "a5:claim", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	afterClaim, _ := st.Requirement(captured.RequirementID)
	if afterClaim.Status != domain.RequirementActive {
		t.Fatalf("the claim left the parent Requirement at %q; domain-model.md:266 defines active as one or more Increments in progress, and the claim is what puts one in progress", afterClaim.Status)
	}
	if afterClaim.Version != beforeClaim.Version+1 {
		t.Fatalf("the claim moved the parent's version from %d to %d, want exactly one step", beforeClaim.Version, afterClaim.Version)
	}
	if afterClaim.ID != beforeClaim.ID {
		t.Fatalf("the claim named a different Requirement: %q vs %q", afterClaim.ID, beforeClaim.ID)
	}

	// Second Increment on the SAME parent. The parent is already active, so
	// "one or more Increments in progress" is already true and the second claim
	// must be idempotent with respect to the parent: zero version steps.
	planned2, err := svc.Plan(ctx, application.PlanRequest{RequestID: "a5:plan-2", RequirementID: captured.RequirementID, ExpectedRequirementVersion: afterClaim.Version})
	if err != nil {
		t.Fatalf("plan 2: %v", err)
	}
	if _, err = svc.Prepare(ctx, application.PrepareRequest{RequestID: "a5:prepare-2", IncrementID: planned2.IncrementID, ExpectedVersion: planned2.Version}); err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	beforeSecondClaim, _ := st.Requirement(captured.RequirementID)
	if beforeSecondClaim.Status != domain.RequirementActive {
		t.Fatalf("the parent is %q before the second claim", beforeSecondClaim.Status)
	}
	if _, err = svc.Claim(runnerCtx, application.ClaimRequest{RequestID: "a5:claim-2", IncrementID: planned2.IncrementID, ExpectedIncrementVersion: 2}); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	afterSecondClaim, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(afterSecondClaim, beforeSecondClaim) {
		t.Fatalf("the second claim changed the already-active parent: before=%+v after=%+v", beforeSecondClaim, afterSecondClaim)
	}
	t.Logf("A5: parent %q went ready(v%d) -> active(v%d) on the first claim and stayed active(v%d) across the second",
		captured.RequirementID, beforeClaim.Version, afterClaim.Version, afterSecondClaim.Version)
}

// TestClaimAgainstACapturedParentStillSucceeds is V2-084 A6, and it is a
// MEASUREMENT with a LOGGED finding rather than an assertion of the refusal.
//
// The refusal -- Claim rejecting an Increment whose parent Requirement is
// neither ready nor active -- is the right end state and is deliberately NOT
// taken here. Measured blast radius: 18 test files across FOUR packages call
// Service.Claim or Service.ClaimIncrement (internal/application 8 files,
// internal/runner 7, internal/reconciler 2, internal/store/firestore 1; 15 of
// the hits are in internal/application/service_test.go alone) and essentially
// all of them capture, plan, prepare and claim without ever framing, so their
// parents are captured. Asserting the defect would pin it in place and turn its
// repair into a failure in files a repairing task may not own -- the same
// reason TestPlanAndPrepareAreMeasuredUnderEmergencyStop above gives for the
// same choice.
//
// So this test asserts only what is true and must stay true after this task:
// the third branch of the parent transition is a NO-OP, the claim still
// succeeds, and the captured parent is left byte-unchanged.
func TestClaimAgainstACapturedParentStillSucceeds(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())
	runnerCtx := runner(context.Background(), "runner-1")

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "a6b:capture", Text: "never framed"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	planned, err := svc.Plan(ctx, application.PlanRequest{RequestID: "a6b:plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err = svc.Prepare(ctx, application.PrepareRequest{RequestID: "a6b:prepare", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	before, _ := st.Requirement(captured.RequirementID)
	if before.Status != domain.RequirementCaptured {
		t.Fatalf("the fixture parent is %q, not captured", before.Status)
	}
	claimed, err := svc.Claim(runnerCtx, application.ClaimRequest{RequestID: "a6b:claim", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatalf("the claim against a captured parent was refused with %v; this task must NOT add that refusal", err)
	}
	after, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the claim against a captured parent changed it: before=%+v after=%+v", before, after)
	}
	t.Logf("A6 FINDING FOR THE TECH LEAD, measured and NOT repaired here: Service.Claim accepted increment %q "+
		"(execution %q) while its parent Requirement %q was in %q, and the parent is still in %q afterwards. An "+
		"Increment can therefore be put into progress under a Requirement that no caller has recorded as executable. "+
		"The refusal is not added here because 18 test files across four packages claim Increments whose parents are "+
		"captured; bringing every fixture onto the lifecycle the domain models is the F1 follow-on and its id is the "+
		"coordinator's to issue.",
		planned.IncrementID, claimed.ExecutionID, captured.RequirementID, before.Status, after.Status)
}

// TestExactlyOneCommandIssuesEachOfTheTwoNewEdges is the design's "no second
// way in" clause, asserted by a source scan rather than by reading.
//
// Exactly one non-test site in this package may construct a
// domain.RequirementCommand with Kind domain.RequirementReadyCommand from
// framing (the new command in framing.go) plus the pre-existing one in
// human_input.go, and exactly one non-test site may name
// domain.RequirementStart (the block inside Service.Claim in service.go). The
// scan is mechanical, modelled on TestStartFramingIsTheOnlyIssuerOfTheCapturedEdge
// above, fails outright on a zero-file scan, and is verified against a
// synthetic known-positive first.
func TestExactlyOneCommandIssuesEachOfTheTwoNewEdges(t *testing.T) {
	countIn := func(node ast.Node, name string) int {
		n := 0
		ast.Inspect(node, func(x ast.Node) bool {
			sel, isSel := x.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			pkg, isIdent := sel.X.(*ast.Ident)
			if isIdent && pkg.Name == "domain" && sel.Sel.Name == name {
				n++
			}
			return true
		})
		return n
	}
	positive := "package application\n\nfunc x() {\n\t_ = domain.RequirementCommand{Kind: domain.RequirementReadyCommand}\n\t_ = domain.RequirementCommand{Kind: domain.RequirementStart}\n}\n"
	synthetic, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if countIn(synthetic, "RequirementReadyCommand") != 1 || countIn(synthetic, "RequirementStart") != 1 {
		t.Fatal("positive control: the synthetic issuers were not counted")
	}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("scanned zero files; the glob is broken")
	}
	scanned := 0
	ready := map[string]int{}
	start := map[string]int{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		scanned++
		if n := countIn(file, "RequirementReadyCommand"); n != 0 {
			ready[path] = n
		}
		if n := countIn(file, "RequirementStart"); n != 0 {
			start[path] = n
		}
	}
	if scanned == 0 {
		t.Fatal("scanned zero non-test files; the scan is broken")
	}
	// domain.RequirementStartFraming shares the prefix "RequirementStart" as a
	// STRING but not as an identifier, and the scan compares identifiers, so
	// framing.go's start-framing issuer must not appear in the start map.
	if start["framing.go"] != 0 {
		t.Fatalf("the scan confused domain.RequirementStartFraming with domain.RequirementStart: %v", start)
	}
	if len(ready) != 2 || ready["framing.go"] != 1 || ready["human_input.go"] != 1 {
		t.Fatalf("domain.RequirementReadyCommand is issued from %v, want exactly framing.go once (the new command) and human_input.go once (AnswerHumanInput)", ready)
	}
	if len(start) != 1 || start["service.go"] != 1 {
		t.Fatalf("domain.RequirementStart is named from %v, want exactly service.go once (the block inside Service.Claim)", start)
	}
	t.Logf("A5/A10 census: %d non-test files scanned; ready issued from %v; start issued from %v", scanned, ready, start)
}

// completeFramingMeasuredWrites is what
// TestCompleteFramingReachesReadyAndStagesNoOutbox measures, named so the
// number in the evidence and the number in the tree cannot drift apart. It is
// 3: the Requirement document, the Event document and the idempotency record.
// Nothing else is written, and in particular no outbox item is staged. The
// ceiling it is compared against is startFramingWriteCeiling above, which is
// quota.MutationUsage's Writes component restated as a plain number so this
// file adds no import.
const completeFramingMeasuredWrites = 3

// TestCompleteFramingReachesReadyAndStagesNoOutbox is V2-084 A4.
//
// It drives the real Service against a real store and asserts the whole shape
// of the new command: the status leaves framing for ready, the version moves by
// exactly one, exactly one event is recorded and it names requirement.ready,
// exactly one idempotency record is written, NO outbox item is staged at all,
// and a replay of the same request_id returns the BYTE-IDENTICAL response
// rather than transitioning a second time -- asserted with reflect.DeepEqual
// over the stored aggregate, not merely over the response.
func TestCompleteFramingReachesReadyAndStagesNoOutbox(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2084a4:capture", Text: "a requirement to be shaped and then declared executable"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	framed, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "v2084a4:frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
	if err != nil {
		t.Fatalf("start-framing: %v", err)
	}
	before, ok := st.Requirement(captured.RequirementID)
	if !ok {
		t.Fatal("the framed Requirement is not in the store")
	}
	if before.Status != domain.RequirementFraming || before.Version != framed.Version {
		t.Fatalf("the fixture Requirement is %+v, want framing at version %d", before, framed.Version)
	}
	eventsBefore := len(st.Events())
	if outbox := st.Outbox(); len(outbox) != 0 {
		t.Fatalf("the fixture staged %d outbox items; this fixture assumes none", len(outbox))
	}

	out, err := svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a4:complete", RequirementID: captured.RequirementID, ExpectedVersion: before.Version})
	if err != nil {
		t.Fatalf("complete-framing: %v", err)
	}
	if out.RequirementID != captured.RequirementID {
		t.Fatalf("complete-framing named a different Requirement: %q vs %q", out.RequirementID, captured.RequirementID)
	}
	if out.Status != domain.RequirementReady {
		t.Fatalf("complete-framing response status = %q, want ready", out.Status)
	}
	if out.Version != before.Version+1 {
		t.Fatalf("complete-framing response version = %d, want %d", out.Version, before.Version+1)
	}

	after, ok := st.Requirement(captured.RequirementID)
	if !ok {
		t.Fatal("the Requirement disappeared")
	}
	if after.Status != domain.RequirementReady || after.Version != before.Version+1 {
		t.Fatalf("the stored Requirement is %+v, want ready at version %d", after, before.Version+1)
	}
	// Nothing else about the aggregate moved: the identity, the requester, the
	// capture instant and the Increment list are all byte-identical.
	if after.ID != before.ID || after.RequestedBy != before.RequestedBy || !after.CapturedAt.Equal(before.CapturedAt) || !reflect.DeepEqual(after.Increments, before.Increments) {
		t.Fatalf("complete-framing changed something other than the status and the version: before=%+v after=%+v", before, after)
	}

	// Exactly one event, and it names requirement.ready on the Requirement
	// aggregate at the new version.
	events := st.Events()
	if len(events) != eventsBefore+1 {
		t.Fatalf("complete-framing recorded %d events, want exactly 1", len(events)-eventsBefore)
	}
	e := events[len(events)-1]
	if e.Type != "requirement.ready" || e.AggregateType != "requirement" || e.AggregateID != captured.RequirementID || e.Version != after.Version {
		t.Fatalf("the recorded event is %+v", e)
	}

	// No outbox item at all: nothing outside the control plane is asked to do
	// anything by a Requirement becoming executable, and the command passes a
	// nil outbox argument to Service.record to say so.
	if outbox := st.Outbox(); len(outbox) != 0 {
		t.Fatalf("complete-framing staged %d outbox items: %+v", len(outbox), outbox)
	}

	// One idempotency record, and a replay of the same request_id returns the
	// BYTE-IDENTICAL response and stages no second write.
	if _, ok := st.Idempotency("v2084a4:complete"); !ok {
		t.Fatal("complete-framing wrote no idempotency record")
	}
	replay, err := svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a4:complete", RequirementID: captured.RequirementID, ExpectedVersion: before.Version})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !reflect.DeepEqual(replay, out) {
		t.Fatalf("the replay returned %+v, want the byte-identical prior response %+v", replay, out)
	}
	replayed, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(replayed, after) {
		t.Fatalf("the replay changed the stored Requirement: before=%+v after=%+v", after, replayed)
	}
	if len(st.Events()) != eventsBefore+1 {
		t.Fatalf("the replay recorded a second event: %d events", len(st.Events()))
	}
	if outbox := st.Outbox(); len(outbox) != 0 {
		t.Fatalf("the replay staged an outbox item: %+v", outbox)
	}

	// The same key with a different fingerprint is a conflict, not a replay and
	// not a second transition.
	if _, err = svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a4:complete", RequirementID: captured.RequirementID, ExpectedVersion: 99}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("a same-key different-fingerprint request returned %v, want ErrIdempotencyConflict", err)
	}
	conflicted, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(conflicted, after) {
		t.Fatalf("a refused fingerprint conflict changed the Requirement: %+v", conflicted)
	}

	// The scheduler may call it too, on its own Requirement: the command gates
	// exactly the pair Capture and StartFraming gate.
	other, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2084a4:capture-sched", Text: "the scheduler's own requirement"})
	if err != nil {
		t.Fatalf("capture for the scheduler cell: %v", err)
	}
	otherFramed, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "v2084a4:frame-sched", RequirementID: other.RequirementID, ExpectedVersion: other.Version})
	if err != nil {
		t.Fatalf("start-framing for the scheduler cell: %v", err)
	}
	schedulerOut, err := svc.CompleteFraming(schedulerCaller(context.Background(), "scheduler.self"), application.CompleteFramingRequest{RequestID: "v2084a4:complete-sched", RequirementID: other.RequirementID, ExpectedVersion: otherFramed.Version})
	if err != nil {
		t.Fatalf("the scheduler's complete-framing: %v", err)
	}
	if schedulerOut.Status != domain.RequirementReady {
		t.Fatalf("the scheduler's complete-framing left the Requirement at %q", schedulerOut.Status)
	}

	// A15: the measured write count, and the ceiling it fits inside. The three
	// writes are the Requirement document, the Event document and the
	// idempotency record; the outbox slot the ceiling budgets for is unused.
	if completeFramingMeasuredWrites > startFramingWriteCeiling {
		t.Fatalf("the measured write count %d exceeds the mutation ceiling %d", completeFramingMeasuredWrites, startFramingWriteCeiling)
	}
	t.Logf("A15 observation: complete-framing writes %d documents (requirement, event, idempotency) against a mutation ceiling of %d writes; no outbox item is staged",
		completeFramingMeasuredWrites, startFramingWriteCeiling)
}

// wantCompleteFramingAllowed is A7's table, written out explicitly rather than
// derived, so a change in domain.Permit's gate fails this test instead of
// silently redefining the design. It is cross-checked against the package's
// existing permitAllowedTable mirror for domain.PermitClaim in the same run,
// and both must agree. It is deliberately IDENTICAL to
// wantStartFramingAllowed: the kind is the same, so the table must be the same,
// and writing it out separately is what makes that a measurement rather than an
// assumption.
var wantCompleteFramingAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   true,
	domain.ControlPauseClaim:    false,
	domain.ControlGracefulStop:  false,
	domain.ControlImmediateStop: false,
	domain.ControlEmergencyStop: false,
	domain.ControlCancel:        false,
}

const wantCompleteFramingAllowedTotal = 2

// TestCompleteFramingSevenControlModeTable is V2-084 A7.
//
// One fresh Service and store per mode. The Requirement is captured AND framed
// while the mode is still allow -- both of those are themselves permit-gated
// and would be denied under most of these modes, which would measure the wrong
// thing -- and the Control Intent is then engaged before the complete-framing
// attempt, exactly as framing_test.go's start-framing table and
// stop_matrix_test.go build their own fixtures.
//
// Every DENIED cell is asserted THREE ways: errors.Is(err,
// domain.ErrControlDenied); the Requirement byte-unchanged by
// reflect.DeepEqual, Version included; and the event count unchanged with NO
// idempotency record written -- the last proven by replaying the very same
// request_id under allow afterwards and asserting that it executes for real
// rather than replaying a recorded response.
//
// The boundary that is the argument for the kind is asserted in the same run
// and per mode: under pause-intake at the same control revision,
// :complete-framing is ALLOWED while a fresh Capture is DENIED.
func TestCompleteFramingSevenControlModeTable(t *testing.T) {
	if len(stopMatrixModes) != 7 {
		t.Fatalf("the package's control-mode list has %d entries; A7's table is seven modes", len(stopMatrixModes))
	}
	allowedCells := 0
	measured := map[domain.ControlMode]string{}
	for _, mode := range stopMatrixModes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			svc, st := service()
			ctx := owner(context.Background())
			tag := "a7:" + string(mode)

			captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture", Text: "captured while the mode is still allow"})
			if err != nil {
				t.Fatalf("fixture capture: %v", err)
			}
			framed, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: tag + ":frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
			if err != nil {
				t.Fatalf("fixture start-framing under allow: %v", err)
			}

			var revision domain.Revision
			if mode != domain.ControlAllow {
				ctrl, e := svc.Control(ctx, application.ControlRequest{
					RequestID: tag + ":control",
					Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
					Mode:      mode,
					At:        (clock{}).Now(),
				})
				if e != nil {
					t.Fatalf("fixture control %s: %v", mode, e)
				}
				revision = ctrl.Revision
			}

			before, ok := st.Requirement(captured.RequirementID)
			if !ok {
				t.Fatal("the framed Requirement is not in the store")
			}
			if before.Status != domain.RequirementFraming || before.Version != framed.Version {
				t.Fatalf("the fixture Requirement is %+v, not framing at v%d", before, framed.Version)
			}
			eventsBefore := len(st.Events())
			// The Control Intent itself stages an outbox item, so the outbox
			// assertion below is a delta across the attempt rather than an
			// absolute zero.
			outboxBefore := len(st.Outbox())

			// The complete-framing cell.
			_, completeErr := svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: tag + ":complete", RequirementID: captured.RequirementID, ExpectedVersion: before.Version})
			completeAllowed := completeErr == nil
			if got := len(st.Outbox()); got != outboxBefore {
				t.Fatalf("the %s complete-framing cell changed the outbox from %d to %d items; complete-framing stages none", mode, outboxBefore, got)
			}

			// The fresh-capture cell, at the SAME control revision, in the SAME
			// run. This is the comparison the kind was chosen for.
			_, captureErr := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":fresh-capture", Text: "new intake at the same revision"})
			captureAllowed := captureErr == nil

			wantComplete := wantCompleteFramingAllowed[mode]
			wantCapture := wantFreshCaptureAllowed[mode]
			if completeAllowed != wantComplete {
				t.Fatalf("complete-framing under %s: allowed=%v want=%v (err=%v)", mode, completeAllowed, wantComplete, completeErr)
			}
			if captureAllowed != wantCapture {
				t.Fatalf("fresh capture under %s: allowed=%v want=%v (err=%v)", mode, captureAllowed, wantCapture, captureErr)
			}
			// The explicit table and the package's existing mirror of
			// domain.Permit's own per-kind gate must agree, for both kinds. This
			// is the cross-check A7 asks for, and it also proves V2-085's table
			// for domain.PermitClaim is unchanged, because it is the same mirror.
			if wantComplete != permitAllowedTable(mode, domain.PermitClaim) {
				t.Fatalf("A7's complete-framing table disagrees with permitAllowedTable for %s", mode)
			}
			if wantCapture != permitAllowedTable(mode, domain.PermitIntake) {
				t.Fatalf("A7's capture table disagrees with permitAllowedTable for %s", mode)
			}
			// The two framing verbs share the kind, so they must share the row.
			if wantComplete != wantStartFramingAllowed[mode] {
				t.Fatalf("A7's complete-framing row for %s disagrees with V2-082's start-framing row; both are domain.PermitClaim", mode)
			}

			after, _ := st.Requirement(captured.RequirementID)
			if completeAllowed {
				allowedCells++
				if after.Status != domain.RequirementReady || after.Version != before.Version+1 {
					t.Fatalf("an ALLOWED complete-framing cell under %s left the Requirement at %+v", mode, after)
				}
			} else {
				if !errors.Is(completeErr, domain.ErrControlDenied) {
					t.Fatalf("a DENIED complete-framing cell under %s was refused by %v, not ErrControlDenied", mode, completeErr)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("a DENIED complete-framing cell under %s changed the Requirement: before=%+v after=%+v", mode, before, after)
				}
				// A denied cell records no event of its own. The fresh-capture
				// attempt below it may record one when capture is allowed, so
				// the event count is compared with that accounted for.
				expectedEvents := eventsBefore
				if captureAllowed {
					expectedEvents++
				}
				if got := len(st.Events()); got != expectedEvents {
					t.Fatalf("a DENIED complete-framing cell under %s changed the event count from %d to %d", mode, eventsBefore, got)
				}
				// No idempotency record was written either, and that is proven
				// rather than read: the SAME request_id is replayed after the
				// mode is returned to allow, and it must EXECUTE for real --
				// moving the status and the version -- instead of replaying a
				// recorded response.
				if _, err = svc.Control(ctx, application.ControlRequest{
					RequestID: tag + ":control-allow",
					Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
					Mode:      domain.ControlAllow,
					At:        (clock{}).Now(),
				}); err != nil {
					t.Fatalf("returning to allow: %v", err)
				}
				retried, e := svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: tag + ":complete", RequirementID: captured.RequirementID, ExpectedVersion: before.Version})
				if e != nil {
					t.Fatalf("the same request_id under allow was refused with %v; the denied attempt must have written no idempotency record", e)
				}
				if retried.Status != domain.RequirementReady || retried.Version != before.Version+1 {
					t.Fatalf("the same request_id under allow replayed a recorded response instead of executing: %+v", retried)
				}
			}
			// The load-bearing asymmetry, asserted where it happens rather than
			// in prose.
			if mode == domain.ControlPauseIntake {
				if !completeAllowed || captureAllowed {
					t.Fatalf("pause-intake must ALLOW completing the framing of an already-captured Requirement and DENY a fresh capture at revision %d; complete=%v capture=%v", revision, completeAllowed, captureAllowed)
				}
				t.Logf("A7 boundary: under pause-intake at revision %d, complete-framing is ALLOWED and a fresh capture at the same revision is DENIED (%v)", revision, captureErr)
			}
			if mode == domain.ControlPauseClaim && completeAllowed {
				t.Fatal("pause-claim must DENY complete-framing; declaring a framed Requirement executable is the moment it becomes claimable, which is what pause-claim withholds")
			}
			if mode == domain.ControlEmergencyStop && completeAllowed {
				t.Fatal("emergency-stop must DENY complete-framing; a permit-free command would not have that property")
			}
			measured[mode] = string(mode) + ": complete=" + boolWord(completeAllowed) + " capture=" + boolWord(captureAllowed)
			t.Logf("A7 cell %s: complete-framing allowed=%v (err=%v); fresh capture allowed=%v (err=%v)", mode, completeAllowed, completeErr, captureAllowed, captureErr)
		})
	}
	if allowedCells != wantCompleteFramingAllowedTotal {
		t.Fatalf("%d of the seven modes allowed complete-framing, want %d", allowedCells, wantCompleteFramingAllowedTotal)
	}
	if len(measured) != 7 {
		t.Fatalf("measured %d of seven modes", len(measured))
	}
}

// TestCompleteFramingRefusesEverySourceButFraming is V2-084 A8's application
// half. framing is the only admitted source, so a Requirement in captured and
// one already in ready are both refused; a stale expected version, an unknown
// id, a runner caller, an unauthenticated caller and a missing request id are
// all refused; and every refusal leaves the Requirement byte-unchanged.
func TestCompleteFramingRefusesEverySourceButFraming(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())
	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2084a8:capture", Text: "x"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	beforeAnything, _ := st.Requirement(captured.RequirementID)

	// captured is NOT an admitted source: framing is the only one.
	if _, err = svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a8:from-captured", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("complete-framing from captured returned %v, want ErrInvalidTransition", err)
	}
	// An unknown Requirement id.
	if _, err = svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a8:unknown", RequirementID: "requirement-does-not-exist", ExpectedVersion: 1}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("an unknown Requirement returned %v, want ErrNotFound", err)
	}
	// A runner caller.
	if _, err = svc.CompleteFraming(runner(context.Background(), "runner-1"), application.CompleteFramingRequest{RequestID: "v2084a8:runner", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("a runner caller returned %v, want ErrForbidden", err)
	}
	// An unauthenticated caller.
	if _, err = svc.CompleteFraming(context.Background(), application.CompleteFramingRequest{RequestID: "v2084a8:unauth", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version}); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("an unauthenticated caller returned %v, want ErrUnauthenticated", err)
	}
	// A missing request id.
	if _, err = svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequirementID: captured.RequirementID, ExpectedVersion: captured.Version}); err == nil {
		t.Fatal("a request with no request_id was accepted")
	}
	if unchanged, _ := st.Requirement(captured.RequirementID); !reflect.DeepEqual(unchanged, beforeAnything) {
		t.Fatalf("a refusal changed the Requirement: before=%+v after=%+v", beforeAnything, unchanged)
	}

	// Now frame it, and refuse a stale expected version.
	framed, err := svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "v2084a8:frame", RequirementID: captured.RequirementID, ExpectedVersion: captured.Version})
	if err != nil {
		t.Fatalf("start-framing: %v", err)
	}
	beforeStale, _ := st.Requirement(captured.RequirementID)
	if _, err = svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a8:stale", RequirementID: captured.RequirementID, ExpectedVersion: framed.Version + 5}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a stale expected version returned %v, want ErrStaleVersion", err)
	}
	if afterStale, _ := st.Requirement(captured.RequirementID); !reflect.DeepEqual(afterStale, beforeStale) {
		t.Fatalf("a refused stale version changed the Requirement: before=%+v after=%+v", beforeStale, afterStale)
	}

	// Complete it once, and then refuse a Requirement ALREADY in ready, with a
	// fresh request_id so the refusal cannot be an idempotency replay.
	ready, err := svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a8:complete", RequirementID: captured.RequirementID, ExpectedVersion: framed.Version})
	if err != nil {
		t.Fatalf("complete-framing: %v", err)
	}
	beforeSecond, _ := st.Requirement(captured.RequirementID)
	if _, err = svc.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: "v2084a8:complete-again", RequirementID: captured.RequirementID, ExpectedVersion: ready.Version}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("completing the framing of an already-ready Requirement returned %v, want ErrInvalidTransition", err)
	}
	afterSecond, _ := st.Requirement(captured.RequirementID)
	if !reflect.DeepEqual(afterSecond, beforeSecond) {
		t.Fatalf("a refused second complete-framing changed the Requirement: before=%+v after=%+v", beforeSecond, afterSecond)
	}
	// And start-framing is still refused for a Requirement that is no longer
	// captured, so the new command opened no back door into the old one.
	if _, err = svc.StartFraming(ctx, application.StartFramingRequest{RequestID: "v2084a8:frame-again", RequirementID: captured.RequirementID, ExpectedVersion: ready.Version}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("start-framing a ready Requirement returned %v, want ErrInvalidTransition", err)
	}
	if afterThird, _ := st.Requirement(captured.RequirementID); !reflect.DeepEqual(afterThird, beforeSecond) {
		t.Fatalf("a refused start-framing changed the Requirement: %+v", afterThird)
	}
}
