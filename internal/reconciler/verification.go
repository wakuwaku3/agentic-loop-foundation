package reconciler

import (
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"time"
)

// VerifyControl is a pure, fail-closed decision for a control revision. A
// missing observation never becomes verified; ambiguous external effects take
// precedence over a successful-looking process report.
func VerifyControl(progress domain.ControlProgress, observations map[string]domain.RunnerObservation, pending, ambiguous bool, now, deadline time.Time) domain.VerificationState {
	if ambiguous {
		return domain.VerificationBlockedAmbiguous
	}
	if pending {
		return domain.VerificationPending
	}
	for _, target := range progress.Targets {
		observation, found := observations[target.Target.RunnerID.String()]
		if !found {
			if !deadline.IsZero() && !now.Before(deadline) {
				return domain.VerificationBlockedUnreachable
			}
			return domain.VerificationPending
		}
		if !observation.Reachable {
			if !deadline.IsZero() && !now.Before(deadline) {
				return domain.VerificationBlockedUnreachable
			}
			return domain.VerificationPending
		}
		if observation.AppliedRevision < progress.Revision {
			return domain.VerificationPending
		}
		terminated := false
		for _, process := range observation.Processes {
			if process.ProcessID == target.ExecutionID.String() && (process.State == "terminated" || process.State == "checkpointed") {
				terminated = true
				break
			}
		}
		if !terminated {
			return domain.VerificationPending
		}
	}
	return domain.VerificationVerified
}
