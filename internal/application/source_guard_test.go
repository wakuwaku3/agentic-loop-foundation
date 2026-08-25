package application_test

// This file proves, mechanically from source rather than by review, the one
// structural property the Provider registry rests on: internal/application has
// no Provider probe path.
//
// Why a source scan and not a review note. An active health probe IS an
// invocation, and every invocation on the Loop's path passes
// internal/runner.CostLedger.Reserve, which counts against the
// runaway-detection thresholds. A registry that probed on every read would
// consume the counters of the detector it exists to report and could itself
// trip the halt it is meant to describe. docs/architecture/validation.md
// section 5 lists "provider health probe does not grow with Requirement count"
// as a growth rate to verify; a passive derivation satisfies it at zero, and
// this scan is what makes that a measured fact instead of a sentence in a
// document. It is also the only durable defence against a later change quietly
// adding a probe.
//
// Modelled on internal/domain/source_guard_test.go: it parses the package's
// own *.go files with go/parser, and it fails outright on a zero-file scan so
// a mis-written glob cannot make the assertions pass vacuously.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// forbiddenProbeImports must appear in no file of this package at all,
// production or test. Each entry is matched as the exact path or as that path
// followed by a slash, so net/http/httptest is caught by the net/http entry.
//
// os/exec and net/http are how a probe would start a CLI or call an API.
// net/url is how a probe target would be built. internal/provider and
// internal/runner are the two packages that know how to invoke a Provider at
// all; not importing them is also what keeps this component's declared
// dependency edges unchanged, which only V2-045 may alter.
//
// The two internal paths are assembled from internalPrefix rather than written
// as literals. internal/ci's manifest derivation reads module-path literals in
// source text -- test files included -- and treats one as a declared component
// edge, so spelling either path out here would move the declared component DAG,
// which only V2-045 may do. runner_version_test.go states the same reason for
// the same construction.
var forbiddenProbeImports = []string{
	"os/exec",
	"net/http",
	"net/url",
	internalPrefix + "provider",
	internalPrefix + "runner",
}

// internalPrefix is the module path of this repository's internal packages.
const internalPrefix = "github.com/takushi/agentic-loop-foundation/v2/internal/"

// netImportAllowlist is the complete set of files in this package that may
// import "net" at all, with the reason.
//
// MEASURED DISCREPANCY with V2-067 A11, recorded rather than papered over. A11
// asks for a scan asserting that no file imports "net". That is not the tree:
// internal/application/outbox.go has imported "net" since before this task, and
// it uses it for exactly two identifiers, net.ErrClosed and net.Error, to
// classify an error it was handed. Neither can open a connection, resolve a
// name or start a process, so neither is a probe.
//
// Deleting that import is outside this task's allowed paths and would change
// outbox error classification, so the guard is made STRONGER instead of
// weaker: "net" is allowed in exactly one named file, and every net.X selector
// anywhere in the package must be in netSelectorAllowlist below. A net.Dial,
// net.Listen, net.LookupHost or net.Resolver appearing in outbox.go -- the one
// file that may import the package at all -- therefore fails this test.
var netImportAllowlist = map[string]string{
	"outbox.go": "net.ErrClosed and net.Error, for classifying an error the dispatcher was handed; neither opens a connection",
}

// netSelectorAllowlist is the complete set of net package members any file in
// this package may name. Both are error values or error interfaces.
var netSelectorAllowlist = map[string]bool{
	"ErrClosed": true,
	"Error":     true,
}

// probeGuardFile is one parsed *.go file of internal/application.
type probeGuardFile struct {
	path    string
	isTest  bool
	file    *ast.File
	imports []string
}

// parseApplicationPackageForProbeGuard parses every *.go file in the current
// directory. go test always runs with the package directory as its working
// directory. A zero-file scan is a Fatal, so no assertion below can pass
// vacuously.
func parseApplicationPackageForProbeGuard(t *testing.T) []probeGuardFile {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("source guard scanned zero files; the working directory is not internal/application or the glob is broken")
	}
	sort.Strings(matches)
	fset := token.NewFileSet()
	out := make([]probeGuardFile, 0, len(matches))
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		imports := make([]string, 0, len(file.Imports))
		for _, imp := range file.Imports {
			value, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import literal %s: %v", path, imp.Path.Value, err)
			}
			imports = append(imports, value)
		}
		out = append(out, probeGuardFile{path: path, isTest: strings.HasSuffix(path, "_test.go"), file: file, imports: imports})
	}
	return out
}

