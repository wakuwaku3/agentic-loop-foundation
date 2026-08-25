package api_test

// Transport closure of the five Repository routes (V2-064), plus the
// structural proof that the Control Plane never invokes gh or git.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, body)
	}
	return v
}

func registerViaAPI(t *testing.T, h http.Handler, requestID, url string) string {
	t.Helper()
	w := call(h, http.MethodPost, "/v1/repositories", `{"request_id":"`+requestID+`","source_url":"`+url+`","default_branch":"main"}`, "owner")
	if w.Code != http.StatusCreated {
		t.Fatalf("register %q: status=%d body=%s", url, w.Code, w.Body.String())
	}
	v := decodeBody(t, w.Body.Bytes())
	id, _ := v["repository_id"].(string)
	if id == "" {
		t.Fatalf("register %q returned no repository_id: %s", url, w.Body.String())
	}
	return id
}

// The three tests below deliberately use a fresh handler each, because a
// bounded read transaction reserves internal/quota.ReadTransactionUsage
// (6,001 reads) against the Installation's daily budget and the reservation
// fails closed at 80% of it. Four owner reads in one store exhaust that
// budget, which is a real production property, not a test artefact: it is why
// the detail view is a bounded read and not a poll.

func TestRepositoryRegisterNormalisesAndIsListedAndDetailed(t *testing.T) {
	h := testHandler(t)
	id := registerViaAPI(t, h, "req-1", "git@github.com:Wakuwaku3/Agentic-Loop-Foundation.git")

	w := call(h, http.MethodGet, "/v1/repositories", "", "owner")
	if w.Code != 200 {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	list := decodeBody(t, w.Body.Bytes())
	rows, ok := list["repositories"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("list body=%s", w.Body.String())
	}
	row := rows[0].(map[string]any)
	locator := row["locator"].(map[string]any)
	if locator["owner"] != "wakuwaku3" || locator["name"] != "agentic-loop-foundation" || locator["forge"] != "github" {
		t.Fatalf("locator not normalised: %v", locator)
	}
	// The raw URL is never echoed back, and is never the identity.
	if strings.Contains(w.Body.String(), "git@github.com") {
		t.Fatalf("list response echoes the raw source URL: %s", w.Body.String())
	}
	if row["repository_id"] == locator["name"] {
		t.Fatal("repository_id must be a loop-issued opaque identifier, not the forge name")
	}

	w = call(h, http.MethodGet, "/v1/repositories/"+id, "", "owner")
	if w.Code != 200 {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	detail := decodeBody(t, w.Body.Bytes())
	// A10: all six declared observables are present, and the ones without a
	// data source carry an explicit state plus a reason.
	for _, field := range []string{"repository_id", "locator", "status", "version", "requested_by", "application_release", "policy", "requirement_backlog", "runners_and_ai_resources", "executability", "effective_control_mode", "effective_control_revision"} {
		if _, ok := detail[field]; !ok {
			t.Fatalf("detail response is missing declared field %q: %s", field, w.Body.String())
		}
	}
	for _, field := range []string{"application_release", "policy", "runners_and_ai_resources"} {
		nested := detail[field].(map[string]any)
		if nested["state"] != "not-implemented" || nested["reason"] == "" {
			t.Fatalf("detail %s = %v; an absent data source must be an explicit state plus a reason", field, nested)
		}
	}
	// V2-071 A11/A28: the repository-scoped backlog is measured now, so this
	// one assertion changed from "unobserved" to the measured count. The test
	// name and every other assertion in it are unchanged. This Repository has
	// no linked Requirement in this test, so the measured count is zero -- a
	// measurement, not an absence -- and installation_scope is still reported
	// separately under its own name.
	backlog := detail["requirement_backlog"].(map[string]any)
	if backlog["state"] != "measured" || backlog["installation_scope"] == nil || backlog["requirement_count"] != float64(0) {
		t.Fatalf("detail requirement_backlog = %v", backlog)
	}
	executability := detail["executability"].(map[string]any)
	if executability["state"] != "unobserved" || executability["executable"] != false || executability["reason"] == "" {
		t.Fatalf("detail executability = %v", executability)
	}
	if _, ok := detail["observation"]; ok {
		t.Fatalf("detail carries an observation before any was submitted: %s", w.Body.String())
	}
}

func TestRepositoryObserveRouteIsRunnerOnlyAndFeedsExecutability(t *testing.T) {
	h := testHandler(t)
	id := registerViaAPI(t, h, "req-1", "https://github.com/O/N")

	w := call(h, http.MethodPost, "/v1/repositories/"+id+":observe", `{"request_id":"obs-1","reachable":true,"can_push":true,"default_branch":"main","forge_node_id":"node-1","adapter_version":"test"}`, "runner")
	if w.Code != 200 {
		t.Fatalf("observe status=%d body=%s", w.Code, w.Body.String())
	}
	accepted := decodeBody(t, w.Body.Bytes())
	if accepted["accepted"] != true {
		t.Fatalf("observe body=%s", w.Body.String())
	}

	w = call(h, http.MethodGet, "/v1/repositories/"+id, "", "owner")
	if w.Code != 200 {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	detail := decodeBody(t, w.Body.Bytes())
	executability := detail["executability"].(map[string]any)
	if executability["state"] != "executable" || executability["executable"] != true {
		t.Fatalf("post-observation executability = %v", executability)
	}
	observation, ok := detail["observation"].(map[string]any)
	if !ok || observation["default_branch"] != "main" || observation["forge_node_id"] != "node-1" {
		t.Fatalf("post-observation detail = %v", detail["observation"])
	}
	// The Observation carries no raw output and no credential-shaped field.
	for key := range observation {
		lowered := strings.ToLower(key)
		for _, bad := range []string{"token", "secret", "credential", "raw", "stdout", "stderr", "output", "header"} {
			if strings.Contains(lowered, bad) {
				t.Fatalf("observation carries a forbidden field %q", key)
			}
		}
	}
}

func TestRepositoryRetireRouteRollsBackAndKeepsTheRecordReadable(t *testing.T) {
	h := testHandler(t)
	id := registerViaAPI(t, h, "req-1", "https://github.com/O/N")

	w := call(h, http.MethodPost, "/v1/repositories/"+id+":retire", `{"request_id":"retire-1"}`, "owner")
	if w.Code != 200 {
		t.Fatalf("retire status=%d body=%s", w.Code, w.Body.String())
	}
	if decodeBody(t, w.Body.Bytes())["status"] != "retired" {
		t.Fatalf("retire body=%s", w.Body.String())
	}
	w = call(h, http.MethodGet, "/v1/repositories", "", "owner")
	if w.Code != 200 {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	if rows := decodeBody(t, w.Body.Bytes())["repositories"].([]any); len(rows) != 0 {
		t.Fatalf("retired repository still listed: %s", w.Body.String())
	}
	// Still readable, so the rollback is observable rather than a deletion.
	w = call(h, http.MethodGet, "/v1/repositories/"+id, "", "owner")
	if w.Code != 200 {
		t.Fatalf("retired detail status=%d body=%s", w.Code, w.Body.String())
	}
	detail := decodeBody(t, w.Body.Bytes())
	if detail["status"] != "retired" {
		t.Fatalf("retired detail body=%s", w.Body.String())
	}
	if detail["executability"].(map[string]any)["state"] != "retired" {
		t.Fatalf("retired executability = %v", detail["executability"])
	}
}

func TestRepositoryRouteAuthRoleMethodAndBodyContract(t *testing.T) {
	h := testHandler(t)
	id := registerViaAPI(t, h, "req-1", "https://github.com/O/N")

	type route struct {
		method, path, body, wantRole string
	}
	routes := []route{
		{http.MethodGet, "/v1/repositories", "", "owner"},
		{http.MethodPost, "/v1/repositories", `{"request_id":"x","source_url":"https://github.com/O/other"}`, "owner"},
		{http.MethodGet, "/v1/repositories/" + id, "", "owner"},
		{http.MethodPost, "/v1/repositories/" + id + ":retire", `{"request_id":"x"}`, "owner"},
		{http.MethodPost, "/v1/repositories/" + id + ":observe", `{"request_id":"x","reachable":true}`, "runner"},
	}
	if len(routes) != 5 {
		t.Fatal("the five declared routes must all be exercised")
	}
	for _, rt := range routes {
		// 401 without authentication.
		if w := call(h, rt.method, rt.path, rt.body, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthenticated status=%d body=%s", rt.method, rt.path, w.Code, w.Body.String())
		}
		// 403 for the wrong role.
		wrong := "runner"
		if rt.wantRole == "runner" {
			wrong = "owner"
		}
		w := call(h, rt.method, rt.path, rt.body, wrong)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s with role %s status=%d body=%s", rt.method, rt.path, wrong, w.Code, w.Body.String())
		}
		// The standard Error envelope with a correlation id.
		envelope := decodeBody(t, w.Body.Bytes())
		for _, field := range []string{"error", "message", "schema_version", "correlation_id"} {
			if v, ok := envelope[field]; !ok || v == "" {
				t.Fatalf("%s %s error envelope is missing %q: %s", rt.method, rt.path, field, w.Body.String())
			}
		}
		if w.Header().Get("X-Correlation-ID") == "" {
			t.Fatalf("%s %s response carries no X-Correlation-ID", rt.method, rt.path)
		}
	}

	// 405 on the wrong method.
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/v1/repositories"},
		{http.MethodPut, "/v1/repositories/" + id},
		{http.MethodGet, "/v1/repositories/" + id + ":retire"},
		{http.MethodGet, "/v1/repositories/" + id + ":observe"},
	} {
		if w := call(h, tc.method, tc.path, "", "owner"); w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status=%d body=%s, want 405", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// 415 on a content type that is not application/json.
	for _, contentType := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		req := httptest.NewRequest(http.MethodPost, "/v1/repositories", strings.NewReader(`{"request_id":"x","source_url":"O/N"}`))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Authorization", "Bearer owner")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("content-type %q status=%d body=%s", contentType, rec.Code, rec.Body.String())
		}
	}
	// 400 on a malformed body and on an unknown field.
	if w := call(h, http.MethodPost, "/v1/repositories", `{`, "owner"); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status=%d", w.Code)
	}
	if w := call(h, http.MethodPost, "/v1/repositories", `{"request_id":"x","source_url":"O/N","surprise":1}`, "owner"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", w.Code, w.Body.String())
	}
	// 400 on a locator that is not a repository coordinate.
	if w := call(h, http.MethodPost, "/v1/repositories", `{"request_id":"x","source_url":"https://github.com/only-one-segment"}`, "owner"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid locator status=%d body=%s", w.Code, w.Body.String())
	}
	// 409 on a duplicate normalised locator.
	if w := call(h, http.MethodPost, "/v1/repositories", `{"request_id":"dup","source_url":"git@github.com:O/N.git","default_branch":"main"}`, "owner"); w.Code != http.StatusConflict {
		t.Fatalf("duplicate locator status=%d body=%s", w.Code, w.Body.String())
	}
	// 404 for an unknown repository and for a bare prefix.
	if w := call(h, http.MethodGet, "/v1/repositories/missing", "", "owner"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown repository status=%d", w.Code)
	}
	if w := call(h, http.MethodPost, "/v1/repositories/"+id+":resurrect", `{"request_id":"x"}`, "owner"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown verb status=%d body=%s", w.Code, w.Body.String())
	}
}

