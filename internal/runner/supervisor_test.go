package runner

import (
	"context"
	"testing"
	"time"
)

func TestSupervisorCancelsProcessGroupTERMThenKILL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := (ProcessSupervisor{TermGrace: 50 * time.Millisecond}).Run(ctx, []string{"sh", "-c", "trap '' TERM; sleep 10"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}
