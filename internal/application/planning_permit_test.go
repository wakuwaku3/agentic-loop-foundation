package application_test

// ===========================================================================
// V2-085: Service.Plan and Service.Prepare are permit-gated.
// ===========================================================================
//
// Measured before this task, at base commit
// b0b9ffef8c628f8091e5dbfdbedf88736144a62c, and reproduced by running the
// probe V2-082 left behind: under an effective control mode of emergency-stop
// Service.Plan returned a nil error and MOVED the parent Requirement's
// canonical state, and Service.Prepare returned a nil error and moved the
// Increment to domain.IncrementReady -- the only status a Claim accepts. The
// two commands referenced neither domain.Permit, nor domain.Controls, nor
// domain.EffectiveControl, nor domain.EffectFromPermit. A stop that still
// permits canonical state to move is not a stop.
//
// The repair is exactly two domain.Permit evaluations of kind
// domain.PermitClaim, one in each command, against a domain.ControlTarget and
// a control revision resolved INSIDE the same transaction from the
// Requirement's own Repository link and domain.EffectiveControl. Nothing comes
// from the request, nothing is passed to domain.EffectFromPermit and no
// OutboxItem is staged.
//
// This file is where the resulting seven-mode table is ASSERTED, per command.
// It is deliberately not in stop_matrix_test.go: docs/product/definition.md
// states 7 modes x 8 kinds = 56 cells with 12 allowed and scopes those figures
// by naming TestStopModeByKindMatrix as their verifier, so widening
// stopMatrixKinds would silently falsify a product document. V2-082 set the
// same precedent one task earlier by putting start-framing's table in
// framing_test.go rather than in the matrix.
//
// Determinism: every test below injects the package clock, starts no goroutine,
// takes no sleep, arms no timer and waits on nothing at all. There is no
// duration in any assertion.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// ---------------------------------------------------------------------------
// A5: the two seven-mode tables, written out rather than derived.
// ---------------------------------------------------------------------------

// wantPlanAllowed is A5's table for Service.Plan, stated explicitly so a
// change in domain.Permit's gate fails this test instead of silently
// redefining the design. It is cross-checked below against the package's
// existing permitAllowedTable mirror for domain.PermitClaim, and both must
// agree.
var wantPlanAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   true,
	domain.ControlPauseClaim:    false,
	domain.ControlGracefulStop:  false,
	domain.ControlImmediateStop: false,
	domain.ControlEmergencyStop: false,
	domain.ControlCancel:        false,
}

// wantPrepareAllowed is the same table for Service.Prepare. The two commands
// share one kind and therefore one table: Plan creates the Increment and
// Prepare puts it in the only status Claim accepts, so both are what makes
// new work claimable.
var wantPrepareAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   true,
	domain.ControlPauseClaim:    false,
	domain.ControlGracefulStop:  false,
	domain.ControlImmediateStop: false,
	domain.ControlEmergencyStop: false,
	domain.ControlCancel:        false,
}

// wantFreshIntakeAllowed is the SAME seven modes for a fresh Service.Capture,
// which is gated on domain.PermitIntake. This third table is the whole
// argument for the kind chosen: under pause-intake the tables must DISAGREE --
// planning and preparing an already-captured Requirement are finishing what
// you already have, while a fresh Capture is taking new work in.
var wantFreshIntakeAllowed = map[domain.ControlMode]bool{
	domain.ControlAllow:         true,
	domain.ControlPauseIntake:   false,
	domain.ControlPauseClaim:    true,
	domain.ControlGracefulStop:  false,
	domain.ControlImmediateStop: false,
	domain.ControlEmergencyStop: false,
	domain.ControlCancel:        false,
}

// wantPlanningAllowedTotal is A5's count: exactly two of the seven modes allow
// each command.
const wantPlanningAllowedTotal = 2

