// Package api is the transport boundary for the v2 control plane.
package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
	"github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	"github.com/takushi/agentic-loop-foundation/v2/internal/web"
)

const maxJSON = 1 << 20

type Authenticator interface {
	Authenticate(*http.Request) (application.Caller, error)
}
type Config struct {
	Authenticator     Authenticator
	Service           *application.Service
	RunnerEnrollment  *runner.Service
	AllowedOrigins    []string
	InternalReconcile func(context.Context) error
	ReconcileIdentity string
}
type Handler struct {
	config Config
	mux    http.Handler
}

func New(cfg Config) *Handler {
	h := &Handler{config: cfg}
	h.mux = http.HandlerFunc(h.route)
	return h
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			h.error(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
		}
	}()
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		cid = correlationID()
	}
	w.Header().Set("X-Correlation-ID", cid)
	w.Header().Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey{}, cid)))
}

type correlationKey struct{}

func CorrelationID(ctx context.Context) string {
	v, _ := ctx.Value(correlationKey{}).(string)
	return v
}
func correlationID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return time.Now().UTC().Format("20060102T150405.000000000Z")
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			h.error(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "schema_version": "v1"})
		return
	}
	if r.URL.Path == "/internal/reconcile" && r.Method == http.MethodPost {
		asserted := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Goog-Authenticated-User-Email")))
		if h.config.InternalReconcile == nil || h.config.ReconcileIdentity == "" || r.Header.Get("X-Agentic-Runner-Session") != "" || asserted != "accounts.google.com:"+strings.ToLower(strings.TrimSpace(h.config.ReconcileIdentity)) || strings.ContainsAny(asserted, " \t\r\n") {
			h.error(w, r, http.StatusUnauthorized, "unauthorized", "reconcile scheduler identity required")
			return
		}
		if err := h.config.InternalReconcile(r.Context()); err != nil {
			h.domainError(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "reconciliation_requested"})
		return
	}
	if r.URL.Path == "/v1/runner/enrollment/challenge" && r.Method == http.MethodPost {
		if !h.enrollmentTransportAuth(r) {
			h.error(w, r, http.StatusUnauthorized, "unauthorized", "IAP owner authentication required")
			return
		}
		h.enrollmentChallenge(w, r)
		return
	}
	if r.URL.Path == "/v1/runner/enrollment/complete" && r.Method == http.MethodPost {
		if !h.enrollmentTransportAuth(r) {
			h.error(w, r, http.StatusUnauthorized, "unauthorized", "IAP owner authentication required")
			return
		}
		h.enrollmentComplete(w, r)
		return
	}
	if h.config.Authenticator == nil || h.config.Service == nil {
		h.error(w, r, http.StatusServiceUnavailable, "not_configured", "authenticated API is not configured")
		return
	}
	if r.URL.Path == "/v1/runner/enrollment" && r.Method == http.MethodPost {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, http.StatusUnauthorized, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, http.StatusForbidden, "forbidden", "owner role required")
			return
		}
		h.issueEnrollment(w, r)
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/owner" || r.URL.Path == "/owner/") {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.ownerUI(w, r)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/owner/assets/") {
		h.ownerAsset(w, r, strings.TrimPrefix(r.URL.Path, "/owner/assets/"))
		return
	}
	if r.URL.Path == "/v1/requirements" && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.listRequirements(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)))
		return
	}
	if r.URL.Path == "/v1/controls" && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.listControls(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/requirements/") && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/requirements/")
		h.getRequirement(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)), id)
		return
	}
	if r.URL.Path == repositoriesPath && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.listRepositories(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)))
		return
	}
	if strings.HasPrefix(r.URL.Path, repositoriesPrefix) && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		id, verb := repositoryVerb(r.URL.Path)
		// :retire and :observe are POST-only verbs; reading them is a method
		// error rather than a missing repository.
		if verb != "" {
			h.error(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if id == "" {
			h.error(w, r, http.StatusNotFound, "not_found", "route not found")
			return
		}
		h.getRepository(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)), id)
		return
	}
	if r.URL.Path == "/v1/queue/summary" && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.queueSummary(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)))
		return
	}
	if r.URL.Path == "/v1/export" && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.export(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)))
		return
	}
	// GET /v1/release/state is the only release route: this surface is
	// read-only. There is deliberately no promote, no rollback and no
	// SetPreview route. The method check comes first, following the /healthz
	// idiom, so a POST to this path is a method error rather than a 404.
	if r.URL.Path == releaseStatePath {
		if r.Method != http.MethodGet {
			h.error(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.releaseState(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/requirements/") && r.Method == http.MethodGet {
		caller, err := h.config.Authenticator.Authenticate(r)
		if err != nil {
			h.error(w, r, 401, "unauthorized", "authentication failed")
			return
		}
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/requirements/")
		h.getRequirement(w, r.WithContext(application.ContextWithCaller(r.Context(), caller)), id)
		return
	}
	if r.Method != http.MethodPost {
		h.error(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	caller, err := h.config.Authenticator.Authenticate(r)
	if err != nil {
		h.error(w, r, http.StatusUnauthorized, "unauthorized", "authentication failed")
		return
	}
	if err = h.originAllowed(r, caller); err != nil {
		h.error(w, r, http.StatusForbidden, "origin_denied", err.Error())
		return
	}
	ctx := application.ContextWithCaller(r.Context(), caller)
	switch r.URL.Path {
	case "/v1/requirements":
		h.capture(w, r.WithContext(ctx))
	case repositoriesPath:
		if caller.Role != application.RoleOwner {
			h.error(w, r, 403, "forbidden", "owner role required")
			return
		}
		h.registerRepository(w, r.WithContext(ctx))
	case "/v1/controls":
		h.control(w, r.WithContext(ctx))
	case "/v1/runner/claims:acquire":
		h.claim(w, r.WithContext(ctx))
	case "/v1/runner/permits:check":
		if caller.Role != application.RoleRunner {
			h.error(w, r, 403, "forbidden", "runner role required")
			return
		}
		h.permit(w, r.WithContext(ctx))
	case "/v1/executions/result":
		h.result(w, r.WithContext(ctx))
	case "/v1/runner/heartbeat":
		h.heartbeat(w, r.WithContext(ctx))
	case "/v1/runner/checkpoints":
		h.checkpoint(w, r.WithContext(ctx))
	default:
		if strings.HasPrefix(r.URL.Path, repositoriesPrefix) {
			id, verb := repositoryVerb(r.URL.Path)
			if id == "" {
				h.error(w, r, http.StatusNotFound, "not_found", "route not found")
				return
			}
			switch verb {
			case "retire":
				if caller.Role != application.RoleOwner {
					h.error(w, r, 403, "forbidden", "owner role required")
					return
				}
				h.retireRepository(w, r.WithContext(ctx), id)
				return
			case "observe":
				// Gated exactly the way /v1/runner/permits:check gates
				// RoleRunner: the forge probe runs on the Runner, so only a
				// Runner session may submit its Observation.
				if caller.Role != application.RoleRunner {
					h.error(w, r, 403, "forbidden", "runner role required")
					return
				}
				h.observeRepository(w, r.WithContext(ctx), id)
				return
			}
			h.error(w, r, http.StatusNotFound, "not_found", "route not found")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/leases/") && strings.HasSuffix(r.URL.Path, ":renew") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/leases/"), ":renew")
			h.renew(w, r.WithContext(ctx), id)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/executions/") && strings.HasSuffix(r.URL.Path, ":start") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/executions/"), ":start")
			h.start(w, r.WithContext(ctx), id)
			return
		}
		h.error(w, r, http.StatusNotFound, "not_found", "route not found")
	}
}

