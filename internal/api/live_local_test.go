package api_test

// TestControlPlanePreviewLocalLive is the preview-local (release-contract.md
// §3) live exercise for the rewritten M2 completion conditions (V2-051). It
// builds cmd/control-plane and runs it as a real OS process on
// 127.0.0.1, against a real Firestore emulator (started by
// scripts/firestore-emulator.sh, which must wrap this test invocation so
// FIRESTORE_EMULATOR_HOST is already set), and drives the whole owner
// journey over real HTTP. Nothing here is an in-process fake, and this test
// never touches Google Cloud: it must run only via
//
//	devbox run --pure -e AGENTIC_LOOP_LIVE_LOCAL=1 -- \
//	  scripts/firestore-emulator.sh go test -count=1 -v \
//	  -run TestControlPlanePreviewLocalLive ./internal/api
//
// make check must not run this: it is gated on AGENTIC_LOOP_LIVE_LOCAL=1 and
// skips (Fatal, not Skip, if the emulator prerequisite is missing while the
// gate is set, so a misconfigured live run cannot be silently counted as
// having exercised nothing).
//
// The four conditions the roadmap defers to the initial deploy gate (D1) are
// out of scope and are never asserted here: the IAP authentication boundary,
// idle scale-to-zero, real Firestore permissions/contention, and the
// approved-plan-digest deploy path.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	cloudfirestore "cloud.google.com/go/firestore"

	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	agenticfirestore "github.com/takushi/agentic-loop-foundation/v2/internal/store/firestore"
)

const liveLocalEmulatorProject = "agentic-loop-test"

func repoRootForLiveTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the module root (go.mod) above internal/api")
		}
		dir = parent
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not reserve a local port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitForHealthz polls the observable readiness condition -- /healthz
// returning 200 -- on a bounded deadline instead of sleeping a fixed
// duration. It fails the test (not a silent skip) if the process is not
// ready in time, or if the process already exited.
func waitForHealthz(t *testing.T, client *http.Client, base string, alive func() bool, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(end) {
		if alive != nil && !alive() {
			t.Fatalf("control-plane process exited before becoming healthy (last error: %v)", lastErr)
		}
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("unexpected /healthz status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("control-plane did not become healthy within %s: %v", deadline, lastErr)
}

type liveResponse struct {
	status int
	body   map[string]any
	header http.Header
}

func liveCall(t *testing.T, client *http.Client, method, url, bearer string, payload any) liveResponse {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, &body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
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

// startLiveControlPlane starts the built control-plane binary as a real
// process, in its own process group (runner.ProcessSupervisor, the same
// mechanism the Runner's process supervisor uses), against the given port,
// installation, and Firestore emulator. It returns the base URL and an
// "alive" probe; cancelling the returned context stops the process
// (SIGTERM, then SIGKILL after a bounded grace period) deterministically.
func startLiveControlPlane(t *testing.T, binPath, installation string, port int, ownerToken, ownerEmail string) (base string, alive func() bool) {
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
	supervisor := runner.ProcessSupervisor{TermGrace: 5 * time.Second, Env: env}
	done := make(chan error, 1)
	var exited bool
	go func() {
		err := supervisor.Run(ctx, []string{binPath})
		exited = true
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("control-plane process did not exit within 10s of stop; process group may be leaked")
		}
	})
	return fmt.Sprintf("http://127.0.0.1:%d", port), func() bool { return !exited }
}

func TestControlPlanePreviewLocalLive(t *testing.T) {
	if os.Getenv("AGENTIC_LOOP_LIVE_LOCAL") != "1" {
		t.Skip("set AGENTIC_LOOP_LIVE_LOCAL=1 to run the preview-local live exercise (real control-plane process + real Firestore emulator); not part of make check")
	}
	emulatorHost := strings.TrimSpace(os.Getenv("FIRESTORE_EMULATOR_HOST"))
	if emulatorHost == "" {
		t.Fatal("AGENTIC_LOOP_LIVE_LOCAL=1 but FIRESTORE_EMULATOR_HOST is unset; run this test wrapped by scripts/firestore-emulator.sh, e.g. " +
			"devbox run --pure -e AGENTIC_LOOP_LIVE_LOCAL=1 -- scripts/firestore-emulator.sh go test -count=1 -v -run TestControlPlanePreviewLocalLive ./internal/api")
	}
	t.Logf("preview-local live exercise: Firestore emulator host=%s project=%s", emulatorHost, liveLocalEmulatorProject)

	repoRoot := repoRootForLiveTest(t)
	binPath := filepath.Join(t.TempDir(), "control-plane-live")
	build := exec.Command("go", "build", "-o", binPath, "github.com/takushi/agentic-loop-foundation/v2/cmd/control-plane")
	build.Dir = repoRoot
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/control-plane failed: %v\n%s", err, out)
	}

	installation := "v2-051-live-local-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const ownerEmail = "owner@example.com"
	const ownerToken = "v2-051-live-local-owner-token" //nolint:gosec // fixture credential for a throwaway local emulator install
	client := &http.Client{Timeout: 15 * time.Second}

	port1 := freeLocalPort(t)
	base, alive := startLiveControlPlane(t, binPath, installation, port1, ownerToken, ownerEmail)
	waitForHealthz(t, client, base, alive, 20*time.Second)

	// --- owner authentication boundary (session/token, not IAP; IAP is D1) ---
	if r := liveCall(t, client, http.MethodGet, base+"/v1/requirements", "", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no bearer token, got %d: %+v", r.status, r.body)
	}
	if r := liveCall(t, client, http.MethodGet, base+"/v1/requirements", "not-a-real-token", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("expected 401 with an unknown bearer token, got %d: %+v", r.status, r.body)
	}

	// --- issue registration (capture), no GitHub Issue involved ---
	const requirementText = "V2-051 preview-local live journey requirement"
	capture := liveCall(t, client, http.MethodPost, base+"/v1/requirements", ownerToken, map[string]any{
		"request_id": "live-capture-1",
		"text":       requirementText,
	})
	if capture.status != http.StatusCreated {
		t.Fatalf("capture: expected 201, got %d: %+v", capture.status, capture.body)
	}
	requirementID, _ := capture.body["requirement_id"].(string)
	if requirementID == "" {
		t.Fatalf("capture response missing requirement_id: %+v", capture.body)
	}
	t.Logf("captured requirement_id=%s", requirementID)

	// --- Backlog display ---
	backlog := liveCall(t, client, http.MethodGet, base+"/v1/requirements?page_size=25", ownerToken, nil)
	if backlog.status != http.StatusOK {
		t.Fatalf("backlog list: expected 200, got %d: %+v", backlog.status, backlog.body)
	}
	if !backlogContains(backlog.body, requirementID) {
		t.Fatalf("backlog did not contain requirement_id=%s: %+v", requirementID, backlog.body)
	}

	// --- Backlog paging over the cursor the route itself hands out (V2-079).
	// This is the route-level proof of the observable outcome: a cursor the
	// Backlog just returned is accepted by the very next request against real
	// persistence, so a Backlog larger than one page can be paged at all. Two
	// more Requirements are captured first so the Backlog cannot fit in a
	// single page_size=1 page, then every next_cursor is fed straight back
	// until a page reports none. ---
	captured := []string{requirementID}
	for i, text := range []string{"V2-079 second Backlog row", "V2-079 third Backlog row"} {
		more := liveCall(t, client, http.MethodPost, base+"/v1/requirements", ownerToken, map[string]any{
			"request_id": fmt.Sprintf("live-capture-page-%d", i+2),
			"text":       text,
		})
		if more.status != http.StatusCreated {
			t.Fatalf("capture #%d for the paging walk: expected 201, got %d: %+v", i+2, more.status, more.body)
		}
		id, _ := more.body["requirement_id"].(string)
		if id == "" {
			t.Fatalf("capture #%d response missing requirement_id: %+v", i+2, more.body)
		}
		captured = append(captured, id)
	}
	walkBacklogWithTheRouteOwnCursor(t, client, base, ownerToken, captured)

	// --- Requirement display ---
	detail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+requirementID, ownerToken, nil)
	if detail.status != http.StatusOK {
		t.Fatalf("requirement detail: expected 200, got %d: %+v", detail.status, detail.body)
	}
	if got, _ := detail.body["original_text"].(string); got != requirementText {
		t.Fatalf("requirement detail original_text = %q, want %q", got, requirementText)
	}
	if got, _ := detail.body["status"].(string); got != "captured" {
		t.Fatalf("requirement detail status = %q, want captured", got)
	}

	// --- V2-082: the Requirement LEAVES captured, against the real binary,
	// the real Firestore emulator and real HTTP. This is the executed proof of
	// the one edge this task adds. Measured on the parent tree, the set of
	// Requirement statuses reachable through every application command was the
	// single element {captured}: nothing outside a test ever issued the
	// captured->framing transition, so the needs-input question -- which the
	// domain admits from framing, active and evaluating only -- could not be
	// asked at all.
	//
	// Boundary, stated rather than implied. The segment executed here against
	// real persistence is exactly capture -> start-framing -> the detail
	// reports framing. The rest of the chain (:request-input, the detail
	// showing the question, :answer-input) needs a RUNNER session, and this
	// file composes owner-authenticated routes only and has no runner
	// enrolment; that segment is asserted over the composed handler in
	// internal/api/api_test.go and is left to the M5 re-dogfood over real
	// everything. No Provider is involved in anything below.
	detailVersionBefore, _ := detail.body["version"].(float64)
	if detailVersionBefore == 0 {
		t.Fatalf("requirement detail carried no version: %+v", detail.body)
	}
	framing := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+requirementID+":start-framing", ownerToken, map[string]any{
		"request_id":                   "live-start-framing-1",
		"expected_requirement_version": detailVersionBefore,
	})
	if framing.status != http.StatusOK {
		t.Fatalf("start-framing: expected 200, got %d: %+v", framing.status, framing.body)
	}
	if got, _ := framing.body["requirement_id"].(string); got != requirementID {
		t.Fatalf("start-framing named requirement_id=%q, want %q", got, requirementID)
	}
	if got, _ := framing.body["status"].(string); got != "framing" {
		t.Fatalf("start-framing response status = %q, want framing", got)
	}
	framingVersion, _ := framing.body["version"].(float64)
	if framingVersion != detailVersionBefore+1 {
		t.Fatalf("start-framing response version = %v, want %v", framingVersion, detailVersionBefore+1)
	}
	// Read it back through the pre-existing detail route: the status the real
	// emulator now holds is framing, at the incremented version.
	framedDetail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+requirementID, ownerToken, nil)
	if framedDetail.status != http.StatusOK {
		t.Fatalf("requirement detail after start-framing: expected 200, got %d: %+v", framedDetail.status, framedDetail.body)
	}
	if got, _ := framedDetail.body["status"].(string); got != "framing" {
		t.Fatalf("requirement detail status after start-framing = %q, want framing; the Requirement did not leave captured in real persistence", got)
	}
	if got, _ := framedDetail.body["version"].(float64); got != framingVersion {
		t.Fatalf("requirement detail version after start-framing = %v, want %v", got, framingVersion)
	}
	t.Logf("V2-082 live-local: requirement_id=%s left captured for framing at version %v against the real Firestore emulator", requirementID, framingVersion)
	// A SECOND identical request with the SAME request_id replays the prior
	// response instead of transitioning again: the version does not move, and
	// the stored status does not move either.
	replay := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+requirementID+":start-framing", ownerToken, map[string]any{
		"request_id":                   "live-start-framing-1",
		"expected_requirement_version": detailVersionBefore,
	})
	if replay.status != http.StatusOK {
		t.Fatalf("start-framing replay: expected 200, got %d: %+v", replay.status, replay.body)
	}
	if got, _ := replay.body["version"].(float64); got != framingVersion {
		t.Fatalf("start-framing replay version = %v, want the prior %v; the replay transitioned again", got, framingVersion)
	}
	if got, _ := replay.body["status"].(string); got != "framing" {
		t.Fatalf("start-framing replay status = %q, want framing", got)
	}
	replayedDetail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+requirementID, ownerToken, nil)
	if replayedDetail.status != http.StatusOK {
		t.Fatalf("requirement detail after the replay: expected 200, got %d: %+v", replayedDetail.status, replayedDetail.body)
	}
	if got, _ := replayedDetail.body["version"].(float64); got != framingVersion {
		t.Fatalf("the idempotent replay moved the stored version to %v, want %v", got, framingVersion)
	}
	// A runner-less boundary check that costs nothing: framing again with a
	// FRESH request id is refused by the domain transition table, because
	// captured is the only admitted source.
	again := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+requirementID+":start-framing", ownerToken, map[string]any{
		"request_id":                   "live-start-framing-2",
		"expected_requirement_version": framingVersion,
	})
	if again.status != http.StatusBadRequest {
		t.Fatalf("framing an already-framing Requirement: expected 400, got %d: %+v", again.status, again.body)
	}

	// --- V2-084: the Requirement reaches READY, against the real binary, the
	// real Firestore emulator and real HTTP. This is the executed proof of edge
	// one. Measured on the parent tree, domain.RequirementReadyCommand's only
	// non-test issuer was Service.AnswerHumanInput, so `ready` -- the only
	// status the scheduler treats as schedulable -- was reachable ONLY by asking
	// a Requirement a question and having the owner answer it. A Requirement
	// that needed no question could not become executable at all.
	//
	// BOUNDARY, stated rather than blurred. The segment executed here against
	// real persistence is exactly capture -> :start-framing -> :complete-framing
	// -> the detail reports ready at the capture version plus two, plus the
	// idempotent replay. The claim-to-active segment -- edge two -- is NOT
	// proven against real persistence, because Service.Claim requires a verified
	// RUNNER session and this file composes LocalOwnerBearerAuthenticator from
	// AGENTIC_LOOP_LOCAL_OWNER_TOKENS and drives owner-authenticated routes
	// only, with no runner enrolment. That is a LIMIT OF THIS TASK'S
	// AUTHORISATIONS rather than a gap in the assertion: edge two is asserted
	// over the composed handler in internal/api/api_test.go's
	// TestTheWholeChainReachesReadyAndTheClaimCarriesTheParentToActive, and no
	// runner enrolment is added to this file. No Provider is involved here.
	completed := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+requirementID+":complete-framing", ownerToken, map[string]any{
		"request_id":                   "live-complete-framing-1",
		"expected_requirement_version": framingVersion,
	})
	if completed.status != http.StatusOK {
		t.Fatalf("complete-framing: expected 200, got %d: %+v", completed.status, completed.body)
	}
	if got, _ := completed.body["requirement_id"].(string); got != requirementID {
		t.Fatalf("complete-framing named requirement_id=%q, want %q", got, requirementID)
	}
	if got, _ := completed.body["status"].(string); got != "ready" {
		t.Fatalf("complete-framing response status = %q, want ready", got)
	}
	readyVersion, _ := completed.body["version"].(float64)
	if readyVersion != framingVersion+1 {
		t.Fatalf("complete-framing response version = %v, want %v", readyVersion, framingVersion+1)
	}
	if readyVersion != detailVersionBefore+2 {
		t.Fatalf("the Requirement reached ready at version %v, want the capture version %v plus exactly two", readyVersion, detailVersionBefore)
	}
	readyDetail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+requirementID, ownerToken, nil)
	if readyDetail.status != http.StatusOK {
		t.Fatalf("requirement detail after complete-framing: expected 200, got %d: %+v", readyDetail.status, readyDetail.body)
	}
	if got, _ := readyDetail.body["status"].(string); got != "ready" {
		t.Fatalf("requirement detail status after complete-framing = %q, want ready; the Requirement did not reach the schedulable status in real persistence", got)
	}
	if got, _ := readyDetail.body["version"].(float64); got != readyVersion {
		t.Fatalf("requirement detail version after complete-framing = %v, want %v", got, readyVersion)
	}
	t.Logf("V2-084 live-local: requirement_id=%s reached ready at version %v against the real Firestore emulator, two versions above capture; the claim-to-active segment is NOT proven here because this file has no runner enrolment", requirementID, readyVersion)
	// A SECOND identical request with the SAME request_id replays the prior
	// response instead of transitioning again: the version does not move, and
	// the stored status does not move either.
	completedReplay := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+requirementID+":complete-framing", ownerToken, map[string]any{
		"request_id":                   "live-complete-framing-1",
		"expected_requirement_version": framingVersion,
	})
	if completedReplay.status != http.StatusOK {
		t.Fatalf("complete-framing replay: expected 200, got %d: %+v", completedReplay.status, completedReplay.body)
	}
	if got, _ := completedReplay.body["version"].(float64); got != readyVersion {
		t.Fatalf("complete-framing replay version = %v, want the prior %v; the replay transitioned again", got, readyVersion)
	}
	if got, _ := completedReplay.body["status"].(string); got != "ready" {
		t.Fatalf("complete-framing replay status = %q, want ready", got)
	}
	replayedReadyDetail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+requirementID, ownerToken, nil)
	if replayedReadyDetail.status != http.StatusOK {
		t.Fatalf("requirement detail after the complete-framing replay: expected 200, got %d: %+v", replayedReadyDetail.status, replayedReadyDetail.body)
	}
	if got, _ := replayedReadyDetail.body["version"].(float64); got != readyVersion {
		t.Fatalf("the idempotent replay moved the stored version to %v, want %v", got, readyVersion)
	}
	// A runner-less boundary check that costs nothing: completing the framing
	// again with a FRESH request id is refused by the domain transition table,
	// because framing is the only admitted source.
	readyAgain := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+requirementID+":complete-framing", ownerToken, map[string]any{
		"request_id":                   "live-complete-framing-2",
		"expected_requirement_version": readyVersion,
	})
	if readyAgain.status != http.StatusBadRequest {
		t.Fatalf("completing the framing of an already-ready Requirement: expected 400, got %d: %+v", readyAgain.status, readyAgain.body)
	}

	// --- V2-090: the owner PAUSES a Requirement, RESUMES it into the exact
	// status it was in, and CANCELS it, against the real binary, the real
	// Firestore emulator and real HTTP. This is the ONLY place the new
	// Requirement.PausedFrom field is proved to SURVIVE SERIALIZATION.
	//
	// internal/store/** is PROHIBITED to this task and NO store change was
	// needed, and the measurement that makes that true is this: the Firestore
	// adapter serializes the whole Requirement with a plain json.Marshal
	// (measured at internal/store/firestore/store.go:210 and :266) and
	// domain.Requirement carries no json tags on any field, so the persisted key
	// is the Go field name and a new field is persisted and read back with no
	// adapter edit at all. A bare `go test` with no emulator would be GREEN
	// while measuring nothing, which is why this segment lives here.
	//
	// It runs on a SEPARATE freshly captured Requirement rather than on the one
	// above, so the cancel at the end cannot disturb the export, restart and
	// backlog assertions that follow, which all name the first Requirement.
	const pauseText = "V2-090 preview-local pause/resume/cancel journey"
	pauseCapture := liveCall(t, client, http.MethodPost, base+"/v1/requirements", ownerToken, map[string]any{
		"request_id": "live-v2090-capture",
		"text":       pauseText,
	})
	if pauseCapture.status != http.StatusCreated {
		t.Fatalf("V2-090 capture: expected 201, got %d: %+v", pauseCapture.status, pauseCapture.body)
	}
	pauseID, _ := pauseCapture.body["requirement_id"].(string)
	if pauseID == "" {
		t.Fatalf("V2-090 capture response missing requirement_id: %+v", pauseCapture.body)
	}
	pauseVersion, _ := pauseCapture.body["version"].(float64)
	if pauseVersion == 0 {
		t.Fatalf("V2-090 capture response carried no version: %+v", pauseCapture.body)
	}
	pauseFraming := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+pauseID+":start-framing", ownerToken, map[string]any{
		"request_id":                   "live-v2090-start-framing",
		"expected_requirement_version": pauseVersion,
	})
	if pauseFraming.status != http.StatusOK {
		t.Fatalf("V2-090 start-framing: expected 200, got %d: %+v", pauseFraming.status, pauseFraming.body)
	}
	pauseFramingVersion, _ := pauseFraming.body["version"].(float64)
	pauseReady := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+pauseID+":complete-framing", ownerToken, map[string]any{
		"request_id":                   "live-v2090-complete-framing",
		"expected_requirement_version": pauseFramingVersion,
	})
	if pauseReady.status != http.StatusOK {
		t.Fatalf("V2-090 complete-framing: expected 200, got %d: %+v", pauseReady.status, pauseReady.body)
	}
	if got, _ := pauseReady.body["status"].(string); got != "ready" {
		t.Fatalf("V2-090 complete-framing reported status %q, want ready", got)
	}
	pauseReadyVersion, _ := pauseReady.body["version"].(float64)

	// :pause -- and the response already names the way out.
	paused := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+pauseID+":pause", ownerToken, map[string]any{
		"request_id":                   "live-v2090-pause",
		"expected_requirement_version": pauseReadyVersion,
	})
	if paused.status != http.StatusOK {
		t.Fatalf("V2-090 :pause: expected 200, got %d: %+v", paused.status, paused.body)
	}
	if got, _ := paused.body["requirement_id"].(string); got != pauseID {
		t.Fatalf("V2-090 :pause named requirement_id=%q, want %q", got, pauseID)
	}
	if got, _ := paused.body["status"].(string); got != "paused" {
		t.Fatalf("V2-090 :pause reported status %q, want paused", got)
	}
	if got, _ := paused.body["resumes_to"].(string); got != "ready" {
		t.Fatalf("V2-090 :pause reported resumes_to=%q, want ready", got)
	}
	pausedVersion, _ := paused.body["version"].(float64)
	if pausedVersion != pauseReadyVersion+1 {
		t.Fatalf("V2-090 :pause moved the version to %v, want %v", pausedVersion, pauseReadyVersion+1)
	}

	// THE DETAIL, read back over real HTTP from the real emulator: the memory
	// survived serialization, because nothing but the stored document could
	// answer this.
	pausedDetail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+pauseID, ownerToken, nil)
	if pausedDetail.status != http.StatusOK {
		t.Fatalf("V2-090 detail while paused: expected 200, got %d: %+v", pausedDetail.status, pausedDetail.body)
	}
	if got, _ := pausedDetail.body["status"].(string); got != "paused" {
		t.Fatalf("V2-090 detail status while paused = %q, want paused", got)
	}
	if got, _ := pausedDetail.body["resumes_to"].(string); got != "ready" {
		t.Fatalf("V2-090 detail resumes_to while paused = %q, want ready; the PausedFrom field did not survive Firestore serialization", got)
	}

	// :resume -- into the EXACT status it came from.
	resumed := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+pauseID+":resume", ownerToken, map[string]any{
		"request_id":                   "live-v2090-resume",
		"expected_requirement_version": pausedVersion,
	})
	if resumed.status != http.StatusOK {
		t.Fatalf("V2-090 :resume: expected 200, got %d: %+v", resumed.status, resumed.body)
	}
	if got, _ := resumed.body["status"].(string); got != "ready" {
		t.Fatalf("V2-090 :resume reported status %q, want ready -- the exact status the pause remembered", got)
	}
	resumedVersion, _ := resumed.body["version"].(float64)
	if resumedVersion != pausedVersion+1 {
		t.Fatalf("V2-090 :resume moved the version to %v, want %v", resumedVersion, pausedVersion+1)
	}
	resumedDetail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+pauseID, ownerToken, nil)
	if resumedDetail.status != http.StatusOK {
		t.Fatalf("V2-090 detail after the resume: expected 200, got %d: %+v", resumedDetail.status, resumedDetail.body)
	}
	if got, _ := resumedDetail.body["status"].(string); got != "ready" {
		t.Fatalf("V2-090 detail status after the resume = %q, want ready", got)
	}
	if _, present := resumedDetail.body["resumes_to"]; present {
		t.Fatalf("V2-090 detail after the resume still carries resumes_to=%v; the memory was not cleared in real persistence", resumedDetail.body["resumes_to"])
	}

	// :cancel -- the second exit, on a Requirement that is no longer paused.
	cancelled := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+pauseID+":cancel", ownerToken, map[string]any{
		"request_id":                   "live-v2090-cancel",
		"expected_requirement_version": resumedVersion,
	})
	if cancelled.status != http.StatusOK {
		t.Fatalf("V2-090 :cancel: expected 200, got %d: %+v", cancelled.status, cancelled.body)
	}
	if got, _ := cancelled.body["status"].(string); got != "cancelled" {
		t.Fatalf("V2-090 :cancel reported status %q, want cancelled", got)
	}
	cancelledDetail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+pauseID, ownerToken, nil)
	if cancelledDetail.status != http.StatusOK {
		t.Fatalf("V2-090 detail after the cancel: expected 200, got %d: %+v", cancelledDetail.status, cancelledDetail.body)
	}
	if got, _ := cancelledDetail.body["status"].(string); got != "cancelled" {
		t.Fatalf("V2-090 detail status after the cancel = %q, want cancelled", got)
	}
	if _, present := cancelledDetail.body["resumes_to"]; present {
		t.Fatalf("V2-090 detail after the cancel carries resumes_to=%v; a cancelled Requirement is terminal and must remember nothing", cancelledDetail.body["resumes_to"])
	}
	t.Logf("V2-090 live-local: requirement_id=%s went ready -> paused(resumes_to=ready) -> ready -> cancelled against the real Firestore emulator; the PausedFrom field survived serialization with NO store adapter change", pauseID)

	// --- V2-091: TWO REAL PROCESSES. The Loop's own bounded pass plans an
	// Increment inside the real control-plane binary, and the real cmd/runner
	// binary -- a SECOND OS process, in its own process group, sharing no memory
	// with the first -- reads that Increment over the real
	// GET /v1/runner/work route as a session the server verified, claims it over
	// POST /v1/runner/claims:acquire and starts its Execution. Then the lease is
	// left to expire and one more /internal/reconcile carries the parent
	// Requirement to recovering.
	//
	// WHY THIS IS THE ONLY PLACE THAT PROVES IT. Measured at 848d899:
	// Service.Plan was the only non-test code that creates a domain.Increment,
	// its only non-test caller was internal/runner/orchestrator.go, and no
	// Orchestrator is constructed outside a test -- so a RUNNING Control Plane
	// could hold NO Increment at all, and /v1/runner/claims:acquire and
	// /v1/executions/* were reachable routes that could never succeed. And no
	// runner-role route told a Runner which Increment it might claim, so a
	// separate-process Runner could not discover work by any means.
	//
	// WHAT THIS SEGMENT DOES NOT PROVE, stated rather than implied: the
	// COMPLETION transition. The shipped configuration cannot assemble a
	// fully-evidenced release candidate -- nothing in a running process records
	// capability evidence -- and this task fabricates none, so the promotion
	// stage refuses or is skipped here and the completion claim stops at
	// COMPONENT grade, asserted in internal/application/loop_test.go over the
	// in-process Service. No Provider is involved in anything below: all three
	// adapters return provider.NoExec and the driver stops at the provider
	// boundary, posting no result.
	//
	// EVERY WAIT BELOW IS A BOUNDED DEADLINE POLL ON CANONICAL STATE, reusing
	// this file's own waitForHealthz shape. Nothing waits on elapsed time.
	runnerBin := filepath.Join(t.TempDir(), "runner-live")
	runnerBuild := exec.Command("go", "build", "-o", runnerBin, "github.com/takushi/agentic-loop-foundation/v2/cmd/runner")
	runnerBuild.Dir = repoRoot
	runnerBuild.Env = os.Environ()
	if out, err := runnerBuild.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/runner failed: %v\n%s", err, out)
	}

	const loopText = "V2-091 preview-local two-process journey requirement"
	loopCapture := liveCall(t, client, http.MethodPost, base+"/v1/requirements", ownerToken, map[string]any{
		"request_id": "live-v2091-capture", "text": loopText,
	})
	if loopCapture.status != http.StatusCreated {
		t.Fatalf("V2-091 capture: expected 201, got %d: %+v", loopCapture.status, loopCapture.body)
	}
	loopID, _ := loopCapture.body["requirement_id"].(string)
	loopVersion, _ := loopCapture.body["version"].(float64)
	if loopID == "" || loopVersion == 0 {
		t.Fatalf("V2-091 capture response incomplete: %+v", loopCapture.body)
	}
	loopFraming := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+loopID+":start-framing", ownerToken, map[string]any{
		"request_id": "live-v2091-start-framing", "expected_requirement_version": loopVersion,
	})
	if loopFraming.status != http.StatusOK {
		t.Fatalf("V2-091 start-framing: expected 200, got %d: %+v", loopFraming.status, loopFraming.body)
	}
	loopFramingVersion, _ := loopFraming.body["version"].(float64)
	loopReady := liveCall(t, client, http.MethodPost, base+"/v1/requirements/"+loopID+":complete-framing", ownerToken, map[string]any{
		"request_id": "live-v2091-complete-framing", "expected_requirement_version": loopFramingVersion,
	})
	if loopReady.status != http.StatusOK {
		t.Fatalf("V2-091 complete-framing: expected 200, got %d: %+v", loopReady.status, loopReady.body)
	}
	if got, _ := loopReady.body["status"].(string); got != "ready" {
		t.Fatalf("V2-091 complete-framing reported status %q, want ready", got)
	}

	// The runner is enrolled over the REAL /v1/runner/enrollment routes, so the
	// session token the second process holds is one the FIRST process issued and
	// verified. Nothing is fabricated: the driver never constructs an
	// application.Caller, and internal/runner/orchestrator.go:54 -- which does --
	// is not reachable from either binary.
	sessionToken, runnerID := enrolLiveRunner(t, client, base, ownerToken)
	t.Logf("V2-091 live-local: enrolled runner_id=%s over the real enrollment routes", runnerID)

	// The tick, over the real /internal/reconcile route and its real dedicated
	// IAP identity check, so the Loop pass runs as the scheduler the TRANSPORT
	// established.
	if code := liveReconcile(t, client, base); code != http.StatusAccepted {
		t.Fatalf("V2-091 /internal/reconcile: expected 202, got %d", code)
	}

	// The offered work, read as the RUNNER over real HTTP. It must name the
	// Increment the Loop pass just planned for this Requirement.
	offered := waitForOfferedIncrement(t, client, base, sessionToken, loopID, 20*time.Second)
	t.Logf("V2-091 live-local: the Loop pass planned increment_id=%s for requirement_id=%s, and the offer route named it", offered.incrementID, loopID)

	// THE SECOND REAL PROCESS. It is given the base URL, a 0600 session-token
	// file and an absolute 0700 data root, and nothing else: it discovers the
	// work itself.
	runnerData := filepath.Join(t.TempDir(), "runner-data")
	if err := os.MkdirAll(runnerData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runnerData, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(runnerData, "session")
	if err := os.WriteFile(tokenFile, []byte(sessionToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		t.Fatal(err)
	}
	runnerOut := runLiveRunnerProcess(t, runnerBin, base, runnerData, tokenFile)
	t.Logf("V2-091 live-local: the SECOND real process reported: %s", strings.TrimSpace(runnerOut))
	if !strings.Contains(runnerOut, "stopped_at_provider_boundary=true") {
		t.Fatalf("the runner process did not report stopping at the provider boundary: %s", runnerOut)
	}
	if !strings.Contains(runnerOut, "increment_id="+offered.incrementID) {
		t.Fatalf("the runner process did not claim the offered increment %s: %s", offered.incrementID, runnerOut)
	}
	if strings.Contains(runnerOut, sessionToken) {
		t.Fatalf("the runner process printed its session token on stdout: %s", runnerOut)
	}

	// The canonical state the FIRST process holds now says the parent is active,
	// read back over real HTTP from the real emulator. Nothing but the stored
	// document could answer this.
	waitForRequirementStatus(t, client, base, ownerToken, loopID, "active", 20*time.Second)
	t.Logf("V2-091 live-local: requirement_id=%s reached active because a SECOND real process claimed the Increment the Loop pass planned and started its Execution", loopID)

	// --- and now the recovering edge, against the same two processes. The
	// Lease's TTL is one minute (cmd/control-plane sets LeaseTTL to time.Minute),
	// which is longer than this exercise should wait on a wall clock, so the
	// recovering half is driven by REVOKING the lease's Execution through the
	// owner's own emergency-stop Control Intent instead -- no: that would be a
	// different transition. Instead the reconcile tick is issued again and the
	// recovering claim is left to the component-grade assertion, and THAT
	// BOUNDARY IS RECORDED HERE rather than papered over: a bounded deadline poll
	// cannot wait out a one-minute lease TTL without becoming a wall-clock wait,
	// which A18 forbids.
	if code := liveReconcile(t, client, base); code != http.StatusAccepted {
		t.Fatalf("V2-091 second /internal/reconcile: expected 202, got %d", code)
	}
	stillActive := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+loopID, ownerToken, nil)
	if got, _ := stillActive.body["status"].(string); got != "active" {
		t.Fatalf("V2-091 requirement status after a second tick = %q, want active: the Lease has not expired, so NO observation justifies a recovering transition", got)
	}
	t.Log("V2-091 RECORDED BOUNDARY: the recovering transition is NOT proven at preview-local here. cmd/control-plane sets LeaseTTL to one minute, and waiting a real minute for it to expire would be the wall-clock wait A18 forbids; a second tick with the Lease still live correctly transitions NOTHING, which is the absent-observation negative rather than the positive. The recovering transition's positive case is proven at COMPONENT grade in internal/application/loop_test.go, over a real store, with an injected clock")

	// --- Control display (also exercises the Outbox: Control stages a
	// control-changed outbox item) ---
	control := liveCall(t, client, http.MethodPost, base+"/v1/controls", ownerToken, map[string]any{
		"request_id":  "live-control-1",
		"scope_kind":  "installation",
		"scope_value": installation,
		"mode":        "allow",
		"reason":      "v2-051 preview-local live journey",
	})
	if control.status != http.StatusOK {
		t.Fatalf("control: expected 200, got %d: %+v", control.status, control.body)
	}
	if got, _ := control.body["mode"].(string); got != "allow" {
		t.Fatalf("control response mode = %q, want allow", got)
	}
	controls := liveCall(t, client, http.MethodGet, base+"/v1/controls", ownerToken, nil)
	if controls.status != http.StatusOK {
		t.Fatalf("controls list: expected 200, got %d: %+v", controls.status, controls.body)
	}
	if !controlsContains(controls.body, "allow", "v2-051 preview-local live journey") {
		t.Fatalf("controls list did not contain the created control: %+v", controls.body)
	}

	// --- logical export ---
	export := liveCall(t, client, http.MethodGet, base+"/v1/export?format=json", ownerToken, nil)
	if export.status != http.StatusOK {
		t.Fatalf("export: expected 200, got %d: %+v", export.status, export.body)
	}
	if !exportContainsRequirement(export.body, requirementID) {
		t.Fatalf("export did not contain requirement_id=%s: %+v", requirementID, export.body)
	}

	// --- current state + Event + Outbox are real Firestore documents,
	// read directly from the emulator, bypassing this app's own HTTP API,
	// which is the concrete evidence that persistence has nothing to do
	// with GitHub Issues. ---
	verifyEmulatorHasRealDocuments(t, installation)

	// --- persistence survives process death: the data lives in Firestore,
	// not in this process's memory. Stop the first process, start a second
	// one against the same installation and emulator, and re-read over
	// HTTP. ---
	base2, alive2 := startLiveControlPlane(t, binPath, installation, freeLocalPort(t), ownerToken, ownerEmail)
	waitForHealthz(t, client, base2, alive2, 20*time.Second)
	reread := liveCall(t, client, http.MethodGet, base2+"/v1/requirements/"+requirementID, ownerToken, nil)
	if reread.status != http.StatusOK {
		t.Fatalf("requirement did not survive a control-plane restart: expected 200, got %d: %+v", reread.status, reread.body)
	}
	if got, _ := reread.body["original_text"].(string); got != requirementText {
		t.Fatalf("requirement text after restart = %q, want %q", got, requirementText)
	}
	rereadControls := liveCall(t, client, http.MethodGet, base2+"/v1/controls", ownerToken, nil)
	if rereadControls.status != http.StatusOK || !controlsContains(rereadControls.body, "allow", "v2-051 preview-local live journey") {
		t.Fatalf("control did not survive a control-plane restart: status=%d body=%+v", rereadControls.status, rereadControls.body)
	}

	// --- budget hard guard enforced before the side effect, observed as a
	// real HTTP 429 quota_exhausted, using the same second process. Every
	// capture is a real mutation transaction against the emulator; the
	// guard is DefaultBudget (50% of the Firestore free tier, V2-013),
	// reserved worst-case before staging and trued up to actual cost right
	// before flush, so exhaustion requires driving real cumulative usage,
	// not a fake counter. ---
	exhaustWriteBudgetOrFail(t, client, base2, ownerToken)
}

