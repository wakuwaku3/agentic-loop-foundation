package reconciler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// TestOrphanSweepRecoversExecutionBehindAlreadyExpiredLease reproduces and
// then closes the second measured non-convergence defect (dp-v2-019 d7,
// acceptance A4/A5). Precondition, reproduced first against the unchanged
// Reconciler.Tick: with a Lease already LeaseExpired (not merely past its
// TTL while still LeaseActive) and its Execution still ExecutionRunning,
// Reconciler.Tick reports {Scanned:0 Recovered:0} on every pass forever,
// because ExpiredActiveLeases filters lease_status == active and
// recoverOne's own re-check also rejects a Lease whose Status is not
// LeaseActive. That precondition is a fact about Reconciler.Tick's scope,
// not a bug to fix in Tick itself: OrphanSweep is the new capability that
// closes it, composing only pre-existing UnitOfWork read ports (dp-v2-019
// d5), and this test asserts OrphanSweep.Tick recovers exactly what
// Reconciler.Tick structurally cannot see.
func TestOrphanSweepRecoversExecutionBehindAlreadyExpiredLease(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1700000600, 0).UTC()
	st := memory.New()

	req, _ := domain.NewRequirementID("req-orphan")
	inc, _ := domain.NewIncrementID("inc-orphan")
	exec, _ := domain.NewExecutionID("exec-orphan")
	lease, _ := domain.NewLeaseID("lease-orphan")
	runner, _ := domain.NewRunnerID("runner-orphan")

	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: exec, IncrementID: inc, RunnerID: runner, FencingToken: 1, Status: domain.LeaseExpired, IssuedAt: at.Add(-time.Hour), ExpiresAt: at.Add(-time.Minute), Version: 2}, 0); err != nil {
			return err
		}
		return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}

	// Precondition: Reconciler.Tick alone cannot see this candidate, on
	// however many passes.
	r := &Reconciler{Tx: st, Clock: fakeClock{now: at}}
	for i := 0; i < 3; i++ {
		report, _, err := r.Tick(ctx, "")
		if err != nil {
			t.Fatalf("pass %d: unexpected error %v", i, err)
		}
		if report.Scanned != 0 || report.Recovered != 0 {
			t.Fatalf("pass %d: report=%#v, want a zero scan (Reconciler.Tick cannot see an already-expired Lease)", i, report)
		}
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		execution, _, _ := u.Execution(ctx, exec.String())
		if execution.Status != domain.ExecutionRunning {
			t.Fatalf("execution=%#v, want still ExecutionRunning before the sweep", execution)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// OrphanSweep.Tick recovers it.
	sweep := &OrphanSweep{Tx: st, Clock: fakeClock{now: at}}
	report, cursor, err := sweep.Tick(ctx, "")
	if err != nil {
		t.Fatalf("orphan sweep returned an error: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report=%#v cursor=%q, want Recovered:1", report, cursor)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		execution, _, _ := u.Execution(ctx, exec.String())
		if execution.Status != domain.ExecutionLost {
			t.Fatalf("execution=%#v, want ExecutionLost after the sweep", execution)
		}
		increment, _, _ := u.Increment(ctx, inc.String())
		if increment.Status != domain.IncrementReady {
			t.Fatalf("increment=%#v, want IncrementReady after the sweep", increment)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A second sweep must find nothing left to recover: it must not
	// re-touch an already-terminal Execution.
	report, _, err = sweep.Tick(ctx, "")
	if err != nil {
		t.Fatalf("second sweep returned an error: %v", err)
	}
	if report.Recovered != 0 {
		t.Fatalf("second sweep report=%#v, want Recovered:0 once converged", report)
	}
}

// TestOrphanSweepRecoversExecutionWithNoLeaseRecordAtAll covers the "absent"
// branch of A4's three orphan causes (absent, expired, revoked): an
// Execution whose LeaseID does not resolve to any Lease record at all must
// still converge, using a synthesised linkage-only reference value so
// domain.MarkExecutionLost's consistency check has something to compare
// against.
func TestOrphanSweepRecoversExecutionWithNoLeaseRecordAtAll(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1700000700, 0).UTC()
	st := memory.New()

	req, _ := domain.NewRequirementID("req-noleaserecord")
	inc, _ := domain.NewIncrementID("inc-noleaserecord")
	exec, _ := domain.NewExecutionID("exec-noleaserecord")
	lease, _ := domain.NewLeaseID("lease-noleaserecord") // never saved
	runner, _ := domain.NewRunnerID("runner-noleaserecord")

	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
			return err
		}
		return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}

	sweep := &OrphanSweep{Tx: st, Clock: fakeClock{now: at}}
	report, _, err := sweep.Tick(ctx, "")
	if err != nil {
		t.Fatalf("orphan sweep returned an error: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report=%#v, want Recovered:1", report)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		execution, _, _ := u.Execution(ctx, exec.String())
		if execution.Status != domain.ExecutionLost {
			t.Fatalf("execution=%#v, want ExecutionLost", execution)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestOrphanSweepRevokedLeaseConverges covers the "revoked" branch.
func TestOrphanSweepRevokedLeaseConverges(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1700000800, 0).UTC()
	st := memory.New()

	req, _ := domain.NewRequirementID("req-revoked")
	inc, _ := domain.NewIncrementID("inc-revoked")
	exec, _ := domain.NewExecutionID("exec-revoked")
	lease, _ := domain.NewLeaseID("lease-revoked")
	runner, _ := domain.NewRunnerID("runner-revoked")

	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: exec, IncrementID: inc, RunnerID: runner, FencingToken: 1, Status: domain.LeaseRevoked, IssuedAt: at.Add(-time.Hour), ExpiresAt: at.Add(-time.Minute), Version: 2}, 0); err != nil {
			return err
		}
		return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}

	sweep := &OrphanSweep{Tx: st, Clock: fakeClock{now: at}}
	report, _, err := sweep.Tick(ctx, "")
	if err != nil {
		t.Fatalf("orphan sweep returned an error: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report=%#v, want Recovered:1", report)
	}
}

