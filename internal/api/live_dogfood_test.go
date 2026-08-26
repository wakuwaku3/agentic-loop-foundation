package api_test

// TestFoundationPreviewLocalDogfood is V2-022's preview-local (release-
// contract.md section 3) dogfood exercise: this Foundation Repository is
// driven as a Preview target on the owner's own machine, with a real
// cmd/control-plane process on 127.0.0.1, a real Firestore emulator, a real
// enrolled Runner session over real HTTP, and the real claude CLI as a real
// child process group.
//
// It is gated on AGENTIC_LOOP_LIVE_DOGFOOD=1 (plus AGENTIC_LOOP_LIVE_PROVIDER=1
// for the sub-exercises that invoke the real CLI) and skips otherwise, so
// `devbox run --pure -- make check` starts zero processes and makes zero
// provider invocations. With the gate set but a prerequisite missing it
// FAILS rather than skipping, so a half-configured live run can never be
// counted as having exercised anything.
//
//	devbox run --pure -e AGENTIC_LOOP_LIVE_DOGFOOD=1 -e AGENTIC_LOOP_LIVE_PROVIDER=1 \
//	  -e HOME=/home/takushi -- scripts/firestore-emulator.sh \
//	  go test -count=1 -v -timeout 3600s -run TestFoundationPreviewLocalDogfood ./internal/api
//
// The four conditions the roadmap defers to the initial deploy gate (D1) are
// out of scope and are never asserted here: the IAP authentication boundary,
// idle scale-to-zero, real Firestore permissions/contention, and the
// approved-plan-digest deploy path. Nothing here reaches Google Cloud, and
// nothing here reaches a forge: no GitHub object is read, created or
// modified by this test.
//
// It composes exactly as V2-051's internal/api/live_local_test.go does and
// reuses that file's helpers (repoRootForLiveTest, freeLocalPort,
// waitForHealthz, liveCall, backlogContains, verifyEmulatorHasRealDocuments,
// exhaustWriteBudgetOrFail). That file is not edited and nothing is moved
// out of it.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	cloudfirestore "cloud.google.com/go/firestore"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
	"github.com/takushi/agentic-loop-foundation/v2/internal/release"
	agenticrunner "github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	agenticfirestore "github.com/takushi/agentic-loop-foundation/v2/internal/store/firestore"
	"github.com/takushi/agentic-loop-foundation/v2/internal/update"
)

const (
	dogfoodGate         = "AGENTIC_LOOP_LIVE_DOGFOOD"
	dogfoodProviderGate = "AGENTIC_LOOP_LIVE_PROVIDER"
	dogfoodRecordRel    = ".agents/v2/provider-preflight/V2-022-provider-live-claude-dogfood.json"
	dogfoodTaskID       = "V2-022"
	dogfoodRepositoryID = "agentic-loop-foundation"
	dogfoodEnvClass     = "preview-local"
)

// dogfoodEligible is dp-v2-022 d2's eligible set as this run MEASURED it: the
// capabilities whose declared success condition was observed in full at this
// grade AND whose declared external_dependencies were all actually connected
// to. dp-v2-022 d2 predicted three; two of those three were measured failing
// against their own declared success condition and are recorded as such:
//
//	cap-backlog-visibility  the Backlog cannot be paged past its first page
//	                        against a real Firestore (E22-11), so a working
//	                        cursor -- part of the declared condition -- is
//	                        absent.
//	cap-user-documentation  the owner console, its ONLY declared external
//	                        system, serves no document route (E22-10), and the
//	                        capability document itself disagrees with the
//	                        running process about /v1/release/state.
//
// This list is the upper bound the promotability measurement is allowed to
// see: a capability outside it carrying an evidence id is a hard failure.
var dogfoodEligible = []string{"cap-requirement-intake"}

// dogfoodIdentifiers is the release-contract.md section 3 environment
// identifier set every capability check carries.
type dogfoodIdentifiers struct {
	EnvironmentClass string
	Hostname         string
	Kernel           string
	EmulatorName     string
	EmulatorVersion  string
	EmulatorProject  string
	EmulatorHost     string
	ClaudePath       string
	ClaudeVersion    string
	GoVersion        string
	SessionIDs       []string
}

func (id dogfoodIdentifiers) String() string {
	return fmt.Sprintf("class=%s machine=%s kernel=%q emulator=%s/%s project=%s host=%s claude=%s/%s go=%s",
		id.EnvironmentClass, id.Hostname, id.Kernel, id.EmulatorName, id.EmulatorVersion,
		id.EmulatorProject, id.EmulatorHost, id.ClaudePath, id.ClaudeVersion, id.GoVersion)
}

type dogfood struct {
	repoRoot     string
	goodBin      string
	installation string
	ownerToken   string
	ownerEmail   string
	client       *http.Client
	base         string
	stopBase     func()
	ids          dogfoodIdentifiers
	recordPath   string
	record       agenticrunner.PreflightRecord
	ledger       *agenticrunner.CostLedger
	dataRoot     string
	workspace    *agenticrunner.Workspace
	runnerToken  string
	runnerID     string

	// controlRevision is the revision of the control this run last made
	// effective. Every runner-facing call carries it, because domain.Permit
	// requires the caller's control_revision to be exactly the effective one.
	controlRevision int

	// Observations later sub-exercises depend on.
	intakeRequirementID string
	capturedIDs         []string
	rollbackObserved    bool
	resumeObserved      bool
	brokenDefect        string
	brokenVersion       string
	goodVersion         string
	invocationPurposes  []string
}

// dogfoodCall is liveCall with an arbitrary header set, which the runner
// session header (X-Agentic-Runner-Session) needs and Authorization cannot
// carry.
func dogfoodCall(t *testing.T, client *http.Client, method, url string, headers map[string]string, payload any) liveResponse {
	t.Helper()
	var body strings.Reader
	raw := ""
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		raw = string(b)
	}
	body = *strings.NewReader(raw)
	req, err := http.NewRequest(method, url, &body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out := liveResponse{status: resp.StatusCode, header: resp.Header}
	if resp.ContentLength != 0 {
		_ = json.NewDecoder(resp.Body).Decode(&out.body)
	}
	return out
}

func (fx *dogfood) owner() map[string]string {
	return map[string]string{"Authorization": "Bearer " + fx.ownerToken}
}

func (fx *dogfood) runner() map[string]string {
	return map[string]string{"X-Agentic-Runner-Session": fx.runnerToken}
}

// startDogfoodProcess starts a real cmd/control-plane process in its own
// process group through agenticrunner.ProcessSupervisor and returns its base
// URL, an aliveness probe, and an explicit stop function so a sub-exercise
// can prove persistence survives process death rather than assuming it.
func startDogfoodProcess(t *testing.T, binPath, installation string, port int, ownerToken, ownerEmail string) (base string, alive func() bool, stop func()) {
	t.Helper()
	env := append(os.Environ(),
		"PORT="+strconv.Itoa(port),
		"INSTALLATION_ID="+installation,
		"GCP_PROJECT_ID="+liveLocalEmulatorProject,
		"OWNER_EMAILS="+ownerEmail,
		"OWNER_ORIGINS=https://owner.invalid",
		"RECONCILE_IDENTITY=reconciler@example.iam.gserviceaccount.com",
		"AGENTIC_LOOP_ALLOW_FIRESTORE_EMULATOR=1",
		"AGENTIC_LOOP_LOCAL_OWNER_TOKENS="+ownerToken+"="+ownerEmail,
	)
	ctx, cancel := context.WithCancel(context.Background())
	supervisor := agenticrunner.ProcessSupervisor{TermGrace: 5 * time.Second, Env: env}
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		err := supervisor.Run(ctx, []string{binPath})
		close(exited)
		done <- err
	}()
	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("control-plane process did not exit within 15s of stop; process group may be leaked")
		}
	}
	t.Cleanup(stop)
	aliveFn := func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port), aliveFn, stop
}

