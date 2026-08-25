package api

// Transport for the Repository aggregate (V2-064). Five routes, all on the
// existing Authenticator/RoleOwner seam with no new authentication mechanism
// and no new header:
//
//	GET  /v1/repositories                            owner
//	POST /v1/repositories                            owner
//	GET  /v1/repositories/{repository_id}            owner
//	POST /v1/repositories/{repository_id}:retire     owner
//	POST /v1/repositories/{repository_id}:observe    runner
//
// The verb-suffix form follows /v1/leases/{lease_id}:renew and
// /v1/executions/{execution_id}:start, which already exist in both the
// router and contracts/openapi/openapi-v1.yaml.
//
// Nothing in this file, or anywhere else in the Control Plane, starts a
// process or reaches a forge: reachability arrives only as a Runner-submitted
// Observation on the :observe route.

import (
	"net/http"
	"strings"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

const repositoriesPath = "/v1/repositories"
const repositoriesPrefix = repositoriesPath + "/"

// repositoryVerb splits "/v1/repositories/{id}:verb" into its identifier and
// its verb. A path with no verb yields an empty verb, and an empty identifier
// is reported as such so the caller can answer 404 rather than routing a
// blank id into the service.
func repositoryVerb(path string) (id, verb string) {
	rest := strings.TrimPrefix(path, repositoriesPrefix)
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		return rest[:colon], rest[colon+1:]
	}
	return rest, ""
}

type registerRepositoryBody struct {
	RequestID     string `json:"request_id"`
	RepositoryID  string `json:"repository_id,omitempty"`
	SourceURL     string `json:"source_url"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

type retireRepositoryBody struct {
	RequestID       string         `json:"request_id"`
	ExpectedVersion domain.Version `json:"expected_version,omitempty"`
}

// observeRepositoryBody is the bounded forge Observation. It has no field for
// raw output, a response body, a status line or a credential: the adapter
// parses on the Runner and only these fields cross the wire.
type observeRepositoryBody struct {
	RequestID      string `json:"request_id"`
	Reachable      bool   `json:"reachable"`
	DefaultBranch  string `json:"default_branch,omitempty"`
	CanPush        bool   `json:"can_push,omitempty"`
	ForgeNodeID    string `json:"forge_node_id,omitempty"`
	AdapterVersion string `json:"adapter_version,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

func (h *Handler) registerRepository(w http.ResponseWriter, r *http.Request) {
	var b registerRepositoryBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.RegisterRepository(r.Context(), application.RegisterRepositoryRequest{RequestID: b.RequestID, RepositoryID: b.RepositoryID, SourceURL: b.SourceURL, DefaultBranch: b.DefaultBranch})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) retireRepository(w http.ResponseWriter, r *http.Request, id string) {
	var b retireRepositoryBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.RetireRepository(r.Context(), application.RetireRepositoryRequest{RequestID: b.RequestID, RepositoryID: id, ExpectedVersion: b.ExpectedVersion})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) observeRepository(w http.ResponseWriter, r *http.Request, id string) {
	var b observeRepositoryBody
	if !h.decode(w, r, &b) {
		return
	}
	out, err := h.config.Service.ObserveRepository(r.Context(), application.ObserveRepositoryRequest{RequestID: b.RequestID, RepositoryID: id, Reachable: b.Reachable, DefaultBranch: b.DefaultBranch, CanPush: b.CanPush, ForgeNodeID: b.ForgeNodeID, AdapterVersion: b.AdapterVersion, Reason: b.Reason})
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) listRepositories(w http.ResponseWriter, r *http.Request) {
	out, err := h.config.Service.ListRepositories(r.Context())
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getRepository(w http.ResponseWriter, r *http.Request, id string) {
	out, ok, err := h.config.Service.GetRepository(r.Context(), id)
	if err != nil {
		h.domainError(w, r, err)
		return
	}
	if !ok {
		h.error(w, r, http.StatusNotFound, "not_found", "repository not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
