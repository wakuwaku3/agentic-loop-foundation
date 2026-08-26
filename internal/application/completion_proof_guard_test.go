package application_test

// V2-091 A11: the structural guard on the completion proof.
//
// WHAT IT PROTECTS, and why a convention could not. domain.StableReleaseProof
// (internal/domain/release.go:117) is a struct whose only field is an
// UNEXPORTED POINTER to an UNEXPORTED type, and whose validity predicate
// valid() (:119-121) is unexported too, so no code outside internal/domain can
// build a valid one by any means other than calling a function that returns
// one. The sole such function is domain.CompletePromotionWithProof (:150). Its
// only non-test caller is release.PromotionGate (internal/release/release.go
// :261), whose only non-test caller is release.Pipeline.Promote
// (internal/release/pipeline.go:38) -- which, measured at 848d899, had ZERO
// non-test callers, so a running process could not obtain a proof at all.
//
// This task gives Pipeline.Promote its first non-test caller, and this file is
// what keeps that "first" from becoming "one of several". The design is
// three-layered and only the third layer lives here:
//
//	(1) internal/domain/** is PROHIBITED to this task, so the shortest wrong
//	    answer -- export a proof constructor, or make stableReleaseProofData
//	    settable -- requires opening a file the task may not touch.
//	(2) internal/release/release.go and internal/release/pipeline.go are
//	    PROHIBITED, so no second gate can be introduced beside the one that
//	    earns the proof.
//	(3) THIS SCAN: over every non-test .go file in the repository, walked from
//	    the module root, the number of call sites of each link in the chain is
//	    asserted exactly, and the file each one lives in is asserted by name.
//
// Modelled on internal/scheduler/source_guard_test.go's parse-and-scan
// structure: parse with go/parser, and FAIL OUTRIGHT on an empty file set so a
// broken glob or a wrong working directory cannot make the scan pass vacuously.
// The number of files parsed is reported in every failure message, so a
// SHRUNKEN scan is visible rather than silently green.
//
// NON-VACUITY IS PROVEN BY EXECUTION, not asserted. The implementer deleted the
// real call site, ran this guard, recorded the RED verdict line and the exact
// failure message, restored the call site and recorded the GREEN verdict line.
// A guard nobody has seen fail is not evidence.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// completionProofChainFile is one parsed non-test file, keyed by its
// repository-relative slash path.
type completionProofChainFile struct {
	path string
	file *ast.File
}

// moduleRootForProofGuard walks up from the package directory to the directory
// holding go.mod. It never hardcodes a depth, so the scan cannot silently
// become a scan of the wrong subtree.
func moduleRootForProofGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the module root (the directory holding go.mod) above internal/application")
		}
		dir = parent
	}
}

// parseRepositoryNonTestFiles parses every non-test .go file in the repository.
func parseRepositoryNonTestFiles(t *testing.T) (string, []completionProofChainFile) {
	t.Helper()
	root := moduleRootForProofGuard(t)
	fset := token.NewFileSet()
	var out []completionProofChainFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// build/** is gitignored generated output and .git/** is not source.
			switch d.Name() {
			case ".git", "build", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, completionProofChainFile{path: filepath.ToSlash(rel), file: parsed})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module root: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the completion-proof guard parsed ZERO non-test .go files; the module root walk is broken and every assertion below would pass vacuously")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return root, out
}

