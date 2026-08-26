package api_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/api"
	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

type ids struct{ n int }

func (i *ids) Next(kind string) (string, error) {
	i.n++
	return kind + "-" + string(rune('a'+i.n)), nil
}

type clock struct{}

func (clock) Now() time.Time { return time.Unix(1700000000, 0).UTC() }

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{"owner": {Role: application.RoleOwner, Subject: "owner"}, "runner": {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"}}
	enrollment, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return api.New(api.Config{Authenticator: auth, Service: svc, RunnerEnrollment: enrollment, AllowedOrigins: []string{"https://console.example"}})
}
func call(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
func TestHealthAndFailClosed(t *testing.T) {
	h := api.New(api.Config{})
	w := call(h, http.MethodGet, "/healthz", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatal(w.Body.String())
	}
	w = call(h, http.MethodPost, "/v1/requirements", `{}`, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
}

func TestInternalReconcileRequiresDedicatedIAPIdentity(t *testing.T) {
	called := false
	h := api.New(api.Config{ReconcileIdentity: "reconciler@example.iam.gserviceaccount.com", InternalReconcile: func(context.Context) error { called = true; return nil }})
	req := httptest.NewRequest(http.MethodPost, "/internal/reconcile", nil)
	req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:reconciler@example.iam.gserviceaccount.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted || !called {
		t.Fatalf("status=%d called=%v body=%s", w.Code, called, w.Body.String())
	}
	called = false
	req = httptest.NewRequest(http.MethodPost, "/internal/reconcile", nil)
	req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:reconciler@example.iam.gserviceaccount.com")
	req.Header.Set("X-Agentic-Runner-Session", "spoof")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || called {
		t.Fatalf("runner credential reached internal reconcile: status=%d called=%v", w.Code, called)
	}
}
func TestStrictJSONAuthRoleAndSpoof(t *testing.T) {
	h := testHandler(t)
	w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"r","text":"x","actor_id":"spoof"}`, "owner")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("spoof status=%d body=%s", w.Code, w.Body.String())
	}
	w = call(h, http.MethodPost, "/v1/runner/claims:acquire", `{"request_id":"r","increment_id":"i","expected_increment_version":1,"control_revision":0}`, "owner")
	if w.Code != http.StatusForbidden {
		t.Fatalf("role status=%d body=%s", w.Code, w.Body.String())
	}
	w = call(h, http.MethodPost, "/v1/requirements", `{"request_id":"r","text":"x"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("capture status=%d body=%s", w.Code, w.Body.String())
	}
	var v map[string]any
	if json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatal("not json")
	}
	if v["requirement_id"] == nil {
		t.Fatal(v)
	}
	for key := range v {
		if key != "requirement_id" && key != "version" && key != "requested_by" {
			t.Fatalf("unexpected capture response field %q", key)
		}
	}
}
func TestGoldenCaptureIdempotencyAndHeaders(t *testing.T) {
	h := testHandler(t)
	body := `{"request_id":"same","text":"hello"}`
	a := call(h, http.MethodPost, "/v1/requirements", body, "owner")
	b := call(h, http.MethodPost, "/v1/requirements", body, "owner")
	if a.Code != 201 || b.Code != 201 || a.Body.String() != b.Body.String() {
		t.Fatalf("retry mismatch: %d %d %s %s", a.Code, b.Code, a.Body.String(), b.Body.String())
	}
	if a.Header().Get("Cache-Control") != "no-store" || a.Header().Get("X-Correlation-ID") == "" {
		t.Fatal(a.Header())
	}
	w := call(h, http.MethodPost, "/v1/requirements", body, "runner")
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}

// TestCaptureAndControlRecordOwnerRequestedBy proves the authenticated
// caller's identity (the same Bearer subject used for authentication, and
// the production analogue of an IAP subject) flows through to the
// requested_by owners see on both a Requirement intake and a Control Intent,
// end to end through the transport boundary.
func TestCaptureAndControlRecordOwnerRequestedBy(t *testing.T) {
	h := testHandler(t)
	w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"cap-rb","text":"hello"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("capture status=%d body=%s", w.Code, w.Body.String())
	}
	var capOut map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &capOut); err != nil {
		t.Fatal(err)
	}
	rb, ok := capOut["requested_by"].(map[string]any)
	if !ok || rb["actor_type"] != "owner" || rb["subject"] != "owner" {
		t.Fatalf("capture requested_by = %#v", capOut["requested_by"])
	}

	w = call(h, http.MethodPost, "/v1/controls", `{"request_id":"ctl-rb","scope_kind":"installation","scope_value":"install","mode":"allow"}`, "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("control status=%d body=%s", w.Code, w.Body.String())
	}
	var ctlOut map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &ctlOut); err != nil {
		t.Fatal(err)
	}
	rb, ok = ctlOut["requested_by"].(map[string]any)
	if !ok || rb["actor_type"] != "owner" || rb["subject"] != "owner" {
		t.Fatalf("control requested_by = %#v", ctlOut["requested_by"])
	}

	w = call(h, http.MethodGet, "/v1/controls", "", "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("list controls status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"requested_by"`) {
		t.Fatalf("listed controls should carry requested_by: %s", w.Body.String())
	}
}
func TestOwnerRequirementReads(t *testing.T) {
	h := testHandler(t)
	body := `{"request_id":"read-cap","text":"hello"}`
	if w := call(h, http.MethodPost, "/v1/requirements", body, "owner"); w.Code != 201 {
		t.Fatal(w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)
	req.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "requirements") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/requirements", nil)
	req.Header.Set("Authorization", "Bearer runner")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestContentTypeAndOrigin(t *testing.T) {
	h := testHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/requirements", strings.NewReader(`{"request_id":"r","text":"x"}`))
	req.Header.Set("Authorization", "Bearer owner")
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatal(w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/requirements", strings.NewReader(`{"request_id":"r2","text":"x"}`))
	req.Header.Set("Authorization", "Bearer owner")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}
func TestRouteContractErrorsAndRunnerPermitRole(t *testing.T) {
	h := testHandler(t)
	if w := call(h, http.MethodPost, "/v1/nope", "{}", "owner"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := call(h, http.MethodGet, "/v1/controls", "", "owner"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call(h, http.MethodPost, "/v1/runner/permits:check", `{"request_id":"p","kind":"claim","control_revision":0,"fencing_token":0,"resource":"x"}`, "owner"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	large := strings.Repeat("x", 1<<20)
	if w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"large","text":"`+large+`"}`, "owner"); w.Code != 413 {
		t.Fatal(w.Code)
	}
}

func TestRunnerEnrollmentHTTPChallengeFlow(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := testHandler(t)
	issue := call(h, http.MethodPost, "/v1/runner/enrollment", `{"runner_id":"runner-http"}`, "owner")
	if issue.Code != http.StatusCreated {
		t.Fatalf("issue=%d %s", issue.Code, issue.Body.String())
	}
	var issued struct {
		Token string `json:"enrollment_token"`
	}
	if err := json.Unmarshal(issue.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	challenge := call(h, http.MethodPost, "/v1/runner/enrollment/challenge", `{"enrollment_token":"`+issued.Token+`","public_key":"`+base64.RawStdEncoding.EncodeToString(pub)+`"}`, "")
	if challenge.Code != http.StatusOK {
		t.Fatalf("challenge=%d %s", challenge.Code, challenge.Body.String())
	}
	var c struct {
		ID        string    `json:"challenge_id"`
		RunnerID  string    `json:"runner_id"`
		Nonce     string    `json:"nonce"`
		Method    string    `json:"method"`
		Path      string    `json:"path"`
		IssuedAt  time.Time `json:"issued_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(challenge.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(c.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(private, runner.ChallengeMessage(runner.Challenge{ID: c.ID, RunnerID: c.RunnerID, Nonce: nonce, Method: c.Method, Path: c.Path, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt}))
	complete := call(h, http.MethodPost, "/v1/runner/enrollment/complete", `{"challenge_id":"`+c.ID+`","signature":"`+base64.RawURLEncoding.EncodeToString(sig)+`"}`, "")
	if complete.Code != http.StatusOK || !strings.Contains(complete.Body.String(), "session_token") {
		t.Fatalf("complete=%d %s", complete.Code, complete.Body.String())
	}
}

// A3: budget exhaustion is an explicit 429 quota_exhausted with a
// Retry-After naming the next UTC midnight, never the default 400
// invalid_request that a generic domain error maps to.
func TestQuotaExhaustionMapsTo429NotBadRequest(t *testing.T) {
	st := memory.New()
	c := clock{}
	svc, err := application.NewServiceWithConfig(st, c, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// Fill the day's write budget directly to exactly the ceiling, so the
	// next mutation's worst-case reservation is refused by one write before
	// any document is staged.
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		return u.ReserveQuota(context.Background(), "fill", c.Now(), quota.Usage{Writes: quota.DefaultBudget.Writes})
	}); err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{"owner": {Role: application.RoleOwner, Subject: "owner"}}
	enrollment, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(api.Config{Authenticator: auth, Service: svc, RunnerEnrollment: enrollment, AllowedOrigins: []string{"https://console.example"}})

	w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"quota-exhausted","text":"x"}`, "owner")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "quota_exhausted" {
		t.Fatalf("expected error code quota_exhausted, got %v", body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header naming the next UTC midnight")
	}
	for key := range body {
		if key != "error" && key != "message" && key != "schema_version" && key != "correlation_id" {
			t.Fatalf("unexpected quota_exhausted response field %q leaks beyond the documented envelope: %v", key, body)
		}
	}
}

// ===========================================================================
// V2-066: the read-only release surface and the api import boundary
// ===========================================================================

// --- the import boundary ----------------------------------------------------

// internalPackagePrefix is assembled from two fragments on purpose. A single
// string literal holding the full module path of internal/release would be
// read by ci/components.json's dependency derivation as a literal api ->
// release edge, which would invert the very guard the literal is assertion
// data for; internal/ci's LiteralFalsePositives table is the only other way
// to record that, and internal/ci is not this task's to edit. Neither
// fragment is a module-relative path on its own, so the derivation sees no
// edge, while the concatenated value is exactly the import path the guard
// must detect.
var internalPackagePrefix = "github.com/takushi/agentic-loop-foundation" + "/v2/internal/"

var (
	releaseImportPath = internalPackagePrefix + "release"
	updateImportPath  = internalPackagePrefix + "update"
)

// isForbiddenAPIImport reports whether path names internal/release or
// internal/update. The api package must reach the release machinery only
// through internal/application, and must not couple to the update channel at
// all: either import would create a component edge ci/components.json does
// not record.
func isForbiddenAPIImport(path string) bool {
	return path == releaseImportPath || strings.HasPrefix(path, releaseImportPath+"/") ||
		path == updateImportPath || strings.HasPrefix(path, updateImportPath+"/")
}

// apiPackageImports parses the non-test .go files in the api package
// directory with go/ast and returns file name -> import paths. It fails the
// test outright on a zero-file scan so the assertion cannot pass vacuously.
func apiPackageImports(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		out[e.Name()] = importPathsOf(file)
	}
	if len(out) == 0 {
		t.Fatal("scanned zero non-test .go files; the working directory is not internal/api or the scan is broken")
	}
	return out
}

func importPathsOf(file *ast.File) []string {
	paths := make([]string, 0, len(file.Imports))
	for _, imp := range file.Imports {
		value, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		paths = append(paths, value)
	}
	return paths
}

// TestAPIImportGuardMatcherIsVerified checks the classifier against a
// known-positive and a known-negative before the scan below trusts it.
func TestAPIImportGuardMatcherIsVerified(t *testing.T) {
	for _, positive := range []string{releaseImportPath, releaseImportPath + "/sub", updateImportPath, updateImportPath + "/sub"} {
		if !isForbiddenAPIImport(positive) {
			t.Fatalf("known-positive %q was not detected as forbidden", positive)
		}
	}
	for _, negative := range []string{
		"net/http",
		internalPackagePrefix + "application",
		internalPackagePrefix + "domain",
		// A path that merely starts with the same letters as a forbidden one
		// must not be flagged: the matcher compares whole path segments.
		internalPackagePrefix + "releasenotes",
		internalPackagePrefix + "updater",
	} {
		if isForbiddenAPIImport(negative) {
			t.Fatalf("known-negative %q was flagged as forbidden", negative)
		}
	}
	// Positive control on the scan itself, not just the matcher: a synthetic
	// file that does import internal/release must be reported.
	src := "package api\n\nimport (\n\t\"net/http\"\n\t_ \"" + releaseImportPath + "\"\n)\n\nvar _ = http.StatusOK\n"
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	flagged := false
	for _, path := range importPathsOf(file) {
		if isForbiddenAPIImport(path) {
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("positive control: a synthetic file importing internal/release was not flagged by the scan")
	}
}

// TestAPIImportsNeitherReleaseNorUpdate is the boundary assertion: the
// release machinery is reached through internal/application only.
func TestAPIImportsNeitherReleaseNorUpdate(t *testing.T) {
	files := apiPackageImports(t)
	total := 0
	for name, paths := range files {
		if len(paths) == 0 {
			continue
		}
		total += len(paths)
		for _, path := range paths {
			if isForbiddenAPIImport(path) {
				t.Errorf("%s imports %q; internal/api must reach the release machinery through internal/application only", name, path)
			}
		}
	}
	if total == 0 {
		t.Fatal("scanned zero import paths across the package's non-test files")
	}
	t.Logf("api import guard scanned %d non-test files and %d import paths", len(files), total)
}

// --- the route --------------------------------------------------------------

// TestReleaseStateRouteIsOwnerOnlyGetAndFailsClosedWhenUnconfigured asserts
// the four transport facts of the new route: 401 unauthenticated, 403 for a
// non-owner role, 405 for POST, and 503 with an explicit code when no release
// observer is configured (which is the state of a process that was not given
// an explicit release source root).
func TestReleaseStateRouteIsOwnerOnlyGetAndFailsClosedWhenUnconfigured(t *testing.T) {
	h := testHandler(t)
	const path = "/v1/release/state"

	w := call(h, http.MethodGet, path, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401 (body %s)", w.Code, w.Body.String())
	}
	w = call(h, http.MethodGet, path, "", "runner")
	if w.Code != http.StatusForbidden {
		t.Fatalf("runner GET status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		w = call(h, method, path, `{}`, "owner")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405 (body %s)", method, w.Code, w.Body.String())
		}
	}
	w = call(h, http.MethodGet, path, "", "owner")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("owner GET with no observer configured: status = %d, want 503 (body %s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "release_observer_not_configured" {
		t.Fatalf("503 error code = %v, want release_observer_not_configured", body["error"])
	}
	for _, field := range []string{"error", "message", "schema_version", "correlation_id"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("503 body is missing the standard envelope field %q: %s", field, w.Body.String())
		}
	}
	// The unconfigured answer must not report a version at all.
	for _, forbidden := range []string{"release_version", "bundle_digest", "conditions", "promotable"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("the 503 body reports release state anyway (%q): %s", forbidden, w.Body.String())
		}
	}
}

// TestNoReleaseMutationRouteExists pins the read-only shape: there is no
// promote, rollback or set-preview route, and the one release path answers
// only GET.
func TestNoReleaseMutationRouteExists(t *testing.T) {
	h := testHandler(t)
	for _, path := range []string{
		"/v1/release/promote",
		"/v1/release/rollback",
		"/v1/release/preview",
		"/v1/release/state:promote",
		"/v1/release/state:rollback",
	} {
		w := call(h, http.MethodPost, path, `{}`, "owner")
		if w.Code != http.StatusNotFound {
			t.Fatalf("POST %s status = %d, want 404 (no such route); body %s", path, w.Code, w.Body.String())
		}
		w = call(h, http.MethodGet, path, "", "owner")
		if w.Code == http.StatusOK {
			t.Fatalf("GET %s answered 200; no such route may exist", path)
		}
	}
}

// TestOwnerConsoleExposesTheReleaseSurface asserts the additive owner-console
// block is served and that the pre-existing sections (including the sibling
// task's Repository section) are untouched. internal/web still has no test
// file of its own, so the assertion lives here, next to the existing
// owner-console assertions.
func TestOwnerConsoleExposesTheReleaseSurface(t *testing.T) {
	h := testHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`id="release-title"`, `id="release-refresh"`, `id="release-version"`, `id="release-promotable"`, `id="release-conditions"`, `id="release-rollback"`, `id="release-not-observed"`, `id="release-gaps"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// Additive only: nothing that was already there was removed.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, `id="repository-list"`, "Release evidence"} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	for _, want := range []string{"/v1/release/state", "release-conditions", "not_observed", "rollback_target_digest", "residual_gaps", "no route recorded", "not-observable"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js does not reference %q", want)
		}
	}
	// The sibling task's block must still be present in the same file.
	for _, want := range []string{"/v1/repositories", "executability"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// The console must not claim a capability was exercised.
	for _, forbidden := range []string{"capability exercised", "preview journey passed"} {
		if strings.Contains(strings.ToLower(html), forbidden) || strings.Contains(strings.ToLower(js), forbidden) {
			t.Fatalf("owner console claims %q", forbidden)
		}
	}
}

// TestCaptureCarriesRepositoryIDThroughTheTransport is acceptance A12 at the
// transport boundary: capture accepts repository_id, and the requirement list
// row and detail response each carry it for a linked Requirement and omit the
// field entirely for an unlinked one. The JSON itself is asserted, not the Go
// struct, so "omitted" means the key is absent from the document.
//
// Two handlers are used deliberately: each bounded owner read reserves
// internal/quota.ReadTransactionUsage against the Installation's daily budget,
// which is a real production property (see the note in repositories_test.go).
func TestCaptureCarriesRepositoryIDThroughTheTransport(t *testing.T) {
	h := testHandler(t)
	repositoryID := registerViaAPI(t, h, "reg-1", "https://github.com/O/N")

	w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"cap-1","text":"linked","repository_id":"`+repositoryID+`"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("linked capture status=%d body=%s", w.Code, w.Body.String())
	}
	linked := decodeBody(t, w.Body.Bytes())
	if linked["repository_id"] != repositoryID {
		t.Fatalf("capture response repository_id = %v, want %q (%s)", linked["repository_id"], repositoryID, w.Body.String())
	}
	linkedID, _ := linked["requirement_id"].(string)
	if linkedID == "" {
		t.Fatalf("capture returned no requirement_id: %s", w.Body.String())
	}

	w = call(h, http.MethodPost, "/v1/requirements", `{"request_id":"cap-2","text":"unlinked"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("unlinked capture status=%d body=%s", w.Code, w.Body.String())
	}
	unlinked := decodeBody(t, w.Body.Bytes())
	if _, present := unlinked["repository_id"]; present {
		t.Fatalf("an unlinked capture response carries repository_id: %s", w.Body.String())
	}
	unlinkedID, _ := unlinked["requirement_id"].(string)

	// A capture naming a Repository that is not registered is refused as a
	// 404 and creates nothing.
	w = call(h, http.MethodPost, "/v1/requirements", `{"request_id":"cap-3","text":"x","repository_id":"no-such-repository"}`, "owner")
	if w.Code != http.StatusNotFound {
		t.Fatalf("capture naming an unregistered repository status=%d body=%s", w.Code, w.Body.String())
	}

	// The page row: the linked row carries the key, the unlinked one omits it.
	w = call(h, http.MethodGet, "/v1/requirements", "", "owner")
	if w.Code != 200 {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	page := decodeBody(t, w.Body.Bytes())
	rows, ok := page["requirements"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("requirements page = %s", w.Body.String())
	}
	seen := map[string]map[string]any{}
	for _, row := range rows {
		m := row.(map[string]any)
		seen[m["requirement_id"].(string)] = m
	}
	if got := seen[linkedID]["repository_id"]; got != repositoryID {
		t.Fatalf("linked row repository_id = %v, want %q (%s)", got, repositoryID, w.Body.String())
	}
	if _, present := seen[unlinkedID]["repository_id"]; present {
		t.Fatalf("unlinked row carries repository_id: %s", w.Body.String())
	}

	// The detail response, on a fresh handler so the read budget is not the
	// thing under test.
	h2 := testHandler(t)
	repositoryID2 := registerViaAPI(t, h2, "reg-1", "https://github.com/O/N")
	w = call(h2, http.MethodPost, "/v1/requirements", `{"request_id":"cap-1","text":"linked","repository_id":"`+repositoryID2+`"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("linked capture status=%d body=%s", w.Code, w.Body.String())
	}
	detailID := decodeBody(t, w.Body.Bytes())["requirement_id"].(string)
	w = call(h2, http.MethodPost, "/v1/requirements", `{"request_id":"cap-2","text":"unlinked"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("unlinked capture status=%d body=%s", w.Code, w.Body.String())
	}
	unlinkedDetailID := decodeBody(t, w.Body.Bytes())["requirement_id"].(string)

	w = call(h2, http.MethodGet, "/v1/requirements/"+detailID, "", "owner")
	if w.Code != 200 {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	if got := decodeBody(t, w.Body.Bytes())["repository_id"]; got != repositoryID2 {
		t.Fatalf("detail repository_id = %v, want %q (%s)", got, repositoryID2, w.Body.String())
	}
	w = call(h2, http.MethodGet, "/v1/requirements/"+unlinkedDetailID, "", "owner")
	if w.Code != 200 {
		t.Fatalf("unlinked detail status=%d body=%s", w.Code, w.Body.String())
	}
	if _, present := decodeBody(t, w.Body.Bytes())["repository_id"]; present {
		t.Fatalf("unlinked detail carries repository_id: %s", w.Body.String())
	}
}

// TestOwnerConsoleExposesTheRepositoryScopedBacklog is acceptance A27's
// observable check. internal/web has no test file of its own (it has no
// browser and no deterministic render to assert), so the assertion that the
// server actually serves the new surface lives with the transport, exactly as
// V2-064's and V2-066's console assertions do.
func TestOwnerConsoleExposesTheRepositoryScopedBacklog(t *testing.T) {
	h := testHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`id="backlog-title"`, `id="backlog"`, `id="backlog-repository"`, `id="backlog-state"`, `id="backlog-reason"`, `id="backlog-rows"`, "V2-071 repository-scoped Requirement backlog"} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// Additive only: every pre-existing surface, including the two sibling
	// tasks' sections, is still there.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, `id="repository-list"`, "Release evidence", `id="release-conditions"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	for _, want := range []string{"requirement_backlog", "requirement_count", "installation_scope", "records no Repository association", "no count was measured", "/v1/repositories/", "/v1/requirements"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js does not reference %q", want)
		}
	}
	// Both sibling blocks must still be present in the same single file.
	for _, want := range []string{"/v1/release/state", "executability"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// The block must not claim a measurement that is absent.
	for _, forbidden := range []string{"capability exercised", "backlog measured for every repository"} {
		if strings.Contains(strings.ToLower(html), forbidden) || strings.Contains(strings.ToLower(js), forbidden) {
			t.Fatalf("owner console claims %q", forbidden)
		}
	}
}

// ===========================================================================
// Runner version report (V2-069)
// ===========================================================================

const (
	runnerDigestA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	runnerDigestB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// runnerVersionHandler builds a handler over a store and Service the test also
// holds, so a fixture that no public route can create (an Increment, which is
// deliberately not an owner operation) can be prepared through the Service
// while every assertion below still goes over HTTP.
func runnerVersionHandler(t *testing.T) (http.Handler, *application.Service) {
	t.Helper()
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{"owner": {Role: application.RoleOwner, Subject: "owner"}, "runner": {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"}}
	enrollment, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return api.New(api.Config{Authenticator: auth, Service: svc, RunnerEnrollment: enrollment, AllowedOrigins: []string{"https://console.example"}}), svc
}

func runnerRowsFromAPI(t *testing.T, h http.Handler) (map[string]any, []map[string]any) {
	t.Helper()
	w := call(h, http.MethodGet, "/v1/runners", "", "owner")
	if w.Code != 200 {
		t.Fatalf("GET /v1/runners status=%d body=%s", w.Code, w.Body.String())
	}
	doc := decodeBody(t, w.Body.Bytes())
	raw, ok := doc["runners"].([]any)
	if !ok {
		t.Fatalf("no runners array in %s", w.Body.String())
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("runner row is not an object: %v", r)
		}
		rows = append(rows, row)
	}
	return doc, rows
}

// TestRunnerVersionReportShapeIsClosedAtTheTransport is A3 at the transport
// boundary: the request DTO the api package declares carries exactly the four
// reported coordinates and no timestamp field, and neither it nor the
// heartbeat body it hangs off names a contract, bundle or provenance
// coordinate. The scan is over the package's own non-test .go files with
// go/ast and fails outright on a zero-file scan.
func TestRunnerVersionReportShapeIsClosedAtTheTransport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	fields := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != "runnerVersionBody" {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					fields["runnerVersionBody"] = append(fields["runnerVersionBody"], name.Name)
				}
				if field.Tag != nil {
					if tag, err := strconv.Unquote(field.Tag.Value); err == nil {
						fields["runnerVersionBody"] = append(fields["runnerVersionBody"], tag)
					}
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned zero non-test .go files; the working directory is not internal/api")
	}
	got := fields["runnerVersionBody"]
	if len(got) == 0 {
		t.Fatal("the scan did not find runnerVersionBody; it was renamed or the scan is broken")
	}
	joined := strings.ToLower(strings.Join(got, " "))
	for _, forbidden := range []string{"timestamp", "hostname", "host", "ip", "path", "root", "env", "message", "detail", "output", "text", "contract_release", "contract_digest", "runner_api", "bundle_digest", "candidate_id", "key_id", "algorithm", "secret", "credential", "password", "bearer", "authorization"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("runnerVersionBody mentions %q: %v", forbidden, got)
		}
	}
	for _, required := range []string{"version", "binary_sha256", "schema_min", "schema_max"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("runnerVersionBody does not carry %q: %v", required, got)
		}
	}
	t.Logf("scanned %d non-test files; runnerVersionBody = %v", scanned, got)
}

