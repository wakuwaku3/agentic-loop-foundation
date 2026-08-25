package update

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleInternalPrefix is this repository's module path plus "/internal/".
// The forbidden package names are appended to it at run time rather than
// written as whole module-path literals, so this guard's own assertion data
// does not read as a dependency edge of the update component -- the same
// trap internal/ci's LiteralFalsePositives table exists to paper over for
// the guard pointing the other way.
const moduleInternalPrefix = "github.com/takushi/agentic-loop-foundation/v2/internal/"

// isStdlib classifies an import path as standard library using the Go
// convention that a standard library import's first path segment contains no
// dot (third-party and module-internal paths do).
func isStdlib(path string) bool {
	if path == "" {
		return false
	}
	first := strings.SplitN(path, "/", 2)[0]
	return !strings.Contains(first, ".")
}

// TestUpdateSourceGuardMatcherIsVerified is the positive control (A16): the
// classifier is checked against known-positive and known-negative paths, and
// the scanner is checked against a synthetic file that actually carries a
// forbidden import, before either is trusted to scan the package.
func TestUpdateSourceGuardMatcherIsVerified(t *testing.T) {
	for _, path := range []string{"fmt", "crypto/ed25519", "path/filepath", "syscall"} {
		if !isStdlib(path) {
			t.Fatalf("known-negative (stdlib) %q must classify as standard library", path)
		}
	}
	// known-positive: the two packages dp-v2-021 d12 forbids in the other
	// direction, plus this repository's own domain package.
	for _, name := range []string{"release", "ci", "domain", "runner", "store/firestore"} {
		if isStdlib(moduleInternalPrefix + name) {
			t.Fatalf("known-positive forbidden import %q classified as standard library", moduleInternalPrefix+name)
		}
	}

	// The scanner itself, against a file that really does import a
	// forbidden package. Without this the scan below could pass because it
	// never looks at imports at all.
	fset := token.NewFileSet()
	synthetic, err := parser.ParseFile(fset, "synthetic.go", "package update\n\nimport (\n\t\"fmt\"\n\t_ \""+moduleInternalPrefix+"release\"\n)\n", parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	offenders := nonStdlibImports(synthetic)
	if len(offenders) != 1 || offenders[0] != moduleInternalPrefix+"release" {
		t.Fatalf("scanner found %v in a file that imports a forbidden package", offenders)
	}
	clean, err := parser.ParseFile(fset, "clean.go", "package update\n\nimport \"encoding/json\"\n", parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if found := nonStdlibImports(clean); len(found) != 0 {
		t.Fatalf("scanner flagged %v in a standard-library-only file", found)
	}
	t.Log("verdict: positive control detected the forbidden import, negative control stayed clean")
}

func nonStdlibImports(file *ast.File) []string {
	var out []string
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if !isStdlib(path) {
			out = append(out, path)
		}
	}
	return out
}

func updatePackageFiles(t *testing.T, includeTests bool) []struct {
	name string
	file *ast.File
} {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []struct {
		name string
		file *ast.File
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if !includeTests && strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, struct {
			name string
			file *ast.File
		}{e.Name(), parsed})
	}
	return files
}

// TestUpdateImportsOnlyTheStandardLibrary is the structural half of section
// 4.1: internal/update links nothing but the standard library, so the
// Bootstrapper cannot acquire a dependency on the Runner, the release layer
// or the store, and dp-v2-021 d12's forbidden edge cannot be created from
// this side either. The existing guard lives in internal/release and would
// not catch an edge pointing this way; this is its mirror image.
func TestUpdateImportsOnlyTheStandardLibrary(t *testing.T) {
	nonTest := updatePackageFiles(t, false)
	if len(nonTest) == 0 {
		t.Fatal("zero non-test .go files scanned; this assertion must not pass vacuously")
	}
	for _, f := range nonTest {
		if offenders := nonStdlibImports(f.file); len(offenders) != 0 {
			t.Errorf("%s imports %v, which is not the standard library", f.name, offenders)
		}
	}
	all := updatePackageFiles(t, true)
	if len(all) <= len(nonTest) {
		t.Fatal("zero test .go files scanned; this assertion must not pass vacuously")
	}
	for _, f := range all {
		if offenders := nonStdlibImports(f.file); len(offenders) != 0 {
			t.Errorf("%s (test file) imports %v; even the tests of this package link only the standard library", f.name, offenders)
		}
	}
	t.Logf("verdict: %d non-test and %d total .go files scanned, all standard-library-only", len(nonTest), len(all))
}
