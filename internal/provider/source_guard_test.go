package provider_test

// Source guard for internal/provider (V2-027 A8, A10, A13, A16), modelled on
// internal/runner/source_guard_test.go and internal/domain's original
// go/ast pattern.
//
// Every scan below reads the AST, never the file text, and every scan fails
// outright on a zero-file walk, so none of them can pass because a glob was
// mis-written or the working directory was wrong. The deny-list matcher is
// itself verified against known-positive and known-negative names first, for
// the same reason: a matcher that matches nothing would otherwise satisfy
// every assertion that uses it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// internalPrefix is the module path of this repository's internal packages.
// The forbidden internal import paths below are assembled from it rather than
// written as literals: internal/ci's manifest derivation reads module-path
// string literals in source text, test files included, and treats one as a
// declared component edge, so spelling either path out here would move the
// declared component DAG. internal/application/source_guard_test.go states
// the same reason for the same construction.
const internalPrefix = "github.com/takushi/agentic-loop-foundation/v2/internal/"

// forbiddenProviderImports must appear in no non-test file of this package.
//
// os/exec is how a file here would start a Provider CLI, which is the one
// thing this package must not be able to do: the runner's supervised path
// owns execution and is the only thing that debits a cost ledger first. net,
// net/http and net/url are how it would reach a Provider over the wire
// instead. The four internal paths are the packages that would turn this
// leaf component into a non-leaf: internal/domain (whose taxonomy the breaker
// deliberately maps to as a declared table rather than an import),
// internal/runner, internal/application, and internal/quota -- which is a
// different resource entirely, being the Firestore daily free-tier
// reservation and not a Provider usage window.
var forbiddenProviderImports = []string{
	"os/exec",
	"net",
	"net/http",
	"net/url",
	internalPrefix + "domain",
	internalPrefix + "runner",
	internalPrefix + "application",
	internalPrefix + "quota",
}

type guardFile struct {
	path    string
	isTest  bool
	file    *ast.File
	imports []string
}

// parseProviderPackage parses every *.go file in the current directory. go
// test runs with the package directory as its working directory. A zero-file
// scan is a Fatal.
func parseProviderPackage(t *testing.T) []guardFile {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse dir: %v", err)
	}
	var out []guardFile
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			imports := make([]string, 0, len(file.Imports))
			for _, spec := range file.Imports {
				value, unquoteErr := strconv.Unquote(spec.Path.Value)
				if unquoteErr != nil {
					t.Fatalf("%s: unquote import %s: %v", path, spec.Path.Value, unquoteErr)
				}
				imports = append(imports, value)
			}
			out = append(out, guardFile{path: path, isTest: strings.HasSuffix(path, "_test.go"), file: file, imports: imports})
		}
	}
	if len(out) == 0 {
		t.Fatal("source guard scanned zero files; the working directory is not internal/provider or the parse is broken")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func nonTestGuardFiles(t *testing.T) []guardFile {
	t.Helper()
	var out []guardFile
	for _, f := range parseProviderPackage(t) {
		if !f.isTest {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("source guard scanned zero non-test files")
	}
	return out
}

// providerCredentialDenyList mirrors internal/runner's list, which mirrors
// internal/domain's. It deliberately excludes the bare word "token":
// Usage.InputTokens, Usage.OutputTokens and this package's Probe are
// legitimate non-secret concepts, and a deny list that caught them would be
// disabled by the first person who hit it. A field literally named
// "AccessToken" or "CredentialValue" is still caught.
var providerCredentialDenyList = []string{
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
	"sessionid",
	"authorization",
	"rawprompt",
	"rawproviderout",
	"credentialvalue",
	"credential",
	"plaintextcredential",
	"executablepath",
	"owneridentity",
}

func normalizeGuardName(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "_", ""), "-", ""))
}

