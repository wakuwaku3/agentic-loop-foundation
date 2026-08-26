package application

// V2-089 A7 and A20. This file is an IN-PACKAGE test file (package
// application, not application_test) for one measured reason: A7 asserts the
// domain equivalence by CALLING requirementStatusAdmitsClaim, which is
// unexported, and A20 asserts that the four statuses are written out exactly
// once. An in-package test file cannot import internal/store/memory -- measured
// verbatim, `imports .../internal/store/memory from claimable_test.go / imports
// .../internal/application from store.go: import cycle not allowed in test` --
// so the Service-level eleven-cell table (A8), the two unknown-parent cells
// over a real store (A9), the precedence (A11) and the renewal (A12) live in
// internal/application/service_test.go, which is package application_test and
// is named in this task's allowed_paths. Splitting on the language's own rule is
// the reason; nothing was dropped.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// claimableDomainDir is the package this task must not edit. It is read here,
// never written, in the shape internal/application/allocation_test.go's
// allocDomainDir already uses -- a relative directory literal and never a module
// path, because internal/ci's manifest check reads a module-path literal as an
// undeclared component edge.
const claimableDomainDir = "../domain"

// claimableParseDir parses every non-test *.go file in dir and fails outright on
// a zero-file parse, so no assertion below can pass vacuously.
func claimableParseDir(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		out[filepath.Base(path)] = file
	}
	if len(out) == 0 {
		t.Fatalf("parsed zero non-test files in %s; the scan is broken", dir)
	}
	return out
}

// requirementStatusConstants returns the declared literal value of every
// constant typed RequirementStatus in the given parsed files. It is the matcher
// A7 requires, and TestTheRequirementStatusAxisIsDerivedFromSource verifies it
// against a synthetic known-positive before trusting it.
func requirementStatusConstants(files map[string]*ast.File) map[string]string {
	out := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok.String() != "const" {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || value.Type == nil {
					continue
				}
				ident, ok := value.Type.(*ast.Ident)
				if !ok || ident.Name != "RequirementStatus" {
					continue
				}
				for i, name := range value.Names {
					if i < len(value.Values) {
						if lit, isLit := value.Values[i].(*ast.BasicLit); isLit {
							literal, _ := strconv.Unquote(lit.Value)
							out[name.Name] = literal
						}
					}
				}
			}
		}
	}
	return out
}

// claimableStatusAxis is the COMPLETE RequirementStatus axis, derived from
// internal/domain's source rather than hand-listed, so a twelfth status added
// later enters every table below unclassified instead of quietly defaulting.
func claimableStatusAxis(t *testing.T) []domain.RequirementStatus {
	t.Helper()
	declared := requirementStatusConstants(claimableParseDir(t, claimableDomainDir))
	if len(declared) == 0 {
		t.Fatal("the RequirementStatus scan found zero constants; the matcher is broken")
	}
	if len(declared) < 11 {
		t.Fatalf("the RequirementStatus axis has %d members, want at least 11: %v", len(declared), declared)
	}
	values := make([]string, 0, len(declared))
	for _, v := range declared {
		values = append(values, v)
	}
	sort.Strings(values)
	out := make([]domain.RequirementStatus, 0, len(values))
	for _, v := range values {
		out = append(out, domain.RequirementStatus(v))
	}
	return out
}

