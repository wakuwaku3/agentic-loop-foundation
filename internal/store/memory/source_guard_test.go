package memory

// This file proves, mechanically from source rather than by review, that this
// adapter has NO record envelope: no struct field, constant, variable or
// string literal named or valued record_schema exists in the package.
//
// Why this is the checkable form of "the same rule is effective in both
// stores": the memory adapter holds typed Go values in maps and has no
// serialization boundary, so it has no envelope to widen. Copying
// internal/store/firestore's accepted set and predicate here would create a
// second envelope implementation carrying a rule with no data to apply it to,
// and two implementations are exactly how two adapters come to disagree. The
// requirement's real content is that there is exactly ONE envelope
// implementation in the repository; the way to keep that true is to assert
// that this adapter has none.
//
// If this adapter ever gains a serialization boundary, this test fails and
// forces the author to route it through internal/store/firestore's single
// predicate rather than write a second one.
//
// Nothing else in internal/store/memory is edited by the task that added this
// file: neither store.go nor store_test.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
)

// envelopeName is the normalised spelling this guard refuses. Normalisation
// (lowercase, underscores and hyphens removed) is applied to every candidate
// so that RecordSchema, record_schema, recordSchema and RECORD_SCHEMA are all
// caught by one entry.
const envelopeName = "recordschema"

// guardFileName is this file, which is excluded from its own scan because it
// necessarily spells the forbidden name.
const guardFileName = "source_guard_test.go"

func normalizeEnvelopeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "")
	name = strings.ReplaceAll(name, "-", "")
	return name
}

type parsedMemoryFile struct {
	path   string
	isTest bool
	file   *ast.File
}

// parseMemoryPackage parses every *.go file in the current directory (the
// internal/store/memory package source directory: go test always runs with the
// package directory as its working directory). It fails outright on an empty
// file set, so a mis-written glob cannot make the scan pass vacuously.
func parseMemoryPackage(t *testing.T) []parsedMemoryFile {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("source guard scanned zero files; the working directory is not internal/store/memory or the glob is broken")
	}
	sort.Strings(matches)
	fset := token.NewFileSet()
	parsed := make([]parsedMemoryFile, 0, len(matches))
	for _, path := range matches {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed = append(parsed, parsedMemoryFile{path: path, isTest: strings.HasSuffix(path, "_test.go"), file: file})
	}
	return parsed
}