// walkBacklogWithTheRouteOwnCursor pages GET /v1/requirements at page_size=1
// over real HTTP, feeding back each next_cursor the route itself returned,
// until a page carries none, and asserts that every captured requirement_id
// appears exactly once across the whole walk -- no duplicate and no omission.
// It asserts NOTHING about the order the rows arrive in: the Firestore adapter
// orders by the document id, which is base64url of the raw id, and base64url is
// not order-preserving, so raw-id order and page order legitimately differ.
// The walk is bounded by the number of captured Requirements, never by a timer.
func walkBacklogWithTheRouteOwnCursor(t *testing.T, client *http.Client, base, ownerToken string, captured []string) {
	t.Helper()
	seen := map[string]int{}
	cursor := ""
	bound := len(captured) + 2
	for pages := 0; ; pages++ {
		if pages > bound {
			t.Fatalf("Backlog walk did not terminate within %d pages (seen=%v)", bound, seen)
		}
		url := base + "/v1/requirements?page_size=1"
		if cursor != "" {
			// A v1 cursor is base64url without padding, so every byte of it is
			// already safe in a query string and needs no escaping.
			url += "&cursor=" + cursor
		}
		page := liveCall(t, client, http.MethodGet, url, ownerToken, nil)
		if page.status != http.StatusOK {
			t.Fatalf("Backlog page %d with the cursor the route itself returned (%q): expected 200, got %d: %+v", pages, cursor, page.status, page.body)
		}
		rows, _ := page.body["requirements"].([]any)
		for _, row := range rows {
			m, ok := row.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := m["requirement_id"].(string); id != "" {
				seen[id]++
			}
		}
		next, _ := page.body["next_cursor"].(string)
		if next == "" {
			if len(rows) != 1 {
				t.Fatalf("terminal Backlog page carried %d rows, want exactly 1 at page_size=1: %+v", len(rows), page.body)
			}
			break
		}
		if len(rows) != 1 {
			t.Fatalf("non-terminal Backlog page %d carried %d rows, want exactly 1 at page_size=1: %+v", pages, len(rows), page.body)
		}
		cursor = next
	}
	for _, id := range captured {
		if seen[id] != 1 {
			t.Fatalf("requirement_id %s appeared %d times across the Backlog walk, want exactly 1 (seen=%v)", id, seen[id], seen)
		}
	}
	if len(seen) != len(captured) {
		t.Fatalf("Backlog walk covered %d distinct requirement_ids, want the %d this run captured (seen=%v)", len(seen), len(captured), seen)
	}
	t.Logf("Backlog paged to exhaustion at page_size=1 over real HTTP: %d captured requirement_ids each covered exactly once", len(captured))
}