// TestTheRequirementStatusAxisIsDerivedFromSource verifies the matcher against a
// synthetic known-positive first, exactly as
// internal/application/framing_test.go explains, and then records the axis it
// derived. A matcher proved on a synthetic file cannot be silently mis-written.
func TestTheRequirementStatusAxisIsDerivedFromSource(t *testing.T) {
	positive := "package domain\n\ntype RequirementStatus string\n\nconst (\n\tSynthOne RequirementStatus = \"synth-one\"\n\tSynthTwo RequirementStatus = \"synth-two\"\n)\n"
	synthetic, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	got := requirementStatusConstants(map[string]*ast.File{"positive.go": synthetic})
	if len(got) != 2 || got["SynthOne"] != "synth-one" || got["SynthTwo"] != "synth-two" {
		t.Fatalf("positive control: the synthetic RequirementStatus constants were not matched: %v", got)
	}
	// A negative control too: a constant of another type must not be counted.
	negative := "package domain\n\nconst Other ControlMode = \"other\"\n"
	syntheticNegative, err := parser.ParseFile(token.NewFileSet(), "negative.go", negative, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(requirementStatusConstants(map[string]*ast.File{"negative.go": syntheticNegative})); n != 0 {
		t.Fatalf("negative control: the matcher counted %d constants of another type", n)
	}

	axis := claimableStatusAxis(t)
	if len(axis) != 11 {
		t.Logf("the derived RequirementStatus axis has %d members, not the 11 measured at V2-089; the tables below are still exhaustive over it", len(axis))
	}
	t.Logf("A7 derived RequirementStatus axis (%d members): %v", len(axis), axis)
}

// TestRequirementStatusAdmitsClaimEqualsTheDomainsOwnStartSources is A7, and it
// is what makes widening the admitting set a BUILD FAILURE rather than a
// decision.
//
// For every status on the go/ast-derived axis it asserts
//
//	requirementStatusAdmitsClaim(s) == (DecideRequirement(s, RequirementStart) succeeds || s == active)
//
// internal/domain/** is prohibited to this task, so the only way to make
// `captured` admitted is to widen internal/domain/model.go's allowed(...) call
// in a file V2-089 may not open. A disagreement here is a stop-and-escalate,
// never a switch case to add.
func TestRequirementStatusAdmitsClaimEqualsTheDomainsOwnStartSources(t *testing.T) {
	axis := claimableStatusAxis(t)
	id, err := domain.NewRequirementID("equivalence")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActorID("equivalence-actor")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_700_900_000, 0).UTC()
	const version = domain.Version(7)
	// Every candidate carries a complete StableReleaseSnapshot so domain.Validate
	// passes for EVERY status on the axis, including completed
	// (internal/domain/model.go rejects a completed Requirement with an
	// incomplete snapshot). Without it the completed cell would answer "not
	// admitted" because the record failed validation rather than because the
	// transition table refused, which is a vacuous pass. The digests are built
	// by concatenating a prefix with a suffix so no secret-shaped literal exists
	// in this file.
	snapshot := domain.StableReleaseSnapshot{
		ReleaseID:      domain.ReleaseID("release-" + "equivalence"),
		ReleaseVersion: 1,
		BundleDigest:   "bundle-" + "equivalence-digest",
		EvidenceDigest: "evidence-" + "equivalence-digest",
	}

	admitted, refused := []string{}, []string{}
	for _, status := range axis {
		current := domain.Requirement{ID: id, Status: status, Version: version, StableSnapshot: snapshot}
		_, decideErr := domain.DecideRequirement(current, domain.RequirementCommand{
			Kind:            domain.RequirementStart,
			Actor:           actor,
			At:              at,
			ExpectedVersion: version,
		})
		domainAdmits := decideErr == nil
		want := domainAdmits || status == domain.RequirementActive
		got := requirementStatusAdmitsClaim(status)
		if got != want {
			t.Fatalf("requirementStatusAdmitsClaim(%q) = %v, but the domain's own answer is %v (DecideRequirement error %v) and status==active is %v. "+
				"The admitting set is DERIVED: it is {s : DecideRequirement admits RequirementStart from s} union {active}. "+
				"Do NOT add a case to the switch; internal/domain is prohibited to this task, so this disagreement is a stop-and-escalate.",
				status, got, domainAdmits, decideErr, status == domain.RequirementActive)
		}
		if got {
			admitted = append(admitted, string(status))
		} else {
			refused = append(refused, string(status))
		}
	}
	if len(admitted) != 4 {
		t.Fatalf("the admitting set has %d members, want exactly 4: %v", len(admitted), admitted)
	}
	for _, want := range []domain.RequirementStatus{domain.RequirementReady, domain.RequirementActive, domain.RequirementWaiting, domain.RequirementRecovering} {
		found := false
		for _, got := range admitted {
			if got == string(want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q is not in the admitting set %v, and the domain admits RequirementStart from it", want, admitted)
		}
	}
	sort.Strings(admitted)
	sort.Strings(refused)
	t.Logf("A7 ELEVEN ANSWERS over the go/ast-derived axis. ADMITTED (%d): %v. REFUSED (%d): %v. "+
		"Each admitted status is one the domain's RequirementStart branch accepts, plus active itself; each refused status is one it does not.",
		len(admitted), admitted, len(refused), refused)
}

// TestAnIncrementWithNoParentLinkIsRefusedWithoutTouchingTheStore is the first
// half of A9, asserted at the altitude where it is cheapest and sharpest: the
// reader answers "does not admit work" for an empty RequirementID BEFORE it
// reads anything, which is provable by passing a nil UnitOfWork. A guard that
// dereferenced the store first would panic here.
//
// The polarity is the whole reason this is a second helper rather than a
// widening of requirementAwaitsHumanInput: that helper returns FALSE for an
// empty link and is right to, because it asks the narrower question "is the
// parent in needs-input". The two questions have opposite correct answers on the
// unknown case, so one helper would have to get one of them wrong.
func TestAnIncrementWithNoParentLinkIsRefusedWithoutTouchingTheStore(t *testing.T) {
	admits, err := requirementAdmitsClaim(context.Background(), nil, domain.Increment{})
	if err != nil {
		t.Fatalf("an Increment with no parent link produced an error rather than a refusal: %v", err)
	}
	if admits {
		t.Fatal("an Increment whose RequirementID is empty was reported as admitting work; docs/product/definition.md:111's 実行可能 cannot be established for a record that does not exist")
	}
	// The opposite polarity of the same case, on the helper V2-065 shipped.
	waiting, err := requirementAwaitsHumanInput(context.Background(), nil, domain.Increment{})
	if err != nil {
		t.Fatalf("requirementAwaitsHumanInput errored on an empty link: %v", err)
	}
	if waiting {
		t.Fatal("requirementAwaitsHumanInput reported an empty parent link as waiting for human input; V2-089 must not have changed it")
	}
	t.Log("A9 first half: for an empty parent link requirementAdmitsClaim answers FALSE (refuse) and requirementAwaitsHumanInput answers FALSE (not waiting). Opposite polarity on the same case, both correct, which is why they are two helpers.")
}

// ===========================================================================
// A20: exactly one issuer and exactly one definition, asserted by source scan
// ===========================================================================

// claimableScanPackage parses every non-test *.go file in this package. It fails
// outright on a zero-file scan.
func claimableScanPackage(t *testing.T) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("scanned zero files; the glob is broken")
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		out[filepath.Base(path)] = file
	}
	if len(out) == 0 {
		t.Fatal("scanned zero non-test files; the scan is broken")
	}
	return out
}