// TestHeartbeatRunnerVersionValidationAtTheTransport is A4 driven through the
// API: the shape table, the partial-object refusal, and the fact that a
// refused report stores nothing at all.
func TestHeartbeatRunnerVersionValidationAtTheTransport(t *testing.T) {
	body := func(request string, object string) string {
		if object == "" {
			return `{"request_id":"` + request + `"}`
		}
		return `{"request_id":"` + request + `","runner_version":` + object + `}`
	}
	full := func(version, digest string, min, max string) string {
		return `{"version":"` + version + `","binary_sha256":"` + digest + `","schema_min":` + min + `,"schema_max":` + max + `}`
	}

	accepted := []struct{ name, object string }{
		{"the smallest legal report", full("0.1.0", runnerDigestA, "1", "1")},
		{"a prerelease version", full("0.1.0-dev", runnerDigestA, "2", "7")},
		{"a dotted prerelease", full("1.0.0-rc.1", runnerDigestB, "3", "9")},
		{"a two-digit triple", full("10.20.30", runnerDigestB, "1", "4096")},
	}
	for i, c := range accepted {
		h, _ := runnerVersionHandler(t)
		w := call(h, http.MethodPost, "/v1/runner/heartbeat", body("hb-ok-"+strconv.Itoa(i), c.object), "runner")
		if w.Code != 200 {
			t.Fatalf("%s: status=%d body=%s", c.name, w.Code, w.Body.String())
		}
		_, rows := runnerRowsFromAPI(t, h)
		if len(rows) != 1 || rows[0]["report_state"] != "reported" {
			t.Fatalf("%s: rows=%v", c.name, rows)
		}
	}

	rejected := []struct{ name, object string }{
		// version shape: six refusals, including a bare major.minor, a
		// leading v, an uppercase prerelease and an empty string.
		{"a bare major.minor version", full("1.2", runnerDigestA, "1", "1")},
		{"a leading v", full("v1.2.3", runnerDigestA, "1", "1")},
		{"an uppercase prerelease", full("1.2.3-RC1", runnerDigestA, "1", "1")},
		{"an empty version", full("", runnerDigestA, "1", "1")},
		{"a four-part version", full("1.2.3.4", runnerDigestA, "1", "1")},
		{"a trailing hyphen", full("1.2.3-", runnerDigestA, "1", "1")},
		// digest shape.
		{"a 63-character digest", full("1.2.3", strings.Repeat("a", 63), "1", "1")},
		{"a 65-character digest", full("1.2.3", strings.Repeat("a", 65), "1", "1")},
		{"an uppercase digest", full("1.2.3", strings.ToUpper(runnerDigestA), "1", "1")},
		{"a non-hex digest", full("1.2.3", strings.Repeat("g", 64), "1", "1")},
		// interval shape.
		{"schema_min below one", full("1.2.3", runnerDigestA, "0", "4")},
		{"schema_max below schema_min", full("1.2.3", runnerDigestA, "5", "4")},
		{"schema_max above the ceiling", full("1.2.3", runnerDigestA, "1", "4097")},
		{"schema_min above the ceiling", full("1.2.3", runnerDigestA, "4097", "4097")},
		// partial objects: any strict non-empty subset, and the empty object.
		{"schema_max with no schema_min", `{"schema_max":7}`},
		{"schema_min alone", `{"schema_min":2}`},
		{"the version alone", `{"version":"1.2.3"}`},
		{"the digest alone", `{"binary_sha256":"` + runnerDigestA + `"}`},
		{"the interval with no binary", `{"schema_min":2,"schema_max":7}`},
		{"the binary with no interval", `{"version":"1.2.3","binary_sha256":"` + runnerDigestA + `"}`},
		{"everything but schema_min", `{"version":"1.2.3","binary_sha256":"` + runnerDigestA + `","schema_max":7}`},
		{"the wholly empty object", `{}`},
	}
	for i, c := range rejected {
		h, _ := runnerVersionHandler(t)
		w := call(h, http.MethodPost, "/v1/runner/heartbeat", body("hb-bad-"+strconv.Itoa(i), c.object), "runner")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s, want 400", c.name, w.Code, w.Body.String())
		}
		// Nothing was stored: the Runner is not even known, because the
		// refused heartbeat wrote no observation either.
		doc, rows := runnerRowsFromAPI(t, h)
		if len(rows) != 0 {
			t.Fatalf("%s: a refused report left %v", c.name, rows)
		}
		if doc["intersection_state"] != "unknown" {
			t.Fatalf("%s: intersection_state=%v after a refusal", c.name, doc["intersection_state"])
		}
	}

	// Omitting the object entirely is a 200 that stores no report, and the
	// Runner still enumerates as not-reported.
	h, _ := runnerVersionHandler(t)
	if w := call(h, http.MethodPost, "/v1/runner/heartbeat", body("hb-absent", ""), "runner"); w.Code != 200 {
		t.Fatalf("a heartbeat with no runner_version: status=%d body=%s", w.Code, w.Body.String())
	}
	doc, rows := runnerRowsFromAPI(t, h)
	if len(rows) != 1 || rows[0]["report_state"] != "not-reported" {
		t.Fatalf("rows=%v", rows)
	}
	for _, absent := range []string{"version", "binary_sha256", "schema_min", "schema_max", "reported_at"} {
		if _, present := rows[0][absent]; present {
			t.Fatalf("a not-reported row carries %q", absent)
		}
	}
	if doc["intersection_state"] != "unknown" {
		t.Fatalf("intersection_state=%v with one unreported Runner", doc["intersection_state"])
	}

	// An unknown field inside runner_version is a 400: DisallowUnknownFields
	// plus additionalProperties:false in the contract.
	h, _ = runnerVersionHandler(t)
	unknown := `{"request_id":"hb-unknown","runner_version":{"version":"1.2.3","binary_sha256":"` + runnerDigestA + `","schema_min":1,"schema_max":2,"contract_release":"r"}}`
	if w := call(h, http.MethodPost, "/v1/runner/heartbeat", unknown, "runner"); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field inside runner_version: status=%d body=%s", w.Code, w.Body.String())
	}
	h, _ = runnerVersionHandler(t)
	reportedAt := `{"request_id":"hb-ts","runner_version":{"version":"1.2.3","binary_sha256":"` + runnerDigestA + `","schema_min":1,"schema_max":2,"reported_at":"2026-01-01T00:00:00Z"}}`
	if w := call(h, http.MethodPost, "/v1/runner/heartbeat", reportedAt, "runner"); w.Code != http.StatusBadRequest {
		t.Fatalf("a Runner-supplied reported_at: status=%d body=%s, want 400", w.Code, w.Body.String())
	}
	t.Logf("accepted %d literals, refused %d literals through the transport", len(accepted), len(rejected))
}

// TestRunnersRouteIsOwnerOnlyGetWithAClosedResponse is A10.
func TestRunnersRouteIsOwnerOnlyGetWithAClosedResponse(t *testing.T) {
	h, _ := runnerVersionHandler(t)
	const path = "/v1/runners"
	if w := call(h, http.MethodGet, path, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(h, http.MethodGet, path, "", "runner"); w.Code != http.StatusForbidden {
		t.Fatalf("runner role status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(h, http.MethodPost, path, `{}`, "owner"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d body=%s; there is deliberately no reporting endpoint here", w.Code, w.Body.String())
	}
	if w := call(h, http.MethodGet, path, "", "owner"); w.Code != 200 {
		t.Fatalf("owner status=%d body=%s", w.Code, w.Body.String())
	}

	body := `{"request_id":"hb-1","runner_version":{"version":"1.2.3","binary_sha256":"` + runnerDigestA + `","schema_min":2,"schema_max":7}}`
	if w := call(h, http.MethodPost, "/v1/runner/heartbeat", body, "runner"); w.Code != 200 {
		t.Fatalf("heartbeat status=%d body=%s", w.Code, w.Body.String())
	}
	doc, rows := runnerRowsFromAPI(t, h)
	topLevel := map[string]bool{"runners": true, "runner_count": true, "truncated": true, "intersection_state": true, "intersection_schema_min": true, "intersection_schema_max": true}
	for key := range doc {
		if !topLevel[key] {
			t.Fatalf("the response carries an unnamed top-level field %q", key)
		}
	}
	rowFields := map[string]bool{"runner_id": true, "report_state": true, "version": true, "binary_sha256": true, "schema_min": true, "schema_max": true, "reported_at": true}
	for _, row := range rows {
		for key := range row {
			if !rowFields[key] {
				t.Fatalf("a runner row carries an unnamed field %q", key)
			}
		}
	}
	if len(rows) != 1 || rows[0]["runner_id"] != "runner-1" || rows[0]["version"] != "1.2.3" {
		t.Fatalf("rows=%v", rows)
	}
	if doc["intersection_state"] != "non-empty" || doc["intersection_schema_min"] != float64(2) || doc["intersection_schema_max"] != float64(7) {
		t.Fatalf("intersection = %v %v %v", doc["intersection_state"], doc["intersection_schema_min"], doc["intersection_schema_max"])
	}
	// No synthetic or placeholder value anywhere in the document.
	raw := runnersRawBody(t, h)
	for _, forbidden := range []string{"contract_release", "contract_digest", "runner_api", "bundle_digest", "candidate_id", "key_id", "algorithm", "TODO", "placeholder", "unknown-version", "0.0.0"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("the response contains %q: %s", forbidden, raw)
		}
	}
}

func runnersRawBody(t *testing.T, h http.Handler) string {
	t.Helper()
	w := call(h, http.MethodGet, "/v1/runners", "", "owner")
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	return w.Body.String()
}

// TestAuthFileDoesNotReadTheRunnerVersionReport is A7's mechanical half in
// internal/api: auth.go, the transport authentication boundary, mentions no
// report identifier. The matcher is verified against a synthetic
// known-positive first, and a zero-declaration scan fails outright.
func TestAuthFileDoesNotReadTheRunnerVersionReport(t *testing.T) {
	identifiers := []string{"RunnerVersionInput", "RunnerVersionReport", "RunnerVersionReports", "runnerVersionBody", "RunnerVersionListView", "listRunners", "runnersPath"}
	hits := func(n ast.Node) []string {
		found := []string{}
		ast.Inspect(n, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			for _, name := range identifiers {
				if ident.Name == name {
					found = append(found, name)
				}
			}
			return true
		})
		return found
	}
	positive := "package api\n\nfunc (a CombinedAuthenticator) Authenticate() { var r RunnerVersionReport; _ = r }\n"
	synthetic, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits(synthetic)) == 0 {
		t.Fatal("positive control: a synthetic authenticator naming RunnerVersionReport was not flagged")
	}

	file, err := parser.ParseFile(token.NewFileSet(), "auth.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse auth.go: %v", err)
	}
	declarations := 0
	names := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		declarations++
		names[fn.Name.Name] = true
		if found := hits(fn); len(found) != 0 {
			t.Fatalf("auth.go: %s names %v; the authentication boundary must never read a Runner's self-claim", fn.Name.Name, found)
		}
	}
	if declarations == 0 {
		t.Fatal("scanned zero function declarations in auth.go; the scan is broken")
	}
	if !names["Authenticate"] {
		t.Fatal("auth.go declares no Authenticate method; the boundary moved and this guard is vacuous")
	}
	t.Logf("scanned auth.go: %d function declarations, none naming a report identifier", declarations)
}

// runnerOperation is one runner-facing HTTP call in the scripted sequence.
type runnerOperation struct {
	name, method, path, body string
}

// driveRunnerRoutes prepares an Increment through the Service (no public route
// creates one), then calls each of the six runner-facing routes once and
// returns the status code and body of each. When report is non-empty it is
// posted first as this Runner's version report; when it is empty an
// equally-shaped heartbeat carrying none is posted instead, so both arms
// consume the same identifiers from the deterministic generator.
func driveRunnerRoutes(t *testing.T, report string) []struct {
	name   string
	status int
	body   string
} {
	t.Helper()
	h, svc := runnerVersionHandler(t)
	ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})

	first := `{"request_id":"hb-0"}`
	if report != "" {
		first = `{"request_id":"hb-0","runner_version":` + report + `}`
	}
	if w := call(h, http.MethodPost, "/v1/runner/heartbeat", first, "runner"); w.Code != 200 {
		t.Fatalf("the first heartbeat: status=%d body=%s", w.Code, w.Body.String())
	}

	captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "cap", Text: "work"})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := svc.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version}); err != nil {
		t.Fatal(err)
	}

	claimed := call(h, http.MethodPost, "/v1/runner/claims:acquire",
		`{"request_id":"claim","increment_id":"`+planned.IncrementID+`","expected_increment_version":2,"control_revision":0}`, "runner")
	claim := decodeBody(t, claimed.Body.Bytes())
	executionID, _ := claim["execution_id"].(string)
	leaseID, _ := claim["lease_id"].(string)
	fencing := 1
	if v, ok := claim["fencing_token"].(float64); ok {
		fencing = int(v)
	}

	operations := []runnerOperation{
		{"permits:check", http.MethodPost, "/v1/runner/permits:check",
			`{"request_id":"permit","kind":"external-effect","target":{},"control_revision":0,"fencing_token":` + strconv.Itoa(fencing) + `,"expected_fencing_token":` + strconv.Itoa(fencing) + `,"resource":"` + executionID + `"}`},
		{"executions:start", http.MethodPost, "/v1/executions/" + executionID + ":start",
			`{"request_id":"start","expected_execution_version":1,"control_revision":0}`},
		{"checkpoints", http.MethodPost, "/v1/runner/checkpoints",
			`{"request_id":"checkpoint","execution_id":"` + executionID + `","lease_id":"` + leaseID + `","fencing_token":` + strconv.Itoa(fencing) + `,"control_revision":0}`},
		{"heartbeat", http.MethodPost, "/v1/runner/heartbeat", `{"request_id":"hb-1"}`},
		{"executions/result", http.MethodPost, "/v1/executions/result",
			`{"request_id":"result","execution_id":"` + executionID + `","lease_id":"` + leaseID + `","expected_execution_version":2,"fencing_token":` + strconv.Itoa(fencing) + `,"control_revision":0,"succeeded":true}`},
	}
	out := []struct {
		name   string
		status int
		body   string
	}{{"claims:acquire", claimed.Code, claimed.Body.String()}}
	for _, op := range operations {
		w := call(h, op.method, op.path, op.body, "runner")
		out = append(out, struct {
			name   string
			status int
			body   string
		}{op.name, w.Code, w.Body.String()})
	}
	return out
}

// TestAnAbsurdRunnerVersionReportChangesNoStatusCode is A7's behavioural half
// at the transport: a Runner whose reported interval sits at the declared
// ceiling -- excluding every plausible canonical schema -- succeeds unchanged
// at all six runner-facing operations, with identical status codes and
// identical response bodies to a Runner that reported nothing.
func TestAnAbsurdRunnerVersionReportChangesNoStatusCode(t *testing.T) {
	absurd := `{"version":"1.2.3","binary_sha256":"` + runnerDigestA + `","schema_min":4096,"schema_max":4096}`
	reported := driveRunnerRoutes(t, absurd)
	silent := driveRunnerRoutes(t, "")
	if len(reported) != 6 || len(silent) != 6 {
		t.Fatalf("expected six runner-facing operations, got %d and %d", len(reported), len(silent))
	}
	for i := range reported {
		if reported[i].status != silent[i].status {
			t.Fatalf("%s: status %d after reporting an absurd interval, %d after reporting nothing", reported[i].name, reported[i].status, silent[i].status)
		}
		if reported[i].status != 200 {
			t.Fatalf("%s: status %d; the operation did not succeed in either arm: %s", reported[i].name, reported[i].status, reported[i].body)
		}
		if reported[i].body != silent[i].body {
			t.Fatalf("%s: body differed\n reported=%s\n silent  =%s", reported[i].name, reported[i].body, silent[i].body)
		}
		t.Logf("%s: status %d in both arms, body identical", reported[i].name, reported[i].status)
	}
}

// TestOwnerConsoleExposesTheRunnerVersionReports is A13.
func TestOwnerConsoleExposesTheRunnerVersionReports(t *testing.T) {
	h, _ := runnerVersionHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`id="runners-title"`, `id="runners-refresh"`, `id="runners-count"`, `id="runners-silent"`, `id="runners-intersection"`, `id="runners-intersection-reason"`, `id="runners-rows"`, "V2-069 Runner version reports"} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// Additive only: every pre-existing surface, including the three sibling
	// tasks' sections, is still there.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, `id="repository-list"`, "Release evidence", `id="release-conditions"`, `id="backlog-rows"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	for _, want := range []string{"/v1/runners", "report_state", "intersection_state", "not-reported", "has never reported a version", "runners-silent", "shared interval"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js does not reference %q", want)
		}
	}
	// The sibling blocks must still be present in the same single file.
	for _, want := range []string{"/v1/release/state", "executability", "requirement_backlog"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// The block renders named rows, not raw JSON.
	marker := strings.Index(js, "// V2-069 Runner version reports")
	if marker < 0 {
		t.Fatal("owner.js does not carry the V2-069 marker comment")
	}
	runnerBlock := js[marker:]
	if strings.Contains(runnerBlock, "JSON.stringify") {
		t.Fatal("the V2-069 owner.js block renders raw JSON")
	}
	// No external asset, script or font is introduced by the block.
	for _, forbidden := range []string{"http://", "https://", "//cdn", "importScripts", "setInterval", "setTimeout", "@font-face"} {
		if strings.Contains(runnerBlock, forbidden) {
			t.Fatalf("the V2-069 owner.js block references %q", forbidden)
		}
	}
	// No credential-shaped and no email-shaped value in the rendered markup.
	lowered := strings.ToLower(html)
	for _, forbidden := range []string{"password", "secret", "api_key", "apikey", "bearer ", "authorization:", "private_key", "@example.", "@gmail.", "accounts.google.com"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the rendered owner console carries a credential- or email-shaped value %q", forbidden)
		}
	}
	if strings.Contains(html, "@") {
		t.Fatalf("the rendered owner console contains an at-sign, which is the email shape this check refuses")
	}
	// The console must not claim a measurement it does not have.
	for _, forbidden := range []string{"every machine reported", "versions are compatible", "capability exercised"} {
		if strings.Contains(lowered, forbidden) || strings.Contains(strings.ToLower(js), forbidden) {
			t.Fatalf("owner console claims %q", forbidden)
		}
	}
}

// ===========================================================================
// V2-067: the Provider registry at the transport boundary
// ===========================================================================

// providerRegistryDoc reads GET /v1/providers as the owner and returns the
// decoded document, the provider rows and the raw body.
func providerRegistryDoc(t *testing.T, h http.Handler) (map[string]any, []map[string]any, string) {
	t.Helper()
	w := call(h, http.MethodGet, "/v1/providers", "", "owner")
	if w.Code != 200 {
		t.Fatalf("GET /v1/providers status=%d body=%s", w.Code, w.Body.String())
	}
	doc := decodeBody(t, w.Body.Bytes())
	raw, ok := doc["providers"].([]any)
	if !ok {
		t.Fatalf("no providers array in %s", w.Body.String())
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("a provider row is not an object: %v", r)
		}
		rows = append(rows, row)
	}
	return doc, rows, w.Body.String()
}

// TestProvidersRouteIsOwnerOnlyGetWithAClosedResponse is V2-067 A13.
func TestProvidersRouteIsOwnerOnlyGetWithAClosedResponse(t *testing.T) {
	h, _ := runnerVersionHandler(t)
	const path = "/v1/providers"
	if w := call(h, http.MethodGet, path, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(h, http.MethodGet, path, "", "runner"); w.Code != http.StatusForbidden {
		t.Fatalf("runner role status=%d body=%s", w.Code, w.Body.String())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if w := call(h, method, path, `{}`, "owner"); w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status=%d body=%s; there is deliberately no mutation verb on this path", method, w.Code, w.Body.String())
		}
	}
	if w := call(h, http.MethodGet, path, "", "owner"); w.Code != 200 {
		t.Fatalf("owner status=%d body=%s", w.Code, w.Body.String())
	}

	doc, rows, raw := providerRegistryDoc(t, h)
	// The response carries no field this Work Order did not name.
	topLevel := map[string]bool{"providers": true}
	for key := range doc {
		if !topLevel[key] {
			t.Fatalf("the response carries an unnamed top-level field %q", key)
		}
	}
	rowFields := map[string]bool{
		"provider": true, "authorized": true, "authorization_ref": true,
		"verified_by_loop_invocation": true, "health": true, "blocked_reason": true,
		"last_observed_at": true, "observation_count": true, "stale": true,
		"runaway_detection": true, "concurrency": true, "assignments": true,
		// V2-074's two additive blocks. The assertion is unchanged in form:
		// the response still carries no field this Work Order did not name.
		"compatibility": true, "handoff": true,
	}
	runawayFields := map[string]bool{"scope": true, "state": true, "thresholds_declared_in": true}
	concurrencyFields := map[string]bool{"active_assignments": true, "declared_ceiling": true, "ceiling_source": true, "remaining": true, "exhausted": true}
	compatibilityFields := map[string]bool{"cli_version_interval": true, "loop_version_interval": true, "observed_loop_version": true, "cli_compatibility": true, "loop_compatibility": true}
	intervalFields := map[string]bool{"from": true, "until": true}
	handoffFields := map[string]bool{"disposition": true, "target": true, "waiting_reason": true}
	if len(rows) != 3 {
		t.Fatalf("the response carries %d providers, want exactly 3", len(rows))
	}
	names := []string{}
	for _, row := range rows {
		for key := range row {
			if !rowFields[key] {
				t.Fatalf("a provider row carries an unnamed field %q", key)
			}
		}
		for _, named := range []string{"provider", "authorized", "authorization_ref", "verified_by_loop_invocation", "health", "blocked_reason", "last_observed_at", "observation_count", "stale", "runaway_detection", "concurrency", "assignments", "compatibility", "handoff"} {
			if _, ok := row[named]; !ok {
				t.Fatalf("a provider row omits the required field %q: %v", named, row)
			}
		}
		runaway, ok := row["runaway_detection"].(map[string]any)
		if !ok {
			t.Fatalf("runaway_detection is not an object: %v", row["runaway_detection"])
		}
		for key := range runaway {
			if !runawayFields[key] {
				t.Fatalf("runaway_detection carries an unnamed field %q", key)
			}
		}
		concurrency, ok := row["concurrency"].(map[string]any)
		if !ok {
			t.Fatalf("concurrency is not an object: %v", row["concurrency"])
		}
		for key := range concurrency {
			if !concurrencyFields[key] {
				t.Fatalf("concurrency carries an unnamed field %q", key)
			}
		}
		compatibility, isObject := row["compatibility"].(map[string]any)
		if !isObject {
			t.Fatalf("compatibility is not an object: %v", row["compatibility"])
		}
		for key := range compatibility {
			if !compatibilityFields[key] {
				t.Fatalf("compatibility carries an unnamed field %q", key)
			}
		}
		for _, intervalKey := range []string{"cli_version_interval", "loop_version_interval"} {
			interval, isInterval := compatibility[intervalKey].(map[string]any)
			if !isInterval {
				t.Fatalf("%s is not an object: %v", intervalKey, compatibility[intervalKey])
			}
			for key := range interval {
				if !intervalFields[key] {
					t.Fatalf("%s carries an unnamed field %q", intervalKey, key)
				}
			}
			for _, bound := range []string{"from", "until"} {
				value, isText := interval[bound].(string)
				if !isText || value == "" {
					t.Fatalf("%s.%s = %v, want a declared version bound", intervalKey, bound, interval[bound])
				}
			}
		}
		// The two verdicts are in the closed set, and unknown is shown as
		// unknown rather than as blank or as compatible.
		for _, verdictKey := range []string{"cli_compatibility", "loop_compatibility"} {
			switch compatibility[verdictKey] {
			case "compatible", "incompatible", "unknown":
			default:
				t.Fatalf("%s = %v, which is outside the closed verdict set", verdictKey, compatibility[verdictKey])
			}
		}
		// With no release observer configured this handler reports no Loop
		// version at all, and the loop verdict is then unknown rather than a
		// synthesized instant or a reassuring default.
		if compatibility["observed_loop_version"] != "" {
			t.Fatalf("observed_loop_version = %v with no release observer configured; no version may be synthesized for an absence", compatibility["observed_loop_version"])
		}
		if compatibility["loop_compatibility"] != "unknown" {
			t.Fatalf("loop_compatibility = %v with no observed Loop version, want unknown", compatibility["loop_compatibility"])
		}
		if compatibility["cli_compatibility"] != "unknown" {
			t.Fatalf("cli_compatibility = %v with no observed contract-incompatible failure, want unknown", compatibility["cli_compatibility"])
		}
		handoff, isObject := row["handoff"].(map[string]any)
		if !isObject {
			t.Fatalf("handoff is not an object: %v", row["handoff"])
		}
		for key := range handoff {
			if !handoffFields[key] {
				t.Fatalf("handoff carries an unnamed field %q", key)
			}
		}
		switch handoff["disposition"] {
		case "none", "handoff-proposed", "waiting":
		default:
			t.Fatalf("disposition = %v, which is outside the closed set", handoff["disposition"])
		}
		name, _ := row["provider"].(string)
		names = append(names, name)
	}
	if !reflect.DeepEqual(names, []string{"codex", "claude", "opencode"}) {
		t.Fatalf("provider order = %v, want the fixed declared order", names)
	}

	// No synthetic or placeholder value anywhere in the document, and no
	// monetary vocabulary and no threshold number either.
	for _, forbidden := range []string{
		"TODO", "placeholder", "unknown-provider", "example.com", "@",
		"budget", "quota", "billing", "spend", "cost", "credit",
		"USD", "usd", "16", "10.0", "2.0",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("the response contains %q: %s", forbidden, raw)
		}
	}
	// A Provider the Loop has never driven reports unknown and stale, not a
	// reassuring default.
	for _, row := range rows {
		if row["health"] != "unknown" {
			t.Fatalf("provider %v health=%v with no observation at all, want unknown", row["provider"], row["health"])
		}
		if row["stale"] != true {
			t.Fatalf("provider %v stale=%v with no observation at all", row["provider"], row["stale"])
		}
		if row["blocked_reason"] != "never-invoked-by-loop" {
			t.Fatalf("provider %v blocked_reason=%v", row["provider"], row["blocked_reason"])
		}
		if row["authorized"] != true || row["authorization_ref"] != "psa-foundation-001" {
			t.Fatalf("provider %v authorization = %v / %v", row["provider"], row["authorized"], row["authorization_ref"])
		}
		if row["verified_by_loop_invocation"] != false {
			t.Fatalf("provider %v is verified with no observation", row["provider"])
		}
	}
}

