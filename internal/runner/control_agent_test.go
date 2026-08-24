package runner

import (
	"context"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/reconciler"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// TestControlAgentAppliesFailClosedStopWhenLatestModeMissingOrUnknown is
// acceptance A10's required negative test: without it the additive
// latest_mode field would not actually be safely optional. When
// LatestRevision exceeds the revision already applied but LatestMode is
// empty (absent from an older/partial response) or carries a value the
// Runner does not recognise, the agent must fail closed to
// domain.ControlImmediateStop rather than silently treat the revision as
// domain.ControlAllow.
func TestControlAgentAppliesFailClosedStopWhenLatestModeMissingOrUnknown(t *testing.T) {
	cases := []struct {
		name string
		mode domain.ControlMode
	}{
		{"latest_mode absent", ""},
		{"latest_mode unknown", domain.ControlMode("some-future-mode")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, err := OpenJournal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			stopped := false
			loop := &ControlLoop{Journal: j, StopProcess: func(context.Context) error { stopped = true; return nil }}
			agent := &ControlAgent{Loop: loop}
			state, applied, err := agent.Tick(context.Background(), application.LifecycleResponse{LatestRevision: 5, LatestMode: tc.mode})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !applied {
				t.Fatal("expected the agent to apply a revision it had not yet applied")
			}
			if !stopped {
				t.Fatal("fail-closed path did not call StopProcess: an unapplied revision with a missing/unknown mode must stop, not guess ControlAllow")
			}
			if state != "terminated" {
				t.Fatalf("state=%q, want terminated", state)
			}
			if agent.Applied() != 5 {
				t.Fatalf("Applied()=%d, want 5", agent.Applied())
			}
			events, err := j.Replay()
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[0].Kind != "control_received" {
				t.Fatalf("events=%#v", events)
			}
		})
	}
}

