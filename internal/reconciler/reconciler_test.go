package reconciler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
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

// TestTickClosesTerminalExecutionLeaseAndRecoversNextCandidate reproduces and
// then closes the poison-pill defect measured by Terra with go test -overlay
// on HEAD 576019e (dp-v2-019 d6, acceptance A2). A Lease that is still
// LeaseActive and past its TTL, but whose Execution already reached
// ExecutionSucceeded before the TTL elapsed, made domain.MarkExecutionLost
// return domain.ErrInvalidTransition; Tick treated only
// domain.ErrStaleVersion/domain.ErrStaleFence as skippable, so that error
// aborted the whole pass and the same poison candidate was re-read forever,
// starving every genuinely recoverable candidate sorted behind it. Before
// the fix this test fails with err wrapping domain.ErrInvalidTransition and
// report={Scanned:2 Recovered:0 Skipped:0} (Closed did not exist yet); after
// the fix the pass completes with no error, the terminal Execution and its
// Lease are accounted as Closed (not Recovered), and the genuinely
// recoverable candidate sorted after it still reaches ExecutionLost with its
// Increment back to ready.
func TestTickClosesTerminalExecutionLeaseAndRecoversNextCandidate(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	at := time.Unix(1700000300, 0).UTC()

	// Poison candidate: Lease still LeaseActive and expired, but its
	// Execution already reached ExecutionSucceeded before the TTL elapsed
	// (the happy-path case: nothing closes the Lease when the Execution
	// succeeds). Its lease id sorts strictly before the recoverable one.
	poisonReq, _ := domain.NewRequirementID("req-poison")
	poisonInc, _ := domain.NewIncrementID("inc-poison")
	poisonExec, _ := domain.NewExecutionID("exec-poison")
	poisonLease, _ := domain.NewLeaseID("lease-a-poison")
	poisonRunner, _ := domain.NewRunnerID("runner-poison")

	// Genuinely recoverable candidate, sorting strictly after the poison
	// lease id, so a pass that aborts on the poison candidate never reaches
	// it.
	realReq, _ := domain.NewRequirementID("req-real")
	realInc, _ := domain.NewIncrementID("inc-real")
	realExec, _ := domain.NewExecutionID("exec-real")
	realLease, _ := domain.NewLeaseID("lease-b-real")
	realRunner, _ := domain.NewRunnerID("runner-real")

	seed := func(u application.UnitOfWork) error {
		if err := u.SaveIncrement(ctx, domain.Increment{ID: poisonInc, RequirementID: poisonReq, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveLease(ctx, domain.Lease{ID: poisonLease, ExecutionID: poisonExec, IncrementID: poisonInc, RunnerID: poisonRunner, FencingToken: 1, Status: domain.LeaseActive, IssuedAt: at.Add(-time.Hour), ExpiresAt: at.Add(-time.Minute), Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveExecution(ctx, domain.Execution{ID: poisonExec, IncrementID: poisonInc, RunnerID: poisonRunner, LeaseID: poisonLease, FencingToken: 1, Status: domain.ExecutionSucceeded, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveIncrement(ctx, domain.Increment{ID: realInc, RequirementID: realReq, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveLease(ctx, domain.Lease{ID: realLease, ExecutionID: realExec, IncrementID: realInc, RunnerID: realRunner, FencingToken: 1, Status: domain.LeaseActive, IssuedAt: at.Add(-time.Hour), ExpiresAt: at.Add(-time.Minute), Version: 1}, 0); err != nil {
			return err
		}
		return u.SaveExecution(ctx, domain.Execution{ID: realExec, IncrementID: realInc, RunnerID: realRunner, LeaseID: realLease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
	}
	if err := st.Transact(ctx, seed); err != nil {
		t.Fatal(err)
	}

	r := &Reconciler{Tx: st, Clock: fakeClock{now: at}}
	report, _, err := r.Tick(ctx, "")
	if err != nil {
		t.Fatalf("Tick returned an error, poison pill still aborts the pass: %v", err)
	}
	if report.Scanned != 2 || report.Closed != 1 || report.Recovered != 1 || report.Skipped != 0 {
		t.Fatalf("report=%#v, want Scanned:2 Closed:1 Recovered:1 Skipped:0", report)
	}

	// A converged pass must not keep re-touching the same candidates: two
	// more Ticks must find nothing left to scan.
	for i := 0; i < 2; i++ {
		report, _, err := r.Tick(ctx, "")
		if err != nil {
			t.Fatalf("pass %d: unexpected error %v", i, err)
		}
		if report != (Report{}) {
			t.Fatalf("pass %d: report=%#v, want a zero report once converged", i, report)
		}
	}

	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		poison, _, _ := u.Lease(ctx, poisonLease.String())
		if poison.Status != domain.LeaseExpired {
			t.Fatalf("poison lease=%#v, want LeaseExpired", poison)
		}
		poisonExecution, _, _ := u.Execution(ctx, poisonExec.String())
		if poisonExecution.Status != domain.ExecutionSucceeded {
			t.Fatalf("terminal execution was mutated: %#v", poisonExecution)
		}
		real, _, _ := u.Lease(ctx, realLease.String())
		if real.Status != domain.LeaseExpired {
			t.Fatalf("real lease=%#v, want LeaseExpired", real)
		}
		realExecution, _, _ := u.Execution(ctx, realExec.String())
		if realExecution.Status != domain.ExecutionLost {
			t.Fatalf("real execution=%#v, want ExecutionLost", realExecution)
		}
		realIncrement, _, _ := u.Increment(ctx, realInc.String())
		if realIncrement.Status != domain.IncrementReady {
			t.Fatalf("real increment=%#v, want IncrementReady", realIncrement)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRecoverOneClassifiesPerItemOutcomes is acceptance A3's table: Tick must
// classify each per-item outcome explicitly and never loop on one.
// domain.ErrStaleVersion and domain.ErrStaleFence must be reported as
// skippable (recoverOne's second return value is meaningless on error, so
// only the error identity is asserted), a terminal Execution must be
// reported as Closed (covered by
// TestTickClosesTerminalExecutionLeaseAndRecoversNextCandidate above and not
// repeated here), and a genuine recovery must be reported as Recovered.
func TestRecoverOneClassifiesPerItemOutcomes(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1700000400, 0).UTC()

	t.Run("missing lease is stale version", func(t *testing.T) {
		st := memory.New()
		r := &Reconciler{Tx: st, Clock: fakeClock{now: at}}
		closed, err := r.recoverOne(ctx, "no-such-lease", at)
		if !errors.Is(err, domain.ErrStaleVersion) {
			t.Fatalf("err=%v, want domain.ErrStaleVersion", err)
		}
		if closed {
			t.Fatalf("closed=true on an error path, want false")
		}
	})

	t.Run("fencing mismatch is stale fence", func(t *testing.T) {
		st := memory.New()
		req, _ := domain.NewRequirementID("req-fence")
		inc, _ := domain.NewIncrementID("inc-fence")
		exec, _ := domain.NewExecutionID("exec-fence")
		lease, _ := domain.NewLeaseID("lease-fence")
		runner, _ := domain.NewRunnerID("runner-fence")
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
				return err
			}
			if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: exec, IncrementID: inc, RunnerID: runner, FencingToken: 7, Status: domain.LeaseActive, IssuedAt: at.Add(-time.Minute), ExpiresAt: at.Add(-time.Second), Version: 1}, 0); err != nil {
				return err
			}
			// FencingToken deliberately does not match the Lease's, which
			// is exactly the mismatch domain.MarkExecutionLost reports as
			// domain.ErrStaleFence.
			return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 3, Status: domain.ExecutionRunning, Version: 1}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		r := &Reconciler{Tx: st, Clock: fakeClock{now: at}}
		closed, err := r.recoverOne(ctx, lease.String(), at)
		if !errors.Is(err, domain.ErrStaleFence) {
			t.Fatalf("err=%v, want domain.ErrStaleFence", err)
		}
		if closed {
			t.Fatalf("closed=true on an error path, want false")
		}
	})

	t.Run("genuine recovery is Recovered not Closed", func(t *testing.T) {
		st := memory.New()
		req, _ := domain.NewRequirementID("req-genuine")
		inc, _ := domain.NewIncrementID("inc-genuine")
		exec, _ := domain.NewExecutionID("exec-genuine")
		lease, _ := domain.NewLeaseID("lease-genuine")
		runner, _ := domain.NewRunnerID("runner-genuine")
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
				return err
			}
			if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: exec, IncrementID: inc, RunnerID: runner, FencingToken: 1, Status: domain.LeaseActive, IssuedAt: at.Add(-time.Minute), ExpiresAt: at.Add(-time.Second), Version: 1}, 0); err != nil {
				return err
			}
			return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		r := &Reconciler{Tx: st, Clock: fakeClock{now: at}}
		closed, err := r.recoverOne(ctx, lease.String(), at)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if closed {
			t.Fatalf("closed=true for a genuine recovery, want false")
		}
	})
}

// fakeOverBudgetTx wraps a real Transactor and, once armed, forces the next
// Transact call to fail with quota.ErrOverBudget without touching the
// wrapped store's state. This isolates the abort-on-budget assertion from
// having to drive the shared in-memory quota.Counter through thousands of
// real reservations.
type fakeOverBudgetTx struct {
	inner application.Transactor
	fail  bool
}

func (f *fakeOverBudgetTx) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	if f.fail {
		return fmt.Errorf("mutation: %w", quota.ErrOverBudget)
	}
	return f.inner.Transact(ctx, fn)
}

// TestTickAbortsPassOnOverBudgetQuota asserts the other half of A3: unlike
// domain.ErrStaleVersion/domain.ErrStaleFence, quota.ErrOverBudget from
// ReserveQuota must still abort the whole pass and be returned to the caller
// unwrapped enough for errors.Is to match. Swallowing it would convert a
// hard Budget stop into a silent partial sweep and would let a new
// reconciliation mutation start after a hard Budget overrun, which
// dp-v2-019 d6 and the Budget safety invariant both forbid.
func TestTickAbortsPassOnOverBudgetQuota(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1700000500, 0).UTC()
	inner := memory.New()
	req, _ := domain.NewRequirementID("req-budget")
	inc, _ := domain.NewIncrementID("inc-budget")
	exec, _ := domain.NewExecutionID("exec-budget")
	lease, _ := domain.NewLeaseID("lease-budget")
	runner, _ := domain.NewRunnerID("runner-budget")
	if err := inner.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
			return err
		}
		if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: exec, IncrementID: inc, RunnerID: runner, FencingToken: 1, Status: domain.LeaseActive, IssuedAt: at.Add(-time.Minute), ExpiresAt: at.Add(-time.Second), Version: 1}, 0); err != nil {
			return err
		}
		return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	tx := &fakeOverBudgetTx{inner: inner}
	r := &Reconciler{Tx: tx, Clock: fakeClock{now: at}}
	// The candidate-listing Transact (a read) must succeed; only
	// recoverOne's mutation Transact is forced over budget.
	tx.fail = true
	report, cursor, err := r.Tick(ctx, "")
	if !errors.Is(err, quota.ErrOverBudget) {
		t.Fatalf("err=%v cursor=%q report=%#v, want quota.ErrOverBudget", err, cursor, report)
	}
	if report.Recovered != 0 || report.Closed != 0 {
		t.Fatalf("report=%#v, want no recovery credited once the pass aborted on budget", report)
	}
}

