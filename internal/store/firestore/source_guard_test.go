package firestore

// This file proves, mechanically from source rather than by review, that the
// five read-side envelope refusal sites this task converted are the COMPLETE
// set: the RecordSchema constant is named only where it may be named, and no
// read-side comparison of an envelope value survives outside the predicate.
//
// It exists because five sites were found by grep, and a grep is not a guard:
// a sixth comparison added later would silently reinstate the split this task
// removed, and it would be added on a scan path, because that is where the
// pattern was already duplicated three times.
//
// Modelled on internal/domain/source_guard_test.go, including its refusal to
// pass on a zero-file scan, which is what stops a mis-written glob from making
// the assertion vacuous.

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

// envelopeIdentifier is the constant whose use sites this file constrains.
const envelopeIdentifier = "RecordSchema"

// envelopeConstUseAllowlist is the complete set of top-level declarations in
// a NON-TEST file of this package that may name the RecordSchema constant:
// its own declaration, the accepted-set declaration, the native-id predicate,
// and the three write sites. Anything else must go through
// RecordSchemaAccepted.
var envelopeConstUseAllowlist = map[string]string{
	"AcceptedRecordSchemas": "the accepted-set declaration",
	"recordSchemaIsNative":  "the native-id predicate",
	"EncodeRecord":          "write site 1 (the JSON codec)",
	"encodeDocument":        "write site 2 (the Firestore document)",
	"BootstrapInstallation": "write site 3 (the installation record)",
}

// envelopeComparisonAllowlist is the complete set of top-level declarations in
// a NON-TEST file that may compare an envelope value with == or !=. Both are
// predicates; neither is a read site.
var envelopeComparisonAllowlist = map[string]bool{
	"RecordSchemaAccepted": true,
	"recordSchemaIsNative": true,
}

type parsedStoreFile struct {
	path   string
	isTest bool
	file   *ast.File
}

// parseFirestorePackage parses every *.go file in the current directory (the
// internal/store/firestore package source directory: go test always runs with
// the package directory as its working directory). It fails outright on an
// empty file set.
func parseFirestorePackage(t *testing.T) []parsedStoreFile {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("source guard scanned zero files; the working directory is not internal/store/firestore or the glob is broken")
	}
	sort.Strings(matches)
	fset := token.NewFileSet()
	parsed := make([]parsedStoreFile, 0, len(matches))
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed = append(parsed, parsedStoreFile{path: path, isTest: strings.HasSuffix(path, "_test.go"), file: file})
	}
	return parsed
}

// topLevelDeclName names the declaration a node belongs to: a function's own
// name, or the first name of a var/const/type spec. It is what the two
// allowlists above are keyed by.
func topLevelDecls(file *ast.File) map[string]ast.Node {
	out := map[string]ast.Node{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil {
				out[d.Name.Name] = d
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.ValueSpec:
					if len(sp.Names) > 0 {
						out[sp.Names[0].Name] = sp
					}
				case *ast.TypeSpec:
					if sp.Name != nil {
						out[sp.Name.Name] = sp
					}
				}
			}
		}
	}
	return out
}

// declaringPositions collects the positions of every identifier occurrence
// that is NOT a use of a package-level constant: a struct or interface field
// name (document.RecordSchema is a legitimate field), a selector's Sel
// (d.RecordSchema is a field read, not the constant), a composite-literal key
// (document{RecordSchema: ...} names the field), and a var/const/type spec's
// own name (the declaration itself).
//
// Distinguishing these syntactically is required rather than optional: a text
// grep for "RecordSchema" matches the document struct field, its selector at
// every read site and the constant, and cannot tell them apart.
func declaringPositions(n ast.Node) map[token.Pos]bool {
	out := map[token.Pos]bool{}
	ast.Inspect(n, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.SelectorExpr:
			if x.Sel != nil {
				out[x.Sel.Pos()] = true
			}
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok {
				out[id.Pos()] = true
			}
		case *ast.Field:
			for _, name := range x.Names {
				out[name.Pos()] = true
			}
		case *ast.ValueSpec:
			for _, name := range x.Names {
				out[name.Pos()] = true
			}
		}
		return true
	})
	return out
}

