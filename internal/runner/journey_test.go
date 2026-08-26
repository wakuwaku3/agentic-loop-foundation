package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// TestJourneyOneLocalIssueIntakeToIncrementArtifact is Journey 1's local
// segment (dp-v2-016 d3): issue intake through to an accepted Increment
// Artifact, fully in-process, with an injected clock and id generator, no
// sleep and no goroutine timer. It has its own t.TempDir() fixtures and its
// own context deadline, and shares no package-level mutable state with any
// other test.
func TestJourneyOneLocalIssueIntakeToIncrementArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	clock := newMutableClock(time.Unix(1_700_200_000, 0).UTC())

	// 1-3: issue an enrollment, complete the challenge, verify the session,
	// and use the resulting runner id as the claim caller identity.
	enrollmentStore := NewMemoryStore()
	enrollmentService, err := NewService(enrollmentStore)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollToken, err := enrollmentService.IssueEnrollment(ctx, "runner-journey-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := enrollmentService.Begin(ctx, enrollToken, pub)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(priv, ChallengeMessage(challenge))
	session, err := enrollmentService.Complete(ctx, challenge.ID, base64.RawURLEncoding.EncodeToString(signature))
	if err != nil {
		t.Fatal(err)
	}
	runnerID, err := enrollmentService.VerifySession(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if runnerID != "runner-journey-1" {
		t.Fatalf("unexpected runner id from session: %s", runnerID)
	}

	// Canonical application boundary, over the in-memory store, with an
	// injected clock and injected id generator (journeyIDs, shared with
	// orchestrator_test.go).
	st := memory.New()
	ids := &journeyIDs{}
	service, err := application.NewServiceWithConfig(st, clock, ids, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	owner := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "owner"})
	runnerCtx := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: runnerID, RunnerID: runnerID})

	// 4: capture, plan, prepare.
	capResp, err := service.Capture(owner, application.CaptureRequest{RequestID: "journey1:capture", Text: "ship the local runner closure"})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// V2-089: this fixture claims, so its parent Requirement is moved to
	// domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- before the Plan, and the Plan
	// carries the POST-seed version.
	readyVersion := runnerSeedRequirementStatus(t, st, owner, capResp.RequirementID, domain.RequirementReady)
	planResp, err := service.Plan(owner, application.PlanRequest{RequestID: "journey1:plan", RequirementID: capResp.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	preparedResp, err := service.Prepare(owner, application.PrepareRequest{RequestID: "journey1:prepare", IncrementID: planResp.IncrementID, ExpectedVersion: planResp.Version})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	target := domain.ControlTarget{RequirementID: mustRequirement(capResp.RequirementID), IncrementID: mustIncrement(planResp.IncrementID)}

	// 5: claim.
	claim, err := service.Claim(runnerCtx, application.ClaimRequest{RequestID: "journey1:attempt-1:claim", IncrementID: planResp.IncrementID, ExpectedIncrementVersion: preparedResp.Version, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// 6: create the workspace.
	workspaceRoot := t.TempDir()
	if err := os.Chmod(workspaceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}

	// 7: start.
	startResp, err := service.Start(runnerCtx, application.StartRequest{RequestID: "journey1:attempt-1:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// 8: tick the lease keeper across more than one LeaseTTL (1 minute here),
	// via explicit ticks over the injected clock -- never a background timer
	// -- so a provider run outliving the TTL still keeps its lease.
	keeper := &LeaseKeeper{Service: service, LeaseID: claim.LeaseID, RequestBase: "journey1:attempt-1"}
	leaseVersion := domain.Version(1)
	var lastExpiry time.Time
	for i := 0; i < 4; i++ {
		clock.Advance(20 * time.Second) // 4 * 20s = 80s > the 60s LeaseTTL.
		out, err := keeper.Tick(runnerCtx, leaseVersion, claim.FencingToken)
		if err != nil {
			t.Fatalf("lease keeper tick %d: %v", i, err)
		}
		if !out.Renewed {
			t.Fatalf("lease keeper tick %d: lease was not renewed", i)
		}
		if out.RenewResponse.ExpiresAt.Equal(lastExpiry) {
			t.Fatalf("lease keeper tick %d: renew did not extend the lease", i)
		}
		lastExpiry = out.RenewResponse.ExpiresAt
		leaseVersion = out.RenewResponse.Version
		if !out.HeartbeatResult.Accepted {
			t.Fatalf("lease keeper tick %d: heartbeat not accepted", i)
		}
	}

	// 9: obtain the process permit.
	processPermit, err := service.Permit(runnerCtx, application.PermitRequest{RequestID: "journey1:attempt-1:permit", Kind: domain.PermitProcess, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil {
		t.Fatalf("process permit: %v", err)
	}
	if !processPermit.Allowed {
		t.Fatalf("process permit denied: %s", processPermit.Reason)
	}

	// 10: build the Work Packet.
	packet := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: capResp.RequirementID, RequirementSummary: "ship the local runner closure", IncrementID: planResp.IncrementID}
	if err := packet.Validate(); err != nil {
		t.Fatalf("work packet failed to validate: %v", err)
	}

	// 11: run the fake Invocation (real CodexAdapter.Build/Parse, fixture
	// bytes only -- no real Provider CLI, no quota consumed).
	invocationRunner := &FakeInvocationRunner{Fixture: []byte(`{"status":"success","checkpoint":"checkpoint-journey-1","output":"increment artifact produced"}`)}
	client := ProviderClient{Adapter: provider.CodexAdapter{}, Runner: invocationRunner}
	result, err := client.Run(ctx, ProviderRequest{OperationID: claim.ExecutionID, Packet: packet, Workspace: ws})
	if err != nil {
		t.Fatalf("provider client run: %v", err)
	}
	if invocationRunner.CallCount() != 1 {
		t.Fatalf("expected exactly one invocation, got %d", invocationRunner.CallCount())
	}
	inv := invocationRunner.Calls[0]

	// A12: workspace write confinement, at the strength dp-v2-016 d12 allows.
	resolvedWorkspace, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if inv.WorkingDirectory != resolvedWorkspace {
		t.Fatalf("invocation working directory %q != resolved workspace %q", inv.WorkingDirectory, resolvedWorkspace)
	}
	for _, arg := range inv.Argv {
		resolvedArg, err := filepath.Abs(arg)
		if err != nil {
			continue
		}
		if filepath.IsAbs(arg) {
			rel, err := filepath.Rel(resolvedWorkspace, resolvedArg)
			if err == nil && (rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator)) {
				t.Fatalf("invocation argv element %q resolves outside the workspace", arg)
			}
		}
	}
	// HISTORICAL MEASUREMENT, 2026-08-25: this step asserted
	// len(inv.Environment) == 0 -- "no Secret Broker grant was merged onto
	// this Invocation, so its Environment must be empty".
	//
	// CURRENT MEASUREMENT, 2026-08-26 (V2-078): the claim is now STRUCTURAL
	// and cannot be written as a runtime assertion, because
	// provider.Invocation no longer declares an Environment field at all --
	// its exported field set is exactly {Argv, Stdin, WorkingDirectory}
	// (dp-v2-078 route (b)). An Invocation built here can carry no
	// environment whatsoever, granted or otherwise, so the assertion did not
	// weaken: it became a property of the type. The field set is held by
	// TestInvocationEnvironmentStaysUnconsumedByTheRunner, and the
	// credential-non-leakage case is still exercised at strength in
	// secret_broker_test.go.
	if info, err := os.Lstat(ws); err != nil {
		t.Fatal(err)
	} else {
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("workspace is a symlink")
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("workspace is group/world accessible: %v", info.Mode().Perm())
		}
	}
	// Detection: a write landing outside the workspace tree must be caught
	// by Workspace.Path (used to build any further per-execution path).
	if _, err := workspace.Path("../escape-attempt"); err == nil {
		t.Fatal("a write path escaping the workspace root was not detected")
	}

	// 12: parse the Result (already done inside ProviderClient.Run via
	// provider.CodexAdapter.Parse; assert on the outcome here).
	if !result.Succeeded || result.Checkpoint != "checkpoint-journey-1" {
		t.Fatalf("unexpected provider result: %#v", result)
	}
	if result.Output == "" {
		t.Fatal("Increment Artifact digest (provider OutputDigest) is empty")
	}

	// 13: checkpoint.
	if _, err := service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "journey1:attempt-1:checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// 14: obtain the result permit.
	resultPermit, err := service.Permit(runnerCtx, application.PermitRequest{RequestID: "journey1:attempt-1:result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil {
		t.Fatalf("result permit: %v", err)
	}
	if !resultPermit.Allowed {
		t.Fatalf("result permit denied: %s", resultPermit.Reason)
	}

	// 15: accept.
	accepted, err := service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: "journey1:attempt-1:accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: claim.FencingToken, Succeeded: result.Succeeded, Target: target})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// 16: assert the terminal Execution status, the recorded checkpoint, and
	// the Increment Artifact digest.
	if accepted.Status != domain.ExecutionSucceeded {
		t.Fatalf("terminal execution status = %s, want %s", accepted.Status, domain.ExecutionSucceeded)
	}
	if result.Checkpoint != "checkpoint-journey-1" {
		t.Fatalf("recorded checkpoint = %q", result.Checkpoint)
	}
	if result.Output == "" {
		t.Fatal("increment artifact digest missing at journey end")
	}
}
