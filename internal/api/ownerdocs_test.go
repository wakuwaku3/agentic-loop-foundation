package api_test

// V2-095 A9/A10, escalation E22-10: the owner documentation surface, asserted
// positively and negatively.
//
// Determinism: every instant comes from the injected clock already declared in
// this package, there is no sleep, no timer, no goroutine and no randomness,
// and the release source is attached from the real repository root through
// V2-091's OWN wiring (application.Service.AttachReleaseSource) rather than
// through a second producer -- internal/application/release_surface.go and
// cmd/** are prohibited here on purpose, so a second wiring path would be
// proof the design was departed from.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/api"
	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	agenticrunner "github.com/takushi/agentic-loop-foundation/v2/internal/runner"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

// ownerDocsExpectedSet is the documentation-role member set as this test
// MEASURES it from the tree, not as a list this test chooses. It is derived by
// walking the two declared globs, so adding a Preview or Stable document makes
// the assertions below cover it automatically and a hand-written list can never
// drift from the bundle.
func ownerDocsExpectedSet(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{filepath.Join("docs", "preview"), filepath.Join("docs", "stable")} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			out = append(out, filepath.ToSlash(filepath.Join(dir, e.Name())))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("the two declared documentation globs resolved to zero files; every assertion below would pass vacuously")
	}
	return out
}

// ownerDocsHandler builds a handler whose Service has V2-091's release source
// attached from the real repository root. configure=false leaves it
// unconfigured, which is the not-configured half of A9.
func ownerDocsHandler(t *testing.T, configure bool) (http.Handler, *application.Service, string) {
	t.Helper()
	root := repoRootForLiveTest(t)
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if configure {
		// V2-091's constructor, with its own explicit configuration. There is
		// no default root here and none anywhere else: an unconfigured process
		// reports no version at all.
		if err := svc.AttachReleaseSource(application.ReleaseSourceConfig{
			Root:             root,
			Repository:       "agentic-loop-foundation",
			EnvironmentClass: "preview-local",
			AssembledAt:      clock{}.Now(),
		}); err != nil {
			t.Fatalf("attach V2-091's release source from the real repository root: %v", err)
		}
		t.Cleanup(svc.DetachReleaseSource)
	}
	auth := api.BearerAuthenticator{
		"owner":  {Role: application.RoleOwner, Subject: "owner"},
		"runner": {Role: application.RoleRunner, Subject: "runner", RunnerID: "runner-1"},
	}
	enrollment, err := agenticrunner.NewService(agenticrunner.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return api.New(api.Config{Authenticator: auth, Service: svc, RunnerEnrollment: enrollment, AllowedOrigins: []string{"https://console.example"}}), svc, root
}

// ownerDocsGet drives the handler with the path EXACTLY as written. The request
// is built against a fixed safe target and its URL.Path is then overwritten, so
// a traversal attempt, a doubled separator or a trailing space reaches the
// handler as the caller wrote it rather than pre-normalised -- and so
// httptest.NewRequest never has to parse an unparseable target.
func ownerDocsGet(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.URL.Path = path
	req.URL.RawPath = ""
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestOwnerDocsIndexNamesTheChannelTheVersionAndExactlyTheAssembledSet is A9's
// positive half for the index.
func TestOwnerDocsIndexNamesTheChannelTheVersionAndExactlyTheAssembledSet(t *testing.T) {
	h, _, root := ownerDocsHandler(t, true)
	w := ownerDocsGet(h, "/owner/docs/", "owner")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /owner/docs/ = %d, want 200: %s", w.Code, w.Body.String())
	}
	var index application.ReleaseDocumentIndexView
	if err := json.Unmarshal(w.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	if index.Channel != "preview" {
		t.Fatalf("channel = %q; this process has recorded no Stable route, so release.ResolveChannel must answer preview", index.Channel)
	}
	if index.ReleaseVersion == "" {
		t.Fatal("the index reports no release version; a document set with no version cannot answer 'the documents corresponding to the channel and version in use'")
	}
	if index.DocsDigest == "" {
		t.Fatal("the index reports no docs digest")
	}
	if !strings.Contains(index.AllowlistSource, "SET MEMBERSHIP") {
		t.Fatalf("the index does not state that its allowlist is a set membership test: %q", index.AllowlistSource)
	}

	got := make([]string, 0, len(index.Documents))
	for _, d := range index.Documents {
		got = append(got, d.Path)
		if d.SHA256 == "" {
			t.Fatalf("%s is listed with no digest", d.Path)
		}
		if d.Route != "/owner/"+d.Path {
			t.Fatalf("%s is listed with route %q, which is not the route that serves it", d.Path, d.Route)
		}
	}
	sort.Strings(got)
	want := ownerDocsExpectedSet(t, root)
	if len(got) != len(want) {
		t.Fatalf("the index lists %d documents %v, want exactly the %d assembled documentation members %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("the index lists %v, want exactly %v", got, want)
		}
	}
	t.Logf("index measured: channel=%s release_version=%s documents=%v", index.Channel, index.ReleaseVersion, got)
}

// TestOwnerDocsServesEveryMemberByteForByte is A9's positive half for the
// per-document route: each member is served and its bytes equal the file's.
func TestOwnerDocsServesEveryMemberByteForByte(t *testing.T) {
	h, _, root := ownerDocsHandler(t, true)
	for _, rel := range ownerDocsExpectedSet(t, root) {
		w := ownerDocsGet(h, "/owner/"+rel, "owner")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /owner/%s = %d, want 200: %s", rel, w.Code, w.Body.String())
		}
		var doc application.ReleaseDocumentView
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		onDisk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if doc.Content != string(onDisk) {
			t.Fatalf("the served bytes of %s differ from the file's bytes", rel)
		}
		digest := sha256.Sum256(onDisk)
		if doc.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("%s is served with digest %s, which is not sha256 of its bytes", rel, doc.SHA256)
		}
		if doc.SizeBytes != len(onDisk) {
			t.Fatalf("%s is served with size %d, want %d", rel, doc.SizeBytes, len(onDisk))
		}
		if doc.Channel != "preview" || doc.ReleaseVersion == "" {
			t.Fatalf("%s is served with channel %q and version %q", rel, doc.Channel, doc.ReleaseVersion)
		}
	}
}