// constUseCount counts uses of the envelope CONSTANT inside n.
func constUseCount(n ast.Node) int {
	skip := declaringPositions(n)
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok || id.Name != envelopeIdentifier || skip[id.Pos()] {
			return true
		}
		count++
		return true
	})
	return count
}

// namesEnvelope reports whether expr is an envelope value: either a use of the
// RecordSchema constant or a read of the RecordSchema document field.
func namesEnvelope(expr ast.Expr, skip map[token.Pos]bool) bool {
	switch x := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return x.Name == envelopeIdentifier && !skip[x.Pos()]
	case *ast.SelectorExpr:
		return x.Sel != nil && x.Sel.Name == envelopeIdentifier
	}
	return false
}

// envelopeComparisonCount counts the == and != comparisons inside n that have
// an envelope value as an operand. This is the matcher the controls below
// verify.
func envelopeComparisonCount(n ast.Node) int {
	skip := declaringPositions(n)
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		bin, ok := node.(*ast.BinaryExpr)
		if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
			return true
		}
		if namesEnvelope(bin.X, skip) || namesEnvelope(bin.Y, skip) {
			count++
		}
		return true
	})
	return count
}

// TestEnvelopeSourceMatcherIsVerified runs the matcher against a synthesized
// known-positive source containing exactly the comparison this guard exists to
// catch, and a known-negative that routes the same decision through the
// predicate. Without these controls a broken matcher would report zero
// findings and the guard below would pass vacuously.
func TestEnvelopeSourceMatcherIsVerified(t *testing.T) {
	const positiveFieldComparison = `package firestore
func scanSomething(d document) error {
	if d.RecordSchema != RecordSchema {
		return ErrInvalidSchema
	}
	return nil
}
`
	const positiveConstComparison = `package firestore
func decodeSomething(schema string) error {
	if schema == RecordSchema {
		return nil
	}
	return ErrInvalidSchema
}
`
	const negativeThroughThePredicate = `package firestore
func scanSomething(d document) error {
	if !RecordSchemaAccepted(d.RecordSchema) {
		return ErrInvalidSchema
	}
	return nil
}
`
	const negativeUnrelatedComparison = `package firestore
func scanSomething(d document, kind string) error {
	if d.Kind != kind {
		return ErrInvalidSchema
	}
	return nil
}
`
	fset := token.NewFileSet()
	parse := func(name, src string) *ast.File {
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse control %s: %v", name, err)
		}
		return f
	}

	positives := map[string]string{
		"field-comparison": positiveFieldComparison,
		"const-comparison": positiveConstComparison,
	}
	for name, src := range positives {
		got := envelopeComparisonCount(parse(name, src))
		if got != 1 {
			t.Fatalf("known-positive control %s: matcher found %d envelope comparisons, want 1", name, got)
		}
		t.Logf("known-positive control %s: 1 envelope comparison found, as expected", name)
	}
	negatives := map[string]string{
		"through-the-predicate": negativeThroughThePredicate,
		"unrelated-comparison":  negativeUnrelatedComparison,
	}
	for name, src := range negatives {
		got := envelopeComparisonCount(parse(name, src))
		if got != 0 {
			t.Fatalf("known-negative control %s: matcher found %d envelope comparisons, want 0", name, got)
		}
		t.Logf("known-negative control %s: 0 envelope comparisons found, as expected", name)
	}

	// The const-use classifier is controlled the same way: a struct field
	// declaration and a field read must not be counted as constant uses.
	const fieldOnly = `package firestore
type document struct {
	RecordSchema string ` + "`firestore:\"record_schema\"`" + `
}
func readIt(d document) string { return d.RecordSchema }
`
	if got := constUseCount(parse("field-only", fieldOnly)); got != 0 {
		t.Fatalf("known-negative control field-only: classifier counted %d constant uses, want 0", got)
	}
	const constOnly = `package firestore
var AcceptedRecordSchemas = []string{RecordSchema}
`
	if got := constUseCount(parse("const-only", constOnly)); got != 1 {
		t.Fatalf("known-positive control const-only: classifier counted %d constant uses, want 1", got)
	}
	t.Log("const-use classifier controls: field declaration and field read counted 0, accepted-set declaration counted 1")
}