func matchesProviderCredentialDenyList(name string) bool {
	normalized := normalizeGuardName(name)
	for _, entry := range providerCredentialDenyList {
		if strings.Contains(normalized, entry) {
			return true
		}
	}
	return false
}

// TestProviderDenyListMatchersAreVerified proves both matchers this file
// relies on actually match known-bad names and do not match this package's
// legitimate names, so no scan below can pass vacuously.
func TestProviderDenyListMatchersAreVerified(t *testing.T) {
	positive := []string{
		"Password", "APIKey", "AccessToken", "SecretValue", "PlaintextCredential",
		"raw_prompt", "raw_provider_output", "SessionID", "session_id",
		"ExecutablePath", "executable_path", "OwnerIdentity", "CredentialValue",
	}
	for _, name := range positive {
		if !matchesProviderCredentialDenyList(name) {
			t.Fatalf("positive control %q did not match the credential deny list (normalized=%q)", name, normalizeGuardName(name))
		}
	}
	negative := []string{
		"Probe", "Serial", "InputTokens", "OutputTokens", "PreflightRecordID",
		"ContentDigest", "OutputDigest", "Checkpoint", "FailureClass", "SlotState",
	}
	for _, name := range negative {
		if matchesProviderCredentialDenyList(name) {
			t.Fatalf("negative control %q matched the credential deny list (normalized=%q)", name, normalizeGuardName(name))
		}
	}

	// The condition-word matcher, same discipline.
	for _, name := range forbiddenConditionWords() {
		if !isForbiddenConditionWord(name) {
			t.Fatalf("positive control %q is not recognised as a forbidden condition word", name)
		}
		if !isForbiddenConditionWord(strings.ToUpper(name[:1]) + name[1:]) {
			t.Fatalf("positive control %q is not recognised when capitalised", name)
		}
	}
	for _, name := range []string{"Sending", "NotSending", "Probing", "available", "quarantined", "openingTable", "lookup", "upstream", "download"} {
		if isForbiddenConditionWord(name) {
			t.Fatalf("negative control %q was wrongly recognised as a forbidden condition word", name)
		}
	}
}

// forbiddenConditionWords are the identifiers that would make a decision of
// this Loop read as a statement about the Provider's condition. The counted
// population is only invocations that went through this Loop's own execution
// path -- the runner's own runbook records six real claude invocations, run by
// hand from a shell during V2-017, that the ledger never saw -- so a field
// named "healthy" would be false in the one case that matters: a Provider a
// person is using successfully while this Loop's own last attempts failed.
func forbiddenConditionWords() []string {
	return []string{"healthy", "unhealthy", "broken", "down", "up", "alive", "dead", "ok"}
}

// isForbiddenConditionWord matches the whole identifier, case-insensitively,
// and never a substring: "openingTable" contains "up" only by accident, and
// a substring rule would forbid correct code and be turned off.
func isForbiddenConditionWord(name string) bool {
	lowered := strings.ToLower(name)
	for _, word := range forbiddenConditionWords() {
		if lowered == word {
			return true
		}
	}
	return false
}