// TestOwnerDocsRefusesEverythingOutsideTheAssembledSet is A9's negative half,
// and r6's mitigation: a path outside the set and a traversal attempt are both
// refused by the SAME set membership test, before any file is opened.
func TestOwnerDocsRefusesEverythingOutsideTheAssembledSet(t *testing.T) {
	h, _, _ := ownerDocsHandler(t, true)
	// (a) INSIDE the route's prefix and OUTSIDE the assembled set. These reach
	// the set membership test and are refused by it, before any file is opened.
	// The traversal shapes are the ones a path-joining handler would fall to;
	// none of them is normalised before the lookup, so none of them can become
	// a member path.
	for _, path := range []string{
		"/owner/docs/preview/does-not-exist.md",
		"/owner/docs/stable/does-not-exist.md",
		"/owner/docs/../go.mod",
		"/owner/docs/preview/../../go.mod",
		"/owner/docs/preview/index.md/../../../go.mod",
		"/owner/docs/../../../../etc/passwd",
		"/owner/docs//preview/index.md",
		"/owner/docs/./preview/index.md",
		"/owner/docs/preview/index.md ",
		"/owner/docs/preview/index.MD",
		"/owner/docs/preview/",
		"/owner/docs/preview",
	} {
		w := ownerDocsGet(h, path, "owner")
		if w.Code == http.StatusOK {
			t.Fatalf("GET %s returned 200; the allowlist is not a set membership test", path)
		}
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 document_not_found", path, w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if code, _ := body["error"].(string); code != "document_not_found" {
			t.Fatalf("GET %s answered error code %q, want document_not_found", path, code)
		}
	}
	// (b) OUTSIDE the route's prefix. These never reach the document surface at
	// all: the prefix is /owner/docs/ and nothing else, so a real repository
	// file elsewhere in the tree is not even a candidate for the lookup. They
	// must not answer 200, and they must not answer as though a document
	// existed.
	for _, path := range []string{
		"/owner/go.mod",
		"/owner/contracts/release-contract/foundation.json",
		"/owner/.agents/v2/evidence/index.json",
		"/owner/../../../../etc/passwd",
		"/owner/DOCS/preview/index.md",
		"/owner/docsx/preview/index.md",
	} {
		w := ownerDocsGet(h, path, "owner")
		if w.Code == http.StatusOK {
			t.Fatalf("GET %s returned 200; a path outside the document route's own prefix must never be served", path)
		}
		if strings.Contains(w.Body.String(), "module ") || strings.Contains(w.Body.String(), "\"capabilities\"") {
			t.Fatalf("GET %s echoed repository file content: %s", path, w.Body.String())
		}
	}
	// A non-owner caller is refused exactly as /owner/ already refuses one.
	console := ownerDocsGet(h, "/owner/", "runner")
	docs := ownerDocsGet(h, "/owner/docs/", "runner")
	doc := ownerDocsGet(h, "/owner/docs/preview/index.md", "runner")
	if docs.Code != console.Code || doc.Code != console.Code {
		t.Fatalf("a runner caller gets %d on /owner/, %d on the docs index and %d on a document; one seam, one answer", console.Code, docs.Code, doc.Code)
	}
	if console.Code != http.StatusForbidden {
		t.Fatalf("a runner caller on /owner/ = %d, want 403", console.Code)
	}
	// And with no token at all.
	if anon := ownerDocsGet(h, "/owner/docs/", ""); anon.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated caller on the docs index = %d, want 401", anon.Code)
	}
	// A non-GET method is a method error and never reaches the Service.
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/owner/docs/", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /owner/docs/ = %d, want 405", w.Code)
	}
}