// Legacy local authenticators keep existing emulator tests usable. Production
// composition uses CombinedAuthenticator and therefore gates both enrollment
// public-looking endpoints on the IAP owner assertion.
func (h *Handler) enrollmentTransportAuth(r *http.Request) bool {
	combined, ok := h.config.Authenticator.(CombinedAuthenticator)
	if !ok {
		return true
	}
	caller, err := combined.Authenticate(r)
	return err == nil && caller.Role == application.RoleOwner
}

func (h *Handler) enrollmentChallenge(w http.ResponseWriter, r *http.Request) {
	if h.config.RunnerEnrollment == nil {
		h.error(w, r, http.StatusServiceUnavailable, "not_configured", "runner enrollment is not configured")
		return
	}
	var b struct {
		EnrollmentToken string `json:"enrollment_token"`
		PublicKey       string `json:"public_key"`
	}
	if !h.decode(w, r, &b) {
		return
	}
	key, err := decodePublicKey(b.PublicKey)
	if err != nil {
		h.error(w, r, http.StatusBadRequest, "invalid_request", "public key required")
		return
	}
	c, err := h.config.RunnerEnrollment.Begin(r.Context(), b.EnrollmentToken, key)
	if err != nil {
		h.error(w, r, http.StatusUnauthorized, "enrollment_failed", "enrollment challenge failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenge_id": c.ID, "runner_id": c.RunnerID, "nonce": base64.RawURLEncoding.EncodeToString(c.Nonce), "method": c.Method, "path": c.Path, "issued_at": c.IssuedAt, "expires_at": c.ExpiresAt})
}