// ===========================================================================
// A13: the Control Plane never invokes gh or git.
// ===========================================================================

// controlPlaneRoots are the directories that make up the Control Plane: the
// transport, the application services, the domain, both persistence adapters
// and the process entry point. internal/runner is deliberately absent: the
// forge adapter lives there precisely because that is the Runner, not the
// Control Plane.
var controlPlaneRoots = []string{
	filepath.Join("..", "api"),
	filepath.Join("..", "application"),
	filepath.Join("..", "domain"),
	filepath.Join("..", "store"),
	filepath.Join("..", "..", "cmd", "control-plane"),
}

// processSpawnFinding is one reason a file was rejected.
type processSpawnFinding struct {
	path   string
	reason string
}

// scanForProcessSpawn reads every non-test *.go file under root with
// go/parser and reports, from the AST rather than from file text:
//
//   - any import of os/exec, syscall or golang.org/x/sys (the only ways to
//     start a process in Go),
//   - any selector expression naming exec.Command, exec.CommandContext,
//     exec.LookPath, os.StartProcess or syscall.Exec,
//   - any string literal whose whitespace-separated tokens include "gh" or
//     "git", which is what a command line naming either tool looks like.
//
// Import path literals are exempt from the last rule: this module's own
// import prefix contains "github.com" and is not a command.
func scanForProcessSpawn(t *testing.T, root string) []processSpawnFinding {
	t.Helper()
	var findings []processSpawnFinding
	// os/exec is the only standard way to start a child process in Go, so it
	// is forbidden outright. A bare "syscall" import is NOT forbidden:
	// cmd/control-plane/main.go legitimately imports it for
	// signal.NotifyContext(os.Interrupt, syscall.SIGTERM), which starts
	// nothing. The process-starting syscall entry points are caught by
	// selector instead, below.
	forbiddenImports := map[string]bool{"os/exec": true}
	forbiddenSelectors := map[string]map[string]bool{
		"exec":    {"Command": true, "CommandContext": true, "LookPath": true},
		"os":      {"StartProcess": true},
		"syscall": {"Exec": true, "ForkExec": true, "StartProcess": true},
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		importLiterals := map[*ast.BasicLit]bool{}
		for _, imp := range file.Imports {
			importLiterals[imp.Path] = true
			value, unquoteErr := strconv.Unquote(imp.Path.Value)
			if unquoteErr != nil {
				continue
			}
			if forbiddenImports[value] || strings.HasPrefix(value, "golang.org/x/sys") {
				findings = append(findings, processSpawnFinding{path: path, reason: "imports " + value})
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				pkg, ok := node.X.(*ast.Ident)
				if !ok {
					return true
				}
				if names, ok := forbiddenSelectors[pkg.Name]; ok && names[node.Sel.Name] {
					findings = append(findings, processSpawnFinding{path: path, reason: "references " + pkg.Name + "." + node.Sel.Name})
				}
			case *ast.BasicLit:
				if node.Kind != token.STRING || importLiterals[node] {
					return true
				}
				value, unquoteErr := strconv.Unquote(node.Value)
				if unquoteErr != nil {
					return true
				}
				for _, word := range strings.FieldsFunc(value, func(r rune) bool {
					return r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\'' || r == ';' || r == '|' || r == '&'
				}) {
					if word == "gh" || word == "git" {
						findings = append(findings, processSpawnFinding{path: path, reason: "string literal names the external tool " + word})
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return findings
}

// TestControlPlaneNeverInvokesGhOrGit is the structural form of dp-v2-064 d6
// and docs/policies/README.md L42: the Control Plane holds no forge client,
// so reachability can only arrive as a Runner-submitted Observation.
func TestControlPlaneNeverInvokesGhOrGit(t *testing.T) {
	scanned := 0
	for _, root := range controlPlaneRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("control plane root %s is not readable: %v", root, err)
		}
		findings := scanForProcessSpawn(t, root)
		if len(findings) != 0 {
			sort.Slice(findings, func(i, j int) bool { return findings[i].path < findings[j].path })
			for _, f := range findings {
				t.Errorf("%s: %s", f.path, f.reason)
			}
			t.Fatalf("%s: the Control Plane must not be able to start a process or name gh/git", root)
		}
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				scanned++
			}
			return nil
		})
	}
	// The scan must not be able to pass because it read nothing.
	if scanned < 15 {
		t.Fatalf("scanned only %d non-test files across the Control Plane roots; the walk is not finding sources", scanned)
	}
	t.Logf("scanned non-test files=%d roots=%v", scanned, controlPlaneRoots)
}

