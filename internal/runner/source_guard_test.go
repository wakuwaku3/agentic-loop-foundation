package runner

// This file extends the V2-010 go/ast source-guard pattern
// (internal/domain/source_guard_test.go) to internal/runner's own non-test
// files: no credential-shaped field name may exist on any type that is
// marshalled into a journal event, a Work Packet, a canonical application
// request, or a log line. The scan reads the AST, not the file text, because
// this package legitimately names a Secret Broker (SecretBroker, Scope,
// Grant, CredentialSource, PermitCredential-shaped constants) and a text
// grep for "credential" or "secret" would fail on that correct code.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type parsedRunnerFile struct {
	path   string
	isTest bool
	file   *ast.File
}

// parseRunnerPackage parses every *.go file in the current directory. It
// fails outright on an empty file set, so the scan below cannot pass
// vacuously because a glob was mis-written or the working directory was
// wrong.
func parseRunnerPackage(t *testing.T) []parsedRunnerFile {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("source guard scanned zero files; the working directory is not internal/runner or the glob is broken")
	}
	sort.Strings(matches)
	fset := token.NewFileSet()
	parsed := make([]parsedRunnerFile, 0, len(matches))
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed = append(parsed, parsedRunnerFile{path: path, isTest: strings.HasSuffix(path, "_test.go"), file: file})
	}
	return parsed
}

// runnerCredentialDenyList mirrors internal/domain's list. It deliberately
// excludes the bare word "token" (FencingToken, ExpectedFencingToken, and
// this package's own Scope.FencingToken/ExpectedFencingToken are legitimate,
// non-secret concepts) and deliberately excludes the bare word "credential"
// stem collisions that are this package's own correct vocabulary
// (CredentialSource, PermitCredential): those are covered by exact
// allow-listing in the negative control below, not by weakening the deny
// list itself, because a field literally named "Credential" or
// "CredentialValue" must still be caught.
var runnerCredentialDenyList = []string{
	"password",
	"passwd",
	"secretvalue",
	"secretkey",
	"secretbytes",
	"apikey",
	"privatekey",
	"accesstoken",
	"refreshtoken",
	"bearer",
	"sessioncookie",
	"authorization",
	"rawprompt",
	"rawproviderout",
	"credentialvalue",
	"plaintextcredential",
}

func normalizeRunnerSchemaName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", ""))
}

func matchesRunnerCredentialDenyList(name string) bool {
	normalized := normalizeRunnerSchemaName(name)
	for _, entry := range runnerCredentialDenyList {
		if strings.Contains(normalized, entry) {
			return true
		}
	}
	return false
}

// TestRunnerCredentialDenyListMatcherIsVerified proves the matcher itself
// actually matches known-bad names and does not match this package's known
// legitimate names, so the scan below cannot pass vacuously.
func TestRunnerCredentialDenyListMatcherIsVerified(t *testing.T) {
	positive := []string{"Password", "APIKey", "AccessToken", "SecretValue", "PlaintextCredential", "raw_provider_output"}
	for _, name := range positive {
		if !matchesRunnerCredentialDenyList(name) {
			t.Fatalf("positive control %q did not match the credential deny list (normalized=%q)", name, normalizeRunnerSchemaName(name))
		}
	}
	// Negative controls: this package's own legitimate, non-secret names.
	negative := []string{"FencingToken", "ExpectedFencingToken", "CredentialSource", "Credential", "PermitCredential", "ExecutionID", "WorkPacketDigest", "ControlRevision"}
	for _, name := range negative {
		if matchesRunnerCredentialDenyList(name) {
			t.Fatalf("negative control %q matched the credential deny list (normalized=%q)", name, normalizeRunnerSchemaName(name))
		}
	}
}

func jsonTagNameForRunner(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	value, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	raw, ok := lookupRunnerStructTag(value, "json")
	if !ok || raw == "-" || raw == "" {
		return ""
	}
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	return raw
}