// TestControlAgentDoesNotReapplyAnAlreadyAppliedOrOlderRevision is a small
// unit check that Tick is a true no-op once caught up: a LatestRevision at
// or below the applied revision must not re-invoke ControlLoop.
func TestControlAgentDoesNotReapplyAnAlreadyAppliedOrOlderRevision(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	loop := &ControlLoop{Journal: j, StopProcess: func(context.Context) error { calls++; return nil }}
	agent := &ControlAgent{Loop: loop}
	if _, applied, err := agent.Tick(context.Background(), application.LifecycleResponse{LatestRevision: 3, LatestMode: domain.ControlImmediateStop}); err != nil || !applied {
		t.Fatalf("applied=%v err=%v, want applied=true", applied, err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	// Same revision again: no-op.
	if _, applied, err := agent.Tick(context.Background(), application.LifecycleResponse{LatestRevision: 3, LatestMode: domain.ControlImmediateStop}); err != nil || applied {
		t.Fatalf("applied=%v err=%v, want applied=false for a revision already applied", applied, err)
	}
	// Zero revision (no control ever set): also a no-op.
	if _, applied, err := agent.Tick(context.Background(), application.LifecycleResponse{}); err != nil || applied {
		t.Fatalf("applied=%v err=%v, want applied=false for a zero LatestRevision", applied, err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want still 1 (no re-application)", calls)
	}
}

// controlAgentFixture builds a claimed lease directly against a fresh
// in-memory application.Service, mirroring leaseTestFixture in lease_test.go
// but also exposing the Requirement/Increment ids Control's installation
// scope needs.
func newControlAgentFixture(t *testing.T) (*application.Service, *memory.Store, *mutableClock, application.ClaimResponse) {
	t.Helper()
	clock := newMutableClock(time.Unix(1_700_100_000, 0).UTC())
	st := memory.New()
	service, err := application.NewServiceWithConfig(st, clock, &journeyIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	owner := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	cap, err := service.Capture(owner, application.CaptureRequest{RequestID: "ca:capture", Text: "control agent fixture"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(owner, application.PlanRequest{RequestID: "ca:plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: cap.Version})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(owner, application.PrepareRequest{RequestID: "ca:prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.ControlTarget{RequirementID: mustRequirement(cap.RequirementID), IncrementID: mustIncrement(plan.IncrementID)}
	claim, err := service.Claim(runnerCtx, application.ClaimRequest{RequestID: "ca:claim", IncrementID: plan.IncrementID, ExpectedIncrementVersion: prepared.Version, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	// Fake-deliver Claim's own "claim-issued" outbox effect so this
	// fixture's verification path is not blocked on an unrelated pending
	// external effect: this test is exercising the control-observation
	// loop (A10), not the outbox dispatcher, which is proven elsewhere
	// (internal/application/outbox_test.go).
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		items, e := u.Outboxes(context.Background(), clock.Now().Add(time.Hour), 100)
		if e != nil {
			return e
		}
		for _, item := range items {
			item.Status = application.OutboxDelivered
			item.DeliveredAt = clock.Now()
			if e := u.SaveOutbox(context.Background(), item, item.Version); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return service, st, clock, claim
}

// TestControlAgentClosesVerificationLoopPositiveAndNegativeControl is
// acceptance A10's positive control: with the agent wired into a
// LeaseKeeper, the Control Plane's verification reconciler reaches
// ControlVerified once the reconciler has also fenced the Runner's now-stale
// Lease (proving the transition is driven by a durable RunnerObservation,
// not by the mere existence of a Control Intent); with the agent absent
// (Tick never applies anything), the same sequence leaves verification
// VerificationPending forever, because Heartbeat.AppliedRevision then never
// reaches the stop revision and no ProcessObservation is ever reported.
func TestControlAgentClosesVerificationLoopPositiveAndNegativeControl(t *testing.T) {
	t.Run("agent wired reaches ControlVerified", func(t *testing.T) {
		service, st, clock, claim := newControlAgentFixture(t)
		owner := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
		runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

		ctrl, err := service.Control(owner, application.ControlRequest{RequestID: "ca:stop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlImmediateStop, At: clock.Now()})
		if err != nil {
			t.Fatal(err)
		}

		j, err := OpenJournal(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		loop := &ControlLoop{Journal: j, StopProcess: func(context.Context) error { return nil }}
		agent := &ControlAgent{Loop: loop}
		keeper := &LeaseKeeper{Service: service, LeaseID: claim.LeaseID, RequestBase: "ca:agent", Agent: agent, ExecutionID: claim.ExecutionID}

		// Tick 1: Renew is denied by the now-effective stop (reported as
		// Denied, not an aborting error); Heartbeat observes LatestRevision
		// and LatestMode; the agent applies immediate-stop through
		// ControlLoop and records "terminated" for the next Heartbeat.
		out1, err := keeper.Tick(runnerCtx, 1, claim.FencingToken)
		if err != nil {
			t.Fatalf("tick 1: %v", err)
		}
		if !out1.Denied {
			t.Fatal("tick 1: expected Renew to be denied by the effective stop")
		}
		if out1.HeartbeatResult.LatestRevision != ctrl.Revision || out1.HeartbeatResult.LatestMode != domain.ControlImmediateStop {
			t.Fatalf("tick 1: heartbeat result=%#v, want LatestRevision=%d LatestMode=immediate-stop", out1.HeartbeatResult, ctrl.Revision)
		}

		// Tick 2: reports the applied revision and the "terminated"
		// observation the agent captured on tick 1.
		out2, err := keeper.Tick(runnerCtx, 1, claim.FencingToken)
		if err != nil {
			t.Fatalf("tick 2: %v", err)
		}
		if out2.HeartbeatResult.AppliedRevision != ctrl.Revision {
			t.Fatalf("tick 2: AppliedRevision=%d, want %d", out2.HeartbeatResult.AppliedRevision, ctrl.Revision)
		}

		// The reconciler must fence the now-stale (never renewed) Lease
		// before verification can stop treating it as still pending.
		clock.Advance(2 * time.Minute)
		rec := &reconciler.Reconciler{Tx: st, Clock: clock}
		if _, _, err := rec.Tick(context.Background(), ""); err != nil {
			t.Fatalf("reconciler tick: %v", err)
		}

		verifier := &reconciler.VerificationReconciler{Tx: st, Clock: clock}
		changed, err := verifier.Tick(context.Background())
		if err != nil {
			t.Fatalf("verification tick: %v", err)
		}
		if changed != 1 {
			t.Fatalf("changed=%d, want 1", changed)
		}
	})

	t.Run("agent absent stays VerificationPending", func(t *testing.T) {
		service, st, clock, claim := newControlAgentFixture(t)
		owner := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
		runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

		if _, err := service.Control(owner, application.ControlRequest{RequestID: "ca:stop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlImmediateStop, At: clock.Now()}); err != nil {
			t.Fatal(err)
		}

		// No ControlAgent wired at all: the negative control.
		keeper := &LeaseKeeper{Service: service, LeaseID: claim.LeaseID, RequestBase: "ca:noagent", ExecutionID: claim.ExecutionID}
		if _, err := keeper.Tick(runnerCtx, 1, claim.FencingToken); err != nil {
			t.Fatalf("tick 1: %v", err)
		}
		if _, err := keeper.Tick(runnerCtx, 1, claim.FencingToken); err != nil {
			t.Fatalf("tick 2: %v", err)
		}

		clock.Advance(2 * time.Minute)
		rec := &reconciler.Reconciler{Tx: st, Clock: clock}
		if _, _, err := rec.Tick(context.Background(), ""); err != nil {
			t.Fatalf("reconciler tick: %v", err)
		}

		verifier := &reconciler.VerificationReconciler{Tx: st, Clock: clock}
		changed, err := verifier.Tick(context.Background())
		if err != nil {
			t.Fatalf("verification tick: %v", err)
		}
		if changed != 0 {
			t.Fatalf("changed=%d, want 0: without the agent, nothing ever reports a terminated/checkpointed process observation", changed)
		}
	})
}
