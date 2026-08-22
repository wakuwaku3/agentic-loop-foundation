package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/api"
	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
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
	return api.New(api.Config{Authenticator: auth, Service: svc, AllowedOrigins: []string{"https://console.example"}})
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
		if key != "requirement_id" && key != "version" {
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