// callSitesOf returns, per file, the names of the functions that CALL a
// function or method whose final name segment is name. It matches both the
// bare identifier form (a call inside the declaring package) and the selector
// form (a qualified call, or a method call on a receiver).
func callSitesOf(files []completionProofChainFile, name string) map[string][]string {
	out := map[string][]string{}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == name {
						out[pf.path] = append(out[pf.path], fn.Name.Name)
					}
				case *ast.SelectorExpr:
					if fun.Sel != nil && fun.Sel.Name == name {
						out[pf.path] = append(out[pf.path], fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	return out
}

func totalCallSites(sites map[string][]string) int {
	n := 0
	for _, names := range sites {
		n += len(names)
	}
	return n
}

// TestTheCompletionProofHasExactlyOneCallSitePerLinkInItsChain is A11.
func TestTheCompletionProofHasExactlyOneCallSitePerLinkInItsChain(t *testing.T) {
	_, files := parseRepositoryNonTestFiles(t)
	parsed := len(files)
	const loopFile = "internal/application/loop.go"

	// (1) domain.CompleteRequirementFromRelease -- the transition that turns a
	// proof into a completed Requirement. Exactly ONE call site, and it is the
	// new pass. A second call site anywhere would be a second place a
	// Requirement can be declared solved.
	completions := callSitesOf(files, "CompleteRequirementFromRelease")
	if totalCallSites(completions) != 1 {
		t.Fatalf("domain.CompleteRequirementFromRelease is called from %d non-test sites across %d parsed files, want exactly 1: %v", totalCallSites(completions), parsed, completions)
	}
	if len(completions[loopFile]) != 1 {
		t.Fatalf("the single call site of domain.CompleteRequirementFromRelease is not in %s (parsed %d files): %v", loopFile, parsed, completions)
	}

	// (2) The gate and the pipeline, OUTSIDE internal/release. Combined, exactly
	// ONE call site, and it is the new pass. Pipeline.Promote is the sanctioned
	// entry point because it re-verifies the bundle against its source tree
	// before the gate runs; calling PromotionGate directly would skip that
	// re-verification, so a call to it from outside the package is the shape
	// this assertion exists to catch.
	promotes := callSitesOf(files, "Promote")
	gates := callSitesOf(files, "PromotionGate")
	outside := map[string][]string{}
	for path, names := range promotes {
		if strings.HasPrefix(path, "internal/release/") {
			continue
		}
		outside[path] = append(outside[path], names...)
	}
	for path, names := range gates {
		if strings.HasPrefix(path, "internal/release/") {
			continue
		}
		outside[path] = append(outside[path], names...)
	}
	if totalCallSites(outside) != 1 {
		t.Fatalf("release.Pipeline.Promote and release.PromotionGate are called from %d non-test sites outside internal/release across %d parsed files, want exactly 1: %v", totalCallSites(outside), parsed, outside)
	}
	if len(outside[loopFile]) != 1 {
		t.Fatalf("the single non-test call site of the promotion path outside internal/release is not in %s (parsed %d files): %v", loopFile, parsed, outside)
	}

	// (3) domain.CompletePromotionWithProof -- the SOLE constructor of a valid
	// proof. Exactly ONE call site outside internal/domain, and it is
	// release.PromotionGate. This is the link this task does not touch, and
	// asserting it here is what makes the chain a chain rather than two
	// independent facts.
	constructors := callSitesOf(files, "CompletePromotionWithProof")
	outsideDomain := map[string][]string{}
	for path, names := range constructors {
		if strings.HasPrefix(path, "internal/domain/") {
			continue
		}
		outsideDomain[path] = append(outsideDomain[path], names...)
	}
	if totalCallSites(outsideDomain) != 1 {
		t.Fatalf("domain.CompletePromotionWithProof is called from %d non-test sites outside internal/domain across %d parsed files, want exactly 1: %v", totalCallSites(outsideDomain), parsed, outsideDomain)
	}
	if len(outsideDomain["internal/release/release.go"]) != 1 || outsideDomain["internal/release/release.go"][0] != "PromotionGate" {
		t.Fatalf("the single call site of domain.CompletePromotionWithProof outside internal/domain is not release.PromotionGate (parsed %d files): %v", parsed, outsideDomain)
	}

	// (4) The new pass must NOT reach past Pipeline.Promote. It may not call the
	// gate directly and it may not call the proof constructor directly.
	if len(gates[loopFile]) != 0 {
		t.Fatalf("%s calls release.PromotionGate directly, skipping Pipeline.Promote's source re-verification: %v", loopFile, gates)
	}
	if len(constructors[loopFile]) != 0 {
		t.Fatalf("%s calls domain.CompletePromotionWithProof directly, bypassing the gate that earns the proof: %v", loopFile, constructors)
	}

	t.Logf("completion-proof chain guard: parsed %d non-test .go files; CompleteRequirementFromRelease call sites=%v; promotion-path call sites outside internal/release=%v; CompletePromotionWithProof call sites outside internal/domain=%v", parsed, completions, outside, outsideDomain)
}

// TestTheProofIsNeverStoredLoggedOrReported asserts, over the AST of the one new
// non-test file, that the GateResult's Proof field is read EXACTLY ONCE and that
// the single read is the argument of the completion transition. A proof placed
// in a report field, a log line, a store call or a returned value would show up
// here as a second read.
func TestTheProofIsNeverStoredLoggedOrReported(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "loop.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse loop.go: %v", err)
	}
	reads := 0
	insideCompletion := 0
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == "Proof" {
			reads++
		}
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		fun, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || fun.Sel == nil || fun.Sel.Name != "CompleteRequirementFromRelease" {
			return true
		}
		for _, arg := range call.Args {
			if argSel, isArgSel := arg.(*ast.SelectorExpr); isArgSel && argSel.Sel != nil && argSel.Sel.Name == "Proof" {
				insideCompletion++
			}
		}
		return true
	})
	if reads != 1 {
		t.Fatalf("loop.go reads GateResult.Proof %d times, want exactly 1: the proof must not be stored, logged, reported or returned", reads)
	}
	if insideCompletion != 1 {
		t.Fatalf("loop.go passes GateResult.Proof to domain.CompleteRequirementFromRelease %d times, want exactly 1", insideCompletion)
	}
}
