package provider_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

func packet() provider.WorkPacket {
	return provider.WorkPacket{Version: provider.ContractVersion, RequirementID: "req-1", RequirementSummary: "fix the queue", IncrementID: "inc-1", Constraints: []string{"bounded"}}
}
func TestAdaptersBuildSafeArgvAndParseMetadata(t *testing.T) {
	for _, a := range []provider.Adapter{provider.CodexAdapter{}, provider.ClaudeAdapter{}, provider.OpenCodeAdapter{}} {
		inv, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()})
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.Argv) < 3 || strings.Contains(string(inv.Stdin), "credential") {
			t.Fatalf("unsafe invocation %#v", inv)
		}
		if inv.WorkingDirectory != "/workspace" {
			t.Fatalf("workspace boundary missing: %#v", inv)
		}
		r, err := a.Parse([]byte(`{"status":"completed","checkpoint":"cp-1","output":"provider conversation omitted","usage":{"input_tokens":2,"output_tokens":3}}`))
		if err != nil || !r.Succeeded || r.OutputDigest == "" || r.Usage.TotalTokens != 0 {
			t.Fatalf("result=%#v err=%v", r, err)
		}
	}
}
func TestFixturesRejectSecretsAndMalformed(t *testing.T) {
	a := provider.CodexAdapter{}
	if _, err := a.Parse([]byte(`{"status":"success","output":"Bearer abcdefghijklmnop"}`)); err == nil {
		t.Fatal("secret fixture accepted")
	}
	if _, err := a.Parse([]byte(`{"status":"success"`)); err == nil {
		t.Fatal("malformed fixture accepted")
	}
}
func TestFailureClassificationAndHandoff(t *testing.T) {
	if f := provider.ClassifyError(context.Canceled); f.Class != provider.FailureCancelled || f.Retryable {
		t.Fatalf("cancel=%#v", f)
	}
	if f := provider.ClassifyError(context.DeadlineExceeded); f.Class != provider.FailureTimeout || !f.Ambiguous {
		t.Fatalf("timeout=%#v", f)
	}
	h, err := provider.PrepareHandoff("codex", "claude", packet(), provider.Result{Provider: "codex", Failure: &provider.Failure{Class: provider.FailureQuota}, Usage: provider.Usage{InputTokens: 1}})
	if err != nil || h.ToProvider != "claude" || h.Packet.RequirementID != "req-1" {
		t.Fatalf("handoff=%#v err=%v", h, err)
	}
	if _, err := provider.PrepareHandoff("codex", "claude", provider.WorkPacket{RequirementID: "r", IncrementID: "i", RequirementSummary: "credential"}, provider.Result{Provider: "codex"}); !errors.Is(err, provider.ErrInvalidPacket) {
		t.Fatalf("unsafe packet err=%v", err)
	}
	_ = time.Second
}

// ===========================================================================
// V2-067 A3, first of two independent pins of the closed Provider identity.
//
// The set {codex, claude, opencode} is pinned twice, in two packages, rather
// than shared by an import: internal/application declares its own table and
// must not import this package (dp-v2-067 d2), which keeps the provider
// component a leaf in ci/components.json and adds no component edge. The cost
// is that the two declarations can drift, and these two tests are the only
// thing that will catch it. If a fourth Provider is ever added, both tables
// must change together.
// ===========================================================================

// providerPackageFiles parses every non-test *.go file in this package
// directory. go test always runs with the package directory as its working
// directory, and a zero-file scan is a Fatal, so no assertion below can pass
// vacuously.
func providerPackageFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out[path] = file
	}
	if len(out) == 0 {
		t.Fatal("scanned zero non-test .go files; the working directory is not internal/provider or the glob is broken")
	}
	return out
}

// receiverTypeName returns the bare type name of a method receiver, following
// one level of pointer, or "" when the declaration is not a method.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}

