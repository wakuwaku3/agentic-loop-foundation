package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type ProviderRequest struct{ OperationID, Prompt, Workspace string }
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