func (h *Handler) issueEnrollment(w http.ResponseWriter, r *http.Request) {
	if h.config.RunnerEnrollment == nil {
		h.error(w, r, http.StatusServiceUnavailable, "not_configured", "runner enrollment is not configured")
		return
	}
	var b struct {
		RunnerID string `json:"runner_id"`
	}
	if !h.decode(w, r, &b) {
		return
	}
	token, err := h.config.RunnerEnrollment.IssueEnrollment(r.Context(), b.RunnerID, runner.TokenTTL)
	if err != nil {
		h.error(w, r, http.StatusBadRequest, "invalid_request", "could not issue enrollment")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"enrollment_token": token, "expires_in_seconds": int(runner.TokenTTL / time.Second)})
}

func (h *Handler) enrollmentComplete(w http.ResponseWriter, r *http.Request) {
	if h.config.RunnerEnrollment == nil {
		h.error(w, r, http.StatusServiceUnavailable, "not_configured", "runner enrollment is not configured")
		return
	}
	var b struct {
		ChallengeID string `json:"challenge_id"`
		Signature   string `json:"signature"`
	}
	if !h.decode(w, r, &b) {
		return
	}
	s, err := h.config.RunnerEnrollment.Complete(r.Context(), b.ChallengeID, b.Signature)
	if err != nil {
		h.error(w, r, http.StatusUnauthorized, "enrollment_failed", "enrollment completion failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runner_id": s.RunnerID, "session_token": s.Token, "expires_at": s.ExpiresAt})
}

func decodePublicKey(value string) (ed25519.PublicKey, error) {
	b, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		b, err = hex.DecodeString(value)
	}
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid public key")
	}
	return ed25519.PublicKey(b), nil
}
func (h *Handler) renew(w http.ResponseWriter, r *http.Request, id string) {
	var b struct {
		RequestID            string              `json:"request_id"`
		ExpectedLeaseVersion domain.Version      `json:"expected_lease_version"`
		FencingToken         domain.FencingToken `json:"fencing_token"`
		ControlRevision      domain.Revision     `json:"control_revision"`
	}
	if !h.decode(w, r, &b) {
		return
	}
	out, e := h.config.Service.Renew(r.Context(), application.RenewRequest{RequestID: b.RequestID, LeaseID: id, ExpectedLeaseVersion: b.ExpectedLeaseVersion, FencingToken: b.FencingToken, ControlRevision: b.ControlRevision})
	if e != nil {
		h.domainError(w, r, e)
		return
	}
	writeJSON(w, 200, out)
}
func (h *Handler) start(w http.ResponseWriter, r *http.Request, id string) {
	var b struct {
		RequestID                string          `json:"request_id"`
		ExpectedExecutionVersion domain.Version  `json:"expected_execution_version"`
		ControlRevision          domain.Revision `json:"control_revision"`
	}
	if !h.decode(w, r, &b) {
		return
	}
	out, e := h.config.Service.Start(r.Context(), application.StartRequest{RequestID: b.RequestID, ExecutionID: id, ExpectedExecutionVersion: b.ExpectedExecutionVersion, ControlRevision: b.ControlRevision})
	if e != nil {
		h.domainError(w, r, e)
		return
	}
	writeJSON(w, 200, out)
}

