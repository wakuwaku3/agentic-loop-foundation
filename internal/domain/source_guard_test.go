package domain

// This file proves, mechanically from source rather than by review, two
// structural properties of internal/domain:
//
//   - Invariant 5 (docs/architecture/validation.md section 2): no credential
//     field exists in the domain schema.
//   - Zero database, GitHub, Provider, network and real-clock dependency.
//
// Both scans read the package's own *.go files with go/parser and walk the
// resulting AST; neither scans file text for words, because
// internal/domain/control.go legitimately declares
// PermitCredential PermitKind = "credential" as a permit-kind value, and a
// text grep for "credential" would fail on that correct code.

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

// parsedDomainFile is one *.go file in this package, parsed once and shared
// by both the credential-field scan and the dependency scan below.
type parsedDomainFile struct {
	path    string
	isTest  bool
	file    *ast.File
	imports []string // raw import paths, e.g. "go/ast"
}

// parseDomainPackage parses every *.go file in the current directory (the
// internal/domain package source directory: `go test` always runs with the
// package directory as its working directory). It fails the test outright on
// an empty file set, so a mis-written glob cannot make either scan below
// pass vacuously.
func parseDomainPackage(t *testing.T) []parsedDomainFile {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("source guard scanned zero files; the working directory is not internal/domain or the glob is broken")
	}
	sort.Strings(matches)
	fset := token.NewFileSet()
	parsed := make([]parsedDomainFile, 0, len(matches))
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
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
		parsed = append(parsed, parsedDomainFile{
			path:    path,
			isTest:  strings.HasSuffix(path, "_test.go"),
			file:    file,
			imports: imports,
		})
	}
	return parsed
}

// ===========================================================================
// Invariant 5: no credential field exists in the domain schema.
// ===========================================================================

// credentialDenyList is closed and normalized (lowercase, underscores
// removed) before matching. It deliberately excludes the bare word "token":
// FencingToken, PreviousFencingToken and ExpectedFencingToken are legitimate,
// non-secret domain concepts that appear in eight structs across the
// package, and a bare "token" entry would flag all of them. AccessToken and
// RefreshToken remain covered explicitly, because those two specific shapes
// do name credential material.
var credentialDenyList = []string{
	"password",
	"passwd",
	"credential",
	"credentials",
	"secret",
	"apikey",
	"privatekey",
	"accesstoken",
	"refreshtoken",
	"bearer",
	"sessioncookie",
	"authorization",
	"rawprompt",
	"rawproviderout",
}

func normalizeSchemaName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", ""))
}

// matchesCredentialDenyList reports whether name, once normalized, contains
// any deny-list entry as a substring. Substring containment (not equality)
// is required: ClientSecret normalizes to "clientsecret", which contains
// "secret" but does not equal it, and raw_provider_output normalizes to
// "rawprovideroutput", which contains the deliberately-stemmed
// "rawproviderout" entry but does not equal it either.
func matchesCredentialDenyList(name string) bool {
	normalized := normalizeSchemaName(name)
	for _, entry := range credentialDenyList {
		if strings.Contains(normalized, entry) {
			return true
		}
	}
	return false
}

func TestCredentialDenyListMatcherIsVerified(t *testing.T) {
	positive := []string{"Password", "APIKey", "ClientSecret", "RawPrompt", "raw_provider_output"}
	for _, name := range positive {
		if !matchesCredentialDenyList(name) {
			t.Fatalf("positive control %q did not match the credential deny list (normalized=%q)", name, normalizeSchemaName(name))
		}
	}
	negative := []string{"FencingToken", "PreviousFencingToken", "ExpectedFencingToken", "ContractDigest", "EvidenceDigest"}
	for _, name := range negative {
		if matchesCredentialDenyList(name) {
			t.Fatalf("negative control %q matched the credential deny list (normalized=%q)", name, normalizeSchemaName(name))
		}
	}
}

// jsonTagName extracts the field-name portion of a `json:"..."` struct tag,
// or "" if there is none, the tag is "-", or the field is unexported without
// a tag.
func jsonTagName(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	value, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	raw, ok := lookupStructTag(value, "json")
	if !ok || raw == "-" || raw == "" {
		return ""
	}
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	return raw
}

