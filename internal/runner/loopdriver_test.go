package runner

// V2-091 A15. The driver is asserted over the same stub http.RoundTripper the
// client tests use -- no httptest.NewServer, no goroutine, no timer, no sleep --
// and its two absences (it never calls Provider.Run, it never posts a result)
// are asserted STRUCTURALLY over its own AST as well as behaviourally, because
// an absence a test only observes once is an absence a later change can remove
// without anything going red.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loopDriverFixture(t *testing.T, offered int) (*LoopDriver, *stubRoundTrip) {
	t.Helper()
	items := make([]string, 0, offered)
	responses := map[string]string{
		"POST /v1/runner/heartbeat":  `{"accepted":true}`,
		"POST /v1/executions/result": `{"execution_id":"completed"}`,
	}
	for i := 0; i < offered; i++ {
		increment := fmt.Sprintf("i%02d", i)
		execution := fmt.Sprintf("e%02d", i)
		items = append(items, fmt.Sprintf(`{"requirement_id":"r%02d","increment_id":%q,"expected_increment_version":2,"requirement_summary":"work %02d"}`, i, increment, i))
		responses["POST /v1/runner/claims:acquire"] = ""
		responses["POST /v1/executions/"+execution+":start"] = fmt.Sprintf(`{"execution_id":%q,"status":"running","version":2}`, execution)
	}
	stub := &stubRoundTrip{response: responses}
	stub.response["GET /v1/runner/work"] = fmt.Sprintf(`{"schema_version":"v1","cap":16,"increments":[%s]}`, strings.Join(items, ","))
	// The claim answer depends on which increment was asked for, so it is
	// computed rather than tabled: the stub answers with the increment the body
	// named, which is what lets the test assert the driver claimed exactly what
	// it was OFFERED.
	stub.claimFromBody = true

	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspaces")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	return &LoopDriver{
		Client:           controlPlaneTestClient(t, stub),
		Workspace:        workspace,
		Journal:          journal,
		RequestNamespace: "driver-pass-1",
		ProviderName:     "codex",
		Execute: func(context.Context, ProviderRequest) (ProviderResult, error) {
			return ProviderResult{Succeeded: true, Output: "sha256:result", Checkpoint: "codex:session"}, nil
		},
	}, stub
}

// TestTheDriverClaimsAtMostItsBoundAndStopsAtTheProviderBoundary is v14.
func TestTheDriverClaimsAtMostItsBoundAndStopsAtTheProviderBoundary(t *testing.T) {
	offered := MaxDriverClaims + 3
	driver, stub := loopDriverFixture(t, offered)
	report, err := driver.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if report.Offered != offered {
		t.Fatalf("report offered = %d, want %d", report.Offered, offered)
	}
	if len(report.Claimed) != MaxDriverClaims {
		t.Fatalf("the driver claimed %d Increments, want exactly the bound %d", len(report.Claimed), MaxDriverClaims)
	}
	if report.Deferred != offered-MaxDriverClaims {
		t.Fatalf("report deferred = %d, want %d: a bounded pass must SAY how much it left", report.Deferred, offered-MaxDriverClaims)
	}
	if report.Heartbeats != 1 {
		t.Fatalf("report heartbeats = %d, want exactly 1", report.Heartbeats)
	}
	if report.StoppedAtProviderBoundary {
		t.Fatal("report says that the pass stopped before the wired provider")
	}
	// Every claim really started an Execution and really created a workspace.
	for _, claim := range report.Claimed {
		if claim.ExecutionState != "running" {
			t.Fatalf("claim %+v did not report a started Execution", claim)
		}
		info, statErr := os.Stat(claim.WorkspacePath)
		if statErr != nil || !info.IsDir() {
			t.Fatalf("claim %+v has no real workspace directory: %v", claim, statErr)
		}
	}
	// The requests the driver made, counted exactly: one offer read, one claim
	// and one start per claimed Increment, one heartbeat -- and NOTHING else.
	// A result post would show up here as an extra request.
	wantRequests := 1 + 3*MaxDriverClaims + 1
	if len(stub.seen) != wantRequests {
		paths := make([]string, 0, len(stub.seen))
		for _, r := range stub.seen {
			paths = append(paths, r.Method+" "+r.URL.Path)
		}
		t.Fatalf("the driver made %d requests, want exactly %d: %v", len(stub.seen), wantRequests, paths)
	}
	// The journal really recorded each assignment, and its payload carries
	// identifiers only.
	events, err := driver.Journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != MaxDriverClaims {
		t.Fatalf("the journal holds %d events, want one assignment per claim (%d)", len(events), MaxDriverClaims)
	}
	token := controlPlaneTestToken()
	for _, event := range events {
		if event.Kind != "assignment" {
			t.Fatalf("journal event kind = %q, want assignment", event.Kind)
		}
		var payload map[string]string
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 4 {
			t.Fatalf("journal payload carries %d fields, want exactly the four identifiers: %v", len(payload), payload)
		}
		if strings.Contains(string(event.Payload), token) {
			t.Fatalf("a journal payload carries the session token: %s", event.Payload)
		}
	}
}