// TestPlanAndPrepareSevenControlModeTable is A5, A6 and A7.
//
// One fresh Service and store per mode. The Requirement the Plan cell targets
// and the proposed Increment the Prepare cell targets are both established
// while the mode is still allow -- Capture is intake-gated and would itself be
// denied under most of these modes, which would measure the wrong thing -- and
// the Control Intent is engaged only afterwards, exactly as
// stop_matrix_test.go establishes its own fixture.
//
// Every DENIED cell is asserted three ways, because two of them are what the
// design forbids separately: the refusal is attributable to
// domain.ErrControlDenied, the aggregate is byte-unchanged by
// reflect.DeepEqual (Version and, for Plan, the Increments slice included),
// and the store staged nothing -- no event, no outbox row and no idempotency
// record, the last proven by replaying the SAME request_id after the stop is
// lifted and asserting it executes for real rather than replaying a stored
// response.
func TestPlanAndPrepareSevenControlModeTable(t *testing.T) {
	if len(stopMatrixModes) != 7 {
		t.Fatalf("the package's control-mode list has %d entries; A5's tables are seven modes", len(stopMatrixModes))
	}
	planAllowedCells, prepareAllowedCells := 0, 0
	measured := map[domain.ControlMode]string{}
	for _, mode := range stopMatrixModes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			svc, st := service()
			ctx := owner(context.Background())
			tag := "v2085:" + string(mode)

			// The fixture, built entirely while the mode is still allow.
			capPlan, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":cap-plan", Text: "captured while the mode is still allow"})
			if err != nil {
				t.Fatalf("fixture capture for the plan cell: %v", err)
			}
			capPrepare, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":cap-prepare", Text: "captured while the mode is still allow"})
			if err != nil {
				t.Fatalf("fixture capture for the prepare cell: %v", err)
			}
			planned, err := svc.Plan(ctx, application.PlanRequest{RequestID: tag + ":plan-fixture", RequirementID: capPrepare.RequirementID, ExpectedRequirementVersion: capPrepare.Version})
			if err != nil {
				t.Fatalf("fixture plan: %v", err)
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

			requirementBefore, ok := st.Requirement(capPlan.RequirementID)
			if !ok {
				t.Fatal("the captured Requirement for the plan cell is not in the store")
			}
			if requirementBefore.Status != domain.RequirementCaptured {
				t.Fatalf("the fixture Requirement is %q, not captured", requirementBefore.Status)
			}
			incrementBefore, ok := st.Increment(planned.IncrementID)
			if !ok {
				t.Fatal("the planned Increment for the prepare cell is not in the store")
			}
			if incrementBefore.Status != domain.IncrementProposed {
				t.Fatalf("the fixture Increment is %q, not proposed", incrementBefore.Status)
			}
			eventsBefore := len(st.Events())
			// The Control Intent itself stages an outbox item, so every outbox
			// assertion below is a delta across the attempt rather than an
			// absolute zero.
			outboxBefore := len(st.Outbox())

			// ---- the Plan cell ----
			planRequestID := tag + ":plan"
			_, planErr := svc.Plan(ctx, application.PlanRequest{RequestID: planRequestID, RequirementID: capPlan.RequirementID, ExpectedRequirementVersion: requirementBefore.Version})
			planAllowed := planErr == nil
			eventsAfterPlan := len(st.Events())
			outboxAfterPlan := len(st.Outbox())
			requirementAfter, _ := st.Requirement(capPlan.RequirementID)

			// ---- the Prepare cell ----
			prepareRequestID := tag + ":prepare"
			_, prepareErr := svc.Prepare(ctx, application.PrepareRequest{RequestID: prepareRequestID, IncrementID: planned.IncrementID, ExpectedVersion: incrementBefore.Version})
			prepareAllowed := prepareErr == nil
			eventsAfterPrepare := len(st.Events())
			outboxAfterPrepare := len(st.Outbox())
			incrementAfter, _ := st.Increment(planned.IncrementID)

			// ---- the fresh-intake cell, at the SAME control revision and in
			// the SAME run. This is the comparison the kind was chosen for.
			_, captureErr := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":fresh-capture", Text: "new intake at the same revision"})
			captureAllowed := captureErr == nil

			// ---- the tables ----
			wantPlan := wantPlanAllowed[mode]
			wantPrepare := wantPrepareAllowed[mode]
			wantCapture := wantFreshIntakeAllowed[mode]
			if planAllowed != wantPlan {
				t.Fatalf("Plan under %s: allowed=%v want=%v (err=%v)", mode, planAllowed, wantPlan, planErr)
			}
			if prepareAllowed != wantPrepare {
				t.Fatalf("Prepare under %s: allowed=%v want=%v (err=%v)", mode, prepareAllowed, wantPrepare, prepareErr)
			}
			if captureAllowed != wantCapture {
				t.Fatalf("fresh Capture under %s: allowed=%v want=%v (err=%v)", mode, captureAllowed, wantCapture, captureErr)
			}
			// The explicit tables and the package's existing mirror of
			// domain.Permit's own per-kind gate must agree, for both kinds.
			if wantPlan != permitAllowedTable(mode, domain.PermitClaim) {
				t.Fatalf("A5's Plan table disagrees with permitAllowedTable for %s", mode)
			}
			if wantPrepare != permitAllowedTable(mode, domain.PermitClaim) {
				t.Fatalf("A5's Prepare table disagrees with permitAllowedTable for %s", mode)
			}
			if wantCapture != permitAllowedTable(mode, domain.PermitIntake) {
				t.Fatalf("A5's fresh-intake table disagrees with permitAllowedTable for %s", mode)
			}

			// Neither command stages an outbox item, allowed or denied:
			// PermitKind.SideEffect() is false for claim and nothing outside
			// the control plane is asked to act.
			if outboxAfterPrepare != outboxBefore {
				t.Fatalf("under %s the two cells changed the outbox from %d to %d items; neither command stages one", mode, outboxBefore, outboxAfterPrepare)
			}
			if outboxAfterPlan != outboxBefore {
				t.Fatalf("under %s the Plan cell changed the outbox from %d to %d items", mode, outboxBefore, outboxAfterPlan)
			}

			// ---- the Plan cell's outcome ----
			if planAllowed {
				planAllowedCells++
				if requirementAfter.Version != requirementBefore.Version+1 {
					t.Fatalf("an ALLOWED Plan cell under %s left the Requirement at version %d, want %d", mode, requirementAfter.Version, requirementBefore.Version+1)
				}
				if len(requirementAfter.Increments) != len(requirementBefore.Increments)+1 {
					t.Fatalf("an ALLOWED Plan cell under %s left %d Increment ids on the Requirement, want %d", mode, len(requirementAfter.Increments), len(requirementBefore.Increments)+1)
				}
				if eventsAfterPlan != eventsBefore+1 {
					t.Fatalf("an ALLOWED Plan cell under %s recorded %d events, want exactly one", mode, eventsAfterPlan-eventsBefore)
				}
			} else {
				if !errors.Is(planErr, domain.ErrControlDenied) {
					t.Fatalf("a DENIED Plan cell under %s was refused by %v, not ErrControlDenied", mode, planErr)
				}
				if !reflect.DeepEqual(requirementAfter, requirementBefore) {
					t.Fatalf("a DENIED Plan cell under %s changed the Requirement: before=%+v after=%+v", mode, requirementBefore, requirementAfter)
				}
				if eventsAfterPlan != eventsBefore {
					t.Fatalf("a DENIED Plan cell under %s changed the event count from %d to %d", mode, eventsBefore, eventsAfterPlan)
				}
				if _, found := st.Idempotency(planRequestID); found {
					t.Fatalf("a DENIED Plan cell under %s wrote an idempotency record for %q", mode, planRequestID)
				}
			}

			// ---- the Prepare cell's outcome ----
			if prepareAllowed {
				prepareAllowedCells++
				if incrementAfter.Status != domain.IncrementReady {
					t.Fatalf("an ALLOWED Prepare cell under %s left the Increment at %q, want ready", mode, incrementAfter.Status)
				}
				if incrementAfter.Version != incrementBefore.Version+1 {
					t.Fatalf("an ALLOWED Prepare cell under %s left the Increment at version %d, want %d", mode, incrementAfter.Version, incrementBefore.Version+1)
				}
				if eventsAfterPrepare != eventsAfterPlan+1 {
					t.Fatalf("an ALLOWED Prepare cell under %s recorded %d events, want exactly one", mode, eventsAfterPrepare-eventsAfterPlan)
				}
			} else {
				if !errors.Is(prepareErr, domain.ErrControlDenied) {
					t.Fatalf("a DENIED Prepare cell under %s was refused by %v, not ErrControlDenied", mode, prepareErr)
				}
				if !reflect.DeepEqual(incrementAfter, incrementBefore) {
					t.Fatalf("a DENIED Prepare cell under %s changed the Increment: before=%+v after=%+v", mode, incrementBefore, incrementAfter)
				}
				if incrementAfter.Status != domain.IncrementProposed {
					t.Fatalf("a DENIED Prepare cell under %s left the Increment at %q, want proposed", mode, incrementAfter.Status)
				}
				if eventsAfterPrepare != eventsAfterPlan {
					t.Fatalf("a DENIED Prepare cell under %s changed the event count from %d to %d", mode, eventsAfterPlan, eventsAfterPrepare)
				}
				if _, found := st.Idempotency(prepareRequestID); found {
					t.Fatalf("a DENIED Prepare cell under %s wrote an idempotency record for %q", mode, prepareRequestID)
				}
			}

			// ---- A7: the boundary that is the whole argument for the kind,
			// asserted where it happens rather than in prose.
			if mode == domain.ControlPauseIntake {
				if !planAllowed || !prepareAllowed || captureAllowed {
					t.Fatalf("pause-intake must ALLOW Plan and Prepare on work already captured and DENY a fresh Capture at revision %d; plan=%v prepare=%v capture=%v", revision, planAllowed, prepareAllowed, captureAllowed)
				}
				t.Logf("A7 boundary: under pause-intake at revision %d, Plan is ALLOWED (err=%v) and Prepare is ALLOWED (err=%v) while a fresh Capture at the same revision is DENIED (%v)", revision, planErr, prepareErr, captureErr)
			}
			if mode == domain.ControlEmergencyStop {
				if planAllowed || prepareAllowed || captureAllowed {
					t.Fatalf("emergency-stop must DENY all three; plan=%v prepare=%v capture=%v", planAllowed, prepareAllowed, captureAllowed)
				}
				t.Logf("A7 boundary: under emergency-stop at revision %d, Plan is DENIED (%v), Prepare is DENIED (%v) and a fresh Capture is DENIED (%v)", revision, planErr, prepareErr, captureErr)
			}

			// ---- A6(c): the refusal wrote no idempotency record, proven by
			// replaying the SAME request_id once the stop is lifted. A replay
			// of a stored response would leave the store where it is; this
			// must execute for real.
			if !planAllowed || !prepareAllowed {
				if _, e := svc.Control(ctx, application.ControlRequest{
					RequestID: tag + ":control-resume",
					Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
					Mode:      domain.ControlAllow,
					At:        (clock{}).Now(),
				}); e != nil {
					t.Fatalf("lifting the stop: %v", e)
				}
			}
			if !planAllowed {
				replayed, e := svc.Plan(ctx, application.PlanRequest{RequestID: planRequestID, RequirementID: capPlan.RequirementID, ExpectedRequirementVersion: requirementBefore.Version})
				if e != nil {
					t.Fatalf("the same request_id after the stop is lifted must execute for real, got %v", e)
				}
				resumed, _ := st.Requirement(capPlan.RequirementID)
				if resumed.Version != requirementBefore.Version+1 || len(resumed.Increments) != len(requirementBefore.Increments)+1 {
					t.Fatalf("the replayed Plan returned %+v but the Requirement is %+v; the refusal must have stored no response", replayed, resumed)
				}
				if _, found := st.Idempotency(planRequestID); !found {
					t.Fatal("the real Plan wrote no idempotency record")
				}
			}
			if !prepareAllowed {
				replayed, e := svc.Prepare(ctx, application.PrepareRequest{RequestID: prepareRequestID, IncrementID: planned.IncrementID, ExpectedVersion: incrementBefore.Version})
				if e != nil {
					t.Fatalf("the same request_id after the stop is lifted must execute for real, got %v", e)
				}
				resumed, _ := st.Increment(planned.IncrementID)
				if resumed.Status != domain.IncrementReady || resumed.Version != incrementBefore.Version+1 {
					t.Fatalf("the replayed Prepare returned %+v but the Increment is %+v; the refusal must have stored no response", replayed, resumed)
				}
				if _, found := st.Idempotency(prepareRequestID); !found {
					t.Fatal("the real Prepare wrote no idempotency record")
				}
			}

			measured[mode] = string(mode) + ": plan=" + boolWord(planAllowed) + " prepare=" + boolWord(prepareAllowed) + " intake=" + boolWord(captureAllowed)
			t.Logf("A5 cell %s: Plan allowed=%v (err=%v); Prepare allowed=%v (err=%v); fresh Capture allowed=%v (err=%v)", mode, planAllowed, planErr, prepareAllowed, prepareErr, captureAllowed, captureErr)
		})
	}
	if planAllowedCells != wantPlanningAllowedTotal {
		t.Fatalf("%d of the seven modes allowed Plan, want %d", planAllowedCells, wantPlanningAllowedTotal)
	}
	if prepareAllowedCells != wantPlanningAllowedTotal {
		t.Fatalf("%d of the seven modes allowed Prepare, want %d", prepareAllowedCells, wantPlanningAllowedTotal)
	}
	if len(measured) != 7 {
		t.Fatalf("measured %d of seven modes", len(measured))
	}
	t.Logf("A5 measured table: %v", measured)
}