type heartbeatBody struct {
	RequestID string `json:"request_id"`
}

func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	var b struct {
		RequestID       string                      `json:"request_id"`
		ControlRevision domain.Revision             `json:"control_revision"`
		Processes       []domain.ProcessObservation `json:"process_observations"`
	}
	if !h.decode(w, r, &b) {
		return
	}
	out, e := h.config.Service.Heartbeat(r.Context(), application.HeartbeatRequest{RequestID: b.RequestID, ControlRevision: b.ControlRevision, Processes: b.Processes})
	if e != nil {
		h.domainError(w, r, e)
		return
	}
	writeJSON(w, 200, out)
}
func (h *Handler) checkpoint(w http.ResponseWriter, r *http.Request) {
	var b struct {
		RequestID       string              `json:"request_id"`
		ExecutionID     string              `json:"execution_id"`
		LeaseID         string              `json:"lease_id"`
		FencingToken    domain.FencingToken `json:"fencing_token"`
		ControlRevision domain.Revision     `json:"control_revision"`
	}
	if !h.decode(w, r, &b) {
		return
	}
	out, e := h.config.Service.Checkpoint(r.Context(), application.CheckpointRequest{RequestID: b.RequestID, ExecutionID: b.ExecutionID, LeaseID: b.LeaseID, FencingToken: b.FencingToken, ControlRevision: b.ControlRevision})
	if e != nil {
		h.domainError(w, r, e)
		return
	}
	writeJSON(w, 200, out)
}
func (h *Handler) listRequirements(w http.ResponseWriter, r *http.Request) {
	size := 0
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		var e error
		size, e = strconv.Atoi(raw)
		if e != nil {
			h.error(w, r, 400, "invalid_page", "page_size must be an integer")
			return
		}
	}
	out, err := h.config.Service.ListRequirementsPage(r.Context(), r.URL.Query().Get("cursor"), size)
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}
func (h *Handler) getRequirement(w http.ResponseWriter, r *http.Request, id string) {
	out, ok, err := h.config.Service.GetRequirementDetail(r.Context(), id)
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	if !ok {
		h.error(w, r, 404, "not_found", "requirement not found")
		return
	}
	writeJSON(w, 200, out)
}

func (h *Handler) listControls(w http.ResponseWriter, r *http.Request) {
	size := 0
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		var err error
		size, err = strconv.Atoi(raw)
		if err != nil {
			h.error(w, r, 400, "invalid_page", "page_size must be an integer")
			return
		}
	}
	out, err := h.config.Service.ListControls(r.Context(), size)
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"controls": out})
}

// releaseStatePath is the single read-only release route. internal/api
// imports neither internal/release nor internal/update: the release
// machinery is reached only through the application Service, and the eight
// promotion conditions are not restated here. A go/ast guard in
// api_test.go asserts both import paths stay absent.
const releaseStatePath = "/v1/release/state"

// releaseState delegates to the Service and maps exactly one application
// error to 503: a process with no explicitly configured release source root
// reports no version at all rather than a guessed one.
func (h *Handler) releaseState(w http.ResponseWriter, r *http.Request) {
	out, err := h.config.Service.ReleaseState(r.Context())
	if err != nil {
		if errors.Is(err, application.ErrReleaseObserverNotConfigured) {
			h.error(w, r, http.StatusServiceUnavailable, "release_observer_not_configured", err.Error())
			return
		}
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

func (h *Handler) queueSummary(w http.ResponseWriter, r *http.Request) {
	out, err := h.config.Service.QueueSummary(r.Context())
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	limit := application.MaxPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, e := strconv.Atoi(raw); e == nil {
			limit = n
		}
	}
	if limit < 1 || limit > application.MaxPageSize {
		h.error(w, r, 400, "invalid_limit", "limit must be between 1 and 100")
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "ndjson" {
		h.error(w, r, 400, "invalid_format", "format must be json or ndjson")
		return
	}
	rows, err := h.config.Service.Export(r.Context(), limit)
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	if format == "ndjson" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", "attachment; filename=agentic-loop-export.ndjson")
		enc := json.NewEncoder(w)
		for _, row := range rows {
			if err := enc.Encode(row); err != nil {
				return
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=agentic-loop-export.json")
	_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "v1", "records": rows})
}
func (h *Handler) ownerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, web.OwnerHTML())
}
func (h *Handler) ownerAsset(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		h.error(w, r, 404, "not_found", "asset not found")
		return
	}
	b, ok := web.Asset(name)
	if !ok {
		h.error(w, r, 404, "not_found", "asset not found")
		return
	}
	ct := "text/plain; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		ct = "text/css; charset=utf-8"
	}
	if strings.HasSuffix(name, ".js") {
		ct = "application/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'")
	_, _ = w.Write(b)
}
func (h *Handler) originAllowed(r *http.Request, caller application.Caller) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	for _, allowed := range h.config.AllowedOrigins {
		if origin == allowed {
			return nil
		}
	}
	if caller.Role == application.RoleOwner {
		return errors.New("origin is not allowed")
	}
	return errors.New("origin is not allowed")
}

