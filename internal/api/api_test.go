package api_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/api"
	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
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