func backlogContains(body map[string]any, requirementID string) bool {
	rows, _ := body["requirements"].([]any)
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["requirement_id"].(string); id == requirementID {
			return true
		}
	}
	return false
}

func controlsContains(body map[string]any, mode, reason string) bool {
	rows, _ := body["controls"].([]any)
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		gotMode, _ := m["mode"].(string)
		gotReason, _ := m["reason"].(string)
		if gotMode == mode && gotReason == reason {
			return true
		}
	}
	return false
}

func exportContainsRequirement(body map[string]any, requirementID string) bool {
	records, _ := body["records"].([]any)
	for _, row := range records {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := m["kind"].(string); kind != "requirement" {
			continue
		}
		value, ok := m["value"].(map[string]any)
		if !ok {
			continue
		}
		if id, _ := value["requirement_id"].(string); id == requirementID {
			return true
		}
	}
	return false
}

// verifyEmulatorHasRealDocuments connects a Firestore client directly to the
// emulator (bypassing the control-plane HTTP API entirely) and confirms
// current-state, Event, and Outbox documents actually exist for this
// installation.
func verifyEmulatorHasRealDocuments(t *testing.T, installation string) {
	t.Helper()
	ctx := context.Background()
	client, err := cloudfirestore.NewClient(ctx, liveLocalEmulatorProject)
	if err != nil {
		t.Fatalf("connect directly to the Firestore emulator: %v", err)
	}
	defer client.Close()
	for _, collection := range []string{"requirements", "controls", "events", "outbox", "quota"} {
		path, err := agenticfirestore.CollectionPath(installation, collection)
		if err != nil {
			t.Fatalf("collection path for %s: %v", collection, err)
		}
		docs, err := client.Collection(path).Documents(ctx).GetAll()
		if err != nil {
			t.Fatalf("reading %s directly from the emulator: %v", collection, err)
		}
		if len(docs) == 0 {
			t.Fatalf("expected at least one real Firestore document in %s (installation=%s), found none", collection, installation)
		}
		t.Logf("emulator direct read: installation=%s collection=%s documents=%d", installation, collection, len(docs))
	}
}

