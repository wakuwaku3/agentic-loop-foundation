package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// ProviderRequest is the runner's provider-neutral invocation boundary. It
// carries a provider.WorkPacket -- facts and evidence only -- and never a raw
// prompt: no Prompt field exists on this type, so a raw prompt is structurally
// unrepresentable rather than merely asserted absent.
type ProviderRequest struct {
	OperationID string
	Packet      provider.WorkPacket
	Workspace   string
}
type ProviderResult struct {
	Output     string
	Succeeded  bool
	Checkpoint string
}
type Provider interface {
	Run(context.Context, ProviderRequest) (ProviderResult, error)
}

// CodexSyntheticAdapter models the provider boundary without starting Codex
// or reading credentials. Production wiring must replace it explicitly.
type CodexSyntheticAdapter struct{}

func (CodexSyntheticAdapter) Run(ctx context.Context, req ProviderRequest) (ProviderResult, error) {
	select {
	case <-ctx.Done():
		return ProviderResult{}, ctx.Err()
	default:
	}
	if req.OperationID == "" || req.Workspace == "" {
		return ProviderResult{}, errors.New("provider request is incomplete")
	}
	return ProviderResult{Output: fmt.Sprintf("synthetic codex completed %s", req.OperationID), Succeeded: true, Checkpoint: "synthetic:" + req.OperationID}, nil
}

type FakeProvider struct {
	mu     sync.Mutex
	Calls  []ProviderRequest
	Result ProviderResult
	Err    error
}

func (f *FakeProvider) Run(ctx context.Context, req ProviderRequest) (ProviderResult, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, req)
	result, err := f.Result, f.Err
	f.mu.Unlock()
	if err != nil {
		return ProviderResult{}, err
	}
	select {
	case <-ctx.Done():
		return ProviderResult{}, ctx.Err()
	default:
		return result, nil
	}
}

// InvocationRunner is the seam between a built provider.Invocation and the
// bytes an adapter can parse. It never itself decides success/failure; it
// only produces (or fails to produce) fixture bytes for Adapter.Parse.
type InvocationRunner interface {
	Run(ctx context.Context, inv provider.Invocation) ([]byte, error)
}

// FakeInvocationRunner never starts a process. It records every Invocation it
// is given (including its Environment, so a Secret Broker grant merged onto
// the Invocation is observable by a test as the positive control for A11) and
// returns a fixed fixture.
type FakeInvocationRunner struct {
	mu      sync.Mutex
	Calls   []provider.Invocation
	Fixture []byte
	Err     error
}

func (f *FakeInvocationRunner) Run(_ context.Context, inv provider.Invocation) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, inv)
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Fixture, nil
}

// CallCount reports how many times Run has been invoked so far.
func (f *FakeInvocationRunner) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// SupervisedInvocationRunner is the seam a real Provider CLI fills: it would
// run a real Invocation through ProcessSupervisor and the bounded diagnostic
// log. V2-016 wires the type but not a real executable; V2-017 fills it.
type SupervisedInvocationRunner struct {
	Supervisor ProcessSupervisor
	Log        *BoundedLog
}

func (SupervisedInvocationRunner) Run(context.Context, provider.Invocation) ([]byte, error) {
	return nil, errors.New("supervised invocation execution is not wired until V2-017")
}

// ProviderClient composes a provider.Adapter (Build/Parse) with an
// InvocationRunner seam. It is the local Provider implementation that carries
// a real, validated Work Packet through Build and Parse without starting a
// real Provider CLI in this task.
type ProviderClient struct {
	Adapter provider.Adapter
	Runner  InvocationRunner
	// Grant, if non-nil, is merged into the built Invocation's Environment
	// immediately before the seam runs it. Nothing else in this package
	// (the journal, the Work Packet, the bounded log, the canonical store)
	// ever observes it.
	Grant *Grant
}

func (c ProviderClient) Run(ctx context.Context, req ProviderRequest) (ProviderResult, error) {
	if c.Adapter == nil || c.Runner == nil {
		return ProviderResult{}, errors.New("provider client dependencies are incomplete")
	}
	if err := req.Packet.Validate(); err != nil {
		return ProviderResult{}, fmt.Errorf("work packet: %w", err)
	}
	inv, err := c.Adapter.Build(provider.Request{OperationID: req.OperationID, Workspace: req.Workspace, Packet: req.Packet})
	if err != nil {
		return ProviderResult{}, err
	}
	if c.Grant != nil {
		inv = c.Grant.Apply(inv)
	}
	raw, err := c.Runner.Run(ctx, inv)
	if err != nil {
		return ProviderResult{}, err
	}
	res, err := c.Adapter.Parse(raw)
	if err != nil {
		return ProviderResult{}, err
	}
	out := ProviderResult{Succeeded: res.Succeeded, Checkpoint: res.Checkpoint, Output: res.OutputDigest}
	if !res.Succeeded {
		msg := "provider run did not succeed"
		if res.Failure != nil && res.Failure.Message != "" {
			msg = res.Failure.Message
		}
		return out, errors.New(msg)
	}
	return out, nil
}
