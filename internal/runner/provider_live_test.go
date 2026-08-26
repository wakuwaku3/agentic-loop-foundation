package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// liveRecordRelPath is the approved provider-preflight record's path
// relative to the repository root (dp-v2-017 B1). It names V2-080's pooled
// re-observation record, which carries its own limits.ledger_path: the gate
// therefore admits this suite against an empty ledger of its own and never
// spends, or exhausts, the headroom of any record this suite was exercised
// under before. Those earlier ledgers were measured on 2026-08-26 and stand
// at 11 of 16 (V2-017), 23 of 24 (V2-022), 11 of 12 (V2-063) and 12 of 12
// (V2-075), so V2-075's is full and borrowing it is impossible as well as
// prohibited. liveTaskID must stay equal to the named record's task_id,
// because CostLedger.Reserve refuses a ledger whose recorded task_id
// disagrees with the run's.
//
// These two constants are inside the runner component's key closure and are
// therefore the only part of the exercised file set a re-observation task
// may touch; the edit must land before the exercise is observed and nothing
// in that set may be edited afterwards (v2-task-dag.md section 4, G6b).
const liveRecordRelPath = ".agents/v2/provider-preflight/V2-080-provider-live-claude-pooled.json"
const liveTaskID = "V2-080"
const liveRepositoryID = "agentic-loop-foundation"

// requireLiveProvider is dp-v2-017 d11's three-condition gate. Every live
// test calls this first; if any condition fails, it SKIPS (never fails),
// logging via t.Logf which condition failed, so a plain `go test ./...`
// (and therefore `make check`) always skips this whole suite with zero
// invocations by default.
func requireLiveProvider(t *testing.T) (repoRoot, recordPath string, record PreflightRecord) {
	t.Helper()
	if os.Getenv("AGENTIC_LOOP_LIVE_PROVIDER") != "1" {
		t.Logf("live provider gate: AGENTIC_LOOP_LIVE_PROVIDER=%q, want \"1\"", os.Getenv("AGENTIC_LOOP_LIVE_PROVIDER"))
		t.Skip("live provider suite is disabled")
	}
	repoRoot = mustRepoRoot(t)
	recordPath = filepath.Join(repoRoot, liveRecordRelPath)
	rec, err := LoadPreflightRecord(repoRoot, recordPath)
	if err != nil {
		t.Logf("live provider gate: approved record does not validate: %v", err)
		t.Skip("live provider suite is disabled")
	}
	if rec.LedgerPath == "" || !filepath.IsAbs(rec.LedgerPath) {
		t.Logf("live provider gate: ledger_path %q is not usable", rec.LedgerPath)
		t.Skip("live provider suite is disabled")
	}
	if err := os.MkdirAll(filepath.Dir(rec.LedgerPath), 0700); err != nil {
		t.Logf("live provider gate: ledger directory is not writable: %v", err)
		t.Skip("live provider suite is disabled")
	}
	probe := filepath.Join(filepath.Dir(rec.LedgerPath), ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
		t.Logf("live provider gate: ledger path is not writable: %v", err)
		t.Skip("live provider suite is disabled")
	}
	_ = os.Remove(probe)
	return repoRoot, recordPath, rec
}

// runInvocation is Build -> Runner.Run -> Parse, exposing the intermediate
// projected bytes (never the real CLI's raw stdout, which SupervisedInvocationRunner
// never returns to any caller) so a live test can journal exactly what a
// real Orchestrator would journal.
func runInvocation(ctx context.Context, runner InvocationRunner, adapter provider.Adapter, req provider.Request) ([]byte, provider.Result, error) {
	if err := req.Validate(); err != nil {
		return nil, provider.Result{}, err
	}
	inv, err := adapter.Build(req)
	if err != nil {
		return nil, provider.Result{}, err
	}
	raw, err := runner.Run(ctx, inv)
	if err != nil {
		return raw, provider.Result{}, err
	}
	result, err := adapter.Parse(raw)
	return raw, result, err
}

// randomMarker returns a highly distinctive, unique token used to prove
// dp-v2-017 B8: the live suite instructs the real model to reply with
// exactly this token, then asserts the token itself never reaches the
// journal (absence) while sha256(token) does (presence), without this test
// ever capturing or persisting the real CLI's raw stdout anywhere.
func randomMarker(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "LOOP-V2-017-B8-" + hex.EncodeToString(b)
}

func livePacket(reqID, incID, instruction string) provider.WorkPacket {
	return provider.WorkPacket{Version: provider.ContractVersion, RequirementID: reqID, RequirementSummary: instruction, IncrementID: incID, Repository: liveRepositoryID}
}

