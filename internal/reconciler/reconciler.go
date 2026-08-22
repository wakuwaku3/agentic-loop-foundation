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

type Report struct{ Scanned, Recovered, Skipped int }

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
		if err := r.recoverOne(ctx, candidate.ID.String(), now); err != nil {
			if errors.Is(err, domain.ErrStaleVersion) || errors.Is(err, domain.ErrStaleFence) {
				report.Skipped++
				continue
			}
			return report, next, err
		}
		report.Recovered++
	}
	return report, next, nil
}

func (r *Reconciler) recoverOne(ctx context.Context, leaseID string, now time.Time) error {
	return r.Tx.Transact(ctx, func(u application.UnitOfWork) error {
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
		if found {
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
		if err = u.SaveLease(ctx, expired, lease.Version); err != nil {
			return err
		}
		eventID := fmt.Sprintf("reconcile-lease:%s:%d", leaseID, lease.Version)
		return u.Record(application.Event{ID: eventID, AggregateID: leaseID, Type: "lease.expired.reconciled", At: now}, nil)
	})
}
