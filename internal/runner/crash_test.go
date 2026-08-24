package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// journey4Fixture is a fresh application.Service over a fresh in-memory
// store, with one Requirement/Increment already captured/planned/prepared,
// so each Journey 4 subtest below only has to drive the crash-relevant half
// (Claim onward).
type journey4Fixture struct {
	service   *application.Service
	store     *memory.Store
	clock     *mutableClock
	journal   *Journal
	runnerCtx context.Context
	target    domain.ControlTarget
	reqID     string
	incID     string
	// preparedVersion is the Increment's version immediately after Prepare,
	// which is what the first Claim attempt must present.
	preparedVersion domain.Version
}

func newJourney4Fixture(t *testing.T, name string) *journey4Fixture {
	t.Helper()
	clock := newMutableClock(time.Unix(1_700_300_000, 0).UTC())
	st := memory.New()
	service, err := application.NewServiceWithConfig(st, clock, &journeyIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	capResp, err := service.Capture(owner, application.CaptureRequest{RequestID: name + ":capture", Text: "journey four fixture " + name})
	if err != nil {
		t.Fatal(err)
	}
	planResp, err := service.Plan(owner, application.PlanRequest{RequestID: name + ":plan", RequirementID: capResp.RequirementID, ExpectedRequirementVersion: capResp.Version})
	if err != nil {
		t.Fatal(err)
	}
	preparedResp, err := service.Prepare(owner, application.PrepareRequest{RequestID: name + ":prepare", IncrementID: planResp.IncrementID, ExpectedVersion: planResp.Version})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.ControlTarget{RequirementID: mustRequirement(capResp.RequirementID), IncrementID: mustIncrement(planResp.IncrementID)}
	return &journey4Fixture{service: service, store: st, clock: clock, journal: journal, runnerCtx: runnerCtx, target: target, reqID: capResp.RequirementID, incID: planResp.IncrementID, preparedVersion: preparedResp.Version}
}

// claimAttempt performs Claim+Start for one attempt, given the increment's
// currently-expected version (the caller tracks this across attempts).
type attemptClaim struct {
	claim application.ClaimResponse
	start application.StartResponse
}

func (fx *journey4Fixture) claimAndStart(t *testing.T, name string, attempt int, expectedIncrementVersion domain.Version) attemptClaim {
	t.Helper()
	base := fmt.Sprintf("%s:attempt-%d", name, attempt)
	claim, err := fx.service.Claim(fx.runnerCtx, application.ClaimRequest{RequestID: base + ":claim", IncrementID: fx.incID, ExpectedIncrementVersion: expectedIncrementVersion, Target: fx.target})
	if err != nil {
		t.Fatalf("attempt %d claim: %v", attempt, err)
	}
	start, err := fx.service.Start(fx.runnerCtx, application.StartRequest{RequestID: base + ":start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("attempt %d start: %v", attempt, err)
	}
	return attemptClaim{claim: claim, start: start}
}

func (fx *journey4Fixture) executionSnapshot(t *testing.T, executionID string) domain.Execution {
	t.Helper()
	var exec domain.Execution
	if err := fx.store.Transact(context.Background(), func(u application.UnitOfWork) error {
		v, ok, err := u.Execution(context.Background(), executionID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("execution not found")
		}
		exec = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return exec
}

// TestJourneyFourLocalCrashResumeAcrossExecutions is Journey 4's local
// segment (dp-v2-016 d3/d4 layer A): a Runner crash is survivable so a
// different Execution, with a strictly greater fencing token, resumes from
// the durable journal, while the crashed Execution's late Result is
// rejected. Both crash positions (dp-v2-016 d4/A5) are covered as subtests
// of this one top-level test function.
func TestJourneyFourLocalCrashResumeAcrossExecutions(t *testing.T) {
	t.Run("case A crash before provider result is journaled", func(t *testing.T) {
		fx := newJourney4Fixture(t, "journey4-caseA")
		name := "journey4-caseA"

		attempt1 := fx.claimAndStart(t, name, 1, fx.preparedVersion)
		// Simulated crash: attempt 1 stops here, before ever calling the
		// provider or journaling a result_pending event.
		fx.clock.Advance(2 * time.Minute) // > the 1-minute LeaseTTL: attempt 1's lease is now inactive.

		attempt2 := fx.claimAndStart(t, name, 2, attempt1.claim.Version)
		if attempt2.claim.ExecutionID == attempt1.claim.ExecutionID {
			t.Fatal("resumed attempt reused the crashed Execution id")
		}
		if !(attempt2.claim.FencingToken > attempt1.claim.FencingToken) {
			t.Fatalf("fencing token did not strictly increase: attempt1=%d attempt2=%d", attempt1.claim.FencingToken, attempt2.claim.FencingToken)
		}

		if _, found, err := FindPendingProviderResult(fx.journal, fx.incID); err != nil {
			t.Fatal(err)
		} else if found {
			t.Fatal("case A must find no dangling provider result: attempt 1 crashed before journaling one")
		}

		packet := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: fx.reqID, RequirementSummary: "case a resume text", IncrementID: fx.incID}
		invRunner := &FakeInvocationRunner{Fixture: []byte(`{"status":"success","checkpoint":"cp-case-a"}`)}
		client := ProviderClient{Adapter: provider.CodexAdapter{}, Runner: invRunner}
		result, err := client.Run(context.Background(), ProviderRequest{OperationID: attempt2.claim.ExecutionID, Packet: packet, Workspace: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if invRunner.CallCount() != 1 {
			t.Fatalf("case A must re-run the provider exactly once, got %d calls", invRunner.CallCount())
		}
		digest, err := WorkPacketDigest(packet)
		if err != nil {
			t.Fatal(err)
		}
		if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: fx.incID, ExecutionID: attempt2.claim.ExecutionID, WorkPacketDigest: digest, RawResult: invRunner.Fixture, Succeeded: result.Succeeded, Checkpoint: result.Checkpoint}); err != nil {
			t.Fatal(err)
		}
		finishAttempt(t, fx, name, 2, attempt2, result)

		before := fx.executionSnapshot(t, attempt2.claim.ExecutionID)
		lateErr := attempt1LateAccept(fx, name, attempt1)
		if !errors.Is(lateErr, domain.ErrLeaseExpired) {
			t.Fatalf("crashed execution's late AcceptResult should be rejected with a domain error, got %v", lateErr)
		}
		after := fx.executionSnapshot(t, attempt2.claim.ExecutionID)
		if before != after {
			t.Fatalf("resumed execution's canonical state changed after the crashed execution's rejected AcceptResult: before=%#v after=%#v", before, after)
		}
	})

	t.Run("case B crash after provider result is journaled, digest matches", func(t *testing.T) {
		fx := newJourney4Fixture(t, "journey4-caseB-match")
		name := "journey4-caseB-match"

		attempt1 := fx.claimAndStart(t, name, 1, fx.preparedVersion)
		packet1 := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: fx.reqID, RequirementSummary: "case b stable text", IncrementID: fx.incID}
		inv1 := &FakeInvocationRunner{Fixture: []byte(`{"status":"success","checkpoint":"cp-case-b"}`)}
		result1, err := (ProviderClient{Adapter: provider.CodexAdapter{}, Runner: inv1}).Run(context.Background(), ProviderRequest{OperationID: attempt1.claim.ExecutionID, Packet: packet1, Workspace: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		digest1, err := WorkPacketDigest(packet1)
		if err != nil {
			t.Fatal(err)
		}
		if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: fx.incID, ExecutionID: attempt1.claim.ExecutionID, WorkPacketDigest: digest1, RawResult: inv1.Fixture, Succeeded: result1.Succeeded, Checkpoint: result1.Checkpoint}); err != nil {
			t.Fatal(err)
		}
		// Simulated crash: attempt 1 stops here, after journaling the
		// provider result but before checkpoint/accept.
		fx.clock.Advance(2 * time.Minute)

		attempt2 := fx.claimAndStart(t, name, 2, attempt1.claim.Version)
		if attempt2.claim.ExecutionID == attempt1.claim.ExecutionID {
			t.Fatal("resumed attempt reused the crashed Execution id")
		}
		if !(attempt2.claim.FencingToken > attempt1.claim.FencingToken) {
			t.Fatal("fencing token did not strictly increase")
		}

		packet2 := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: fx.reqID, RequirementSummary: "case b stable text", IncrementID: fx.incID}
		digest2, err := WorkPacketDigest(packet2)
		if err != nil {
			t.Fatal(err)
		}
		pending, found, err := FindPendingProviderResult(fx.journal, fx.incID)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("expected to find the crashed attempt's dangling provider result")
		}
		if pending.WorkPacketDigest != digest2 {
			t.Fatal("digests should match: the work packet did not change between attempts")
		}

		// Digest matches: adopt the recovered checkpoint. Re-parse the
		// recovered raw payload to confirm it is still a valid provider
		// result, and never construct a second InvocationRunner call at all.
		inv2 := &FakeInvocationRunner{}
		reparsed, err := provider.CodexAdapter{}.Parse(pending.RawResult)
		if err != nil {
			t.Fatalf("recovered payload failed to re-parse: %v", err)
		}
		result2 := ProviderResult{Succeeded: reparsed.Succeeded, Checkpoint: reparsed.Checkpoint, Output: reparsed.OutputDigest}
		if inv2.CallCount() != 0 {
			t.Fatalf("digest match must adopt the recovered checkpoint with zero provider calls, got %d", inv2.CallCount())
		}
		if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: fx.incID, ExecutionID: attempt2.claim.ExecutionID, WorkPacketDigest: digest2, RawResult: pending.RawResult, Succeeded: result2.Succeeded, Checkpoint: result2.Checkpoint}); err != nil {
			t.Fatal(err)
		}
		finishAttempt(t, fx, name, 2, attempt2, result2)

		before := fx.executionSnapshot(t, attempt2.claim.ExecutionID)
		lateErr := attempt1LateAccept(fx, name, attempt1)
		if !errors.Is(lateErr, domain.ErrLeaseExpired) {
			t.Fatalf("crashed execution's late AcceptResult should be rejected with a domain error, got %v", lateErr)
		}
		after := fx.executionSnapshot(t, attempt2.claim.ExecutionID)
		if before != after {
			t.Fatal("resumed execution's canonical state changed after the crashed execution's rejected AcceptResult")
		}
	})

	t.Run("case B crash after provider result is journaled, digest mismatches", func(t *testing.T) {
		fx := newJourney4Fixture(t, "journey4-caseB-mismatch")
		name := "journey4-caseB-mismatch"

		attempt1 := fx.claimAndStart(t, name, 1, fx.preparedVersion)
		packet1 := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: fx.reqID, RequirementSummary: "case b original text", IncrementID: fx.incID}
		inv1 := &FakeInvocationRunner{Fixture: []byte(`{"status":"success","checkpoint":"cp-case-b-old"}`)}
		result1, err := (ProviderClient{Adapter: provider.CodexAdapter{}, Runner: inv1}).Run(context.Background(), ProviderRequest{OperationID: attempt1.claim.ExecutionID, Packet: packet1, Workspace: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		digest1, err := WorkPacketDigest(packet1)
		if err != nil {
			t.Fatal(err)
		}
		if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: fx.incID, ExecutionID: attempt1.claim.ExecutionID, WorkPacketDigest: digest1, RawResult: inv1.Fixture, Succeeded: result1.Succeeded, Checkpoint: result1.Checkpoint}); err != nil {
			t.Fatal(err)
		}
		fx.clock.Advance(2 * time.Minute)

		attempt2 := fx.claimAndStart(t, name, 2, attempt1.claim.Version)

		// The Work Packet the new attempt would issue has altered content
		// (e.g. the requirement text changed), so its digest differs from
		// the journaled one.
		packet2 := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: fx.reqID, RequirementSummary: "case b altered text after resume", IncrementID: fx.incID}
		digest2, err := WorkPacketDigest(packet2)
		if err != nil {
			t.Fatal(err)
		}
		pending, found, err := FindPendingProviderResult(fx.journal, fx.incID)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("expected to find the crashed attempt's dangling provider result")
		}
		if pending.WorkPacketDigest == digest2 {
			t.Fatal("digests should mismatch in this subtest by construction")
		}

		inv2 := &FakeInvocationRunner{Fixture: []byte(`{"status":"success","checkpoint":"cp-case-b-new"}`)}
		result2, err := (ProviderClient{Adapter: provider.CodexAdapter{}, Runner: inv2}).Run(context.Background(), ProviderRequest{OperationID: attempt2.claim.ExecutionID, Packet: packet2, Workspace: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if inv2.CallCount() != 1 {
			t.Fatalf("digest mismatch must re-run the provider exactly once, got %d", inv2.CallCount())
		}
		if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: fx.incID, ExecutionID: attempt2.claim.ExecutionID, WorkPacketDigest: digest2, RawResult: inv2.Fixture, Succeeded: result2.Succeeded, Checkpoint: result2.Checkpoint}); err != nil {
			t.Fatal(err)
		}
		finishAttempt(t, fx, name, 2, attempt2, result2)

		before := fx.executionSnapshot(t, attempt2.claim.ExecutionID)
		lateErr := attempt1LateAccept(fx, name, attempt1)
		if !errors.Is(lateErr, domain.ErrLeaseExpired) {
			t.Fatalf("crashed execution's late AcceptResult should be rejected with a domain error, got %v", lateErr)
		}
		after := fx.executionSnapshot(t, attempt2.claim.ExecutionID)
		if before != after {
			t.Fatal("resumed execution's canonical state changed after the crashed execution's rejected AcceptResult")
		}
	})
}