// findChildPID polls (bounded deadline, no fixed sleep) for a direct child
// of the current process whose /proc/<pid>/status Name contains nameSubstr,
// returning its pid once found. It is used only to make "the process was
// still running when we acted on it" an observable fact for I5's real
// SIGKILL and I8's real cancel, never to defeat a fixed-sleep prohibition
// with a disguised one: the poll interval is short and bounded exactly like
// waitForPath/waitUntilProcessGone already in this package.
func findChildPID(t *testing.T, nameSubstr string, deadline time.Duration) int {
	t.Helper()
	self := os.Getpid()
	end := time.Now().Add(deadline)
	for {
		entries, err := os.ReadDir("/proc")
		if err == nil {
			for _, e := range entries {
				pid, convErr := strconv.Atoi(e.Name())
				if convErr != nil {
					continue
				}
				b, rerr := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
				if rerr != nil {
					continue
				}
				var name string
				ppid := -1
				for _, line := range strings.Split(string(b), "\n") {
					if strings.HasPrefix(line, "Name:") {
						name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
					}
					if strings.HasPrefix(line, "PPid:") {
						ppid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
					}
				}
				if ppid == self && strings.Contains(name, nameSubstr) {
					return pid
				}
			}
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for a child process named %q", nameSubstr)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// liveFixture bundles the canonical-application state shared by every I1..I8
// phase: one Repository (liveRepositoryID), one clock, one journal, one
// CostLedger bound to the approved record's real ledger_path.
type liveFixture struct {
	repoRoot, recordPath string
	record               PreflightRecord
	ledger               *CostLedger
	clock                *mutableClock
	store                *memory.Store
	service              *application.Service
	journal              *Journal
	dataRoot             string
	workspace            *Workspace

	// i1* is retained so caseB (B15) can reuse I1's journal without any
	// additional invocation.
	i1RequirementID string
	i1IncrementID   string
	i1ExecutionID   string
	i1Packet        provider.WorkPacket
	i1RawResult     []byte
}

func newLiveFixture(t *testing.T, repoRoot, recordPath string, record PreflightRecord) *liveFixture {
	t.Helper()
	clock := newMutableClock(time.Unix(1_700_600_000, 0).UTC())
	st := memory.New()
	service, err := application.NewServiceWithConfig(st, clock, &journeyIDs{}, application.ServiceConfig{InstallationID: "install-live", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	journal, err := OpenJournal(filepath.Join(dataRoot, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	if err := os.Chmod(workspaceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &liveFixture{
		repoRoot: repoRoot, recordPath: recordPath, record: record,
		ledger: &CostLedger{Path: record.LedgerPath, Provider: "claude", TaskID: liveTaskID},
		clock:  clock, store: st, service: service, journal: journal, dataRoot: dataRoot, workspace: workspace,
	}
}

func (fx *liveFixture) newRunner(t *testing.T, purpose, executionID string) SupervisedInvocationRunner {
	t.Helper()
	log, err := NewBoundedLog(fx.dataRoot, executionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return SupervisedInvocationRunner{
		Supervisor: ProcessSupervisor{TermGrace: 3 * time.Second},
		Log:        log,
		Ledger:     fx.ledger,
		RepoRoot:   fx.repoRoot,
		RecordPath: fx.recordPath,
		Purpose:    purpose,
	}
}

func (fx *liveFixture) callers(ctx context.Context) (owner, runnerCtx context.Context) {
	owner = application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "owner-live"})
	runnerCtx = application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: "runner-live", RunnerID: "runner-live"})
	return owner, runnerCtx
}

// captureplanPrepare is the shared setup every named invocation's Increment
// needs.
func (fx *liveFixture) capturePlanPrepare(t *testing.T, owner context.Context, base, text string) (reqID, incID string, preparedVersion domain.Version) {
	t.Helper()
	capResp, err := fx.service.Capture(owner, application.CaptureRequest{RequestID: base + ":capture", Text: text})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	planResp, err := fx.service.Plan(owner, application.PlanRequest{RequestID: base + ":plan", RequirementID: capResp.RequirementID, ExpectedRequirementVersion: capResp.Version})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	preparedResp, err := fx.service.Prepare(owner, application.PrepareRequest{RequestID: base + ":prepare", IncrementID: planResp.IncrementID, ExpectedVersion: planResp.Version})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return capResp.RequirementID, planResp.IncrementID, preparedResp.Version
}

// TestProviderLiveVerticalSlice is the whole dp-v2-017 d9 vertical slice:
// eight named real invocations against the real claude CLI, each mapped
// 1:1 to one proof. It skips entirely unless requireLiveProvider's three
// conditions all hold (dp-v2-017 d11); a plain `go test ./...` therefore
// makes zero real invocations.
func TestProviderLiveVerticalSlice(t *testing.T) {
	repoRoot, recordPath, record := requireLiveProvider(t)
	fx := newLiveFixture(t, repoRoot, recordPath, record)

	if !t.Run("I1_happy_journey", fx.testI1) {
		t.Fatal("I1 failed; stopping before spending further ledger budget on I2..I8")
	}
	if !t.Run("I2_wire_and_identity", fx.testI2) {
		t.Fatal("I2 failed; stopping before spending further ledger budget on I3..I8")
	}
	if !t.Run("I3_credential_isolation", fx.testI3) {
		t.Fatal("I3 failed; stopping before spending further ledger budget on I4..I8")
	}
	if !t.Run("I4_lease_continuity", fx.testI4) {
		t.Fatal("I4 failed; stopping before spending further ledger budget on I5..I8")
	}
	if !t.Run("I5_I6_crash_resume", fx.testI5AndI6) {
		t.Fatal("I5/I6 failed; stopping before spending further ledger budget on I7/I8")
	}
	if !t.Run("I7_real_transport_failure", fx.testI7) {
		t.Fatal("I7 failed; stopping before spending further ledger budget on I8")
	}
	if !t.Run("I8_real_cancel", fx.testI8) {
		t.Fatal("I8 failed")
	}
	t.Run("caseB_reuses_I1_journal_zero_invocations", fx.testCaseB)
	t.Run("ledger_snapshot", fx.logLedgerSnapshot)
}

// testI1 (dp-v2-017 B11): the full journey -- requirement intake through
// claim, workspace create, Work Packet build and Validate, real invocation,
// checkpoint, AcceptResult, terminal ExecutionSucceeded and a non-empty
// Increment Artifact digest. Also carries B8's marker/digest proof.
func (fx *liveFixture) testI1(t *testing.T) {
	fx.clock.Advance(25 * time.Hour) // fresh per-day quota accounting for each phase.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner, runnerCtx := fx.callers(ctx)

	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, "v2017-i1", "V2-017 I1: prove the real Provider vertical slice end to end")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}
	claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i1:claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ws, err := fx.workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	startResp, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i1:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	processPermit, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: "v2017-i1:permit", Kind: domain.PermitProcess, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil || !processPermit.Allowed {
		t.Fatalf("process permit: err=%v allowed=%v reason=%s", err, processPermit.Allowed, processPermit.Reason)
	}

	marker := randomMarker(t)
	packet := livePacket(reqID, incID, fmt.Sprintf("Do not use any tools. Do not read or write any files. Output ONLY the following exact text, with no other words, no punctuation, and no explanation: %s", marker))
	if err := packet.Validate(); err != nil {
		t.Fatalf("work packet failed to validate: %v", err)
	}

	runner := fx.newRunner(t, "V2-017-I1-happy-journey", claim.ExecutionID)
	raw, result, err := runInvocation(ctx, runner, provider.ClaudeAdapter{}, provider.Request{OperationID: claim.ExecutionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("invocation: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("provider run did not succeed: %#v", result)
	}
	if result.OutputDigest == "" {
		t.Fatal("Increment Artifact digest (OutputDigest) is empty")
	}
	if !strings.HasPrefix(result.Checkpoint, "claude:") {
		t.Fatalf("unexpected checkpoint shape: %s", result.Checkpoint)
	}

	digest, err := WorkPacketDigest(packet)
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: incID, ExecutionID: claim.ExecutionID, WorkPacketDigest: digest, RawResult: raw, Succeeded: result.Succeeded, Checkpoint: result.Checkpoint}); err != nil {
		t.Fatal(err)
	}

	// --- B8: the model's response text never reaches the journal; only
	// sha256(response) does. Checked immediately after journaling (before
	// AcceptResult), because FindPendingProviderResult deliberately
	// excludes already-accepted executions -- that is its own correct
	// recovery semantics, not a bug, so this proof must not rely on it
	// after Accept. The journal's on-disk bytes base64-encode
	// PendingProviderRecord.RawResult (encoding/json's standard []byte
	// behaviour, unrelated to this task), so the meaningful check decodes
	// the journal the same way a real resume path would (via
	// FindPendingProviderResult) rather than grepping raw file bytes,
	// which would be defeated by that encoding for the presence half of
	// this proof even though it happens to still work for the absence
	// half.
	pendingCheck, found, err := FindPendingProviderResult(fx.journal, incID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("B8: could not find the just-journaled pending provider record")
	}
	if strings.Contains(string(pendingCheck.RawResult), marker) {
		t.Fatal("B8 violation: the marker (a stand-in for the model's response text) is present in the journaled projection")
	}
	var projected struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(pendingCheck.RawResult, &projected); err != nil {
		t.Fatalf("could not parse the journaled projection to extract its output digest: %v", err)
	}
	if !strings.HasPrefix(projected.Output, "sha256:") {
		t.Fatalf("projection output is not a sha256 digest: %q", projected.Output)
	}
	t.Logf("B8: marker absent from the journaled projection; output digest present: %s", projected.Output)

	if _, err := fx.service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "v2017-i1:checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	resultPermit, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: "v2017-i1:result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil || !resultPermit.Allowed {
		t.Fatalf("result permit: err=%v allowed=%v", err, resultPermit.Allowed)
	}
	accepted, err := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: "v2017-i1:accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: claim.FencingToken, Succeeded: result.Succeeded, Target: target})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Status != domain.ExecutionSucceeded {
		t.Fatalf("terminal execution status = %s, want %s", accepted.Status, domain.ExecutionSucceeded)
	}
	if err := JournalResultAccepted(fx.journal, claim.ExecutionID, accepted.Status); err != nil {
		t.Fatal(err)
	}

	fx.i1RequirementID, fx.i1IncrementID, fx.i1Packet, fx.i1RawResult, fx.i1ExecutionID = reqID, incID, packet, raw, claim.ExecutionID
}

// testI2 (dp-v2-017 B12): wire and identity capture. A second, independent
// real invocation whose sole purpose is to prove the resolved absolute argv
// really ran the real binary and that session_id/usage/total_cost_usd/
// duration_api_ms/duration_ms/num_turns are captured into the ledger.
func (fx *liveFixture) testI2(t *testing.T) {
	fx.clock.Advance(25 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner, runnerCtx := fx.callers(ctx)

	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, "v2017-i2", "V2-017 I2: wire and identity capture")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}
	claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i2:claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ws, err := fx.workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	startResp, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i2:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	marker := randomMarker(t)
	packet := livePacket(reqID, incID, fmt.Sprintf("Do not use any tools. Output ONLY the following exact text, with no other words: %s", marker))
	runner := fx.newRunner(t, "V2-017-I2-wire-and-identity", claim.ExecutionID)

	snapBefore, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := runInvocation(ctx, runner, provider.ClaudeAdapter{}, provider.Request{OperationID: claim.ExecutionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("invocation: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("provider run did not succeed: %#v", result)
	}
	snapAfter, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapAfter.InvocationCount != snapBefore.InvocationCount+1 {
		t.Fatalf("ledger invocation count did not advance by exactly one: before=%d after=%d", snapBefore.InvocationCount, snapAfter.InvocationCount)
	}
	entry := snapAfter.Entries[len(snapAfter.Entries)-1]
	if entry.SessionID == "" {
		t.Fatal("ledger entry has no session_id")
	}
	if entry.ActualUSD <= 0 {
		t.Fatalf("ledger entry actual_usd is not positive: %v", entry.ActualUSD)
	}
	if entry.InputCount <= 0 && entry.CacheReadCount <= 0 && entry.CacheCreationCount <= 0 {
		t.Fatalf("ledger entry has no usage counts at all: %#v", entry)
	}
	if entry.DurationAPIMS <= 0 && entry.DurationMS <= 0 {
		t.Fatalf("ledger entry has no duration: %#v", entry)
	}
	if entry.NumTurns <= 0 {
		t.Fatalf("ledger entry num_turns is not positive: %d", entry.NumTurns)
	}

	// Empirically determined minimum environment.base_names: HOME and PATH
	// are sufficient (confirmed by this invocation's own success; a prior
	// out-of-band manual probe with exactly HOME+PATH and no other
	// variable, made outside this ledger, first established this before
	// any ledger-tracked invocation was made). HOME: the claude CLI reads
	// its own credential store and local settings under $HOME/.claude.
	// PATH: needed for the CLI's own internal subprocess resolution even
	// though argv[0] itself is resolved to an absolute path by this
	// runner. No third variable was needed.
	t.Logf("I2: session_id present=%v usage(in=%d out=%d cache_read=%d cache_creation=%d) total_cost_usd=%.4f duration_api_ms=%d duration_ms=%d num_turns=%d",
		entry.SessionID != "", entry.InputCount, entry.OutputCount, entry.CacheReadCount, entry.CacheCreationCount, entry.ActualUSD, entry.DurationAPIMS, entry.DurationMS, entry.NumTurns)
	t.Logf("I2: environment.base_names confirmed sufficient: %v (justification: HOME locates the CLI's own credential/config store; PATH is required by the CLI's own internal process resolution)", fx.record.EnvironmentBaseNames)
	t.Log("I2: the projection is built in memory only; the sha256 recorded in the ledger's checkpoint/output fields is the sole persisted representation of the response")

	// Finish the canonical lifecycle so the Execution is not left dangling.
	if _, err := fx.service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "v2017-i2:checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	resultPermit, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: "v2017-i2:result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil || !resultPermit.Allowed {
		t.Fatalf("result permit: err=%v allowed=%v", err, resultPermit.Allowed)
	}
	accepted, err := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: "v2017-i2:accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: claim.FencingToken, Succeeded: result.Succeeded, Target: target})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Status != domain.ExecutionSucceeded {
		t.Fatalf("terminal status = %s", accepted.Status)
	}

	// V2-080 A8, second proof, in the two phases the two shipped mechanisms
	// need. These are nested inside I2 on purpose: I2 is the wire-and-identity
	// phase, and the exercise's nine top-level subtests are not widened.
	t.Run("f_declared_working_directory_reported_by_the_real_child_unconfined", func(t *testing.T) {
		fx.probeWorkingDirectoryFromTheChild(t, "v2080-a8-unconfined", "V2-080-A8-working-directory-unconfined", false)
	})
	t.Run("g_declared_working_directory_reported_by_the_real_child_under_confinement", func(t *testing.T) {
		fx.probeWorkingDirectoryFromTheChild(t, "v2080-a8-confined", "V2-080-A8-working-directory-confined", true)
	})
}

// probeWorkingDirectoryFromTheChild is V2-080 A8's second proof: it asks the
// real CLI to state the working directory it is running in and compares the
// sha256 the projection carries against the sha256 of the directory the
// Invocation declared. The comparison is one-way and by digest only, so no
// response text is ever captured, journalled or recorded -- exactly the
// mechanism B8 already relies on -- yet the value being compared is one the
// CHILD produced and not one the harness asserted.
//
// It runs twice, once with Confine nil and once with Confine set, because the
// two are different shipped mechanisms and only one of them can be proved by
// the kernel read I5 performs. With Confine nil the directory is applied by
// ProcessSupervisor through cmd.Dir. With Confine set, cmd.Dir is
// deliberately never assigned (ProcessSupervisor's doc comment and
// TestProcessSupervisorNeverAssignsCmdDirUnderConfinement) because a cmd.Dir
// chdir would happen in the forked child before unshare runs and therefore
// before either mount pair exists, so NamespaceConfinement.wrap emits one
// shell-quoted cd inside the namespace after both mount pairs and
// immediately before exec. No live child had ever confirmed either.
//
// The unconfined phase doubles as the calibration for the confined one: it
// runs the identical prompt against a directory the kernel read in I5
// independently confirms, so a digest match there establishes that the
// candidate set below can express what this CLI actually emits. If the
// unconfined phase does not match, the confined phase's non-match says
// nothing about the working directory and is recorded as UNPROVEN rather
// than asserted either way. Nothing here is ever asserted from the harness.
func (fx *liveFixture) probeWorkingDirectoryFromTheChild(t *testing.T, base, purpose string, confine bool) {
	t.Helper()
	fx.clock.Advance(25 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	owner, runnerCtx := fx.callers(ctx)

	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, base, "V2-080 A8: the working directory an Invocation declares must reach the real child")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}
	claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: base + ":claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ws, err := fx.workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	startResp, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: base + ":start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	packet := livePacket(reqID, incID, "Do not use any tools. Do not read or write any file. Reply with ONLY the absolute filesystem path of the working directory you are running in, exactly as it appears in the environment information you were given, with no other words, no quotes, no code fence and no trailing punctuation.")
	if err := packet.Validate(); err != nil {
		t.Fatalf("work packet failed to validate: %v", err)
	}
	inv, err := provider.ClaudeAdapter{}.Build(provider.Request{OperationID: claim.ExecutionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if inv.WorkingDirectory != ws {
		t.Fatalf("the Invocation declares working directory %q but the Increment's workspace is %q", inv.WorkingDirectory, ws)
	}
	runner := fx.newRunner(t, purpose, claim.ExecutionID)
	if confine {
		runner.Supervisor.Confine = &NamespaceConfinement{Workspace: ws}
	}

	raw, runErr := runner.Run(ctx, inv)
	verdict := "UNPROVEN"
	matched := ""
	if runErr != nil {
		t.Logf("A8 %s under confine=%v: the invocation produced no result to compare (%v); recorded as an attempt, never asserted from the harness", verdict, confine, runErr)
	} else {
		var projected struct {
			Output string `json:"output"`
		}
		switch {
		case json.Unmarshal(raw, &projected) != nil:
			t.Logf("A8 %s under confine=%v: the projection could not be parsed", verdict, confine)
		case !strings.HasPrefix(projected.Output, "sha256:"):
			t.Logf("A8 %s under confine=%v: the projection carries no response digest, so the child produced no comparable value", verdict, confine)
		default:
			for _, candidate := range []struct{ shape, value string }{
				{"exact", ws},
				{"trailing-newline", ws + "\n"},
				{"trailing-slash", ws + "/"},
				{"backticked", "`" + ws + "`"},
				{"double-quoted", "\"" + ws + "\""},
				{"single-quoted", "'" + ws + "'"},
				{"trailing-period", ws + "."},
				{"labelled", "Working directory: " + ws},
			} {
				if provider.DigestOutput(candidate.value) == projected.Output {
					matched, verdict = candidate.shape, "PROVEN"
					break
				}
			}
			// A negative control, so a match is known not to be an
			// artefact of the comparison: the same procedure against a
			// sibling directory of the same shape must NOT match.
			if provider.DigestOutput(ws+"-not-the-workspace") == projected.Output {
				t.Errorf("A8 negative control failed under confine=%v: a sibling path digests to the same value as the child's response", confine)
			}
			if verdict == "PROVEN" {
				t.Logf("A8 PROVEN from the real child under confine=%v: the child's own statement of its working directory digests exactly to the declared Invocation.WorkingDirectory (%s), matching shape %q; the mechanism proved is %s", confine, ws, matched, mechanismName(confine))
			} else {
				t.Logf("A8 UNPROVEN under confine=%v: the child produced a response digest but it matches none of the 8 exact renderings of the declared directory %s, so nothing is asserted; the mechanism left unproven is %s", confine, ws, mechanismName(confine))
			}
		}
	}

	// Finish the canonical lifecycle either way, so no Execution is left
	// dangling by a probe.
	result, parseErr := (provider.ClaudeAdapter{}).Parse(raw)
	succeeded := parseErr == nil && result.Succeeded
	if _, err := fx.service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: base + ":checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	resultPermit, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: base + ":result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil || !resultPermit.Allowed {
		t.Fatalf("result permit: err=%v allowed=%v", err, resultPermit.Allowed)
	}
	if _, err := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: base + ":accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: claim.FencingToken, Succeeded: succeeded, Target: target}); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

// mechanismName names which of the two shipped working-directory mechanisms a
// probe exercised, so an evidence transcription cannot confuse them.
func mechanismName(confine bool) string {
	if confine {
		return "NamespaceConfinement.wrap's single cd, emitted inside the namespace after both mount pairs and immediately before exec (V2-077)"
	}
	return "ProcessSupervisor.Dir applied through cmd.Dir, which is the path taken whenever Confine is nil"
}

// testI3 (dp-v2-017 B13/d8): credential isolation, all six sub-proofs
// against a real invocation. Each is asserted and reported separately.
func (fx *liveFixture) testI3(t *testing.T) {
	fx.clock.Advance(25 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner, runnerCtx := fx.callers(ctx)

	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, "v2017-i3", "V2-017 I3: credential isolation observation")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}
	claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i3:claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ws, err := fx.workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	startResp, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i3:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	marker := randomMarker(t)
	packet := livePacket(reqID, incID, fmt.Sprintf("Do not use any tools. Output ONLY the following exact text, with no other words: %s", marker))
	runner := fx.newRunner(t, "V2-017-I3-credential-isolation", claim.ExecutionID)
	raw, result, err := runInvocation(ctx, runner, provider.ClaudeAdapter{}, provider.Request{OperationID: claim.ExecutionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("invocation: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("provider run did not succeed: %#v", result)
	}

	// (a) The approved record declares environment.granted_names empty.
	t.Run("a_granted_names_empty", func(t *testing.T) {
		if len(fx.record.EnvironmentGranted) != 0 {
			t.Fatalf("environment.granted_names is not empty: %v", fx.record.EnvironmentGranted)
		}
	})

	// (b) The SecretBroker allowlist for "claude" is empty, so Lease
	// refuses -- measured as a positive control, not declared.
	t.Run("b_secret_broker_allowlist_empty_measured", func(t *testing.T) {
		broker := NewSecretBroker(nil, MapCredentialSource{}, map[string][]string{}, fx.clock.Now)
		scope := Scope{ExecutionID: claim.ExecutionID, Repository: liveRepositoryID, Provider: "claude", Target: target, ControlRevision: 1, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, ExpiresAt: fx.clock.Now().Add(time.Hour)}
		if _, err := broker.Lease(runnerCtx, scope, []string{"ANTHROPIC_API_KEY"}); !errors.Is(err, ErrSecretNotAllowed) {
			t.Fatalf("want ErrSecretNotAllowed, got %v", err)
		}
	})

	// (c) The environment the child receives is set-equal to
	// environment.base_names. Run's only call site for building that
	// environment is buildEnvironmentFromBaseNames(record.
	// EnvironmentBaseNames) (provider.go), verified here by calling the
	// same function directly and by AST-scanning provider.go's single call
	// site. Re-measured 2026-08-26 (V2-078): this comment used to say "No
	// Grant is attached; the built Invocation.Environment is ...". There is
	// no longer any Grant to attach and no Invocation field to attach it to
	// -- Grant.Apply, ProviderClient.Grant and Invocation.Environment were
	// all deleted -- and the assertion below is unchanged, because it always
	// measured the environment the runner builds rather than anything the
	// Invocation carried.
	t.Run("c_environment_set_equal_to_base_names", func(t *testing.T) {
		env, err := buildEnvironmentFromBaseNames(fx.record.EnvironmentBaseNames)
		if err != nil {
			t.Fatal(err)
		}
		if len(env) != len(fx.record.EnvironmentBaseNames) {
			t.Fatalf("built environment has %d entries, want %d (set-equal to base_names)", len(env), len(fx.record.EnvironmentBaseNames))
		}
		names := map[string]bool{}
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			names[name] = true
		}
		for _, want := range fx.record.EnvironmentBaseNames {
			if !names[want] {
				t.Fatalf("base name %s missing from built environment", want)
			}
		}
	})

	// (d) GuardEnvironment against exactly that allowlist, and GuardCommand
	// over the resolved argv, both pass.
	t.Run("d_guard_environment_and_guard_command_pass", func(t *testing.T) {
		env, err := buildEnvironmentFromBaseNames(fx.record.EnvironmentBaseNames)
		if err != nil {
			t.Fatal(err)
		}
		if err := GuardEnvironment(env, allowlistFromNames(fx.record.EnvironmentBaseNames)); err != nil {
			t.Fatalf("GuardEnvironment: %v", err)
		}
		resolvedArgv := []string{fx.record.ExecutablePath, "--print", "--output-format", "json", "--no-session-persistence"}
		if err := GuardCommand(resolvedArgv, env); err != nil {
			t.Fatalf("GuardCommand: %v", err)
		}
	})

	// (e) Six-target absence scan (V2-016 A11's pattern) plus a positive
	// control that the environment values (HOME/PATH) are genuinely
	// present somewhere observable (proving the search below is not
	// vacuous), even though HOME/PATH are not credential-shaped secrets.
	t.Run("e_six_target_absence_scan", func(t *testing.T) {
		digest, err := WorkPacketDigest(packet)
		if err != nil {
			t.Fatal(err)
		}
		if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: incID, ExecutionID: claim.ExecutionID, WorkPacketDigest: digest, RawResult: raw, Succeeded: result.Succeeded, Checkpoint: result.Checkpoint}); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "v2017-i3:checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		resultPermit, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: "v2017-i3:result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
		if err != nil || !resultPermit.Allowed {
			t.Fatalf("result permit: err=%v allowed=%v", err, resultPermit.Allowed)
		}
		accepted, err := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: "v2017-i3:accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: claim.FencingToken, Succeeded: result.Succeeded, Target: target})
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if err := JournalResultAccepted(fx.journal, claim.ExecutionID, accepted.Status); err != nil {
			t.Fatal(err)
		}
		acceptedJSON, err := json.Marshal(accepted)
		if err != nil {
			t.Fatal(err)
		}
		var events []application.Event
		if err := fx.store.Transact(context.Background(), func(u application.UnitOfWork) error {
			page, _, err := u.EventsPage(context.Background(), "", 1000)
			events = page
			return err
		}); err != nil {
			t.Fatal(err)
		}
		eventsJSON, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		journalBytes, err := os.ReadFile(filepath.Join(fx.dataRoot, "journal", "events.log"))
		if err != nil {
			t.Fatal(err)
		}
		realLog, err := os.ReadFile(filepath.Join(fx.dataRoot, "logs", claim.ExecutionID+".log"))
		if err != nil {
			t.Fatal(err)
		}
		packetBytes, err := json.Marshal(packet)
		if err != nil {
			t.Fatal(err)
		}

		env, err := buildEnvironmentFromBaseNames(fx.record.EnvironmentBaseNames)
		if err != nil {
			t.Fatal(err)
		}
		values := make([][]byte, 0, len(env))
		for _, kv := range env {
			if _, v, ok := strings.Cut(kv, "="); ok && v != "" {
				values = append(values, []byte(v))
			}
		}

		targets := map[string][]byte{
			"journal file":           journalBytes,
			"marshalled work packet": packetBytes,
			"bounded log file":       realLog,
			"canonical store events": eventsJSON,
			"accepted result":        acceptedJSON,
		}
		for name, data := range targets {
			if secretValue.Match(data) {
				t.Fatalf("%s matches the runner's secretValue pattern", name)
			}
			for _, v := range values {
				if len(v) > 2 && bytesContainsString(data, string(v)) {
					t.Fatalf("%s contains a value present in the child environment (%s)", name, v)
				}
			}
		}
		for i, arg := range os.Args {
			if secretValue.MatchString(arg) {
				t.Fatalf("os.Args[%d] matches the runner's secretValue pattern", i)
			}
		}
		found := false
		for _, v := range values {
			for _, arg := range os.Args {
				if bytesContainsString([]byte(arg), string(v)) {
					found = true
				}
			}
		}
		_ = found // os.Args legitimately may contain unrelated substrings; this is not a failure condition either way.
		t.Log("e: six-target absence scan passed (journal, work packet, bounded log, canonical events, accepted result, os.Args/argv)")
	})

	// (f) is TestNoCredentialStorePathReferencedInRunnerNonTestFiles
	// (source_guard_test.go), reported here by reference: it is a
	// standing, always-run AST scan, not specific to this one invocation.
	t.Log("f: see TestNoCredentialStorePathReferencedInRunnerNonTestFiles (source_guard_test.go) for the non-test-file AST scan")
}

// testI4 (dp-v2-017 B14): a second Increment on the same Repository
// spanning more than one LeaseTTL of LeaseKeeper ticks, with a real
// invocation in between; every RenewResponse.ExpiresAt strictly advances
// and the checkpoint is preserved across the renewal.
func (fx *liveFixture) testI4(t *testing.T) {
	fx.clock.Advance(25 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner, runnerCtx := fx.callers(ctx)

	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, "v2017-i4", "V2-017 I4: lease continuity across a real invocation")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}
	claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i4:claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ws, err := fx.workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	startResp, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i4:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	keeper := &LeaseKeeper{Service: fx.service, LeaseID: claim.LeaseID, RequestBase: "v2017-i4"}
	leaseVersion := domain.Version(1)
	var lastExpiry time.Time
	tick := func(n int) {
		for i := 0; i < n; i++ {
			fx.clock.Advance(20 * time.Second) // > 1/3 of the 60s LeaseTTL each tick.
			out, err := keeper.Tick(runnerCtx, leaseVersion, claim.FencingToken)
			if err != nil {
				t.Fatalf("lease tick: %v", err)
			}
			if !out.Renewed {
				t.Fatal("lease was not renewed")
			}
			if out.RenewResponse.ExpiresAt.Equal(lastExpiry) {
				t.Fatal("renew did not extend the lease")
			}
			lastExpiry = out.RenewResponse.ExpiresAt
			leaseVersion = out.RenewResponse.Version
		}
	}
	tick(2) // first half of the LeaseTTL span, before the real invocation.

	marker := randomMarker(t)
	packet := livePacket(reqID, incID, fmt.Sprintf("Do not use any tools. Output ONLY the following exact text, with no other words: %s", marker))
	runner := fx.newRunner(t, "V2-017-I4-lease-continuity", claim.ExecutionID)
	_, result, err := runInvocation(ctx, runner, provider.ClaudeAdapter{}, provider.Request{OperationID: claim.ExecutionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("invocation: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("provider run did not succeed: %#v", result)
	}

	tick(2) // second half: total 80s of ticks > the 60s LeaseTTL, spanning the real invocation.

	if _, err := fx.service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "v2017-i4:checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
		t.Fatalf("checkpoint after lease renewal: %v", err)
	}
	resultPermit, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: "v2017-i4:result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil || !resultPermit.Allowed {
		t.Fatalf("result permit: err=%v allowed=%v", err, resultPermit.Allowed)
	}
	accepted, err := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: "v2017-i4:accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: claim.FencingToken, Succeeded: result.Succeeded, Target: target})
	if err != nil {
		t.Fatalf("accept: %v (the checkpoint/lease was not preserved across renewal)", err)
	}
	if accepted.Status != domain.ExecutionSucceeded {
		t.Fatalf("terminal status = %s", accepted.Status)
	}
}

// testI5AndI6 (dp-v2-017 B15): I5 SIGKILLs the real CLI's process group mid
// -invocation before any result is journaled; I6 resumes under a new
// ExecutionID with a strictly greater fencing token, re-running the
// provider exactly once, and the crashed attempt's late AcceptResult is
// rejected.
//
// V2-058 (dp-v2-058 d2/d3/d6, acceptance A8): since V2-058, Service.Claim's
// reclaim branch drives the superseded Execution to ExecutionLost in the
// same transaction that expires its Lease, and domain.MarkExecutionLost
// always advances the Execution's Version -- so the crashed attempt's late
// AcceptResult, replayed with the version it captured before the crash, is
// now rejected earlier and further out, by AcceptResult's outer
// optimistic-concurrency guard with domain.ErrStaleVersion, instead of by
// domain.ValidateExecutionResult's !lease.ActiveAt branch with
// domain.ErrLeaseExpired (see internal/runner/crash_test.go's identical,
// locally-executed pin and its doc comment for the exact mechanism). A
// second probe, re-submitted at the superseded Execution's current version,
// still proves the lease-expiry guard independently.
//
// V2-058 could only change this at the source: it must not start a real
// Provider CLI, so it proved the revision compiled (go vet plus a test
// build of this package) and recorded that in evidence with status
// skipped, never passed. V2-063 then ran this journey for real against
// the live CLI and both probes passed, so the skip is closed: see
// ev-v2-063-provider-live-claude-refresh. That run also re-measured the
// live record's declared key, which V2-045's evidence-key redesign had
// made stale by the record's own declared method.
func (fx *liveFixture) testI5AndI6(t *testing.T) {
	fx.clock.Advance(25 * time.Hour)
	owner, runnerCtx := fx.callers(context.Background())
	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, "v2017-i56", "V2-017 I5/I6: crash resume against a real invocation")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}

	// --- I5: attempt 1, killed mid-flight. ---
	attempt1Claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i56:attempt-1:claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("attempt 1 claim: %v", err)
	}
	attempt1Start, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i56:attempt-1:start", ExecutionID: attempt1Claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("attempt 1 start: %v", err)
	}
	ws1, err := fx.workspace.Create(attempt1Claim.ExecutionID)
	if err != nil {
		t.Fatalf("attempt 1 workspace: %v", err)
	}
	marker1 := randomMarker(t)
	packet1 := livePacket(reqID, incID, fmt.Sprintf("Do not use any tools. Output ONLY the following exact text, with no other words: %s", marker1))
	inv1, err := provider.ClaudeAdapter{}.Build(provider.Request{OperationID: attempt1Claim.ExecutionID, Workspace: ws1, Packet: packet1})
	if err != nil {
		t.Fatalf("attempt 1 build: %v", err)
	}
	runner1 := fx.newRunner(t, "V2-017-I5-sigkill", attempt1Claim.ExecutionID)

	type runOutcome struct {
		raw []byte
		err error
	}
	killCtx, killCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer killCancel()
	resultCh := make(chan runOutcome, 1)
	go func() {
		raw, err := runner1.Run(killCtx, inv1)
		resultCh <- runOutcome{raw: raw, err: err}
	}()

	pid := findChildPID(t, "claude", 10*time.Second)

	// V2-080 A8, first proof: the working directory the Invocation declares
	// is read back FROM THE REAL CHILD. /proc/<pid>/cwd is the kernel's own
	// record of where that process actually is; it is produced by the
	// child's chdir, not computed by this test, and the harness knows only
	// what it declared. It is taken here, inside the window I5 already
	// holds open between finding the live child and signalling it, so no
	// goroutine, timer or sleep is added to obtain it. The mechanism proved
	// on this path is ProcessSupervisor.Dir applied through cmd.Dir, which
	// is what runs when Confine is nil as it is for this whole suite;
	// NamespaceConfinement's cd-inside-the-wrap-script path, which V2-077
	// introduced precisely because cmd.Dir would chdir before unshare runs,
	// is proved separately by I2's confined phase.
	childCWD, childCWDErr := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	selfCWD, selfCWDErr := os.Readlink("/proc/self/cwd")
	switch {
	case childCWDErr != nil:
		t.Logf("A8 UNPROVEN on the cmd.Dir path: the real child's /proc/%d/cwd could not be read: %v", pid, childCWDErr)
	case childCWD != inv1.WorkingDirectory:
		t.Errorf("A8: the real child's working directory is %q but the Invocation declares %q", childCWD, inv1.WorkingDirectory)
	default:
		t.Logf("A8 PROVEN from the real child on the cmd.Dir path: /proc/%d/cwd equals the declared Invocation.WorkingDirectory (%s)", pid, childCWD)
	}
	if selfCWDErr != nil {
		t.Logf("A8 positive control unavailable: the test process's own cwd could not be read: %v", selfCWDErr)
	} else if selfCWD == inv1.WorkingDirectory {
		t.Errorf("A8 positive control failed: the test process's own cwd %q already equals the declared directory, so the equality above could hold without the child having moved at all", selfCWD)
	} else {
		t.Logf("A8 positive control: the test process's own cwd is %s, which differs from the declared directory, so the equality above is not vacuous", selfCWD)
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("sigkill process group %d: %v", pid, err)
	}

	var attempt1Outcome runOutcome
	select {
	case attempt1Outcome = <-resultCh:
	case <-time.After(20 * time.Second):
		t.Fatal("SupervisedInvocationRunner.Run did not return after the process group was SIGKILLed")
	}
	if attempt1Outcome.err == nil {
		t.Fatal("expected a non-nil error from a SIGKILLed invocation")
	}
	waitUntilProcessGone(t, pid, 10*time.Second)

	snap, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var i5Entry *LedgerEntry
	for i := range snap.Entries {
		if snap.Entries[i].Purpose == "V2-017-I5-sigkill" {
			i5Entry = &snap.Entries[i]
		}
	}
	if i5Entry == nil {
		t.Fatal("no ledger entry recorded for I5")
	}
	if i5Entry.State != "reserved" {
		t.Fatalf("I5's ledger entry state = %s, want \"reserved\" (killed invocations are never settled)", i5Entry.State)
	}

	if found, foundErr := func() (bool, error) {
		_, found, err := FindPendingProviderResult(fx.journal, incID)
		return found, err
	}(); foundErr != nil {
		t.Fatal(foundErr)
	} else if found {
		t.Fatal("case A must find no dangling provider result: attempt 1 was killed before journaling one")
	}

	fx.clock.Advance(2 * time.Minute) // > the 60s LeaseTTL: attempt 1's lease is now inactive.

	// --- I6: attempt 2 resumes with a real invocation. ---
	attempt2Claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i56:attempt-2:claim", IncrementID: incID, ExpectedIncrementVersion: attempt1Claim.Version, Target: target})
	if err != nil {
		t.Fatalf("attempt 2 claim: %v", err)
	}
	if attempt2Claim.ExecutionID == attempt1Claim.ExecutionID {
		t.Fatal("resumed attempt reused the crashed Execution id")
	}
	if !(attempt2Claim.FencingToken > attempt1Claim.FencingToken) {
		t.Fatalf("fencing token did not strictly increase: attempt1=%d attempt2=%d", attempt1Claim.FencingToken, attempt2Claim.FencingToken)
	}
	attempt2Start, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i56:attempt-2:start", ExecutionID: attempt2Claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("attempt 2 start: %v", err)
	}
	ws2, err := fx.workspace.Create(attempt2Claim.ExecutionID)
	if err != nil {
		t.Fatalf("attempt 2 workspace: %v", err)
	}
	marker2 := randomMarker(t)
	packet2 := livePacket(reqID, incID, fmt.Sprintf("Do not use any tools. Output ONLY the following exact text, with no other words: %s", marker2))
	runner2 := fx.newRunner(t, "V2-017-I6-resume", attempt2Claim.ExecutionID)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel2()
	raw2, result2, err := runInvocation(ctx2, runner2, provider.ClaudeAdapter{}, provider.Request{OperationID: attempt2Claim.ExecutionID, Workspace: ws2, Packet: packet2})
	if err != nil {
		t.Fatalf("attempt 2 invocation: %v", err)
	}
	if !result2.Succeeded {
		t.Fatalf("attempt 2 provider run did not succeed: %#v", result2)
	}
	digest2, err := WorkPacketDigest(packet2)
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: incID, ExecutionID: attempt2Claim.ExecutionID, WorkPacketDigest: digest2, RawResult: raw2, Succeeded: result2.Succeeded, Checkpoint: result2.Checkpoint}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "v2017-i56:attempt-2:checkpoint", ExecutionID: attempt2Claim.ExecutionID, LeaseID: attempt2Claim.LeaseID, FencingToken: attempt2Claim.FencingToken}); err != nil {
		t.Fatalf("attempt 2 checkpoint: %v", err)
	}
	resultPermit2, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: "v2017-i56:attempt-2:result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: attempt2Claim.FencingToken, ExpectedFencingToken: attempt2Claim.FencingToken, Resource: attempt2Claim.ExecutionID})
	if err != nil || !resultPermit2.Allowed {
		t.Fatalf("attempt 2 result permit: err=%v allowed=%v", err, resultPermit2.Allowed)
	}
	accepted2, err := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: "v2017-i56:attempt-2:accept", ExecutionID: attempt2Claim.ExecutionID, LeaseID: attempt2Claim.LeaseID, ExpectedExecutionVersion: attempt2Start.Version, FencingToken: attempt2Claim.FencingToken, Succeeded: result2.Succeeded, Target: target})
	if err != nil {
		t.Fatalf("attempt 2 accept: %v", err)
	}
	if accepted2.Status != domain.ExecutionSucceeded {
		t.Fatalf("attempt 2 terminal status = %s", accepted2.Status)
	}
	if err := JournalResultAccepted(fx.journal, attempt2Claim.ExecutionID, accepted2.Status); err != nil {
		t.Fatal(err)
	}

	// The crashed attempt's own late AcceptResult, replayed with its own
	// (now stale) lease/version/fencing token, is rejected. Since V2-058
	// this is domain.ErrStaleVersion (the outer optimistic-concurrency
	// guard), not domain.ErrLeaseExpired: see this function's doc comment
	// and internal/runner/crash_test.go's identical, locally-executed pin
	// for the exact mechanism. Exercised for real by V2-063 against the
	// live CLI; the second probe below pins the lease-expiry guard.
	_, lateErr := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{
		RequestID: "v2017-i56:attempt-1:late-accept", ExecutionID: attempt1Claim.ExecutionID, LeaseID: attempt1Claim.LeaseID,
		ExpectedExecutionVersion: attempt1Start.Version, FencingToken: attempt1Claim.FencingToken, Succeeded: true, Target: target,
	})
	if !errors.Is(lateErr, domain.ErrStaleVersion) {
		t.Fatalf("crashed execution's late AcceptResult should be rejected with domain.ErrStaleVersion, got %v", lateErr)
	}

	// Second probe (dp-v2-058 d3): re-read the superseded Execution's
	// current version and resubmit with it, under its own RequestID, which
	// still reaches domain.ValidateExecutionResult's !lease.ActiveAt branch
	// and must still be rejected with domain.ErrLeaseExpired.
	var attempt1Current domain.Execution
	if err := fx.store.Transact(context.Background(), func(u application.UnitOfWork) error {
		v, ok, err := u.Execution(context.Background(), attempt1Claim.ExecutionID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("superseded execution %s not found", attempt1Claim.ExecutionID)
		}
		attempt1Current = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, secondErr := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{
		RequestID: "v2017-i56:attempt-1:late-accept-current-version", ExecutionID: attempt1Claim.ExecutionID, LeaseID: attempt1Claim.LeaseID,
		ExpectedExecutionVersion: attempt1Current.Version, FencingToken: attempt1Claim.FencingToken, Succeeded: true, Target: target,
	})
	if !errors.Is(secondErr, domain.ErrLeaseExpired) {
		t.Fatalf("crashed execution's late AcceptResult at the current version should still be rejected with domain.ErrLeaseExpired, got %v", secondErr)
	}
}