// TestOwnerDocsAnswersTheSame503ShapeAsReleaseStateWhenUnconfigured is A9's
// not-configured half, and A10's "never a default root".
func TestOwnerDocsAnswersTheSame503ShapeAsReleaseStateWhenUnconfigured(t *testing.T) {
	h, _, _ := ownerDocsHandler(t, false)
	state := ownerDocsGet(h, "/v1/release/state", "owner")
	if state.Code != http.StatusServiceUnavailable {
		t.Fatalf("with no release source configured GET /v1/release/state = %d, want 503", state.Code)
	}
	var stateBody map[string]any
	if err := json.Unmarshal(state.Body.Bytes(), &stateBody); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/owner/docs/", "/owner/docs/preview/index.md"} {
		w := ownerDocsGet(h, path, "owner")
		if w.Code != state.Code {
			t.Fatalf("GET %s = %d with no release source configured, want the same %d GET /v1/release/state answers", path, w.Code, state.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["error"] != stateBody["error"] {
			t.Fatalf("GET %s answered error code %v, want the same %v", path, body["error"], stateBody["error"])
		}
		if body["message"] != stateBody["message"] {
			t.Fatalf("GET %s answered a different message than /v1/release/state; the two must be the same condition", path)
		}
		// NEVER A DEFAULT ROOT: the unconfigured answer carries no document,
		// no channel and no version at all.
		for _, forbidden := range []string{"documents", "channel", "release_version", "docs_digest", "content"} {
			if _, present := body[forbidden]; present {
				t.Fatalf("the unconfigured answer to %s carries %q; a defaulted root would make this process report a version it was not assembled from", path, forbidden)
			}
		}
	}
	t.Logf("unconfigured answer measured: status=%d error=%v (identical on /v1/release/state, /owner/docs/ and one document)", state.Code, stateBody["error"])
}

// TestOwnerDocsIndexAndDocumentResolveTheSameSet is the assertion that stops
// the two routes from drifting: everything the index lists is servable, and
// nothing servable is missing from the index.
func TestOwnerDocsIndexAndDocumentResolveTheSameSet(t *testing.T) {
	h, svc, root := ownerDocsHandler(t, true)
	ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})
	index, err := svc.ReleaseDocumentIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range index.Documents {
		if _, err := svc.ReleaseDocument(ctx, d.Path); err != nil {
			t.Fatalf("the index lists %s but the document route refuses it: %v", d.Path, err)
		}
	}
	listed := map[string]bool{}
	for _, d := range index.Documents {
		listed[d.Path] = true
	}
	for _, rel := range ownerDocsExpectedSet(t, root) {
		if !listed[rel] {
			t.Fatalf("%s is a documentation member of the tree but the index does not list it", rel)
		}
		if w := ownerDocsGet(h, "/owner/"+rel, "owner"); w.Code != http.StatusOK {
			t.Fatalf("%s is servable through the Service but the route answers %d", rel, w.Code)
		}
	}
}

