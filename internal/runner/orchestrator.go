package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

type Orchestrator struct {
	Service   *application.Service
	Provider  Provider
	Workspace *Workspace
	Journal   *Journal
	RunnerID  string
	// Caller is the identity the owner-side half of the journey runs as, and it
	// is REQUIRED and INJECTED. Before V2-086 this component fabricated it --
	// application.Caller{Role: application.RoleOwner, Subject:
	// "runner-local-owner"} -- so a Runner asserted the owner's role to get
	// through a role gate. Relabelling that literal RoleScheduler would have
	// been the same self-naming one word over, and DEFAULTING this field would
	// be the fabrication again, so it has no default: RunFakeJourney refuses
	// when its Role or its Subject is empty, in the same style as the existing
	// dependency guard. Whoever constructs the Orchestrator supplies the
	// identity; measured at V2-086, that is exactly one site and it is a test.
	Caller application.Caller
	// Hooks are test-only/local guards immediately before external effects.
	// Production wiring leaves them nil and relies on the application permit.
	BeforeProvider func(context.Context, domain.ControlTarget) error
	BeforeAccept   func(context.Context, domain.ControlTarget) error
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
	if o.Caller.Role == "" || o.Caller.Subject == "" {
		return JourneyResult{}, errors.New("orchestrator caller identity is incomplete")
	}
	callerCtx := application.ContextWithCaller(ctx, o.Caller)
	runnerCtx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: o.RunnerID, RunnerID: o.RunnerID})
	cap, err := o.Service.Capture(callerCtx, application.CaptureRequest{RequestID: req.RequestID + ":capture", Text: req.Text})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("capture: %w", err)
	}
	plan, err := o.Service.Plan(callerCtx, application.PlanRequest{RequestID: req.RequestID + ":plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: cap.Version})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("plan: %w", err)
	}
	prepared, err := o.Service.Prepare(callerCtx, application.PrepareRequest{RequestID: req.RequestID + ":prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
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
	result, pending, err := o.pendingResult(req.RequestID)
	if err != nil {
		return JourneyResult{}, fmt.Errorf("pending journal: %w", err)
	}
	if !pending {
		if o.BeforeProvider != nil {
			if err := o.BeforeProvider(ctx, target); err != nil {
				return JourneyResult{}, fmt.Errorf("provider guard: %w", err)
			}
		}
		packet := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: cap.RequirementID, RequirementSummary: req.Text, IncrementID: plan.IncrementID}
		result, err = o.Provider.Run(ctx, ProviderRequest{OperationID: claim.ExecutionID, Packet: packet, Workspace: ws})
		if err != nil {
			return JourneyResult{}, fmt.Errorf("provider: %w", err)
		}
		if _, err := o.Service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: req.RequestID + ":checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
			return JourneyResult{}, fmt.Errorf("checkpoint: %w", err)
		}
		if err := o.journal("result_pending", req.RequestID, map[string]any{"execution_id": claim.ExecutionID, "succeeded": result.Succeeded, "checkpoint": result.Checkpoint}); err != nil {
			return JourneyResult{}, fmt.Errorf("result journal: %w", err)
		}
	}
	resultPermit, err := o.Service.Permit(runnerCtx, application.PermitRequest{RequestID: req.RequestID + ":result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("result permit: %w", err)
	}
	if !resultPermit.Allowed {
		return JourneyResult{}, fmt.Errorf("result permit denied: %s", resultPermit.Reason)
	}
	if o.BeforeAccept != nil {
		if err := o.BeforeAccept(ctx, target); err != nil {
			return JourneyResult{}, fmt.Errorf("accept guard: %w", err)
		}
	}
	accepted, err := o.Service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: req.RequestID + ":accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: start.Version, FencingToken: claim.FencingToken, Succeeded: result.Succeeded, Target: target})
	if err != nil {
		return JourneyResult{}, fmt.Errorf("accept: %w", err)
	}
	if err := o.journal("result_accepted", req.RequestID, map[string]any{"execution_id": claim.ExecutionID, "status": accepted.Status}); err != nil {
		return JourneyResult{}, fmt.Errorf("accepted journal: %w", err)
	}
	return JourneyResult{RequirementID: cap.RequirementID, IncrementID: plan.IncrementID, ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, Status: accepted.Status, Checkpoint: result.Checkpoint}, nil
}

func (o *Orchestrator) pendingResult(id string) (ProviderResult, bool, error) {
	events, err := o.Journal.Replay()
	if err != nil {
		return ProviderResult{}, false, err
	}
	for _, event := range events {
		if event.ID != id+":result_pending" {
			continue
		}
		var p struct {
			Succeeded  bool   `json:"succeeded"`
			Checkpoint string `json:"checkpoint"`
		}
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return ProviderResult{}, false, ErrJournalCorrupt
		}
		return ProviderResult{Succeeded: p.Succeeded, Checkpoint: p.Checkpoint}, true, nil
	}
	return ProviderResult{}, false, nil
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

// WorkPacketDigest returns a stable content digest of a Work Packet. It binds
// a journaled provider event to the exact Work Packet that produced it
// (A5/A3), so a resuming Execution can tell whether a recovered checkpoint
// was produced for a Work Packet identical to the one it would issue itself.
func WorkPacketDigest(p provider.WorkPacket) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// PendingProviderRecord is the journal payload for a provider result that has
// been produced but not yet canonically accepted. It is keyed for recovery
// by IncrementID (the stable identity that survives a crash across
// Executions), not by the per-attempt canonical request id, per dp-v2-016 d5.
type PendingProviderRecord struct {
	IncrementID      string `json:"increment_id"`
	ExecutionID      string `json:"execution_id"`
	WorkPacketDigest string `json:"work_packet_digest"`
	RawResult        []byte `json:"raw_result,omitempty"`
	Succeeded        bool   `json:"succeeded"`
	Checkpoint       string `json:"checkpoint"`
}

// JournalProviderPending records a provider result pending canonical
// acceptance. The event id includes executionID so two different Executions
// (e.g. a crashed attempt and the Execution that resumes it) never collide in
// the journal's own per-id idempotency; FindPendingProviderResult recovers
// by IncrementID so a resuming Execution can find a prior attempt's dangling
// record even though its own ExecutionID is new.
func JournalProviderPending(j *Journal, rec PendingProviderRecord) error {
	if rec.IncrementID == "" || rec.ExecutionID == "" {
		return errors.New("pending provider record requires increment and execution ids")
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	err = j.Append(JournalEvent{ID: rec.ExecutionID + ":result_pending", Kind: "result_pending", Payload: payload})
	if errors.Is(err, ErrJournalDuplicate) {
		return nil
	}
	return err
}

// FindPendingProviderResult returns the most recent result_pending event for
// incrementID that has no corresponding result_accepted event for the same
// ExecutionID recorded after it -- i.e. a provider result a crashed
// Execution produced (or was about to produce) but never got to canonically
// accept.
func FindPendingProviderResult(j *Journal, incrementID string) (PendingProviderRecord, bool, error) {
	events, err := j.Replay()
	if err != nil {
		return PendingProviderRecord{}, false, err
	}
	accepted := map[string]bool{}
	for _, e := range events {
		if e.Kind != "result_accepted" {
			continue
		}
		var a struct {
			ExecutionID string `json:"execution_id"`
		}
		if jsonErr := json.Unmarshal(e.Payload, &a); jsonErr != nil {
			return PendingProviderRecord{}, false, ErrJournalCorrupt
		}
		accepted[a.ExecutionID] = true
	}
	var found PendingProviderRecord
	ok := false
	for _, e := range events {
		if e.Kind != "result_pending" {
			continue
		}
		var rec PendingProviderRecord
		if jsonErr := json.Unmarshal(e.Payload, &rec); jsonErr != nil {
			return PendingProviderRecord{}, false, ErrJournalCorrupt
		}
		if rec.IncrementID != incrementID || accepted[rec.ExecutionID] {
			continue
		}
		found = rec
		ok = true
	}
	return found, ok, nil
}

// JournalResultAccepted records that executionID's result was canonically
// accepted, so FindPendingProviderResult stops treating its result_pending
// event as dangling.
func JournalResultAccepted(j *Journal, executionID string, status domain.ExecutionStatus) error {
	payload, err := json.Marshal(map[string]any{"execution_id": executionID, "status": status})
	if err != nil {
		return err
	}
	err = j.Append(JournalEvent{ID: executionID + ":result_accepted", Kind: "result_accepted", Payload: payload})
	if errors.Is(err, ErrJournalDuplicate) {
		return nil
	}
	return err
}

// RunProcess is the local-only process boundary. Callers must obtain an
// application Permit before invoking it; no shell/raw prompt is accepted.
func (o *Orchestrator) RunProcess(ctx context.Context, argv []string, grace time.Duration) error {
	if err := GuardCommand(argv, nil); err != nil {
		return err
	}
	return (ProcessSupervisor{TermGrace: grace}).Run(ctx, argv)
}