// lookupStructTag is a minimal, dependency-free reimplementation of
// reflect.StructTag.Lookup: it does not require importing "reflect" into
// this test file, keeping the test-file import allowlist smaller.
func lookupStructTag(tag, key string) (string, bool) {
	for tag != "" {
		tag = strings.TrimLeft(tag, " \t")
		if tag == "" {
			break
		}
		i := 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		quoted := tag[:i+1]
		tag = tag[i+1:]
		if key == name {
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

func TestNoCredentialFieldInDomainSchema(t *testing.T) {
	files := parseDomainPackage(t)
	const wantMinScannedFiles = 9
	if len(files) < wantMinScannedFiles {
		t.Fatalf("scanned %d files, want at least %d", len(files), wantMinScannedFiles)
	}

	structCount := 0
	fieldCount := 0
	tagCount := 0
	for _, pf := range files {
		ast.Inspect(pf.file, func(n ast.Node) bool {
			structType, ok := n.(*ast.StructType)
			if !ok || structType.Fields == nil {
				return true
			}
			structCount++
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					fieldCount++
					if matchesCredentialDenyList(name.Name) {
						t.Fatalf("%s: struct field %q matches the credential deny list", pf.path, name.Name)
					}
				}
				if tagName := jsonTagName(field.Tag); tagName != "" {
					tagCount++
					if matchesCredentialDenyList(tagName) {
						t.Fatalf("%s: json tag %q matches the credential deny list", pf.path, tagName)
					}
				}
			}
			return true
		})
	}
	if structCount == 0 {
		t.Fatal("scanned zero struct types; the AST walk is not finding declarations")
	}
	if fieldCount == 0 {
		t.Fatal("scanned zero struct fields; the AST walk is not finding field names")
	}
	t.Logf("scanned files=%d structs=%d fields=%d json-tags=%d", len(files), structCount, fieldCount, tagCount)
}

// ===========================================================================
// Zero database, GitHub, Provider, network and real-clock dependency.
// ===========================================================================

// nonTestImportAllowlist is the complete set of import paths a non-test file
// in internal/domain may use. It is deliberately small and deliberately
// resistant to a casual widening: adding any new import to a production file
// (including a standard-library package such as "slices" or "maps") must be
// a conscious edit to this constant, not a silent side effect.
var nonTestImportAllowlist = map[string]bool{
	"errors":  true,
	"fmt":     true,
	"sort":    true,
	"strings": true,
	"time":    true,
}

// testImportAllowlist is the complete set of import paths any _test.go file
// in internal/domain may use, across every test file in the package
// (existing and new). It is wider than the production allowlist because
// source_guard_test.go must read its own package's source with go/parser.
var testImportAllowlist = map[string]bool{
	"errors":        true,
	"sort":          true,
	"strconv":       true,
	"strings":       true,
	"testing":       true,
	"time":          true,
	"go/ast":        true,
	"go/parser":     true,
	"go/token":      true,
	"path/filepath": true,
}

// forbiddenExactImports must never appear in any file in the package,
// production or test.
var forbiddenExactImports = map[string]bool{
	"net":          true,
	"net/http":     true,
	"net/url":      true,
	"database/sql": true,
	"os/exec":      true,
	"math/rand":    true,
	"crypto/rand":  true,
}

// forbiddenImportSubstrings catches cloud-provider and GitHub client paths
// by substring, since those live at varying sub-paths.
var forbiddenImportSubstrings = []string{
	"cloud.google.com",
	"google.golang.org",
	"github.com/google",
}

func TestNoDatabaseNetworkProviderOrRealClockDependency(t *testing.T) {
	files := parseDomainPackage(t)
	const wantMinScannedFiles = 9
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

		for _, imp := range pf.imports {
			if forbiddenExactImports[imp] {
				t.Fatalf("%s: imports forbidden package %q", pf.path, imp)
			}
			for _, substr := range forbiddenImportSubstrings {
				if strings.Contains(imp, substr) {
					t.Fatalf("%s: imports forbidden path %q (matches %q)", pf.path, imp, substr)
				}
			}
			if pf.isTest {
				if !testImportAllowlist[imp] {
					t.Fatalf("%s: import %q is not in the test-file import allowlist; widen testImportAllowlist deliberately if this is intended", pf.path, imp)
				}
			} else {
				if !nonTestImportAllowlist[imp] {
					t.Fatalf("%s: import %q is not in the non-test import allowlist {errors, fmt, sort, strings, time}", pf.path, imp)
				}
			}
		}

		if pf.isTest {
			continue
		}
		// No non-test file may contain a time.Now, time.Since or time.Until
		// selector expression. A stdlib-only dependency set does not by
		// itself exclude a real-clock read, since "time" is allowed for
		// legitimate reasons (durations, explicit timestamps).
		ast.Inspect(pf.file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "time" {
				return true
			}
			switch sel.Sel.Name {
			case "Now", "Since", "Until":
				t.Fatalf("%s: forbidden real-clock selector time.%s", pf.path, sel.Sel.Name)
			}
			return true
		})
	}

	if nonTestFiles == 0 {
		t.Fatal("scanned zero non-test files")
	}
	if testFiles == 0 {
		t.Fatal("scanned zero test files")
	}
	t.Logf("scanned files=%d non-test=%d test=%d", len(files), nonTestFiles, testFiles)
}

// TestDependencyAllowlistsAreExercised is a narrow sanity check that the two
// allowlists above are not vacuously large: every entry in each allowlist is
// actually used by at least one file in the package as currently written.
// This does not replace go list -deps ./internal/domain (recorded as a
// separate evidence check argv), which additionally proves the transitive
// closure is stdlib-only; this test only proves the direct-import allowlists
// declared here are not overly permissive relative to real usage.
func TestDependencyAllowlistsAreExercised(t *testing.T) {
	files := parseDomainPackage(t)
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
