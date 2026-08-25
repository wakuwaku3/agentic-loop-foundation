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
	"sort"
	"strconv"
	"strings"
	"testing"
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