// TestPlanAndPrepareResolveTheTargetFromTheRequirementsOwnRepositoryLink is
// A3's and A4's load-bearing claim, asserted rather than described: the
// ControlTarget the gate is evaluated against is built from the Requirement's
// OWN Repository link, read inside the same transaction, and never from the
// request. A repository-scoped stop naming the linked Repository therefore
// reaches both commands, and one naming a DIFFERENT Repository does not.
//
// This is also what makes the absence of a request field checkable: neither
// PlanRequest nor PrepareRequest carries a repository id or a control
// revision, so the only way either stop can be attributed is the link.
func TestPlanAndPrepareResolveTheTargetFromTheRequirementsOwnRepositoryLink(t *testing.T) {
	linked := func(t *testing.T, stopped bool) (planErr, prepareErr error) {
		t.Helper()
		svc, st := service()
		ctx := owner(context.Background())
		mine := registerRepository(t, svc, ctx, "v2085:reg-mine", "https://github.com/O/Mine")
		other := registerRepository(t, svc, ctx, "v2085:reg-other", "https://github.com/O/Other")

		capPlan, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2085:link-cap-plan", Text: "linked", RepositoryID: mine.RepositoryID})
		if err != nil {
			t.Fatalf("linked capture: %v", err)
		}
		capPrepare, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2085:link-cap-prepare", Text: "linked", RepositoryID: mine.RepositoryID})
		if err != nil {
			t.Fatalf("linked capture: %v", err)
		}
		planned, err := svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:link-plan-fixture", RequirementID: capPrepare.RequirementID, ExpectedRequirementVersion: capPrepare.Version})
		if err != nil {
			t.Fatalf("fixture plan: %v", err)
		}
		// The canonical target Plan writes carries the linked Repository, so
		// the target the gate reads and the target the store keeps are the
		// same value.
		var canonical domain.ControlTarget
		if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
			var x error
			canonical, _, x = u.CanonicalTarget(context.Background(), planned.IncrementID, "")
			return x
		}); e != nil {
			t.Fatalf("reading the canonical target: %v", e)
		}
		if canonical.RepositoryID != mine.RepositoryID {
			t.Fatalf("the canonical target's repository = %q, want the Requirement's own link %q", canonical.RepositoryID, mine.RepositoryID)
		}

		stopTarget := other.RepositoryID
		if stopped {
			stopTarget = mine.RepositoryID
		}
		if _, e := svc.Control(ctx, application.ControlRequest{
			RequestID: "v2085:link-control",
			Scope:     domain.ControlScope{Kind: domain.ScopeRepository, Value: stopTarget},
			Mode:      domain.ControlEmergencyStop,
			At:        (clock{}).Now(),
		}); e != nil {
			t.Fatalf("repository-scoped control: %v", e)
		}
		requirement, _ := st.Requirement(capPlan.RequirementID)
		increment, _ := st.Increment(planned.IncrementID)
		_, planErr = svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:link-plan", RequirementID: capPlan.RequirementID, ExpectedRequirementVersion: requirement.Version})
		_, prepareErr = svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:link-prepare", IncrementID: planned.IncrementID, ExpectedVersion: increment.Version})
		return planErr, prepareErr
	}

	planErr, prepareErr := linked(t, true)
	if !errors.Is(planErr, domain.ErrControlDenied) {
		t.Fatalf("a repository-scoped emergency-stop on the LINKED Repository must deny Plan, got %v", planErr)
	}
	if !errors.Is(prepareErr, domain.ErrControlDenied) {
		t.Fatalf("a repository-scoped emergency-stop on the LINKED Repository must deny Prepare, got %v", prepareErr)
	}
	planErr, prepareErr = linked(t, false)
	if planErr != nil {
		t.Fatalf("a repository-scoped stop on a DIFFERENT Repository must not reach Plan, got %v", planErr)
	}
	if prepareErr != nil {
		t.Fatalf("a repository-scoped stop on a DIFFERENT Repository must not reach Prepare, got %v", prepareErr)
	}
	t.Log("A3/A4: the gate's ControlTarget comes from the Requirement's own Repository link -- a repository-scoped stop on the linked Repository denies both commands, and the same stop on another Repository denies neither")
}

