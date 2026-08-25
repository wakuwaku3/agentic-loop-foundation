package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateImportPath is the package the Runner must not link. Naming it here is
// safe as an edge: cmd/bootstrap is a runner component root and the runner
// component already declares update as a dependency, which is exactly the
// asymmetry this guard enforces -- the Bootstrapper links internal/update,
// the Runner does not.
const updateImportPath = "github.com/takushi/agentic-loop-foundation/v2/internal/update"

// runnerRoots are the directories docs/operations/self-update.md section 4.1
// forbids from importing internal/update, relative to this package.
var runnerRoots = []string{
	filepath.Join("..", "runner"),
	filepath.Join("..", "..", "internal", "runner"),
}

// importsUpdate reports whether a parsed file imports internal/update or a
// package under it.
func importsUpdate(file *ast.File) bool {
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if path == updateImportPath || strings.HasPrefix(path, updateImportPath+"/") {
			return true
		}
	}
	return false
}

func parseDir(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out[path] = parsed
	}
	return out
}

// TestBootstrapperIndependenceGuardIsVerified is the positive control: the
// matcher is shown to detect a file that really imports internal/update and
// to leave a file that does not alone, before it is trusted to scan the
// Runner's directories.
func TestBootstrapperIndependenceGuardIsVerified(t *testing.T) {
	fset := token.NewFileSet()
	offending, err := parser.ParseFile(fset, "synthetic.go", "package runner\n\nimport (\n\t\"context\"\n\t_ \""+updateImportPath+"\"\n)\n", parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !importsUpdate(offending) {
		t.Fatal("known-positive: a file importing internal/update was not detected")
	}
	nested, err := parser.ParseFile(fset, "nested.go", "package runner\n\nimport _ \""+updateImportPath+"/sub\"\n", parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !importsUpdate(nested) {
		t.Fatal("known-positive: a file importing a package under internal/update was not detected")
	}
	clean, err := parser.ParseFile(fset, "clean.go", "package runner\n\nimport \"context\"\n", parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if importsUpdate(clean) {
		t.Fatal("known-negative: a file importing only the standard library was flagged")
	}
	// This package itself is the known-positive in the tree: the
	// Bootstrapper does link internal/update, which is what makes the
	// asymmetry an asymmetry rather than a blanket ban.
	self := parseDir(t, ".")
	found := false
	for _, file := range self {
		if importsUpdate(file) {
			found = true
		}
	}
	if !found {
		t.Fatal("cmd/bootstrap does not link internal/update; the guard below would then be vacuous")
	}
	t.Log("verdict: matcher detected both forbidden shapes, ignored the clean file, and found the Bootstrapper's own legitimate edge")
}

// TestRunnerDoesNotLinkTheUpdatePackage is section 4.1's structural claim:
// "the process that updates X must not be X" holds because the symbols are
// not in the Runner's binary, not because of a convention. An update is the
// parent installing side by side, flipping the symlink and starting a new
// child; the process being replaced never performs the install.
func TestRunnerDoesNotLinkTheUpdatePackage(t *testing.T) {
	scanned := 0
	for _, dir := range runnerRoots {
		files := parseDir(t, dir)
		if len(files) == 0 {
			t.Fatalf("zero .go files scanned under %s; this assertion must not pass vacuously", dir)
		}
		for path, file := range files {
			scanned++
			if importsUpdate(file) {
				t.Errorf("%s imports internal/update; the Runner must not link the package that updates it", path)
			}
		}
	}
	t.Logf("verdict: %d .go files under %v scanned, none links internal/update", scanned, runnerRoots)
}