func TestMemoryAdapterHasNoRecordEnvelope(t *testing.T) {
	// Control: the matcher recognises every spelling it claims to, and does
	// not fire on unrelated names. Without this a broken normaliser would
	// report zero findings and the scan below would pass vacuously.
	for _, positive := range []string{"RecordSchema", "record_schema", "recordSchema", "RECORD_SCHEMA", "record-schema"} {
		if normalizeEnvelopeName(positive) != envelopeName {
			t.Fatalf("known-positive control %q normalised to %q, want %q", positive, normalizeEnvelopeName(positive), envelopeName)
		}
	}
	for _, negative := range []string{"schema", "Requirement", "recordSchemaVersionOfSomethingElse", "requirements"} {
		if normalizeEnvelopeName(negative) == envelopeName {
			t.Fatalf("known-negative control %q normalised to the envelope name", negative)
		}
	}

	files := parseMemoryPackage(t)
	const wantMinScannedFiles = 2
	if len(files) < wantMinScannedFiles {
		t.Fatalf("scanned %d files, want at least %d", len(files), wantMinScannedFiles)
	}

	// This guard file is excluded from its own scan: it necessarily spells the
	// forbidden name in envelopeName and in the matcher controls above. Every
	// other file in the package, production and test alike, is scanned.
	structCount, fieldCount, literalCount, identCount, skipped := 0, 0, 0, 0, 0
	for _, pf := range files {
		if pf.path == guardFileName {
			skipped++
			continue
		}
		ast.Inspect(pf.file, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.StructType:
				if x.Fields == nil {
					return true
				}
				structCount++
				for _, field := range x.Fields.List {
					for _, name := range field.Names {
						fieldCount++
						if normalizeEnvelopeName(name.Name) == envelopeName {
							t.Fatalf("%s: struct field %q is a record envelope. internal/store/firestore holds the repository's only envelope implementation; route this through its RecordSchemaAccepted predicate rather than declaring a second one", pf.path, name.Name)
						}
					}
					if field.Tag != nil {
						tag, err := strconv.Unquote(field.Tag.Value)
						if err == nil && strings.Contains(normalizeEnvelopeName(tag), envelopeName) {
							t.Fatalf("%s: struct tag %s names a record envelope", pf.path, field.Tag.Value)
						}
					}
				}
			case *ast.ValueSpec:
				for _, name := range x.Names {
					identCount++
					if normalizeEnvelopeName(name.Name) == envelopeName {
						t.Fatalf("%s: declaration %q is a record envelope constant or variable", pf.path, name.Name)
					}
				}
			case *ast.TypeSpec:
				if x.Name != nil {
					identCount++
					if normalizeEnvelopeName(x.Name.Name) == envelopeName {
						t.Fatalf("%s: type %q is a record envelope", pf.path, x.Name.Name)
					}
				}
			case *ast.FuncDecl:
				if x.Name != nil {
					identCount++
					if strings.Contains(normalizeEnvelopeName(x.Name.Name), envelopeName) {
						t.Fatalf("%s: function %q names a record envelope", pf.path, x.Name.Name)
					}
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				literalCount++
				value, err := strconv.Unquote(x.Value)
				if err != nil {
					return true
				}
				if strings.Contains(normalizeEnvelopeName(value), envelopeName) {
					t.Fatalf("%s: string literal %s carries a record envelope value", pf.path, x.Value)
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
	if identCount == 0 {
		t.Fatal("scanned zero declared identifiers; the AST walk is not finding declarations")
	}
	if literalCount == 0 {
		t.Fatal("scanned zero string literals; the literal scan is not running")
	}
	if skipped != 1 {
		t.Fatalf("excluded %d files from the scan, want exactly 1 (%s); a wider exclusion would make the guard vacuous", skipped, guardFileName)
	}
	if len(files)-skipped == 0 {
		t.Fatal("every file was excluded from the scan")
	}
	t.Logf("scanned files=%d (excluded %s) structs=%d fields=%d declared-identifiers=%d string-literals=%d; no record envelope exists in this package", len(files)-skipped, guardFileName, structCount, fieldCount, identCount, literalCount)
}

// ===========================================================================
// V2-087 A5: completeness of the quota instrumentation, enforced mechanically
// ===========================================================================
//
// A read this adapter forgets to count is not a visible bug. It is a SILENT
// DISCOUNT: the end-of-transaction true-up over-credits, this adapter quietly
// charges less than internal/store/firestore does for the same work, and the
// divergence V2-087 exists to remove comes back through whichever method
// somebody adds next month. Nothing about that shows up as a failing test.
//
// So the expected set is derived from the PORT INTERFACE by reflection, not
// written down here. A hand-written method list is exactly what goes stale --
// seventy methods were measured on unit -- and it would pass silently for a new
// UnitOfWork method that reads state and forgets to charge for it. Reflection
// over application.UnitOfWork plus a go/ast walk of this package's own source
// is the only form of this check that survives the next port addition.

// countingHelpers are the helper names in store.go that charge a record read or
// a record write. A method of the port that calls none of them is charging
// nothing, and this guard fails.
var countingHelpers = []string{
	"countKeyedRead",
	"countFetchedReads",
	"countWrite",
	"countQueueCounter",
	"countLeaseScan",
	"countControlScan",
}

// uncountedPortMethods is the allow-list: methods of application.UnitOfWork
// that touch NO record at all and therefore charge nothing. Every entry needs a
// one-line justification, and a generous allow-list removes this task's main
// safety property, so the list is deliberately empty: every method of the port
// this adapter implements reads or writes at least one record.
var uncountedPortMethods = map[string]string{}

// unitReceiverType is the adapter's unit-of-work type, whose methods must all
// charge. It is matched on the AST receiver, so a method moved to another file
// in this package is still found.
const unitReceiverType = "unit"

// portMethodNames enumerates application.UnitOfWork's method set by reflection.
// An interface's method set is exactly what an implementation must provide, so
// this is the set that has to be instrumented -- no more and no less.
func portMethodNames(t *testing.T) []string {
	t.Helper()
	iface := reflect.TypeOf((*application.UnitOfWork)(nil)).Elem()
	if iface.Kind() != reflect.Interface {
		t.Fatalf("application.UnitOfWork resolved to a %s, not an interface; the reflection target is wrong", iface.Kind())
	}
	names := make([]string, 0, iface.NumMethod())
	for i := 0; i < iface.NumMethod(); i++ {
		names = append(names, iface.Method(i).Name)
	}
	sort.Strings(names)
	return names
}

// receiverTypeName reports the (possibly pointer) receiver type name of a
// method declaration, or "" when the declaration is not a method.
func receiverTypeName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) != 1 {
		return ""
	}
	expr := decl.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// calledHelpers returns the counting helpers a method body calls, by walking
// every call expression in it. A helper reached through another helper counts:
// countQueueCounter and the two scan helpers are themselves counting helpers,
// which is why they are named in countingHelpers rather than inlined.
func calledHelpers(body *ast.BlockStmt) []string {
	found := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		case *ast.Ident:
			name = fun.Name
		}
		for _, helper := range countingHelpers {
			if name == helper {
				found[helper] = true
			}
		}
		return true
	})
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestEveryUnitOfWorkMethodCountsWhatItReadOrWrote(t *testing.T) {
	// Control: the helper names this guard looks for are really declared in
	// this package, and each is really used somewhere. Without this a renamed
	// or deleted helper would leave the guard looking for a name that can never
	// appear, and every method would fail rather than passing vacuously -- but
	// a helper declared and never used would make an entry in the list dead
	// weight that hides how the charging actually happens.
	files := parseMemoryPackage(t)
	declared := map[string]bool{}
	used := map[string]bool{}
	methods := map[string]*ast.FuncDecl{}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			if receiverTypeName(fn) == unitReceiverType {
				declared[fn.Name.Name] = true
				if fn.Body != nil {
					methods[fn.Name.Name] = fn
					for _, helper := range calledHelpers(fn.Body) {
						used[helper] = true
					}
				}
			}
		}
	}
	for _, helper := range countingHelpers {
		if !declared[helper] {
			t.Fatalf("counting helper %q is not declared as a method on %s; this guard would look for a name that can never appear", helper, unitReceiverType)
		}
		if !used[helper] {
			t.Fatalf("counting helper %q is declared but never called; either it is dead or the AST walk is not finding calls", helper)
		}
	}
	// Control: a name that is not a counting helper must not be accepted as
	// one, or the walk above would match anything.
	for _, negative := range []string{"Requirement", "append", "countedKey", "sort"} {
		for _, helper := range countingHelpers {
			if negative == helper {
				t.Fatalf("known-negative control %q is in countingHelpers", negative)
			}
		}
	}

	port := portMethodNames(t)
	const wantMinPortMethods = 40
	if len(port) < wantMinPortMethods {
		t.Fatalf("application.UnitOfWork reported %d methods, want at least %d; the reflection walk is not seeing the embedded ports", len(port), wantMinPortMethods)
	}

	checked, exempted := 0, 0
	uncounted := make([]string, 0)
	for _, name := range port {
		if reason, ok := uncountedPortMethods[name]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Fatalf("allow-list entry %q carries no justification", name)
			}
			exempted++
			continue
		}
		fn, ok := methods[name]
		if !ok {
			t.Fatalf("port method %q has no method on %s in this package's source; either the adapter no longer implements the port or the AST walk is broken", name, unitReceiverType)
		}
		if len(calledHelpers(fn.Body)) == 0 {
			uncounted = append(uncounted, name)
			continue
		}
		checked++
	}
	if len(uncounted) != 0 {
		t.Fatalf("these application.UnitOfWork methods charge no record read and no record write: %v. An uncounted read is a silent discount, not a visible failure: the end-of-transaction true-up over-credits and this adapter charges less than internal/store/firestore for the same work. Add the counting call that mirrors the Firestore path, or add an allow-list entry with a justification for why the method touches no record", uncounted)
	}
	if checked == 0 {
		t.Fatal("checked zero port methods; the guard is vacuous")
	}
	t.Logf("port methods=%d instrumented=%d allow-listed=%d counting-helpers=%v", len(port), checked, exempted, countingHelpers)
}