// TestProviderObservationShapeIsClosedAtTheTransport is V2-067 A10 and A13 at
// the transport boundary: the additive request objects carry exactly the named
// fields, an unknown field is refused, and a value outside a closed enum is a
// 400 that records nothing.
func TestProviderObservationShapeIsClosedAtTheTransport(t *testing.T) {
	// The DTO the api package declares carries exactly three fields and none of
	// them can hold text.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name == nil || spec.Name.Name != "providerObservationBody" {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				fields["providerObservationBody"] = append(fields["providerObservationBody"], name.Name)
			}
		}
		return true
	})
	got := fields["providerObservationBody"]
	if !reflect.DeepEqual(got, []string{"Name", "FailureClass", "StoppedForInspection"}) {
		t.Fatalf("providerObservationBody fields = %v, want exactly [Name FailureClass StoppedForInspection]", got)
	}
	for _, forbidden := range []string{"Message", "Detail", "Output", "Result", "Session", "Text", "At", "ObservedAt", "Prompt", "Response"} {
		for _, name := range got {
			if name == forbidden {
				t.Fatalf("providerObservationBody declares %q", forbidden)
			}
		}
	}

	h, _ := runnerVersionHandler(t)
	// An unknown field anywhere in the additive objects is refused by the
	// existing strict decoder, which is what additionalProperties:false
	// declares in the contract.
	unknown := []string{
		`{"request_id":"r-1","execution_id":"e-1","lease_id":"l-1","expected_execution_version":1,"fencing_token":1,"control_revision":0,"succeeded":true,"provider_observation":{"name":"claude","message":"hello"}}`,
		`{"request_id":"r-2","execution_id":"e-1","lease_id":"l-1","expected_execution_version":1,"fencing_token":1,"control_revision":0,"succeeded":true,"provider_observation":{"name":"claude","output":"raw provider text"}}`,
		`{"request_id":"r-3","execution_id":"e-1","lease_id":"l-1","expected_execution_version":1,"fencing_token":1,"control_revision":0,"succeeded":true,"provider_observation":{"name":"claude","session_id":"s-1"}}`,
	}
	for _, body := range unknown {
		w := call(h, http.MethodPost, "/v1/executions/result", body, "runner")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("an unknown provider_observation field was accepted: status=%d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid_json") {
			t.Fatalf("unexpected refusal for an unknown field: %s", w.Body.String())
		}
	}
	// A failure_class outside the closed enum is a 400, and so is an
	// unrecognised provider name, and neither records anything: the registry
	// still reports every Provider as never invoked.
	badEnum := []string{
		`{"request_id":"r-4","execution_id":"e-1","lease_id":"l-1","expected_execution_version":1,"fencing_token":1,"control_revision":0,"succeeded":true,"provider_observation":{"name":"claude","failure_class":"provider-quota"}}`,
		`{"request_id":"r-5","execution_id":"e-1","lease_id":"l-1","expected_execution_version":1,"fencing_token":1,"control_revision":0,"succeeded":true,"provider_observation":{"name":"gemini"}}`,
		`{"request_id":"r-6","execution_id":"e-1","lease_id":"l-1","expected_execution_version":1,"fencing_token":1,"control_revision":0,"succeeded":true,"provider_observation":{"name":""}}`,
	}
	for _, body := range badEnum {
		w := call(h, http.MethodPost, "/v1/executions/result", body, "runner")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("a value outside a closed enum was accepted: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	// An unrecognised provider on start is a 400 too.
	if w := call(h, http.MethodPost, "/v1/executions/e-1:start", `{"request_id":"s-1","expected_execution_version":1,"control_revision":0,"provider":"gemini"}`, "runner"); w.Code != http.StatusBadRequest {
		t.Fatalf("start accepted an undeclared provider: status=%d body=%s", w.Code, w.Body.String())
	}
	// And nothing was recorded by any of the refusals.
	_, rows, _ := providerRegistryDoc(t, h)
	for _, row := range rows {
		if row["observation_count"] != float64(0) || row["health"] != "unknown" {
			t.Fatalf("a refused request recorded state: %v", row)
		}
		assignments, _ := row["assignments"].([]any)
		if len(assignments) != 0 {
			t.Fatalf("a refused request recorded an assignment: %v", row)
		}
	}
}

// TestOwnerConsoleExposesTheProviderRegistry is V2-067 A14.
func TestOwnerConsoleExposesTheProviderRegistry(t *testing.T) {
	h, _ := runnerVersionHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`id="providers-title"`, `id="providers-refresh"`, `id="providers-waiting"`, `id="providers-stopped"`, `id="providers-rows"`, "V2-067 Provider registry"} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// Additive only: every pre-existing surface, including the sibling tasks'
	// sections, is still there.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, `id="repository-list"`, "Release evidence", `id="release-conditions"`, `id="backlog-rows"`, `id="runners-rows"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	marker := strings.Index(js, "// V2-067 Provider registry")
	if marker < 0 {
		t.Fatal("owner.js does not carry the V2-067 marker comment")
	}
	block := js[marker:]
	// The block reads the registry and renders each declared observable.
	for _, want := range []string{"/v1/providers", "health", "blocked", "stale", "runaway_detection", "active_assignments", "verified_by_loop_invocation", "authorization", "providers-waiting", "providers-stopped"} {
		if !strings.Contains(block, want) {
			t.Fatalf("the V2-067 owner.js block does not reference %q", want)
		}
	}
	// The sibling blocks are still present in the same single file.
	for _, want := range []string{"/v1/release/state", "/v1/runners", "executability", "requirement_backlog"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// The block renders named rows, not raw JSON, and adds no timer, no
	// external asset, script or font: internal/web is embedded and stays
	// self-contained.
	for _, forbidden := range []string{"JSON.stringify", "http://", "https://", "//cdn", "importScripts", "setInterval", "setTimeout", "@font-face"} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("the V2-067 owner.js block references %q", forbidden)
		}
	}
	// No credential-shaped and no email-shaped value in the block or in the
	// rendered markup of the section. The matcher is verified first: the
	// pre-existing repository-locator block does contain an '@', so a scan
	// over the whole file would not be evidence about this block.
	if !strings.Contains(js, `"@"`) {
		t.Fatal("the pre-existing owner.js locator block no longer contains an '@'; this scan's control is stale")
	}
	if strings.Contains(block, "@") {
		t.Fatalf("the V2-067 owner.js block carries an '@'")
	}
	htmlMarker := strings.Index(html, "V2-067 Provider registry")
	section := html[htmlMarker:]
	if strings.Contains(section, "@") {
		t.Fatal("the V2-067 owner console section carries an '@'")
	}
	lowered := strings.ToLower(section)
	for _, forbidden := range []string{"password", "secret", "api_key", "apikey", "bearer ", "authorization:", "private_key", "budget", "billing", "quota", "spend", "credit"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the rendered Providers section carries a forbidden value %q", forbidden)
		}
	}
	// A reader can see which Providers are waiting for the owner without
	// parsing anything: the waiting list is a named heading with its own list.
	if !strings.Contains(section, "Waiting for the owner to sign in to a CLI") {
		t.Fatal("the Providers section does not name the authentication wait in plain words")
	}
}

// ===========================================================================
// V2-073 A11/A12: the capture time on the HTTP surface and on the owner
// console.
// ===========================================================================

// captureTimeHandler builds a handler together with the store behind it, so a
// Requirement that predates the capture time field can be written directly
// rather than simulated.
func captureTimeHandler(t *testing.T) (http.Handler, *memory.Store) {
	t.Helper()
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{"owner": {Role: application.RoleOwner, Subject: "owner"}, "runner": {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"}}
	enrollment, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return api.New(api.Config{Authenticator: auth, Service: svc, RunnerEnrollment: enrollment, AllowedOrigins: []string{"https://console.example"}}), st
}

// TestRequirementResponsesCarryTheCaptureTimeAndOmitItWhenAbsent is A11.
func TestRequirementResponsesCarryTheCaptureTimeAndOmitItWhenAbsent(t *testing.T) {
	h, st := captureTimeHandler(t)

	w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"capture-1","text":"a requirement"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/requirements status=%d body=%s", w.Code, w.Body.String())
	}
	created := decodeBody(t, w.Body.Bytes())
	id, _ := created["requirement_id"].(string)
	if id == "" {
		t.Fatalf("no requirement_id in %s", w.Body.String())
	}
	// The capture response itself is NOT widened by this task.
	if _, ok := created["captured_at"]; ok {
		t.Fatalf("the capture response carries captured_at, which this Work Order did not name: %s", w.Body.String())
	}

	// A legacy Requirement written directly, with no capture time at all.
	ctx := context.Background()
	rid, err := domainRequirementID("requirement-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, requirementWithNoCaptureTime(rid), 0)
	}); err != nil {
		t.Fatal(err)
	}

	wantDetailKeys := map[string]bool{"requirement_id": true, "original_text": true, "status": true, "version": true, "increments": true, "next_action": true, "page_size": true, "truncated": true, "requested_by": true, "repository_id": true, "captured_at": true}
	// Detail: recorded.
	w = call(h, http.MethodGet, "/v1/requirements/"+id, "", "owner")
	if w.Code != 200 {
		t.Fatalf("GET /v1/requirements/{id} status=%d body=%s", w.Code, w.Body.String())
	}
	detail := decodeBody(t, w.Body.Bytes())
	at, ok := detail["captured_at"].(string)
	if !ok || at == "" {
		t.Fatalf("a recorded Requirement's detail carries no captured_at: %s", w.Body.String())
	}
	wantAt := (clock{}).Now().Format(time.RFC3339Nano)
	if at != wantAt {
		t.Fatalf("captured_at = %q, want the injected clock's instant %q", at, wantAt)
	}
	for key := range detail {
		if !wantDetailKeys[key] {
			t.Fatalf("the detail response carries %q, which this Work Order did not name", key)
		}
	}

	// Detail: legacy. The key is absent, not empty and not the zero instant.
	w = call(h, http.MethodGet, "/v1/requirements/requirement-legacy", "", "owner")
	if w.Code != 200 {
		t.Fatalf("GET legacy detail status=%d body=%s", w.Code, w.Body.String())
	}
	if _, present := decodeBody(t, w.Body.Bytes())["captured_at"]; present {
		t.Fatalf("a legacy Requirement's detail carries captured_at: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "0001-01-01") {
		t.Fatalf("a legacy Requirement's detail carries the zero instant: %s", w.Body.String())
	}

	// List: one recorded and one legacy row in the same response.
	w = call(h, http.MethodGet, "/v1/requirements", "", "owner")
	if w.Code != 200 {
		t.Fatalf("GET /v1/requirements status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "0001-01-01") {
		t.Fatalf("the Requirement list carries the zero instant: %s", w.Body.String())
	}
	rows, ok := decodeBody(t, w.Body.Bytes())["requirements"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("requirements array = %v, want 2 rows: %s", rows, w.Body.String())
	}
	wantRowKeys := map[string]bool{"requirement_id": true, "status": true, "version": true, "increment_ids": true, "text": true, "requested_by": true, "repository_id": true, "captured_at": true}
	recorded, legacy := 0, 0
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("row is not an object: %v", raw)
		}
		for key := range row {
			if !wantRowKeys[key] {
				t.Fatalf("a list row carries %q, which this Work Order did not name", key)
			}
		}
		if _, present := row["captured_at"]; present {
			recorded++
		} else {
			legacy++
		}
	}
	if recorded != 1 || legacy != 1 {
		t.Fatalf("rows with captured_at = %d, without = %d, want 1 and 1", recorded, legacy)
	}

	// Role gating and status codes are unchanged on both routes.
	for _, path := range []string{"/v1/requirements", "/v1/requirements/" + id} {
		if got := call(h, http.MethodGet, path, "", "").Code; got != 401 {
			t.Fatalf("GET %s unauthenticated = %d, want 401", path, got)
		}
		if got := call(h, http.MethodGet, path, "", "runner").Code; got != 403 {
			t.Fatalf("GET %s as runner = %d, want 403", path, got)
		}
	}
	if got := call(h, http.MethodGet, "/v1/requirements/does-not-exist", "", "owner").Code; got != 404 {
		t.Fatalf("GET an unknown Requirement = %d, want 404", got)
	}
}

// TestCaptureRejectsACallerSuppliedCaptureTime is A4's transport half: the
// capture time is not caller-suppliable, and a body that tries is refused with
// 400 rather than silently ignored.
func TestCaptureRejectsACallerSuppliedCaptureTime(t *testing.T) {
	h, _ := captureTimeHandler(t)
	w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"capture-supplied","text":"x","captured_at":"2020-01-01T00:00:00Z"}`, "owner")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a body carrying captured_at = %d, want 400: %s", w.Code, w.Body.String())
	}
	// The refusal happened before anything was written.
	if got := call(h, http.MethodGet, "/v1/requirements", "", "owner"); !strings.Contains(got.Body.String(), `"requirements":[]`) {
		t.Fatalf("the refused capture wrote something: %s", got.Body.String())
	}
	// The same body without the field is accepted, so the 400 above is about
	// the field and not about the rest of the request.
	if w = call(h, http.MethodPost, "/v1/requirements", `{"request_id":"capture-supplied","text":"x"}`, "owner"); w.Code != http.StatusCreated {
		t.Fatalf("the same body without captured_at = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestOwnerConsoleExposesTheRequirementCaptureTime is A12.
func TestOwnerConsoleExposesTheRequirementCaptureTime(t *testing.T) {
	h, _ := captureTimeHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`id="captured-title"`, `id="captured-refresh"`, `id="captured-missing"`, `id="captured-rows"`, "V2-073 Requirement capture time"} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// Additive only: every pre-existing surface, including the sibling tasks'
	// sections, is still there.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, `id="repository-list"`, "Release evidence", `id="release-conditions"`, `id="backlog-rows"`, `id="runners-rows"`, `id="providers-rows"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	marker := strings.Index(js, "// V2-073 Requirement capture time")
	if marker < 0 {
		t.Fatal("owner.js does not carry the V2-073 marker comment")
	}
	block := js[marker:]
	for _, want := range []string{"/v1/requirements", "captured_at", "captured-rows", "captured-missing", "No capture time was recorded."} {
		if !strings.Contains(block, want) {
			t.Fatalf("the V2-073 owner.js block does not reference %q", want)
		}
	}
	// The sibling blocks are still present in the same single file.
	for _, want := range []string{"/v1/release/state", "/v1/runners", "/v1/providers", "executability", "requirement_backlog"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// No raw JSON, no timer, no external asset, script or font.
	for _, forbidden := range []string{"JSON.stringify", "http://", "https://", "//cdn", "importScripts", "setInterval", "setTimeout", "@font-face"} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("the V2-073 owner.js block references %q", forbidden)
		}
	}
	// No credential-shaped and no email-shaped value. The matcher is verified
	// first: the pre-existing repository-locator block does contain an '@', so
	// a scan over the whole file would not be evidence about this block.
	if !strings.Contains(js, `"@"`) {
		t.Fatal("the pre-existing owner.js locator block no longer contains an '@'; this scan's control is stale")
	}
	if strings.Contains(block, "@") {
		t.Fatal("the V2-073 owner.js block carries an '@'")
	}
	section := html[strings.Index(html, "V2-073 Requirement capture time"):]
	if strings.Contains(section, "@") {
		t.Fatal("the V2-073 owner console section carries an '@'")
	}
	lowered := strings.ToLower(section)
	for _, forbidden := range []string{"password", "secret", "api_key", "apikey", "bearer ", "authorization:", "private_key", "@example.", "@gmail.", "accounts.google.com"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the rendered capture-time section carries a credential- or email-shaped value %q", forbidden)
		}
	}
	// The absence of a capture time is shown as an absence, in words: not an
	// empty string and not the zero instant.
	if strings.Contains(html, "0001-01-01") || strings.Contains(js, "0001-01-01") {
		t.Fatal("the owner console renders the zero instant")
	}
	if !strings.Contains(section, "no capture time") {
		t.Fatal("the capture-time section does not name the absent case in plain words")
	}
}

// domainRequirementID and requirementWithNoCaptureTime build the legacy
// fixture the assertions above need. They live here rather than in a shared
// helper because internal/api is the only package in this task that needs a
// Requirement written behind the HTTP surface.
func domainRequirementID(id string) (domain.RequirementID, error) { return domain.NewRequirementID(id) }

func requirementWithNoCaptureTime(id domain.RequirementID) domain.Requirement {
	return domain.Requirement{ID: id, Status: domain.RequirementCaptured, Version: 1}
}

// ===========================================================================
// V2-068: the allocation surface, the import boundary and the untouched packages
// ===========================================================================

// --- the import boundary ----------------------------------------------------

// schedulerImportPathForAPI is assembled from internalPackagePrefix for the
// reason that variable's own comment records: a full module-path literal here
// would be read by ci/components.json's dependency derivation as an api ->
// scheduler edge, which is the very thing this guard asserts does not exist.
var schedulerImportPathForAPI = internalPackagePrefix + "scheduler"

// isSchedulerImport matches the exact path or that path followed by a slash.
// Prefix-without-slash matching would make "scheduler" forbid "schedulerless".
func isSchedulerImport(path string) bool {
	return path == schedulerImportPathForAPI || strings.HasPrefix(path, schedulerImportPathForAPI+"/")
}

// TestAPIDoesNotImportTheScheduler is V2-068 A3. internal/application imports
// internal/scheduler because the allocation report must be computed inside the
// transaction that reads the state it describes, and internal/api holds no
// UnitOfWork and cannot open one. The edge must therefore point into the
// scheduler through the application layer and nowhere else.
func TestAPIDoesNotImportTheScheduler(t *testing.T) {
	// The matcher is verified against a known-positive and a known-negative
	// before the scan trusts it.
	for _, positive := range []string{schedulerImportPathForAPI, schedulerImportPathForAPI + "/sub"} {
		if !isSchedulerImport(positive) {
			t.Fatalf("known-positive %q was not detected", positive)
		}
	}
	for _, negative := range []string{
		"net/http",
		internalPackagePrefix + "application",
		internalPackagePrefix + "domain",
		internalPackagePrefix + "schedulerless",
	} {
		if isSchedulerImport(negative) {
			t.Fatalf("known-negative %q was flagged", negative)
		}
	}
	// Positive control on the scan itself: a synthetic file that does import
	// the scheduler must be reported.
	src := "package api\n\nimport (\n\t\"net/http\"\n\t_ \"" + schedulerImportPathForAPI + "\"\n)\n\nvar _ = http.StatusOK\n"
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic_scheduler.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	flagged := false
	for _, path := range importPathsOf(file) {
		if isSchedulerImport(path) {
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("positive control: a synthetic file importing internal/scheduler was not flagged")
	}

	// apiPackageImports fails outright on a zero-file scan, so this cannot
	// pass vacuously.
	files := apiPackageImports(t)
	total := 0
	for name, paths := range files {
		total += len(paths)
		for _, path := range paths {
			if isSchedulerImport(path) {
				t.Errorf("%s imports %q; the scheduler is reached through internal/application only", name, path)
			}
		}
	}
	if total == 0 {
		t.Fatal("scanned zero import paths")
	}
	t.Logf("api scheduler-import guard scanned %d non-test files and %d import paths, and found no edge", len(files), total)
}

// TestTheUntouchablePackagesAreUntouched is V2-068 A7's first proof, and it
// lives here rather than in internal/application because V2-067's probe guard
// forbids os/exec in every file of that package, test files included. That
// guard is correct and is not weakened; this assertion was moved instead.
//
// The L3 permit closure cannot have moved if the files that hold it did not
// change, and git is the authority on that rather than a reading of a diff.
func TestTheUntouchablePackagesAreUntouched(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"internal/domain", "internal/scheduler", "internal/application/stop_matrix_test.go", "contracts/release-contract"} {
		// Both the working tree and the index are compared against HEAD, so a
		// change that was staged is caught as well as one that was not.
		for _, args := range [][]string{
			{"diff", "--stat", "HEAD", "--", dir},
			{"diff", "--stat", "--cached", "HEAD", "--", dir},
		} {
			out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
			if err != nil {
				t.Skipf("git %v over %s could not run (%v); recorded as skipped, never counted as a pass", args, dir, err)
			}
			if strings.TrimSpace(string(out)) != "" {
				t.Fatalf("%s changed (git %v):\n%s", dir, args, out)
			}
		}
		t.Logf("%s: zero changed files in the working tree and in the index", dir)
	}
}

// --- the route --------------------------------------------------------------

// TestQueueSummaryRouteAuthorizationIsUnchanged is V2-068 A15's transport half:
// the three authorization outcomes are exactly what they were before the three
// objects were added.
func TestQueueSummaryRouteAuthorizationIsUnchanged(t *testing.T) {
	h := testHandler(t)
	if w := call(h, http.MethodGet, "/v1/queue/summary", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(h, http.MethodGet, "/v1/queue/summary", "", "runner"); w.Code != http.StatusForbidden {
		t.Fatalf("runner status=%d body=%s", w.Code, w.Body.String())
	}
	w := call(h, http.MethodGet, "/v1/queue/summary", "", "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not json: %s", w.Body.String())
	}
	for _, want := range []string{"requirements", "by_requirement_status", "increments", "by_increment_status", "active_executions", "allocation", "waiting", "exhaustion"} {
		if _, ok := body[want]; !ok {
			t.Fatalf("the response has no %q key: %s", want, w.Body.String())
		}
	}
	if len(body) != 8 {
		t.Fatalf("the response has %d keys, want 8: %s", len(body), w.Body.String())
	}
	// No placeholder the Snapshot modelling needed may reach the wire. The scan
	// is verified first against a body that does contain one.
	raw := w.Body.String()
	placeholders := application.AllocationSnapshotPlaceholders()
	if len(placeholders) == 0 {
		t.Fatal("the placeholder list is empty; the scan would pass vacuously")
	}
	for _, placeholder := range placeholders {
		if placeholder == "" {
			t.Fatal("a placeholder is the empty string; the scan would match everything")
		}
		if !strings.Contains(`{"x":"`+placeholder+`"}`, placeholder) {
			t.Fatalf("positive control failed for %q", placeholder)
		}
		if strings.Contains(raw, placeholder) {
			t.Fatalf("the response carries the modelling placeholder %q: %s", placeholder, raw)
		}
	}
	t.Logf("401/403/200 unchanged; none of the %d modelling placeholders reaches the response", len(placeholders))
}