// TestPlanAndPrepareSuccessPathIsUnchangedUnderAllow is A11. Under allow the
// two commands return exactly the responses they returned before this task,
// write exactly the same documents, record exactly the same event types and
// produce exactly the same versions -- and stage no outbox item and exactly
// one idempotency record each. An unlinked Requirement is the pre-existing
// default fixture shape, and it stays permitted: an absent Control Intent
// yields domain.EffectiveControl{Mode: ControlAllow, Found: false} and
// revision 0, which domain.Permit allows.
func TestPlanAndPrepareSuccessPathIsUnchangedUnderAllow(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2085:ok-capture", Text: "x"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if captured.Version != 1 {
		t.Fatalf("capture left the Requirement at version %d, want 1", captured.Version)
	}
	eventsAfterCapture := len(st.Events())
	outboxAfterCapture := len(st.Outbox())

	plan, err := svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:ok-plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.RequirementID != captured.RequirementID || plan.IncrementID == "" || plan.Version != 1 {
		t.Fatalf("PlanResponse = %+v; want the same requirement id, a fresh increment id and version 1", plan)
	}
	requirement, ok := st.Requirement(captured.RequirementID)
	if !ok {
		t.Fatal("the Requirement is gone")
	}
	if requirement.Version != 2 || len(requirement.Increments) != 1 || requirement.Increments[0].String() != plan.IncrementID {
		t.Fatalf("Plan left the Requirement at %+v; want version 2 with exactly the one Increment id", requirement)
	}
	increment, ok := st.Increment(plan.IncrementID)
	if !ok {
		t.Fatal("Plan created no Increment")
	}
	if increment.Status != domain.IncrementProposed || increment.Version != 1 || increment.RequirementID.String() != captured.RequirementID {
		t.Fatalf("Plan left the Increment at %+v; want proposed/v1 under its own parent", increment)
	}
	planEvents := st.Events()
	if len(planEvents) != eventsAfterCapture+1 {
		t.Fatalf("Plan recorded %d events, want exactly one", len(planEvents)-eventsAfterCapture)
	}
	if got := planEvents[len(planEvents)-1]; got.Type != "increment.proposed" || got.AggregateType != "increment" || got.AggregateID != plan.IncrementID || got.Version != 1 {
		t.Fatalf("Plan's event = %+v; want increment.proposed on the increment aggregate at version 1", got)
	}
	if len(st.Outbox()) != outboxAfterCapture {
		t.Fatalf("Plan staged %d outbox items, want none", len(st.Outbox())-outboxAfterCapture)
	}
	// The canonical target is written from the Requirement's own link, which
	// is absent here, so the RepositoryID is empty -- not an error.
	var canonical domain.ControlTarget
	var canonicalFound bool
	if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		var x error
		canonical, canonicalFound, x = u.CanonicalTarget(context.Background(), plan.IncrementID, "")
		return x
	}); e != nil {
		t.Fatalf("reading the canonical target: %v", e)
	}
	if !canonicalFound {
		t.Fatal("Plan wrote no canonical target")
	}
	want := domain.ControlTarget{InstallationID: "install", RepositoryID: "", RequirementID: increment.RequirementID, IncrementID: increment.ID}
	if canonical != want {
		t.Fatalf("canonical target = %+v, want %+v", canonical, want)
	}

	prepared, err := svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:ok-prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.IncrementID != plan.IncrementID || prepared.Version != 2 {
		t.Fatalf("PrepareResponse = %+v; want the same increment id at version 2", prepared)
	}
	ready, _ := st.Increment(plan.IncrementID)
	if ready.Status != domain.IncrementReady || ready.Version != 2 {
		t.Fatalf("Prepare left the Increment at %+v; want ready/v2", ready)
	}
	prepareEvents := st.Events()
	if len(prepareEvents) != len(planEvents)+1 {
		t.Fatalf("Prepare recorded %d events, want exactly one", len(prepareEvents)-len(planEvents))
	}
	if got := prepareEvents[len(prepareEvents)-1]; got.Type != "increment.ready" || got.AggregateType != "increment" || got.AggregateID != plan.IncrementID || got.Version != 2 {
		t.Fatalf("Prepare's event = %+v; want increment.ready on the increment aggregate at version 2", got)
	}
	if len(st.Outbox()) != outboxAfterCapture {
		t.Fatalf("Prepare staged %d outbox items, want none", len(st.Outbox())-outboxAfterCapture)
	}

	// The idempotency guarantee is unchanged, and it stays AHEAD of the gate:
	// a replay reproduces the committed response byte-for-byte.
	replayPlan, err := svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:ok-plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("plan replay: %v", err)
	}
	if !reflect.DeepEqual(replayPlan, plan) {
		t.Fatalf("plan replay = %+v, want %+v", replayPlan, plan)
	}
	replayPrepare, err := svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:ok-prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatalf("prepare replay: %v", err)
	}
	if !reflect.DeepEqual(replayPrepare, prepared) {
		t.Fatalf("prepare replay = %+v, want %+v", replayPrepare, prepared)
	}
	if len(st.Events()) != len(prepareEvents) {
		t.Fatalf("the two replays recorded %d further events, want none", len(st.Events())-len(prepareEvents))
	}
	if got := len(requirementSnapshotIncrements(t, st, captured.RequirementID)); got != 1 {
		t.Fatalf("the two replays left %d Increment ids on the Requirement, want 1", got)
	}
}