// TestTheShippedZeroCandidateReleaseSourceConfigurationActuallyAttaches is the
// regression guard for the defect V2-095 MEASURED in V2-091's wiring and
// repaired in place under section 12.3.
//
// WHAT WAS MEASURED. cmd/control-plane/main.go reads
// AGENTIC_LOOP_RELEASE_SOURCE_ROOT, AGENTIC_LOOP_RELEASE_REPOSITORY and
// AGENTIC_LOOP_RELEASE_ENVIRONMENT_CLASS and calls
// application.Service.AttachReleaseSource with ReleaseSourceConfig.Candidate
// left deliberately ZERO, because that process records no capability evidence
// and must not fabricate any. AttachReleaseSource called
// release.AssembleBundle unconditionally, which calls release.NewBundle, which
// calls domain.ValidateRelease, which requires a non-empty release id and a
// positive version. So with the three variables set the shipped binary printed
// "control-plane: assemble the release source root: domain id must not be
// empty" and exited. Every pre-existing test of that function supplied a
// fully-evidenced candidate, so the only path the shipped binary can take was
// never executed by any test.
//
// THIS test executes exactly that path: a ZERO Candidate, the real repository
// root, and nothing else. It is here rather than in internal/application
// because internal/application/loop_test.go is outside this task's allowed
// paths; the assertion is about a behaviour internal/api depends on, so it
// belongs on a route this task owns.
func TestTheShippedZeroCandidateReleaseSourceConfigurationActuallyAttaches(t *testing.T) {
	root := repoRootForLiveTest(t)
	st := memory.New()
	svc, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// The SHIPPED configuration, byte for byte: no Candidate field at all.
	if err := svc.AttachReleaseSource(application.ReleaseSourceConfig{
		Root:             root,
		Repository:       "agentic-loop-foundation",
		EnvironmentClass: "preview-local",
		AssembledAt:      clock{}.Now(),
	}); err != nil {
		t.Fatalf("the shipped zero-candidate release-source configuration was refused: %v", err)
	}
	defer svc.DetachReleaseSource()

	ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner"})

	// It OBSERVES: the state read answers with the assembled version.
	state, err := svc.ReleaseState(ctx)
	if err != nil {
		t.Fatalf("with the shipped configuration attached, ReleaseState still refuses: %v", err)
	}
	if state.ReleaseVersion == "" {
		t.Fatal("the observer reports no release version")
	}
	if state.EnvironmentClass != "preview-local" {
		t.Fatalf("environment class = %q, want the declared preview-local", state.EnvironmentClass)
	}

	// It does NOT become promotable. The repair separates observing a release
	// from being able to promote one; it does not make an unevidenced candidate
	// promotable, and this is the assertion that says so.
	if state.Promotable {
		t.Fatal("a process that recorded no capability evidence reports itself promotable; the repair widened more than it was allowed to")
	}
	if state.Route.Recorded {
		t.Fatalf("a zero-candidate assembly recorded a Preview route: %+v", state.Route)
	}
	if len(state.CapabilitiesWithoutEvidence) == 0 {
		t.Fatal("a process that recorded no capability evidence reports no capability as lacking evidence")
	}

	// And it serves its own documents, which is the reason A9's routes can
	// share ONE release-source configuration rather than adding a second.
	index, err := svc.ReleaseDocumentIndex(ctx)
	if err != nil {
		t.Fatalf("the document index refuses under the shipped configuration: %v", err)
	}
	if len(index.Documents) == 0 {
		t.Fatal("the document index lists nothing under the shipped configuration")
	}
	t.Logf("the shipped zero-candidate configuration attaches: release_version=%s promotable=%v route_recorded=%v documents=%d capabilities_without_evidence=%d",
		state.ReleaseVersion, state.Promotable, state.Route.Recorded, len(index.Documents), len(state.CapabilitiesWithoutEvidence))
}
