package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

func TestControlLoopFsyncsReceiptAndObservation(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkpointed := false
	l := &ControlLoop{Journal: j, Checkpoint: func(context.Context) error { checkpointed = true; return nil }}
	if err := l.Apply(context.Background(), domain.ControlIntent{Revision: 1, Mode: domain.ControlGracefulStop}); err != nil {
		t.Fatal(err)
	}
	if !checkpointed {
		t.Fatal("checkpoint was not requested")
	}
	events, err := j.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "control_received" || events[1].Kind != "control_observation" {
		t.Fatalf("events=%#v", events)
	}
}

func TestControlLoopImmediateStopUnknownObservation(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &ControlLoop{Journal: j, StopProcess: func(context.Context) error { return errors.New("child did not confirm exit") }}
	if err := l.Apply(context.Background(), domain.ControlIntent{Revision: 2, Mode: domain.ControlImmediateStop}); err != nil {
		t.Fatal(err)
	}
	events, err := j.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%#v", events)
	}
}