// dogfoodPoll polls an observable condition on a bounded deadline. It is the
// only waiting primitive this file uses: no fixed sleep of its own, no timer,
// no goroutine of its own beyond the supervised child, and no dependence on
// another test's order.
func dogfoodPoll(t *testing.T, what string, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		if cond() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("bounded poll for %s did not observe its condition within %s", what, deadline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustHostname is the machine identifier release-contract.md section 3 asks
// for. It is gethostname(2), which is exactly what the hostname(1) command
// prints; that command is not in the devbox --pure PATH, so the syscall is
// read directly rather than shelling out to a binary that is absent.
func mustHostname(t *testing.T) string {
	t.Helper()
	name, err := os.Hostname()
	if err != nil {
		t.Fatalf("read the machine identifier (gethostname): %v", err)
	}
	return name
}

func mustCommandOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestFoundationPreviewLocalDogfood(t *testing.T) {
	if os.Getenv(dogfoodGate) != "1" {
		t.Logf("live dogfood gate: %s=%q, want \"1\"", dogfoodGate, os.Getenv(dogfoodGate))
		t.Skip("preview-local dogfood exercise is disabled; it starts zero processes and makes zero provider invocations")
	}
	// Past this point every unmet prerequisite is a Fatal, never a Skip
	// (gate rule G7: a skipped test is not a pass).
	emulatorHost := strings.TrimSpace(os.Getenv("FIRESTORE_EMULATOR_HOST"))
	if emulatorHost == "" {
		t.Fatalf("%s=1 but FIRESTORE_EMULATOR_HOST is unset; wrap this invocation in scripts/firestore-emulator.sh", dogfoodGate)
	}
	if os.Getenv(dogfoodProviderGate) != "1" {
		t.Fatalf("%s=1 requires %s=1: the dogfood exercise makes real claude invocations under %s and must not silently omit them", dogfoodGate, dogfoodProviderGate, dogfoodRecordRel)
	}

	repoRoot := repoRootForLiveTest(t)
	recordPath := filepath.Join(repoRoot, dogfoodRecordRel)
	record, err := agenticrunner.LoadPreflightRecord(repoRoot, recordPath)
	if err != nil {
		t.Fatalf("the V2-022 provider-preflight record at %s does not load/validate: %v", dogfoodRecordRel, err)
	}
	if record.LedgerPath == "" || !filepath.IsAbs(record.LedgerPath) {
		t.Fatalf("limits.ledger_path %q is not an absolute path", record.LedgerPath)
	}
	if err := os.MkdirAll(filepath.Dir(record.LedgerPath), 0o700); err != nil {
		t.Fatalf("ledger directory is not writable: %v", err)
	}
	probe := filepath.Join(filepath.Dir(record.LedgerPath), ".v2-022-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		t.Fatalf("ledger path is not writable: %v", err)
	}
	_ = os.Remove(probe)

	fx := &dogfood{
		repoRoot:     repoRoot,
		installation: "v2-022-dogfood-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		ownerToken:   "v2-022-dogfood-owner-token", //nolint:gosec // fixture credential for a throwaway local emulator install
		ownerEmail:   "owner@example.com",
		client:       &http.Client{Timeout: 30 * time.Second},
		recordPath:   recordPath,
		record:       record,
		ledger:       &agenticrunner.CostLedger{Path: record.LedgerPath, Provider: "claude", TaskID: dogfoodTaskID},
	}
	fx.ids = dogfoodIdentifiers{
		EnvironmentClass: dogfoodEnvClass,
		Hostname:         mustHostname(t),
		Kernel:           mustCommandOutput(t, "uname", "-sr"),
		EmulatorName:     "firebase firestore emulator",
		EmulatorVersion:  mustCommandOutput(t, "firebase", "--version"),
		EmulatorProject:  liveLocalEmulatorProject,
		EmulatorHost:     emulatorHost,
		ClaudePath:       record.ExecutablePath,
		ClaudeVersion:    mustCommandOutput(t, record.ExecutablePath, "--version"),
		GoVersion:        runtime.Version(),
	}

	fx.dataRoot = t.TempDir()
	workspaceRoot := t.TempDir()
	if err := os.Chmod(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fx.workspace, err = agenticrunner.NewWorkspace(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}

	fx.goodBin = filepath.Join(t.TempDir(), "control-plane-dogfood")
	build := exec.Command("go", "build", "-o", fx.goodBin, "github.com/takushi/agentic-loop-foundation/v2/cmd/control-plane")
	build.Dir = repoRoot
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/control-plane failed: %v\n%s", err, out)
	}

	base, alive, stop := startDogfoodProcess(t, fx.goodBin, fx.installation, freeLocalPort(t), fx.ownerToken, fx.ownerEmail)
	waitForHealthz(t, fx.client, base, alive, 30*time.Second)
	fx.base, fx.stopBase = base, stop

	t.Run("environment-identifiers", fx.environmentIdentifiers)
	t.Run("cap-requirement-intake", fx.capRequirementIntake)
	t.Run("cap-backlog-visibility", fx.capBacklogVisibility)
	t.Run("cap-user-documentation", fx.capUserDocumentation)
	t.Run("cap-repository-registration", fx.capRepositoryRegistration)
	t.Run("cap-human-input-request", fx.capHumanInputRequest)
	t.Run("cap-autonomous-resolution", fx.capAutonomousResolution)
	t.Run("cap-loop-control", fx.capLoopControl)
	t.Run("cap-shared-resource-allocation", fx.capSharedResourceAllocation)
	t.Run("cap-provider-operation", fx.capProviderOperation)
	t.Run("cap-preview-operation", fx.capPreviewOperation)
	t.Run("cap-stable-promotion", fx.capStablePromotion)
	t.Run("channel-break-and-resume", fx.channelBreakAndResume)
	t.Run("cap-loop-self-update", fx.capLoopSelfUpdate)
	t.Run("promotability-negative", fx.promotabilityNegative)
	t.Run("promotability-positive", fx.promotabilityPositive)
	t.Run("ledger-snapshot", fx.ledgerSnapshot)
}

func (fx *dogfood) environmentIdentifiers(t *testing.T) {
	for name, value := range map[string]string{
		"hostname": fx.ids.Hostname, "kernel": fx.ids.Kernel, "emulator version": fx.ids.EmulatorVersion,
		"emulator host": fx.ids.EmulatorHost, "claude path": fx.ids.ClaudePath,
		"claude version": fx.ids.ClaudeVersion, "go version": fx.ids.GoVersion,
	} {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("environment identifier %q is empty; a preview-local record cannot be distinguished from a unit test without it", name)
		}
	}
	if !filepath.IsAbs(fx.ids.ClaudePath) {
		t.Fatalf("claude executable path %q is not absolute", fx.ids.ClaudePath)
	}
	t.Logf("environment identifiers: %s", fx.ids)
	t.Logf("preflight record: path=%s digest=%s max_invocations=%d max_total_cost_usd=%.2f worst_case_reservation_usd=%.2f ledger=%s base_names=%v granted_names=%v",
		dogfoodRecordRel, fx.record.Digest, fx.record.Limits.MaxInvocations, fx.record.Limits.MaxTotalCostUSD,
		fx.record.Limits.WorstCaseReservationUSD, fx.record.LedgerPath, fx.record.EnvironmentBaseNames, fx.record.EnvironmentGranted)
}

// capRequirementIntake exercises cap-requirement-intake against its declared
// success condition ("captureRequirement returns a unique Requirement id and
// the persisted content, and the same content is readable from the Backlog")
// and its declared rollback condition, over real HTTP. Declared external
// systems: owner UI and Firestore, both actually connected to.
func (fx *dogfood) capRequirementIntake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	_ = ctx

	// (a) the owner authentication boundary at this grade is a session/token
	// boundary; the IAP boundary is D1 and is never asserted here.
	if r := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements", nil, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d: %+v", r.status, r.body)
	}
	if r := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements", map[string]string{"Authorization": "Bearer not-a-real-token"}, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("expected 401 with an unknown bearer token, got %d: %+v", r.status, r.body)
	}

	// (b) capture returns a unique id and the owner actor type.
	const text = "V2-022 preview-local dogfood: capture a Requirement through the real HTTP surface"
	first := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/requirements", fx.owner(), map[string]any{
		"request_id": "v2-022-intake-1", "text": text,
	})
	if first.status != http.StatusCreated {
		t.Fatalf("capture: expected 201, got %d: %+v", first.status, first.body)
	}
	requirementID, _ := first.body["requirement_id"].(string)
	if requirementID == "" {
		t.Fatalf("capture response carries no requirement_id: %+v", first.body)
	}
	requestedBy, _ := first.body["requested_by"].(map[string]any)
	if actor, _ := requestedBy["actor_type"].(string); actor != "owner" {
		t.Fatalf("requested_by.actor_type = %q, want owner: %+v", actor, first.body)
	}
	second := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/requirements", fx.owner(), map[string]any{
		"request_id": "v2-022-intake-2", "text": text + " (second)",
	})
	if second.status != http.StatusCreated {
		t.Fatalf("second capture: expected 201, got %d: %+v", second.status, second.body)
	}
	secondID, _ := second.body["requirement_id"].(string)
	if secondID == "" || secondID == requirementID {
		t.Fatalf("Requirement ids are not unique: %q and %q", requirementID, secondID)
	}
	fx.intakeRequirementID = requirementID
	fx.capturedIDs = append(fx.capturedIDs, requirementID, secondID)

	// (c) the persisted content is readable back over HTTP.
	detail := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements/"+requirementID, fx.owner(), nil)
	if detail.status != http.StatusOK {
		t.Fatalf("requirement detail: expected 200, got %d: %+v", detail.status, detail.body)
	}
	if got, _ := detail.body["original_text"].(string); got != text {
		t.Fatalf("original_text = %q, want %q", got, text)
	}
	if got, _ := detail.body["status"].(string); got != "captured" {
		t.Fatalf("status = %q, want captured", got)
	}

	// (d) the declared external system is a real Firestore emulator: read the
	// requirement, event and outbox documents directly, bypassing this
	// application's own HTTP API entirely. Capture stages a Requirement and an
	// Event but no Outbox item (only a control change, a claim and an accepted
	// result stage one), so the installation's control baseline is recorded
	// first -- a real owner action over real HTTP, not a fixture write.
	fx.setControl(t, "v2-022-intake:baseline-control", "allow", "v2-022 dogfood: record the installation's control baseline", 0)
	fx.verifyEmulatorDocuments(t, "requirements", "events", "outbox", "quota")

	// (e) persistence is not this process's memory: stop the process, prove
	// it is gone by polling, start a second one against the same
	// installation and emulator, and re-read over real HTTP.
	deathInstallation := fx.installation
	tempPort := freeLocalPort(t)
	tempBase, tempAlive, tempStop := startDogfoodProcess(t, fx.goodBin, deathInstallation, tempPort, fx.ownerToken, fx.ownerEmail)
	waitForHealthz(t, fx.client, tempBase, tempAlive, 30*time.Second)
	tempStop()
	dogfoodPoll(t, "the stopped control-plane process to be gone", 20*time.Second, func() bool { return !tempAlive() })
	secondBase, secondAlive, _ := startDogfoodProcess(t, fx.goodBin, deathInstallation, freeLocalPort(t), fx.ownerToken, fx.ownerEmail)
	waitForHealthz(t, fx.client, secondBase, secondAlive, 30*time.Second)
	reread := dogfoodCall(t, fx.client, http.MethodGet, secondBase+"/v1/requirements/"+requirementID, fx.owner(), nil)
	if reread.status != http.StatusOK {
		t.Fatalf("the Requirement did not survive process death: status %d body %+v", reread.status, reread.body)
	}
	if got, _ := reread.body["original_text"].(string); got != text {
		t.Fatalf("text after a second process read it back = %q, want %q", got, text)
	}

	// (f) the declared rollback condition, observed rather than argued: drive
	// real capture mutations against a FRESH installation until the budget
	// hard guard answers HTTP 429 quota_exhausted with Retry-After BEFORE the
	// side effect, never a 400. A fresh installation is used so exhausting
	// the daily write budget cannot poison the rest of this exercise.
	rollbackInstallation := fx.installation + "-rollback"
	rollbackBase, rollbackAlive, _ := startDogfoodProcess(t, fx.goodBin, rollbackInstallation, freeLocalPort(t), fx.ownerToken, fx.ownerEmail)
	waitForHealthz(t, fx.client, rollbackBase, rollbackAlive, 30*time.Second)
	exhaustWriteBudgetOrFail(t, fx.client, rollbackBase, fx.ownerToken)
	t.Logf("cap-requirement-intake observed in full at %s; identifiers: %s", dogfoodEnvClass, fx.ids)
}

// verifyEmulatorDocuments connects a Firestore client directly to the
// emulator, bypassing the control-plane HTTP API entirely, and confirms that
// each named collection holds at least one real document for this
// installation.
func (fx *dogfood) verifyEmulatorDocuments(t *testing.T, collections ...string) {
	t.Helper()
	ctx := context.Background()
	client, err := cloudfirestore.NewClient(ctx, liveLocalEmulatorProject)
	if err != nil {
		t.Fatalf("connect directly to the Firestore emulator: %v", err)
	}
	defer client.Close()
	for _, collection := range collections {
		path, err := agenticfirestore.CollectionPath(fx.installation, collection)
		if err != nil {
			t.Fatalf("collection path for %s: %v", collection, err)
		}
		docs, err := client.Collection(path).Documents(ctx).GetAll()
		if err != nil {
			t.Fatalf("reading %s directly from the emulator: %v", collection, err)
		}
		if len(docs) == 0 {
			t.Fatalf("expected at least one real Firestore document in %s (installation=%s), found none", collection, fx.installation)
		}
		t.Logf("emulator direct read (bypassing the HTTP API): installation=%s collection=%s documents=%d", fx.installation, collection, len(docs))
	}
}

// capBacklogVisibility exercises cap-backlog-visibility: listRequirements and
// queueSummary report the current Backlog with no shortfall, computed from
// this run's own captures rather than from a constant. Declared external
// systems: owner UI and Firestore.
func (fx *dogfood) capBacklogVisibility(t *testing.T) {
	page := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements?page_size=1", fx.owner(), nil)
	if page.status != http.StatusOK {
		t.Fatalf("backlog page: expected 200, got %d: %+v", page.status, page.body)
	}
	rows, _ := page.body["requirements"].([]any)
	if len(rows) != 1 {
		t.Fatalf("page_size=1 returned %d rows; the page is not bounded", len(rows))
	}
	cursor, _ := page.body["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("a bounded page over %d captures reported no next_cursor: %+v", len(fx.capturedIDs), page.body)
	}

	// The cursor is measured, not assumed -- and what is measured now is the
	// whole walk, not one hop.
	//
	// HISTORY, kept rather than deleted. On 2026-08-26, V2-022's dogfood run
	// measured escalation E22-11 right here: the second Backlog page was
	// refused with HTTP 400. internal/store/firestore's RequirementsPage
	// passed StartAfter(collectionPath + "/" + key) while ordering by
	// firestore.DocumentID, and the Go client, whose contract is that a string
	// cursor under a DocumentID order is the document id RELATIVE to the
	// queried collection, prefixed the collection's own resource name a second
	// time; the server refused the doubled parent, and internal/api's
	// unclassified-error default dressed that storage-side InvalidArgument as
	// 400 invalid_request. This probe then FAILED ON PURPOSE if the second page
	// ever succeeded, with a message instructing the reader to re-judge this
	// capability's verdict rather than assume it.
	//
	// V2-079 supplied exactly that judgement: it replaced both doubled-prefix
	// expressions with the bare collection-relative document id through one
	// named helper (internal/store/firestore.documentIDCursor), changing no
	// byte of what this route returns. So the inverted assertion is replaced by
	// the assertion it demanded -- page to exhaustion at page_size=1, feeding
	// back each next_cursor the route itself issued, and cover every
	// Requirement this run captured exactly once. No order is asserted: the
	// Firestore adapter orders by the base64url document key, which is not raw
	// id order. The capability VERDICT remains the M5 re-dogfood's to issue.
	walked := map[string]int{}
	countRows := func(raw []any) {
		for _, row := range raw {
			if m, ok := row.(map[string]any); ok {
				if id, _ := m["requirement_id"].(string); id != "" {
					walked[id]++
				}
			}
		}
	}
	countRows(rows)
	// The walk is bounded by the number of Requirements this run captured,
	// never by a timer.
	bound := len(fx.capturedIDs) + 2
	for pages := 1; ; pages++ {
		if pages > bound {
			t.Fatalf("the Backlog walk did not terminate within %d pages (walked=%v)", bound, walked)
		}
		next := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements?page_size=1&cursor="+url.QueryEscape(cursor), fx.owner(), nil)
		if next.status != http.StatusOK {
			t.Fatalf("Backlog page %d with the next_cursor this run was handed: expected 200, got %d: %+v", pages, next.status, next.body)
		}
		nextRows, _ := next.body["requirements"].([]any)
		countRows(nextRows)
		cursor, _ = next.body["next_cursor"].(string)
		if cursor == "" {
			if len(nextRows) != 1 {
				t.Fatalf("terminal Backlog page carried %d rows, want exactly 1 at page_size=1: %+v", len(nextRows), next.body)
			}
			break
		}
		if len(nextRows) != 1 {
			t.Fatalf("non-terminal Backlog page %d carried %d rows, want exactly 1 at page_size=1: %+v", pages, len(nextRows), next.body)
		}
	}
	for _, id := range fx.capturedIDs {
		if walked[id] != 1 {
			t.Fatalf("Requirement %s appeared %d times across the Backlog walk, want exactly 1 (walked=%v)", id, walked[id], walked)
		}
	}
	t.Logf("the Backlog was paged to exhaustion at page_size=1 over the cursor the route itself issued: every one of the %d Requirements this run captured was covered exactly once, with no duplicate and no omission", len(fx.capturedIDs))

	// The whole Backlog is still readable in one bounded page, so the rest of
	// the declared success condition can still be measured against this run's
	// own captures rather than against a constant.
	whole := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements?page_size=100", fx.owner(), nil)
	if whole.status != http.StatusOK {
		t.Fatalf("full Backlog page: expected 200, got %d: %+v", whole.status, whole.body)
	}
	wholeRows, _ := whole.body["requirements"].([]any)
	seen := map[string]bool{}
	for _, row := range wholeRows {
		if m, ok := row.(map[string]any); ok {
			if id, _ := m["requirement_id"].(string); id != "" {
				seen[id] = true
			}
		}
	}
	total := len(seen)
	for _, id := range fx.capturedIDs {
		if !seen[id] {
			t.Fatalf("the Backlog did not report Requirement %s that this run captured", id)
		}
	}

	summary := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/queue/summary", fx.owner(), nil)
	if summary.status != http.StatusOK {
		t.Fatalf("queue summary: expected 200, got %d: %+v", summary.status, summary.body)
	}
	requirements, ok := summary.body["requirements"].(float64)
	if !ok {
		t.Fatalf("queue summary carries no requirements count: %+v", summary.body)
	}
	if int(requirements) != total {
		t.Fatalf("queueSummary reports %d Requirements but the Backlog list walked %d; the two disagree", int(requirements), total)
	}
	byRequirement, ok := summary.body["by_requirement_status"].(map[string]any)
	if !ok || len(byRequirement) == 0 {
		t.Fatalf("queue summary carries no by_requirement_status breakdown: %+v", summary.body)
	}
	captured := 0
	for _, v := range byRequirement {
		if n, ok := v.(float64); ok {
			captured += int(n)
		}
	}
	if captured != int(requirements) {
		t.Fatalf("by_requirement_status sums to %d but requirements is %d", captured, int(requirements))
	}
	if _, ok := summary.body["by_increment_status"]; !ok {
		t.Fatalf("queue summary carries no by_increment_status breakdown: %+v", summary.body)
	}
	if _, ok := summary.body["active_executions"].(float64); !ok {
		t.Fatalf("queue summary carries no active_executions count: %+v", summary.body)
	}

	// Measured shortfall, recorded as a shortfall and not as a failure of the
	// declared success condition (E22-7): the declared user action "filter by
	// related Repository" has no implementation. GET /v1/requirements accepts
	// only page_size and cursor, so a repository_id query parameter changes
	// nothing about the page it returns.
	unfiltered := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements?page_size=25", fx.owner(), nil)
	filtered := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements?page_size=25&repository_id=does-not-exist", fx.owner(), nil)
	if unfiltered.status != http.StatusOK || filtered.status != http.StatusOK {
		t.Fatalf("E22-7 probe: unexpected statuses %d and %d", unfiltered.status, filtered.status)
	}
	unfilteredRows, _ := unfiltered.body["requirements"].([]any)
	filteredRows, _ := filtered.body["requirements"].([]any)
	if len(unfilteredRows) != len(filteredRows) {
		t.Fatalf("E22-7 probe: a repository_id parameter changed the page (%d vs %d rows); re-measure the shortfall", len(unfilteredRows), len(filteredRows))
	}
	t.Logf("E22-7 measured: GET /v1/requirements ignores repository_id (%d rows with and without it); the Backlog cannot be filtered by Repository", len(filteredRows))
	t.Logf("what WAS observed: a bounded page, a next_cursor, and a queueSummary whose counts by Requirement status, by Increment status and active Executions agree with the %d Requirements this run itself created; the declared rollback condition also holds, because every read above left the canonical state unchanged.", total)
	t.Log("E22-11, measured on 2026-08-26 and the reason cap-backlog-visibility was recorded FAILED then, no longer reproduces: the Backlog was paged past its first page over the cursor the route itself issued, walked to exhaustion, covering every Requirement this run captured exactly once (V2-079). The remaining measured shortfall is E22-7: the declared user action \"filter by related Repository\" is still unimplemented. This helper records what it measured and issues no verdict; the cap-backlog-visibility verdict and its evidence_ids belong to the M5 re-dogfood.")
}