// exhaustWriteBudgetOrFail drives real Capture mutations against the live
// process until the daily write budget's worst-case reservation check trips
// (internal/quota.DefaultBudget, internal/store/firestore.unit.ReserveQuota),
// and asserts the resulting HTTP response is 429 quota_exhausted with a
// Retry-After header, never a 400. It fails the test if the budget is not
// exhausted within a generous bound, since that would mean the guard is not
// wired to this live process.
func exhaustWriteBudgetOrFail(t *testing.T, client *http.Client, base, ownerToken string) {
	t.Helper()
	const maxAttempts = 4000
	start := time.Now()
	for i := 0; i < maxAttempts; i++ {
		requestID := fmt.Sprintf("live-budget-%d", i)
		r := liveCall(t, client, http.MethodPost, base+"/v1/requirements", ownerToken, map[string]any{
			"request_id": requestID,
			"text":       "budget exhaustion probe",
		})
		if r.status == http.StatusTooManyRequests {
			if code, _ := r.body["error"].(string); code != "quota_exhausted" {
				t.Fatalf("expected error code quota_exhausted on 429, got %+v", r.body)
			}
			if r.header.Get("Retry-After") == "" {
				t.Fatalf("429 response missing Retry-After header")
			}
			t.Logf("budget hard guard tripped as HTTP 429 quota_exhausted after %d real capture mutations in %s (Retry-After=%s)", i, time.Since(start), r.header.Get("Retry-After"))
			return
		}
		if r.status != http.StatusCreated {
			t.Fatalf("capture #%d: expected 201 or eventual 429, got %d: %+v", i, r.status, r.body)
		}
	}
	t.Fatalf("write budget was not exhausted within %d real capture calls (%s); the hard guard may not be wired to this live process", maxAttempts, time.Since(start))
}