// TestControlPlaneProcessSpawnScannerHasAPositiveControl plants each pattern
// the scanner is supposed to catch in a scratch directory and asserts the
// same scanner rejects it. Without this, a scanner that silently matched
// nothing would report the Control Plane clean.
func TestControlPlaneProcessSpawnScannerHasAPositiveControl(t *testing.T) {
	cases := map[string]string{
		"exec.Command": `package scratch

import "os/exec"

func spawn() { _ = exec.Command("/usr/bin/true") }
`,
		"exec.LookPath": `package scratch

import "os/exec"

func find() { _, _ = exec.LookPath("true") }
`,
		"gh in a string literal": `package scratch

func argv() []string { return []string{"gh", "api", "repos/o/n"} }
`,
		"git in a command line": `package scratch

func script() string { return "git push origin HEAD" }
`,
		"syscall.Exec": `package scratch

import "syscall"

func replace() { _ = syscall.Exec("/usr/bin/true", nil, nil) }
`,
		"os.StartProcess": `package scratch

import "os"

func spawn() { _, _ = os.StartProcess("/usr/bin/true", nil, nil) }
`,
	}
	for name, source := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		findings := scanForProcessSpawn(t, dir)
		if len(findings) == 0 {
			t.Fatalf("positive control %q was not detected by the scanner", name)
		}
		t.Logf("positive control %q detected: %s", name, findings[0].reason)
	}
	// Negative control: a file that merely imports this module (whose import
	// prefix contains "github.com") is not a finding, so the scan above is
	// not passing by accident of a broken matcher.
	dir := t.TempDir()
	clean := `package scratch

import "github.com/takushi/agentic-loop-foundation/v2/internal/domain"

var _ = domain.ControlAllow
`
	if err := os.WriteFile(filepath.Join(dir, "clean.go"), []byte(clean), 0o600); err != nil {
		t.Fatal(err)
	}
	if findings := scanForProcessSpawn(t, dir); len(findings) != 0 {
		t.Fatalf("negative control was flagged: %v", findings)
	}
}