func requirementSnapshotIncrements(t *testing.T, st *memory.Store, id string) []domain.IncrementID {
	t.Helper()
	r, ok := st.Requirement(id)
	if !ok {
		t.Fatalf("Requirement %q is gone", id)
	}
	return r.Increments
}

// TestTheIdempotencyReplayStaysAheadOfThePlanningGate is the other half of the
// ordering the design fixes: a request that already committed must still
// replay its committed response after a stop is engaged, because otherwise the
// idempotency guarantee would depend on the control mode and a completed
// operation would be turned into a refusal.
func TestTheIdempotencyReplayStaysAheadOfThePlanningGate(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2085:order-capture", Text: "x"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	plan, err := svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:order-plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	prepared, err := svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:order-prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err = svc.Control(ctx, application.ControlRequest{
		RequestID: "v2085:order-control",
		Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
		Mode:      domain.ControlEmergencyStop,
		At:        (clock{}).Now(),
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	events := len(st.Events())

	replayPlan, err := svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:order-plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("a committed Plan must still replay under emergency-stop, got %v", err)
	}
	if !reflect.DeepEqual(replayPlan, plan) {
		t.Fatalf("the replayed Plan = %+v, want %+v", replayPlan, plan)
	}
	replayPrepare, err := svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:order-prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatalf("a committed Prepare must still replay under emergency-stop, got %v", err)
	}
	if !reflect.DeepEqual(replayPrepare, prepared) {
		t.Fatalf("the replayed Prepare = %+v, want %+v", replayPrepare, prepared)
	}
	if got := len(st.Events()); got != events {
		t.Fatalf("the two replays recorded %d further events under emergency-stop, want none", got-events)
	}
	// A fingerprint conflict on the same request_id is still refused as a
	// conflict rather than as a control denial: the replay branch runs first.
	if _, err = svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:order-plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: 99}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("a fingerprint conflict under emergency-stop returned %v, want ErrIdempotencyConflict", err)
	}
	t.Log("A3/A5 ordering: the idempotency replay and its fingerprint-conflict refusal both stay ahead of the new gate")
}