// testI7 (dp-v2-017 B16): a real CLI failure induced without an API call by
// pointing the CLI at an unreachable base URL. This exercises transport
// failure specifically, never model failure or quota failure (M6's V2-028
// owns the per-Provider success/error/quota/cancel matrix).
func (fx *liveFixture) testI7(t *testing.T) {
	fx.clock.Advance(25 * time.Hour)
	owner, runnerCtx := fx.callers(context.Background())
	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, "v2017-i7", "V2-017 I7: real transport failure (unreachable base URL)")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}
	claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i7:claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ws, err := fx.workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	startResp, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i7:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	packet := livePacket(reqID, incID, "Do not use any tools. Output ONLY the text: OK")
	runner := fx.newRunner(t, "V2-017-I7-transport-failure", claim.ExecutionID)
	// ANTHROPIC_BASE_URL is a diagnostic-only override for this one
	// invocation (dp-v2-017 B16/I7); it is not part of environment.base_names
	// and no other invocation in this task uses ExtraEnv.
	runner.ExtraEnv = []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:1"}

	// Empirically, an unreachable base URL causes the real CLI to hang
	// (measured: still unresponsive after 90s with no output at all)
	// rather than failing fast, so this invocation is bounded by a real
	// context deadline rather than relying on the CLI to give up on its
	// own. The deadline firing (context.DeadlineExceeded, not
	// context.Canceled) is deliberately NOT treated as a
	// cancel-and-never-settle case: Run only skips settlement for an
	// explicit cancel (I8) or a real external signal (I5); a deadline
	// timeout still falls through to classification, matching B16's
	// requirement that this failure IS classified through the adapter.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	raw, result, err := runInvocation(ctx, runner, provider.ClaudeAdapter{}, provider.Request{OperationID: claim.ExecutionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("Run itself must not return an error for a classified failure (dp-v2-017 InvocationRunner contract); got %v", err)
	}
	if result.Succeeded {
		t.Fatal("expected the transport failure to classify as unsuccessful")
	}
	if result.Failure == nil {
		t.Fatal("expected a non-nil Failure")
	}
	if result.Failure.Class != provider.FailureTransport {
		t.Fatalf("expected FailureTransport, got %s", result.Failure.Class)
	}

	digest, err := WorkPacketDigest(packet)
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalProviderPending(fx.journal, PendingProviderRecord{IncrementID: incID, ExecutionID: claim.ExecutionID, WorkPacketDigest: digest, RawResult: raw, Succeeded: false, Checkpoint: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.service.Checkpoint(runnerCtx, application.CheckpointRequest{RequestID: "v2017-i7:checkpoint", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, FencingToken: claim.FencingToken}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	resultPermit, err := fx.service.Permit(runnerCtx, application.PermitRequest{RequestID: "v2017-i7:result-permit", Kind: domain.PermitExternalEffect, Target: target, FencingToken: claim.FencingToken, ExpectedFencingToken: claim.FencingToken, Resource: claim.ExecutionID})
	if err != nil || !resultPermit.Allowed {
		t.Fatalf("result permit: err=%v allowed=%v", err, resultPermit.Allowed)
	}
	accepted, err := fx.service.AcceptResult(runnerCtx, application.AcceptResultRequest{RequestID: "v2017-i7:accept", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: claim.FencingToken, Succeeded: false, Target: target})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if accepted.Status != domain.ExecutionFailed {
		t.Fatalf("terminal status = %s, want %s (no retry converts this to green)", accepted.Status, domain.ExecutionFailed)
	}
	if err := JournalResultAccepted(fx.journal, claim.ExecutionID, accepted.Status); err != nil {
		t.Fatal(err)
	}
	t.Logf("I7: classified as %s (transport), execution ended %s, no API call was reached (unreachable base URL)", result.Failure.Class, accepted.Status)
}

// testI8 (dp-v2-017 B17): context cancellation during a real invocation.
// The process group receives TERM then KILL, the classification is
// FailureCancelled, no result is journaled, and the ledger entry remains
// reserved at worst case.
func (fx *liveFixture) testI8(t *testing.T) {
	fx.clock.Advance(25 * time.Hour)
	owner, runnerCtx := fx.callers(context.Background())
	reqID, incID, preparedVersion := fx.capturePlanPrepare(t, owner, "v2017-i8", "V2-017 I8: real cancellation mid-invocation")
	target := domain.ControlTarget{RequirementID: mustRequirement(reqID), IncrementID: mustIncrement(incID)}
	claim, err := fx.service.Claim(runnerCtx, application.ClaimRequest{RequestID: "v2017-i8:claim", IncrementID: incID, ExpectedIncrementVersion: preparedVersion, Target: target})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ws, err := fx.workspace.Create(claim.ExecutionID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if _, err := fx.service.Start(runnerCtx, application.StartRequest{RequestID: "v2017-i8:start", ExecutionID: claim.ExecutionID, ExpectedExecutionVersion: 1}); err != nil {
		t.Fatalf("start: %v", err)
	}

	marker := randomMarker(t)
	packet := livePacket(reqID, incID, fmt.Sprintf("Do not use any tools. Output ONLY the following exact text, with no other words: %s", marker))
	inv, err := provider.ClaudeAdapter{}.Build(provider.Request{OperationID: claim.ExecutionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runner := fx.newRunner(t, "V2-017-I8-cancel", claim.ExecutionID)
	runner.Supervisor.TermGrace = 500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	type runOutcome struct {
		err error
	}
	resultCh := make(chan runOutcome, 1)
	go func() {
		_, err := runner.Run(ctx, inv)
		resultCh <- runOutcome{err: err}
	}()

	pid := findChildPID(t, "claude", 10*time.Second)
	cancel()

	var outcome runOutcome
	select {
	case outcome = <-resultCh:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if outcome.err == nil {
		t.Fatal("expected a non-nil error from a cancelled invocation")
	}
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("expected the error to wrap context.Canceled, got %v", outcome.err)
	}
	classification := provider.ClassifyError(outcome.err)
	if classification.Class != provider.FailureCancelled {
		t.Fatalf("expected FailureCancelled, got %s", classification.Class)
	}
	waitUntilProcessGone(t, pid, 10*time.Second)

	if found, err := func() (bool, error) { _, found, err := FindPendingProviderResult(fx.journal, incID); return found, err }(); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("no result should ever be journaled for a cancelled invocation")
	}

	snap, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var i8Entry *LedgerEntry
	for i := range snap.Entries {
		if snap.Entries[i].Purpose == "V2-017-I8-cancel" {
			i8Entry = &snap.Entries[i]
		}
	}
	if i8Entry == nil {
		t.Fatal("no ledger entry recorded for I8")
	}
	if i8Entry.State != "reserved" {
		t.Fatalf("I8's ledger entry state = %s, want \"reserved\"", i8Entry.State)
	}
	t.Logf("I8: classification=%s, ledger entry stays reserved at %.2f USD", classification.Class, i8Entry.ReservedUSD)
}

// testCaseB (dp-v2-017 B15 case B): crash-resume case B reuses I1's
// journaled projection and must consume zero additional invocations. I1
// itself completed its own AcceptResult for real (it is not left dangling
// -- that would defeat I1's own acceptance), so this proof does not use
// FindPendingProviderResult (which correctly excludes already-accepted
// executions by design, matching crash_test.go's dangling-record
// semantics). Instead it (a) confirms I1's result_pending event still
// physically exists in the durable journal by replaying it directly, and
// (b) re-parses I1's own projected bytes (captured once, directly from
// I1's own Run call, never a second invocation) exactly as a resumed
// Execution would, asserting the ledger's invocation count does not move.
func (fx *liveFixture) testCaseB(t *testing.T) {
	if fx.i1IncrementID == "" || fx.i1RawResult == nil {
		t.Skip("I1 did not complete; case B has nothing to reuse")
	}
	before, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	events, err := fx.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	foundPending := false
	for _, e := range events {
		if e.Kind == "result_pending" && strings.Contains(string(e.Payload), fx.i1ExecutionID) {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatal("I1's result_pending event is not present in the durable journal")
	}

	// The digest-binding check itself is exercised by I5/I6's real
	// crash-resume case A; case B's own contribution is the
	// zero-additional-invocation reuse asserted below.
	reparsed, err := (provider.ClaudeAdapter{}).Parse(fx.i1RawResult)
	if err != nil {
		t.Fatalf("recovered payload failed to re-parse: %v", err)
	}
	if !reparsed.Succeeded || reparsed.OutputDigest == "" {
		t.Fatalf("recovered payload did not re-parse to a successful result: %#v", reparsed)
	}

	after, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if after.InvocationCount != before.InvocationCount {
		t.Fatalf("case B must consume zero additional invocations: before=%d after=%d", before.InvocationCount, after.InvocationCount)
	}
	t.Logf("case B: re-adopted I1's recovered checkpoint (%s) with zero additional invocations (ledger count stayed at %d)", reparsed.Checkpoint, after.InvocationCount)
}

// logLedgerSnapshot (dp-v2-017 B18) logs the full, redacted per-entry
// ledger table for evidence transcription: sequence/purpose/state/reserved/
// actual/session/timestamps. No prompt or response text is ever in scope
// here -- the ledger schema structurally cannot carry either.
func (fx *liveFixture) logLedgerSnapshot(t *testing.T) {
	snap, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger snapshot: provider=%s task_id=%s halted=%v invocation_count=%d settled_total_usd=%.4f reserved_total_usd=%.4f",
		snap.Provider, snap.TaskID, snap.Halted, snap.InvocationCount, snap.SettledTotalUSD, snap.ReservedTotalUSD)
	for _, e := range snap.Entries {
		t.Logf("  seq=%d purpose=%s state=%s reserved_usd=%.4f actual_usd=%.4f session_id_present=%v started_at=%s finished_at=%s",
			e.Sequence, e.Purpose, e.State, e.ReservedUSD, e.ActualUSD, e.SessionID != "", e.StartedAt.Format(time.RFC3339), e.FinishedAt.Format(time.RFC3339))
	}
	// The two stops below are the APPROVED RECORD's own declared limits,
	// read from the record this run loaded, not literals carried over from
	// whichever record the suite was first written against. Until V2-080
	// they were hard-coded as 16 invocations and 10.00 USD -- V2-017's
	// values -- which would have made this subtest fail by construction
	// under any later record with different limits. Reading the record is
	// not raising a limit: no threshold is changed, and reaching one is
	// still a fail-closed stop for inspection.
	if snap.InvocationCount > fx.record.Limits.MaxInvocations {
		t.Fatalf("invocation_count %d exceeds the approved record's max_invocations %d", snap.InvocationCount, fx.record.Limits.MaxInvocations)
	}
	if snap.SettledTotalUSD+snap.ReservedTotalUSD > fx.record.Limits.MaxTotalCostUSD {
		t.Fatalf("settled+outstanding %.4f exceeds the approved record's max_total_cost_usd %.2f", snap.SettledTotalUSD+snap.ReservedTotalUSD, fx.record.Limits.MaxTotalCostUSD)
	}
}