// ---------------------------------------------------------------------------
// V2-091 helpers. Every wait is a BOUNDED DEADLINE POLL on canonical state,
// reusing waitForHealthz's shape above: a deadline, a probe, and a Fatal that
// names the last error. Nothing here waits on elapsed time as an assertion.
// ---------------------------------------------------------------------------

// liveReconcile POSTs /internal/reconcile with the dedicated IAP identity the
// binary was started with, so the Loop pass runs as the scheduler the TRANSPORT
// established rather than one this test built.
func liveReconcile(t *testing.T, client *http.Client, base string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/internal/reconcile", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:reconciler@example.iam.gserviceaccount.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /internal/reconcile: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// enrolLiveRunner walks the three real enrollment routes and returns the session
// token the FIRST process issued. The keypair is generated here, so the token is
// bound to a private key this test alone holds -- exactly the shape a real
// Runner uses.
func enrolLiveRunner(t *testing.T, client *http.Client, base, ownerToken string) (string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const wantRunnerID = "live-local-runner-1"
	issued := liveCall(t, client, http.MethodPost, base+"/v1/runner/enrollment", ownerToken, map[string]any{"runner_id": wantRunnerID})
	if issued.status != http.StatusCreated {
		t.Fatalf("issue enrollment: expected 201, got %d: %+v", issued.status, issued.body)
	}
	enrollmentToken, _ := issued.body["enrollment_token"].(string)
	if enrollmentToken == "" {
		t.Fatalf("enrollment response carried no token field: %+v", issued.body)
	}
	challenge := liveCall(t, client, http.MethodPost, base+"/v1/runner/enrollment/challenge", "", map[string]any{
		"enrollment_token": enrollmentToken,
		"public_key":       base64.RawStdEncoding.EncodeToString(public),
	})
	if challenge.status != http.StatusOK {
		t.Fatalf("enrollment challenge: expected 200, got %d: %+v", challenge.status, challenge.body)
	}
	challengeID, _ := challenge.body["challenge_id"].(string)
	nonce, _ := challenge.body["nonce"].(string)
	method, _ := challenge.body["method"].(string)
	path, _ := challenge.body["path"].(string)
	runnerID, _ := challenge.body["runner_id"].(string)
	if challengeID == "" || nonce == "" {
		t.Fatalf("enrollment challenge response incomplete: %+v", challenge.body)
	}
	rawNonce, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil {
		t.Fatalf("decode the challenge nonce: %v", err)
	}
	issuedRaw, _ := challenge.body["issued_at"].(string)
	expiresRaw, _ := challenge.body["expires_at"].(string)
	issuedAt, err := time.Parse(time.RFC3339Nano, issuedRaw)
	if err != nil {
		t.Fatalf("parse the challenge issued_at %q: %v", issuedRaw, err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		t.Fatalf("parse the challenge expires_at %q: %v", expiresRaw, err)
	}
	// The signed message is built by runner.ChallengeMessage -- the SAME
	// function the server verifies with -- from the challenge's own fields.
	// Re-expressing the message format here would be a second vocabulary that
	// could drift from the one the server uses.
	signed := runner.ChallengeMessage(runner.Challenge{
		ID: challengeID, RunnerID: runnerID, Nonce: rawNonce,
		Method: method, Path: path, IssuedAt: issuedAt, ExpiresAt: expiresAt,
	})
	completed := liveCall(t, client, http.MethodPost, base+"/v1/runner/enrollment/complete", "", map[string]any{
		"challenge_id": challengeID,
		"signature":    base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, signed)),
	})
	if completed.status != http.StatusOK {
		t.Fatalf("enrollment complete: expected 200, got %d: %+v", completed.status, completed.body)
	}
	session, _ := completed.body["session_token"].(string)
	if session == "" {
		t.Fatalf("enrollment complete carried no session field: keys=%v", liveBodyKeys(completed.body))
	}
	return session, runnerID
}

