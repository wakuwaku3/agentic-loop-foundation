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
