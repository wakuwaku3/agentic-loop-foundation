package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ControlAgent is the runner-side control-read boundary that closes the
// observation loop (dp-v2-019 d4, acceptance A10). It turns
// (LatestRevision, LatestMode) from a LifecycleResponse into a
// domain.ControlIntent whenever LatestRevision exceeds the revision this
// agent has already applied, and applies that intent through ControlLoop --
// which is the same real, previously-unwired StopProcess/Checkpoint path
// acceptance A9 proves actually kills or checkpoints a process. It is driven
// only by explicit Tick calls (normally from LeaseKeeper.Tick); it never
// starts a background goroutine or timer of its own. Never calling Tick at
// all -- or never wiring an Agent into a LeaseKeeper -- is the negative
// control that proves observation, not inference, is what advances
// verification: the Control Plane's verification reconciler can only leave
// VerificationPending for a Runner nothing has ever reported on.
type ControlAgent struct {
	Loop *ControlLoop

	applied domain.Revision
}

// knownControlMode reports whether mode is one of the seven modes the
// domain understands. An empty or unrecognised mode is never silently
// treated as domain.ControlAllow: acceptance A10 requires a negative,
// fail-closed test for exactly this case, per the Work Order's recorded
// fallback ("the Runner treats any unapplied revision as a fail-closed
// stop").
func knownControlMode(mode domain.ControlMode) bool {
	switch mode {
	case domain.ControlAllow, domain.ControlPauseIntake, domain.ControlPauseClaim, domain.ControlGracefulStop, domain.ControlImmediateStop, domain.ControlEmergencyStop, domain.ControlCancel:
		return true
	}
	return false
}

// Tick applies resp's control revision through ControlLoop when
// resp.LatestRevision exceeds the revision this agent has already applied.
// A LatestRevision that does not exceed what is already applied is a no-op
// (applied=false): the agent never re-applies the same or an older
// revision, and never resurrects a revision Renew has already told the
// Runner is denied. When applied, state is the process observation
// ControlLoop actually recorded ("terminated", "checkpointed", "unknown" or
// "received") so the caller (LeaseKeeper) can report it back on the next
// Heartbeat; resp.LatestEffectiveAt (not the Runner's own clock) is used as
// the resulting domain.ControlIntent's EffectiveAt.
func (a *ControlAgent) Tick(ctx context.Context, resp application.LifecycleResponse) (state string, applied bool, err error) {
	if a == nil || a.Loop == nil {
		return "", false, errors.New("control agent loop is required")
	}
	if resp.LatestRevision == 0 || resp.LatestRevision <= a.applied {
		return "", false, nil
	}
	mode := resp.LatestMode
	if !knownControlMode(mode) {
		// Fail-closed: an unapplied revision whose mode is missing or not
		// recognised must stop the Runner's work, not keep it running on a
		// guess about what the Control Plane actually asked for.
		mode = domain.ControlImmediateStop
	}
	intent := domain.ControlIntent{Mode: mode, Revision: resp.LatestRevision, EffectiveAt: resp.LatestEffectiveAt}
	if err := a.Loop.Apply(ctx, intent); err != nil {
		return "", false, err
	}
	a.applied = resp.LatestRevision
	state, err = a.lastObservedState(intent.Revision)
	if err != nil {
		return "", true, err
	}
	return state, true, nil
}

// Applied reports the last revision this agent has applied.
func (a *ControlAgent) Applied() domain.Revision {
	if a == nil {
		return 0
	}
	return a.applied
}

// lastObservedState re-reads ControlLoop's own durable journal for the
// control_observation event ControlLoop.Apply just wrote for revision,
// rather than re-deriving the state from the applied mode. Using the
// journal as the source of truth means a StopProcess failure (which
// ControlLoop.observe records as "unknown", not "terminated") is reported
// faithfully instead of optimistically.
func (a *ControlAgent) lastObservedState(revision domain.Revision) (string, error) {
	events, err := a.Loop.Journal.Replay()
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "control_observation" {
			continue
		}
		var payload struct {
			Revision domain.Revision `json:"revision"`
			State    string          `json:"state"`
		}
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Revision == revision {
			return payload.State, nil
		}
	}
	return "", nil
}