// TestOrphanSweepIsCursorPagedAndBounded is acceptance A5's structural
// pagination assertion, in the shape of A6/d9: over 150 Requirements each
// bound to exactly one orphaned Execution, one OrphanSweep.Tick pass must
// scan at most MaxBatch Requirements and never recover more than MaxBatch
// items, returning a non-empty cursor; the continuation pass must recover
// the remainder and return an empty cursor; no candidate is recovered
// twice; both passes must complete inside a bounded context deadline.
func TestOrphanSweepIsCursorPagedAndBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	at := time.Unix(1700000900, 0).UTC()
	st := memory.New()

	const total = MaxBatch + 50
	for i := 0; i < total; i++ {
		reqID := fmt.Sprintf("req-page-%04d", i)
		incID := fmt.Sprintf("inc-page-%04d", i)
		execID := fmt.Sprintf("exec-page-%04d", i)
		leaseID := fmt.Sprintf("lease-page-%04d", i)
		runnerID := fmt.Sprintf("runner-page-%04d", i)
		req, _ := domain.NewRequirementID(reqID)
		inc, _ := domain.NewIncrementID(incID)
		exec, _ := domain.NewExecutionID(execID)
		lease, _ := domain.NewLeaseID(leaseID)
		runner, _ := domain.NewRunnerID(runnerID)
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			r := domain.Requirement{ID: req, Status: domain.RequirementCaptured, Version: 1, Increments: []domain.IncrementID{inc}}
			if err := u.SaveRequirement(ctx, r, 0); err != nil {
				return err
			}
			if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
				return err
			}
			if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: exec, IncrementID: inc, RunnerID: runner, FencingToken: 1, Status: domain.LeaseExpired, IssuedAt: at.Add(-time.Hour), ExpiresAt: at.Add(-time.Minute), Version: 2}, 0); err != nil {
				return err
			}
			return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}

	sweep := &OrphanSweep{Tx: st, Clock: fakeClock{now: at}}
	start := time.Now()
	report1, cursor1, err := sweep.Tick(ctx, "")
	if err != nil {
		t.Fatalf("pass 1: unexpected error %v", err)
	}
	if report1.Scanned > MaxBatch || report1.Recovered > MaxBatch {
		t.Fatalf("pass 1: report=%#v, want at most MaxBatch=%d scanned and recovered", report1, MaxBatch)
	}
	if cursor1 == "" {
		t.Fatalf("pass 1: cursor is empty, want a continuation cursor with %d candidates remaining", total-report1.Recovered)
	}
	report2, cursor2, err := sweep.Tick(ctx, cursor1)
	if err != nil {
		t.Fatalf("pass 2: unexpected error %v", err)
	}
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("context deadline exceeded across both passes")
	}
	if report1.Recovered+report2.Recovered != total {
		t.Fatalf("recovered %d+%d=%d of %d total, want exactly %d recovered across both passes with none recovered twice", report1.Recovered, report2.Recovered, report1.Recovered+report2.Recovered, total, total)
	}
	if cursor2 != "" {
		t.Fatalf("pass 2 cursor=%q, want empty once the scan reaches the end", cursor2)
	}
	t.Logf("measured: %d candidates recovered across two passes in %s (observation only, not a threshold)", total, elapsed)
}