// dogfoodDocSet is the documentation role's real member set (the same five
// files release.assembleMembers resolves for RoleDocumentation).
var dogfoodDocSet = []string{
	"docs/preview/README.md",
	"docs/preview/capabilities.md",
	"docs/preview/index.md",
	"docs/preview/stable-diff.md",
	"docs/stable/index.md",
}

func (fx *dogfood) readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fx.repoRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// capUserDocumentation exercises cap-user-documentation: the documents that
// correspond to the channel/version in use are completely referenceable and
// do not disagree with observed behaviour. Declared external system: owner UI.
func (fx *dogfood) capUserDocumentation(t *testing.T) {
	assembled, err := release.AssembleFromRoot(fx.repoRoot)
	if err != nil {
		t.Fatalf("assemble the tree the running binary was built from: %v", err)
	}
	contractRelease := assembled.Contract.Version

	var contract struct {
		Release      string `json:"release"`
		Capabilities []struct {
			ID          string   `json:"id"`
			Status      string   `json:"status"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"capabilities"`
	}
	raw, err := os.ReadFile(filepath.Join(fx.repoRoot, "contracts", "release-contract", "foundation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Release != contractRelease {
		t.Fatalf("foundation.json release %q disagrees with the compiled contract %q", contract.Release, contractRelease)
	}
	previewIndex := fx.readDoc(t, "docs/preview/index.md")
	if err := release.VerifyPreviewReleaseMarker(previewIndex, contractRelease); err != nil {
		t.Fatalf("docs/preview/index.md's Release: marker does not name the release in use: %v", err)
	}

	// The deterministic document routing checks of internal/release/docs.go,
	// over the real doc set.
	if err := release.VerifyLinksResolve(fx.repoRoot, dogfoodDocSet); err != nil {
		t.Fatalf("VerifyLinksResolve over the real doc set: %v", err)
	}
	if err := release.VerifyCapabilityAnchorBijection(fx.readDoc(t, "docs/preview/capabilities.md"), assembled.Contract.Capabilities); err != nil {
		t.Fatalf("capability anchor bijection over the real doc set: %v", err)
	}
	anyStable := false
	for _, c := range contract.Capabilities {
		if c.Status == "stable" {
			anyStable = true
		}
	}
	if anyStable {
		t.Fatal("no capability may claim stable status in this task: nine capabilities cannot be exercised and a promotion would be fabricated")
	}
	stableIndex := fx.readDoc(t, "docs/stable/index.md")
	if err := release.VerifyStableReleaseMarker(stableIndex, anyStable); err != nil {
		t.Fatalf("docs/stable/index.md's marker: %v", err)
	}
	if !strings.Contains(stableIndex, "Stable release: none") {
		t.Fatal("docs/stable/index.md no longer carries the exact marker line 'Stable release: none'")
	}
	if err := release.VerifyRequiredSections(fx.readDoc(t, "docs/preview/stable-diff.md"), release.RequiredPreviewSections); err != nil {
		t.Fatalf("the four required Preview sections: %v", err)
	}
	if err := release.VerifyNoStableToPreviewLinks(fx.repoRoot, []string{"docs/stable/index.md"}); err != nil {
		t.Fatalf("Stable-to-Preview link check: %v", err)
	}
	blocks := 0
	for _, doc := range dogfoodDocSet {
		blocks += len(release.ExtractCodeBlocks(fx.readDoc(t, doc)))
	}
	if err := release.VerifyCodeBlockAllowlist(nil, nil); err != nil {
		t.Fatalf("code-block allowlisting over an empty block set: %v", err)
	}
	if blocks != 0 {
		t.Fatalf("the real doc set carries %d fenced code blocks; every block must be allowlisted before this claim holds", blocks)
	}

	// The declared external system is the owner UI: it is served by the
	// running process.
	console := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/owner/", fx.owner(), nil)
	if console.status != http.StatusOK {
		t.Fatalf("GET /owner/ expected 200, got %d", console.status)
	}

	// Deviation E22-10, measured and recorded rather than asserted away: the
	// owner console serves no document route, so at preview-local the owner
	// reads the repository working tree. Probing the two shapes a document
	// route would take shows neither exists.
	for _, probe := range []string{"/owner/docs/preview/index.md", "/docs/preview/index.md"} {
		r := dogfoodCall(t, fx.client, http.MethodGet, fx.base+probe, fx.owner(), nil)
		if r.status == http.StatusOK {
			t.Fatalf("E22-10 probe: %s returned 200, so the console does serve a document route; re-measure the deviation", probe)
		}
		t.Logf("E22-10 measured: GET %s -> %d (no document route)", probe, r.status)
	}
	// Second measured deviation, new at this commit and NOT anticipated by
	// wo-v2-022: docs/preview/capabilities.md states, for
	// cap-preview-operation, that the owner reads the assembled Preview
	// release version from GET /v1/release/state and that a process which
	// recorded no route answers "no route recorded" rather than a guess. The
	// shipped cmd/control-plane attaches no ReleaseObserver, so the running
	// process answers 503 release_observer_not_configured and reports no
	// version at all. That is a difference between the document and the
	// observed behaviour, which is the second half of this capability's own
	// success condition.
	state := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/release/state", fx.owner(), nil)
	documentClaimsReleaseStateIsReadable := strings.Contains(fx.readDoc(t, "docs/preview/capabilities.md"), "/v1/release/state")
	if !documentClaimsReleaseStateIsReadable {
		t.Fatal("docs/preview/capabilities.md no longer mentions /v1/release/state; re-measure the document-versus-behaviour difference below rather than reporting a stale one")
	}
	if state.status == http.StatusOK {
		t.Fatalf("GET /v1/release/state returned 200 (%+v): the document-versus-behaviour difference recorded below no longer holds and this capability's verdict must be re-judged, not assumed", state.body)
	}
	t.Logf("measured document-versus-behaviour difference: docs/preview/capabilities.md describes /v1/release/state as owner-readable, and the running process answers %d %v", state.status, state.body["error"])

	t.Logf("every deterministic document routing check of internal/release/docs.go holds over the real doc set, and the release string agrees three ways (contract, compiled contract and the docs Release: marker are all %s).", contractRelease)
	t.Log("cap-user-documentation recorded FAILED against its declared success condition, and its evidence_ids stays empty. Two reasons, both measured, and the empty set is preferred over a claim in doubt (wo-v2-022 A8):")
	t.Log("  (1) E22-10: the owner console -- the capability's ONLY declared external system -- serves no document route, so the documents corresponding to the channel and version in use cannot be referenced through the declared surface at all; at preview-local the owner reads the repository working tree instead.")
	t.Log("  (2) a measured difference from actual behaviour: docs/preview/capabilities.md describes GET /v1/release/state as owner-readable and describes its unrouted answer as \"no route recorded\", and the shipped control-plane answers 503 release_observer_not_configured instead.")
}

// capRepositoryRegistration exercises cap-repository-registration as far as
// this task's declared side-effect surface reaches, and records it FAILED
// against its declared success condition. wo-v2-022 A11 predicted no
// registration route at all (E22-1); that prediction is false at this commit
// and the measurement below says so.
func (fx *dogfood) capRepositoryRegistration(t *testing.T) {
	repositoryID := "v2-022-dogfood-repo"
	registered := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/repositories", fx.owner(), map[string]any{
		"request_id":     "v2-022-register-1",
		"repository_id":  repositoryID,
		"source_url":     "https://github.com/wakuwaku3/agentic-loop-foundation.git",
		"default_branch": "main",
	})
	if registered.status != http.StatusCreated {
		t.Fatalf("POST /v1/repositories: expected 201, got %d: %+v", registered.status, registered.body)
	}
	t.Logf("E22-1 re-measured and FALSE at this commit: POST /v1/repositories returned %d, so a Repository registration route and command exist (V2-064/V2-071)", registered.status)

	list := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/repositories", fx.owner(), nil)
	if list.status != http.StatusOK {
		t.Fatalf("GET /v1/repositories: expected 200, got %d: %+v", list.status, list.body)
	}
	rows, _ := list.body["repositories"].([]any)
	found := false
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["repository_id"].(string); id != repositoryID {
			continue
		}
		found = true
		if _, ok := m["executability"]; !ok {
			t.Fatalf("the registered Repository reports no executability projection: %+v", m)
		}
		t.Logf("registered Repository reported with identity and loop executability: %+v", m)
	}
	if !found {
		t.Fatalf("the registered Repository is absent from GET /v1/repositories: %+v", list.body)
	}
	detail := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/repositories/"+repositoryID, fx.owner(), nil)
	if detail.status != http.StatusOK {
		t.Fatalf("GET /v1/repositories/{id}: expected 200, got %d: %+v", detail.status, detail.body)
	}

	// The negative observation, executed rather than asserted: the declared
	// external systems include GitHub and Git. Loop-executability is reported
	// only from a Runner-submitted forge Observation, and this task's declared
	// side-effect surface forbids reaching a forge at all, so no honest
	// Observation can be produced here. Nothing below contacts GitHub.
	exec, _ := detail.body["executability"].(map[string]any)
	t.Logf("cap-repository-registration recorded FAILED: the declared external systems GitHub and Git were NOT connected to (this task's declared side-effect surface excludes every forge and every remote), so loop executability stays as the process reports it without an Observation: %+v", exec)
	t.Logf("cap-repository-registration bucket: ineligible because a declared external system was not connected (G3-1). E22-1's original ground -- no registration route, no Repository aggregate command, no Git or GitHub client -- is FALSE at this commit: internal/runner/forge.go and internal/runner/git.go both exist.")
}

// dogfoodIDs is the identifier generator the direct-store Service uses for
// the two commands the HTTP surface deliberately does not expose (Plan and
// Prepare: "Increment decomposition/planning is intentionally not" part of
// the /v1 surface, contracts/openapi/openapi-v1.yaml).
type dogfoodIDs struct{ n int }

func (g *dogfoodIDs) Next(kind string) (string, error) {
	g.n++
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-v2022-%s-%d", kind, hex.EncodeToString(b[:]), g.n), nil
}

type dogfoodClock struct{}

func (dogfoodClock) Now() time.Time { return time.Now().UTC() }

// directStoreService opens a second Service over the SAME Firestore emulator
// and the SAME installation as the running control-plane process. It exists
// for exactly one reason, recorded as a measured limit rather than hidden:
// the /v1 surface exposes no route that creates an Increment, so an enrolled
// Runner cannot reach claims:acquire over HTTP at all without another writer
// producing one. Everything the Runner protocol itself does is still driven
// over real HTTP against the real process.
func (fx *dogfood) directStoreService(t *testing.T) (*application.Service, func()) {
	t.Helper()
	ctx := context.Background()
	store, err := agenticfirestore.NewEmulatorClient(ctx, liveLocalEmulatorProject, fx.installation)
	if err != nil {
		t.Fatalf("open a direct emulator store for the installation under test: %v", err)
	}
	service, err := application.NewServiceWithConfig(store, dogfoodClock{}, &dogfoodIDs{}, application.ServiceConfig{InstallationID: fx.installation, LeaseTTL: time.Minute})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return service, func() { _ = store.Close() }
}

