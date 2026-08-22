package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ControlLoop is the local runner-side control boundary. Journal append is
// fsync-backed; callbacks are supplied by the daemon and never contain raw
// prompts or credentials.
type ControlLoop struct {
	Journal     *Journal
	Checkpoint  func(context.Context) error
	StopProcess func(context.Context) error
}

func (l *ControlLoop) Apply(ctx context.Context, intent domain.ControlIntent) error {
	if l == nil || l.Journal == nil {
		return errors.New("control loop journal is required")
	}
	b, err := json.Marshal(map[string]any{"revision": intent.Revision, "mode": intent.Mode, "effective_at": intent.EffectiveAt})
	if err != nil {
		return err
	}
	if err = l.Journal.Append(JournalEvent{ID: fmt.Sprintf("control:%d:received", intent.Revision), Kind: "control_received", Payload: b}); err != nil && !errors.Is(err, ErrJournalDuplicate) {
		return err
	}
	switch intent.Mode {
	case domain.ControlGracefulStop:
		if l.Checkpoint == nil {
			return errors.New("graceful stop checkpoint is not configured")
		}
		if err := l.Checkpoint(ctx); err != nil {
			return err
		}
		return l.observe(intent, "checkpointed")
	case domain.ControlImmediateStop, domain.ControlEmergencyStop, domain.ControlCancel:
		if l.StopProcess == nil {
			return errors.New("immediate stop process controller is not configured")
		}
		if err := l.StopProcess(ctx); err != nil {
			return l.observe(intent, "unknown")
		}
		return l.observe(intent, "terminated")
	default:
		return l.observe(intent, "received")
	}
}

func (l *ControlLoop) observe(intent domain.ControlIntent, state string) error {
	b, err := json.Marshal(map[string]any{"revision": intent.Revision, "state": state})
	if err != nil {
		return err
	}
	err = l.Journal.Append(JournalEvent{ID: fmt.Sprintf("control:%d:observation", intent.Revision), Kind: "control_observation", Payload: b})
	if errors.Is(err, ErrJournalDuplicate) {
		return nil
	}
	return err
}
