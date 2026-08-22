// Package api is the transport boundary for the v2 control plane.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

const maxJSON = 1 << 20

type Authenticator interface {
	Authenticate(*http.Request) (application.Caller, error)
}
type Config struct {
	Authenticator  Authenticator
	Service        *application.Service
	AllowedOrigins []string
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
	if h.config.Authenticator == nil || h.config.Service == nil {
		h.error(w, r, http.StatusServiceUnavailable, "not_configured", "authenticated API is not configured")
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
	var b heartbeatBody
	if !h.decode(w, r, &b) {
		return
	}
	out, e := h.config.Service.Heartbeat(r.Context(), application.HeartbeatRequest{RequestID: b.RequestID})
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
	out, err := h.config.Service.ListRequirements(r.Context())
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"requirements": out})
}
func (h *Handler) getRequirement(w http.ResponseWriter, r *http.Request, id string) {
	out, ok, err := h.config.Service.GetRequirement(r.Context(), id)
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
}

func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	var b captureBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.Capture(r.Context(), application.CaptureRequest{RequestID: b.RequestID, RequirementID: b.RequirementID, Text: b.Text})
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
	if errors.Is(err, domain.ErrStaleFence) || errors.Is(err, domain.ErrLeaseExpired) || errors.Is(err, application.ErrIdempotencyConflict) {
		status = http.StatusConflict
		code = "conflict"
	}
	h.error(w, r, status, code, err.Error())
}
func (h *Handler) error(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": code, "message": msg, "schema_version": "v1", "correlation_id": CorrelationID(r.Context())})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
