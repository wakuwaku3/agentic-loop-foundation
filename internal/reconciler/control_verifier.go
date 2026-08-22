package reconciler

import (
	"context"
	"errors"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

type VerificationReconciler struct {
	Tx       application.Transactor
	Clock    application.Clock
	Deadline time.Duration
}

func (r *VerificationReconciler) Tick(ctx context.Context) (int, error) {
	if r == nil || r.Tx == nil || r.Clock == nil {
		return 0, errors.New("verification reconciler is not configured")
	}
	now := r.Clock.Now()
	if now.IsZero() {
		return 0, errors.New("verification clock returned zero time")
	}
	deadline := r.Deadline
	if deadline <= 0 {
		deadline = time.Minute
	}
	var rows []domain.ControlProgress
	err := r.Tx.Transact(ctx, func(u application.UnitOfWork) error {
		var err error
		rows, err = u.PendingControlProgresses(ctx, 100)
		return err
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, candidate := range rows {
		err = r.Tx.Transact(ctx, func(u application.UnitOfWork) error {
			progress, found, err := u.ControlProgress(ctx, candidate.Revision)
			if err != nil || !found || progress.Verification != domain.VerificationPending {
				return err
			}
			observations := make(map[string]domain.RunnerObservation, len(progress.Targets))
			ambiguous := false
			pending := false
			for _, snapshot := range progress.Targets {
				lease, found, e := u.Lease(ctx, snapshot.LeaseID.String())
				if e != nil {
					return e
				}
				if found && lease.Status == domain.LeaseActive {
					pending = true
				}
				observation, found, e := u.RunnerObservation(ctx, snapshot.Target.RunnerID.String())
				if e != nil {
					return e
				}
				if found {
					observations[snapshot.Target.RunnerID.String()] = observation
				}
				resolution, e := u.OutboxResolution(ctx, snapshot.LeaseID.String())
				if e != nil {
					return e
				}
				if resolution.Ambiguous {
					ambiguous = true
				}
				if resolution.Pending {
					pending = true
				}
			}
			state := VerifyControl(progress, observations, pending, ambiguous, now, progress.EffectiveAt.Add(deadline))
			if state == progress.Verification {
				return nil
			}
			next := progress
			next.Verification = state
			if state == domain.VerificationVerified {
				next.State = domain.ControlVerified
				next.VerifiedAt = now
			} else if state == domain.VerificationBlockedUnreachable || state == domain.VerificationBlockedAmbiguous {
				next.State = domain.ControlEffective
			}
			if err := u.SaveControlProgress(ctx, next, progress.State); err != nil {
				return err
			}
			count++
			return nil
		})
		if err != nil {
			return count, err
		}
	}
	return count, err
}
