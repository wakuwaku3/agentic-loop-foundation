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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