// funcsReturning returns the names of the top-level functions and methods whose
// body RETURNS an expression naming ident, per file.
func funcsReturning(files map[string]*ast.File, ident string) map[string][]string {
	out := map[string][]string{}
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			hit := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ret, isRet := n.(*ast.ReturnStmt)
				if !isRet {
					return true
				}
				ast.Inspect(ret, func(x ast.Node) bool {
					if id, isID := x.(*ast.Ident); isID && id.Name == ident {
						hit = true
					}
					return true
				})
				return true
			})
			if hit {
				out[name] = append(out[name], fn.Name.Name)
			}
		}
	}
	return out
}

// TestTheNotClaimableRefusalHasExactlyOneIssuerAndOneDefinition is A20. A second
// list of the four statuses anywhere in non-test source, a second function
// returning the sentinel, or a second declaration of the predicate each fails
// this scan, which is what makes "declared once, read twice" a build failure
// rather than a claim.
func TestTheNotClaimableRefusalHasExactlyOneIssuerAndOneDefinition(t *testing.T) {
	// Positive control on the return matcher, before it is trusted.
	positive := "package application\n\nfunc issue() error { return fmt.Errorf(\"%w\", ErrRequirementNotClaimable) }\nfunc other() error { return nil }\n"
	synthetic, err := parser.ParseFile(token.NewFileSet(), "positive.go", positive, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	control := funcsReturning(map[string]*ast.File{"positive.go": synthetic}, "ErrRequirementNotClaimable")
	if len(control["positive.go"]) != 1 || control["positive.go"][0] != "issue" {
		t.Fatalf("positive control: the synthetic issuer was not matched: %v", control)
	}

	files := claimableScanPackage(t)

	// (1) The sentinel is RETURNED from exactly one function in this package.
	issuers := funcsReturning(files, "ErrRequirementNotClaimable")
	total := 0
	for _, names := range issuers {
		total += len(names)
	}
	if total != 1 {
		t.Fatalf("application.ErrRequirementNotClaimable is returned from %d functions in non-test source, want exactly 1: %v", total, issuers)
	}
	if len(issuers["service.go"]) != 1 || issuers["service.go"][0] != "Claim" {
		t.Fatalf("the single issuer is not service.go's Claim: %v", issuers)
	}

	// (2) The predicate is DECLARED exactly once, and called from exactly one
	// non-test site: the reader in the same file.
	declarations, calls := 0, map[string][]string{}
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == "requirementStatusAdmitsClaim" {
				declarations++
			}
			if fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if id, isID := call.Fun.(*ast.Ident); isID && id.Name == "requirementStatusAdmitsClaim" {
					calls[name] = append(calls[name], fn.Name.Name)
				}
				return true
			})
		}
	}
	if declarations != 1 {
		t.Fatalf("requirementStatusAdmitsClaim is declared %d times in non-test source, want exactly 1", declarations)
	}
	callTotal := 0
	for _, names := range calls {
		callTotal += len(names)
	}
	// V2-091 widens this closed set from ONE call site to TWO, under
	// v2-task-dag.md 12.12, and the widening is the MEASUREMENT rather than a
	// weakening. One entry is added, with its reason:
	//   - loop.go's loopObserve reads this predicate to filter the runner-role
	//     offer behind GET /v1/runner/work down to Increments whose PARENT
	//     admits work. Offering an Increment whose parent refuses a claim would
	//     offer a Runner work that the very next call refuses, and re-expressing
	//     the four statuses in loop.go is exactly the drift assertion (3) below
	//     exists to catch -- so READING this predicate is the only way to build
	//     the offer without a second list.
	// Nothing else in this guard changes: the predicate is still DECLARED exactly
	// once, the sentinel is still returned from exactly one function, the four
	// statuses still appear together in exactly one switch, and the set stays
	// CLOSED -- a THIRD call site still fails here.
	wantCalls := map[string][]string{
		"claimable.go": {"requirementAdmitsClaim"},
		"loop.go":      {"loopObserve"},
	}
	wantCallTotal := 0
	for file, names := range wantCalls {
		wantCallTotal += len(names)
		if len(calls[file]) != len(names) {
			t.Fatalf("requirementStatusAdmitsClaim call sites in %s = %v, want exactly %v (all sites: %v)", file, calls[file], names, calls)
		}
		for i, name := range names {
			if calls[file][i] != name {
				t.Fatalf("requirementStatusAdmitsClaim call site %d in %s = %q, want %q (all sites: %v)", i, file, calls[file][i], name, calls)
			}
		}
	}
	if callTotal != wantCallTotal || len(calls) != len(wantCalls) {
		t.Fatalf("requirementStatusAdmitsClaim is called from %d non-test sites in %d files, want exactly %d in %d: %v", callTotal, len(calls), wantCallTotal, len(wantCalls), calls)
	}

	// (3) The four status constants appear TOGETHER in exactly one switch in
	// non-test source. This is the assertion that catches a second list.
	wanted := map[string]bool{"RequirementReady": true, "RequirementActive": true, "RequirementWaiting": true, "RequirementRecovering": true}
	switchMatcher := func(file *ast.File) int {
		n := 0
		ast.Inspect(file, func(node ast.Node) bool {
			sw, isSwitch := node.(*ast.SwitchStmt)
			if !isSwitch || sw.Body == nil {
				return true
			}
			seen := map[string]bool{}
			ast.Inspect(sw.Body, func(x ast.Node) bool {
				sel, isSel := x.(*ast.SelectorExpr)
				if !isSel {
					return true
				}
				if pkg, isID := sel.X.(*ast.Ident); isID && pkg.Name == "domain" && wanted[sel.Sel.Name] {
					seen[sel.Sel.Name] = true
				}
				return true
			})
			if len(seen) == len(wanted) {
				n++
			}
			return true
		})
		return n
	}
	positiveSwitch := "package application\n\nfunc p(s domain.RequirementStatus) bool {\n\tswitch s {\n\tcase domain.RequirementReady, domain.RequirementActive, domain.RequirementWaiting, domain.RequirementRecovering:\n\t\treturn true\n\t}\n\treturn false\n}\n"
	syntheticSwitch, err := parser.ParseFile(token.NewFileSet(), "positive-switch.go", positiveSwitch, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	if switchMatcher(syntheticSwitch) != 1 {
		t.Fatal("positive control: the synthetic four-status switch was not counted")
	}
	perFile := map[string]int{}
	switches := 0
	for name, file := range files {
		if n := switchMatcher(file); n != 0 {
			perFile[name] = n
			switches += n
		}
	}
	if switches != 1 {
		t.Fatalf("the four admitting statuses appear together in %d switches in non-test source, want exactly 1: %v. A second list is a second definition.", switches, perFile)
	}
	if perFile["claimable.go"] != 1 {
		t.Fatalf("the single four-status switch is not in claimable.go: %v", perFile)
	}
	t.Logf("A20: scanned %d non-test files in this package. ErrRequirementNotClaimable is returned from exactly one function (service.go Claim); requirementStatusAdmitsClaim is declared once and called from exactly one non-test site (claimable.go requirementAdmitsClaim); the four admitting statuses appear together in exactly one switch (claimable.go).", len(files))
}
