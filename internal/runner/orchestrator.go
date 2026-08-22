package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

type Orchestrator struct {
	Service   *application.Service
	Provider  Provider
	Workspace *Workspace
	Journal   *Journal
	RunnerID  string
}
type JourneyRequest struct{ RequestID, Text string }
type JourneyResult struct {
	RequirementID, IncrementID, ExecutionID, LeaseID string
	Status                                           domain.ExecutionStatus
	Checkpoint                                       string
}

func (o *Orchestrator) RunFakeJourney(ctx context.Context, req JourneyRequest) (JourneyResult, error) {
	if o.Service == nil || o.Provider == nil || o.Workspace == nil || o.Journal == nil || o.RunnerID == "" {
		return JourneyResult{}, errors.New("orchestrator dependencies are incomplete")
	}
	owner := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "runner-local-owner"})
	runnerCtx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: o.RunnerID, RunnerID: o.RunnerID})
	cap, err := o.Service.Capture(owner, application.CaptureRequest{RequestID: req.RequestID + ":capture", Text: req.Text})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("capture: %w", err)
	}
	plan, err := o.Service.Plan(owner, application.PlanRequest{RequestID: req.RequestID + ":plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: cap.Version})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("plan: %w", err)
	}
	prepared, err := o.Service.Prepare(owner, application.PrepareRequest{RequestID: req.RequestID + ":prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("prepare: %w", err)
	}
	// Planning persists no runner binding; Claim binds the lease to this
	// runner. Keep the request target runner-neutral so result fencing agrees
	// with the canonical target on retries.
	target := domain.ControlTarget{RequirementID: mustRequirement(cap.RequirementID), IncrementID: mustIncrement(plan.IncrementID)}
	claim, err := o.Service.Claim(runnerCtx, application.ClaimRequest{RequestID: req.RequestID + ":claim", IncrementID: plan.IncrementID, ExpectedIncrementVersion: prepared.Version, Target: target})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("claim: %w", err)
	}
	if err := o.journal("assignment", req.RequestID, map[string]string{"execution_id": claim.ExecutionID, "lease_id": claim.LeaseID}); err != nil {
		return JourneyResult{}, fmt.Errorf("assignment journal: %w", err)
	}
	ws, err := o.Workspace.Create(claim.ExecutionID)
	if err != nil {
		return JourneyResult{}, fmt.Errorf("workspace: %w", err)
	}
	// ClaimResponse.Version is the increment version. A newly-created execution
	// always starts at version one; using the increment version here would make
	// the first start stale as soon as the increment has been claimed.
	start, err := o.Service.Start(runnerCtx, application.StartRequest{RequestID: req.RequestID + ":start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("start: %w", err)
	}
	permit, err := o.Service.Permit(runnerCtx, application.PermitRequest{RequestID: req.RequestID + ":permit", Kind: domain.PermitProcess, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("permit: %w", err)
	}
	if !permit.Allowed {
		return JourneyResult{}, fmt.Errorf("process permit denied: %s", permit.Reason)
	}
	result, err := o.Provider.Run(ctx, ProviderRequest{OperationID: claim.ExecutionID, Workspace: ws})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("provider: %w", err)
	}
	if err := o.journal("result_pending", req.RequestID, map[string]any{"execution_id": claim.ExecutionID, "succeeded": result.Succeeded}); err != nil {
		return JourneyResult{}, fmt.Errorf("result journal: %w", err)
	}
	if _, err := o.Service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: req.RequestID + ":checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
		return JourneyResult{}, fmt.Errorf("checkpoint: %w", err)
	}
	resultPermit, err := o.Service.Permit(runnerCtx, application.PermitRequest{RequestID: req.RequestID + ":result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("result permit: %w", err)
	}
	if !resultPermit.Allowed {
		return JourneyResult{}, fmt.Errorf("result permit denied: %s", resultPermit.Reason)
	}
	accepted, err := o.Service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: req.RequestID + ":accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: start.Version, FencingToken: claim.FencingToken, Succeeded: result.Succeeded, Target: target})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("accept: %w", err)
	}
	return JourneyResult{RequirementID: cap.RequirementID, IncrementID: plan.IncrementID, ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, Status: accepted.Status, Checkpoint: result.Checkpoint}, nil
}
func (o *Orchestrator) journal(kind, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	err = o.Journal.Append(JournalEvent{ID: id + ":" + kind, Kind: kind, Payload: b})
	if errors.Is(err, ErrJournalDuplicate) {
		// Replaying a recovered journey is intentionally idempotent. The
		// application operations have their own request-id fences as well.
		return nil
	}
	return err
}
func mustRequirement(v string) domain.RequirementID { x, _ := domain.NewRequirementID(v); return x }
func mustIncrement(v string) domain.IncrementID     { x, _ := domain.NewIncrementID(v); return x }

// RunProcess is the local-only process boundary. Callers must obtain an
// application Permit before invoking it; no shell/raw prompt is accepted.
func (o *Orchestrator) RunProcess(ctx context.Context, argv []string, grace time.Duration) error {
	if err := GuardCommand(argv, nil); err != nil {
		return err
	}
	return (ProcessSupervisor{TermGrace: grace}).Run(ctx, argv)
}