func TestTheFiveReadSideEnvelopeSitesAreTheCompleteSet(t *testing.T) {
	files := parseFirestorePackage(t)
	const wantMinScannedFiles = 4
	if len(files) < wantMinScannedFiles {
		t.Fatalf("scanned %d files, want at least %d", len(files), wantMinScannedFiles)
	}

	// The declaration must actually exist. A refactor that renamed the
	// constant would otherwise make every assertion below vacuous.
	declarationFound := false
	declaredValue := ""
	nonTestFiles := 0
	for _, pf := range files {
		if pf.isTest {
			continue
		}
		nonTestFiles++
		for _, decl := range pf.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range value.Names {
					if name.Name != envelopeIdentifier {
						continue
					}
					declarationFound = true
					if i < len(value.Values) {
						if lit, ok := value.Values[i].(*ast.BasicLit); ok {
							declaredValue, _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	if nonTestFiles == 0 {
		t.Fatal("scanned zero non-test files")
	}
	if !declarationFound {
		t.Fatalf("no const declaration of %s was found in a non-test file; if it was renamed, this guard is vacuous and must be updated", envelopeIdentifier)
	}
	if declaredValue != RecordSchema {
		t.Fatalf("the const declaration of %s carries literal %q but the package value is %q", envelopeIdentifier, declaredValue, RecordSchema)
	}

	// Every constant use in a non-test file must sit in an allowlisted
	// declaration, and every allowlisted declaration must actually contain
	// one, so the allowlist cannot rot into a list of names that no longer
	// exist.
	usesByDecl := map[string]int{}
	for _, pf := range files {
		if pf.isTest {
			continue
		}
		for name, node := range topLevelDecls(pf.file) {
			count := constUseCount(node)
			if count == 0 {
				continue
			}
			if name == envelopeIdentifier {
				continue // the declaration itself
			}
			if _, ok := envelopeConstUseAllowlist[name]; !ok {
				t.Fatalf("%s: %s names the %s constant %d time(s); only the accepted-set declaration, the native-id predicate and the three write sites may. A read-side decision must call RecordSchemaAccepted instead", pf.path, name, envelopeIdentifier, count)
			}
			usesByDecl[name] += count
		}
	}
	for name, role := range envelopeConstUseAllowlist {
		if usesByDecl[name] == 0 {
			t.Fatalf("allowlist entry %q (%s) does not name the %s constant anywhere in a non-test file; the allowlist is stale and the guard is weaker than it reads", name, role, envelopeIdentifier)
		}
	}
	if len(usesByDecl) != len(envelopeConstUseAllowlist) {
		t.Fatalf("found constant uses in %d declarations, allowlist has %d", len(usesByDecl), len(envelopeConstUseAllowlist))
	}

	// No read-side comparison of an envelope value survives outside the two
	// predicates. This is the property that makes "five sites, one predicate"
	// checkable rather than reviewable.
	comparisons := map[string]int{}
	for _, pf := range files {
		if pf.isTest {
			continue
		}
		for name, node := range topLevelDecls(pf.file) {
			count := envelopeComparisonCount(node)
			if count == 0 {
				continue
			}
			if !envelopeComparisonAllowlist[name] {
				t.Fatalf("%s: %s compares an envelope value with == or != %d time(s). Every read-side envelope decision must call RecordSchemaAccepted; a sixth comparison is exactly the split this guard exists to prevent", pf.path, name, count)
			}
			comparisons[name] += count
		}
	}
	if comparisons["recordSchemaIsNative"] == 0 {
		t.Fatal("recordSchemaIsNative contains no envelope comparison; the comparison matcher is not finding the one comparison that legitimately exists")
	}
	t.Logf("scanned files=%d non-test=%d; constant uses by declaration=%v; envelope comparisons by declaration=%v", len(files), nonTestFiles, usesByDecl, comparisons)
}
