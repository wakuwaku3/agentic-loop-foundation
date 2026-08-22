package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

func TestVerifyControlFailsClosedForMixedReachabilityAndAmbiguity(t *testing.T) {
	now := time.Unix(10, 0)
	runner, _ := domain.NewRunnerID("runner-1")
	execution, _ := domain.NewExecutionID("execution-1")
	p := domain.ControlProgress{Revision: 2, Verification: domain.VerificationPending, Targets: []domain.ControlTargetSnapshot{{Target: domain.ControlTarget{RunnerID: runner}, ExecutionID: execution}}}
	obs := map[string]domain.RunnerObservation{runner.String(): {RunnerID: runner, Reachable: false, AppliedRevision: 2, Processes: []domain.ProcessObservation{{ProcessID: execution.String(), State: "terminated"}}}}
	if got := VerifyControl(p, obs, false, false, now, now.Add(time.Minute)); got != domain.VerificationPending {
		t.Fatal(got)
	}
	if got := VerifyControl(p, obs, false, false, now.Add(2*time.Minute), now.Add(time.Minute)); got != domain.VerificationBlockedUnreachable {
		t.Fatal(got)
	}
	if got := VerifyControl(p, obs, false, true, now, now); got != domain.VerificationBlockedAmbiguous {
		t.Fatal(got)
	}
	obs[runner.String()] = domain.RunnerObservation{RunnerID: runner, Reachable: true, AppliedRevision: 2, Processes: []domain.ProcessObservation{{ProcessID: execution.String(), State: "terminated"}}}
	if got := VerifyControl(p, obs, false, false, now, now.Add(time.Minute)); got != domain.VerificationVerified {
		t.Fatal(got)
	}
	if got := VerifyControl(domain.ControlProgress{Revision: 3}, nil, false, false, now, now); got != domain.VerificationVerified {
		t.Fatalf("empty target set must verify, got %s", got)
	}
}

func TestVerificationReconcilerPersistsVerifiedEvidence(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000200, 0).UTC()
	runner, _ := domain.NewRunnerID("runner-1")
	execution, _ := domain.NewExecutionID("execution-1")
	lease, _ := domain.NewLeaseID("lease-1")
	st := memory.New()
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: execution, RunnerID: runner, Status: domain.LeaseExpired, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveControlProgress(ctx, domain.ControlProgress{Revision: 4, State: domain.ControlRequested, EffectiveAt: now.Add(-time.Minute), Verification: domain.VerificationPending, Targets: []domain.ControlTargetSnapshot{{Target: domain.ControlTarget{RunnerID: runner}, LeaseID: lease, ExecutionID: execution}}}, ""); err != nil {
			return err
		}
		return u.SaveRunnerObservation(ctx, domain.RunnerObservation{RunnerID: runner, AppliedRevision: 4, Reachable: true, Processes: []domain.ProcessObservation{{ProcessID: execution.String(), State: "terminated", At: now}}, ObservedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	r := &VerificationReconciler{Tx: st, Clock: fakeClock{now: now}}
	if changed, err := r.Tick(ctx); err != nil || changed != 1 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		progress, found, err := u.ControlProgress(ctx, 4)
		if err != nil || !found || progress.State != domain.ControlVerified || progress.Verification != domain.VerificationVerified || !progress.VerifiedAt.Equal(now) {
			t.Fatalf("progress=%#v found=%v err=%v", progress, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
