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

// OrphanSweep finds a non-terminal Execution whose bound Lease is absent,
// expired or revoked, and drives it to a terminal state (ExecutionLost) with
// the current fencing token retained. It closes the reconciliation gap that
// Reconciler.Tick cannot see: ExpiredActiveLeases filters lease_status ==
// active, so once a Lease has already moved past LeaseActive (expired,
// revoked, or never saved at all) the Execution bound to it is invisible to
// the lease-keyed pass forever (dp-v2-019 d5/d7, acceptance A4/A5).
//
// It adds no repository port and no store change: it composes only ports
// that already exist on application.UnitOfWork --
// RequirementsPage(afterID, limit), IncrementsForRequirements(ids),
// ExecutionsForIncrements(ids) and Lease(id) -- exactly as dp-v2-019 d5
// requires. The scan is cursor-paged over Requirements at MaxBatch per
// pass, mirroring Reconciler.Tick's own bound.
type OrphanSweep struct {
	Tx    application.Transactor
	Clock application.Clock
}

// staleLeaseStatus reports whether a found Lease can no longer be renewed or
// relied upon to still be protecting its Execution.
func staleLeaseStatus(status domain.LeaseStatus) bool {
	return status == domain.LeaseExpired || status == domain.LeaseRevoked
}

// Tick scans up to MaxBatch Requirements after cursor, and for every
// non-terminal Execution reachable from them whose bound Lease is absent,
// expired or revoked, closes it. It returns the cursor to resume the
// Requirements scan; an empty cursor means the scan reached the end.
func (o *OrphanSweep) Tick(ctx context.Context, cursor string) (Report, string, error) {
	if o == nil || o.Tx == nil || o.Clock == nil {
		return Report{}, cursor, errors.New("orphan sweep transaction and clock are required")
	}
	now := o.Clock.Now()
	if now.IsZero() {
		return Report{}, cursor, errors.New("orphan sweep clock returned zero time")
	}
	var executions []domain.Execution
	next := cursor
	if err := o.Tx.Transact(ctx, func(u application.UnitOfWork) error {
		requirements, more, err := u.RequirementsPage(ctx, cursor, MaxBatch)
		if err != nil {
			return err
		}
		if more && len(requirements) > 0 {
			next = requirements[len(requirements)-1].ID.String()
		} else {
			next = ""
		}
		ids := make([]string, len(requirements))
		for i, r := range requirements {
			ids[i] = r.ID.String()
		}
		increments, err := u.IncrementsForRequirements(ctx, ids)
		if err != nil {
			return err
		}
		incIDs := make([]string, len(increments))
		for i, inc := range increments {
			incIDs[i] = inc.ID.String()
		}
		executions, err = u.ExecutionsForIncrements(ctx, incIDs)
		return err
	}); err != nil {
		return Report{}, cursor, err
	}
	report := Report{Scanned: len(executions)}
	for _, exec := range executions {
		if executionIsTerminal(exec.Status) {
			continue
		}
		recovered, err := o.recoverOrphan(ctx, exec.ID.String(), now)
		if err != nil {
			if errors.Is(err, domain.ErrStaleVersion) || errors.Is(err, domain.ErrStaleFence) {
				report.Skipped++
				continue
			}
			return report, next, err
		}
		if recovered {
			report.Recovered++
		}
	}
	return report, next, nil
}

// recoverOrphan re-reads one Execution and its bound Lease inside a fresh
// transaction (never trusting the scan's read-only snapshot) and, if the
// Lease is absent, expired or revoked, marks the Execution lost and returns
// its Increment to ready. If the Lease is actually still active, the
// candidate is stale by the time recoverOrphan runs and is reported as
// nothing to do (recovered=false, err=nil), not as a stale error, because
// the scan itself never claimed the candidate was still orphaned.
func (o *OrphanSweep) recoverOrphan(ctx context.Context, executionID string, now time.Time) (bool, error) {
	recovered := false
	err := o.Tx.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.ReserveQuota(ctx, "orphan-sweep:"+executionID, now, quota.MutationUsage); err != nil {
			return err
		}
		exec, found, err := u.Execution(ctx, executionID)
		if err != nil {
			return err
		}
		if !found || executionIsTerminal(exec.Status) {
			return nil
		}
		lease, leaseFound, err := u.Lease(ctx, exec.LeaseID.String())
		if err != nil {
			return err
		}
		if leaseFound && !staleLeaseStatus(lease.Status) {
			// The Lease is still active (or in some other non-stale
			// status): this Execution is not actually orphaned.
			return nil
		}
		reference := lease
		if !leaseFound {
			// No Lease record exists at all for this Execution's LeaseID.
			// domain.MarkExecutionLost only checks that the Execution and
			// the Lease it is given agree with each other, so a reference
			// value carrying exactly the Execution's own linkage fields
			// satisfies that check without asserting anything false about
			// a Lease that was never actually issued.
			reference = domain.Lease{ID: exec.LeaseID, ExecutionID: exec.ID, FencingToken: exec.FencingToken}
		}
		next, err := domain.MarkExecutionLost(exec, reference)
		if err != nil {
			return err
		}
		if err = u.SaveExecution(ctx, next, exec.Version); err != nil {
			return err
		}
		inc, exists, err := u.Increment(ctx, next.IncrementID.String())
		if err != nil {
			return err
		}
		if exists {
			actor, _ := domain.NewActorID("reconciler")
			incNext, recoverErr := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementRecover, Actor: actor, At: now, ExpectedVersion: inc.Version})
			if recoverErr == nil {
				if err = u.SaveIncrement(ctx, incNext, inc.Version); err != nil {
					return err
				}
			}
		}
		eventID := fmt.Sprintf("orphan-sweep:%s:%d", executionID, next.Version)
		if err := u.Record(application.Event{ID: eventID, AggregateType: "execution", AggregateID: executionID, Type: "execution.orphan.reconciled", At: now}, nil); err != nil {
			return err
		}
		recovered = true
		return nil
	})
	return recovered, err
}