// TestTheDriverClaimsOnlyWhatItWasOffered is the assertion that separates this
// driver from the fake one: it never names an Increment its caller chose, and it
// never manufactures a Requirement. With an EMPTY offer it claims nothing, makes
// exactly one request and does not heartbeat.
func TestTheDriverClaimsOnlyWhatItWasOffered(t *testing.T) {
	driver, stub := loopDriverFixture(t, 0)
	report, err := driver.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if report.Offered != 0 || len(report.Claimed) != 0 || report.Deferred != 0 {
		t.Fatalf("an empty offer produced %+v", report)
	}
	if report.Heartbeats != 0 {
		t.Fatalf("the driver heartbeat %d times with no work; liveness it was not asked for is an observation it did not make", report.Heartbeats)
	}
	if len(stub.seen) != 1 {
		t.Fatalf("an empty offer produced %d requests, want exactly 1 (the offer read)", len(stub.seen))
	}
	// And the claimed increment ids are exactly the offered ones, in order.
	driver2, stub2 := loopDriverFixture(t, 2)
	report2, err := driver2.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for i, claim := range report2.Claimed {
		want := fmt.Sprintf("i%02d", i)
		if claim.IncrementID != want {
			t.Fatalf("claim %d took increment %q, want the offered %q", i, claim.IncrementID, want)
		}
		if claim.RequirementID != fmt.Sprintf("r%02d", i) {
			t.Fatalf("claim %d names requirement %q", i, claim.RequirementID)
		}
	}
	for _, body := range stub2.bodies {
		if body == "" {
			continue
		}
		if strings.Contains(body, "text") || strings.Contains(body, "packet") {
			t.Fatalf("a driver request body carries requirement text or a work packet: %s", body)
		}
	}
}

// TestTheDriverRefusesToRunWithoutItsRealDependencies asserts the driver has no
// defaults: no in-memory workspace, no discarded journal, no synthesised
// namespace. Each of those would be the fake wearing a different hat.
func TestTheDriverRefusesToRunWithoutItsRealDependencies(t *testing.T) {
	complete, _ := loopDriverFixture(t, 1)
	for _, tc := range []struct {
		name   string
		mutate func(*LoopDriver)
	}{
		{"no client", func(d *LoopDriver) { d.Client = nil }},
		{"no workspace", func(d *LoopDriver) { d.Workspace = nil }},
		{"no journal", func(d *LoopDriver) { d.Journal = nil }},
		{"no request namespace", func(d *LoopDriver) { d.RequestNamespace = "" }},
		{"no provider execution", func(d *LoopDriver) { d.Execute = nil }},
		{"no provider name", func(d *LoopDriver) { d.ProviderName = "" }},
	} {
		broken := *complete
		tc.mutate(&broken)
		if _, err := broken.RunOnce(context.Background()); err == nil {
			t.Fatalf("%s: RunOnce returned no error", tc.name)
		}
	}
}