// TestControlAcceptsTheOptionalLimitAndStillRejectsUnknownFields is the rest of
// A15's request half.
func TestControlAcceptsTheOptionalLimitAndStillRejectsUnknownFields(t *testing.T) {
	h := testHandler(t)
	// The field is optional: a body without it still succeeds.
	w := call(h, http.MethodPost, "/v1/controls", `{"request_id":"c1","scope_kind":"installation","scope_value":"install","mode":"allow"}`, "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("control without the limit: status=%d body=%s", w.Code, w.Body.String())
	}
	// And with it.
	w = call(h, http.MethodPost, "/v1/controls", `{"request_id":"c2","scope_kind":"installation","scope_value":"install","mode":"allow","allocation_limit":{"installation_concurrent_executions":6}}`, "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("control with the limit: status=%d body=%s", w.Code, w.Body.String())
	}
	// Out of range is a 400 and the response body is an error, not a revision.
	for _, bad := range []string{"0", "-1", "21"} {
		w = call(h, http.MethodPost, "/v1/controls", `{"request_id":"c-bad-`+bad+`","scope_kind":"installation","scope_value":"install","mode":"allow","allocation_limit":{"installation_concurrent_executions":`+bad+`}}`, "owner")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("limit %s: status=%d body=%s", bad, w.Code, w.Body.String())
		}
	}
	// An unknown field inside the new object is still rejected.
	w = call(h, http.MethodPost, "/v1/controls", `{"request_id":"c3","scope_kind":"installation","scope_value":"install","mode":"allow","allocation_limit":{"installation_concurrent_executions":6,"per_repository":2}}`, "owner")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field inside allocation_limit: status=%d body=%s", w.Code, w.Body.String())
	}
	// And at the top level.
	w = call(h, http.MethodPost, "/v1/controls", `{"request_id":"c4","scope_kind":"installation","scope_value":"install","mode":"allow","allocation":{"limit":6}}`, "owner")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown top-level field: status=%d body=%s", w.Code, w.Body.String())
	}
	// A different POST body's unknown-field refusal is unchanged too.
	w = call(h, http.MethodPost, "/v1/requirements", `{"request_id":"r1","text":"x","allocation_limit":{"installation_concurrent_executions":6}}`, "owner")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field on the capture body: status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestOpenAPIDeclaresTheAllocationSurfaceExactly is A15's contract half. It
// compares the openapi document's required lists against the Go structs' own
// json tags, so the two cannot drift.
func TestOpenAPIDeclaresTheAllocationSurfaceExactly(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "openapi", "openapi-v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document := string(data)
	for _, want := range []string{
		"QueueSummaryResponse:",
		"QueueAllocation:",
		"QueueWaiting:",
		"QueueExhaustion:",
		"AllocationLimitRequest:",
		"$ref: '#/components/schemas/QueueSummaryResponse'",
		"allocation_limit: {$ref: '#/components/schemas/AllocationLimitRequest'}",
	} {
		if !strings.Contains(document, want) {
			t.Fatalf("the openapi document does not declare %q", want)
		}
	}
	// additionalProperties:false on every new object.
	for _, schema := range []string{"QueueSummaryResponse", "QueueAllocation", "QueueWaiting", "QueueExhaustion", "AllocationLimitRequest"} {
		block := openAPISchemaBlock(t, document, schema)
		if !strings.Contains(block, "additionalProperties: false") {
			t.Fatalf("schema %s does not declare additionalProperties: false:\n%s", schema, block)
		}
	}
	// The five pre-existing QueueSummary fields still carry their own names in
	// the shared schema, which GET /v1/repositories/{id} still points at.
	shared := openAPISchemaBlock(t, document, "QueueSummary")
	for _, want := range []string{"requirements", "by_requirement_status", "increments", "by_increment_status", "active_executions"} {
		if !strings.Contains(shared, want) {
			t.Fatalf("the shared QueueSummary schema lost %q", want)
		}
	}
	if strings.Contains(shared, "allocation") {
		t.Fatalf("the shared QueueSummary schema gained an allocation field; installation_scope cannot populate it:\n%s", shared)
	}

	// Every json tag of the response struct appears in the response schema's
	// required list, and nothing else does.
	declared := openAPIRequiredList(t, document, "QueueSummaryResponse")
	actual := jsonTagsOf(t, application.QueueSummaryResponse{})
	sort.Strings(declared)
	sort.Strings(actual)
	if !reflect.DeepEqual(declared, actual) {
		t.Fatalf("QueueSummaryResponse required %v, struct emits %v", declared, actual)
	}
	for _, pair := range []struct {
		schema string
		value  any
	}{
		{"QueueAllocation", application.AllocationView{}},
		{"QueueExhaustion", application.ExhaustionView{}},
		{"QueueWaiting", application.WaitingView{}},
		{"AllocationLimitRequest", application.AllocationLimitInput{}},
	} {
		want := openAPIRequiredList(t, document, pair.schema)
		got := jsonTagsOf(t, pair.value)
		sort.Strings(want)
		sort.Strings(got)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s required %v, struct emits %v", pair.schema, want, got)
		}
	}
	// The waiting buckets published in the contract are exactly the ones the
	// code reports, so the document cannot promise a reason the scheduler does
	// not give.
	waiting := openAPISchemaBlock(t, document, "QueueWaiting")
	for _, bucket := range application.WaitingReasonBuckets() {
		if !strings.Contains(waiting, bucket+": {type: integer") {
			t.Fatalf("the QueueWaiting schema does not declare the bucket %q:\n%s", bucket, waiting)
		}
	}
	t.Logf("openapi declares the allocation surface with %d waiting buckets and additionalProperties:false throughout", len(application.WaitingReasonBuckets()))
}

// openAPISchemaBlock returns the text of one schema declaration, from its name
// to the next schema at the same indentation.
func openAPISchemaBlock(t *testing.T, document, name string) string {
	t.Helper()
	start := strings.Index(document, "\n    "+name+":\n")
	if start < 0 {
		t.Fatalf("schema %s is not declared", name)
	}
	rest := document[start+1:]
	lines := strings.Split(rest, "\n")
	out := []string{lines[0]}
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "     ") && strings.HasSuffix(strings.TrimSpace(l), ":") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// openAPIRequiredList extracts the required list of one schema.
func openAPIRequiredList(t *testing.T, document, name string) []string {
	t.Helper()
	block := openAPISchemaBlock(t, document, name)
	marker := strings.Index(block, "required: [")
	if marker < 0 {
		t.Fatalf("schema %s declares no required list:\n%s", name, block)
	}
	rest := block[marker+len("required: ["):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatalf("schema %s has an unterminated required list", name)
	}
	out := []string{}
	for _, raw := range strings.Split(rest[:end], ",") {
		if v := strings.TrimSpace(raw); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		t.Fatalf("schema %s has an empty required list", name)
	}
	return out
}

// jsonTagsOf returns the json field names a struct actually marshals,
// flattening one level of embedding exactly as encoding/json does.
func jsonTagsOf(t *testing.T, value any) []string {
	t.Helper()
	out := []string{}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if field.Anonymous && field.Type.Kind() == reflect.Struct && field.Tag.Get("json") == "" {
				walk(field.Type)
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			out = append(out, tag)
		}
	}
	walk(reflect.TypeOf(value))
	if len(out) == 0 {
		t.Fatalf("no json tags found on %T; the walk is broken", value)
	}
	return out
}

// --- the owner console ------------------------------------------------------

// TestOwnerConsoleExposesTheAllocationSurface is V2-068 A16.
//
// RECORDED DEVIATION FROM A16's WORDING. A16 asks for the numeric input "on the
// control form". The pre-existing Control form's submit handler is part of the
// first, single-line block of owner.js, and adding a field to that form without
// rewriting that handler would render an input the page never sends. Every block
// in owner.js and owner.html is appended and self-contained precisely so that no
// task rewrites another's, so this task's block carries its own control form,
// posting to the same POST /v1/controls with the same seven modes. The
// pre-existing form is unchanged and is asserted below to still submit.
func TestOwnerConsoleExposesTheAllocationSurface(t *testing.T) {
	h := testHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{
		`id="allocation-title"`, `id="allocation-form"`, `id="allocation-limit"`, `id="allocation-refresh"`,
		`id="allocation-limit-line"`, `id="allocation-active"`, `id="allocation-exhaustion"`, `id="allocation-waiting-rows"`,
		"V2-068 shared resource allocation",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// The limit input is a bounded numeric input, not a free-text field.
	section := html[strings.Index(html, "V2-068 shared resource allocation"):]
	for _, want := range []string{`type="number"`, `min="1"`, `max="20"`} {
		if !strings.Contains(section, want) {
			t.Fatalf("the allocation section's limit input does not declare %s", want)
		}
	}
	// Additive only: every pre-existing surface, including the sibling tasks'
	// sections, is still there.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, `id="repository-list"`, "Release evidence", `id="release-conditions"`, `id="backlog-rows"`, `id="runners-rows"`, `id="providers-rows"`, `id="captured-rows"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}

	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	marker := strings.Index(js, "// V2-068 shared resource allocation")
	if marker < 0 {
		t.Fatal("owner.js does not carry the V2-068 marker comment")
	}
	block := js[marker:]
	for _, want := range []string{"/v1/queue/summary", "/v1/controls", "allocation_limit", "installation_concurrent_executions", "by_reason", "binding_limit", "planned_assignments", "limit_source", "allocation-waiting-rows"} {
		if !strings.Contains(block, want) {
			t.Fatalf("the V2-068 owner.js block does not reference %q", want)
		}
	}
	// The reader can see WHY work is waiting without parsing raw JSON: every
	// scheduler reason has words of its own in the block.
	for _, reason := range application.WaitingReasonBuckets() {
		if !strings.Contains(block, `"`+reason+`"`) {
			t.Fatalf("the V2-068 owner.js block has no words for the waiting reason %q", reason)
		}
	}
	// The sibling blocks are still present in the same single file.
	for _, want := range []string{"/v1/release/state", "/v1/runners", "/v1/providers", "executability", "requirement_backlog", "captured_at"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// No raw JSON rendering, no timer, no external asset, script or font.
	for _, forbidden := range []string{"JSON.stringify", "http://", "https://", "//cdn", "importScripts", "setInterval", "setTimeout", "@font-face", "@"} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("the V2-068 owner.js block references %q", forbidden)
		}
	}
	if strings.Contains(section, "@") {
		t.Fatal("the V2-068 owner console section carries an '@'")
	}
	lowered := strings.ToLower(section)
	for _, forbidden := range []string{"password", "secret", "api_key", "apikey", "bearer ", "authorization:", "private_key", "@example.", "@gmail.", "accounts.google.com"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the rendered allocation section carries a credential- or email-shaped value %q", forbidden)
		}
	}
	// The section says the two things a reader must not get wrong.
	for _, want := range []string{"not a control mode", "revokes nothing"} {
		if !strings.Contains(lowered, strings.ToLower(want)) {
			t.Fatalf("the allocation section does not say %q in plain words", want)
		}
	}
}

// TestBothControlFormsStillSubmitWithTheLimitFieldEmpty is the rest of A16: the
// pre-existing control form is untouched and still works, and this task's own
// form succeeds with the limit field left empty -- which is the case that must
// send no allocation_limit key at all.
func TestBothControlFormsStillSubmitWithTheLimitFieldEmpty(t *testing.T) {
	h := testHandler(t)
	// What the pre-existing form sends, byte for byte in shape: no
	// allocation_limit key.
	w := call(h, http.MethodPost, "/v1/controls", `{"request_id":"legacy-form","scope_kind":"installation","scope_value":"install","mode":"allow"}`, "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("the pre-existing control form no longer submits: status=%d body=%s", w.Code, w.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first["revision"] == nil {
		t.Fatalf("the pre-existing form's response carries no revision: %s", w.Body.String())
	}
	// What this task's form sends with the field left empty: identical shape.
	w = call(h, http.MethodPost, "/v1/controls", `{"request_id":"allocation-form-empty","scope_kind":"installation","scope_value":"install","mode":"pause-claim"}`, "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("the allocation form with an empty limit field: status=%d body=%s", w.Code, w.Body.String())
	}
	// And the effective limit is untouched by that revision, so an empty field
	// really changed nothing.
	w = call(h, http.MethodGet, "/v1/queue/summary", "", "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("queue summary status=%d", w.Code)
	}
	var summary struct {
		Allocation struct {
			Limit       int    `json:"limit"`
			LimitSource string `json:"limit_source"`
		} `json:"allocation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Allocation.LimitSource != "architecture-design-ceiling" || summary.Allocation.Limit != application.AllocationLimitCeiling {
		t.Fatalf("two control revisions with no limit field changed the effective limit: %#v", summary.Allocation)
	}
	// The owner.js block sends the key only when the field is non-empty, and
	// that is visible in the block itself rather than only in behaviour.
	js := call(h, http.MethodGet, "/owner/assets/owner.js", "", "").Body.String()
	block := js[strings.Index(js, "// V2-068 shared resource allocation"):]
	if !strings.Contains(block, `if(raw!==""){`) {
		t.Fatal("the V2-068 owner.js block does not guard the allocation_limit key on a non-empty field")
	}
}

// ===========================================================================
// V2-065: the needs-input verbs at the transport boundary
// ===========================================================================
//
// Additive block appended at the end of this file. Nothing above it was
// rewritten.

// needsInputHandler builds a handler plus the store behind it, so a
// Requirement can be seeded into a status the needs-input transition is legal
// from. No application command can reach framing, active or evaluating (see
// internal/application/human_input_test.go), and there is deliberately no
// route that does either, so the seed goes through the store.
func needsInputHandler(t *testing.T) (http.Handler, *memory.Store, *application.Service) {
	t.Helper()
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{
		"owner":     {Role: application.RoleOwner, Subject: "owner"},
		"runner":    {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"},
		"scheduler": {Role: application.RoleScheduler, Subject: "scheduler.self"},
	}
	return api.New(api.Config{Authenticator: auth, Service: svc, AllowedOrigins: []string{"https://console.example"}}), st, svc
}

// seedActiveRequirement captures a Requirement through the service and moves
// it to active through the store, returning its id and current version.
func seedActiveRequirement(t *testing.T, st *memory.Store, svc *application.Service, tag string) (string, domain.Version) {
	t.Helper()
	ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
	out, err := svc.Capture(ctx, application.CaptureRequest{RequestID: tag + ":capture", Text: "needs a decision"})
	if err != nil {
		t.Fatal(err)
	}
	var version domain.Version
	if err = st.Transact(context.Background(), func(u application.UnitOfWork) error {
		r, ok, e := u.Requirement(context.Background(), out.RequirementID)
		if e != nil || !ok {
			t.Fatalf("seed: ok=%v err=%v", ok, e)
		}
		next := r
		next.Status = domain.RequirementActive
		next.Version++
		version = next.Version
		return u.SaveRequirement(context.Background(), next, r.Version)
	}); err != nil {
		t.Fatal(err)
	}
	return out.RequirementID, version
}

const needsInputAskBody = `{"request_id":"REQUEST_ID","expected_requirement_version":VERSION,"question":"Delete the branch or keep it?","reason_class":"destructive-irreversible","reason":"Both choices lose something the Loop may not lose.","options":[{"option_id":"delete","summary":"Delete","impact":"The commits stop being reachable."},{"option_id":"keep","summary":"Keep","impact":"The Increment stays blocked."}],"stopped_scope":["new-claims-for-this-requirement","lease-renewal-for-this-requirement"],"continuing_scope":["other-requirements","owner-reads"]}`

// askBody fills the ask template. strconv and strings are already imported by
// this file; fmt deliberately is not, so this block adds no import and cannot
// conflict with a sibling task editing the same import block.
func askBody(requestID string, version domain.Version) string {
	body := strings.Replace(needsInputAskBody, "REQUEST_ID", requestID, 1)
	return strings.Replace(body, "VERSION", strconv.FormatInt(int64(version), 10), 1)
}

func answerBodyJSON(requestID string, version domain.Version, optionID string) string {
	return `{"request_id":"` + requestID + `","expected_requirement_version":` + strconv.FormatInt(int64(version), 10) + `,"option_id":"` + optionID + `"}`
}

// TestNeedsInputRoutesAreRoleGated is V2-065 A9.
func TestNeedsInputRoutesAreRoleGated(t *testing.T) {
	h, st, svc := needsInputHandler(t)
	id, version := seedActiveRequirement(t, st, svc, "route")
	askPath := "/v1/requirements/" + id + ":request-input"
	answerPath := "/v1/requirements/" + id + ":answer-input"
	ask := func(requestID, token string, v domain.Version) *httptest.ResponseRecorder {
		return call(h, http.MethodPost, askPath, askBody(requestID, v), token)
	}

	// The ask: 401 unauthenticated, 403 as the owner, accepted for a Runner.
	if w := ask("r-unauth", "", version); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ask status=%d body=%s", w.Code, w.Body.String())
	}
	if w := ask("r-owner", "owner", version); w.Code != http.StatusForbidden {
		t.Fatalf("owner ask status=%d body=%s; the owner answers questions rather than asking them", w.Code, w.Body.String())
	}
	// The answer: 401 unauthenticated, 403 as a Runner and as the scheduler.
	answerBody := `{"request_id":"a-1","expected_requirement_version":1,"option_id":"keep"}`
	if w := call(h, http.MethodPost, answerPath, answerBody, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated answer status=%d", w.Code)
	}
	for _, token := range []string{"runner", "scheduler"} {
		if w := call(h, http.MethodPost, answerPath, answerBody, token); w.Code != http.StatusForbidden {
			t.Fatalf("%s answer status=%d body=%s", token, w.Code, w.Body.String())
		}
	}
	// Nothing above changed the Requirement.
	if r, _ := st.Requirement(id); r.Status != domain.RequirementActive || r.Version != version {
		t.Fatalf("a refused request changed the Requirement: %+v", r)
	}

	// The scheduler may ask, on its own Requirement.
	other, otherVersion := seedActiveRequirement(t, st, svc, "route-scheduler")
	if w := call(h, http.MethodPost, "/v1/requirements/"+other+":request-input", askBody("r-sched", otherVersion), "scheduler"); w.Code != http.StatusOK {
		t.Fatalf("scheduler ask status=%d body=%s", w.Code, w.Body.String())
	}

	// The Runner may ask, and the response carries no key this task did not
	// name.
	w := ask("r-runner", "runner", version)
	if w.Code != http.StatusOK {
		t.Fatalf("runner ask status=%d body=%s", w.Code, w.Body.String())
	}
	askDoc := decodeBody(t, w.Body.Bytes())
	askKeys := map[string]bool{"requirement_id": true, "status": true, "version": true, "asked_at": true, "asked_by": true}
	for key := range askDoc {
		if !askKeys[key] {
			t.Fatalf("the ask response carries an unnamed field %q", key)
		}
	}
	if askDoc["status"] != "needs-input" {
		t.Fatalf("ask response status = %v", askDoc["status"])
	}

	// The recorded question is on the existing owner detail route, and only
	// the owner may read it.
	if w = call(h, http.MethodGet, "/v1/requirements/"+id, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated detail status=%d", w.Code)
	}
	if w = call(h, http.MethodGet, "/v1/requirements/"+id, "", "runner"); w.Code != http.StatusForbidden {
		t.Fatalf("runner detail status=%d", w.Code)
	}
	w = call(h, http.MethodGet, "/v1/requirements/"+id, "", "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("owner detail status=%d body=%s", w.Code, w.Body.String())
	}
	detail := decodeBody(t, w.Body.Bytes())
	question, ok := detail["needs_input"].(map[string]any)
	if !ok {
		t.Fatalf("the detail carries no needs_input object: %s", w.Body.String())
	}
	for _, want := range []string{"question", "reason_class", "reason", "options", "stopped_scope", "continuing_scope", "asked_at", "asked_by"} {
		if question[want] == nil {
			t.Fatalf("needs_input is missing %q: %s", want, w.Body.String())
		}
	}
	for _, absent := range []string{"answered_at", "answered_option_id", "answered_by"} {
		if _, present := question[absent]; present {
			t.Fatalf("an unanswered question reports %q: %s", absent, w.Body.String())
		}
	}
	// No field name in the response carries a credential-shaped or
	// provider-output-shaped name.
	raw := strings.ToLower(w.Body.String())
	for _, forbidden := range []string{"password", "credential", "raw_prompt", "raw_provider_output"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("the detail response carries %q", forbidden)
		}
	}

	// The owner answers, and the SAME Requirement resumes.
	askDocVersion := domain.Version(askDoc["version"].(float64))
	w = call(h, http.MethodPost, answerPath, answerBodyJSON("a-2", askDocVersion, "keep"), "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("owner answer status=%d body=%s", w.Code, w.Body.String())
	}
	answerDoc := decodeBody(t, w.Body.Bytes())
	answerKeys := map[string]bool{"requirement_id": true, "status": true, "version": true, "answered_option_id": true, "answered_at": true, "answered_by": true}
	for key := range answerDoc {
		if !answerKeys[key] {
			t.Fatalf("the answer response carries an unnamed field %q", key)
		}
	}
	if answerDoc["requirement_id"] != id || answerDoc["status"] != "ready" {
		t.Fatalf("the answer did not resume the same Requirement: %s", w.Body.String())
	}
	// An unknown option is a refusal, not a fallback, and there is no field
	// that could carry a default.
	w = call(h, http.MethodPost, answerPath, `{"request_id":"a-3","expected_requirement_version":9,"option_id":"whatever"}`, "owner")
	if w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
		t.Fatalf("an unknown option status=%d body=%s", w.Code, w.Body.String())
	}
	w = call(h, http.MethodPost, answerPath, `{"request_id":"a-4","expected_requirement_version":1,"option_id":"keep","default_option":"keep"}`, "owner")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a default_option field status=%d body=%s; the decoder must refuse it", w.Code, w.Body.String())
	}
}

// TestNeedsInputPrefixesAreNotSwallowedByTheRequirementGetBranch is A9's
// routing clause. The GET /v1/requirements/ prefix branch is gated on the GET
// method, so a POST to either verb reaches its own handler; a POST to the bare
// detail path is a 404 rather than being routed to one of the verbs.
func TestNeedsInputPrefixesAreNotSwallowedByTheRequirementGetBranch(t *testing.T) {
	h, st, svc := needsInputHandler(t)
	id, version := seedActiveRequirement(t, st, svc, "swallow")
	if w := call(h, http.MethodPost, "/v1/requirements/"+id, `{"request_id":"x"}`, "runner"); w.Code != http.StatusNotFound {
		t.Fatalf("POST on the bare detail path status=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(h, http.MethodPost, "/v1/requirements/:request-input", askBody("empty-id", version), "runner"); w.Code != http.StatusNotFound {
		t.Fatalf("an empty requirement id status=%d body=%s", w.Code, w.Body.String())
	}
	// The POST verb reaches the command rather than the GET branch: the proof
	// is that it changes state.
	if w := call(h, http.MethodPost, "/v1/requirements/"+id+":request-input", askBody("reaches", version), "runner"); w.Code != http.StatusOK {
		t.Fatalf("the ask did not reach its handler: status=%d body=%s", w.Code, w.Body.String())
	}
	if r, _ := st.Requirement(id); r.Status != domain.RequirementNeedsInput {
		t.Fatalf("the POST did not reach the command; requirement is %q", r.Status)
	}
	// Both new paths are declared in the OpenAPI document, and the read
	// surface the capability declares was already there.
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "openapi", "openapi-v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  /v1/requirements/{requirement_id}:request-input:",
		"  /v1/requirements/{requirement_id}:answer-input:",
		"  /v1/requirements/{requirement_id}:",
		"NeedsInputQuestion:",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("openapi-v1.yaml does not declare %q", want)
		}
	}
}

// TestOwnerConsoleExposesTheNeedsInputQuestion is A11.
func TestOwnerConsoleExposesTheNeedsInputQuestion(t *testing.T) {
	h, _, _ := needsInputHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`id="needs-input-title"`, `id="needs-input"`, `id="needs-input-requirement"`, `id="needs-input-question"`, `id="needs-input-reason"`, `id="needs-input-options"`, `id="needs-input-stopped"`, `id="needs-input-continuing"`, `id="needs-input-submit"`, `id="needs-input-answer-state"`, "V2-065 needs-input question"} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// Additive only: every sibling task's section is still there.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, "Release evidence", `id="release-conditions"`, `id="backlog-rows"`, `id="runners-rows"`, `id="providers-waiting"`, `id="captured-rows"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	marker := strings.Index(js, "// V2-065 needs-input question")
	if marker < 0 {
		t.Fatal("owner.js does not carry the V2-065 marker comment")
	}
	block := js[marker:]
	for _, want := range []string{":answer-input", "needs_input", "reason_class", "stopped_scope", "continuing_scope", "impact", "no question is recorded", "Nothing is submitted until"} {
		if !strings.Contains(block, want) && !strings.Contains(html, want) {
			t.Fatalf("the V2-065 block does not reference %q", want)
		}
	}
	// The sibling blocks must still be present in the same single file.
	for _, want := range []string{"/v1/release/state", "executability", "requirement_backlog", "/v1/runners", "/v1/providers", "captured-rows"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// The block renders named rows, not raw JSON, and adds no timer and no
	// external asset.
	if strings.Contains(block, "JSON.stringify") {
		t.Fatal("the V2-065 owner.js block renders raw JSON")
	}
	for _, forbidden := range []string{"http://", "https://", "//cdn", "importScripts", "setInterval", "setTimeout", "@font-face"} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("the V2-065 owner.js block references %q", forbidden)
		}
	}
	// Nothing submits an answer without an explicit owner action: the only
	// call to the answer route is inside the function bound to the submit
	// button's onclick, and the button starts disabled.
	if !strings.Contains(html, `id="needs-input-submit" type="button" disabled`) {
		t.Fatal("the submit button is not disabled until an option is selected")
	}
	if !strings.Contains(block, "submit.onclick=answer") {
		t.Fatal("the answer is not bound to an explicit owner action")
	}
	// No credential-shaped and no email-shaped value in the rendered markup.
	// The whole-document list is the one the sibling tasks' checks already
	// hold; "credential" is checked against this task's own section instead,
	// because the pre-existing page header says in prose that credentials are
	// never rendered, and that sentence must not be deleted to satisfy a
	// substring check.
	lowered := strings.ToLower(html)
	for _, forbidden := range []string{"password", "secret", "api_key", "apikey", "bearer ", "authorization:", "private_key"} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the rendered owner console carries %q", forbidden)
		}
	}
	sectionStart := strings.Index(html, "V2-065 needs-input question")
	sectionEnd := strings.Index(html, "/V2-065 needs-input question")
	if sectionStart < 0 || sectionEnd <= sectionStart {
		t.Fatal("the V2-065 section markers are not both present in owner.html")
	}
	section := strings.ToLower(html[sectionStart:sectionEnd])
	for _, forbidden := range []string{"credential", "raw_prompt", "raw_provider_output", "password", "secret"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("the V2-065 owner-console section carries %q", forbidden)
		}
	}
	if strings.Contains(strings.ToLower(block), "credential") || strings.Contains(strings.ToLower(block), "raw_provider_output") {
		t.Fatal("the V2-065 owner.js block references a credential or raw provider output")
	}
	if strings.Contains(html, "@") {
		t.Fatalf("the rendered owner console contains an at-sign, which is the email shape this check refuses")
	}
	// The console must not claim a measurement it does not have.
	for _, forbidden := range []string{"capability exercised", "preview journey passed"} {
		if strings.Contains(lowered, forbidden) || strings.Contains(strings.ToLower(js), forbidden) {
			t.Fatalf("owner console claims %q", forbidden)
		}
	}
}