// TestTickPaginatesOverMaxBatchExpiredLeases is acceptance A6 / dp-v2-019
// d9's structural ceiling assertion for Reconciler.Tick itself (distinct
// from OrphanSweep's own pagination test in orphan_test.go): over 150
// expired candidates, the first Tick must recover exactly MaxBatch=100 and
// return a non-empty cursor, the continuation Tick must recover the
// remaining 50 and return an empty cursor, no candidate must be recovered
// twice, and both Ticks must complete inside a bounded context deadline.
// Terra's pre-change measurement on the memory store was {Scanned:100
// Recovered:100} then {Scanned:50 Recovered:50} in 10.7ms total; that
// duration is recorded here as an observation, not asserted as a
// threshold (validation.md section 9 forbids promoting a wall-clock-varying
// result).
func TestTickPaginatesOverMaxBatchExpiredLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	at := time.Unix(1700001000, 0).UTC()
	st := memory.New()

	const total = MaxBatch + 50
	for i := 0; i < total; i++ {
		req, _ := domain.NewRequirementID(fmt.Sprintf("req-batch-%04d", i))
		inc, _ := domain.NewIncrementID(fmt.Sprintf("inc-batch-%04d", i))
		exec, _ := domain.NewExecutionID(fmt.Sprintf("exec-batch-%04d", i))
		lease, _ := domain.NewLeaseID(fmt.Sprintf("lease-batch-%04d", i))
		runner, _ := domain.NewRunnerID(fmt.Sprintf("runner-batch-%04d", i))
		if err := st.Transact(ctx, func(u application.UnitOfWork) error {
			if err := u.SaveIncrement(ctx, domain.Increment{ID: inc, RequirementID: req, Status: domain.IncrementLeased, Version: 1}, 0); err != nil {
				return err
			}
			if err := u.SaveLease(ctx, domain.Lease{ID: lease, ExecutionID: exec, IncrementID: inc, RunnerID: runner, FencingToken: 1, Status: domain.LeaseActive, IssuedAt: at.Add(-time.Hour), ExpiresAt: at.Add(-time.Minute), Version: 1}, 0); err != nil {
				return err
			}
			return u.SaveExecution(ctx, domain.Execution{ID: exec, IncrementID: inc, RunnerID: runner, LeaseID: lease, FencingToken: 1, Status: domain.ExecutionRunning, Version: 1}, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}

	r := &Reconciler{Tx: st, Clock: fakeClock{now: at}}
	start := time.Now()
	report1, cursor1, err := r.Tick(ctx, "")
	if err != nil {
		t.Fatalf("pass 1: unexpected error %v", err)
	}
	if report1.Scanned != MaxBatch || report1.Recovered != MaxBatch {
		t.Fatalf("pass 1: report=%#v, want Scanned:%d Recovered:%d", report1, MaxBatch, MaxBatch)
	}
	if cursor1 == "" {
		t.Fatal("pass 1: cursor is empty, want a continuation cursor with 50 candidates remaining")
	}
	report2, cursor2, err := r.Tick(ctx, cursor1)
	if err != nil {
		t.Fatalf("pass 2: unexpected error %v", err)
	}
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatal("context deadline exceeded across both passes")
	}
	if report2.Scanned != total-MaxBatch || report2.Recovered != total-MaxBatch {
		t.Fatalf("pass 2: report=%#v, want Scanned:%d Recovered:%d", report2, total-MaxBatch, total-MaxBatch)
	}
	if cursor2 != "" {
		t.Fatalf("pass 2 cursor=%q, want empty once the scan reaches the end", cursor2)
	}
	if report1.Recovered+report2.Recovered != total {
		t.Fatalf("recovered %d+%d=%d of %d total, want none recovered twice", report1.Recovered, report2.Recovered, report1.Recovered+report2.Recovered, total)
	}
	t.Logf("measured: %d candidates recovered across two passes in %s (observation only, not a threshold)", total, elapsed)
}
