package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// secretBrokerFixture wires a fresh application.Service plus a claimed
// increment/execution/lease so each fail-closed test below has a valid
// baseline scope to mutate exactly one field of.
type secretBrokerFixture struct {
	service      *application.Service
	store        *memory.Store
	clock        *mutableClock
	runnerCtx    context.Context
	target       domain.ControlTarget
	executionID  string
	leaseID      string
	fencingToken domain.FencingToken
}

func newSecretBrokerFixture(t *testing.T) *secretBrokerFixture {
	t.Helper()
	clock := newMutableClock(time.Unix(1_700_100_000, 0).UTC())
	st := memory.New()
	service, err := application.NewServiceWithConfig(st, clock, &journeyIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	owner := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
	runnerCtx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner-1", RunnerID: "runner-1"})

	cap, err := service.Capture(owner, application.CaptureRequest{RequestID: "sb:capture", Text: "secret broker fixture"})
	if err != nil {
		t.Fatal(err)
	}
	// V2-089: this fixture claims, so its parent Requirement is moved to
	// domain.RequirementReady -- '優先順位評価済みで実行可能',
	// docs/architecture/domain-model.md:265 -- before the Plan, and the Plan
	// carries the POST-seed version.
	readyVersion := runnerSeedRequirementStatus(t, st, owner, cap.RequirementID, domain.RequirementReady)
	plan, err := service.Plan(owner, application.PlanRequest{RequestID: "sb:plan", RequirementID: cap.RequirementID, ExpectedRequirementVersion: readyVersion})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(owner, application.PrepareRequest{RequestID: "sb:prepare", IncrementID: plan.IncrementID, ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.ControlTarget{InstallationID: "install", RequirementID: mustRequirement(cap.RequirementID), IncrementID: mustIncrement(plan.IncrementID)}
	claim, err := service.Claim(runnerCtx, application.ClaimRequest{RequestID: "sb:claim", IncrementID: plan.IncrementID, ExpectedIncrementVersion: prepared.Version, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	return &secretBrokerFixture{service: service, store: st, clock: clock, runnerCtx: runnerCtx, target: target, executionID: claim.ExecutionID, leaseID: claim.LeaseID, fencingToken: claim.FencingToken}
}

func (fx *secretBrokerFixture) baseScope(executionID string) Scope {
	return Scope{
		ExecutionID:          executionID,
		Repository:           "owner/repo",
		Provider:             "codex",
		Target:               fx.target,
		FencingToken:         fx.fencingToken,
		ExpectedFencingToken: fx.fencingToken,
		ExpiresAt:            fx.clock.Now().Add(time.Minute),
	}
}

func genSecretValue(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "sk-" + hex.EncodeToString(b)
}

// --- A8: five independent fail-closed refusals, each its own assertion. ---

func TestSecretBrokerRefusesAlreadyExpiredScope(t *testing.T) {
	fx := newSecretBrokerFixture(t)
	secret := genSecretValue(t)
	broker := NewSecretBroker(fx.service, MapCredentialSource{"CODEX_TOKEN": secret}, map[string][]string{"codex": {"CODEX_TOKEN"}}, fx.clock.Now)
	scope := fx.baseScope(fx.executionID)
	scope.ExpiresAt = fx.clock.Now() // at now, not after now: expired.
	if _, err := broker.Lease(fx.runnerCtx, scope, []string{"CODEX_TOKEN"}); !errors.Is(err, ErrSecretExpired) {
		t.Fatalf("expected ErrSecretExpired, got %v", err)
	}
}

func TestSecretBrokerRefusesUnderEffectiveStop(t *testing.T) {
	fx := newSecretBrokerFixture(t)
	owner := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
	ctrl, err := fx.service.Control(owner, application.ControlRequest{RequestID: "sb:stop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlEmergencyStop, Reason: "test emergency stop", At: fx.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	secret := genSecretValue(t)
	broker := NewSecretBroker(fx.service, MapCredentialSource{"CODEX_TOKEN": secret}, map[string][]string{"codex": {"CODEX_TOKEN"}}, fx.clock.Now)
	scope := fx.baseScope(fx.executionID)
	scope.ControlRevision = ctrl.Revision
	if _, err := broker.Lease(fx.runnerCtx, scope, []string{"CODEX_TOKEN"}); !errors.Is(err, ErrSecretDenied) || !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("expected ErrSecretDenied wrapping domain.ErrControlDenied, got %v", err)
	}
}

func TestSecretBrokerRefusesCredentialNameOutsideProviderAllowlist(t *testing.T) {
	fx := newSecretBrokerFixture(t)
	secret := genSecretValue(t)
	broker := NewSecretBroker(fx.service, MapCredentialSource{"CODEX_TOKEN": secret, "NOT_ALLOWED": secret}, map[string][]string{"codex": {"CODEX_TOKEN"}}, fx.clock.Now)
	scope := fx.baseScope(fx.executionID)
	if _, err := broker.Lease(fx.runnerCtx, scope, []string{"NOT_ALLOWED"}); !errors.Is(err, ErrSecretNotAllowed) {
		t.Fatalf("expected ErrSecretNotAllowed, got %v", err)
	}
}

func TestSecretBrokerRefusesZeroOrMismatchedFencingToken(t *testing.T) {
	fx := newSecretBrokerFixture(t)
	secret := genSecretValue(t)
	broker := NewSecretBroker(fx.service, MapCredentialSource{"CODEX_TOKEN": secret}, map[string][]string{"codex": {"CODEX_TOKEN"}}, fx.clock.Now)

	zero := fx.baseScope(fx.executionID)
	zero.FencingToken = 0
	zero.ExpectedFencingToken = 0
	if _, err := broker.Lease(fx.runnerCtx, zero, []string{"CODEX_TOKEN"}); !errors.Is(err, ErrSecretFencingRequired) {
		t.Fatalf("zero fencing token: expected ErrSecretFencingRequired, got %v", err)
	}

	mismatched := fx.baseScope(fx.executionID)
	mismatched.ExpectedFencingToken = fx.fencingToken + 1
	if _, err := broker.Lease(fx.runnerCtx, mismatched, []string{"CODEX_TOKEN"}); !errors.Is(err, ErrSecretFencingMismatch) {
		t.Fatalf("mismatched fencing token: expected ErrSecretFencingMismatch, got %v", err)
	}
}

func TestSecretBrokerRefusesSecondUseOfConsumedOrRevokedGrant(t *testing.T) {
	fx := newSecretBrokerFixture(t)
	secret := genSecretValue(t)
	broker := NewSecretBroker(fx.service, MapCredentialSource{"CODEX_TOKEN": secret}, map[string][]string{"codex": {"CODEX_TOKEN"}}, fx.clock.Now)

	scope := fx.baseScope(fx.executionID)
	if _, err := broker.Lease(fx.runnerCtx, scope, []string{"CODEX_TOKEN"}); err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := broker.Lease(fx.runnerCtx, scope, []string{"CODEX_TOKEN"}); !errors.Is(err, ErrSecretGrantConsumed) {
		t.Fatalf("expected ErrSecretGrantConsumed on second use, got %v", err)
	}

	// A distinct execution id proves revocation is checked independently of
	// consumption: it has never been leased, only revoked.
	otherScope := fx.baseScope(fx.executionID + "-other")
	broker.Revoke(otherScope.ExecutionID)
	if _, err := broker.Lease(fx.runnerCtx, otherScope, []string{"CODEX_TOKEN"}); !errors.Is(err, ErrSecretGrantRevoked) {
		t.Fatalf("expected ErrSecretGrantRevoked, got %v", err)
	}
}

// TestSecretBrokerTwoChannelsAreSeparate proves the guarded base environment
// (GuardEnvironment) is untouched by the broker's existence: the same
// rejection GuardEnvironment always produced still fires, because the two
// channels are structurally different code paths.
func TestSecretBrokerTwoChannelsAreSeparate(t *testing.T) {
	if err := GuardEnvironment([]string{"API_TOKEN=abc"}, map[string]bool{"API_TOKEN": true}); err == nil {
		t.Fatal("guarded base environment channel accepted a secret-shaped variable")
	}
}

// --- A11: runtime credential non-leakage with a presence positive control. ---

func TestSecretBrokerCredentialNeverLeaksExceptIntoOneInvocationEnvironment(t *testing.T) {
	fx := newSecretBrokerFixture(t)
	dataRoot := t.TempDir()
	secret := genSecretValue(t) // crypto/rand-derived fixture; never a real credential, never read from env or disk outside t.TempDir().
	secretBytes := []byte(secret)

	broker := NewSecretBroker(fx.service, MapCredentialSource{"CODEX_TOKEN": secret}, map[string][]string{"codex": {"CODEX_TOKEN"}}, fx.clock.Now)
	scope := fx.baseScope(fx.executionID)
	grant, err := broker.Lease(fx.runnerCtx, scope, []string{"CODEX_TOKEN"})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}

	packet := provider.WorkPacket{Version: provider.ContractVersion, RequirementID: "req-1", RequirementSummary: "verify secret material never leaks", IncrementID: "inc-1"}
	workspace := t.TempDir()
	inv, err := provider.CodexAdapter{}.Build(provider.Request{OperationID: fx.executionID, Workspace: workspace, Packet: packet})
	if err != nil {
		t.Fatal(err)
	}

	runner := &FakeInvocationRunner{Fixture: []byte(`{"status":"success","checkpoint":"cp-1"}`)}
	raw, err := runner.Run(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.CodexAdapter{}.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	// Journal a provider-pending record and a bounded log line, exactly as
	// the runner's real pipeline would, from data that never touched the
	// credential.
	journal, err := OpenJournal(filepath.Join(dataRoot, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := WorkPacketDigest(packet)
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalProviderPending(journal, PendingProviderRecord{IncrementID: "inc-1", ExecutionID: fx.executionID, WorkPacketDigest: digest, RawResult: raw, Succeeded: result.Succeeded, Checkpoint: result.Checkpoint}); err != nil {
		t.Fatal(err)
	}

	boundedLog, err := NewBoundedLog(dataRoot, fx.executionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := boundedLog.Write(string(raw)); err != nil {
		t.Fatal(err)
	}

	// Accept the result canonically and collect every response the runner
	// received along the way, marshalled exactly as it would be to reach any
	// durable or observable form.
	startResp, err := fx.service.Start(fx.runnerCtx, application.StartRequest{RequestID: "sb:start", ExecutionID: fx.executionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := fx.service.AcceptResult(fx.runnerCtx, application.AcceptResultRequest{RequestID: "sb:accept", ExecutionID: fx.executionID, LeaseID: fx.leaseID, ExpectedExecutionVersion: startResp.Version, FencingToken: fx.fencingToken, Succeeded: result.Succeeded, Target: fx.target})
	if err != nil {
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

	journalBytes, err := os.ReadFile(filepath.Join(dataRoot, "journal", "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(boundedLog.Path())
	if err != nil {
		t.Fatal(err)
	}
	packetBytes, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}

	// Absence: the secret's bytes appear in none of these.
	absenceTargets := map[string][]byte{
		"journal file":           journalBytes,
		"marshalled work packet": packetBytes,
		"bounded log file":       logBytes,
		"canonical store events": eventsJSON,
		"accepted result":        acceptedJSON,
	}
	for name, data := range absenceTargets {
		if bytes.Contains(data, secretBytes) {
			t.Fatalf("credential leaked into %s", name)
		}
	}
	for i, arg := range os.Args {
		if bytes.Contains([]byte(arg), secretBytes) {
			t.Fatalf("credential leaked into process argv[%d]", i)
		}
	}
	for i, arg := range runner.Calls[0].Argv {
		if bytes.Contains([]byte(arg), secretBytes) {
			t.Fatalf("credential leaked into invocation argv[%d]", i)
		}
	}

	// Presence (positive control): a search that never finds anything proves
	// nothing by itself. The credential MUST appear in the one channel it is
	// supposed to reach -- the leased Grant's own environment.
	//
	// HISTORICAL MEASUREMENT, 2026-08-25 (V2-077): this control read
	// runner.Calls[0].Environment, i.e. the credential after Grant.Apply had
	// merged it onto the Invocation and FakeInvocationRunner had recorded it.
	//
	// CURRENT MEASUREMENT, 2026-08-26 (V2-078): Grant.Apply,
	// ProviderClient.Grant and provider.Invocation.Environment are deleted
	// (dp-v2-078 route (b)), because that merge reached a test fake and never
	// a process. The control now reads grant.Environment() -- the same value
	// one hop earlier, at exactly the same strength: its only job is to prove
	// the crypto/rand fixture this test generated is genuinely the value the
	// broker produced, so that the absence scans above are over the right
	// bytes. Every absence target and both argv scans are unchanged.
	//
	// The test's NAME is unchanged deliberately, and "one invocation
	// environment" now names the one leased Grant environment destined for
	// one invocation -- the CHANNEL, not a struct field. The name is load
	// bearing outside this package: internal/reconciler's failure matrix
	// carries it as a string literal for the "secret-like fixture in commit,
	// log, Provider outbound" row of the failure model.
	found := false
	for _, e := range grant.Environment() {
		if bytes.Contains([]byte(e), secretBytes) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("positive control failed: the credential did not appear in the leased grant's own environment; the absence search above is vacuous")
	}
}