type captureBody struct {
	RequestID     string `json:"request_id"`
	RequirementID string `json:"requirement_id,omitempty"`
	Text          string `json:"text"`
	// RepositoryID is optional (V2-071 A12). When present, Capture writes the
	// write-once Requirement-to-Repository link; when absent, nothing about
	// the existing capture path changes.
	RepositoryID string `json:"repository_id,omitempty"`
}

func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	var b captureBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.Capture(r.Context(), application.CaptureRequest{RequestID: b.RequestID, RequirementID: b.RequirementID, Text: b.Text, RepositoryID: b.RepositoryID})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

type controlBody struct {
	RequestID  string                  `json:"request_id"`
	ScopeKind  domain.ControlScopeKind `json:"scope_kind"`
	ScopeValue string                  `json:"scope_value"`
	Mode       domain.ControlMode      `json:"mode"`
	Reason     string                  `json:"reason,omitempty"`
}
type targetDTO struct {
	InstallationID string `json:"installation_id,omitempty"`
	RepositoryID   string `json:"repository_id,omitempty"`
	RequirementID  string `json:"requirement_id,omitempty"`
	IncrementID    string `json:"increment_id,omitempty"`
	RunnerID       string `json:"runner_id,omitempty"`
	Channel        string `json:"channel,omitempty"`
}

func (t targetDTO) domain() domain.ControlTarget {
	return domain.ControlTarget{InstallationID: t.InstallationID, RepositoryID: t.RepositoryID, RequirementID: domain.RequirementID(t.RequirementID), IncrementID: domain.IncrementID(t.IncrementID), RunnerID: domain.RunnerID(t.RunnerID), Channel: t.Channel}
}