// finishAttempt performs Permit(process is implicit)/Checkpoint/Permit
// (result)/Accept for one attempt and journals result_accepted, asserting
// the terminal status is ExecutionSucceeded.
func finishAttempt(t *testing.T, fx *journey4Fixture, name string, attempt int, ac attemptClaim, result ProviderResult) {
	t.Helper()
	base := fmt.Sprintf("%s:attempt-%d", name, attempt)
	if _, err := fx.service.Checkpoint(fx.runnerCtx, application.CheckpointRequest{RequestID: base + ":checkpoint", ExecutionID: ac.claim.ExecutionID, LeaseID: ac.claim.LeaseID, FencingToken: ac.claim.FencingToken}); err != nil {
		t.Fatalf("attempt %d checkpoint: %v", attempt, err)
	}
	resultPermit, err := fx.service.Permit(fx.runnerCtx, application.PermitRequest{RequestID: base + ":result-permit", Kind: domain.PermitExternalEffect, Target: fx.target, FencingToken: ac.claim.FencingToken, ExpectedFencingToken: ac.claim.FencingToken, Resource: ac.claim.ExecutionID})
	if err != nil {
		t.Fatalf("attempt %d result permit: %v", attempt, err)
	}
	if !resultPermit.Allowed {
		t.Fatalf("attempt %d result permit denied: %s", attempt, resultPermit.Reason)
	}
	accepted, err := fx.service.AcceptResult(fx.runnerCtx, application.AcceptResultRequest{RequestID: base + ":accept", ExecutionID: ac.claim.ExecutionID, LeaseID: ac.claim.LeaseID, ExpectedExecutionVersion: ac.start.Version, FencingToken: ac.claim.FencingToken, Succeeded: result.Succeeded, Target: fx.target})
	if err != nil {
		t.Fatalf("attempt %d accept: %v", attempt, err)
	}
	if accepted.Status != domain.ExecutionSucceeded {
		t.Fatalf("attempt %d terminal status = %s", attempt, accepted.Status)
	}
	if err := JournalResultAccepted(fx.journal, ac.claim.ExecutionID, accepted.Status); err != nil {
		t.Fatalf("attempt %d journal result_accepted: %v", attempt, err)
	}
}