// importIsForbiddenProbePath is the matcher. It matches the exact path or that
// path followed by a slash: prefix-without-slash matching would make "net"
// forbid "netip", and equality-only matching would let net/http/httptest
// through.
func importIsForbiddenProbePath(path string) (string, bool) {
	for _, forbidden := range forbiddenProbeImports {
		if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
			return forbidden, true
		}
	}
	return "", false
}

// TestProbeImportMatcherIsVerified checks the matcher against known-positive
// and known-negative paths before either scan below trusts it. A guard whose
// matcher was never exercised is a guard that can quietly stop matching.
func TestProbeImportMatcherIsVerified(t *testing.T) {
	positives := []string{
		"os/exec",
		"net/http",
		"net/http/httptest",
		"net/url",
		internalPrefix + "provider",
		internalPrefix + "runner",
		internalPrefix + "runner/costledger",
	}
	for _, path := range positives {
		if _, ok := importIsForbiddenProbePath(path); !ok {
			t.Fatalf("known-positive import %q was not matched by the probe-path matcher", path)
		}
	}
	negatives := []string{
		"os",
		"net",
		"netip",
		"context",
		"encoding/json",
		internalPrefix + "domain",
		internalPrefix + "quota",
		internalPrefix + "release",
		// A path that merely starts with a forbidden component's name is not
		// that component: the matcher requires the exact path or a slash.
		internalPrefix + "providerregistryless",
	}
	for _, path := range negatives {
		if entry, ok := importIsForbiddenProbePath(path); ok {
			t.Fatalf("known-negative import %q was matched on %q", path, entry)
		}
	}
	// And the synthetic positive control for the net-selector half: a file
	// that imports net and dials must be caught by the selector allowlist.
	const dialer = `package application
import "net"
func probe() { _, _ = net.Dial("tcp", "example.invalid:443") }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic_probe.go", dialer, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	offending := netSelectorsIn(file)
	if len(offending) != 1 || offending[0] != "Dial" {
		t.Fatalf("positive control: net selectors found = %v, want exactly [Dial]", offending)
	}
	if netSelectorAllowlist["Dial"] {
		t.Fatal("the net selector allowlist admits Dial; the guard would not catch a probe")
	}
	// The synthetic negative control: the two members outbox.go really uses.
	const classifier = `package application
import "net"
func classify(err error) bool { var n net.Error; _ = n; return err == net.ErrClosed }
`
	file, err = parser.ParseFile(fset, "synthetic_classifier.go", classifier, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	found := netSelectorsIn(file)
	sort.Strings(found)
	if len(found) != 2 || found[0] != "ErrClosed" || found[1] != "Error" {
		t.Fatalf("negative control: net selectors found = %v, want [ErrClosed Error]", found)
	}
	for _, name := range found {
		if !netSelectorAllowlist[name] {
			t.Fatalf("negative control: %q is not in the net selector allowlist", name)
		}
	}
}

// netSelectorsIn returns every net.X selector name used in a file.
func netSelectorsIn(file *ast.File) []string {
	out := []string{}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "net" {
			return true
		}
		out = append(out, sel.Sel.Name)
		return true
	})
	return out
}

// TestApplicationHasNoProviderProbePath is V2-067 A11. It is the mechanical
// proof that the number of Provider CLI invocations caused by one
// GET /v1/providers is exactly zero: the package cannot start a process, cannot
// speak HTTP, cannot build a URL, and cannot reach either of the two packages
// that know how to invoke a Provider.
func TestApplicationHasNoProviderProbePath(t *testing.T) {
	files := parseApplicationPackageForProbeGuard(t)
	const wantMinScannedFiles = 12
	if len(files) < wantMinScannedFiles {
		t.Fatalf("scanned %d files, want at least %d; the scan is not seeing the package", len(files), wantMinScannedFiles)
	}

	nonTest, test, totalImports := 0, 0, 0
	netImporters := map[string]bool{}
	for _, pf := range files {
		if pf.isTest {
			test++
		} else {
			nonTest++
		}
		for _, path := range pf.imports {
			totalImports++
			if entry, bad := importIsForbiddenProbePath(path); bad {
				t.Errorf("%s imports %q (matches the forbidden probe path %q); the registry must derive health from recorded observations only", pf.path, path, entry)
			}
			if path == "net" {
				netImporters[pf.path] = true
			}
		}
	}
	if nonTest == 0 {
		t.Fatal("scanned zero non-test files")
	}
	if test == 0 {
		t.Fatal("scanned zero test files")
	}
	if totalImports == 0 {
		t.Fatal("scanned zero import paths; the AST walk is not finding declarations")
	}

	// "net" is admitted in exactly the named files, for the recorded reason.
	for path := range netImporters {
		if _, ok := netImportAllowlist[path]; !ok {
			t.Errorf("%s imports \"net\"; only %v may, and only for error classification", path, keysOf(netImportAllowlist))
		}
	}
	for path := range netImportAllowlist {
		if !netImporters[path] {
			t.Errorf("the net import allowlist names %q, which does not import \"net\"; the allowlist is stale and reads stronger than it is", path)
		}
	}

	// And no file may name any net member outside the two error-classification
	// identifiers, so the one admitted import cannot grow into a dialer.
	selectorSites := 0
	for _, pf := range files {
		for _, name := range netSelectorsIn(pf.file) {
			selectorSites++
			if !netSelectorAllowlist[name] {
				t.Errorf("%s names net.%s; only %v are permitted, because neither can open a connection", pf.path, name, keysOf(netSelectorAllowlist))
			}
		}
	}
	if selectorSites == 0 {
		t.Fatal("found zero net selectors; outbox.go is expected to use two, so the selector scan is broken and would pass vacuously")
	}
	t.Logf("probe guard scanned files=%d (non-test=%d test=%d) imports=%d net-selectors=%d; provider CLI invocations caused by one GET /v1/providers = 0",
		len(files), nonTest, test, totalImports, selectorSites)
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestNoInvocationSeamExistsInTheProviderReadPath is the second half of A11:
// the absence of an import is necessary but not sufficient, so the read path is
// also asserted to name no invocation seam. The identifier scan is over the
// whole package's AST, so a seam smuggled in through an interface declared here
// rather than imported from elsewhere is caught too.
func TestNoInvocationSeamExistsInTheProviderReadPath(t *testing.T) {
	forbidden := []string{
		"InvocationRunner", "SupervisedInvocationRunner", "CostLedger", "ProviderRequest",
		"Invoke", "Invocation", "Spawn", "StartProcess", "Command", "CommandContext",
		"Dial", "DialContext", "Listen", "LookupHost", "Resolver", "Transport", "RoundTrip",
		"Probe", "ProbeProvider", "HealthCheck", "Ping",
	}
	// The matcher is verified first: a synthetic declaration naming one of the
	// identifiers must be found.
	const synthetic = `package application
type syntheticSeam interface { Invoke() error }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic_seam.go", synthetic, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if hits := identifierHits(file, forbidden); len(hits) != 1 || hits[0] != "Invoke" {
		t.Fatalf("positive control: identifier hits = %v, want exactly [Invoke]", hits)
	}

	files := parseApplicationPackageForProbeGuard(t)
	scanned := 0
	for _, pf := range files {
		if pf.isTest {
			// Test files legitimately name Probe-shaped helpers of their own;
			// the guarantee this test makes is about the shipped code.
			continue
		}
		scanned++
		if hits := identifierHits(pf.file, forbidden); len(hits) > 0 {
			t.Errorf("%s names %v; internal/application must contain no invocation or probe seam", pf.path, hits)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned zero non-test files")
	}
	t.Logf("invocation-seam scan covered %d non-test files and found none of the %d forbidden identifiers", scanned, len(forbidden))
}

func identifierHits(file *ast.File, forbidden []string) []string {
	want := map[string]bool{}
	for _, name := range forbidden {
		want[name] = true
	}
	seen := map[string]bool{}
	out := []string{}
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || !want[ident.Name] || seen[ident.Name] {
			return true
		}
		seen[ident.Name] = true
		out = append(out, ident.Name)
		return true
	})
	sort.Strings(out)
	return out
}
