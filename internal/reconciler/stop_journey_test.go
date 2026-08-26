package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// stopJourneyClock and stopJourneyIDs are small fixtures private to this
// file's test functions; a fresh instance is created inside each test, so
// neither is package-level mutable state.
type stopJourneyClock struct{ now time.Time }

func (c *stopJourneyClock) Now() time.Time { return c.now }

type stopJourneyIDs struct{ n int }

func (g *stopJourneyIDs) Next(kind string) (string, error) {
	g.n++
	return kind + "-" + string(rune('a'+g.n)), nil
}

// runJourneyFiveHappyPath drives failure-model.md section 5's whole
// protocol for a connected Runner, for one stop mode, through the real
// application.Service, a real runner.LeaseKeeper/ControlAgent/ControlLoop,
// a real OutboxDispatcher, Reconciler and VerificationReconciler. It never
// asserts ControlVerified from the Control Intent alone: every state
// transition it checks is read back from the store after the operation
// that is supposed to have caused it.
func runJourneyFiveHappyPath(t *testing.T, mode domain.ControlMode, expectProcessState string, wireControlLoop func(loop *runner.ControlLoop, checkpointCall func() error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	clock := &stopJourneyClock{now: time.Unix(1_700_300_000, 0).UTC()}
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock, &stopJourneyIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "owner"})
	runnerCtx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	cap, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: "j5:capture"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: this journey claims, so its parent Requirement is moved to
	// domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- before the Plan, and the Plan
	// carries the POST-seed version.
	readyVersion := reconcilerSeedRequirementStatus(t, st, ownerCtx, cap.RequirementID, domain.RequirementReady)
	plan, err := svc.Plan(ownerCtx, application.PlanRequest{RequestID: "j5:plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := svc.Prepare(ownerCtx, application.PrepareRequest{RequestID: "j5:prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatal(err)
	}
	// A second, independent Increment used only to prove step 2 (new claim
	// stops): never claimed by the happy-path Execution below.
	capB, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: "j5:captureB"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: the SECOND Requirement is moved to domain.RequirementReady too,
	// and for a sharper reason than the first. Step 2 below asserts that a new
	// claim is DENIED once the stop is in force. V2-089's guard is ordered
	// BEFORE domain.Permit, so leaving this parent in `captured` would still
	// make step 2's `err == nil` check pass -- with ErrRequirementNotClaimable
	// instead of domain.ErrControlDenied -- and the step would measure the
	// wrong refusal. Seeding it `ready` keeps step 2 measuring the stop.
	readyVersionB := reconcilerSeedRequirementStatus(t, st, ownerCtx, capB.RequirementID, domain.RequirementReady)
	planB, err := svc.Plan(ownerCtx, application.PlanRequest{RequestID: "j5:planB", RequirementID: capB.RequirementID, ExpectedRequirementVersion: readyVersionB})
	if err != nil {
		t.Fatal(err)
	}
	preparedB, err := svc.Prepare(ownerCtx, application.PrepareRequest{RequestID: "j5:prepareB", IncrementID: planB.IncrementID, ExpectedVersion: planB.Version})
	if err != nil {
		t.Fatal(err)
	}

	claim, err := svc.Claim(runnerCtx, application.ClaimRequest{RequestID: "j5:claim", IncrementID: plan.IncrementID, ExpectedIncrementVersion: prepared.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(runnerCtx, application.StartRequest{RequestID: "j5:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	// Settle Claim's own "claim-issued" outbox effect so it does not itself
	// block verification later: this journey's own step 4 (below)
	// separately proves outbox reconciliation using a fresh effect.
	settleAllOutbox(t, st, clock)

	// Step 1: the Control Intent is durably written; ControlProgress starts
	// requested and unverified.
	ctrl, err := svc.Control(ownerCtx, application.ControlRequest{RequestID: "j5:control", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: mode, At: clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	assertControlProgress(t, st, ctrl.Revision, domain.ControlRequested, domain.VerificationPending)

	// Step 2: new claim (and, by the same store-backed Permit gate proven
	// in acceptance A12's TestStopModeByKindMatrix, credential issuance,
	// which this journey does not re-derive) stop.
	if _, err := svc.Claim(runnerCtx, application.ClaimRequest{RequestID: "j5:claimB-denied", IncrementID: planB.IncrementID, ExpectedIncrementVersion: preparedB.Version, ControlRevision: ctrl.Revision}); err == nil {
		t.Fatal("expected a new claim to be denied once stop is in effect")
	} else if errors.Is(err, application.ErrRequirementNotClaimable) {
		// V2-089: the denial must be the STOP, not the parent's status. This
		// arm exists because the new guard is ordered ahead of domain.Permit,
		// so a captured parent would satisfy the `err == nil` check above
		// while measuring nothing about the stop.
		t.Fatalf("the new claim was refused because its parent admits no work, not because the stop is in force: %v", err)
	}

	// Step 3: the Runner acks the revision through Heartbeat, and step 3's
	// other half -- checkpoint or terminate -- happens via ControlLoop.
	journalDir := t.TempDir()
	journal, err := runner.OpenJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	loop := &runner.ControlLoop{Journal: journal}
	wireControlLoop(loop, func() error {
		_, err := svc.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "j5:checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken, ControlRevision: ctrl.Revision})
		return err
	})
	agent := &runner.ControlAgent{Loop: loop}
	keeper := &runner.LeaseKeeper{Service: svc, LeaseID: claim.LeaseID, RequestBase: "j5:keeper", Agent: agent, ExecutionID: claim.ExecutionID}

	out1, err := keeper.Tick(runnerCtx, 1, claim.FencingToken)
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if !out1.Denied {
		t.Fatal("tick 1: expected Renew to be denied once stop is in effect")
	}
	if out1.HeartbeatResult.LatestRevision != ctrl.Revision || out1.HeartbeatResult.LatestMode != mode {
		t.Fatalf("tick 1: heartbeat=%#v, want LatestRevision=%d LatestMode=%s", out1.HeartbeatResult, ctrl.Revision, mode)
	}
	assertControlProgress(t, st, ctrl.Revision, domain.ControlRequested, domain.VerificationPending)

	out2, err := keeper.Tick(runnerCtx, 1, claim.FencingToken)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if out2.HeartbeatResult.AppliedRevision != ctrl.Revision {
		t.Fatalf("tick 2: AppliedRevision=%d, want %d", out2.HeartbeatResult.AppliedRevision, ctrl.Revision)
	}
	// Step 3's ack: the progress advances to ControlAcknowledged only after
	// the Runner's Heartbeat reports the applied revision -- not merely
	// because the Control Intent exists.
	assertControlProgress(t, st, ctrl.Revision, domain.ControlAcknowledged, domain.VerificationPending)

	events, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	var sawExpectedObservation bool
	for _, event := range events {
		if event.Kind == "control_observation" && strings.Contains(string(event.Payload), `"state":"`+expectProcessState+`"`) {
			sawExpectedObservation = true
		}
	}
	if !sawExpectedObservation {
		t.Fatalf("journal has no %s control_observation: events=%#v", expectProcessState, events)
	}

	// Step 4: Result / outbox operations reconcile. Control() itself created
	// exactly one durable outbox record for this stop -- a "control-changed"
	// notification bound to the Installation scope, not to any specific
	// Lease -- and this is the one outbox operation the protocol expects to
	// still settle while a stop is in effect: every other, lease-fence-bound
	// effect (including a fresh AcceptResult, per acceptance A12's own
	// matrix) is denied once any new Control Intent has changed the
	// authoritative revision, by design (a stale effect must never fire
	// under a different policy epoch). Dispatching now proves that
	// reconciliation, using the real OutboxDispatcher, not an assumption.
	settleAllOutbox(t, st, clock)
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		items, e := u.Outboxes(ctx, clock.Now().Add(time.Hour), 100)
		if e != nil {
			return e
		}
		if len(items) != 0 {
			t.Fatalf("outbox operations did not all reconcile: still-pending=%#v", items)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Step 5: no active Lease remains, and only then does verification
	// reach ControlVerified. The Lease is still LeaseActive until the
	// reconciler actually fences it (Renew has been denied since tick 1,
	// but nothing expires a Lease early just because stop is in effect).
	clock.now = clock.now.Add(2 * time.Minute)
	rec := &Reconciler{Tx: st, Clock: clock}
	if _, _, err := rec.Tick(ctx, ""); err != nil {
		t.Fatalf("reconciler tick: %v", err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		lease, _, e := u.Lease(ctx, claim.LeaseID)
		if e != nil {
			return e
		}
		if lease.Status == domain.LeaseActive {
			t.Fatalf("lease=%#v, want no longer active before verification is asserted", lease)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	verifier := &VerificationReconciler{Tx: st, Clock: clock}
	changed, err := verifier.Tick(ctx)
	if err != nil {
		t.Fatalf("verification tick: %v", err)
	}
	if changed != 1 {
		t.Fatalf("changed=%d, want 1", changed)
	}
	assertControlProgress(t, st, ctrl.Revision, domain.ControlVerified, domain.VerificationVerified)
}

func settleAllOutbox(t *testing.T, st *memory.Store, clock *stopJourneyClock) {
	t.Helper()
	sink := &journeyFakeSink{}
	dispatcher, err := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "j5:settle"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type journeyFakeSink struct{}

func (journeyFakeSink) Deliver(context.Context, application.EffectDelivery) error { return nil }

func assertControlProgress(t *testing.T, st *memory.Store, revision domain.Revision, wantState domain.ControlState, wantVerification domain.VerificationState) {
	t.Helper()
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		progress, found, e := u.ControlProgress(context.Background(), revision)
		if e != nil {
			return e
		}
		if !found {
			t.Fatalf("no ControlProgress for revision %d", revision)
		}
		if progress.State != wantState || progress.Verification != wantVerification {
			t.Fatalf("progress=%#v, want State=%s Verification=%s", progress, wantState, wantVerification)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestJourneyFiveGracefulStopReachesControlVerified is Journey 5 for the
// graceful-stop family (acceptance A8): the whole failure-model.md section
// 5 protocol for a connected Runner, ending at ControlVerified with no
// active Lease remaining, where step 3's process action is a real
// Service.Checkpoint call (not a terminate). The three fail-closed
// directions are asserted as subtests of this same journey, against the
// real VerificationReconciler.Tick (the reconciler layer), not by
// re-deriving the pure VerifyControl proof TestVerifyControlFailsClosedForMixedReachabilityAndAmbiguity
// already carries.
func TestJourneyFiveGracefulStopReachesControlVerified(t *testing.T) {
	runJourneyFiveHappyPath(t, domain.ControlGracefulStop, "checkpointed", func(loop *runner.ControlLoop, checkpointCall func() error) {
		loop.Checkpoint = func(context.Context) error { return checkpointCall() }
	})

	t.Run("missing observation is pending before deadline and blocked-unreachable after", func(t *testing.T) {
		ctx := context.Background()
		now := time.Unix(1_700_400_000, 0).UTC()
		st := memory.New()
		runnerID, _ := domain.NewRunnerID("runner-missing")
		execID, _ := domain.NewExecutionID("exec-missing")
		leaseID, _ := domain.NewLeaseID("lease-missing")
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			return u.SaveControlProgress(ctx, domain.ControlProgress{Revision: 1, State: domain.ControlRequested, RequestedAt: now, EffectiveAt: now, Verification: domain.VerificationPending, Targets: []domain.ControlTargetSnapshot{{Target: domain.ControlTarget{RunnerID: runnerID}, LeaseID: leaseID, ExecutionID: execID}}}, "")
		}); err != nil {
			t.Fatal(err)
		}
		clock := &stopJourneyClock{now: now.Add(30 * time.Second)}
		verifier := &VerificationReconciler{Tx: st, Clock: clock, Deadline: time.Minute}
		if changed, err := verifier.Tick(ctx); err != nil || changed != 0 {
			t.Fatalf("before deadline: changed=%d err=%v, want 0/nil", changed, err)
		}
		assertControlProgress(t, st, 1, domain.ControlRequested, domain.VerificationPending)

		clock.now = now.Add(2 * time.Minute)
		if changed, err := verifier.Tick(ctx); err != nil || changed != 1 {
			t.Fatalf("after deadline: changed=%d err=%v, want 1/nil", changed, err)
		}
		assertControlProgress(t, st, 1, domain.ControlEffective, domain.VerificationBlockedUnreachable)
	})

	t.Run("mixed reachable and unreachable targets never verify", func(t *testing.T) {
		ctx := context.Background()
		now := time.Unix(1_700_500_000, 0).UTC()
		st := memory.New()
		reachableRunner, _ := domain.NewRunnerID("runner-reachable")
		reachableExec, _ := domain.NewExecutionID("exec-reachable")
		unreachableRunner, _ := domain.NewRunnerID("runner-unreachable")
		unreachableExec, _ := domain.NewExecutionID("exec-unreachable")
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			if err := u.SaveRunnerObservation(ctx, domain.RunnerObservation{RunnerID: reachableRunner, AppliedRevision: 1, Reachable: true, Processes: []domain.ProcessObservation{{ProcessID: reachableExec.String(), State: "checkpointed", At: now}}, ObservedAt: now}); err != nil {
				return err
			}
			return u.SaveControlProgress(ctx, domain.ControlProgress{Revision: 1, State: domain.ControlRequested, RequestedAt: now, EffectiveAt: now, Verification: domain.VerificationPending, Targets: []domain.ControlTargetSnapshot{
				{Target: domain.ControlTarget{RunnerID: reachableRunner}, ExecutionID: reachableExec},
				{Target: domain.ControlTarget{RunnerID: unreachableRunner}, ExecutionID: unreachableExec},
			}}, "")
		}); err != nil {
			t.Fatal(err)
		}
		clock := &stopJourneyClock{now: now.Add(2 * time.Minute)}
		verifier := &VerificationReconciler{Tx: st, Clock: clock, Deadline: time.Minute}
		if _, err := verifier.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			progress, _, e := u.ControlProgress(ctx, 1)
			if e != nil {
				return e
			}
			if progress.Verification == domain.VerificationVerified {
				t.Fatalf("mixed reachable/unreachable target set verified: %#v", progress)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ambiguous outbox resolution blocks ahead of a success-looking report", func(t *testing.T) {
		ctx := context.Background()
		now := time.Unix(1_700_600_000, 0).UTC()
		st := memory.New()
		runnerID, _ := domain.NewRunnerID("runner-ambiguous")
		execID, _ := domain.NewExecutionID("exec-ambiguous")
		leaseID, _ := domain.NewLeaseID("lease-ambiguous")
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			if err := u.SaveRunnerObservation(ctx, domain.RunnerObservation{RunnerID: runnerID, AppliedRevision: 1, Reachable: true, Processes: []domain.ProcessObservation{{ProcessID: execID.String(), State: "terminated", At: now}}, ObservedAt: now}); err != nil {
				return err
			}
			if err := u.SaveControlProgress(ctx, domain.ControlProgress{Revision: 1, State: domain.ControlRequested, RequestedAt: now, EffectiveAt: now, Verification: domain.VerificationPending, Targets: []domain.ControlTargetSnapshot{{Target: domain.ControlTarget{RunnerID: runnerID}, LeaseID: leaseID, ExecutionID: execID}}}, ""); err != nil {
				return err
			}
			return u.Record(application.Event{ID: "ambiguous-event"}, &application.OutboxItem{ID: "ambiguous-outbox", OperationID: "operation-ambiguous", Kind: "fake", Target: "target-ambiguous", LeaseID: leaseID.String(), Status: application.OutboxAmbiguous})
		}); err != nil {
			t.Fatal(err)
		}
		clock := &stopJourneyClock{now: now}
		verifier := &VerificationReconciler{Tx: st, Clock: clock, Deadline: time.Minute}
		if changed, err := verifier.Tick(ctx); err != nil || changed != 1 {
			t.Fatalf("changed=%d err=%v, want 1/nil", changed, err)
		}
		assertControlProgress(t, st, 1, domain.ControlEffective, domain.VerificationBlockedAmbiguous)
	})
}

// TestJourneyFiveImmediateStopReachesControlVerified is Journey 5 for the
// immediate/emergency-stop family (acceptance A8): identical protocol to
// the graceful-stop journey above, except step 3's process action is a real
// terminate, not a checkpoint. emergency-stop and cancel share
// immediate-stop's kind gate exactly (control.go permitAllowed denies every
// kind identically for all three) and are not repeated here.
func TestJourneyFiveImmediateStopReachesControlVerified(t *testing.T) {
	runJourneyFiveHappyPath(t, domain.ControlImmediateStop, "terminated", func(loop *runner.ControlLoop, _ func() error) {
		loop.StopProcess = func(context.Context) error { return nil }
	})
}
