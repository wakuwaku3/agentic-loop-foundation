package runner

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

type journeyClock struct{}

func (journeyClock) Now() time.Time { return time.Unix(1700000000, 0).UTC() }

type journeyIDs struct {
	mu sync.Mutex
	n  int
}

func (g *journeyIDs) Next(kind string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s-%d", kind, g.n), nil
}

func TestOrchestratorFakeJourney(t *testing.T) {
	st := memory.New()
	service, err := application.NewServiceWithConfig(st, journeyClock{}, &journeyIDs{}, application.ServiceConfig{
		InstallationID: "install",
		LeaseTTL:       time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &FakeProvider{Result: ProviderResult{Succeeded: true, Checkpoint: "checkpoint-1"}}
	workspaceRoot := t.TempDir()
	if err := os.Chmod(workspaceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{Service: service, Provider: provider, Workspace: workspace, Journal: journal, RunnerID: "runner-1"}

	result, err := o.RunFakeJourney(context.Background(), JourneyRequest{RequestID: "journey-1", Text: "build the fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.ExecutionSucceeded || result.Checkpoint != "checkpoint-1" {
		t.Fatalf("unexpected journey result: %#v", result)
	}
	if _, err := o.RunFakeJourney(context.Background(), JourneyRequest{RequestID: "journey-1", Text: "build the fixture"}); err != nil {
		t.Fatalf("retry journey: %v", err)
	}
	if len(provider.Calls) != 1 {
		t.Fatalf("provider boundary called unexpectedly: %#v", provider.Calls)
	}
	// ProviderRequest no longer has a Prompt field at all (a raw prompt is
	// structurally unrepresentable); it carries a validated Work Packet
	// instead. This replaces the old Calls[0].Prompt == "" assertion.
	if err := provider.Calls[0].Packet.Validate(); err != nil {
		t.Fatalf("provider boundary carried an invalid work packet instead of a raw prompt: %v", err)
	}
	events, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Kind != "assignment" || events[1].Kind != "result_pending" || events[2].Kind != "result_accepted" {
		t.Fatalf("unexpected journal: %#v", events)
	}
}
