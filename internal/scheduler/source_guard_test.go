package scheduler

// V2-030 / A9: proves internal/scheduler's purity mechanically, via a
// go/parser AST scan, rather than by review. docs/operations/scheduler-local.md
// asserts the package performs no Firestore, network, process, or provider
// operation, and validation.md section 5's growth-rate items depend on the
// decision cost being a function of the bounded Snapshot alone; a prose
// claim cannot enforce either one, but an import scan can. This is also what
// keeps the new internal/domain import (A3) from silently becoming a door to
// a store or a real clock: any further import added to this package is a
// test failure until this file's allowlists are deliberately widened.
//
// Modelled on internal/domain/source_guard_test.go's parse-and-scan
// structure (parse every *.go file with go/parser, fail outright on an empty
// file set so a broken glob cannot pass vacuously), but scoped to this
// package's narrower question: which packages may internal/scheduler import
// at all.

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type parsedSchedulerFile struct {
	path    string
	isTest  bool
	imports []string
}

// parseSchedulerPackage parses every *.go file in the current directory (the
// internal/scheduler package source directory: go test always runs with the
// package directory as its working directory). It fails the test outright on
// an empty file set, so a mis-written glob cannot make the scan below pass
// vacuously.
func parseSchedulerPackage(t *testing.T) []parsedSchedulerFile {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("source guard scanned zero files; the working directory is not internal/scheduler or the glob is broken")
	}
	sort.Strings(matches)
	fset := token.NewFileSet()
	parsed := make([]parsedSchedulerFile, 0, len(matches))
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var imports []string
		for _, imp := range file.Imports {
			value, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import literal %s: %v", path, imp.Path.Value, err)
			}
			imports = append(imports, value)
		}
		parsed = append(parsed, parsedSchedulerFile{
			path:    path,
			isTest:  strings.HasSuffix(path, "_test.go"),
			imports: imports,
		})
	}
	return parsed
}

// schedulerDomainImport is the one and only non-standard-library import any
// file in this package may use, per A3/A9: internal/scheduler may read
// internal/domain, and nothing else.
const schedulerDomainImport = "github.com/takushi/agentic-loop-foundation/v2/internal/domain"

// nonTestImportAllowlist is the complete set of import paths a non-test file
// in internal/scheduler may use. Adding any new import to a production file
// (including a standard-library package) must be a conscious, deliberate
// widening of this constant.
var nonTestImportAllowlist = map[string]bool{
	"errors":              true,
	"fmt":                 true,
	"sort":                true,
	"time":                true,
	schedulerDomainImport: true,
}

// testImportAllowlist is the complete set of import paths any _test.go file
// in internal/scheduler may use, across every test file in the package
// (existing and new). It is wider than the production allowlist because
// this file must read its own package's source with go/parser, and
// budget_test.go measures a real wall-clock duration against a
// context.WithTimeout deadline (A8).
var testImportAllowlist = map[string]bool{
	"context":             true,
	"errors":              true,
	"go/parser":           true,
	"go/token":            true,
	"path/filepath":       true,
	"reflect":             true,
	"sort":                true,
	"strconv":             true,
	"strings":             true,
	"testing":             true,
	"time":                true,
	schedulerDomainImport: true,
}

func isAllowedImport(imp string, allowlist map[string]bool) bool {
	return allowlist[imp]
}

// TestImportAllowlistMatcherRejectsKnownBadImportsAndAcceptsDomain is the
// positive/negative control for the matcher the main scan below relies on:
// it proves isAllowedImport actually discriminates, using imports that are
// not otherwise exercised by this package (so the main scan passing would
// not, by itself, prove the matcher can ever return false).
func TestImportAllowlistMatcherRejectsKnownBadImportsAndAcceptsDomain(t *testing.T) {
	forbidden := []string{
		"net/http",
		"os/exec",
		"database/sql",
		"cloud.google.com/go/firestore",
		"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory",
		"github.com/takushi/agentic-loop-foundation/v2/internal/reconciler",
	}
	for _, imp := range forbidden {
		if isAllowedImport(imp, nonTestImportAllowlist) {
			t.Fatalf("positive control failed: %q was accepted by the non-test import allowlist", imp)
		}
		if isAllowedImport(imp, testImportAllowlist) {
			t.Fatalf("positive control failed: %q was accepted by the test import allowlist", imp)
		}
	}
	if !isAllowedImport(schedulerDomainImport, nonTestImportAllowlist) {
		t.Fatalf("negative control failed: %q was rejected by the non-test import allowlist", schedulerDomainImport)
	}
	if !isAllowedImport("errors", nonTestImportAllowlist) {
		t.Fatal("negative control failed: stdlib \"errors\" was rejected by the non-test import allowlist")
	}
}

func TestSchedulerImportsAreStdlibOrDomainOnly(t *testing.T) {
	files := parseSchedulerPackage(t)
	const wantMinScannedFiles = 6
	if len(files) < wantMinScannedFiles {
		t.Fatalf("scanned %d files, want at least %d", len(files), wantMinScannedFiles)
	}

	nonTestFiles, testFiles := 0, 0
	for _, pf := range files {
		if pf.isTest {
			testFiles++
		} else {
			nonTestFiles++
		}
		allowlist := nonTestImportAllowlist
		if pf.isTest {
			allowlist = testImportAllowlist
		}
		for _, imp := range pf.imports {
			if !isAllowedImport(imp, allowlist) {
				t.Fatalf("%s: import %q is not in the allowed set (standard library plus %s only); widen the allowlist in this file deliberately if this is intended", pf.path, imp, schedulerDomainImport)
			}
		}
	}
	if nonTestFiles == 0 {
		t.Fatal("scanned zero non-test files")
	}
	if testFiles == 0 {
		t.Fatal("scanned zero test files")
	}
	t.Logf("scanned files=%d non-test=%d test=%d", len(files), nonTestFiles, testFiles)
}

// TestImportAllowlistsAreExercised is a narrow sanity check that neither
// allowlist above is vacuously large: every entry is actually used by at
// least one file in the package as currently written. It does not replace
// go list -deps ./internal/scheduler (recorded as a separate evidence
// check), which additionally proves the transitive closure is stdlib+domain
// only; this test only proves the direct-import allowlists declared here are
// not overly permissive relative to real usage.
func TestImportAllowlistsAreExercised(t *testing.T) {
	files := parseSchedulerPackage(t)
	used := map[string]bool{}
	for _, pf := range files {
		for _, imp := range pf.imports {
			used[imp] = true
		}
	}
	for imp := range nonTestImportAllowlist {
		if !used[imp] {
			t.Errorf("nonTestImportAllowlist entry %q is never imported by any file in the package", imp)
		}
	}
	for imp := range testImportAllowlist {
		if !used[imp] {
			t.Errorf("testImportAllowlist entry %q is never imported by any file in the package", imp)
		}
	}
}