// TestPlanAndPrepareRefusalsStayAttributable is the other half of the same
// ordering: the gate sits AFTER the aggregate load and the expected-version
// check, so an unknown id is still ErrNotFound and a stale version is still
// domain.ErrStaleVersion even while a stop is engaged. The seven-mode table
// therefore measures the mode and nothing else.
func TestPlanAndPrepareRefusalsStayAttributable(t *testing.T) {
	svc, st := service()
	ctx := owner(context.Background())

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2085:attr-capture", Text: "x"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	plan, err := svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:attr-plan-fixture", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatalf("fixture plan: %v", err)
	}
	if _, err = svc.Control(ctx, application.ControlRequest{
		RequestID: "v2085:attr-control",
		Scope:     domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"},
		Mode:      domain.ControlEmergencyStop,
		At:        (clock{}).Now(),
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	events := len(st.Events())

	if _, err = svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:attr-unknown", RequirementID: "requirement-does-not-exist", ExpectedRequirementVersion: 1}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("an unknown Requirement under emergency-stop returned %v, want ErrNotFound", err)
	}
	if _, err = svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:attr-stale", RequirementID: captured.RequirementID, ExpectedRequirementVersion: 99}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a stale Requirement version under emergency-stop returned %v, want ErrStaleVersion", err)
	}
	if _, err = svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:attr-unknown-inc", IncrementID: "increment-does-not-exist", ExpectedVersion: 1}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("an unknown Increment under emergency-stop returned %v, want ErrNotFound", err)
	}
	if _, err = svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:attr-stale-inc", IncrementID: plan.IncrementID, ExpectedVersion: 99}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a stale Increment version under emergency-stop returned %v, want ErrStaleVersion", err)
	}
	if got := len(st.Events()); got != events {
		t.Fatalf("the four attributable refusals recorded %d events, want none", got-events)
	}
}