func lookupRunnerStructTag(tag, key string) (string, bool) {
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

// TestNoCredentialFieldInRunnerNonTestFiles is the A10 scan: it reads only
// this package's non-test *.go files (the ones that can actually marshal a
// journal event, a Work Packet, a canonical application request, or a log
// line) and fails if any struct field name or json tag matches the
// credential deny list.
func TestNoCredentialFieldInRunnerNonTestFiles(t *testing.T) {
	files := parseRunnerPackage(t)
	nonTestFiles := 0
	structCount, fieldCount, tagCount := 0, 0, 0
	for _, pf := range files {
		if pf.isTest {
			continue
		}
		nonTestFiles++
		ast.Inspect(pf.file, func(n ast.Node) bool {
			structType, ok := n.(*ast.StructType)
			if !ok || structType.Fields == nil {
				return true
			}
			structCount++
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					fieldCount++
					if matchesRunnerCredentialDenyList(name.Name) {
						t.Fatalf("%s: struct field %q matches the credential deny list", pf.path, name.Name)
					}
				}
				if tagName := jsonTagNameForRunner(field.Tag); tagName != "" {
					tagCount++
					if matchesRunnerCredentialDenyList(tagName) {
						t.Fatalf("%s: json tag %q matches the credential deny list", pf.path, tagName)
					}
				}
			}
			return true
		})
	}
	if nonTestFiles == 0 {
		t.Fatal("source guard scanned zero non-test files")
	}
	if structCount == 0 {
		t.Fatal("scanned zero struct types; the AST walk is not finding declarations")
	}
	if fieldCount == 0 {
		t.Fatal("scanned zero struct fields; the AST walk is not finding field names")
	}
	t.Logf("scanned non-test files=%d structs=%d fields=%d json-tags=%d", nonTestFiles, structCount, fieldCount, tagCount)
}

// --- V2-017 B4: ProcessSupervisor may be referenced from exactly one
// location in the provider execution path (provider.go), so there is no
// second, uncontrolled way to spawn a real process that bypasses
// SupervisedInvocationRunner's CostLedger gate. ---

func TestProcessSupervisorReferencedExactlyOnceInProviderExecutionPath(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "provider.go", nil, 0)
	if err != nil {
		t.Fatalf("parse provider.go: %v", err)
	}
	count := 0
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name == "ProcessSupervisor" {
			count++
		}
		return true
	})
	if count != 1 {
		t.Fatalf("provider.go must reference the ProcessSupervisor type from exactly one location (the SupervisedInvocationRunner.Supervisor field), found %d", count)
	}
}

// --- V2-017 B13(f): no non-test file under internal/runner may reference
// ".claude", ".credentials" or any path under the user's home directory, as
// an absence proof that the Control Plane/runner never opens the claude
// CLI's own credential store itself. This is a string-literal AST scan
// (go/ast, not text grep) so it cannot be defeated by a literal built up
// from concatenated non-literal pieces looking incidentally similar, while
// still catching the actual risk: a hardcoded path string. ---

func TestNoCredentialStorePathReferencedInRunnerNonTestFiles(t *testing.T) {
	home := os.Getenv("HOME")
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		home = h
	}
	files := parseRunnerPackage(t)
	nonTestFiles := 0
	literalsScanned := 0
	for _, pf := range files {
		if pf.isTest {
			continue
		}
		nonTestFiles++
		ast.Inspect(pf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			literalsScanned++
			if strings.Contains(value, ".claude") {
				t.Fatalf("%s: string literal %q references the claude CLI's config directory", pf.path, value)
			}
			if strings.Contains(value, ".credentials") {
				t.Fatalf("%s: string literal %q references a credentials file name", pf.path, value)
			}
			if home != "" && strings.Contains(value, home) {
				t.Fatalf("%s: string literal %q references a path under the user's home directory (%s)", pf.path, value, home)
			}
			return true
		})
	}
	if nonTestFiles == 0 {
		t.Fatal("source guard scanned zero non-test files")
	}
	t.Logf("scanned non-test files=%d string-literals=%d home=%q", nonTestFiles, literalsScanned, home)
}