// enroll performs the real enrollment exchange over real HTTP: the owner
// issues an enrollment token, the Runner presents a public key and receives a
// challenge, signs the challenge message with the matching private key, and
// exchanges it for a session token. Nothing here is faked and no session
// token is recorded anywhere this task writes.
func (fx *dogfood) enroll(t *testing.T) {
	t.Helper()
	if fx.runnerToken != "" {
		return
	}
	fx.runnerID = "v2-022-dogfood-runner"
	issued := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/enrollment", fx.owner(), map[string]any{"runner_id": fx.runnerID})
	if issued.status != http.StatusCreated {
		t.Fatalf("POST /v1/runner/enrollment: expected 201, got %d: %+v", issued.status, issued.body)
	}
	enrollmentToken, _ := issued.body["enrollment_token"].(string)
	if enrollmentToken == "" {
		t.Fatalf("no enrollment_token in the response: %+v", issued.body)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	challenge := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/enrollment/challenge", nil, map[string]any{
		"enrollment_token": enrollmentToken,
		"public_key":       base64.RawStdEncoding.EncodeToString(pub),
	})
	if challenge.status != http.StatusOK {
		t.Fatalf("enrollment challenge: expected 200, got %d: %+v", challenge.status, challenge.body)
	}
	nonceRaw, _ := challenge.body["nonce"].(string)
	nonce, err := base64.RawURLEncoding.DecodeString(nonceRaw)
	if err != nil {
		t.Fatalf("decode challenge nonce: %v", err)
	}
	issuedAt, err := time.Parse(time.RFC3339Nano, stringField(challenge.body, "issued_at"))
	if err != nil {
		t.Fatalf("parse issued_at: %v", err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, stringField(challenge.body, "expires_at"))
	if err != nil {
		t.Fatalf("parse expires_at: %v", err)
	}
	message := agenticrunner.ChallengeMessage(agenticrunner.Challenge{
		ID:        stringField(challenge.body, "challenge_id"),
		RunnerID:  stringField(challenge.body, "runner_id"),
		PublicKey: pub,
		Nonce:     nonce,
		Method:    stringField(challenge.body, "method"),
		Path:      stringField(challenge.body, "path"),
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	})
	completed := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/enrollment/complete", nil, map[string]any{
		"challenge_id": stringField(challenge.body, "challenge_id"),
		"signature":    base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, message)),
	})
	if completed.status != http.StatusOK {
		t.Fatalf("enrollment complete: expected 200, got %d: %+v", completed.status, completed.body)
	}
	fx.runnerToken = stringField(completed.body, "session_token")
	if fx.runnerToken == "" {
		t.Fatalf("no session_token in the enrollment completion response: %+v", completed.body)
	}
	t.Logf("enrolled Runner session established over real HTTP for runner_id=%s (session token not recorded)", stringField(completed.body, "runner_id"))
}

// setControl issues one real control over real HTTP and returns the revision
// it was recorded at, which every subsequent runner-facing call must carry.
func (fx *dogfood) setControl(t *testing.T, requestID, mode, reason string, allocation int) int {
	t.Helper()
	body := map[string]any{
		"request_id": requestID, "scope_kind": "installation", "scope_value": fx.installation,
		"mode": mode, "reason": reason,
	}
	if allocation > 0 {
		body["allocation_limit"] = map[string]any{"installation_concurrent_executions": allocation}
	}
	r := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/controls", fx.owner(), body)
	if r.status != http.StatusOK {
		t.Fatalf("POST /v1/controls %s: expected 200, got %d: %+v", mode, r.status, r.body)
	}
	revision := int(floatField(r.body, "revision"))
	if revision == 0 {
		t.Fatalf("the control response carries no revision: %+v", r.body)
	}
	if got := stringField(r.body, "mode"); got != mode {
		t.Fatalf("the control response mode = %q, want %q", got, mode)
	}
	fx.controlRevision = revision
	t.Logf("control recorded over real HTTP: mode=%s revision=%d state=%s", mode, revision, stringField(r.body, "state"))
	return revision
}

func stringField(body map[string]any, key string) string {
	v, _ := body[key].(string)
	return v
}

func floatField(body map[string]any, key string) float64 {
	v, _ := body[key].(float64)
	return v
}

// capture is one real capture over real HTTP; it returns the id and version.
func (fx *dogfood) capture(t *testing.T, requestID, text string) (string, domain.Version) {
	t.Helper()
	r := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/requirements", fx.owner(), map[string]any{"request_id": requestID, "text": text})
	if r.status != http.StatusCreated {
		t.Fatalf("capture %s: expected 201, got %d: %+v", requestID, r.status, r.body)
	}
	id := stringField(r.body, "requirement_id")
	if id == "" {
		t.Fatalf("capture %s returned no requirement_id: %+v", requestID, r.body)
	}
	fx.capturedIDs = append(fx.capturedIDs, id)
	return id, domain.Version(floatField(r.body, "version"))
}

// capHumanInputRequest drives cap-human-input-request's declared chain end to
// end against the dogfood's real base URL and real callers, and records what
// it measured. It issues no verdict: the verdict and its evidence_ids belong to
// the M5 re-dogfood.
//
// HISTORY, kept rather than deleted. wo-v2-022 A11 predicted that no
// application command, no API route and no detail field existed for asking the
// owner a question (escalation E22-2). On 2026-08-26, V2-022's dogfood run
// measured that all three DID exist -- V2-065 had shipped them -- and found the
// remaining, decisive gap one level deeper: domain.DecideRequirement admits the
// needs-input command from framing, active and evaluating only, the only
// command that leaves "captured" is domain.RequirementStartFraming, and NO
// application command issued it. So no Requirement created through the real
// surface could reach a status from which a question was a legal transition:
// the ask trigger was not merely unwired to a decision, its precondition was
// unreachable. This probe therefore FAILED ON PURPOSE if POST :request-input
// ever succeeded on a captured Requirement, with a message instructing the
// reader that "this capability's eligibility must be re-judged, not assumed".
//
// V2-082 supplied exactly that judgement. It added one caller-initiated
// application command, Service.StartFraming, and one route, POST
// /v1/requirements/{requirement_id}:start-framing, issuing the transition the
// domain had always admitted from captured; internal/domain was not edited,
// because the transition, its guard and its target status already existed. The
// set of Requirement statuses reachable through the application went from the
// single element {captured} to exactly {captured, framing, needs-input,
// ready}. So the inverted assertion is replaced by the assertion it demanded:
// the whole declared chain, in order, over real HTTP with the real callers.
func (fx *dogfood) capHumanInputRequest(t *testing.T) {
	fx.enroll(t)
	requirementID, version := fx.capture(t, "v2-022-needs-input-1", "V2-022 dogfood: ask the owner a question about this Requirement")

	before := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements/"+requirementID, fx.owner(), nil)
	if before.status != http.StatusOK {
		t.Fatalf("requirement detail before the ask: %d %+v", before.status, before.body)
	}
	if got := stringField(before.body, "status"); got != "captured" {
		t.Fatalf("a freshly captured Requirement reports status %q, want captured", got)
	}
	if _, present := before.body["needs_input"]; present {
		t.Fatalf("a freshly captured Requirement already reports a needs_input object: %+v", before.body)
	}
	// The Backlog row count, read here and again after the answer, so "the
	// SAME Requirement resumed" is asserted against a count rather than
	// inferred. It is read inline rather than through a new helper: this task
	// changes exactly one function in this file.
	rowCount := func(when string) int {
		page := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements?page_size=100", fx.owner(), nil)
		if page.status != http.StatusOK {
			t.Fatalf("the requirement page %s: %d %+v", when, page.status, page.body)
		}
		rows, _ := page.body["requirements"].([]any)
		return len(rows)
	}
	backlogBefore := rowCount("before the chain")

	// 1. The Requirement leaves captured. This is the edge V2-082 added, and
	// it is what makes everything below reachable at all.
	framing := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/requirements/"+requirementID+":start-framing", fx.owner(), map[string]any{
		"request_id":                   "v2-022-start-framing-1",
		"expected_requirement_version": int(version),
	})
	if framing.status != http.StatusOK {
		t.Fatalf("POST :start-framing on a captured Requirement: %d %+v", framing.status, framing.body)
	}
	if got := stringField(framing.body, "status"); got != "framing" {
		t.Fatalf("start-framing reports status %q, want framing", got)
	}
	framingVersion := domain.Version(floatField(framing.body, "version"))
	if framingVersion != version+1 {
		t.Fatalf("start-framing reports version %d, want %d", framingVersion, version+1)
	}
	framed := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements/"+requirementID, fx.owner(), nil)
	if framed.status != http.StatusOK {
		t.Fatalf("requirement detail after start-framing: %d %+v", framed.status, framed.body)
	}
	if got := stringField(framed.body, "status"); got != "framing" {
		t.Fatalf("the detail after start-framing reports %q, want framing", got)
	}

	// 2. The Loop asks the question, as a Runner.
	ask := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/requirements/"+requirementID+":request-input", fx.runner(), map[string]any{
		"request_id":                   "v2-022-ask-1",
		"expected_requirement_version": int(framingVersion),
		"question":                     "Should this dogfood run register the owner's own Repository against the real forge?",
		"reason_class":                 "destructive-irreversible",
		"reason":                       "reaching a forge is outside this task's declared side-effect surface",
		"options": []map[string]string{
			{"option_id": "skip", "summary": "do not reach any forge", "impact": "loop executability stays unobserved for this Repository"},
			{"option_id": "reach", "summary": "reach the forge read-only", "impact": "an external system outside the declared surface is contacted"},
		},
		"stopped_scope":    []string{"new-claims-for-this-requirement", "lease-renewal-for-this-requirement"},
		"continuing_scope": []string{"other-requirements", "owner-reads"},
	})
	if ask.status != http.StatusOK {
		t.Fatalf("POST :request-input on a framing Requirement: %d %+v", ask.status, ask.body)
	}
	if got := stringField(ask.body, "status"); got != "needs-input" {
		t.Fatalf("the ask reports status %q, want needs-input", got)
	}
	askVersion := domain.Version(floatField(ask.body, "version"))

	// 3. The owner's read surface shows the question: the reason class, every
	// option with its impact, and both scope lists.
	asked := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements/"+requirementID, fx.owner(), nil)
	if asked.status != http.StatusOK {
		t.Fatalf("requirement detail after the ask: %d %+v", asked.status, asked.body)
	}
	if got := stringField(asked.body, "status"); got != "needs-input" {
		t.Fatalf("the detail after the ask reports %q, want needs-input", got)
	}
	question, ok := asked.body["needs_input"].(map[string]any)
	if !ok {
		t.Fatalf("the detail reports no needs_input object after the ask: %+v", asked.body)
	}
	if stringField(question, "reason_class") != "destructive-irreversible" {
		t.Fatalf("the displayed question reports reason_class %q", stringField(question, "reason_class"))
	}
	if stringField(question, "question") == "" {
		t.Fatalf("the displayed question carries no question text: %+v", question)
	}
	options, ok := question["options"].([]any)
	if !ok || len(options) != 2 {
		t.Fatalf("the displayed question reports %v options, want 2", question["options"])
	}
	seen := map[string]bool{}
	for _, raw := range options {
		option, isObject := raw.(map[string]any)
		if !isObject {
			t.Fatalf("a displayed option is not an object: %v", raw)
		}
		if stringField(option, "impact") == "" {
			t.Fatalf("a displayed option carries no impact: %+v", option)
		}
		seen[stringField(option, "option_id")] = true
	}
	if !seen["skip"] || !seen["reach"] {
		t.Fatalf("the displayed question does not report both recorded options: %v", seen)
	}
	for _, list := range []string{"stopped_scope", "continuing_scope"} {
		entries, isList := question[list].([]any)
		if !isList || len(entries) == 0 {
			t.Fatalf("the displayed question reports no %s: %v", list, question[list])
		}
	}

	// 4. The owner answers by naming one recorded option, and the SAME
	// Requirement resumes. The declared failure condition -- a new Requirement
	// instead of a resumed one -- is asserted in the same run.
	answer := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/requirements/"+requirementID+":answer-input", fx.owner(), map[string]any{
		"request_id":                   "v2-022-answer-1",
		"expected_requirement_version": int(askVersion),
		"option_id":                    "skip",
	})
	if answer.status != http.StatusOK {
		t.Fatalf("POST :answer-input: %d %+v", answer.status, answer.body)
	}
	if got := stringField(answer.body, "requirement_id"); got != requirementID {
		t.Fatalf("the answer resumed requirement_id %q, want the SAME %q", got, requirementID)
	}
	if got := stringField(answer.body, "status"); got != "ready" {
		t.Fatalf("the answer left the Requirement at %q, want ready", got)
	}
	if got := stringField(answer.body, "answered_option_id"); got != "skip" {
		t.Fatalf("the answer recorded option %q", got)
	}
	if got := rowCount("after the chain"); got != backlogBefore {
		t.Fatalf("the chain changed the number of Requirements from %d to %d; resuming must create nothing", backlogBefore, got)
	}

	t.Logf("measured: requirement_id=%s walked captured -> framing -> needs-input -> ready over the real base URL with the real owner and runner callers, at versions %d -> %d -> %d, the displayed question carried its reason class, both options with their impacts and both scope lists, and the Requirement row count was unchanged at %d. E22-2's original ground (no command, no route, no detail field) was already FALSE at V2-065; the precondition gap this probe was inverted to detect -- that no application command issued the captured->framing transition -- was closed by V2-082 (Service.StartFraming and POST /v1/requirements/{requirement_id}:start-framing, with internal/domain unedited). This helper records what it measured and issues no verdict; the cap-human-input-request verdict and its evidence_ids belong to the M5 re-dogfood.", requirementID, version, framingVersion, askVersion, backlogBefore)
}