func TestProviderNonTestFilesImportNothingThatCouldExecuteOrReachOut(t *testing.T) {
	files := nonTestGuardFiles(t)
	scanned := 0
	for _, f := range files {
		for _, imported := range f.imports {
			scanned++
			for _, forbidden := range forbiddenProviderImports {
				if imported == forbidden || strings.HasPrefix(imported, forbidden+"/") {
					t.Fatalf("%s imports %q, which is forbidden in this package (matched %q)", f.path, imported, forbidden)
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned zero imports; the AST walk is not finding import specs")
	}
	t.Logf("scanned non-test files=%d imports=%d against %d forbidden paths", len(files), scanned, len(forbiddenProviderImports))
}

func jsonTagName(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	value, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	raw, present := lookupStructTag(value, "json")
	if !present || raw == "-" || raw == "" {
		return ""
	}
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	return raw
}

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

func TestNoCredentialFieldOrTagInProviderNonTestFiles(t *testing.T) {
	files := nonTestGuardFiles(t)
	structCount, fieldCount, tagCount := 0, 0, 0
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			structType, isStruct := n.(*ast.StructType)
			if !isStruct || structType.Fields == nil {
				return true
			}
			structCount++
			for _, field := range structType.Fields.List {
				for _, name := range field.Names {
					fieldCount++
					if matchesProviderCredentialDenyList(name.Name) {
						t.Fatalf("%s: struct field %q matches the credential deny list", f.path, name.Name)
					}
				}
				if tag := jsonTagName(field.Tag); tag != "" {
					tagCount++
					if matchesProviderCredentialDenyList(tag) {
						t.Fatalf("%s: json tag %q matches the credential deny list", f.path, tag)
					}
				}
			}
			return true
		})
	}
	if structCount == 0 || fieldCount == 0 {
		t.Fatalf("scanned structs=%d fields=%d; the AST walk is not finding declarations", structCount, fieldCount)
	}
	t.Logf("scanned non-test files=%d structs=%d fields=%d json-tags=%d", len(files), structCount, fieldCount, tagCount)
}

func TestNoIdentifierNamesTheProvidersCondition(t *testing.T) {
	files := nonTestGuardFiles(t)
	identifiers := 0
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			ident, isIdent := n.(*ast.Ident)
			if !isIdent {
				return true
			}
			identifiers++
			if isForbiddenConditionWord(ident.Name) {
				t.Fatalf("%s: identifier %q names the Provider's condition; this package may only name this Loop's own decision (sending, not-sending, probing)", f.path, ident.Name)
			}
			return true
		})
	}
	if identifiers == 0 {
		t.Fatal("scanned zero identifiers")
	}
	t.Logf("scanned non-test files=%d identifiers=%d against %v", len(files), identifiers, forbiddenConditionWords())
}

// monetaryWords is the closed vocabulary ban of A10. Reaching a limit in this
// Loop is a stop for inspection, never a success, never a failure and never a
// billing event, and the ban is the only enforceable form of that ruling one
// layer below the runner's own ledger.
func monetaryWords() []string {
	return []string{"usd", "cost", "price", "billing", "spend", "budget", "credit"}
}

func TestNoMonetaryVocabularyInProviderDeclaredNames(t *testing.T) {
	files := nonTestGuardFiles(t)
	identifiers, tags := 0, 0
	check := func(path, kind, name string) {
		lowered := strings.ToLower(strings.ReplaceAll(name, "_", ""))
		for _, word := range monetaryWords() {
			if strings.Contains(lowered, word) {
				t.Fatalf("%s: %s %q contains the monetary word %q; this package's usage window is not a monetary object", path, kind, name, word)
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			if ident, isIdent := n.(*ast.Ident); isIdent {
				identifiers++
				check(f.path, "identifier", ident.Name)
				return true
			}
			structType, isStruct := n.(*ast.StructType)
			if !isStruct || structType.Fields == nil {
				return true
			}
			for _, field := range structType.Fields.List {
				if tag := jsonTagName(field.Tag); tag != "" {
					tags++
					check(f.path, "json tag", tag)
				}
			}
			return true
		})
	}
	// Positive control: the matcher must actually catch the shapes it exists
	// for, so a silently-empty scan cannot pass.
	for _, name := range []string{"TotalCostUSD", "total_cost_usd", "SpendLimit", "creditsRemaining", "BudgetExceeded", "priceCents", "billingID"} {
		lowered := strings.ToLower(strings.ReplaceAll(name, "_", ""))
		matched := false
		for _, word := range monetaryWords() {
			if strings.Contains(lowered, word) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("positive control %q was not caught by the monetary vocabulary scan", name)
		}
	}
	if identifiers == 0 {
		t.Fatal("scanned zero identifiers")
	}
	t.Logf("scanned non-test files=%d identifiers=%d json-tags=%d against %v", len(files), identifiers, tags, monetaryWords())
}