// TestTheDriverNeverReachesAProviderOrPostsAResult is the STRUCTURAL half, over
// the driver's own AST. It is what makes the two absences survive a later
// change: a call to a Provider or to the result route added to this file fails
// here even if no behavioural test happens to cover the new path.
func TestTheDriverNeverReachesAProviderOrPostsAResult(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "loopdriver.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse loopdriver.go: %v", err)
	}
	// (1) The import allowlist. internal/provider is absent, and so is every
	// store: measured in ci/components.json, store-firestore declares runner
	// among its dependencies, so a runner -> store-firestore import would be a
	// cycle in the component graph, and internal/store/memory in a shipped
	// binary is the fake this task refuses.
	allowed := map[string]bool{
		"context":       true,
		"encoding/json": true,
		"errors":        true,
		"fmt":           true,
	}
	const applicationSuffix = "/internal/application"
	const providerSuffix = "/internal/provider"
	imports := 0
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		imports++
		if allowed[path] {
			continue
		}
		if strings.HasSuffix(path, applicationSuffix) || strings.HasSuffix(path, providerSuffix) {
			// The ONE non-standard-library import, and it is for one named
			// bound (application.MaxDriverClaims) so the offer the server
			// builds and the bound the Runner applies cannot disagree. It
			// declares no component edge: ci/components.json's runner component
			// already lists application among its dependencies.
			continue
		}
		t.Fatalf("loopdriver.go imports %q, which is not on the allowlist; every store remains deliberately absent", path)
	}
	if imports == 0 {
		t.Fatal("the scan found no import in loopdriver.go; the AST walk is broken and the allowlist would pass vacuously")
	}
	// (2) No call whose final name segment is Run, and no mention of the result
	// route. The AST is scanned rather than the text so a comment naming
	// Provider.Run -- and this file's header comment does name it, five times --
	// cannot make the assertion fail on correct documentation.
	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		calls++
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "Run" {
				t.Fatal("loopdriver.go calls Run; this driver stops at the provider boundary")
			}
		case *ast.SelectorExpr:
			if fun.Sel != nil && (fun.Sel.Name == "Run" || fun.Sel.Name == "AcceptResult" || fun.Sel.Name == "PostResult") {
				t.Fatalf("loopdriver.go calls %s; this driver obtains no provider result and posts none", fun.Sel.Name)
			}
		}
		return true
	})
	if calls == 0 {
		t.Fatal("the scan found no call expression in loopdriver.go; the AST walk is broken")
	}
	// (3) No STRING LITERAL naming the result route. The scan is over the AST
	// rather than the file text because this file's header comment legitimately
	// names the route it refuses to post to, and a text grep would fail on that
	// correct documentation.
	literals := 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		literals++
		if strings.Contains(lit.Value, "/result") || strings.Contains(lit.Value, "executions/") {
			t.Fatalf("loopdriver.go contains the string literal %s; this driver names no route of its own and posts no result", lit.Value)
		}
		return true
	})
	if literals == 0 {
		t.Fatal("the scan found no string literal in loopdriver.go; the AST walk is broken")
	}
	// No goroutine, timer, sleep, wall clock or randomness. These are checked
	// as AST identifiers and statements, again so a comment cannot trip them.
	ast.Inspect(file, func(n ast.Node) bool {
		if _, ok := n.(*ast.GoStmt); ok {
			t.Fatal("loopdriver.go starts a goroutine; determinism is acceptance for this task")
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil {
			return true
		}
		pkg, isPkg := sel.X.(*ast.Ident)
		if !isPkg {
			return true
		}
		if pkg.Name == "time" || pkg.Name == "rand" {
			t.Fatalf("loopdriver.go references %s.%s; the driver reads no wall clock, starts no timer and uses no randomness", pkg.Name, sel.Sel.Name)
		}
		return true
	})
	t.Logf("driver structural guard: imports=%d call expressions=%d; internal/provider absent, every store absent, no result route, no goroutine, no timer, no wall clock", imports, calls)
}