// ===========================================================================
// V2-074 A4 and A15: no admission path reads a compatibility type, and the
// owner console renders both relations with an explicit unknown.
// ===========================================================================

// TestNoRequestAdmissionPathReadsACompatibilityType is A4's mechanical half at
// the transport boundary, modelled on the equivalent V2-069 scan above: the
// authentication boundary and the request-admission surface mention no type
// this task declares. The matcher is verified against a synthetic
// known-positive first, and a zero-declaration scan fails outright.
func TestNoRequestAdmissionPathReadsACompatibilityType(t *testing.T) {
	identifiers := []string{
		"ProviderCompatibilityView", "ProviderCompatibilityVerdict", "ProviderVersionIntervalView",
		"ProviderHandoffView", "ProviderHandoffDisposition", "ProviderHandoffWaitingReason",
		"ProviderCandidateObstacle", "ProviderSourceState", "ProviderSelectionCell",
		"ProviderCLIVersionInterval", "ProviderLoopVersionInterval", "ProviderVersionVerdict",
		"ProviderHandoffDecisionTable", "ProviderSourceStateFor", "ProviderCandidateObstacleFor",
	}
	hits := func(n ast.Node) []string {
		found := []string{}
		ast.Inspect(n, func(node ast.Node) bool {
			ident, isIdent := node.(*ast.Ident)
			if !isIdent {
				return true
			}
			for _, name := range identifiers {
				if ident.Name == name {
					found = append(found, name)
				}
			}
			return true
		})
		return found
	}
	positive := "package api\n\nfunc (a CombinedAuthenticator) Authenticate() { var v application.ProviderCompatibilityView; _ = v }\n"
	synthetic, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits(synthetic)) == 0 {
		t.Fatal("positive control: a synthetic authenticator naming a compatibility type was not flagged")
	}

	declarations := 0
	names := map[string]bool{}
	for _, path := range []string{"auth.go", "api.go"} {
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.AllErrors)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc {
				continue
			}
			declarations++
			names[fn.Name.Name] = true
			if found := hits(fn); len(found) != 0 {
				t.Fatalf("%s: %s names %v; no request-admission path may read a type this task declares", path, fn.Name.Name, found)
			}
		}
	}
	if declarations == 0 {
		t.Fatal("scanned zero function declarations; the scan is broken")
	}
	if !names["Authenticate"] {
		t.Fatal("auth.go declares no Authenticate method; the boundary moved and this guard is vacuous")
	}
	t.Logf("scanned auth.go and api.go: %d function declarations, none naming any of the %d types V2-074 declares", declarations, len(identifiers))
}

// TestARequestForADeclaredIncompatibleProviderStillSucceedsEverywhereItDidBefore
// is A4's behavioural half. The scripted runner sequence below is the same one
// the pre-existing tests drive, run against a Provider whose observed failure
// class has already made cli_compatibility incompatible: no route changes its
// status code, because compatibility is a REPORT and not an admission rule.
func TestARequestForADeclaredIncompatibleProviderStillSucceedsEverywhereItDidBefore(t *testing.T) {
	h, _ := runnerVersionHandler(t)
	// Drive the Provider into the incompatible state through the route that
	// carries the class end to end, then assert every route still answers as
	// before for that same Provider.
	capture := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"compat-r-1","text":"a requirement whose provider is declared incompatible"}`, "owner")
	if capture.Code != http.StatusOK && capture.Code != http.StatusCreated {
		t.Fatalf("capture status=%d body=%s", capture.Code, capture.Body.String())
	}
	before, beforeRows, _ := providerRegistryDoc(t, h)
	if len(beforeRows) != 3 {
		t.Fatalf("baseline registry has %d rows", len(beforeRows))
	}
	_ = before

	// The one path that can move the CLI verdict: an execution result carrying
	// the contract-incompatible class. It is driven exactly as the pre-existing
	// observation test drives it, and its status code is asserted unchanged.
	type step struct {
		name, method, path, body, role string
	}
	steps := []step{
		{name: "providers-read-as-owner", method: http.MethodGet, path: "/v1/providers", role: "owner"},
		{name: "providers-read-as-runner", method: http.MethodGet, path: "/v1/providers", role: "runner"},
		{name: "providers-read-unauthenticated", method: http.MethodGet, path: "/v1/providers", role: ""},
		{name: "providers-post", method: http.MethodPost, path: "/v1/providers", body: `{}`, role: "owner"},
		{name: "owner-console", method: http.MethodGet, path: "/owner/", role: "owner"},
		{name: "owner-asset", method: http.MethodGet, path: "/owner/assets/owner.js", role: ""},
		{name: "healthz", method: http.MethodGet, path: "/healthz", role: ""},
	}
	baseline := map[string]int{}
	for _, s := range steps {
		w := call(h, s.method, s.path, s.body, s.role)
		baseline[s.name] = w.Code
	}

	// Now make claude declared-incompatible through the real result path.
	observed := call(h, http.MethodPost, "/v1/executions/e-compat:result",
		`{"request_id":"compat-res-1","execution_id":"e-compat","lease_id":"l-1","expected_execution_version":1,"fencing_token":1,"control_revision":0,"succeeded":false,"provider_observation":{"name":"claude","failure_class":"contract-incompatible"}}`,
		"runner")
	// Whatever this returns -- the Execution may not exist in this fixture --
	// it must not be a server error, and it must not change any other route.
	if observed.Code >= 500 {
		t.Fatalf("the observation path returned %d: %s", observed.Code, observed.Body.String())
	}
	for _, s := range steps {
		w := call(h, s.method, s.path, s.body, s.role)
		if w.Code != baseline[s.name] {
			t.Fatalf("%s changed from %d to %d once a Provider was declared incompatible; compatibility is a report, never an admission rule", s.name, baseline[s.name], w.Code)
		}
	}
	// And the registry still answers 200 with exactly three rows and the same
	// closed field set.
	_, afterRows, raw := providerRegistryDoc(t, h)
	if len(afterRows) != 3 {
		t.Fatalf("the registry reports %d rows after the observation", len(afterRows))
	}
	if strings.Contains(raw, "TODO") || strings.Contains(raw, "placeholder") {
		t.Fatalf("the response carries a placeholder: %s", raw)
	}
	t.Logf("%d routes kept their exact status codes across a Provider becoming declared-incompatible: %v", len(steps), baseline)
}

// TestOwnerConsoleReportsBothRelationsAndTheDisposition is A15.
func TestOwnerConsoleReportsBothRelationsAndTheDisposition(t *testing.T) {
	h, _ := runnerVersionHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{
		`id="provider-handoff-title"`, `id="provider-handoff-refresh"`, `id="provider-handoff-rows"`,
		`id="provider-handoff-waiting"`, `id="provider-handoff-proposed"`, `id="provider-handoff-state"`,
		"V2-074 provider compatibility and handoff",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// Additive only: every pre-existing surface, including the sibling tasks'
	// sections, is still there.
	for _, want := range []string{
		`id="capture"`, `id="control"`, `id="queue"`, `id="repository"`, `id="repository-list"`,
		"Release evidence", `id="release-conditions"`, `id="backlog-rows"`, `id="runners-rows"`,
		`id="providers-rows"`, `id="captured-rows"`, `id="needs-input"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}

	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	marker := strings.Index(js, "// V2-074 provider compatibility and handoff")
	if marker < 0 {
		t.Fatal("owner.js does not carry the V2-074 marker comment")
	}
	block := js[marker:]
	// The block reads the existing registry and renders each declared
	// observable of this task in words.
	for _, want := range []string{
		"/v1/providers", "cli_version_interval", "loop_version_interval", "observed_loop_version",
		"cli_compatibility", "loop_compatibility", "disposition", "waiting_reason", "target",
		"exhausted", "provider-handoff-waiting", "provider-handoff-proposed",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("the V2-074 owner.js block does not reference %q", want)
		}
	}
	// unknown is shown as unknown, and never as compatible or blank.
	if !strings.Contains(block, `"unknown":"unknown, because an input was absent"`) {
		t.Fatal("the block does not render unknown as unknown in plain words")
	}
	// Every declared waiting reason has words of its own in the block, so a
	// reader never has to parse an enum value.
	for _, reason := range application.ProviderHandoffWaitingReasons() {
		if !strings.Contains(block, `"`+string(reason)+`"`) {
			t.Fatalf("the V2-074 owner.js block has no words for the waiting reason %q", reason)
		}
	}
	for _, disposition := range application.ProviderHandoffDispositions() {
		if !strings.Contains(block, `"`+string(disposition)+`"`) {
			t.Fatalf("the V2-074 owner.js block has no words for the disposition %q", disposition)
		}
	}
	// The sibling blocks are still present in the same single file.
	for _, want := range []string{"/v1/release/state", "/v1/runners", "/v1/providers", "executability", "requirement_backlog", "captured_at", "allocation_limit"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js lost the pre-existing block reference %q", want)
		}
	}
	// No raw JSON, no timer, no external asset, script or font, and no
	// email-shaped value.
	for _, forbidden := range []string{"JSON.stringify", "http://", "https://", "//cdn", "importScripts", "setInterval", "setTimeout", "@font-face", "@"} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("the V2-074 owner.js block references %q", forbidden)
		}
	}
	if !strings.Contains(js, `"@"`) {
		t.Fatal("the pre-existing owner.js locator block no longer contains an '@'; this scan's control is stale")
	}
	section := html[strings.Index(html, "V2-074 provider compatibility and handoff"):]
	if strings.Contains(section, "@") {
		t.Fatal("the V2-074 owner console section carries an '@'")
	}
	lowered := strings.ToLower(section)
	for _, forbidden := range []string{
		"password", "secret", "api_key", "apikey", "bearer ", "authorization:", "private_key",
		"@example.", "@gmail.", "accounts.google.com", "credential", "raw_prompt", "raw_provider_output",
		"budget", "billing", "quota", "spend", "credit",
	} {
		if strings.Contains(lowered, forbidden) {
			t.Fatalf("the rendered V2-074 section carries a forbidden value %q", forbidden)
		}
	}
	// No threshold number: the section between its own two markers carries no
	// digit at all outside its task identifier and the heading tag names.
	start := strings.Index(html, "V2-074 provider compatibility and handoff")
	end := strings.Index(html, "/V2-074 provider compatibility and handoff")
	if start < 0 || end <= start {
		t.Fatal("the V2-074 section markers are not both present in owner.html")
	}
	own := html[start:end]
	scrubbed := strings.ReplaceAll(own, "V2-074", "")
	for _, tag := range []string{"<h2", "</h2", "<h3", "</h3"} {
		scrubbed = strings.ReplaceAll(scrubbed, tag, "")
	}
	if regexp.MustCompile(`[0-9]`).MatchString(scrubbed) {
		t.Fatalf("the V2-074 owner console section carries a digit outside its own task identifier and its heading tags: %s", own)
	}
	// The console must not claim a measurement it does not have.
	for _, forbidden := range []string{"capability exercised", "preview journey passed", "versions are compatible", "every machine reported"} {
		if strings.Contains(lowered, forbidden) || strings.Contains(strings.ToLower(block), forbidden) {
			t.Fatalf("the V2-074 owner console claims %q", forbidden)
		}
	}
	// And it says the two things a reader must not get wrong, in plain words.
	for _, want := range []string{
		"a proposal an owner reads",
		"reading unknown as fine would be a mistake",
		"is established by nothing on this page",
	} {
		if !strings.Contains(lowered, strings.ToLower(want)) {
			t.Fatalf("the V2-074 section does not say %q in plain words", want)
		}
	}
}

// TestTheBacklogStillRefusesACursorItDidNotIssue pins the refusal boundary in
// BOTH directions, in the same change that makes a cursor the route issued
// actually work (V2-079). The fix moved a query argument inside the Firestore
// adapter; it touched no validation, and this test is what says so rather than
// promising it.
//
// Measured on this tree, the shapes the route refuses with 400 invalid_request
// and the shapes it accepts with 200 are exactly what they were before the fix.
// The boundary the refusals must NOT cross is asserted here too: a well-formed
// v1 envelope naming a Requirement id that does not exist is NOT an error, and
// neither is one naming an id that belongs to another aggregate; the cursor
// carries an opaque ordering key, not a proof of existence, and turning either
// into a 400 would be a behaviour change this task forbids itself.
func TestTheBacklogStillRefusesACursorItDidNotIssue(t *testing.T) {
	h := testHandler(t)
	for _, r := range []string{"one", "two", "three"} {
		w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"`+r+`","text":"x"}`, "owner")
		if w.Code != http.StatusCreated {
			t.Fatalf("seeding %s: %d %s", r, w.Code, w.Body.String())
		}
	}
	first := call(h, http.MethodGet, "/v1/requirements?page_size=1", "", "owner")
	if first.Code != http.StatusOK {
		t.Fatalf("first page: %d %s", first.Code, first.Body.String())
	}
	var page struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || page.NextCursor == "" {
		t.Fatalf("first page carried no cursor to work from: %v %s", err, first.Body.String())
	}
	// The cursor the route itself issued is accepted: that is the outcome this
	// task delivers, and it is what makes the refusals below meaningful rather
	// than a route that refuses everything.
	if w := call(h, http.MethodGet, "/v1/requirements?page_size=1&cursor="+page.NextCursor, "", "owner"); w.Code != http.StatusOK {
		t.Fatalf("the cursor the route itself issued was refused: %d %s", w.Code, w.Body.String())
	}

	envelope := func(body string) string { return base64.RawURLEncoding.EncodeToString([]byte(body)) }
	for _, bad := range []struct{ name, cursor string }{
		// Fabricated: not base64url at all ("." and "*" are outside the alphabet).
		{"fabricated-not-base64url", "fabricated.cursor*the-route-never-issued"},
		// A well-formed base64url envelope whose version is not v1.
		{"envelope-version-is-not-v1", envelope(`{"v":"v2","after":"requirement-b"}`)},
		// A truncated prefix of a cursor the route actually issued.
		{"truncated-prefix-of-a-real-cursor", page.NextCursor[:len(page.NextCursor)/2]},
		// Another collection's key space: the Firestore document key of an
		// Increment, which is base64url of the Increment id. It decodes, and
		// what it decodes to is not a cursor envelope.
		{"another-collections-document-key-space", envelope("increment-b")},
		// The lease-reconciliation paged read's own cursor shape, which is
		// base64url of "<expires-at>\n<document-key>": another key space again,
		// and refused by this envelope rather than silently paged.
		{"another-paged-reads-cursor-shape", envelope("2023-11-14T22:13:20Z\naW5jcmVtZW50LWI")},
	} {
		t.Run(bad.name, func(t *testing.T) {
			w := call(h, http.MethodGet, "/v1/requirements?page_size=1&cursor="+bad.cursor, "", "owner")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"error":"invalid_request"`) {
				t.Fatalf("error code is not invalid_request: %s", w.Body.String())
			}
		})
	}

	// The boundary the refusals must not cross. Both of these are 200 today and
	// this task must not turn either into a 400: the ordering key inside the
	// envelope is opaque, and a page positioned after a key no row holds is an
	// empty or later page, not a caller mistake.
	for _, ok := range []struct{ name, cursor string }{
		{"v1-envelope-naming-a-requirement-that-does-not-exist", envelope(`{"v":"v1","after":"requirement-does-not-exist"}`)},
		{"v1-envelope-naming-another-aggregates-id", envelope(`{"v":"v1","after":"increment-b"}`)},
	} {
		t.Run(ok.name, func(t *testing.T) {
			w := call(h, http.MethodGet, "/v1/requirements?page_size=1&cursor="+ok.cursor, "", "owner")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (this shape is accepted today and must stay accepted): %s", w.Code, w.Body.String())
			}
		})
	}
}

// ===========================================================================
// V2-082: a Requirement can reach the state that asks a person a question.
// ===========================================================================

// startFramingBody builds the whole request body for the new verb. The request
// carries exactly request_id and expected_requirement_version: no
// control_revision, no repository_id and no timestamp. strconv and strings are
// already imported by this file, so this adds no import.
func startFramingBody(requestID string, version domain.Version) string {
	return `{"request_id":"` + requestID + `","expected_requirement_version":` + strconv.FormatInt(int64(version), 10) + `}`
}