// dogfoodInvocation is Build -> Run -> Parse against the real claude CLI,
// exposing only the projection SupervisedInvocationRunner produces. The real
// CLI's raw stdout is never returned to a caller by that type and nothing
// here writes a prompt or a response anywhere.
func (fx *dogfood) dogfoodInvocation(t *testing.T, ctx context.Context, purpose, executionID, requirementID, incrementID, instruction string) (provider.Result, error) {
	t.Helper()
	ws, err := fx.workspace.Create(executionID)
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	log, err := agenticrunner.NewBoundedLog(fx.dataRoot, executionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	packet := provider.WorkPacket{
		Version:            provider.ContractVersion,
		RequirementID:      requirementID,
		RequirementSummary: instruction,
		IncrementID:        incrementID,
		Repository:         dogfoodRepositoryID,
	}
	if err := packet.Validate(); err != nil {
		t.Fatalf("work packet failed to validate: %v", err)
	}
	inv, err := provider.ClaudeAdapter{}.Build(provider.Request{OperationID: executionID, Workspace: ws, Packet: packet})
	if err != nil {
		t.Fatalf("build invocation: %v", err)
	}
	sup := agenticrunner.SupervisedInvocationRunner{
		Supervisor: agenticrunner.ProcessSupervisor{TermGrace: 3 * time.Second},
		Log:        log,
		Ledger:     fx.ledger,
		RepoRoot:   fx.repoRoot,
		RecordPath: fx.recordPath,
		Purpose:    purpose,
	}
	fx.invocationPurposes = append(fx.invocationPurposes, purpose)
	raw, err := sup.Run(ctx, inv)
	if err != nil {
		return provider.Result{}, err
	}
	result, err := provider.ClaudeAdapter{}.Parse(raw)
	if err != nil {
		return provider.Result{}, err
	}
	return result, nil
}

// dogfoodTarget is the ControlTarget every runner-facing call carries.
func (fx *dogfood) dogfoodTarget(requirementID, incrementID string) map[string]any {
	return map[string]any{"installation_id": fx.installation, "requirement_id": requirementID, "increment_id": incrementID}
}

// planAndPrepare creates the Increment through the direct-store Service,
// because the /v1 surface exposes no route that creates one.
func (fx *dogfood) planAndPrepare(t *testing.T, base, requirementID string, version domain.Version) (string, domain.Version) {
	t.Helper()
	service, closeStore := fx.directStoreService(t)
	defer closeStore()
	ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: fx.ownerEmail})
	// V2-089: every journey fed by this helper claims the Increment it creates,
	// and a claim is refused unless the parent Requirement is in one of the four
	// statuses that admit work -- ready, active, waiting, recovering. Each
	// caller passes a Requirement that fx.capture left in `captured`, so the
	// helper walks it to `ready` through the product's OWN commands -- V2-082's
	// Service.StartFraming and V2-084's Service.CompleteFraming, on the same
	// direct-store Service this helper already holds -- and threads the
	// resulting version into the Plan. No store write and no assertion is
	// touched: this block is purely additive, and it is the only change V2-089
	// makes to this file.
	//
	// This file is behind the live dogfood gate and was NOT EXECUTED by V2-089
	// (no Provider CLI invocation is authorised and no Google Cloud resource may
	// be contacted), so this edit is compile-checked by `go vet ./internal/api`
	// and `gofmt` only and is recorded as unexecuted.
	framed, err := service.StartFraming(ctx, application.StartFramingRequest{RequestID: base + ":start-framing", RequirementID: requirementID, ExpectedVersion: version})
	if err != nil {
		t.Fatalf("start framing (direct store, so the claim below is not refused for a parent that admits no work): %v", err)
	}
	readied, err := service.CompleteFraming(ctx, application.CompleteFramingRequest{RequestID: base + ":complete-framing", RequirementID: requirementID, ExpectedVersion: framed.Version})
	if err != nil {
		t.Fatalf("complete framing (direct store): %v", err)
	}
	version = readied.Version
	planned, err := service.Plan(ctx, application.PlanRequest{RequestID: base + ":plan", RequirementID: requirementID, ExpectedRequirementVersion: version})
	if err != nil {
		t.Fatalf("plan (direct store, because no /v1 route creates an Increment): %v", err)
	}
	prepared, err := service.Prepare(ctx, application.PrepareRequest{RequestID: base + ":prepare", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version})
	if err != nil {
		t.Fatalf("prepare (direct store): %v", err)
	}
	return planned.IncrementID, prepared.Version
}

// capAutonomousResolution drives the real runner-facing protocol over real
// HTTP as an enrolled Runner, makes one real claude invocation inside that
// journey, and records the capability FAILED against its declared success
// condition with every unobserved half named.
func (fx *dogfood) capAutonomousResolution(t *testing.T) {
	fx.enroll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	requirementID, version := fx.capture(t, "v2-022-auto-1", "V2-022 dogfood: drive one Requirement through the real runner-facing protocol")
	incrementID, incrementVersion := fx.planAndPrepare(t, "v2-022-auto", requirementID, version)
	target := fx.dogfoodTarget(requirementID, incrementID)

	claim := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/claims:acquire", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:claim", "increment_id": incrementID,
		"expected_increment_version": int(incrementVersion), "control_revision": fx.controlRevision, "target": target,
	})
	if claim.status != http.StatusOK {
		t.Fatalf("claims:acquire over real HTTP: expected 200, got %d: %+v", claim.status, claim.body)
	}
	executionID := stringField(claim.body, "execution_id")
	leaseID := stringField(claim.body, "lease_id")
	fence := int(floatField(claim.body, "fencing_token"))
	if executionID == "" || leaseID == "" || fence == 0 {
		t.Fatalf("claim response is incomplete: %+v", claim.body)
	}

	processPermit := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/permits:check", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:process-permit", "kind": "process", "target": target,
		"control_revision": fx.controlRevision, "fencing_token": fence, "expected_fencing_token": fence, "resource": executionID,
	})
	if processPermit.status != http.StatusOK {
		t.Fatalf("permits:check(process): expected 200, got %d: %+v", processPermit.status, processPermit.body)
	}
	if allowed, _ := processPermit.body["allowed"].(bool); !allowed {
		t.Fatalf("the process permit was refused: %+v", processPermit.body)
	}

	started := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/executions/"+executionID+":start", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:start", "expected_execution_version": 1, "control_revision": fx.controlRevision, "provider": "claude",
	})
	if started.status != http.StatusOK {
		t.Fatalf("executions/{id}:start: expected 200, got %d: %+v", started.status, started.body)
	}
	executionVersion := int(floatField(started.body, "version"))

	renewed := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/leases/"+leaseID+":renew", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:renew", "expected_lease_version": 1, "fencing_token": fence, "control_revision": fx.controlRevision,
	})
	if renewed.status != http.StatusOK {
		t.Fatalf("leases/{id}:renew: expected 200, got %d: %+v", renewed.status, renewed.body)
	}
	beat := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/heartbeat", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:heartbeat", "control_revision": fx.controlRevision,
	})
	if beat.status != http.StatusOK {
		t.Fatalf("runner/heartbeat: expected 200, got %d: %+v", beat.status, beat.body)
	}

	result, err := fx.dogfoodInvocation(t, ctx, "V2-022-autonomous-resolution", executionID, requirementID, incrementID,
		"Do not use any tools. Do not read or write any files. Reply with exactly the single word ACKNOWLEDGED and nothing else.")
	if err != nil {
		t.Fatalf("real claude invocation inside the runner-facing journey: %v", err)
	}
	if !result.Succeeded {
		class, code, retryable := "", "", false
		if result.Failure != nil {
			class, code, retryable = string(result.Failure.Class), result.Failure.Code, result.Failure.Retryable
		}
		t.Fatalf("the real provider run did not succeed: failure class=%q code=%q retryable=%v", class, code, retryable)
	}
	if result.OutputDigest == "" {
		t.Fatal("the Increment Artifact digest (OutputDigest) is empty")
	}

	checkpoint := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/checkpoints", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:checkpoint", "execution_id": executionID, "lease_id": leaseID,
		"fencing_token": fence, "control_revision": fx.controlRevision,
	})
	if checkpoint.status != http.StatusOK {
		t.Fatalf("runner/checkpoints: expected 200, got %d: %+v", checkpoint.status, checkpoint.body)
	}
	effectPermit := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/permits:check", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:effect-permit", "kind": "external-effect", "target": target,
		"control_revision": fx.controlRevision, "fencing_token": fence, "expected_fencing_token": fence, "resource": executionID,
	})
	if effectPermit.status != http.StatusOK {
		t.Fatalf("permits:check(external-effect): expected 200, got %d: %+v", effectPermit.status, effectPermit.body)
	}
	accepted := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/executions/result", fx.runner(), map[string]any{
		"request_id": "v2-022-auto:result", "execution_id": executionID, "lease_id": leaseID,
		"expected_execution_version": executionVersion, "fencing_token": fence, "control_revision": fx.controlRevision,
		"succeeded": true, "target": target,
		"provider_observation": map[string]any{"name": "claude", "stopped_for_inspection": false},
	})
	if accepted.status != http.StatusOK {
		t.Fatalf("executions/result: expected 200, got %d: %+v", accepted.status, accepted.body)
	}
	if got := stringField(accepted.body, "status"); got != "succeeded" {
		t.Fatalf("terminal execution status = %q, want succeeded: %+v", got, accepted.body)
	}

	detail := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/requirements/"+requirementID, fx.owner(), nil)
	if detail.status != http.StatusOK {
		t.Fatalf("requirement detail after the journey: %d %+v", detail.status, detail.body)
	}
	increments, _ := detail.body["increments"].([]any)
	if len(increments) == 0 {
		t.Fatalf("the Requirement detail reports no Increment after a completed journey: %+v", detail.body)
	}
	t.Logf("state transitions observed on GET /v1/requirements/%s: status=%q increments=%d next_action=%q",
		requirementID, stringField(detail.body, "status"), len(increments), stringField(detail.body, "next_action"))

	t.Log("cap-autonomous-resolution recorded FAILED against its declared success condition. What was NOT observed, and why:")
	t.Log("  (1) no code change, no verification run and no integration happened: this task's declared side-effect surface excludes every forge and every remote, so no Git or GitHub operation was performed. internal/runner/forge.go and internal/runner/git.go DO exist at this commit, so E22-1's original ground (no Git or GitHub client anywhere in the tree) is FALSE; the block here is scope, not absence.")
	t.Log("  (2) the shipped cmd/runner binary cannot do any of this: it refuses to run without --fake and prints that no external control-plane wiring is enabled (E22-6). The Runner side of this journey was this test process speaking the real protocol over real HTTP.")
	t.Log("  (3) no /v1 route creates an Increment, so claims:acquire is unreachable over HTTP without another writer; the Increment here was created through a second Service over the same emulator and installation.")
	t.Log("  (4) the capability declares codex, claude and opencode; only claude was used, and codex and opencode are unauthenticated on this machine (M6/V2-028). It also declares GitHub, which was not connected to (G3-1).")
}

// dogfoodChildPID polls for a direct child process whose /proc name contains
// nameSubstr. It exists to make "the process was still running when the
// control path acted on it" an observable fact rather than an assumption.
func dogfoodChildPID(t *testing.T, nameSubstr string, deadline time.Duration) int {
	t.Helper()
	self := os.Getpid()
	end := time.Now().Add(deadline)
	for {
		if entries, err := os.ReadDir("/proc"); err == nil {
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
		time.Sleep(5 * time.Millisecond)
	}
}

func dogfoodProcessGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return true
	}
	return strings.Contains(string(b), "State:\tZ")
}