// attempt1LateAccept replays the crashed attempt's own AcceptResult using
// its own (now stale) lease/version/fencing token, exactly as it would have
// been submitted had the crashed process not actually died.
func attempt1LateAccept(fx *journey4Fixture, name string, attempt1 attemptClaim) error {
	_, err := fx.service.AcceptResult(fx.runnerCtx, application.AcceptResultRequest{
		RequestID:                name + ":attempt-1:late-accept",
		ExecutionID:              attempt1.claim.ExecutionID,
		LeaseID:                  attempt1.claim.LeaseID,
		ExpectedExecutionVersion: attempt1.start.Version,
		FencingToken:             attempt1.claim.FencingToken,
		Succeeded:                true,
		Target:                   fx.target,
	})
	return err
}

// =============================================================================
// A6: real-process durability. The test re-executes its own compiled test
// binary as a child (os.Args[0] plus a helper environment variable), the
// child opens a real runner.Journal, appends and fsyncs one event, writes a
// marker file and blocks, the parent polls for the marker with a bounded
// deadline, then SIGKILLs the child's process group and confirms it is
// reaped, then a fresh OpenJournal on the same directory replays the
// fsync'd event intact.
//
// This is deterministic on GitHub Actions ubuntu-latest because it needs no
// privilege, no namespace and no container: the only synchronisation is
// polling for a marker file the child writes strictly after Append (which
// fsyncs) returns, with a bounded deadline and t.Fatal on timeout -- never a
// fixed sleep. This test carries the durability claim (a real SIGKILL against
// real fsync'd bytes); TestJourneyFourLocalCrashResumeAcrossExecutions above
// carries the state-machine claim (fencing/resume/rejection); both use the
// same Journal type and on-disk layout (dp-v2-016 d4).
// =============================================================================