func (h *Handler) control(w http.ResponseWriter, r *http.Request) {
	var b controlBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.Control(r.Context(), application.ControlRequest{RequestID: b.RequestID, Scope: domain.ControlScope{Kind: b.ScopeKind, Value: b.ScopeValue}, Mode: b.Mode, Reason: b.Reason})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type claimBody struct {
	RequestID                string          `json:"request_id"`
	IncrementID              string          `json:"increment_id"`
	ExpectedIncrementVersion domain.Version  `json:"expected_increment_version"`
	ControlRevision          domain.Revision `json:"control_revision"`
	Target                   targetDTO       `json:"target,omitempty"`
}

func (h *Handler) claim(w http.ResponseWriter, r *http.Request) {
	var b claimBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.Claim(r.Context(), application.ClaimRequest{RequestID: b.RequestID, IncrementID: b.IncrementID, ExpectedIncrementVersion: b.ExpectedIncrementVersion, ControlRevision: b.ControlRevision, Target: b.Target.domain()})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type permitBody struct {
	RequestID            string              `json:"request_id"`
	Kind                 domain.PermitKind   `json:"kind"`
	Target               targetDTO           `json:"target"`
	ControlRevision      domain.Revision     `json:"control_revision"`
	FencingToken         domain.FencingToken `json:"fencing_token"`
	ExpectedFencingToken domain.FencingToken `json:"expected_fencing_token"`
	Resource             string              `json:"resource"`
}

func (h *Handler) permit(w http.ResponseWriter, r *http.Request) {
	var b permitBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.Permit(r.Context(), application.PermitRequest{RequestID: b.RequestID, Kind: b.Kind, Target: b.Target.domain(), ControlRevision: b.ControlRevision, FencingToken: b.FencingToken, ExpectedFencingToken: b.ExpectedFencingToken, Resource: b.Resource})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type resultBody struct {
	RequestID                string              `json:"request_id"`
	ExecutionID              string              `json:"execution_id"`
	LeaseID                  string              `json:"lease_id"`
	ExpectedExecutionVersion domain.Version      `json:"expected_execution_version"`
	FencingToken             domain.FencingToken `json:"fencing_token"`
	ControlRevision          domain.Revision     `json:"control_revision"`
	Succeeded                bool                `json:"succeeded"`
	Target                   targetDTO           `json:"target,omitempty"`
}

func (h *Handler) result(w http.ResponseWriter, r *http.Request) {
	var b resultBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.AcceptResult(r.Context(), application.AcceptResultRequest{RequestID: b.RequestID, ExecutionID: b.ExecutionID, LeaseID: b.LeaseID, ExpectedExecutionVersion: b.ExpectedExecutionVersion, FencingToken: b.FencingToken, ControlRevision: b.ControlRevision, Succeeded: b.Succeeded, Target: b.Target.domain()})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	ct := r.Header.Get("Content-Type")
	media, _, err := mime.ParseMediaType(ct)
	if err != nil || media != "application/json" {
		h.error(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "content-type must be application/json")
		return false
	}
	if r.ContentLength > maxJSON {
		h.error(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request exceeds 1 MiB")
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSON+1))
	if err != nil || len(body) > maxJSON {
		h.error(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request exceeds 1 MiB")
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		h.error(w, r, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		return false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		h.error(w, r, http.StatusBadRequest, "invalid_json", "request must contain one JSON value")
		return false
	}
	return true
}
func (h *Handler) domainError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, quota.ErrOverBudget) {
		// Capacity exhaustion is an explicit waiting state, never a caller
		// mistake: 429 with a Retry-After naming the next UTC midnight, when
		// the daily counter resets. The message repeats only the ceilings
		// already documented in docs/operations/gcp-runbook.md; it never
		// echoes the wrapped error's actual usage figures.
		w.Header().Set("Retry-After", nextUTCMidnight(time.Now()).Format(http.TimeFormat))
		h.error(w, r, http.StatusTooManyRequests, "quota_exhausted", "the daily Firestore quota budget is exhausted for this installation; retry after the next UTC midnight")
		return
	}
	status := http.StatusBadRequest
	code := "invalid_request"
	if errors.Is(err, application.ErrNotFound) {
		status = http.StatusNotFound
		code = "not_found"
	}
	if errors.Is(err, application.ErrUnauthenticated) {
		status = 401
		code = "unauthorized"
	}
	if errors.Is(err, application.ErrForbidden) {
		status = 403
		code = "forbidden"
	}
	if errors.Is(err, domain.ErrControlDenied) || errors.Is(err, domain.ErrLeaseNotOwned) {
		status = 403
		code = "policy_denied"
	}
	if errors.Is(err, domain.ErrStaleVersion) {
		status = 409
		code = "conflict"
	}
	if errors.Is(err, application.ErrRepositoryAlreadyRegistered) {
		// A duplicate source locator is a conflict with existing state, not a
		// malformed request: the same normalised (forge, owner, name) triple
		// is already registered for this Installation.
		status = http.StatusConflict
		code = "conflict"
	}
	if errors.Is(err, domain.ErrStaleFence) || errors.Is(err, domain.ErrLeaseExpired) || errors.Is(err, application.ErrIdempotencyConflict) {
		status = http.StatusConflict
		code = "conflict"
	}
	h.error(w, r, status, code, err.Error())
}

// nextUTCMidnight returns the first instant of the UTC day after at, which
// is when quota.Counter's daily reservation resets.
func nextUTCMidnight(at time.Time) time.Time {
	u := at.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}
func (h *Handler) error(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": code, "message": msg, "schema_version": "v1", "correlation_id": CorrelationID(r.Context())})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
