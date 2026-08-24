package release

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const domainImportPath = "github.com/takushi/agentic-loop-foundation/v2/internal/domain"

// isStdlibImport classifies an import path as standard library using the Go
// convention that a standard library import's first path segment contains
// no dot (third-party and internal module paths do, e.g. "github.com/...").
func isStdlibImport(path string) bool {
	if path == "" {
		return false
	}
	first := strings.SplitN(path, "/", 2)[0]
	return !strings.Contains(first, ".")
}

func isAllowedNonTestImport(path string) bool {
	return isStdlibImport(path) || path == domainImportPath
}

// isForbiddenReleaseImport reports whether path names internal/ci or
// internal/update, which dp-v2-021 d12 forbids internal/release from
// importing (in test files or otherwise): such an import would create a
// component edge ci/components.json does not record.
func isForbiddenReleaseImport(path string) bool {
	return path == "github.com/takushi/agentic-loop-foundation/v2/internal/ci" ||
		strings.HasPrefix(path, "github.com/takushi/agentic-loop-foundation/v2/internal/ci/") ||
		path == "github.com/takushi/agentic-loop-foundation/v2/internal/update" ||
		strings.HasPrefix(path, "github.com/takushi/agentic-loop-foundation/v2/internal/update/")
}

// TestSourceGuardMatcherIsVerified checks the classifier itself against a
// known-positive and a known-negative import path, per A16, before trusting
// it to scan the package.
func TestSourceGuardMatcherIsVerified(t *testing.T) {
	if !isStdlibImport("fmt") {
		t.Fatal("known-negative (stdlib) fmt must classify as standard library")
	}
	if !isStdlibImport("path/filepath") {
		t.Fatal("known-negative (stdlib) path/filepath must classify as standard library")
	}
	if isStdlibImport(domainImportPath) {
		t.Fatal("internal/domain must not classify as standard library")
	}
	if !isAllowedNonTestImport(domainImportPath) {
		t.Fatal("internal/domain must be an allowed non-test import")
	}
	if isAllowedNonTestImport("github.com/takushi/agentic-loop-foundation/v2/internal/ci") {
		t.Fatal("internal/ci must not be an allowed non-test import")
	}

	// known-positive: an actual forbidden import path must be detected.
	if !isForbiddenReleaseImport("github.com/takushi/agentic-loop-foundation/v2/internal/ci") {
		t.Fatal("known-positive forbidden import (internal/ci) was not detected")
	}
	if !isForbiddenReleaseImport("github.com/takushi/agentic-loop-foundation/v2/internal/update") {
		t.Fatal("known-positive forbidden import (internal/update) was not detected")
	}
	// known-negative: an allowed import path must not be flagged.
	if isForbiddenReleaseImport(domainImportPath) {
		t.Fatal("known-negative import (internal/domain) was incorrectly flagged as forbidden")
	}
	if isForbiddenReleaseImport("fmt") {
		t.Fatal("known-negative import (fmt) was incorrectly flagged as forbidden")
	}
}

// packageGoFiles lists the .go files directly in this package's directory
// (no subpackages), parsed with go/ast rather than matched as text.
func packageGoFiles(t *testing.T, includeTests bool) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if !includeTests && strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, file)
	}
	return files
}

func importPaths(file *ast.File) []string {
	out := make([]string, 0, len(file.Imports))
	for _, imp := range file.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	return out
}

// TestSourceGuardNonTestFilesImportOnlyStdlibAndDomain is A16's primary
// boundary assertion: every non-test file in this package imports only the
// standard library plus internal/domain. It fails outright on a zero-file
// scan so it cannot pass vacuously.
func TestSourceGuardNonTestFilesImportOnlyStdlibAndDomain(t *testing.T) {
	files := packageGoFiles(t, false)
	if len(files) == 0 {
		t.Fatal("zero non-test .go files scanned; this assertion must not pass vacuously")
	}
	for i, file := range files {
		for _, path := range importPaths(file) {
			if !isAllowedNonTestImport(path) {
				t.Errorf("file %d: import %q is neither standard library nor internal/domain", i, path)
			}
			if isForbiddenReleaseImport(path) {
				t.Errorf("file %d: forbidden import %q (internal/ci or internal/update)", i, path)
			}
		}
	}
	t.Logf("source guard scanned %d non-test .go files", len(files))
}

// TestSourceGuardNoFileImportsCIOrUpdate is A16's second half: no file in
// the package, test file or not, imports internal/ci or internal/update.
func TestSourceGuardNoFileImportsCIOrUpdate(t *testing.T) {
	files := packageGoFiles(t, true)
	if len(files) == 0 {
		t.Fatal("zero .go files scanned (test and non-test); this assertion must not pass vacuously")
	}
	for i, file := range files {
		for _, path := range importPaths(file) {
			if isForbiddenReleaseImport(path) {
				t.Errorf("file %d imports forbidden package %q", i, path)
			}
		}
	}
	t.Logf("source guard scanned %d total .go files (including tests)", len(files))
}