// TestOwnerConsoleExposesTheRepositorySurface is A18's deterministic,
// browser-free assertion: the server-rendered owner console carries the
// Repository registration form and the list container, and the served assets
// carry the code that renders identity and executability. internal/web is
// still left without a test file of its own.
func TestOwnerConsoleExposesTheRepositorySurface(t *testing.T) {
	h := testHandler(t)
	w := call(h, http.MethodGet, "/owner/", "", "owner")
	if w.Code != 200 {
		t.Fatalf("owner console status=%d", w.Code)
	}
	html := w.Body.String()
	for _, want := range []string{`id="repository"`, `id="source-url"`, `id="default-branch"`, `id="repository-list"`, `id="repository-refresh"`, `id="repository-status"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console does not carry %s", want)
		}
	}
	// The pre-existing capture and control forms are untouched.
	for _, want := range []string{`id="capture"`, `id="control"`, `id="queue"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("owner console lost the pre-existing surface %s", want)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.js", "", "")
	if w.Code != 200 {
		t.Fatalf("owner.js status=%d", w.Code)
	}
	js := w.Body.String()
	for _, want := range []string{"/v1/repositories", "executability", "repository_id", "unobserved"} {
		if !strings.Contains(js, want) {
			t.Fatalf("owner.js does not reference %q", want)
		}
	}
	// No provider SDK, no external asset and no secret-bearing data path in
	// the served script. The HTML's own prose mentions credentials only to
	// state that they are never rendered, so the name scan applies to the
	// script; the HTML is scanned for external asset references instead.
	for _, forbidden := range []string{"token", "secret", "credential", "https://", "http://"} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("owner.js references %q", forbidden)
		}
	}
	for _, forbidden := range []string{"src=\"http", "href=\"http", "//cdn.", "integrity="} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("owner console loads an external asset (%q)", forbidden)
		}
	}
	w = call(h, http.MethodGet, "/owner/assets/owner.css", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "repositories") {
		t.Fatalf("owner.css status=%d body=%s", w.Code, w.Body.String())
	}
}