// ---------------------------------------------------------------------------
// A14: the quota reservation.
// ---------------------------------------------------------------------------

// planningUnit counts the reads and writes Plan and Prepare actually perform,
// following the countingUnit idiom human_input_test.go already keeps in this
// package.
type planningUnit struct {
	application.UnitOfWork
	authority context.Context
	reads     *int
	writes    *int
}

func (u planningUnit) AuthorityContext() context.Context { return u.authority }

func (u planningUnit) Idempotency(ctx context.Context, requestID, operation string) (application.IdempotentResponse, bool, error) {
	*u.reads++
	return u.UnitOfWork.Idempotency(ctx, requestID, operation)
}
func (u planningUnit) Requirement(ctx context.Context, id string) (domain.Requirement, bool, error) {
	*u.reads++
	return u.UnitOfWork.Requirement(ctx, id)
}
func (u planningUnit) Increment(ctx context.Context, id string) (domain.Increment, bool, error) {
	*u.reads++
	return u.UnitOfWork.Increment(ctx, id)
}
func (u planningUnit) Controls(ctx context.Context) ([]domain.ControlIntent, error) {
	*u.reads++
	return u.UnitOfWork.Controls(ctx)
}
func (u planningUnit) RequirementRepositoryLink(ctx context.Context, id string) (domain.RequirementRepositoryLink, bool, error) {
	*u.reads++
	return u.UnitOfWork.RequirementRepositoryLink(ctx, id)
}
func (u planningUnit) SaveRequirement(ctx context.Context, value domain.Requirement, expected domain.Version) error {
	*u.writes++
	return u.UnitOfWork.SaveRequirement(ctx, value, expected)
}
func (u planningUnit) SaveIncrement(ctx context.Context, value domain.Increment, expected domain.Version) error {
	*u.writes++
	return u.UnitOfWork.SaveIncrement(ctx, value, expected)
}
func (u planningUnit) SaveCanonicalTarget(ctx context.Context, incrementID string, target domain.ControlTarget) error {
	*u.writes++
	return u.UnitOfWork.SaveCanonicalTarget(ctx, incrementID, target)
}
func (u planningUnit) Record(event application.Event, outbox *application.OutboxItem) error {
	// One event document, plus the idempotency record and the quota document
	// the adapter writes for every mutation. Counted as three writes so the
	// comparison against the reservation is conservative in the same
	// direction the reservation itself is.
	*u.writes += 3
	return u.UnitOfWork.Record(event, outbox)
}

