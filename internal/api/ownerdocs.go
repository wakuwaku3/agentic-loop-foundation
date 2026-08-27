package api

// Transport for the owner-readable documentation surface (V2-095 A9, closing
// escalation E22-10). Two GET routes, both on the EXISTING Authenticator and
// application.RoleOwner seam, both under the existing /owner/ prefix, with no
// new authentication mechanism and no new header:
//
//	GET /owner/docs/            owner   the index
//	GET /owner/docs/**.md       owner   one document
//
// WHY THE ALLOWLIST IS SAFE, stated here because the dangerous version of this
// route is a path-joining file server under /owner/. The set of documents this
// surface serves is EXACTLY the documentation-role member set of the release
// bundle the process assembled from its explicitly configured source root --
// the same set internal/release resolves for the promotion gate, recomputed
// from the tree rather than written down. A request is answered by SET
// MEMBERSHIP on the recorded member path: the caller's string is used for one
// thing only, a map lookup, and the path that is actually opened is the
// MEMBER'S own recorded path. So a traversal attempt, an absolute path, a
// duplicated separator and a percent-encoded escape are all simply absent from
// the map and refused BEFORE any file is opened. Nothing here calls
// filepath.Join on caller input.
//
// WHY THE PREFIX IS /owner/ AND NOT /owner/docs/{suffix}. The member paths the
// bundle records already begin with "docs/", so stripping the fixed "/owner/"
// prefix from the request path yields the member path VERBATIM, with no
// transform to get wrong. GET /owner/docs/preview/index.md therefore looks up
// exactly "docs/preview/index.md". The index is the one exact-match exception,
// at /owner/docs/, which is not a member path and so cannot collide.
//
// internal/api imports neither internal/release nor internal/update -- a
// go/ast guard in api_test.go asserts both stay absent -- so the assembled
// member set is reached only through the application Service, exactly as the
// release state read already is.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
)

// ownerDocsIndexPath is the exact path of the index route, and
// ownerDocsPrefix is the prefix a per-document request must carry. Both are
// named constants for the same reason releaseStatePath is: a scan over the
// route table can be written against the constant.
const ownerDocsIndexPath = "/owner/docs/"
const ownerDocsPrefix = "/owner/"

// ownerDocsMember returns the member path a request names, and whether the
// request is a per-document request at all.
//
// It performs no cleaning and no normalisation on purpose: normalising would
// turn a traversal attempt into a different string that might then be found in
// the member map. The remainder is handed to the set membership test exactly as
// the caller wrote it, and anything that is not literally a member path is
// refused.
func ownerDocsMember(path string) (string, bool) {
	if !strings.HasPrefix(path, ownerDocsPrefix) {
		return "", false
	}
	member := strings.TrimPrefix(path, ownerDocsPrefix)
	if member == "" {
		return "", false
	}
	return member, true
}

// ownerDocsIndex answers the index. It reports the resolved channel, the
// assembled release version, the documentation digest and the member set.
func (h *Handler) ownerDocsIndex(w http.ResponseWriter, r *http.Request) {
	out, err := h.config.Service.ReleaseDocumentIndex(r.Context())
	if err != nil {
		h.ownerDocsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ownerDocsDocument answers one document.
func (h *Handler) ownerDocsDocument(w http.ResponseWriter, r *http.Request, member string) {
	out, err := h.config.Service.ReleaseDocument(r.Context(), member)
	if err != nil {
		h.ownerDocsError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ownerDocsError maps the three application errors this surface can produce.
//
// The not-configured answer is the SAME 503 shape and the SAME error code
// GET /v1/release/state answers, because it is the same condition and the same
// error value: a process given no explicit release source root can report no
// channel, no version and no document set, and a defaulted root would make it
// report a version it was not assembled from. Answering 404 here instead would
// tell an owner "there is no such document" when the truth is "this process
// does not know which documents it serves".
func (h *Handler) ownerDocsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrReleaseObserverNotConfigured):
		h.error(w, r, http.StatusServiceUnavailable, "release_observer_not_configured", err.Error())
	case errors.Is(err, application.ErrReleaseDocumentNotFound):
		h.error(w, r, http.StatusNotFound, "document_not_found", err.Error())
	case errors.Is(err, application.ErrReleaseDocumentDrifted):
		h.error(w, r, http.StatusConflict, "document_drifted", err.Error())
	default:
		h.domainError(w, r, err)
	}
}