// captureRequirement drives the real capture route as the owner and returns the
// id and version it reports. It exists so the V2-082 tests below never write a
// status into the store by hand: the whole point of this task is that the
// application layer, and only the application layer, moves the status.
func captureRequirement(t *testing.T, h http.Handler, tag string) (string, domain.Version) {
	t.Helper()
	w := call(h, http.MethodPost, "/v1/requirements", `{"request_id":"`+tag+`:capture","text":"a requirement to be shaped"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("capture status=%d body=%s", w.Code, w.Body.String())
	}
	doc := decodeBody(t, w.Body.Bytes())
	id, ok := doc["requirement_id"].(string)
	if !ok || id == "" {
		t.Fatalf("capture response carries no requirement_id: %s", w.Body.String())
	}
	version, ok := doc["version"].(float64)
	if !ok {
		t.Fatalf("capture response carries no version: %s", w.Body.String())
	}
	return id, domain.Version(version)
}

// humanInputRowExists reads the needs-input side table directly, so "the
// absence of a row" is asserted against storage rather than inferred from a
// response body.
func humanInputRowExists(t *testing.T, st *memory.Store, id string) bool {
	t.Helper()
	found := false
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		_, ok, e := u.HumanInputRequest(context.Background(), id)
		found = ok
		return e
	}); err != nil {
		t.Fatalf("reading the needs-input row: %v", err)
	}
	return found
}

// TestRequestInputIsRefusedForACapturedRequirement is V2-082 A3's
// reproduction, kept in the tree as A7's permanent assertion.
//
// A3 asked for the defect to be reproduced over HTTP before it was repaired,
// and A7 asks for that reproduction to become permanent, because the repair is
// that framing became REACHABLE -- never that the needs-input command became
// callable from captured. domain.DecideRequirement admits
// domain.RequirementNeedInput from framing, active and evaluating only, so a
// Requirement that has only been captured must refuse the ask, and the refusal
// must be attributable to the domain transition rather than to validation, to
// the role gate or to the expected version.
func TestRequestInputIsRefusedForACapturedRequirement(t *testing.T) {
	h, st, _ := needsInputHandler(t)
	id, version := captureRequirement(t, h, "a3")

	before, ok := st.Requirement(id)
	if !ok {
		t.Fatalf("the captured Requirement %q is not in the store", id)
	}
	if before.Status != domain.RequirementCaptured || before.Version != version {
		t.Fatalf("the capture route did not leave the Requirement at captured/v%d: %+v", version, before)
	}
	if humanInputRowExists(t, st, id) {
		t.Fatal("a freshly captured Requirement already carries a needs-input row")
	}

	// The ask is well-formed, is sent by a role the route accepts, and names
	// the Requirement's actual current version, so none of validation, the
	// role gate or the version check can be what refuses it.
	w := call(h, http.MethodPost, "/v1/requirements/"+id+":request-input", askBody("a3-ask", version), "runner")
	if w.Code == http.StatusOK {
		t.Fatalf("the needs-input ask succeeded from captured; domain.DecideRequirement admits needs-input from framing, active and evaluating only: %s", w.Body.String())
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("the refusal is not the domain transition: status=%d body=%s (401/403 would be the role gate, 409 the version check)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, domain.ErrInvalidTransition.Error()) {
		t.Fatalf("the refusal does not name the domain transition error %q: %s", domain.ErrInvalidTransition.Error(), body)
	}
	t.Logf("A3 reproduction: POST :request-input on a captured Requirement -> status=%d body=%s", w.Code, body)

	// The refusal changed nothing: status, version and the absence of the row
	// are all exactly what they were.
	after, ok := st.Requirement(id)
	if !ok {
		t.Fatal("the Requirement disappeared")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused ask changed the Requirement: before=%+v after=%+v", before, after)
	}
	if humanInputRowExists(t, st, id) {
		t.Fatal("a refused ask created a needs-input row")
	}
}

// framingClock is the injected clock the V2-082 chain test advances by hand.
// It exists for one measured reason: a read transaction reserves the
// conservative read boundary against the daily quota budget, and the memory
// adapter never trues that reservation up, so a fixed clock allows only four
// read transactions per UTC day per store. The chain below performs more than
// four reads, so the clock is stepped forward a whole day between them. That
// is an injected clock advanced explicitly, never a sleep, a timer or a
// goroutine, and the assertions below never depend on the values it returns.
type framingClock struct{ at time.Time }

func (c *framingClock) Now() time.Time { return c.at }

func (c *framingClock) nextDay() { c.at = c.at.Add(25 * time.Hour) }

// framingHandler composes the same handler needsInputHandler composes -- owner,
// runner and scheduler bearer tokens over a memory store -- with the steppable
// clock above. It is a separate builder rather than an edit to
// needsInputHandler, whose signature several pre-existing tests depend on.
func framingHandler(t *testing.T) (http.Handler, *memory.Store, *framingClock) {
	t.Helper()
	st := memory.New()
	c := &framingClock{at: time.Unix(1700000000, 0).UTC()}
	svc, err := application.NewServiceWithConfig(st, c, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{
		"owner":     {Role: application.RoleOwner, Subject: "owner"},
		"runner":    {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"},
		"scheduler": {Role: application.RoleScheduler, Subject: "scheduler.self"},
	}
	return api.New(api.Config{Authenticator: auth, Service: svc, AllowedOrigins: []string{"https://console.example"}}), st, c
}

// TestStartFramingRouteIsUnambiguousAndRoleGated is V2-082 A8, plus the
// transport half of A7.
//
// The routing claims are asserted rather than read. The new suffix cannot be
// swallowed by the /v1/executions/{id}:start branch, because that branch is
// prefix-gated on /v1/executions/ and because ":start-framing" does not
// satisfy HasSuffix(path, ":start") either -- both directions are checked. It
// cannot be swallowed by the GET /v1/requirements/ prefix branch, because that
// branch is method-gated, and the proof that the POST reaches the command is
// that it changes state. An empty id is a 404 rather than a route to the
// command with an empty target.
func TestStartFramingRouteIsUnambiguousAndRoleGated(t *testing.T) {
	h, st, _ := framingHandler(t)
	id, version := captureRequirement(t, h, "a8")
	path := "/v1/requirements/" + id + startFramingRouteSuffix

	// 401 unauthenticated.
	if w := call(h, http.MethodPost, path, startFramingBody("a8-unauth", version), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", w.Code, w.Body.String())
	}
	// 403 for a runner: a Runner executes an Increment it was handed and does
	// not decide that a Requirement should start being shaped.
	if w := call(h, http.MethodPost, path, startFramingBody("a8-runner", version), "runner"); w.Code != http.StatusForbidden {
		t.Fatalf("runner status=%d body=%s", w.Code, w.Body.String())
	}
	// An empty requirement id is a 404, not a command with an empty target.
	if w := call(h, http.MethodPost, "/v1/requirements/"+startFramingRouteSuffix, startFramingBody("a8-empty", version), "owner"); w.Code != http.StatusNotFound {
		t.Fatalf("empty id status=%d body=%s", w.Code, w.Body.String())
	}
	// The execution-scoped :start branch does not claim this suffix, in either
	// direction: an executions path carrying :start-framing is a 404, because
	// ":start-framing" does not satisfy HasSuffix(path, ":start").
	if w := call(h, http.MethodPost, "/v1/executions/"+id+startFramingRouteSuffix, startFramingBody("a8-exec", version), "owner"); w.Code != http.StatusNotFound {
		t.Fatalf("an executions path with the framing suffix status=%d body=%s; it must not reach either handler", w.Code, w.Body.String())
	}
	// A GET to the same path is not the framing route: it falls through to the
	// method-gated read branch, where the framing suffix is part of an id no
	// Requirement has, and it changes nothing.
	if w := call(h, http.MethodGet, path, "", "owner"); w.Code == http.StatusOK {
		t.Fatalf("a GET to the framing path returned 200: %s", w.Body.String())
	}
	// A body field the contract does not declare is refused by the strict
	// decoder rather than ignored: the request carries no control_revision, no
	// repository_id and no timestamp, and cannot be made to.
	for _, smuggled := range []string{
		`{"request_id":"a8-smuggle-a","expected_requirement_version":1,"control_revision":0}`,
		`{"request_id":"a8-smuggle-b","expected_requirement_version":1,"repository_id":"repository-a"}`,
		`{"request_id":"a8-smuggle-c","expected_requirement_version":1,"at":"2026-01-01T00:00:00Z"}`,
	} {
		if w := call(h, http.MethodPost, path, smuggled, "owner"); w.Code != http.StatusBadRequest {
			t.Fatalf("a smuggled field was accepted: status=%d body=%s request=%s", w.Code, w.Body.String(), smuggled)
		}
	}

	// Every refusal above left the Requirement byte-unchanged.
	before, ok := st.Requirement(id)
	if !ok {
		t.Fatal("the Requirement disappeared")
	}
	if before.Status != domain.RequirementCaptured || before.Version != version {
		t.Fatalf("a refused request changed the Requirement: %+v", before)
	}

	// An unknown Requirement id is a 404 from the command, not from the router.
	if w := call(h, http.MethodPost, "/v1/requirements/requirement-does-not-exist"+startFramingRouteSuffix, startFramingBody("a8-unknown", 1), "owner"); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown Requirement status=%d body=%s", w.Code, w.Body.String())
	}
	// A stale expected version is a 409.
	if w := call(h, http.MethodPost, path, startFramingBody("a8-stale", version+7), "owner"); w.Code != http.StatusConflict {
		t.Fatalf("a stale expected version status=%d body=%s", w.Code, w.Body.String())
	}
	if after, _ := st.Requirement(id); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused request changed the Requirement: before=%+v after=%+v", before, after)
	}

	// The owner may call it, and the proof that the POST reached the command
	// rather than the method-gated read branch is that the state moved.
	w := call(h, http.MethodPost, path, startFramingBody("a8-owner", version), "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("owner start-framing status=%d body=%s", w.Code, w.Body.String())
	}
	doc := decodeBody(t, w.Body.Bytes())
	for key := range doc {
		if key != "requirement_id" && key != "status" && key != "version" {
			t.Fatalf("the start-framing response carries an unnamed field %q", key)
		}
	}
	if doc["requirement_id"] != id || doc["status"] != "framing" {
		t.Fatalf("the start-framing response is %s", w.Body.String())
	}
	framed, _ := st.Requirement(id)
	if framed.Status != domain.RequirementFraming || framed.Version != version+1 {
		t.Fatalf("the POST did not reach the command; the Requirement is %+v", framed)
	}
	// A second framing of the same Requirement, with a fresh request id, is
	// refused by the domain transition table: captured is the only source.
	if w = call(h, http.MethodPost, path, startFramingBody("a8-again", framed.Version), "owner"); w.Code != http.StatusBadRequest {
		t.Fatalf("framing an already-framing Requirement status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), domain.ErrInvalidTransition.Error()) {
		t.Fatalf("the second framing was not refused by the domain transition: %s", w.Body.String())
	}
	if again, _ := st.Requirement(id); !reflect.DeepEqual(again, framed) {
		t.Fatalf("a refused second framing changed the Requirement: %+v", again)
	}

	// The scheduler is accepted too, on its own Requirement: the route gates
	// exactly the pair the capture route gates.
	otherID, otherVersion := captureRequirement(t, h, "a8-scheduler")
	if w = call(h, http.MethodPost, "/v1/requirements/"+otherID+startFramingRouteSuffix, startFramingBody("a8-sched", otherVersion), "scheduler"); w.Code != http.StatusOK {
		t.Fatalf("scheduler start-framing status=%d body=%s", w.Code, w.Body.String())
	}

	// The route is declared in the transport contract, with the operation id
	// and the two schemas this task names, and the read surface the capability
	// declares is still declared too.
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "openapi", "openapi-v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	yaml := string(data)
	for _, want := range []string{
		"  /v1/requirements/{requirement_id}" + startFramingRouteSuffix + ":",
		"operationId: startRequirementFraming",
		"StartRequirementFramingRequest:",
		"StartRequirementFramingResponse:",
		"  /v1/requirements/{requirement_id}:",
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("openapi-v1.yaml does not declare %q", want)
		}
	}
	// The declared request carries exactly the two fields the command accepts.
	block := yaml[strings.Index(yaml, "StartRequirementFramingRequest:"):]
	block = block[:strings.Index(block, "StartRequirementFramingResponse:")]
	if !strings.Contains(block, "additionalProperties: false") || !strings.Contains(block, "required: [request_id, expected_requirement_version]") {
		t.Fatalf("the declared request schema is not closed to exactly two required fields: %s", block)
	}
	// The forbidden-name check runs against the declared PROPERTIES rather
	// than the whole block: the block's description says in prose which three
	// fields the request deliberately does not carry, and that sentence must
	// not be deleted to satisfy a substring check.
	properties := block[strings.Index(block, "properties:"):]
	for _, forbidden := range []string{"control_revision", "repository_id", "timestamp", "password", "credential", "token", "secret", "raw_prompt", "raw_provider_output"} {
		if strings.Contains(properties, forbidden) {
			t.Fatalf("the declared request properties carry %q", forbidden)
		}
	}
	if !strings.Contains(properties, "request_id") || !strings.Contains(properties, "expected_requirement_version") {
		t.Fatalf("the declared request properties are not the two fields the command accepts: %s", properties)
	}
}

// startFramingRouteSuffix is the route suffix, built here rather than imported
// because internal/api's constant is unexported. It is a literal path segment
// and not a module path, so it is invisible to the manifest derivation.
const startFramingRouteSuffix = ":start-framing"

// TestARequirementCapturedThroughTheRouteCanBeDrivenToTheQuestionAndResumedAsTheSameRequirement
// is V2-082 A10.
//
// One test, over the composed handler, for the whole chain: capture ->
// start-framing -> request-input -> the detail shows the question ->
// answer-input. It is the only place the declared success condition is
// asserted in one run, and it asserts the declared failure condition in the
// same run: the requirement_id is IDENTICAL at the end, and the total number
// of Requirements is unchanged, so resuming created nothing.
//
// Nothing in this test writes a status into the store by hand. Every status
// the Requirement passes through is produced by an application command reached
// over HTTP, which is precisely what was impossible before this task: the only
// Requirement status reachable through any application command was captured.
func TestARequirementCapturedThroughTheRouteCanBeDrivenToTheQuestionAndResumedAsTheSameRequirement(t *testing.T) {
	h, st, c := framingHandler(t)

	// 1. Capture, over the real route, as the owner.
	id, capturedVersion := captureRequirement(t, h, "a10")
	if capturedVersion != 1 {
		t.Fatalf("capture reported version %d; the capture response contract is version 1", capturedVersion)
	}

	requirementCount := func(step string) int {
		c.nextDay()
		w := call(h, http.MethodGet, "/v1/requirements?page_size=100", "", "owner")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: the requirement page status=%d body=%s", step, w.Code, w.Body.String())
		}
		var page struct {
			Requirements []struct {
				RequirementID string `json:"requirement_id"`
			} `json:"requirements"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return len(page.Requirements)
	}
	detail := func(step string) map[string]any {
		c.nextDay()
		w := call(h, http.MethodGet, "/v1/requirements/"+id, "", "owner")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: the detail status=%d body=%s", step, w.Code, w.Body.String())
		}
		return decodeBody(t, w.Body.Bytes())
	}

	countAfterCapture := requirementCount("after capture")
	if countAfterCapture != 1 {
		t.Fatalf("the fixture holds %d Requirements, want exactly 1", countAfterCapture)
	}

	// 2. The detail reports captured and carries no needs_input key at all.
	afterCapture := detail("after capture")
	if afterCapture["status"] != "captured" {
		t.Fatalf("the detail reports %v after capture", afterCapture["status"])
	}
	if _, present := afterCapture["needs_input"]; present {
		t.Fatalf("a captured Requirement already carries a needs_input key: %+v", afterCapture)
	}
	if afterCapture["version"] != float64(capturedVersion) {
		t.Fatalf("the detail reports version %v after capture", afterCapture["version"])
	}

	// 3. start-framing, as the owner. The response AND the detail both report
	// framing, at exactly one more than the captured version.
	w := call(h, http.MethodPost, "/v1/requirements/"+id+startFramingRouteSuffix, startFramingBody("a10-frame", capturedVersion), "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("start-framing status=%d body=%s", w.Code, w.Body.String())
	}
	framingResponse := decodeBody(t, w.Body.Bytes())
	if framingResponse["requirement_id"] != id {
		t.Fatalf("start-framing named a different Requirement: %s", w.Body.String())
	}
	if framingResponse["status"] != "framing" {
		t.Fatalf("start-framing reports %v", framingResponse["status"])
	}
	if framingResponse["version"] != float64(capturedVersion+1) {
		t.Fatalf("start-framing reports version %v, want %d", framingResponse["version"], capturedVersion+1)
	}
	framingVersion := domain.Version(framingResponse["version"].(float64))
	afterFraming := detail("after start-framing")
	if afterFraming["status"] != "framing" || afterFraming["version"] != float64(framingVersion) {
		t.Fatalf("the detail after start-framing is %+v", afterFraming)
	}
	if _, present := afterFraming["needs_input"]; present {
		t.Fatalf("framing alone recorded a question: %+v", afterFraming)
	}
	// A13: nextAction is status-agnostic apart from completed and cancelled,
	// so a framing Requirement with no Increment still reports the same next
	// action a captured one does. This is why no read model changed.
	if afterFraming["next_action"] != afterCapture["next_action"] {
		t.Fatalf("framing changed next_action from %v to %v; the read model was supposed to be untouched", afterCapture["next_action"], afterFraming["next_action"])
	}

	// 4. request-input, as a runner, with a reason class, two options each
	// carrying a non-empty impact, and both scope lists.
	w = call(h, http.MethodPost, "/v1/requirements/"+id+":request-input", askBody("a10-ask", framingVersion), "runner")
	if w.Code != http.StatusOK {
		t.Fatalf("request-input status=%d body=%s", w.Code, w.Body.String())
	}
	askResponse := decodeBody(t, w.Body.Bytes())
	if askResponse["status"] != "needs-input" || askResponse["requirement_id"] != id {
		t.Fatalf("the ask response is %s", w.Body.String())
	}
	askVersion := domain.Version(askResponse["version"].(float64))

	// 5. The detail carries the question: the reason class, every option with
	// its impact, and both scope lists.
	asked := detail("after request-input")
	if asked["status"] != "needs-input" {
		t.Fatalf("the detail after the ask reports %v", asked["status"])
	}
	question, ok := asked["needs_input"].(map[string]any)
	if !ok {
		t.Fatalf("the detail carries no needs_input object: %+v", asked)
	}
	if question["question"] != "Delete the branch or keep it?" {
		t.Fatalf("the detail reports the question as %v", question["question"])
	}
	if question["reason_class"] != "destructive-irreversible" {
		t.Fatalf("the detail reports the reason class as %v", question["reason_class"])
	}
	options, ok := question["options"].([]any)
	if !ok || len(options) != 2 {
		t.Fatalf("the detail reports %v options, want 2", question["options"])
	}
	seen := map[string]bool{}
	for _, raw := range options {
		option, isObject := raw.(map[string]any)
		if !isObject {
			t.Fatalf("an option is not an object: %v", raw)
		}
		impact, isString := option["impact"].(string)
		if !isString || impact == "" {
			t.Fatalf("an option carries no impact: %v", option)
		}
		optionID, isString := option["option_id"].(string)
		if !isString || optionID == "" {
			t.Fatalf("an option carries no option_id: %v", option)
		}
		seen[optionID] = true
	}
	if !seen["delete"] || !seen["keep"] {
		t.Fatalf("the detail does not report both recorded options: %v", seen)
	}
	for _, list := range []string{"stopped_scope", "continuing_scope"} {
		entries, isList := question[list].([]any)
		if !isList || len(entries) == 0 {
			t.Fatalf("the detail reports no %s: %v", list, question[list])
		}
	}

	// 6. answer-input, as the owner, naming one recorded option. The SAME
	// Requirement resumes: identical id, status ready, and the total number of
	// Requirements is unchanged. That triple is the declared success condition
	// and the declared failure condition asserted in the same run.
	w = call(h, http.MethodPost, "/v1/requirements/"+id+":answer-input", answerBodyJSON("a10-answer", askVersion, "keep"), "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("answer-input status=%d body=%s", w.Code, w.Body.String())
	}
	answer := decodeBody(t, w.Body.Bytes())
	if answer["requirement_id"] != id {
		t.Fatalf("the answer resumed a different Requirement: %v vs %q", answer["requirement_id"], id)
	}
	if answer["status"] != "ready" {
		t.Fatalf("the answer left the Requirement at %v, want ready", answer["status"])
	}
	if answer["answered_option_id"] != "keep" {
		t.Fatalf("the answer recorded option %v", answer["answered_option_id"])
	}
	if got := requirementCount("after the answer"); got != countAfterCapture {
		t.Fatalf("the chain changed the number of Requirements from %d to %d; resuming must create nothing", countAfterCapture, got)
	}
	final, ok := st.Requirement(id)
	if !ok {
		t.Fatal("the Requirement disappeared")
	}
	if final.Status != domain.RequirementReady {
		t.Fatalf("the stored Requirement is %+v, want ready", final)
	}
	t.Logf("A10: one Requirement %q walked captured -> framing -> needs-input -> ready over the composed handler, at versions %d -> %d -> %d -> %d, with the Requirement count unchanged at %d",
		id, capturedVersion, framingVersion, askVersion, final.Version, countAfterCapture)
}

// TestARequirementLeftInFramingStillReadsAfterTheChangeIsReverted is V2-082
// A16's verification clause. Rollback is a revert of internal/application/framing.go,
// the one branch and one suffix constant in internal/api/api.go, and the one
// path block in contracts/openapi/openapi-v1.yaml -- and nothing else, because
// the change adds no port, no field to any stored record, no document kind and
// no index, and writes only documents the existing capture and plan paths
// already write.
//
// The claim that needs verifying is the last one: that no data exists which
// only the new code can read. framing is a status the domain already accepts
// and every read model already reports, so a Requirement left in framing by
// reverted code still reads. That is asserted here by putting a Requirement
// into framing WITHOUT the new command -- writing the status directly, exactly
// as the pre-existing needs-input fixture writes active -- and reading it back
// through the pre-existing detail route.
func TestARequirementLeftInFramingStillReadsAfterTheChangeIsReverted(t *testing.T) {
	h, st, c := framingHandler(t)
	id, version := captureRequirement(t, h, "a16")

	// The state a reverted tree would be left holding, written without any
	// V2-082 code path.
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		r, ok, e := u.Requirement(context.Background(), id)
		if e != nil || !ok {
			t.Fatalf("seed: ok=%v err=%v", ok, e)
		}
		next := r
		next.Status = domain.RequirementFraming
		next.Version++
		return u.SaveRequirement(context.Background(), next, r.Version)
	}); err != nil {
		t.Fatal(err)
	}

	c.nextDay()
	w := call(h, http.MethodGet, "/v1/requirements/"+id, "", "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("a Requirement stored in framing does not read: status=%d body=%s", w.Code, w.Body.String())
	}
	doc := decodeBody(t, w.Body.Bytes())
	if doc["status"] != "framing" {
		t.Fatalf("the detail reports %v for a Requirement stored in framing", doc["status"])
	}
	if doc["version"] != float64(version+1) {
		t.Fatalf("the detail reports version %v", doc["version"])
	}
	// And the read model reports the same next action it reports for a
	// captured Requirement with no Increment, which is why readmodels.go is
	// prohibited by design rather than by omission.
	if doc["next_action"] != "plan increments" {
		t.Fatalf("a framing Requirement with no Increment reports next_action %v", doc["next_action"])
	}
	c.nextDay()
	if w = call(h, http.MethodGet, "/v1/requirements?page_size=100", "", "owner"); w.Code != http.StatusOK {
		t.Fatalf("the requirement page does not read with a framing Requirement in it: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), id) {
		t.Fatalf("the requirement page omits the framing Requirement: %s", w.Body.String())
	}
}

// ===========================================================================
// V2-083: a storage fault is not the caller's mistake.
//
// A3 -- THE PIN TABLE. This table is written and measured GREEN ON THE
// UNEDITED TREE before internal/api/api.go's domainError default is touched,
// and re-measured afterwards. Every row pins BOTH the HTTP status and the
// error code of one (method, path, role, body) case, so "no existing status
// code moved" is a measurement rather than a promise. A row is never adjusted
// to match a new answer: a row that moves means the design was departed from.
// ===========================================================================

// pinInstallation and pinReconcileIdentity are the fixture's two identities.
// The reconcile identity is asserted through the IAP header exactly the way
// internal/api/api.go's /internal/reconcile branch reads it.
const (
	pinInstallation       = "install"
	pinReconcileIdentity  = "reconciler@example.com"
	pinReconcileAssertion = "accounts.google.com:" + pinReconcileIdentity
)

// pinPublicKey is a deterministic ed25519 public key in the encoding
// internal/api's decodePublicKey accepts first. It is derived from an
// all-zero seed rather than generated, so no row depends on randomness.
var pinPublicKey = base64.RawStdEncoding.EncodeToString([]byte(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)))

// pinRow is one measured case. wantCode is the "error" field of the response
// envelope; an empty wantCode means the row is a success and carries no error
// code at all.
type pinRow struct {
	name        string
	method      string
	path        string
	body        string
	token       string
	contentType string // "" means application/json; "-" means send no Content-Type
	origin      string
	headers     map[string]string
	// sameDay keeps the injected clock where it is. Every other row steps it
	// a whole UTC day first, because a read transaction reserves the
	// conservative read boundary against the daily quota budget and the
	// memory adapter never trues that reservation up, so a fixed clock allows
	// only four read transactions per UTC day per store. That is an injected
	// clock advanced explicitly -- never a sleep, a timer or a goroutine.
	sameDay bool
	// unconfigured drives the row against a Handler composed with neither an
	// Authenticator nor a Service, which is the fail-closed shape.
	unconfigured bool
	// exhaustQuota fills the day's whole write budget before the request, so
	// the next mutation's worst-case reservation is refused by one write.
	exhaustQuota bool
	wantStatus   int
	wantCode     string
	// capture records values from a success response for later rows.
	capture func(*pinFixture, map[string]any)
}

// pinFixture composes the whole authenticated surface once: a memory store, a
// steppable injected clock, owner/runner/scheduler bearer tokens, runner
// enrollment, an /internal/reconcile hook that succeeds, and a second
// Handler with nothing configured at all.
type pinFixture struct {
	handler      http.Handler
	unconfigured http.Handler
	store        *memory.Store
	service      *application.Service
	clock        *framingClock
	values       map[string]string
}

