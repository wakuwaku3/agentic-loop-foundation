package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

func TestTickExpiresLeaseAndFencesLostExecution(t *testing.T) {
	st := memory.New()
	at := time.Unix(1700000100, 0).UTC()
	rid, _ := domain.NewRequirementID("req")
	iid, _ := domain.NewIncrementID("inc")
	eid, _ := domain.NewExecutionID("exec")
	lid, _ := domain.NewLeaseID("lease")
	runnerID, _ := domain.NewRunnerID("runner")
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		if err := u.SaveIncrement(context.Background(), domain.Increment{ID: iid, RequirementID: rid, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveLease(context.Background(), domain.Lease{ID: lid, ExecutionID: eid, IncrementID: iid, RunnerID: runnerID, FencingToken: 7, Status: domain.LeaseActive, IssuedAt: at.Add(-time.Minute), ExpiresAt: at.Add(-time.Second), Version: 1}, 0); err != nil {
			return err
		}
		return u.SaveExecution(context.Background(), domain.Execution{ID: eid, IncrementID: iid, RunnerID: runnerID, LeaseID: lid, FencingToken: 7, Status: domain.ExecutionRunning, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Tx: st, Clock: fakeClock{now: at}}
	report, cursor, err := r.Tick(context.Background(), "")
	if err != nil || report.Recovered != 1 {
		t.Fatalf("report=%#v cursor=%q err=%v", report, cursor, err)
	}
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		lease, _, _ := u.Lease(context.Background(), "lease")
		if lease.Status != domain.LeaseExpired {
			t.Fatalf("lease=%#v", lease)
		}
		exec, _, _ := u.Execution(context.Background(), "exec")
		if exec.Status != domain.ExecutionLost {
			t.Fatalf("execution=%#v", exec)
		}
		inc, _, _ := u.Increment(context.Background(), "inc")
		if inc.Status != domain.IncrementReady {
			t.Fatalf("increment=%#v", inc)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