// capLoopControl issues a real graceful-stop control over real HTTP, observes
// the requested/acknowledged/effective/verified progression and the
// verification state, proves the control reaches a real process by terminating
// a real in-flight claude child process group and confirming its death by
// polling, and observes that a Runner claim is refused while claims are paused
// and succeeds once the pause is released. It is recorded FAILED against its
// declared success condition.
func (fx *dogfood) capLoopControl(t *testing.T) {
	fx.enroll(t)

	// --- claims paused, then released, observed as a real refusal ---
	pauseRequirement, pauseVersion := fx.capture(t, "v2-022-pause-1", "V2-022 dogfood: a claim refused while claims are paused")
	pauseIncrement, pauseIncrementVersion := fx.planAndPrepare(t, "v2-022-pause", pauseRequirement, pauseVersion)
	pauseRevision := fx.setControl(t, "v2-022-control:pause", "pause-claim", "v2-022 dogfood: prove a claim is refused while claims are paused", 0)
	refused := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/claims:acquire", fx.runner(), map[string]any{
		"request_id": "v2-022-control:claim-while-paused", "increment_id": pauseIncrement,
		"expected_increment_version": int(pauseIncrementVersion), "control_revision": pauseRevision,
		"target": fx.dogfoodTarget(pauseRequirement, pauseIncrement),
	})
	if refused.status == http.StatusOK {
		t.Fatalf("a claim succeeded while claims were paused: %+v", refused.body)
	}
	t.Logf("measured: claims:acquire while pause-claim is effective -> %d %v", refused.status, refused.body["error"])
	allowRevision := fx.setControl(t, "v2-022-control:release", "allow", "v2-022 dogfood: release the pause", 0)
	admitted := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/claims:acquire", fx.runner(), map[string]any{
		"request_id": "v2-022-control:claim-after-release", "increment_id": pauseIncrement,
		"expected_increment_version": int(pauseIncrementVersion), "control_revision": allowRevision,
		"target": fx.dogfoodTarget(pauseRequirement, pauseIncrement),
	})
	if admitted.status != http.StatusOK {
		t.Fatalf("a claim was still refused after the pause was released: %d %+v", admitted.status, admitted.body)
	}
	t.Log("measured: the same claim succeeds once the pause is released")

	// The Requirement whose live child the control path will act on is
	// captured and prepared BEFORE the graceful stop is issued, because
	// graceful-stop denies intake: capturing after the stop would be refused,
	// which is itself the control working.
	killRequirement, killVersion := fx.capture(t, "v2-022-kill-1", "V2-022 dogfood: terminate a live provider child through the control path")
	killIncrement, _ := fx.planAndPrepare(t, "v2-022-kill", killRequirement, killVersion)

	// --- graceful-stop, and the progression on GET /v1/controls ---
	stopRevision := fx.setControl(t, "v2-022-control:graceful-stop", "graceful-stop", "v2-022 dogfood: graceful stop against a live child process group", 0)

	// The Runner acknowledges the revision through the heartbeat it already
	// sends, then the reconcile scheduler identity drives verification.
	beat := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/runner/heartbeat", fx.runner(), map[string]any{
		"request_id": "v2-022-control:ack", "control_revision": stopRevision,
	})
	if beat.status != http.StatusOK {
		t.Fatalf("heartbeat acknowledging the control: %d %+v", beat.status, beat.body)
	}
	reconcile := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/internal/reconcile",
		map[string]string{"X-Goog-Authenticated-User-Email": "accounts.google.com:reconciler@example.iam.gserviceaccount.com"}, map[string]any{})
	if reconcile.status != http.StatusAccepted {
		t.Logf("POST /internal/reconcile returned %d %+v (verification progression is observed from GET /v1/controls below regardless)", reconcile.status, reconcile.body)
	}
	controls := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/controls", fx.owner(), nil)
	if controls.status != http.StatusOK {
		t.Fatalf("GET /v1/controls: expected 200, got %d: %+v", controls.status, controls.body)
	}
	rows, _ := controls.body["controls"].([]any)
	var stopRow map[string]any
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if stringField(m, "mode") == "graceful-stop" {
			stopRow = m
		}
	}
	if stopRow == nil {
		t.Fatalf("GET /v1/controls does not report the graceful-stop control: %+v", controls.body)
	}
	for _, flag := range []string{"requested", "acknowledged", "effective", "verified"} {
		if _, present := stopRow[flag]; !present {
			t.Fatalf("the control read model carries no %q flag: %+v", flag, stopRow)
		}
	}
	if _, present := stopRow["verification"]; !present {
		t.Fatalf("the control read model carries no verification state: %+v", stopRow)
	}
	t.Logf("graceful-stop progression observed: requested=%v acknowledged=%v effective=%v verified=%v verification=%v",
		stopRow["requested"], stopRow["acknowledged"], stopRow["effective"], stopRow["verified"], stopRow["verification"])

	// --- the control reaches a real process: a real claude child process
	// group is terminated while genuinely in flight, and its death is
	// confirmed by polling rather than assumed. The single goroutine below is
	// joined before this subtest returns; it exists because a child cannot be
	// acted upon while in flight without one (the same reason
	// internal/runner/provider_live_test.go starts one).
	invCtx, invCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer invCancel()
	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, err := fx.dogfoodInvocation(t, invCtx, "V2-022-graceful-stop-live-child", "v2-022-kill-execution", killRequirement, killIncrement,
			"Do not use any tools. Reply with exactly the single word ACKNOWLEDGED and nothing else.")
		done <- outcome{err: err}
	}()
	pid := dogfoodChildPID(t, "claude", 60*time.Second)
	t.Logf("real claude child process group observed in flight: pid=%d", pid)
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		t.Fatalf("terminate the child process group %d: %v", pid, err)
	}
	select {
	case out := <-done:
		if out.err == nil {
			t.Log("the terminated invocation still returned a result; the child had already produced output before the signal landed")
		} else {
			t.Logf("the in-flight invocation ended as the control path required: %v", out.err)
		}
	case <-time.After(90 * time.Second):
		invCancel()
		<-done
		t.Fatal("the supervised invocation did not return after its process group was signalled")
	}
	dogfoodPoll(t, "the terminated claude child process to be gone", 30*time.Second, func() bool { return dogfoodProcessGone(pid) })
	t.Logf("the child's death was confirmed by polling /proc, not assumed: pid=%d", pid)

	// The declared rollback condition, executed: the control this exercise
	// issued is released and the installation is returned to its prior mode.
	restored := fx.setControl(t, "v2-022-control:rollback", "allow", "v2-022 dogfood: release the graceful stop and return the installation to its prior mode", 0)
	if restored <= stopRevision {
		t.Fatalf("the releasing control was recorded at revision %d, which does not follow the graceful stop at %d", restored, stopRevision)
	}
	t.Logf("declared rollback condition observed: the graceful stop at revision %d was released at revision %d", stopRevision, restored)

	t.Log("cap-loop-control recorded FAILED against its declared success condition. What was NOT observed:")
	t.Log("  (1) the declared success condition requires the target Runner, process, lease and new-side-effect permission to be displayed. application.ControlReadModel carries no such projection (its fields are scope, mode, revision, the four progression flags and their timestamps, reason, evidence_ref, verification and requested_by) and the owner console renders none of it (E22-8, re-measured and still TRUE).")
	t.Log("  (2) the capability declares Google Cloud Run, which is D1 condition (iv); no evidence id can be written for it in this task no matter how well the local behaviour is observed.")
}

// capSharedResourceAllocation exercises cap-shared-resource-allocation and
// records it FAILED. wo-v2-022 A11 predicted internal/scheduler is imported by
// nothing outside itself, no control mode sets a concurrency limit, and
// queueSummary shows no allocation, waiting reason or exhaustion (E22-5). All
// three predictions are false at this commit (V2-068) and the measurement says so.
func (fx *dogfood) capSharedResourceAllocation(t *testing.T) {
	fx.setControl(t, "v2-022-allocation:limit", "allow", "v2-022 dogfood: set an explicit allocation limit", 2)
	summary := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/queue/summary", fx.owner(), nil)
	if summary.status != http.StatusOK {
		t.Fatalf("GET /v1/queue/summary: expected 200, got %d: %+v", summary.status, summary.body)
	}
	allocation, ok := summary.body["allocation"].(map[string]any)
	if !ok {
		t.Fatalf("queue summary carries no allocation projection: %+v", summary.body)
	}
	for _, field := range []string{"limit", "limit_source", "control_revision", "active", "remaining", "planned_assignments"} {
		if _, present := allocation[field]; !present {
			t.Fatalf("the allocation projection carries no %q: %+v", field, allocation)
		}
	}
	if got := stringField(allocation, "limit_source"); got != "control-revision" {
		t.Fatalf("limit_source = %q after an explicit allocation limit was set, want control-revision: %+v", got, allocation)
	}
	if int(floatField(allocation, "limit")) != 2 {
		t.Fatalf("the reported limit is %v, want the 2 this run set: %+v", allocation["limit"], allocation)
	}
	if _, present := summary.body["waiting"]; !present {
		t.Fatalf("queue summary carries no waiting projection: %+v", summary.body)
	}
	if _, present := summary.body["exhaustion"]; !present {
		t.Fatalf("queue summary carries no exhaustion projection: %+v", summary.body)
	}
	t.Logf("E22-5 re-measured and FALSE at this commit: internal/scheduler is imported by internal/application/allocation.go, POST /v1/controls carries allocation_limit, and queueSummary reports allocation=%+v waiting=%+v exhaustion=%+v", allocation, summary.body["waiting"], summary.body["exhaustion"])
	t.Log("cap-shared-resource-allocation recorded FAILED: the declared external providers are codex, claude and opencode, and only claude is authenticated on this machine, so 'shared AI resource allocated across Repositories' cannot be observed for the declared set (M6/V2-028). The multi-Repository half is M7 (V2-030, V2-031): this run holds one Repository, so no cross-Repository starvation bound was measured. No provider invocation was made for this capability.")
}

// capProviderOperation reads the Provider registry that the autonomous-
// resolution journey's own result populated, and records the capability
// FAILED. No provider invocation is made here.
func (fx *dogfood) capProviderOperation(t *testing.T) {
	providers := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/providers", fx.owner(), nil)
	if providers.status != http.StatusOK {
		t.Fatalf("GET /v1/providers: expected 200, got %d: %+v", providers.status, providers.body)
	}
	t.Logf("E22-4 re-measured and FALSE at this commit: GET /v1/providers exists and reports %+v", providers.body)
	if r := dogfoodCall(t, fx.client, http.MethodPost, fx.base+"/v1/providers", fx.owner(), map[string]any{"request_id": "x"}); r.status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/providers: expected 405 (this surface is read-only), got %d: %+v", r.status, r.body)
	}
	t.Log("cap-provider-operation recorded FAILED: the declared external providers are codex, claude and opencode. Only claude is authenticated on this machine, so no compatible second Provider exists and the declared handoff-or-stated-wait half of the success condition cannot be observed at all (M6/V2-028). Provider limits are runner-local: the cost ledger lives outside the repository and the control plane cannot read it, so 'limits' is reported by the registry as a runner-local scope rather than as a value.")
}

// capPreviewOperation and capStablePromotion are both D1 by declaration
// (Google Cloud Run) and both additionally measured against the running
// process here rather than asserted.
func (fx *dogfood) capPreviewOperation(t *testing.T) {
	state := dogfoodCall(t, fx.client, http.MethodGet, fx.base+"/v1/release/state", fx.owner(), nil)
	t.Logf("measured: GET /v1/release/state on the shipped control-plane -> %d %+v", state.status, state.body)
	if state.status == http.StatusOK {
		t.Logf("E22-3 re-measured: the running process reports a release state, so the Preview version IS observable through the API surface: %+v", state.body)
	} else if state.status != http.StatusServiceUnavailable {
		t.Fatalf("GET /v1/release/state returned neither 200 nor 503: %d %+v", state.status, state.body)
	}
	t.Log("E22-3 re-measured and PARTLY FALSE at this commit: internal/release is imported by internal/application/release_surface.go and GET /v1/release/state exists (V2-066). The residual gap is wiring: cmd/control-plane attaches no ReleaseObserver, so the shipped binary answers 503 release_observer_not_configured and reports no version at all.")
	t.Log("cap-preview-operation recorded FAILED: the declared external systems include Google Cloud Run, so this capability's evidence belongs to the initial deploy gate D1 (conditions (ii) and (iv)) by declaration. Additionally, the declared success condition requires that on a failure new claims stop and routing returns to Stable; no route and no control mode does that, and the Loop channel rollback observed in channel-break-and-resume is a different layer (internal/update), not a Release Contract routing change.")
}

func (fx *dogfood) capStablePromotion(t *testing.T) {
	for _, probe := range []string{"/v1/release/promote", "/v1/release/state"} {
		r := dogfoodCall(t, fx.client, http.MethodPost, fx.base+probe, fx.owner(), map[string]any{"request_id": "v2-022-promote-probe"})
		if r.status == http.StatusOK {
			t.Fatalf("POST %s returned 200: a promotion route exists and this capability's eligibility must be re-judged", probe)
		}
		t.Logf("measured: POST %s -> %d (no promotion route on any surface)", probe, r.status)
	}
	t.Log("cap-stable-promotion recorded FAILED: the declared external systems include Google Cloud Run and GitHub, so this capability's evidence belongs to D1 by declaration. The release surface is read-only: there is deliberately no promote, no rollback and no SetPreview route anywhere, so the eight promotion conditions and the rollback target are not displayable to the owner through any API surface. No Stable release exists, none was promoted, and none was faked.")
}

// dogfoodCopyTree copies the repository tree into dst, skipping .git and
// build output. It exists so the deliberate defect of the broken build can
// only ever be applied to a copy under t.TempDir(); no tracked file is
// touched by this test.
func dogfoodCopyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		top := strings.Split(filepath.ToSlash(rel), "/")[0]
		if top == ".git" || top == "build" || top == ".devbox" || top == "tmp" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy the tree for the broken build: %v", err)
	}
}