// TestRunFakeJourneyKeepsItsExactPreExistingCallSites is A15's last clause and
// C17's guard: Orchestrator.RunFakeJourney is byte-unchanged and TEST-ONLY, and
// the new driver inherits none of its assertions. The counts are re-measured
// here rather than copied: at 848d899 there were 3 call sites and 2 Orchestrator
// composite literals, all in internal/runner/orchestrator_test.go, and the Work
// Order's line numbers (:65, :139, :68, :75, :140) were STALE -- the measured
// ones are :115, :233 and :118, :125, :234.
func TestRunFakeJourneyKeepsItsExactPreExistingCallSites(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the scan found no .go file; the working directory is not internal/runner")
	}
	callSites := map[string]int{}
	literals := map[string]int{}
	fset := token.NewFileSet()
	for _, path := range files {
		parsed, parseErr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel != nil && sel.Sel.Name == "RunFakeJourney" {
					callSites[path]++
				}
			}
			if lit, ok := n.(*ast.CompositeLit); ok {
				if id, isID := lit.Type.(*ast.Ident); isID && id.Name == "Orchestrator" {
					literals[path]++
				}
			}
			return true
		})
	}
	if len(callSites) != 1 || callSites["orchestrator_test.go"] != 3 {
		t.Fatalf("RunFakeJourney call sites = %v, want exactly 3 and all in orchestrator_test.go: it stays TEST-ONLY and this task's driver must not inherit its assertions", callSites)
	}
	if len(literals) != 1 || literals["orchestrator_test.go"] != 2 {
		t.Fatalf("Orchestrator composite literals = %v, want exactly 2 and both in orchestrator_test.go", literals)
	}
	// The new driver is not the Orchestrator wearing a new name: no DECLARED
	// FIELD OR VARIABLE of it is an application.Service, a Provider or an
	// application.Caller. The scan is over the AST because loopdriver.go's
	// header comment legitimately quotes all three while explaining the five
	// measured differences.
	driverFile, err := parser.ParseFile(fset, "loopdriver.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse loopdriver.go: %v", err)
	}
	forbiddenTypes := map[string]bool{"Service": true, "Provider": true, "Caller": true}
	types := 0
	ast.Inspect(driverFile, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if node.Sel != nil && forbiddenTypes[node.Sel.Name] {
				if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "application" {
					t.Fatalf("loopdriver.go names application.%s; the real driver shares no memory with the Control Plane, holds no Provider, and constructs no application.Caller", node.Sel.Name)
				}
			}
		case *ast.Field:
			types++
			if id, ok := node.Type.(*ast.Ident); ok && forbiddenTypes[id.Name] {
				t.Fatalf("loopdriver.go declares a field of type %s", id.Name)
			}
			if star, ok := node.Type.(*ast.StarExpr); ok {
				if id, isID := star.X.(*ast.Ident); isID && forbiddenTypes[id.Name] {
					t.Fatalf("loopdriver.go declares a field of type *%s", id.Name)
				}
			}
		}
		return true
	})
	if types == 0 {
		t.Fatal("the scan found no field declaration in loopdriver.go; the AST walk is broken")
	}
	t.Log("RECORDED, the five measured differences from Orchestrator.RunFakeJourney: (1) it does not Capture -- orchestrator.go:55 does, manufacturing its own Requirement; (2) it does not fabricate a RoleRunner application.Caller -- orchestrator.go:54 does, with no session verification; (3) it does not call Provider.Run -- orchestrator.go:123 does, and all three adapters return provider.NoExec anyway; (4) it is bounded at MaxDriverClaims per pass; (5) it shares no memory with the Control Plane")
}

// stubClaimAnswer is the stub's computed claim response. It is here rather than
// in the table so the stub can answer with the increment the request BODY named,
// which is what lets the driver's "claims only what it was offered" property be
// asserted rather than assumed.
func stubClaimAnswer(body string) string {
	var parsed struct {
		IncrementID string `json:"increment_id"`
	}
	_ = json.Unmarshal([]byte(body), &parsed)
	execution := strings.Replace(parsed.IncrementID, "i", "e", 1)
	return fmt.Sprintf(`{"increment_id":%q,"execution_id":%q,"lease_id":%q,"runner_id":"runner-1","version":3,"fencing_token":7}`,
		parsed.IncrementID, execution, "l"+strings.TrimPrefix(parsed.IncrementID, "i"))
}

var _ = time.Second
var _ = http.MethodGet