type planningTransactor struct {
	inner  application.Transactor
	reads  int
	writes int
}

func (c *planningTransactor) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	return c.inner.Transact(ctx, func(u application.UnitOfWork) error {
		return fn(planningUnit{UnitOfWork: u, authority: ctx, reads: &c.reads, writes: &c.writes})
	})
}

// TestPlanAndPrepareStayInsideTheMutationReservation is A14. Each command
// gains one keyed read of the Requirement-to-Repository link (Prepare) or
// re-orders one it already performed (Plan), plus one read of the Control
// Intent collection. Both must stay inside quota.MutationUsage, which
// s.mutate reserves once per transaction before the callback can stage
// anything.
func TestPlanAndPrepareStayInsideTheMutationReservation(t *testing.T) {
	st := memory.New()
	counter := &planningTransactor{inner: st}
	svc, err := application.NewServiceWithConfig(counter, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "v2085:quota-capture", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}

	measure := func(name string, run func() error) {
		t.Helper()
		counter.reads, counter.writes = 0, 0
		if e := run(); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		t.Logf("A14 %s: reads=%d writes=%d; reservation reads=%d writes=%d", name, counter.reads, counter.writes, quota.MutationUsage.Reads, quota.MutationUsage.Writes)
		if int64(counter.reads) > quota.MutationUsage.Reads || int64(counter.writes) > quota.MutationUsage.Writes {
			t.Fatalf("%s performed reads=%d writes=%d, outside the conservative mutation reservation %+v", name, counter.reads, counter.writes, quota.MutationUsage)
		}
	}

	var plan application.PlanResponse
	measure("the plan", func() error {
		var e error
		plan, e = svc.Plan(ctx, application.PlanRequest{RequestID: "v2085:quota-plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
		return e
	})
	measure("the prepare", func() error {
		_, e := svc.Prepare(ctx, application.PrepareRequest{RequestID: "v2085:quota-prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
		return e
	})
}