func TestProviderIdentityIsExactlyThreeAdapterNames(t *testing.T) {
	// Half one: the runtime values are exactly the three declared names, in
	// the documented order the standing authorization record and
	// contracts/schemas/provider-standing-authorization.json's enum use.
	want := []string{"codex", "claude", "opencode"}
	got := []string{provider.CodexAdapter{}.Name(), provider.ClaudeAdapter{}.Name(), provider.OpenCodeAdapter{}.Name()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter names = %v, want exactly %v", got, want)
	}
	// No two of them collide, so the name really is an identity.
	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Fatalf("adapter name %q is declared twice; a name cannot be an identity", name)
		}
		seen[name] = true
	}

	// Half two: no fourth adapter type exists in the package. The scan is
	// over declarations, not over the three literals above, so a fourth
	// adapter added without touching this list still fails.
	files := providerPackageFiles(t)
	namers := map[string]bool{}
	adapterTypes := map[string]bool{}
	for path, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Name" {
				if recv := receiverTypeName(fn); recv != "" {
					namers[recv] = true
				}
				continue
			}
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil {
					continue
				}
				// Only concrete struct types count: Adapter itself is the
				// interface every adapter satisfies, not a fourth adapter.
				if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
					continue
				}
				if strings.HasSuffix(ts.Name.Name, "Adapter") {
					adapterTypes[ts.Name.Name] = true
					_ = path
				}
			}
		}
	}
	wantTypes := map[string]bool{"CodexAdapter": true, "ClaudeAdapter": true, "OpenCodeAdapter": true}
	if !reflect.DeepEqual(adapterTypes, wantTypes) {
		t.Fatalf("declared *Adapter types = %v, want exactly %v; a fourth adapter type may not be added by this task", adapterTypes, wantTypes)
	}
	if !reflect.DeepEqual(namers, wantTypes) {
		t.Fatalf("types declaring a Name() method = %v, want exactly %v", namers, wantTypes)
	}
	if len(adapterTypes) != len(want) {
		t.Fatalf("%d adapter types for %d declared names", len(adapterTypes), len(want))
	}
	t.Logf("scanned %d non-test files; adapter identity is the closed set %v", len(files), want)
}

// TestUnauthenticatedIsItsOwnFailureClass is dp-v2-067 d9. Before this
// constant existed, every one of these inputs normalised to
// FailureTransport, which reads as an infrastructure fault; the state that
// actually blocks the milestone was therefore invisible in the record meant
// to reveal it.
func TestUnauthenticatedIsItsOwnFailureClass(t *testing.T) {
	if provider.FailureUnauthenticated == provider.FailureTransport {
		t.Fatal("FailureUnauthenticated must not be an alias of FailureTransport")
	}
	if provider.FailureUnauthenticated != "provider-unauthenticated" {
		t.Fatalf("FailureUnauthenticated = %q", provider.FailureUnauthenticated)
	}
	unauthenticated := []string{
		"Error: not logged in. Run `codex login` first.",
		"you are not authenticated; please sign in to continue",
		"unauthorized",
		"HTTP 401 Unauthorized",
		"authentication required",
		"no active session found for this machine",
	}
	for _, a := range []provider.Adapter{provider.CodexAdapter{}, provider.ClaudeAdapter{}, provider.OpenCodeAdapter{}} {
		for _, text := range unauthenticated {
			f := a.NormalizeError(errors.New(text))
			if f.Class != provider.FailureUnauthenticated {
				t.Fatalf("%s: %q normalised to %q, want %q", a.Name(), text, f.Class, provider.FailureUnauthenticated)
			}
			if f.Retryable {
				t.Fatalf("%s: %q is retryable; retrying cannot create a session, because authenticating a CLI uses the owner's own identity", a.Name(), text)
			}
			// No provider text is copied into the Failure.
			if strings.Contains(f.Message, "login") || strings.Contains(f.Message, "401") || f.Message != "provider cli has no authenticated session on this machine" {
				t.Fatalf("%s: message %q is not the fixed literal", a.Name(), f.Message)
			}
		}
	}
	// The negative half: an ordinary transport failure is still transport, so
	// the new class did not swallow the old one.
	for _, text := range []string{"connection reset by peer", "dial tcp: no route to host"} {
		if f := (provider.CodexAdapter{}).NormalizeError(errors.New(text)); f.Class != provider.FailureTransport {
			t.Fatalf("%q normalised to %q, want %q", text, f.Class, provider.FailureTransport)
		}
	}
}