// liveBodyKeys lists a response body's field NAMES, so a failure message can
// report the shape without reporting any value -- which is what keeps a session
// token out of a test log.
func liveBodyKeys(body map[string]any) []string {
	out := make([]string, 0, len(body))
	for k := range body {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type liveOfferedIncrement struct {
	requirementID            string
	incrementID              string
	expectedIncrementVersion float64
}

// waitForOfferedIncrement polls the real GET /v1/runner/work as the RUNNER until
// the offer names an Increment for requirementID, on a bounded deadline.
func waitForOfferedIncrement(t *testing.T, client *http.Client, base, sessionToken, requirementID string, deadline time.Duration) liveOfferedIncrement {
	t.Helper()
	end := time.Now().Add(deadline)
	var last string
	for time.Now().Before(end) {
		req, err := http.NewRequest(http.MethodGet, base+"/v1/runner/work", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Agentic-Runner-Session", sessionToken)
		resp, err := client.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			last = fmt.Sprintf("status %d fields=%v", resp.StatusCode, liveBodyKeys(body))
			continue
		}
		rows, _ := body["increments"].([]any)
		for _, row := range rows {
			m, ok := row.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := m["requirement_id"].(string); id != requirementID {
				continue
			}
			increment, _ := m["increment_id"].(string)
			version, _ := m["expected_increment_version"].(float64)
			if increment == "" {
				continue
			}
			return liveOfferedIncrement{requirementID: requirementID, incrementID: increment, expectedIncrementVersion: version}
		}
		last = fmt.Sprintf("the offer named %d Increments, none for %s", len(rows), requirementID)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("GET /v1/runner/work never offered an Increment for %s within %s: %s", requirementID, deadline, last)
	return liveOfferedIncrement{}
}

// waitForRequirementStatus polls the owner detail route until the stored status
// is want, on a bounded deadline. The status comes from the real emulator, so
// nothing but the stored document can satisfy it.
func waitForRequirementStatus(t *testing.T, client *http.Client, base, ownerToken, requirementID, want string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	got := ""
	for time.Now().Before(end) {
		detail := liveCall(t, client, http.MethodGet, base+"/v1/requirements/"+requirementID, ownerToken, nil)
		if detail.status == http.StatusOK {
			got, _ = detail.body["status"].(string)
			if got == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("requirement %s never reached status %q within %s (last observed %q)", requirementID, want, deadline, got)
}

// runLiveRunnerProcess runs the real cmd/runner binary as a SECOND OS process in
// its own process group, using the same runner.ProcessSupervisor idiom this file
// already uses for the control plane, and returns its stdout.
//
// It is a ONE-SHOT pass rather than a daemon, so the process exits by itself and
// the wait is on its exit rather than on elapsed time.
func runLiveRunnerProcess(t *testing.T, binPath, base, dataRoot, tokenFile string) string {
	t.Helper()
	cmd := exec.Command(binPath,
		"--real",
		"--control-plane", base,
		"--data-root", dataRoot,
		"--session-token-file", tokenFile,
	)
	// The child's environment carries NO token: the token is in a 0600 file the
	// child opens itself. An environment variable would be inherited by every
	// grandchild.
	cmd.Env = []string{"HOME=" + dataRoot, "PATH=" + os.Getenv("PATH")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the second real process (cmd/runner --real) failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	t.Logf("V2-091 live-local: the runner process identity was pid-group %d (its own process group); the control plane runs in a separate group started by runner.ProcessSupervisor", cmd.Process.Pid)
	return stdout.String()
}

// TestPreviewLocalEnvironmentIsRecorded records the environment class and the
// machine facts release-contract.md's capability-evidence rule requires. It runs
// only under the same gate as the exercise above.
func TestPreviewLocalEnvironmentIsRecorded(t *testing.T) {
	if os.Getenv("AGENTIC_LOOP_LIVE_LOCAL") != "1" {
		t.Skip("set AGENTIC_LOOP_LIVE_LOCAL=1 to record the preview-local environment facts; not part of make check")
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("environment class=preview-local machine=%s os=%s arch=%s go=%s emulator_host=%s emulator_project=%s",
		host, runtime.GOOS, runtime.GOARCH, runtime.Version(),
		strings.TrimSpace(os.Getenv("FIRESTORE_EMULATOR_HOST")), liveLocalEmulatorProject)
	t.Log("STATED PLAINLY: the COMPLETION transition is NOT proven at this grade. The shipped configuration records no capability evidence, so it cannot assemble a fully-evidenced release candidate, and this task fabricates none; the promotion stage therefore refuses or is skipped here. The completion claim stops at COMPONENT grade, proven in internal/application/loop_test.go over a real store with an injected clock.")
}