// dogfoodSignBundle produces a real signed update bundle. The keypair is
// ephemeral and generated by this test, so the signature check really runs and
// no claim whatsoever is made about key management (V2-034 owns that).
func dogfoodSignBundle(t *testing.T, priv ed25519.PrivateKey, keyID, version, candidateID string, binary []byte, assembled release.AssembledBundle) update.Bundle {
	t.Helper()
	digest := sha256.Sum256(binary)
	manifest := update.Manifest{
		Schema: update.ManifestSchema, Version: version, OS: runtime.GOOS, Architecture: runtime.GOARCH,
		BinarySHA256: hex.EncodeToString(digest[:]), SchemaMin: 1, SchemaMax: 1,
		SigningKeyID: keyID, Algorithm: update.AlgorithmEd25519,
		BundleDigest: assembled.BundleDigest, CandidateID: candidateID,
		ContractRelease: assembled.Contract.Version, ContractDigest: assembled.ContractDigest,
		RunnerAPIMin: 1, RunnerAPIMax: 1,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signed := append(append([]byte(nil), raw...), digest[:]...)
	return update.Bundle{Manifest: raw, Binary: binary, Signature: ed25519.Sign(priv, signed)}
}

// channelBreakAndResume is the M5 completion condition "break the Preview on
// purpose and resume a Requirement on the previous Stable", at the Loop
// channel layer (internal/update), which is the only Preview/Stable routing
// that exists as running code.
func (fx *dogfood) channelBreakAndResume(t *testing.T) {
	fx.enroll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// --- the broken build: a copy of the tree in t.TempDir() with one
	// deliberate defect applied to the copy only. ---
	brokenTree := filepath.Join(t.TempDir(), "broken-tree")
	if err := os.MkdirAll(brokenTree, 0o755); err != nil {
		t.Fatal(err)
	}
	dogfoodCopyTree(t, fx.repoRoot, brokenTree)
	defectPath := filepath.Join(brokenTree, "internal", "api", "api.go")
	original, err := os.ReadFile(defectPath)
	if err != nil {
		t.Fatal(err)
	}
	const postMarker = "\tctx := application.ContextWithCaller(r.Context(), caller)\n\tswitch r.URL.Path {"
	if strings.Count(string(original), postMarker) != 1 {
		t.Fatalf("the POST dispatch marker was not found exactly once in the copied tree; the defect cannot be applied deterministically")
	}
	defected := strings.Replace(string(original), postMarker,
		"\tctx := application.ContextWithCaller(r.Context(), caller)\n\th.error(w, r, http.StatusInternalServerError, \"deliberate_dogfood_defect\", \"V2-022 deliberate defect: this build refuses every authenticated POST\")\n\tif ctx != nil {\n\t\treturn\n\t}\n\tswitch r.URL.Path {",
		1)
	if err := os.WriteFile(defectPath, []byte(defected), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.brokenDefect = "internal/api/api.go: the authenticated POST dispatch returns HTTP 500 deliberate_dogfood_defect before reaching any handler, so every POST /v1/... fails on this build -- capture and the Runner claim alike (applied only to the copied tree under t.TempDir(), never to a tracked file)"
	brokenBin := filepath.Join(t.TempDir(), "control-plane-broken")
	build := exec.Command("go", "build", "-o", brokenBin, "./cmd/control-plane")
	build.Dir = brokenTree
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the deliberately defective control-plane from the copied tree: %v\n%s", err, out)
	}

	goodBinary, err := os.ReadFile(fx.goodBin)
	if err != nil {
		t.Fatal(err)
	}
	brokenBinary, err := os.ReadFile(brokenBin)
	if err != nil {
		t.Fatal(err)
	}
	goodAssembled, err := release.AssembleFromRoot(fx.repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	brokenAssembled, err := release.AssembleFromRoot(brokenTree)
	if err != nil {
		t.Fatal(err)
	}

	// --- install both as real signed bundles through internal/update ---
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "v2-022-dogfood-ephemeral"
	anchors := update.NewAnchorSet(update.Ed25519Anchor(keyID, pub))
	channelRoot := filepath.Join(t.TempDir(), "channels")
	if err := os.MkdirAll(channelRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	state := update.NewState(1)
	fx.goodVersion = "0.1.1-dogfood-good"
	fx.brokenVersion = "0.1.2-dogfood-broken"
	goodCandidate := "candidate-" + fx.goodVersion
	brokenCandidate := "candidate-" + fx.brokenVersion
	now := time.Now().UTC()
	if _, err := update.InstallRecorded(channelRoot, state, dogfoodSignBundle(t, priv, keyID, fx.goodVersion, goodCandidate, goodBinary, goodAssembled), anchors, now); err != nil {
		t.Fatalf("install the good build as a real signed bundle: %v", err)
	}
	if _, err := update.InstallRecorded(channelRoot, state, dogfoodSignBundle(t, priv, keyID, fx.brokenVersion, brokenCandidate, brokenBinary, brokenAssembled), anchors, now); err != nil {
		t.Fatalf("install the broken build as a real signed bundle: %v", err)
	}
	// The signature check really runs: a bundle signed by a different key is
	// refused, which is what makes the two installs above meaningful.
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := update.Install(channelRoot, dogfoodSignBundle(t, otherPriv, keyID, "0.1.3-dogfood-unsigned", "candidate-unsigned", goodBinary, goodAssembled), anchors, 1); err == nil {
		t.Fatal("a bundle signed by a key the anchor does not hold was accepted")
	}

	// --- route the stable channel at the good version and serve it ---
	if err := update.Switch(channelRoot, state, update.SwitchRequest{Channel: "preview", Version: fx.goodVersion, Direction: update.SwitchForward, Reason: "v2-022 dogfood: route preview at the verified build", CandidateDigest: goodCandidate}, now); err != nil {
		t.Fatalf("switch preview to the good version: %v", err)
	}
	if err := update.Switch(channelRoot, state, update.SwitchRequest{Channel: "stable", Version: fx.goodVersion, Direction: update.SwitchForward, Reason: "v2-022 dogfood: advance stable onto the current preview target", CandidateDigest: goodCandidate}, now); err != nil {
		t.Fatalf("switch stable to the good version: %v", err)
	}
	stableVersion, err := update.ResolveChannel(channelRoot, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if stableVersion != fx.goodVersion {
		t.Fatalf("the stable channel resolves to %q, want %q", stableVersion, fx.goodVersion)
	}
	// update.Install writes its binary under the file name "runner" whatever
	// it contains: that is a naming artefact of internal/update and not a
	// claim about cmd/runner.
	stableBin := filepath.Join(update.VersionDir(channelRoot, stableVersion), "runner")
	stableBase, stableAlive, stopStable := startDogfoodProcess(t, stableBin, fx.installation, freeLocalPort(t), fx.ownerToken, fx.ownerEmail)
	waitForHealthz(t, fx.client, stableBase, stableAlive, 30*time.Second)

	preBreak := dogfoodCall(t, fx.client, http.MethodPost, stableBase+"/v1/requirements", fx.owner(), map[string]any{
		"request_id": "v2-022-channel:pre-break", "text": "V2-022 dogfood: captured before the Preview was broken on purpose",
	})
	if preBreak.status != http.StatusCreated {
		t.Fatalf("capture on the channel-routed stable build: expected 201, got %d: %+v", preBreak.status, preBreak.body)
	}
	preBreakID := stringField(preBreak.body, "requirement_id")
	preBreakVersion := domain.Version(floatField(preBreak.body, "version"))
	fx.capturedIDs = append(fx.capturedIDs, preBreakID)
	stopStable()
	dogfoodPoll(t, "the stable-channel process to be gone", 20*time.Second, func() bool { return !stableAlive() })

	// --- point the preview channel at the broken build and serve that ---
	if err := update.Switch(channelRoot, state, update.SwitchRequest{Channel: "preview", Version: fx.brokenVersion, Direction: update.SwitchForward, Reason: "v2-022 dogfood: break the Preview on purpose", CandidateDigest: brokenCandidate}, now); err != nil {
		t.Fatalf("switch preview to the broken version: %v", err)
	}
	previewVersion, err := update.ResolveChannel(channelRoot, "preview")
	if err != nil {
		t.Fatal(err)
	}
	if previewVersion != fx.brokenVersion {
		t.Fatalf("the preview channel resolves to %q, want the broken %q", previewVersion, fx.brokenVersion)
	}
	brokenBase, brokenAlive, stopBroken := startDogfoodProcess(t, filepath.Join(update.VersionDir(channelRoot, previewVersion), "runner"), fx.installation, freeLocalPort(t), fx.ownerToken, fx.ownerEmail)
	waitForHealthz(t, fx.client, brokenBase, brokenAlive, 30*time.Second)
	broken := dogfoodCall(t, fx.client, http.MethodPost, brokenBase+"/v1/requirements", fx.owner(), map[string]any{
		"request_id": "v2-022-channel:on-broken", "text": "this capture must fail on the broken Preview",
	})
	if broken.status != http.StatusInternalServerError {
		t.Fatalf("the broken Preview accepted a capture: expected 500, got %d: %+v", broken.status, broken.body)
	}
	if got := stringField(broken.body, "error"); got != "deliberate_dogfood_defect" {
		t.Fatalf("the broken Preview failed for the wrong reason: %+v", broken.body)
	}
	t.Logf("the deliberate defect was observed over real HTTP on the channel-routed Preview: %d %s", broken.status, dogfoodErrorCode(broken.body))
	resumeIncrement, resumeIncrementVersion := fx.planAndPrepare(t, "v2-022-channel-resume", preBreakID, preBreakVersion)
	brokenClaim := dogfoodCall(t, fx.client, http.MethodPost, brokenBase+"/v1/runner/claims:acquire", fx.runner(), map[string]any{
		"request_id": "v2-022-channel:claim-on-broken", "increment_id": resumeIncrement,
		"expected_increment_version": int(resumeIncrementVersion), "control_revision": fx.controlRevision,
		"target": fx.dogfoodTarget(preBreakID, resumeIncrement),
	})
	if brokenClaim.status == http.StatusOK {
		t.Fatalf("a Runner claim against the broken Preview succeeded: %+v", brokenClaim.body)
	}
	t.Logf("a Runner claim against the broken Preview failed over real HTTP, as required: %d %v", brokenClaim.status, brokenClaim.body["error"])
	stopBroken()
	dogfoodPoll(t, "the broken-preview process to be gone", 20*time.Second, func() bool { return !brokenAlive() })

	// --- roll the channel back to the previously verified version ---
	if err := update.Switch(channelRoot, state, update.SwitchRequest{Channel: "preview", Version: fx.goodVersion, Direction: update.SwitchForward, Reason: "v2-022 dogfood: return the Preview channel to the previously verified version", CandidateDigest: goodCandidate}, now); err != nil {
		t.Fatalf("return the preview channel to the verified version: %v", err)
	}
	restored, err := update.ResolveChannel(channelRoot, "preview")
	if err != nil {
		t.Fatal(err)
	}
	if restored != fx.goodVersion {
		t.Fatalf("the preview channel resolves to %q after the rollback, want %q", restored, fx.goodVersion)
	}
	if _, err := os.Lstat(update.VersionDir(channelRoot, fx.brokenVersion)); err != nil {
		t.Fatalf("update.Switch deleted the prior target; rollback is the same operation with the prior verified version: %v", err)
	}
	fx.rollbackObserved = true

	// --- the Requirement captured before the break is still in the same
	// emulator canonical state, and advances to a terminal state under the
	// restored build with one real claude invocation. ---
	client, err := cloudfirestore.NewClient(context.Background(), liveLocalEmulatorProject)
	if err != nil {
		t.Fatalf("connect directly to the Firestore emulator: %v", err)
	}
	defer client.Close()
	restoredBase, restoredAlive, _ := startDogfoodProcess(t, filepath.Join(update.VersionDir(channelRoot, restored), "runner"), fx.installation, freeLocalPort(t), fx.ownerToken, fx.ownerEmail)
	waitForHealthz(t, fx.client, restoredBase, restoredAlive, 30*time.Second)
	survivor := dogfoodCall(t, fx.client, http.MethodGet, restoredBase+"/v1/requirements/"+preBreakID, fx.owner(), nil)
	if survivor.status != http.StatusOK {
		t.Fatalf("the Requirement captured before the break is not present under the restored build: %d %+v", survivor.status, survivor.body)
	}

	claim := dogfoodCall(t, fx.client, http.MethodPost, restoredBase+"/v1/runner/claims:acquire", fx.runner(), map[string]any{
		"request_id": "v2-022-channel:claim-after-restore", "increment_id": resumeIncrement,
		"expected_increment_version": int(resumeIncrementVersion), "control_revision": fx.controlRevision,
		"target": fx.dogfoodTarget(preBreakID, resumeIncrement),
	})
	if claim.status != http.StatusOK {
		t.Fatalf("claim under the restored build: expected 200, got %d: %+v", claim.status, claim.body)
	}
	executionID := stringField(claim.body, "execution_id")
	leaseID := stringField(claim.body, "lease_id")
	fence := int(floatField(claim.body, "fencing_token"))
	started := dogfoodCall(t, fx.client, http.MethodPost, restoredBase+"/v1/executions/"+executionID+":start", fx.runner(), map[string]any{
		"request_id": "v2-022-channel:start", "expected_execution_version": 1, "control_revision": fx.controlRevision, "provider": "claude",
	})
	if started.status != http.StatusOK {
		t.Fatalf("start under the restored build: %d %+v", started.status, started.body)
	}
	result, err := fx.dogfoodInvocation(t, ctx, "V2-022-resume-after-channel-rollback", executionID, preBreakID, resumeIncrement,
		"Do not use any tools. Reply with exactly the single word RESUMED and nothing else.")
	if err != nil {
		t.Fatalf("the resumed Requirement's real claude invocation failed: %v", err)
	}
	if !result.Succeeded {
		// Only the classification is reported. No response text, no excerpt.
		class, code, retryable := "", "", false
		if result.Failure != nil {
			class, code, retryable = string(result.Failure.Class), result.Failure.Code, result.Failure.Retryable
		}
		t.Fatalf("the resumed invocation did not succeed: failure class=%q code=%q retryable=%v", class, code, retryable)
	}
	if r := dogfoodCall(t, fx.client, http.MethodPost, restoredBase+"/v1/runner/checkpoints", fx.runner(), map[string]any{
		"request_id": "v2-022-channel:checkpoint", "execution_id": executionID, "lease_id": leaseID, "fencing_token": fence, "control_revision": fx.controlRevision,
	}); r.status != http.StatusOK {
		t.Fatalf("checkpoint under the restored build: %d %+v", r.status, r.body)
	}
	accepted := dogfoodCall(t, fx.client, http.MethodPost, restoredBase+"/v1/executions/result", fx.runner(), map[string]any{
		"request_id": "v2-022-channel:result", "execution_id": executionID, "lease_id": leaseID,
		"expected_execution_version": int(floatField(started.body, "version")), "fencing_token": fence,
		"control_revision": fx.controlRevision, "succeeded": true, "target": fx.dogfoodTarget(preBreakID, resumeIncrement),
		"provider_observation": map[string]any{"name": "claude", "stopped_for_inspection": false},
	})
	if accepted.status != http.StatusOK {
		t.Fatalf("result under the restored build: %d %+v", accepted.status, accepted.body)
	}
	if got := stringField(accepted.body, "status"); got != "succeeded" {
		t.Fatalf("the resumed Execution's terminal status = %q, want succeeded", got)
	}
	fx.resumeObserved = true

	t.Log("channel-break-and-resume: this is a Loop CHANNEL rollback and resume at the self-host grade release-contract.md section 3 anticipates while D1 is unpassed. It is NOT a Release Contract Stable release: no Stable release exists, none was promoted and none was faked.")
	t.Logf("the signing key was ephemeral and generated by this test, so no claim is made about key management (V2-034 owns that). The deliberate defect was %s", fx.brokenDefect)
	t.Log("update.Install writes its binary under the file name \"runner\" regardless of what it contains: that is a naming artefact of internal/update, not a claim about cmd/runner.")
	t.Log("measured limit of update.Switch: SwitchRollback is a stable-channel-only direction, so returning the preview channel to the previously verified version is expressed as a forward move naming that version's own gate-passed candidate. The prior target was not deleted, which is what makes the return possible.")
}

func dogfoodErrorCode(body map[string]any) string { return stringField(body, "error") }

// capLoopSelfUpdate records cap-loop-self-update FAILED, referring to the
// channel journey for what WAS observed.
func (fx *dogfood) capLoopSelfUpdate(t *testing.T) {
	if !fx.rollbackObserved || !fx.resumeObserved {
		t.Fatalf("the channel journey did not complete, so nothing may be claimed here: rollback=%v resume=%v", fx.rollbackObserved, fx.resumeObserved)
	}
	t.Logf("what WAS observed (channel-break-and-resume): two real signed bundles installed through internal/update, the preview channel switched to a deliberately defective build, the failure and the Runner claim refusal observed over real HTTP, the channel returned to the previously verified version %s, and the Requirement captured before the break resumed from the same emulator canonical state to a terminal state with a real claude invocation.", fx.goodVersion)
	t.Log("cap-loop-self-update recorded FAILED: the declared external systems are Google Cloud Run, Git, GitHub, Firestore and the local Runner machine. Cloud Run makes this capability's evidence D1 by declaration (conditions (ii) and (iv)), and Git and GitHub were not connected to because this task's declared side-effect surface excludes every forge and remote. What was observed is a Loop channel switch on the owner's own machine, not a version change of a deployed service.")
}

// dogfoodContractIneligible reads the real contract and returns the capability
// ids whose evidence_ids is empty. That set is exactly the input the promotion
// gate refuses for, so it is measured from the contract rather than assumed.
func (fx *dogfood) dogfoodContractIneligible(t *testing.T) (ineligible, eligible []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fx.repoRoot, "contracts", "release-contract", "foundation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Capabilities []struct {
			ID          string   `json:"id"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	for _, c := range contract.Capabilities {
		if len(c.EvidenceIDs) == 0 {
			ineligible = append(ineligible, c.ID)
			continue
		}
		eligible = append(eligible, c.ID)
	}
	return ineligible, eligible
}

// promotabilityNegative is the real-tree half: the Foundation candidate, with
// no fabricated evidence, must still be refused, and the measured set of
// capability ids the gate refuses for is reported as a list.
func (fx *dogfood) promotabilityNegative(t *testing.T) {
	assembled, err := release.AssembleFromRoot(fx.repoRoot)
	if err != nil {
		t.Fatalf("assemble the real Foundation tree: %v", err)
	}
	releaseID, err := domain.NewReleaseID("candidate-v2-022-real-tree")
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]domain.CapabilityTarget{}
	for _, capability := range assembled.Contract.Capabilities {
		targets[capability] = domain.CapabilityTarget{Target: dogfoodEnvClass, Provider: "claude"}
	}
	_, candidate, err := release.AssembleCandidate(fx.repoRoot, release.CandidateInput{
		ReleaseID: releaseID, CandidateID: releaseID, CandidateDigest: "candidate-v2-022-real-tree",
		Version: 1, Status: domain.ReleaseExercising, CapabilityTargets: targets,
		RollbackEvidence: fx.rollbackObserved, ResumeEvidence: fx.resumeObserved,
		ExpectedControlRevision: 1, FencingToken: 1,
		Evidence: nil, // no fabricated evidence for the real tree
	})
	if err != nil {
		t.Fatalf("AssembleCandidate on the real tree: %v", err)
	}
	if len(candidate.Capabilities) != 12 {
		t.Fatalf("the real candidate declares %d capabilities, want 12", len(candidate.Capabilities))
	}
	err = candidate.CanPromote()
	if err == nil {
		t.Fatal("the real Foundation candidate must NOT be promotable")
	}
	if !strings.Contains(err.Error(), "capability") {
		t.Fatalf("the refusal does not name a missing capability: %v", err)
	}
	ineligible, eligible := fx.dogfoodContractIneligible(t)
	named := false
	for _, id := range ineligible {
		if strings.Contains(err.Error(), id) {
			named = true
		}
	}
	if !named {
		t.Fatalf("the refusal %q names no capability whose evidence_ids is empty; the refusal set measurement below would be vacuous", err)
	}
	// The real-tree candidate's own digests, derived from source bytes and
	// never supplied by a caller. They are what the evidence record's
	// artifact_refs carry, so they are logged here rather than recomputed by
	// hand anywhere else.
	t.Logf("real-tree candidate digests at this commit: bundle=%s contract=%s docs=%s", candidate.BundleDigest, candidate.ContractDigest, candidate.DocsDigest)
	t.Logf("real-tree bundle member count: %d", len(assembled.Members))
	t.Logf("real-tree candidate refusal (expected): %v", err)
	t.Logf("measured refusal set (capability ids the gate refuses for, i.e. every id whose evidence_ids is empty at this commit), as a list of %d: %v", len(ineligible), ineligible)
	t.Logf("measured eligible set (capability ids carrying an evidence id at this commit), as a list of %d: %v", len(eligible), eligible)
	if len(ineligible)+len(eligible) != 12 {
		t.Fatalf("the measured refusal and eligible sets do not partition the twelve capabilities: %d + %d", len(ineligible), len(eligible))
	}
	permitted := map[string]bool{}
	for _, id := range dogfoodEligible {
		permitted[id] = true
	}
	for _, id := range eligible {
		if !permitted[id] {
			t.Fatalf("capability %q carries an evidence id but is not in the A16 eligible set %v; the refusal set measurement would be reporting a claim this task is not permitted to make", id, dogfoodEligible)
		}
	}
	t.Log("the real-tree candidate remains NOT promotable; no evidence was fabricated for it and the real-tree assertion was not weakened.")
}

// dogfoodSyntheticTree builds a hermetic source tree under t.TempDir() whose
// contract declares only capabilityIDs, resolving all seven bundle roles.
func dogfoodSyntheticTree(t *testing.T, capabilityIDs []string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, data []byte) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type capability struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Status      string   `json:"status"`
		EvidenceIDs []string `json:"evidence_ids"`
	}
	caps := make([]capability, 0, len(capabilityIDs))
	anchors := ""
	for _, id := range capabilityIDs {
		caps = append(caps, capability{ID: id, Name: id, Status: "preview", EvidenceIDs: []string{"ev-v2-022-release-live-dogfood"}})
		anchors += "<a id=\"" + id + "\"></a>\n\n## " + id + "\n\nsynthetic\n\n"
	}
	contract := map[string]any{
		"schema_version": "v1", "id": "rc-synthetic-v2-022", "kind": "release-contract",
		"created_at": "2026-08-26T00:00:00Z", "correlation_id": "v2-022-promotability-positive",
		"release": "0.1.0-preview.1-synthetic", "capabilities": caps,
		"verification": []string{"go test"},
		"rollback":     map[string]string{"procedure": "git revert", "target": "main"},
	}
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	write("contracts/release-contract/foundation.json", raw)
	write("contracts/schemas/dummy.json", []byte(`{"schema_version":"v1"}`))
	write("contracts/openapi/openapi-v1.yaml", []byte("openapi: 3.0.0\n"))
	write("ci/components.json", []byte(`{"version":1}`))
	write("go.mod", []byte("module synthetic\n"))
	write("go.sum", []byte("\n"))
	write("devbox.lock", []byte("{}"))
	write("firestore.indexes.json", []byte("{}"))
	write("devbox.json", []byte("{}"))
	write("firebase.json", []byte("{}"))
	write("docs/preview/index.md", []byte("# Preview\n\nRelease: 0.1.0-preview.1-synthetic\n"))
	write("docs/preview/capabilities.md", []byte("# Capabilities\n\n"+anchors))
	write("docs/stable/index.md", []byte("# Stable\n\nStable release: none\n"))
	return root
}

// promotabilityPositive is the hermetic half: the same gate promotes when the
// evidence is real, using digests this run itself produced.
func (fx *dogfood) promotabilityPositive(t *testing.T) {
	_, eligible := fx.dogfoodContractIneligible(t)
	if len(eligible) == 0 {
		eligible = dogfoodEligible
		t.Logf("the real contract carries no evidence id yet (commit A); the positive control therefore declares the A16 eligible set %v", eligible)
	}
	root := dogfoodSyntheticTree(t, eligible)
	releaseID, err := domain.NewReleaseID("candidate-v2-022-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]domain.CapabilityTarget{}
	for _, capability := range eligible {
		targets[capability] = domain.CapabilityTarget{Target: dogfoodEnvClass, Provider: "claude"}
	}
	input := release.CandidateInput{
		ReleaseID: releaseID, CandidateID: releaseID, CandidateDigest: "candidate-v2-022-synthetic",
		Version: 1, Status: domain.ReleaseExercising, CapabilityTargets: targets,
		RollbackEvidence: fx.rollbackObserved, ResumeEvidence: fx.resumeObserved,
		ExpectedControlRevision: 1, FencingToken: 1,
	}
	if !input.RollbackEvidence || !input.ResumeEvidence {
		t.Fatal("the positive control's rollback/resume booleans come from the channel journey's actual outcome; that journey did not complete")
	}
	bundle, assembled, err := release.AssembleBundle(root, "synthetic-foundation", input, time.Unix(1, 0))
	if err != nil {
		t.Fatalf("assemble the synthetic bundle: %v", err)
	}
	evidence := make([]domain.CapabilityEvidence, 0, len(eligible))
	for _, capability := range eligible {
		evidence = append(evidence, domain.CapabilityEvidence{
			Capability: capability, CandidateID: bundle.Candidate.CandidateID, CandidateDigest: bundle.Candidate.CandidateDigest,
			BundleDigest: assembled.BundleDigest, ContractDigest: bundle.Candidate.ContractDigest, DocsDigest: bundle.Candidate.DocsDigest,
			Digest: "ev-v2-022-release-live-dogfood:" + capability, Verified: true, Fresh: true,
			Target: dogfoodEnvClass, Provider: "claude",
		})
	}
	bundle.Candidate.Evidence = evidence
	promotable := bundle.Snapshot()
	if err := promotable.Candidate.CanPromote(); err != nil {
		t.Fatalf("the hermetic positive control is not promotable: %v", err)
	}
	router := release.NewRouter()
	// Router.Promote advances the stable route only onto what the preview
	// route currently points at, so the candidate is routed as Preview first.
	// That mirrors the channel-layer rule update.Switch enforces and is not a
	// weakening of the gate: PromotionGate still runs in full below.
	if err := router.SetPreview("synthetic-foundation", bundle.Candidate.CandidateDigest); err != nil {
		t.Fatalf("route the synthetic candidate as Preview: %v", err)
	}
	pipeline := release.NewPipeline(release.NewMemoryStore(), router, root)
	control := domain.EffectiveControlResult{Found: true, Mode: domain.ControlAllow, Revision: 1}
	permit, err := domain.Permit(control, domain.PermitRequest{Kind: domain.PermitPromotion, ControlRevision: 1, FencingToken: 1, ExpectedFencingToken: 1, Resource: "release"})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := pipeline.Promote(promotable, control, permit)
	if err != nil {
		t.Fatalf("the hermetic positive control was refused by the pipeline: %v", err)
	}
	if gate.Candidate.Status != domain.ReleaseStable {
		t.Fatalf("the promoted candidate's status = %q, want %q", gate.Candidate.Status, domain.ReleaseStable)
	}
	routeBefore, _ := router.Get("synthetic-foundation")
	t.Logf("hermetic positive control: the same gate promotes when the evidence is real, from this run's own digests (bundle=%s docs=%s)", assembled.BundleDigest, bundle.Candidate.DocsDigest)

	// Docs drift: flip one byte of one documentation member of the synthetic
	// tree and assert the same candidate is refused and the router is unchanged.
	docPath := filepath.Join(root, "docs", "preview", "index.md")
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	flipped := append([]byte(nil), data...)
	flipped[0] ^= 0x20
	if err := os.WriteFile(docPath, flipped, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Promote(promotable, control, permit); err == nil {
		t.Fatal("the same candidate was still promoted after one byte of one documentation member drifted")
	} else {
		t.Logf("docs drift refusal (expected): %v", err)
	}
	routeAfter, _ := router.Get("synthetic-foundation")
	if routeAfter != routeBefore {
		t.Fatalf("the router changed on a refused promotion: before=%+v after=%+v", routeBefore, routeAfter)
	}
	t.Log("the real tree was NOT given fabricated evidence and the real-tree assertion above was not weakened; this control is hermetic and lives entirely under t.TempDir().")
}

// ledgerSnapshot transcribes the full V2-022 cost ledger table and the halted
// flag. Usage is reported as input, output, cache-read and cache-creation
// counts. No prompt and no response, in any form, appears here.
func (fx *dogfood) ledgerSnapshot(t *testing.T) {
	snapshot, err := fx.ledger.Snapshot()
	if err != nil {
		t.Fatalf("read the V2-022 cost ledger: %v", err)
	}
	t.Logf("V2-022 cost ledger: path=%s provider=%s task_id=%s halted=%v invocations=%d settled_total_usd=%.4f reserved_total_usd=%.4f limits(max_invocations=%d max_total_cost_usd=%.2f worst_case_reservation_usd=%.2f) single_invocation_anomaly_threshold_usd=%.2f",
		fx.ledger.Path, fx.ledger.Provider, fx.ledger.TaskID, snapshot.Halted, snapshot.InvocationCount,
		snapshot.SettledTotalUSD, snapshot.ReservedTotalUSD,
		fx.record.Limits.MaxInvocations, fx.record.Limits.MaxTotalCostUSD, fx.record.Limits.WorstCaseReservationUSD,
		agenticrunner.CostAnomalyThresholdUSD)
	for _, e := range snapshot.Entries {
		t.Logf("ledger entry: sequence=%d purpose=%s state=%s reserved_usd=%.4f actual_usd=%.4f session_id=%s input_count=%d output_count=%d cache_read_count=%d cache_creation_count=%d duration_api_ms=%d duration_ms=%d num_turns=%d started_at=%s finished_at=%s",
			e.Sequence, e.Purpose, e.State, e.ReservedUSD, e.ActualUSD, e.SessionID,
			e.InputCount, e.OutputCount, e.CacheReadCount, e.CacheCreationCount,
			e.DurationAPIMS, e.DurationMS, e.NumTurns,
			e.StartedAt.UTC().Format(time.RFC3339), e.FinishedAt.UTC().Format(time.RFC3339))
	}
	if snapshot.Halted {
		t.Fatal("the V2-022 ledger's halted flag is set: this is a stop for inspection, neither a success nor a capability failure. Do not edit the record's limits, do not edit or reinitialise the ledger, do not clear halted.")
	}
	if snapshot.InvocationCount >= fx.record.Limits.MaxInvocations {
		t.Fatalf("the V2-022 ledger reached its max_invocations runaway threshold (%d): this is a stop for inspection, not a failure of any capability", fx.record.Limits.MaxInvocations)
	}
	settled := 0
	for _, e := range snapshot.Entries {
		if e.State == "settled" && e.SessionID != "" {
			settled++
		}
	}
	if settled == 0 {
		t.Fatal("no settled invocation with a provider-issued session_id was recorded; the live provider half of this exercise did not happen")
	}
	t.Logf("invocation purposes this process accounted: %v", fx.invocationPurposes)
	t.Logf("environment identifiers for every check in this record: %s", fx.ids)
}