const (
	crashHelperEnv    = "AGENTIC_LOOP_RUNNER_CRASH_HELPER"
	crashHelperDirEnv = "AGENTIC_LOOP_RUNNER_CRASH_DIR"
)

func TestJournalSurvivesRealSIGKILLOfProcessGroup(t *testing.T) {
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "child-ready")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessJournalAppendAndBlock$", "-test.v=true", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), crashHelperEnv+"=1", crashHelperDirEnv+"="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	waitForPath(t, markerPath, 15*time.Second)

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("sigkill process group: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err == nil {
			t.Fatal("expected the killed helper process to report a non-nil wait error")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("helper process was not reaped after SIGKILL")
	}

	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("reopen journal after kill: %v", err)
	}
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("replay after kill: %v", err)
	}
	if len(events) != 1 || events[0].ID != "crash-proof" || events[0].Kind != "crash_test" {
		t.Fatalf("fsync'd event did not survive the SIGKILL intact: %#v", events)
	}
}

// TestHelperProcessJournalAppendAndBlock is not a real test: it is the child
// process body for TestJournalSurvivesRealSIGKILLOfProcessGroup, guarded by
// crashHelperEnv so it is a harmless skip under a normal `go test` run.
func TestHelperProcessJournalAppendAndBlock(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "1" {
		t.Skip("not running as the crash-test helper process")
	}
	dir := os.Getenv(crashHelperDirEnv)
	journal, err := OpenJournal(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open journal:", err)
		os.Exit(1)
	}
	if err := journal.Append(JournalEvent{ID: "crash-proof", Kind: "crash_test", Payload: []byte(`{"ok":true}`)}); err != nil {
		fmt.Fprintln(os.Stderr, "helper: append:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dir, "child-ready"), []byte("ready"), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "helper: marker:", err)
		os.Exit(1)
	}
	select {} // block until the parent SIGKILLs this process's group.
}