// expand substitutes the fixture's recorded values into a path or body.
func (fx *pinFixture) expand(s string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	for k, v := range fx.values {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func (fx *pinFixture) ownerContext() context.Context {
	return application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
}

// pinSeedRequirement captures a Requirement through the real route with a
// caller-supplied identifier, so every row below names a stable id.
func (fx *pinFixture) pinSeedRequirement(t *testing.T, id string) {
	t.Helper()
	fx.clock.nextDay()
	w := pinCall(fx.handler, pinRow{method: http.MethodPost, token: "owner"}, "/v1/requirements",
		`{"request_id":"pin:capture:`+id+`","requirement_id":"`+id+`","text":"a requirement the pin table names"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seeding requirement %s: %d %s", id, w.Code, w.Body.String())
	}
}

// pinMakeActive moves a captured Requirement to active through the store, the
// same way seedActiveRequirement does: no /v1 route advances it.
func (fx *pinFixture) pinMakeActive(t *testing.T, id string) domain.Version {
	t.Helper()
	fx.clock.nextDay()
	var version domain.Version
	if err := fx.store.Transact(context.Background(), func(u application.UnitOfWork) error {
		r, ok, e := u.Requirement(context.Background(), id)
		if e != nil || !ok {
			t.Fatalf("seed active %s: ok=%v err=%v", id, ok, e)
		}
		next := r
		next.Status = domain.RequirementActive
		next.Version++
		version = next.Version
		return u.SaveRequirement(context.Background(), next, r.Version)
	}); err != nil {
		t.Fatal(err)
	}
	return version
}

func (fx *pinFixture) pinSeedRepository(t *testing.T, id, url string) {
	t.Helper()
	fx.clock.nextDay()
	w := pinCall(fx.handler, pinRow{method: http.MethodPost, token: "owner"}, "/v1/repositories",
		`{"request_id":"pin:repo:`+id+`","repository_id":"`+id+`","source_url":"`+url+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seeding repository %s: %d %s", id, w.Code, w.Body.String())
	}
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	st := memory.New()
	clk := &framingClock{at: time.Unix(1700000000, 0).UTC()}
	svc, err := application.NewServiceWithConfig(st, clk, &ids{}, application.ServiceConfig{InstallationID: pinInstallation, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{
		"owner":     {Role: application.RoleOwner, Subject: "owner"},
		"runner":    {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"},
		"scheduler": {Role: application.RoleScheduler, Subject: "scheduler.self"},
	}
	fx := &pinFixture{
		handler: api.New(api.Config{
			Authenticator:     auth,
			Service:           svc,
			RunnerEnrollment:  enrollment,
			AllowedOrigins:    []string{"https://console.example"},
			InternalReconcile: func(context.Context) error { return nil },
			ReconcileIdentity: pinReconcileIdentity,
		}),
		unconfigured: api.New(api.Config{}),
		store:        st,
		service:      svc,
		clock:        clk,
		values:       map[string]string{},
	}

	// Three Requirements with distinct roles, so no row depends on a status
	// another row moved: one stays captured for ever, one is the
	// :start-framing success, one is active for the needs-input pair, and one
	// is the Increment the runner-facing journey claims.
	for _, id := range []string{"pin-captured", "pin-framing", "pin-active", "pin-loop"} {
		fx.pinSeedRequirement(t, id)
	}
	activeVersion := fx.pinMakeActive(t, "pin-active")
	loopVersion := fx.pinMakeActive(t, "pin-loop")
	fx.pinSeedRepository(t, "repo-pin", "https://github.com/pin/observed")
	fx.pinSeedRepository(t, "repo-retire", "https://github.com/pin/retired")

	// The Increment the runner-facing journey claims. No /v1 route creates
	// one, so Plan and Prepare are driven through the Service directly, the
	// way internal/api/live_dogfood_test.go's planAndPrepare does.
	fx.clock.nextDay()
	planned, err := svc.Plan(fx.ownerContext(), application.PlanRequest{RequestID: "pin:plan", RequirementID: "pin-loop", ExpectedRequirementVersion: loopVersion})
	if err != nil {
		t.Fatalf("plan (through the Service, because no /v1 route creates an Increment): %v", err)
	}
	fx.clock.nextDay()
	prepared, err := svc.Prepare(fx.ownerContext(), application.PrepareRequest{RequestID: "pin:prepare", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version})
	if err != nil {
		t.Fatalf("prepare (through the Service): %v", err)
	}

	fx.values["captured"] = "pin-captured"
	fx.values["framing"] = "pin-framing"
	fx.values["active"] = "pin-active"
	fx.values["activeVersion"] = strconv.FormatInt(int64(activeVersion), 10)
	fx.values["loop"] = "pin-loop"
	fx.values["increment"] = planned.IncrementID
	fx.values["incrementVersion"] = strconv.FormatInt(int64(prepared.Version), 10)
	fx.values["publicKey"] = pinPublicKey
	return fx
}

// pinCorrelationID is sent on every row so the whole response body is
// deterministic and can be compared BYTE FOR BYTE before and after the
// default is inverted (A14): correlation_id is the only field in the error
// envelope that is otherwise random per request.
const pinCorrelationID = "pin-correlation-id"

func pinCall(h http.Handler, row pinRow, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(row.method, path, strings.NewReader(body))
	req.Header.Set("X-Correlation-ID", pinCorrelationID)
	switch row.contentType {
	case "":
		req.Header.Set("Content-Type", "application/json")
	case "-":
	default:
		req.Header.Set("Content-Type", row.contentType)
	}
	if row.token != "" {
		req.Header.Set("Authorization", "Bearer "+row.token)
	}
	if row.origin != "" {
		req.Header.Set("Origin", row.origin)
	}
	for k, v := range row.headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// pinAskBody fills the existing needs-input ask template with a request id and
// a version placeholder the fixture expands, so the table reuses the same body
// shape TestNeedsInputRoutesAreRoleGated already pins.
func pinAskBody(requestID, version string) string {
	body := strings.Replace(needsInputAskBody, "REQUEST_ID", requestID, 1)
	return strings.Replace(body, "VERSION", version, 1)
}

// pinTarget is the ControlTarget every runner-facing row names.
const pinTarget = `"target":{"installation_id":"` + pinInstallation + `","requirement_id":"{loop}","increment_id":"{increment}"}`

// pinRows is the whole table. It is ordered: the runner-facing journey rows
// run inside one UTC day because a Lease lives one minute, and the quota row
// is last because it fills the day's write budget.
func pinRows() []pinRow {
	return []pinRow{
		// --- /healthz, and the fail-closed shape -------------------------
		{name: "healthz", method: http.MethodGet, path: "/healthz", wantStatus: 200},
		{name: "healthz-rejects-post", method: http.MethodPost, path: "/healthz", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "healthz-with-nothing-configured", method: http.MethodGet, path: "/healthz", unconfigured: true, wantStatus: 200},
		{name: "authenticated-route-with-nothing-configured", method: http.MethodGet, path: "/v1/requirements", unconfigured: true, wantStatus: 503, wantCode: "not_configured"},
		{name: "capture-with-nothing-configured", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"x","text":"y"}`, unconfigured: true, wantStatus: 503, wantCode: "not_configured"},
		{name: "reconcile-with-nothing-configured", method: http.MethodPost, path: "/internal/reconcile", unconfigured: true, wantStatus: 401, wantCode: "unauthorized"},

		// --- POST /internal/reconcile ------------------------------------
		{name: "reconcile-without-the-scheduler-identity", method: http.MethodPost, path: "/internal/reconcile", wantStatus: 401, wantCode: "unauthorized"},
		{name: "reconcile-with-a-runner-session-header", method: http.MethodPost, path: "/internal/reconcile",
			headers: map[string]string{"X-Goog-Authenticated-User-Email": pinReconcileAssertion, "X-Agentic-Runner-Session": "anything"},
			wantStatus: 401, wantCode: "unauthorized"},
		{name: "reconcile-with-a-foreign-identity", method: http.MethodPost, path: "/internal/reconcile",
			headers: map[string]string{"X-Goog-Authenticated-User-Email": "accounts.google.com:someone.else@example.com"},
			wantStatus: 401, wantCode: "unauthorized"},
		{name: "reconcile-accepted", method: http.MethodPost, path: "/internal/reconcile",
			headers:    map[string]string{"X-Goog-Authenticated-User-Email": pinReconcileAssertion},
			wantStatus: 202},
		{name: "reconcile-rejects-get", method: http.MethodGet, path: "/internal/reconcile", wantStatus: 405, wantCode: "method_not_allowed"},

		// --- the three enrollment routes ---------------------------------
		{name: "enrollment-issue-unauthenticated", method: http.MethodPost, path: "/v1/runner/enrollment", body: `{"runner_id":"runner-1"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "enrollment-issue-wrong-role", method: http.MethodPost, path: "/v1/runner/enrollment", body: `{"runner_id":"runner-1"}`, token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "enrollment-issue-accepted", method: http.MethodPost, path: "/v1/runner/enrollment", body: `{"runner_id":"runner-1"}`, token: "owner", wantStatus: 201},
		{name: "enrollment-issue-empty-runner-id", method: http.MethodPost, path: "/v1/runner/enrollment", body: `{"runner_id":""}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "enrollment-issue-malformed-body", method: http.MethodPost, path: "/v1/runner/enrollment", body: `{`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "enrollment-issue-unknown-field", method: http.MethodPost, path: "/v1/runner/enrollment", body: `{"runner_id":"runner-1","extra":1}`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "enrollment-issue-wrong-content-type", method: http.MethodPost, path: "/v1/runner/enrollment", body: `{"runner_id":"runner-1"}`, token: "owner", contentType: "text/plain", wantStatus: 415, wantCode: "unsupported_media_type"},
		{name: "enrollment-challenge-malformed-public-key", method: http.MethodPost, path: "/v1/runner/enrollment/challenge", body: `{"enrollment_token":"t","public_key":"zz"}`, wantStatus: 400, wantCode: "invalid_request"},
		{name: "enrollment-challenge-unknown-token", method: http.MethodPost, path: "/v1/runner/enrollment/challenge", body: `{"enrollment_token":"never-issued","public_key":"{publicKey}"}`, wantStatus: 401, wantCode: "enrollment_failed"},
		{name: "enrollment-challenge-malformed-body", method: http.MethodPost, path: "/v1/runner/enrollment/challenge", body: `{`, wantStatus: 400, wantCode: "invalid_json"},
		{name: "enrollment-challenge-rejects-get", method: http.MethodGet, path: "/v1/runner/enrollment/challenge", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "enrollment-complete-unknown-challenge", method: http.MethodPost, path: "/v1/runner/enrollment/complete", body: `{"challenge_id":"never-issued","signature":"AAAA"}`, wantStatus: 401, wantCode: "enrollment_failed"},
		{name: "enrollment-complete-malformed-body", method: http.MethodPost, path: "/v1/runner/enrollment/complete", body: `{`, wantStatus: 400, wantCode: "invalid_json"},

		// --- /owner and /owner/assets/ -----------------------------------
		{name: "owner-console", method: http.MethodGet, path: "/owner", token: "owner", wantStatus: 200},
		{name: "owner-console-trailing-slash", method: http.MethodGet, path: "/owner/", token: "owner", wantStatus: 200},
		{name: "owner-console-unauthenticated", method: http.MethodGet, path: "/owner", wantStatus: 401, wantCode: "unauthorized"},
		{name: "owner-console-wrong-role", method: http.MethodGet, path: "/owner", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "owner-asset", method: http.MethodGet, path: "/owner/assets/owner.js", wantStatus: 200},
		{name: "owner-asset-unknown", method: http.MethodGet, path: "/owner/assets/nothing.js", wantStatus: 404, wantCode: "not_found"},
		{name: "owner-asset-traversal", method: http.MethodGet, path: "/owner/assets/a..b", wantStatus: 404, wantCode: "not_found"},
		{name: "owner-asset-empty-name", method: http.MethodGet, path: "/owner/assets/", wantStatus: 404, wantCode: "not_found"},

		// --- GET /v1/requirements ----------------------------------------
		{name: "list-requirements", method: http.MethodGet, path: "/v1/requirements", token: "owner", wantStatus: 200},
		{name: "list-requirements-unauthenticated", method: http.MethodGet, path: "/v1/requirements", wantStatus: 401, wantCode: "unauthorized"},
		{name: "list-requirements-wrong-role", method: http.MethodGet, path: "/v1/requirements", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "list-requirements-page-size-is-not-an-integer", method: http.MethodGet, path: "/v1/requirements?page_size=abc", token: "owner", wantStatus: 400, wantCode: "invalid_page"},
		{name: "list-requirements-page-size-below-one", method: http.MethodGet, path: "/v1/requirements?page_size=-1", token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "list-requirements-page-size-above-the-maximum", method: http.MethodGet, path: "/v1/requirements?page_size=101", token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "list-requirements-cursor-the-route-never-issued", method: http.MethodGet, path: "/v1/requirements?cursor=fabricated.cursor*", token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "list-requirements-rejects-put", method: http.MethodPut, path: "/v1/requirements", token: "owner", wantStatus: 405, wantCode: "method_not_allowed"},

		// --- POST /v1/requirements (capture) -----------------------------
		{name: "capture", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-1","text":"a captured requirement"}`, token: "owner", wantStatus: 201},
		{name: "capture-as-the-scheduler", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-2","text":"a captured requirement"}`, token: "scheduler", wantStatus: 201},
		{name: "capture-unauthenticated", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-3","text":"x"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "capture-wrong-role", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-4","text":"x"}`, token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "capture-malformed-json", method: http.MethodPost, path: "/v1/requirements", body: `{`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "capture-unknown-field", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-5","text":"x","extra":1}`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "capture-two-json-values", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-6","text":"x"} {}`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "capture-without-a-request-id", method: http.MethodPost, path: "/v1/requirements", body: `{"text":"x"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "capture-with-empty-text-is-accepted-today", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-7","text":""}`, token: "owner", wantStatus: 201},
		{name: "capture-wrong-content-type", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-8","text":"x"}`, token: "owner", contentType: "text/plain", wantStatus: 415, wantCode: "unsupported_media_type"},
		{name: "capture-from-a-denied-origin", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-9","text":"x"}`, token: "owner", origin: "https://evil.example", wantStatus: 403, wantCode: "origin_denied"},
		{name: "capture-from-an-allowed-origin", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-10","text":"x"}`, token: "owner", origin: "https://console.example", wantStatus: 201},
		{name: "capture-reusing-a-request-id-with-another-body", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-1","text":"a different text"}`, token: "owner", wantStatus: 409, wantCode: "conflict"},
		{name: "capture-with-an-unknown-repository", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:capture-11","text":"x","repository_id":"no-such-repository"}`, token: "owner", wantStatus: 404, wantCode: "not_found"},

		// --- GET /v1/requirements/{id} -----------------------------------
		{name: "requirement-detail", method: http.MethodGet, path: "/v1/requirements/{captured}", token: "owner", wantStatus: 200},
		{name: "requirement-detail-unknown-id", method: http.MethodGet, path: "/v1/requirements/no-such-requirement", token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "requirement-detail-unauthenticated", method: http.MethodGet, path: "/v1/requirements/{captured}", wantStatus: 401, wantCode: "unauthorized"},
		{name: "requirement-detail-wrong-role", method: http.MethodGet, path: "/v1/requirements/{captured}", token: "runner", wantStatus: 403, wantCode: "forbidden"},

		// --- POST /v1/requirements/{id}:request-input --------------------
		{name: "request-input-unauthenticated", method: http.MethodPost, path: "/v1/requirements/{active}:request-input", body: `{"request_id":"pin:ask-0"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "request-input-refused-for-the-owner", method: http.MethodPost, path: "/v1/requirements/{active}:request-input", body: `{"request_id":"pin:ask-1"}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "request-input-empty-id", method: http.MethodPost, path: "/v1/requirements/:request-input", body: `{"request_id":"pin:ask-2"}`, token: "runner", wantStatus: 404, wantCode: "not_found"},
		{name: "request-input-malformed-json", method: http.MethodPost, path: "/v1/requirements/{active}:request-input", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "request-input-without-a-request-id", method: http.MethodPost, path: "/v1/requirements/{active}:request-input", body: `{"question":"?"}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "request-input-with-no-question", method: http.MethodPost, path: "/v1/requirements/{active}:request-input", body: `{"request_id":"pin:ask-3","expected_requirement_version":{activeVersion}}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "request-input-for-a-captured-requirement", method: http.MethodPost, path: "/v1/requirements/{captured}:request-input", body: pinAskBody("pin:ask-4", "1"), token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "request-input-unknown-requirement", method: http.MethodPost, path: "/v1/requirements/no-such-requirement:request-input", body: pinAskBody("pin:ask-5", "1"), token: "runner", wantStatus: 404, wantCode: "not_found"},
		{name: "request-input-accepted", method: http.MethodPost, path: "/v1/requirements/{active}:request-input", body: pinAskBody("pin:ask-6", "{activeVersion}"), token: "runner", wantStatus: 200,
			capture: func(fx *pinFixture, doc map[string]any) {
				if v, ok := doc["version"].(float64); ok {
					fx.values["activeVersion"] = strconv.FormatInt(int64(v), 10)
				}
			}},

		// --- POST /v1/requirements/{id}:answer-input ---------------------
		{name: "answer-input-unauthenticated", method: http.MethodPost, path: "/v1/requirements/{active}:answer-input", body: `{"request_id":"pin:answer-0"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "answer-input-refused-for-the-runner", method: http.MethodPost, path: "/v1/requirements/{active}:answer-input", body: `{"request_id":"pin:answer-1"}`, token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "answer-input-empty-id", method: http.MethodPost, path: "/v1/requirements/:answer-input", body: `{"request_id":"pin:answer-2"}`, token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "answer-input-malformed-json", method: http.MethodPost, path: "/v1/requirements/{active}:answer-input", body: `{`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "answer-input-without-a-request-id", method: http.MethodPost, path: "/v1/requirements/{active}:answer-input", body: `{"option_id":"delete"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "answer-input-with-an-option-that-was-not-offered", method: http.MethodPost, path: "/v1/requirements/{active}:answer-input", body: `{"request_id":"pin:answer-3","expected_requirement_version":{activeVersion},"option_id":"never-offered"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "answer-input-unknown-requirement", method: http.MethodPost, path: "/v1/requirements/no-such-requirement:answer-input", body: `{"request_id":"pin:answer-4","expected_requirement_version":1,"option_id":"delete"}`, token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "answer-input-accepted", method: http.MethodPost, path: "/v1/requirements/{active}:answer-input", body: `{"request_id":"pin:answer-5","expected_requirement_version":{activeVersion},"option_id":"delete"}`, token: "owner", wantStatus: 200,
			capture: func(fx *pinFixture, doc map[string]any) {
				if v, ok := doc["version"].(float64); ok {
					fx.values["activeVersion"] = strconv.FormatInt(int64(v), 10)
				}
			}},

		// --- POST /v1/requirements/{id}:start-framing --------------------
		{name: "start-framing-unauthenticated", method: http.MethodPost, path: "/v1/requirements/{framing}:start-framing", body: `{"request_id":"pin:framing-0","expected_requirement_version":1}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "start-framing-refused-for-the-runner", method: http.MethodPost, path: "/v1/requirements/{framing}:start-framing", body: `{"request_id":"pin:framing-1","expected_requirement_version":1}`, token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "start-framing-empty-id", method: http.MethodPost, path: "/v1/requirements/:start-framing", body: `{"request_id":"pin:framing-2","expected_requirement_version":1}`, token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "start-framing-malformed-json", method: http.MethodPost, path: "/v1/requirements/{framing}:start-framing", body: `{`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "start-framing-without-a-request-id", method: http.MethodPost, path: "/v1/requirements/{framing}:start-framing", body: `{"expected_requirement_version":1}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "start-framing-unknown-requirement", method: http.MethodPost, path: "/v1/requirements/no-such-requirement:start-framing", body: `{"request_id":"pin:framing-3","expected_requirement_version":1}`, token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "start-framing-stale-version", method: http.MethodPost, path: "/v1/requirements/{framing}:start-framing", body: `{"request_id":"pin:framing-4","expected_requirement_version":99}`, token: "owner", wantStatus: 409, wantCode: "conflict"},
		{name: "start-framing-for-an-active-requirement", method: http.MethodPost, path: "/v1/requirements/{active}:start-framing", body: `{"request_id":"pin:framing-5","expected_requirement_version":{activeVersion}}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "start-framing-accepted", method: http.MethodPost, path: "/v1/requirements/{framing}:start-framing", body: `{"request_id":"pin:framing-6","expected_requirement_version":1}`, token: "owner", wantStatus: 200},
		{name: "start-framing-accepted-as-the-scheduler", method: http.MethodPost, path: "/v1/requirements/{captured}:start-framing", body: `{"request_id":"pin:framing-7","expected_requirement_version":1}`, token: "scheduler", wantStatus: 200},

		// --- GET /v1/controls --------------------------------------------
		{name: "list-controls", method: http.MethodGet, path: "/v1/controls", token: "owner", wantStatus: 200},
		{name: "list-controls-unauthenticated", method: http.MethodGet, path: "/v1/controls", wantStatus: 401, wantCode: "unauthorized"},
		{name: "list-controls-wrong-role", method: http.MethodGet, path: "/v1/controls", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "list-controls-page-size-is-not-an-integer", method: http.MethodGet, path: "/v1/controls?page_size=abc", token: "owner", wantStatus: 400, wantCode: "invalid_page"},
		{name: "list-controls-page-size-above-the-maximum", method: http.MethodGet, path: "/v1/controls?page_size=101", token: "owner", wantStatus: 400, wantCode: "invalid_request"},

		// --- GET /v1/queue/summary ---------------------------------------
		{name: "queue-summary", method: http.MethodGet, path: "/v1/queue/summary", token: "owner", wantStatus: 200},
		{name: "queue-summary-unauthenticated", method: http.MethodGet, path: "/v1/queue/summary", wantStatus: 401, wantCode: "unauthorized"},
		{name: "queue-summary-wrong-role", method: http.MethodGet, path: "/v1/queue/summary", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "queue-summary-rejects-post", method: http.MethodPost, path: "/v1/queue/summary", token: "owner", wantStatus: 404, wantCode: "not_found"},

		// --- GET /v1/runners ---------------------------------------------
		{name: "list-runners", method: http.MethodGet, path: "/v1/runners", token: "owner", wantStatus: 200},
		{name: "list-runners-unauthenticated", method: http.MethodGet, path: "/v1/runners", wantStatus: 401, wantCode: "unauthorized"},
		{name: "list-runners-wrong-role", method: http.MethodGet, path: "/v1/runners", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "list-runners-rejects-post", method: http.MethodPost, path: "/v1/runners", token: "owner", wantStatus: 405, wantCode: "method_not_allowed"},

		// --- GET /v1/providers -------------------------------------------
		{name: "list-providers", method: http.MethodGet, path: "/v1/providers", token: "owner", wantStatus: 200},
		{name: "list-providers-unauthenticated", method: http.MethodGet, path: "/v1/providers", wantStatus: 401, wantCode: "unauthorized"},
		{name: "list-providers-wrong-role", method: http.MethodGet, path: "/v1/providers", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "list-providers-rejects-post", method: http.MethodPost, path: "/v1/providers", token: "owner", wantStatus: 405, wantCode: "method_not_allowed"},

		// --- GET /v1/export ----------------------------------------------
		{name: "export", method: http.MethodGet, path: "/v1/export", token: "owner", wantStatus: 200},
		{name: "export-ndjson", method: http.MethodGet, path: "/v1/export?format=ndjson", token: "owner", wantStatus: 200},
		{name: "export-unauthenticated", method: http.MethodGet, path: "/v1/export", wantStatus: 401, wantCode: "unauthorized"},
		{name: "export-wrong-role", method: http.MethodGet, path: "/v1/export", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "export-limit-below-one", method: http.MethodGet, path: "/v1/export?limit=0", token: "owner", wantStatus: 400, wantCode: "invalid_limit"},
		{name: "export-limit-above-the-maximum", method: http.MethodGet, path: "/v1/export?limit=101", token: "owner", wantStatus: 400, wantCode: "invalid_limit"},
		{name: "export-unknown-format", method: http.MethodGet, path: "/v1/export?format=xml", token: "owner", wantStatus: 400, wantCode: "invalid_format"},

		// --- GET /v1/release/state ---------------------------------------
		{name: "release-state-without-an-observer", method: http.MethodGet, path: "/v1/release/state", token: "owner", wantStatus: 503, wantCode: "release_observer_not_configured"},
		{name: "release-state-unauthenticated", method: http.MethodGet, path: "/v1/release/state", wantStatus: 401, wantCode: "unauthorized"},
		{name: "release-state-wrong-role", method: http.MethodGet, path: "/v1/release/state", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "release-state-rejects-post", method: http.MethodPost, path: "/v1/release/state", token: "owner", wantStatus: 405, wantCode: "method_not_allowed"},

		// --- the five /v1/repositories routes ----------------------------
		{name: "list-repositories", method: http.MethodGet, path: "/v1/repositories", token: "owner", wantStatus: 200},
		{name: "list-repositories-unauthenticated", method: http.MethodGet, path: "/v1/repositories", wantStatus: 401, wantCode: "unauthorized"},
		{name: "list-repositories-wrong-role", method: http.MethodGet, path: "/v1/repositories", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "register-repository", method: http.MethodPost, path: "/v1/repositories", body: `{"request_id":"pin:register-1","source_url":"https://github.com/pin/registered"}`, token: "owner", wantStatus: 201},
		{name: "register-repository-unauthenticated", method: http.MethodPost, path: "/v1/repositories", body: `{"request_id":"pin:register-2","source_url":"https://github.com/pin/x"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "register-repository-wrong-role", method: http.MethodPost, path: "/v1/repositories", body: `{"request_id":"pin:register-3","source_url":"https://github.com/pin/x"}`, token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "register-repository-malformed-json", method: http.MethodPost, path: "/v1/repositories", body: `{`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "register-repository-without-a-request-id", method: http.MethodPost, path: "/v1/repositories", body: `{"source_url":"https://github.com/pin/y"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "register-repository-with-an-unparsable-source-url", method: http.MethodPost, path: "/v1/repositories", body: `{"request_id":"pin:register-4","source_url":"not a locator"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "register-repository-duplicate-locator", method: http.MethodPost, path: "/v1/repositories", body: `{"request_id":"pin:register-5","source_url":"https://github.com/pin/observed"}`, token: "owner", wantStatus: 409, wantCode: "conflict"},
		{name: "repository-detail", method: http.MethodGet, path: "/v1/repositories/repo-pin", token: "owner", wantStatus: 200},
		{name: "repository-detail-unknown-id", method: http.MethodGet, path: "/v1/repositories/no-such-repository", token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "repository-detail-unauthenticated", method: http.MethodGet, path: "/v1/repositories/repo-pin", wantStatus: 401, wantCode: "unauthorized"},
		{name: "repository-detail-wrong-role", method: http.MethodGet, path: "/v1/repositories/repo-pin", token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "repository-detail-empty-id", method: http.MethodGet, path: "/v1/repositories/", token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "repository-verb-is-not-readable", method: http.MethodGet, path: "/v1/repositories/repo-pin:retire", token: "owner", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "observe-repository-refused-for-the-owner", method: http.MethodPost, path: "/v1/repositories/repo-pin:observe", body: `{"request_id":"pin:observe-1","reachable":true}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "observe-repository-unauthenticated", method: http.MethodPost, path: "/v1/repositories/repo-pin:observe", body: `{"request_id":"pin:observe-2","reachable":true}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "observe-repository-without-a-request-id", method: http.MethodPost, path: "/v1/repositories/repo-pin:observe", body: `{"reachable":true}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "observe-repository-malformed-json", method: http.MethodPost, path: "/v1/repositories/repo-pin:observe", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "observe-repository-unknown-repository", method: http.MethodPost, path: "/v1/repositories/no-such-repository:observe", body: `{"request_id":"pin:observe-3","reachable":true}`, token: "runner", wantStatus: 404, wantCode: "not_found"},
		{name: "observe-repository-accepted", method: http.MethodPost, path: "/v1/repositories/repo-pin:observe", body: `{"request_id":"pin:observe-4","reachable":true,"default_branch":"main","can_push":true,"adapter_version":"pin"}`, token: "runner", wantStatus: 200},
		{name: "retire-repository-refused-for-the-runner", method: http.MethodPost, path: "/v1/repositories/repo-retire:retire", body: `{"request_id":"pin:retire-1"}`, token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "retire-repository-unauthenticated", method: http.MethodPost, path: "/v1/repositories/repo-retire:retire", body: `{"request_id":"pin:retire-2"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "retire-repository-without-a-request-id", method: http.MethodPost, path: "/v1/repositories/repo-retire:retire", body: `{}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "retire-repository-unknown-repository", method: http.MethodPost, path: "/v1/repositories/no-such-repository:retire", body: `{"request_id":"pin:retire-3"}`, token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "retire-repository-accepted", method: http.MethodPost, path: "/v1/repositories/repo-retire:retire", body: `{"request_id":"pin:retire-4"}`, token: "owner", wantStatus: 200},
		{name: "repository-unknown-verb", method: http.MethodPost, path: "/v1/repositories/repo-pin:frobnicate", body: `{"request_id":"pin:verb"}`, token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "repository-empty-id-with-a-verb", method: http.MethodPost, path: "/v1/repositories/:retire", body: `{"request_id":"pin:verb-2"}`, token: "owner", wantStatus: 404, wantCode: "not_found"},

		// --- the runner-facing routes, refusals first --------------------
		{name: "claim-unauthenticated", method: http.MethodPost, path: "/v1/runner/claims:acquire", body: `{"request_id":"pin:claim-0","increment_id":"{increment}","expected_increment_version":{incrementVersion},` + pinTarget + `}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "claim-refused-for-the-owner", method: http.MethodPost, path: "/v1/runner/claims:acquire", body: `{"request_id":"pin:claim-1","increment_id":"{increment}","expected_increment_version":{incrementVersion},` + pinTarget + `}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "claim-malformed-json", method: http.MethodPost, path: "/v1/runner/claims:acquire", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "claim-without-a-request-id", method: http.MethodPost, path: "/v1/runner/claims:acquire", body: `{"increment_id":"{increment}"}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "claim-unknown-increment", method: http.MethodPost, path: "/v1/runner/claims:acquire", body: `{"request_id":"pin:claim-2","increment_id":"no-such-increment","expected_increment_version":1,` + pinTarget + `}`, token: "runner", wantStatus: 404, wantCode: "not_found"},
		{name: "permit-unauthenticated", method: http.MethodPost, path: "/v1/runner/permits:check", body: `{"request_id":"pin:permit-0","kind":"process",` + pinTarget + `}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "permit-refused-for-the-owner", method: http.MethodPost, path: "/v1/runner/permits:check", body: `{"request_id":"pin:permit-1","kind":"process",` + pinTarget + `}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "permit-malformed-json", method: http.MethodPost, path: "/v1/runner/permits:check", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "permit-without-a-request-id", method: http.MethodPost, path: "/v1/runner/permits:check", body: `{"kind":"process",` + pinTarget + `}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "start-execution-unauthenticated", method: http.MethodPost, path: "/v1/executions/no-such-execution:start", body: `{"request_id":"pin:start-0","expected_execution_version":1}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "start-execution-refused-for-the-owner", method: http.MethodPost, path: "/v1/executions/no-such-execution:start", body: `{"request_id":"pin:start-1","expected_execution_version":1}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "start-execution-malformed-json", method: http.MethodPost, path: "/v1/executions/no-such-execution:start", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "start-execution-without-a-request-id", method: http.MethodPost, path: "/v1/executions/no-such-execution:start", body: `{"expected_execution_version":1}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "start-execution-unknown-provider-name", method: http.MethodPost, path: "/v1/executions/no-such-execution:start", body: `{"request_id":"pin:start-2","expected_execution_version":1,"provider":"not-a-provider"}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "start-execution-unknown-execution", method: http.MethodPost, path: "/v1/executions/no-such-execution:start", body: `{"request_id":"pin:start-3","expected_execution_version":1}`, token: "runner", wantStatus: 404, wantCode: "not_found"},
		{name: "renew-unauthenticated", method: http.MethodPost, path: "/v1/leases/no-such-lease:renew", body: `{"request_id":"pin:renew-0","expected_lease_version":1}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "renew-refused-for-the-owner", method: http.MethodPost, path: "/v1/leases/no-such-lease:renew", body: `{"request_id":"pin:renew-1","expected_lease_version":1}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "renew-malformed-json", method: http.MethodPost, path: "/v1/leases/no-such-lease:renew", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "renew-without-a-request-id", method: http.MethodPost, path: "/v1/leases/no-such-lease:renew", body: `{"expected_lease_version":1}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "renew-unknown-lease", method: http.MethodPost, path: "/v1/leases/no-such-lease:renew", body: `{"request_id":"pin:renew-2","expected_lease_version":1}`, token: "runner", wantStatus: 404, wantCode: "not_found"},
		{name: "heartbeat-unauthenticated", method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{"request_id":"pin:beat-0"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "heartbeat-refused-for-the-owner", method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{"request_id":"pin:beat-1"}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "heartbeat-malformed-json", method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "heartbeat-without-a-request-id", method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "heartbeat-with-an-incomplete-runner-version-report", method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{"request_id":"pin:beat-2","runner_version":{"version":"","binary_sha256":"","schema_min":0,"schema_max":0}}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "checkpoint-unauthenticated", method: http.MethodPost, path: "/v1/runner/checkpoints", body: `{"request_id":"pin:checkpoint-0","execution_id":"no-such-execution","lease_id":"no-such-lease"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "checkpoint-refused-for-the-owner", method: http.MethodPost, path: "/v1/runner/checkpoints", body: `{"request_id":"pin:checkpoint-1","execution_id":"no-such-execution","lease_id":"no-such-lease"}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "checkpoint-malformed-json", method: http.MethodPost, path: "/v1/runner/checkpoints", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "checkpoint-without-a-request-id", method: http.MethodPost, path: "/v1/runner/checkpoints", body: `{"execution_id":"no-such-execution"}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "checkpoint-unknown-execution", method: http.MethodPost, path: "/v1/runner/checkpoints", body: `{"request_id":"pin:checkpoint-2","execution_id":"no-such-execution","lease_id":"no-such-lease"}`, token: "runner", wantStatus: 404, wantCode: "not_found"},
		{name: "result-unauthenticated", method: http.MethodPost, path: "/v1/executions/result", body: `{"request_id":"pin:result-0","execution_id":"no-such-execution","lease_id":"no-such-lease"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "result-refused-for-the-owner", method: http.MethodPost, path: "/v1/executions/result", body: `{"request_id":"pin:result-1","execution_id":"no-such-execution","lease_id":"no-such-lease"}`, token: "owner", wantStatus: 403, wantCode: "forbidden"},
		{name: "result-malformed-json", method: http.MethodPost, path: "/v1/executions/result", body: `{`, token: "runner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "result-without-a-request-id", method: http.MethodPost, path: "/v1/executions/result", body: `{"execution_id":"no-such-execution"}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "result-with-an-unknown-provider-name", method: http.MethodPost, path: "/v1/executions/result", body: `{"request_id":"pin:result-2","execution_id":"no-such-execution","lease_id":"no-such-lease","provider_observation":{"name":"not-a-provider"}}`, token: "runner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "result-unknown-execution", method: http.MethodPost, path: "/v1/executions/result", body: `{"request_id":"pin:result-3","execution_id":"no-such-execution","lease_id":"no-such-lease","expected_execution_version":1,"succeeded":true,` + pinTarget + `}`, token: "runner", wantStatus: 404, wantCode: "not_found"},

		// --- the runner-facing journey, all inside one UTC day ----------
		{name: "claim-accepted", method: http.MethodPost, path: "/v1/runner/claims:acquire", body: `{"request_id":"pin:claim-accepted","increment_id":"{increment}","expected_increment_version":{incrementVersion},` + pinTarget + `}`, token: "runner", wantStatus: 200,
			capture: func(fx *pinFixture, doc map[string]any) {
				fx.values["execution"], _ = doc["execution_id"].(string)
				fx.values["lease"], _ = doc["lease_id"].(string)
				if v, ok := doc["fencing_token"].(float64); ok {
					fx.values["fence"] = strconv.FormatInt(int64(v), 10)
				}
			}},
		{name: "permit-process-accepted", sameDay: true, method: http.MethodPost, path: "/v1/runner/permits:check", body: `{"request_id":"pin:permit-process","kind":"process",` + pinTarget + `,"fencing_token":{fence},"expected_fencing_token":{fence},"resource":"{execution}"}`, token: "runner", wantStatus: 200},
		{name: "start-execution-accepted", sameDay: true, method: http.MethodPost, path: "/v1/executions/{execution}:start", body: `{"request_id":"pin:start-accepted","expected_execution_version":1,"provider":"claude"}`, token: "runner", wantStatus: 200,
			capture: func(fx *pinFixture, doc map[string]any) {
				if v, ok := doc["version"].(float64); ok {
					fx.values["executionVersion"] = strconv.FormatInt(int64(v), 10)
				}
			}},
		{name: "renew-accepted", sameDay: true, method: http.MethodPost, path: "/v1/leases/{lease}:renew", body: `{"request_id":"pin:renew-accepted","expected_lease_version":1,"fencing_token":{fence}}`, token: "runner", wantStatus: 200},
		{name: "heartbeat-accepted", sameDay: true, method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{"request_id":"pin:beat-accepted"}`, token: "runner", wantStatus: 200},
		{name: "heartbeat-accepted-with-a-complete-runner-version-report", sameDay: true, method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{"request_id":"pin:beat-accepted-2","runner_version":{"version":"1.2.3","binary_sha256":"0000000000000000000000000000000000000000000000000000000000000000","schema_min":1,"schema_max":1}}`, token: "runner", wantStatus: 200},
		{name: "checkpoint-accepted", sameDay: true, method: http.MethodPost, path: "/v1/runner/checkpoints", body: `{"request_id":"pin:checkpoint-accepted","execution_id":"{execution}","lease_id":"{lease}","fencing_token":{fence}}`, token: "runner", wantStatus: 200},
		{name: "permit-external-effect-accepted", sameDay: true, method: http.MethodPost, path: "/v1/runner/permits:check", body: `{"request_id":"pin:permit-effect","kind":"external-effect",` + pinTarget + `,"fencing_token":{fence},"expected_fencing_token":{fence},"resource":"{execution}"}`, token: "runner", wantStatus: 200},
		{name: "result-accepted", sameDay: true, method: http.MethodPost, path: "/v1/executions/result", body: `{"request_id":"pin:result-accepted","execution_id":"{execution}","lease_id":"{lease}","expected_execution_version":{executionVersion},"fencing_token":{fence},"succeeded":true,` + pinTarget + `,"provider_observation":{"name":"claude"}}`, token: "runner", wantStatus: 200},

		// --- POST /v1/controls, after the journey ------------------------
		{name: "control-unauthenticated", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-0","scope_kind":"channel","scope_value":"pin","mode":"allow"}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "control-wrong-role", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-1","scope_kind":"channel","scope_value":"pin","mode":"allow"}`, token: "runner", wantStatus: 403, wantCode: "forbidden"},
		{name: "control-malformed-json", method: http.MethodPost, path: "/v1/controls", body: `{`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "control-unknown-field", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-2","scope_kind":"channel","scope_value":"pin","mode":"allow","extra":1}`, token: "owner", wantStatus: 400, wantCode: "invalid_json"},
		{name: "control-without-a-request-id", method: http.MethodPost, path: "/v1/controls", body: `{"scope_kind":"channel","scope_value":"pin","mode":"allow"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "control-unknown-mode", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-3","scope_kind":"channel","scope_value":"pin","mode":"not-a-mode"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "control-unknown-scope-kind", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-4","scope_kind":"not-a-scope","scope_value":"pin","mode":"allow"}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "control-with-a-negative-allocation-limit", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-5","scope_kind":"installation","scope_value":"` + pinInstallation + `","mode":"allow","allocation_limit":{"installation_concurrent_executions":-1}}`, token: "owner", wantStatus: 400, wantCode: "invalid_request"},
		{name: "control-accepted", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-6","scope_kind":"channel","scope_value":"pin","mode":"allow"}`, token: "owner", wantStatus: 200},
		{name: "control-accepted-with-an-allocation-limit", method: http.MethodPost, path: "/v1/controls", body: `{"request_id":"pin:control-7","scope_kind":"installation","scope_value":"` + pinInstallation + `","mode":"allow","allocation_limit":{"installation_concurrent_executions":3}}`, token: "owner", wantStatus: 200},

		// --- routing: what is neither a route nor a method --------------
		{name: "unknown-route-unauthenticated", method: http.MethodPost, path: "/v1/nothing-here", body: `{}`, wantStatus: 401, wantCode: "unauthorized"},
		{name: "unknown-route-post", method: http.MethodPost, path: "/v1/nothing-here", body: `{}`, token: "owner", wantStatus: 404, wantCode: "not_found"},
		{name: "unknown-route-get", method: http.MethodGet, path: "/v1/nothing-here", token: "owner", wantStatus: 405, wantCode: "method_not_allowed"},
		{name: "root-path", method: http.MethodGet, path: "/", token: "owner", wantStatus: 405, wantCode: "method_not_allowed"},

		// --- the quota answer, last: it fills the day's write budget ----
		{name: "capture-when-the-daily-write-budget-is-exhausted", method: http.MethodPost, path: "/v1/requirements", body: `{"request_id":"pin:quota","text":"x"}`, token: "owner", exhaustQuota: true, wantStatus: 429, wantCode: "quota_exhausted"},
	}
}

// TestEveryRouteStillAnswersTheStatusItAnsweredBefore is V2-083 A3 and A6.
//
// It is measured GREEN on the unedited tree first and recorded as the
// baseline, then re-measured after domainError's default is inverted. Every
// row must answer identically both times. A row that moves is not a row to
// adjust: it means a status code the caller already depends on has changed,
// which this task forbids itself.
func TestEveryRouteStillAnswersTheStatusItAnsweredBefore(t *testing.T) {
	fx := newPinFixture(t)
	for _, row := range pinRows() {
		row := row
		t.Run(row.name, func(t *testing.T) {
			h := fx.handler
			if row.unconfigured {
				h = fx.unconfigured
			}
			if !row.sameDay {
				fx.clock.nextDay()
			}
			if row.exhaustQuota {
				if err := fx.store.Transact(context.Background(), func(u application.UnitOfWork) error {
					return u.ReserveQuota(context.Background(), "pin-fill", fx.clock.Now(), quota.Usage{Writes: quota.DefaultBudget.Writes})
				}); err != nil {
					t.Fatal(err)
				}
			}
			w := pinCall(h, row, fx.expand(row.path), fx.expand(row.body))
			code := ""
			var doc map[string]any
			if json.Unmarshal(w.Body.Bytes(), &doc) == nil {
				code, _ = doc["error"].(string)
			}
			t.Logf("PIN\t%s\t%s\t%s\t%d\t%s", row.name, row.method, row.path, w.Code, code)
			// A14's message-neutrality measurement: the whole body, not just
			// the code. Every row is logged so the after run can be compared
			// byte for byte against the recorded baseline.
			t.Logf("PINBODY\t%s\t%s", row.name, strings.ReplaceAll(w.Body.String(), "\n", "\\n"))
			if got := w.Header().Get("X-Correlation-ID"); got != pinCorrelationID {
				t.Errorf("X-Correlation-ID = %q, want %q", got, pinCorrelationID)
			}
			if w.Code != row.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", w.Code, row.wantStatus, w.Body.String())
			}
			if code != row.wantCode {
				t.Errorf("error code = %q, want %q (body %s)", code, row.wantCode, w.Body.String())
			}
			if row.capture != nil && w.Code == row.wantStatus {
				row.capture(fx, doc)
			}
		})
	}
}

// ===========================================================================
// V2-083 A7: the repair, asserted per route and in both fault shapes.
// ===========================================================================

// injectedStoreFaultText is storage-shaped text copied from V2-079's
// measurement: a DOUBLED Firestore parent name carrying the installation key
// and the collection segments, plus a byte offset. It is carried by a plain
// errors.New -- no gRPC status package is imported, because internal/ci
// derives the component manifest from imports and a new edge would turn make
// check red for a reason unrelated to this repair.
const injectedStoreFaultText = "rpc error: code = InvalidArgument desc = Invalid StartAfter value: " +
	"projects/agentic-loop-local/databases/(default)/documents/installations/install-7f3a/requirements/" +
	"projects/agentic-loop-local/databases/(default)/documents/installations/install-7f3a/requirements/" +
	"cmVxdWlyZW1lbnQtYg at byte offset 118"

// injectedStoreFaultFragments are the parts of that text no response may
// contain. Matching text here is the OPPOSITE of classifying by text: the
// test matches to prove the text is GONE.
var injectedStoreFaultFragments = []string{
	injectedStoreFaultText,
	"InvalidArgument",
	"StartAfter",
	"projects/agentic-loop-local",
	"install-7f3a",
	"byte offset",
	"cmVxdWlyZW1lbnQtYg",
	"rpc error",
}

// faultTransactor is a test-local decorator over the application.Transactor
// port, in the two shapes a real store fault takes.
type faultTransactor struct {
	inner         application.Transactor
	fault         error
	armed         bool
	afterCallback bool
}

func (f *faultTransactor) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	if !f.armed {
		return f.inner.Transact(ctx, fn)
	}
	if !f.afterCallback {
		// Variant A: the fault arrives INSTEAD of the callback, which is the
		// shape a refused query or a failed transaction start takes. Nothing
		// is read and nothing is staged.
		return f.fault
	}
	// Variant B: the commit-time shape. The callback runs and the transaction
	// then fails. Whatever the callback itself returned is deliberately
	// discarded -- a commit that fails fails -- because the property under
	// test is that no handler can answer 2xx while Transact returns non-nil.
	_ = f.inner.Transact(ctx, fn)
	return f.fault
}

type faultFixture struct {
	handler http.Handler
	tx      *faultTransactor
	clock   *framingClock
	values  map[string]string
}

// faultReconcileIdentity is the scheduler identity the /internal/reconcile
// branch asserts through the IAP header.
const faultReconcileIdentity = "reconciler@example.com"

func (fx *faultFixture) expand(s string) string {
	for k, v := range fx.values {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// newFaultFixture seeds real state with the decorator DISARMED, then arms it.
func newFaultFixture(t *testing.T, afterCallback bool) *faultFixture {
	t.Helper()
	st := memory.New()
	tx := &faultTransactor{inner: st, fault: errors.New(injectedStoreFaultText), afterCallback: afterCallback}
	clk := &framingClock{at: time.Unix(1700000000, 0).UTC()}
	svc, err := application.NewServiceWithConfig(tx, clk, &ids{}, application.ServiceConfig{InstallationID: pinInstallation, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := runner.NewService(runner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	auth := api.BearerAuthenticator{
		"owner":     {Role: application.RoleOwner, Subject: "owner"},
		"runner":    {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"},
		"scheduler": {Role: application.RoleScheduler, Subject: "scheduler.self"},
	}
	// The /internal/reconcile branch reaches domainError through this hook
	// rather than through the Transactor, so the same fault is injected here:
	// a reconciliation the caller did not perform failing is the clearest case
	// of an error the request did not cause, and it answered 400
	// invalid_request before this change.
	h := api.New(api.Config{
		Authenticator: auth, Service: svc, RunnerEnrollment: enrollment,
		AllowedOrigins:    []string{"https://console.example"},
		InternalReconcile: func(context.Context) error { return tx.fault },
		ReconcileIdentity: faultReconcileIdentity,
	})
	fx := &faultFixture{handler: h, tx: tx, clock: clk, values: map[string]string{}}

	for _, seed := range []struct{ path, body string }{
		{"/v1/requirements", `{"request_id":"fault:capture-1","requirement_id":"fault-captured","text":"seed"}`},
		{"/v1/requirements", `{"request_id":"fault:capture-2","requirement_id":"fault-active","text":"seed"}`},
		{"/v1/repositories", `{"request_id":"fault:repo","repository_id":"repo-fault","source_url":"https://github.com/fault/seed"}`},
	} {
		clk.nextDay()
		if w := pinCall(h, pinRow{method: http.MethodPost, token: "owner"}, seed.path, seed.body); w.Code != http.StatusCreated {
			t.Fatalf("seeding %s: %d %s", seed.path, w.Code, w.Body.String())
		}
	}
	clk.nextDay()
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		r, ok, e := u.Requirement(context.Background(), "fault-active")
		if e != nil || !ok {
			t.Fatalf("seed active: ok=%v err=%v", ok, e)
		}
		next := r
		next.Status = domain.RequirementActive
		next.Version++
		return u.SaveRequirement(context.Background(), next, r.Version)
	}); err != nil {
		t.Fatal(err)
	}
	fx.values["captured"] = "fault-captured"
	fx.values["active"] = "fault-active"
	fx.values["repository"] = "repo-fault"
	tx.armed = true
	return fx
}

// faultRoutes is every route that reaches h.domainError through the
// application Service, and therefore every route a store fault can surface
// on. Four routes reach domainError without ever opening a transaction and
// are named here rather than silently omitted: /healthz and /owner/assets/
// never call domainError at all; the three /v1/runner/enrollment routes reach
// internal/runner's own store and answer their own codes; POST
// /internal/reconcile reaches domainError through the injected reconcile hook,
// which is measured separately (A10); and GET /v1/release/state reads the
// release observer and opens no transaction, so its only domainError path is
// the already-classified 401/403 one.
type faultRoute struct {
	name    string
	method  string
	path    string
	body    string
	token   string
	headers map[string]string
}

func faultRoutes(variant string) []faultRoute {
	rid := func(s string) string { return `"request_id":"fault:` + variant + ":" + s + `"` }
	target := `"target":{"installation_id":"` + pinInstallation + `","requirement_id":"{active}","increment_id":"increment-x"}`
	return []faultRoute{
		{name: "internal-reconcile", method: http.MethodPost, path: "/internal/reconcile",
			headers: map[string]string{"X-Goog-Authenticated-User-Email": "accounts.google.com:" + faultReconcileIdentity}},
		{name: "list-requirements", method: http.MethodGet, path: "/v1/requirements", token: "owner"},
		{name: "capture", method: http.MethodPost, path: "/v1/requirements", body: `{` + rid("capture") + `,"text":"a requirement"}`, token: "owner"},
		{name: "requirement-detail", method: http.MethodGet, path: "/v1/requirements/{captured}", body: "", token: "owner"},
		{name: "request-input", method: http.MethodPost, path: "/v1/requirements/{active}:request-input", body: pinAskBody("fault:"+variant+":ask", "2"), token: "runner"},
		{name: "answer-input", method: http.MethodPost, path: "/v1/requirements/{active}:answer-input", body: `{` + rid("answer") + `,"expected_requirement_version":2,"option_id":"delete"}`, token: "owner"},
		{name: "start-framing", method: http.MethodPost, path: "/v1/requirements/{captured}:start-framing", body: `{` + rid("framing") + `,"expected_requirement_version":1}`, token: "owner"},
		{name: "list-controls", method: http.MethodGet, path: "/v1/controls", body: "", token: "owner"},
		{name: "control", method: http.MethodPost, path: "/v1/controls", body: `{` + rid("control") + `,"scope_kind":"channel","scope_value":"fault","mode":"allow"}`, token: "owner"},
		{name: "queue-summary", method: http.MethodGet, path: "/v1/queue/summary", body: "", token: "owner"},
		{name: "list-runners", method: http.MethodGet, path: "/v1/runners", body: "", token: "owner"},
		{name: "list-providers", method: http.MethodGet, path: "/v1/providers", body: "", token: "owner"},
		{name: "export", method: http.MethodGet, path: "/v1/export", body: "", token: "owner"},
		{name: "claim", method: http.MethodPost, path: "/v1/runner/claims:acquire", body: `{` + rid("claim") + `,"increment_id":"increment-x","expected_increment_version":1,` + target + `}`, token: "runner"},
		{name: "permit", method: http.MethodPost, path: "/v1/runner/permits:check", body: `{` + rid("permit") + `,"kind":"process",` + target + `,"resource":"execution-x"}`, token: "runner"},
		{name: "result", method: http.MethodPost, path: "/v1/executions/result", body: `{` + rid("result") + `,"execution_id":"execution-x","lease_id":"lease-x","expected_execution_version":1,"succeeded":true,` + target + `}`, token: "runner"},
		{name: "start-execution", method: http.MethodPost, path: "/v1/executions/execution-x:start", body: `{` + rid("start") + `,"expected_execution_version":1}`, token: "runner"},
		{name: "renew", method: http.MethodPost, path: "/v1/leases/lease-x:renew", body: `{` + rid("renew") + `,"expected_lease_version":1}`, token: "runner"},
		{name: "heartbeat", method: http.MethodPost, path: "/v1/runner/heartbeat", body: `{` + rid("heartbeat") + `}`, token: "runner"},
		{name: "checkpoint", method: http.MethodPost, path: "/v1/runner/checkpoints", body: `{` + rid("checkpoint") + `,"execution_id":"execution-x","lease_id":"lease-x"}`, token: "runner"},
		{name: "list-repositories", method: http.MethodGet, path: "/v1/repositories", body: "", token: "owner"},
		{name: "register-repository", method: http.MethodPost, path: "/v1/repositories", body: `{` + rid("register") + `,"source_url":"https://github.com/fault/new"}`, token: "owner"},
		{name: "repository-detail", method: http.MethodGet, path: "/v1/repositories/{repository}", body: "", token: "owner"},
		{name: "retire-repository", method: http.MethodPost, path: "/v1/repositories/{repository}:retire", body: `{` + rid("retire") + `}`, token: "owner"},
		{name: "observe-repository", method: http.MethodPost, path: "/v1/repositories/{repository}:observe", body: `{` + rid("observe") + `,"reachable":true}`, token: "runner"},
	}
}

// TestAStorageFaultIsNotReportedAsTheCallersMistake is V2-083 A7.
func TestAStorageFaultIsNotReportedAsTheCallersMistake(t *testing.T) {
	for _, variant := range []struct {
		name          string
		afterCallback bool
	}{
		{"the-fault-arrives-instead-of-the-callback", false},
		{"the-fault-arrives-after-the-callback-ran-which-is-the-commit-time-shape", true},
	} {
		variant := variant
		t.Run(variant.name, func(t *testing.T) {
			fx := newFaultFixture(t, variant.afterCallback)
			for _, route := range faultRoutes(variant.name) {
				route := route
				t.Run(route.name, func(t *testing.T) {
					fx.clock.nextDay()
					w := pinCall(fx.handler, pinRow{method: route.method, token: route.token, headers: route.headers}, fx.expand(route.path), fx.expand(route.body))
					body := w.Body.String()
					t.Logf("FAULT\t%s\t%s\t%s %s\t%d", variant.name, route.name, route.method, route.path, w.Code)

					if w.Code != http.StatusInternalServerError {
						t.Fatalf("status = %d, want 500: %s", w.Code, body)
					}
					if w.Code == http.StatusBadRequest {
						t.Fatalf("a fault the request did not cause was reported as the caller's mistake: %s", body)
					}
					if w.Code >= 200 && w.Code < 300 {
						t.Fatalf("an unclassified failure became a success: %d %s", w.Code, body)
					}
					doc := decodeBody(t, w.Body.Bytes())
					if doc["error"] != "internal_error" {
						t.Fatalf("error code = %v, want internal_error: %s", doc["error"], body)
					}
					if doc["message"] != "internal server error" {
						t.Fatalf("message = %v, want the constant \"internal server error\": %s", doc["message"], body)
					}
					for _, fragment := range injectedStoreFaultFragments {
						if strings.Contains(body, fragment) {
							t.Fatalf("the response leaks part of the injected store fault (%q): %s", fragment, body)
						}
					}
					if len(doc) != 4 {
						t.Fatalf("body carries %d keys, want exactly 4: %s", len(doc), body)
					}
					for key := range doc {
						switch key {
						case "error", "message", "schema_version", "correlation_id":
						default:
							t.Fatalf("unexpected response field %q: %s", key, body)
						}
					}
					if got := w.Header().Get("X-Correlation-ID"); got == "" {
						t.Fatal("the X-Correlation-ID header is no longer set")
					}
				})
			}
		})
	}
}