// TestProviderPackageIsDeterministicByConstruction is A16. It scans every
// file in the package, test files included, because a fixed sleep or a
// goroutine in a test is exactly as much of a determinism defect as one in
// production code.
func TestProviderPackageIsDeterministicByConstruction(t *testing.T) {
	forbiddenTimeCalls := map[string]bool{
		"Now": true, "Sleep": true, "After": true, "Tick": true,
		"NewTimer": true, "NewTicker": true, "AfterFunc": true,
	}
	files := parseProviderPackage(t)
	selectors, goStatements := 0, 0
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			if _, isGo := n.(*ast.GoStmt); isGo {
				goStatements++
				t.Fatalf("%s: a go statement makes this package's tests non-deterministic; waiting is bounded deadline polling over an injected time value", f.path)
			}
			sel, isSel := n.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			pkg, isIdent := sel.X.(*ast.Ident)
			if !isIdent || pkg.Name != "time" {
				return true
			}
			selectors++
			if forbiddenTimeCalls[sel.Sel.Name] {
				t.Fatalf("%s: time.%s reads or waits on the wall clock; every timestamp and every deadline comparison must come from a value the caller passes in", f.path, sel.Sel.Name)
			}
			return true
		})
	}
	if goStatements != 0 {
		t.Fatalf("go statements = %d", goStatements)
	}
	t.Logf("scanned files=%d time.X selectors=%d go-statements=%d", len(files), selectors, goStatements)
}

// TestFailureClassSetIsExactlyWhatTheASTDeclares is the "by construction"
// half of A11: the opening table and FailureClasses are both compared against
// the FailureClass constants read out of this package's own AST, so a tenth
// class added without a row and without a listing fails here.
func TestFailureClassSetIsExactlyWhatTheASTDeclares(t *testing.T) {
	declared := map[string]bool{}
	for _, f := range nonTestGuardFiles(t) {
		for _, decl := range f.file.Decls {
			gen, isGen := decl.(*ast.GenDecl)
			if !isGen || gen.Tok != token.CONST {
				continue
			}
			currentType := ""
			for _, spec := range gen.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				if value.Type != nil {
					if ident, isIdent := value.Type.(*ast.Ident); isIdent {
						currentType = ident.Name
					} else {
						currentType = ""
					}
				}
				if currentType != "FailureClass" {
					continue
				}
				for _, v := range value.Values {
					lit, isLit := v.(*ast.BasicLit)
					if !isLit || lit.Kind != token.STRING {
						t.Fatalf("%s: a FailureClass constant is not a string literal; the AST scan cannot enumerate it", f.path)
					}
					unquoted, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: unquote %s: %v", f.path, lit.Value, err)
					}
					declared[unquoted] = true
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("the AST scan found zero FailureClass constants")
	}
	listed := map[string]bool{}
	for _, class := range provider.FailureClasses() {
		listed[string(class)] = true
	}
	tabled := map[string]bool{}
	for _, class := range provider.OpeningTableClasses() {
		tabled[string(class)] = true
	}
	for value := range declared {
		if !listed[value] {
			t.Fatalf("FailureClass %q is declared in the package but is not in FailureClasses(); a class the package declares must be enumerable", value)
		}
		if !tabled[value] {
			t.Fatalf("FailureClass %q is declared in the package but has no row in the circuit breaker's opening table", value)
		}
	}
	for value := range listed {
		if !declared[value] {
			t.Fatalf("FailureClasses() names %q, which the package does not declare as a constant", value)
		}
	}
	for value := range tabled {
		if !declared[value] {
			t.Fatalf("the opening table has a row for %q, which the package does not declare as a constant", value)
		}
	}
	t.Logf("declared FailureClass constants = %d, all listed and all tabled", len(declared))
}
