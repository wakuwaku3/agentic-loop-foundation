// Package reconciler contains bounded durable recovery passes. It never calls
// a provider; it only re-reads authoritative state inside a transaction and
// advances lease/execution/increment state with the current fencing token.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
)

const MaxBatch = 100

type Reconciler struct {
	Tx    application.Transactor
	Clock application.Clock
}

// Report tallies one Tick pass. Closed counts a Lease whose Execution had
// already reached a terminal outcome before its TTL elapsed: the Lease is
// still closed (so it stops being scanned again), but nothing was actually
// recovered, so Closed must stay distinguishable from Recovered
// (dp-v2-019 d6, acceptance A2).
type Report struct{ Scanned, Recovered, Skipped, Closed int }

// executionIsTerminal reports whether an Execution has already reached an
// outcome that domain.MarkExecutionLost refuses to move away from. Shared by
// the lease-expiry pass (recoverOne) and the orphan sweep (orphan.go) so both
// use one definition of "nothing left to recover".
func executionIsTerminal(status domain.ExecutionStatus) bool {
	switch status {
	case domain.ExecutionSucceeded, domain.ExecutionFailed, domain.ExecutionTerminated, domain.ExecutionLost:
		return true
	}
	return false
}

func (r *Reconciler) Tick(ctx context.Context, cursor string) (Report, string, error) {
	if r == nil || r.Tx == nil || r.Clock == nil {
		return Report{}, cursor, errors.New("reconciler transaction and clock are required")
	}
	now := r.Clock.Now()
	if now.IsZero() {
		return Report{}, cursor, errors.New("reconciler clock returned zero time")
	}
	var leases []domain.Lease
	next := cursor
	if err := r.Tx.Transact(ctx, func(u application.UnitOfWork) error {
		var err error
		leases, next, err = u.ExpiredActiveLeases(ctx, now, cursor, MaxBatch)
		return err
	}); err != nil {
		return Report{}, cursor, err
	}
	report := Report{Scanned: len(leases)}
	for _, candidate := range leases {
		closed, err := r.recoverOne(ctx, candidate.ID.String(), now)
		if err != nil {
			if errors.Is(err, domain.ErrStaleVersion) || errors.Is(err, domain.ErrStaleFence) {
				report.Skipped++
				continue
			}
			// quota.ErrOverBudget (and any other unrecognised error) must
			// still abort the pass here: a hard Budget stop must not be
			// converted into a silently partial sweep (dp-v2-019 d6, A3).
			return report, next, err
		}
		if closed {
			report.Closed++
		} else {
			report.Recovered++
		}
	}
	return report, next, nil
}

// recoverOne expires one candidate Lease and, if its Execution is not
// already terminal, fences the Execution and returns its Increment to
// ready. If the Execution already reached a terminal state (for example
// ExecutionSucceeded reached before the Lease's TTL elapsed), the Lease is
// still closed so it stops recurring in every future pass, but the second
// return value reports true so the caller counts it as Closed rather than
// Recovered: domain.MarkExecutionLost correctly refuses to transition a
// terminal Execution, and previously that refusal (ErrInvalidTransition)
// propagated out of Tick and aborted the whole pass forever (dp-v2-019 d6).
func (r *Reconciler) recoverOne(ctx context.Context, leaseID string, now time.Time) (bool, error) {
	closed := false
	err := r.Tx.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.ReserveQuota(ctx, "reconcile:"+leaseID, now, quota.MutationUsage); err != nil {
			return err
		}
		lease, ok, err := u.Lease(ctx, leaseID)
		if err != nil {
			return err
		}
		if !ok || lease.Status != domain.LeaseActive || lease.ExpiresAt.After(now) {
			return domain.ErrStaleVersion
		}
		expired, err := domain.ExpireLease(lease, now)
		if err != nil {
			return err
		}
		exec, found, err := u.ExecutionByLease(ctx, leaseID)
		if err != nil {
			return err
		}
		terminal := false
		if found {
			if executionIsTerminal(exec.Status) {
				terminal = true
			} else {
				exec, err = domain.MarkExecutionLost(exec, lease)
				if err != nil {
					return err
				}
				if err = u.SaveExecution(ctx, exec, exec.Version-1); err != nil {
					return err
				}
				inc, exists, err := u.Increment(ctx, exec.IncrementID.String())
				if err != nil {
					return err
				}
				if exists {
					actor, _ := domain.NewActorID("reconciler")
					next, recoverErr := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementRecover, Actor: actor, At: now, ExpectedVersion: inc.Version})
					if recoverErr == nil {
						if err = u.SaveIncrement(ctx, next, inc.Version); err != nil {
							return err
						}
					}
				}
			}
		}
		if err = u.SaveLease(ctx, expired, lease.Version); err != nil {
			return err
		}
		eventType := "lease.expired.reconciled"
		if terminal {
			eventType = "lease.closed.terminal-execution"
		}
		eventID := fmt.Sprintf("reconcile-lease:%s:%d", leaseID, lease.Version)
		if err := u.Record(application.Event{ID: eventID, AggregateID: leaseID, Type: eventType, At: now}, nil); err != nil {
			return err
		}
		closed = terminal
		return nil
	})
	return closed, err
}
