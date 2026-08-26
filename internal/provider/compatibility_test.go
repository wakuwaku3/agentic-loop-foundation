package provider_test

// Provider version compatibility: the two relations, their totality, their
// authority and the pre-invocation refusal (V2-074 A3-A6).
//
// Determinism is acceptance here: there is no fixed sleep, no wall-clock timer
// and no goroutine in this file, every table enumerates its cross product
// rather than sampling it, and every instant this file needs comes from the
// package's single injected base() value.
//
// SCOPE, stated before the assertions so nothing below can be read as more
// than it is. Everything here is a property of a DECLARATION and of code that
// reads it. Whether a declared interval is TRUE of a real Provider CLI is
// established by nothing in this repository: no Provider CLI was started by
// this task, and V2-028 owns the live half.

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/provider"
)

// ---------------------------------------------------------------------------
// A3: the two relations are declared, closed and total
// ---------------------------------------------------------------------------

// TestBothDeclaredIntervalsAreHalfOpenAndNonEmpty is A3's first clause. An
// empty interval would be a declaration that says nothing while looking like a
// claim.
func TestBothDeclaredIntervalsAreHalfOpenAndNonEmpty(t *testing.T) {
	checked := 0
	for _, name := range provider.PoolNames() {
		interval, err := provider.SupportedCLIVersions(name)
		if err != nil {
			t.Fatalf("%s: SupportedCLIVersions: %v", name, err)
		}
		if err := interval.Validate(); err != nil {
			t.Fatalf("%s: R1 interval %#v is not half-open and non-empty: %v", name, interval, err)
		}
		// Half-open in the only way that is observable: the lower bound is
		// inside and the upper bound is not.
		if inside, known := interval.Contains(interval.From); !known || !inside {
			t.Fatalf("%s: the lower bound %q is not inside its own interval (known=%v)", name, interval.From, known)
		}
		if inside, known := interval.Contains(interval.Until); !known || inside {
			t.Fatalf("%s: the upper bound %q is inside its own interval; the interval must be half-open", name, interval.Until)
		}
		checked++
		t.Logf("R1 %s: [%s, %s)", name, interval.From, interval.Until)
	}
	if checked != len(provider.PoolNames()) {
		t.Fatalf("checked %d adapters, want %d", checked, len(provider.PoolNames()))
	}

	loop, err := provider.SupportedLoopVersions(provider.ContractVersion)
	if err != nil {
		t.Fatalf("SupportedLoopVersions(%q): %v", provider.ContractVersion, err)
	}
	if err := loop.Validate(); err != nil {
		t.Fatalf("R2 interval %#v is not half-open and non-empty: %v", loop, err)
	}
	if inside, known := loop.Contains(loop.From); !known || !inside {
		t.Fatalf("R2: the lower bound %q is not inside its own interval", loop.From)
	}
	if inside, known := loop.Contains(loop.Until); !known || inside {
		t.Fatalf("R2: the upper bound %q is inside its own interval", loop.Until)
	}
	t.Logf("R2 contract %q: [%s, %s)", provider.ContractVersion, loop.From, loop.Until)

	// An adapter name outside the closed set has no interval, and a contract
	// identity we declare nothing for has none either. Neither is a verdict.
	if _, err := provider.SupportedCLIVersions("gemini"); !errors.Is(err, provider.ErrUnknownAdapter) {
		t.Fatalf("SupportedCLIVersions(gemini) err = %v, want ErrUnknownAdapter", err)
	}
	if _, err := provider.SupportedLoopVersions("v2"); !errors.Is(err, provider.ErrUnknownContract) {
		t.Fatalf("SupportedLoopVersions(v2) err = %v, want ErrUnknownContract", err)
	}
	// And the refusal of an interval that is not half-open is real, not
	// theoretical: an inverted and an equal-bound interval are both refused.
	for _, bad := range []provider.VersionInterval{
		{From: "2.0.0", Until: "1.0.0"},
		{From: "1.2.3", Until: "1.2.3"},
		{From: "", Until: "1.0.0"},
		{From: "1.0.0", Until: "not-a-version"},
	} {
		if err := bad.Validate(); !errors.Is(err, provider.ErrInvalidVersionInterval) {
			t.Fatalf("Validate(%#v) = %v, want ErrInvalidVersionInterval", bad, err)
		}
	}
}

// TestEveryFixtureEntrysDeclaredVersionLiesInsideItsAdaptersInterval is A3's
// second clause. The measured value is read from the manifest on disk -- the
// same file the existing provenance test reads -- and never from a second copy
// in this file, so the two cannot drift.
func TestEveryFixtureEntrysDeclaredVersionLiesInsideItsAdaptersInterval(t *testing.T) {
	manifest := readManifest(t)
	perProvider := map[string]int{}
	for _, entry := range manifest.Entries {
		interval, err := provider.SupportedCLIVersions(entry.Provider)
		if err != nil {
			t.Fatalf("%s: %v", entry.File, err)
		}
		inside, known := interval.Contains(entry.CLIVersionDeclared)
		if !known {
			t.Fatalf("%s: cli_version_declared %q is not a comparable version", entry.File, entry.CLIVersionDeclared)
		}
		if !inside {
			t.Fatalf("%s: cli_version_declared %q is outside %s's declared interval [%s, %s)", entry.File, entry.CLIVersionDeclared, entry.Provider, interval.From, interval.Until)
		}
		if verdict, err := provider.CLICompatibility(entry.Provider, entry.CLIVersionDeclared); err != nil || verdict != provider.VerdictCompatible {
			t.Fatalf("%s: CLICompatibility = %q err=%v, want compatible", entry.File, verdict, err)
		}
		perProvider[entry.Provider]++
	}
	if len(perProvider) != len(provider.PoolNames()) {
		t.Fatalf("the manifest covers %d providers, want %d", len(perProvider), len(provider.PoolNames()))
	}
	for _, name := range provider.PoolNames() {
		if perProvider[name] == 0 {
			t.Fatalf("%s has no manifest entry; this assertion would pass vacuously for it", name)
		}
	}
	t.Logf("checked %d manifest entries across %d adapters; every declared version lies inside its adapter's declared interval", len(manifest.Entries), len(perProvider))
}

// versionCase is one row of A3's totality table. Every row names which of the
// seven declared positions it occupies.
type versionCase struct {
	position string
	value    func(provider.VersionInterval) string
	want     provider.CompatibilityVerdict
}

// versionTotalityTable is the seven positions A3 names, in a fixed order:
// below the interval, at the lower bound, inside, at the upper bound, above
// the interval, an empty version, and a version that does not match the
// declared semver shape. The last two are unknown; the rest have a verdict.
var versionTotalityTable = []versionCase{
	{position: "below-the-interval", value: func(i provider.VersionInterval) string { return decrementedBelow(i.From) }, want: provider.VerdictIncompatible},
	{position: "at-the-lower-bound", value: func(i provider.VersionInterval) string { return i.From }, want: provider.VerdictCompatible},
	{position: "inside", value: func(i provider.VersionInterval) string { return insideOf(i) }, want: provider.VerdictCompatible},
	{position: "at-the-upper-bound", value: func(i provider.VersionInterval) string { return i.Until }, want: provider.VerdictIncompatible},
	{position: "above-the-interval", value: func(i provider.VersionInterval) string { return incrementedAbove(i.Until) }, want: provider.VerdictIncompatible},
	{position: "empty", value: func(provider.VersionInterval) string { return "" }, want: provider.VerdictUnknown},
	{position: "malformed", value: func(provider.VersionInterval) string { return "v1.2" }, want: provider.VerdictUnknown},
}

// decrementedBelow returns a version strictly below the given one. Every lower
// bound this task declares has a non-zero minor, so decrementing the minor is
// enough and needs no borrow across the major.
func decrementedBelow(version string) string {
	parts := strings.Split(version, ".")
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor == 0 {
		return "0.0.0"
	}
	return parts[0] + "." + strconv.Itoa(minor-1) + ".0"
}

func incrementedAbove(version string) string {
	parts := strings.Split(version, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "999.0.0"
	}
	return strconv.Itoa(major+1) + ".0.0"
}

// insideOf returns a version strictly between the bounds. Every interval this
// task declares spans at least one whole patch or minor step above its lower
// bound, so the lower bound's patch plus one is inside; the assertion below
// checks that rather than assuming it.
func insideOf(i provider.VersionInterval) string {
	parts := strings.Split(i.From, ".")
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return i.From
	}
	candidate := parts[0] + "." + parts[1] + "." + strconv.Itoa(patch+1)
	if inside, known := i.Contains(candidate); known && inside {
		return candidate
	}
	return i.From
}

// TestCompatibilityIsTotalOverTheDeclaredVersionPositions is A3's third
// clause, enumerated over the full cross product of the seven positions and
// every declared interval -- three R1 intervals plus the R2 interval.
func TestCompatibilityIsTotalOverTheDeclaredVersionPositions(t *testing.T) {
	type target struct {
		label    string
		interval provider.VersionInterval
		verdict  func(string) (provider.CompatibilityVerdict, error)
	}
	targets := []target{}
	for _, name := range provider.PoolNames() {
		interval, err := provider.SupportedCLIVersions(name)
		if err != nil {
			t.Fatal(err)
		}
		adapter := name
		targets = append(targets, target{
			label:    "R1/" + adapter,
			interval: interval,
			verdict:  func(v string) (provider.CompatibilityVerdict, error) { return provider.CLICompatibility(adapter, v) },
		})
	}
	loop, err := provider.SupportedLoopVersions(provider.ContractVersion)
	if err != nil {
		t.Fatal(err)
	}
	targets = append(targets, target{
		label:    "R2/" + provider.ContractVersion,
		interval: loop,
		verdict: func(v string) (provider.CompatibilityVerdict, error) {
			return provider.LoopCompatibility(provider.ContractVersion, v)
		},
	})

	cells := 0
	verdictsSeen := map[provider.CompatibilityVerdict]bool{}
	for _, tgt := range targets {
		for _, row := range versionTotalityTable {
			value := row.value(tgt.interval)
			// The generated value must actually occupy the position it claims,
			// or the row would assert the wrong thing quietly.
			switch row.position {
			case "inside":
				if value == tgt.interval.From || value == tgt.interval.Until {
					t.Fatalf("%s: the inside probe %q collapsed onto a bound; the table would not cover the inside position", tgt.label, value)
				}
			case "below-the-interval", "above-the-interval":
				if value == tgt.interval.From || value == tgt.interval.Until {
					t.Fatalf("%s: the %s probe %q collapsed onto a bound", tgt.label, row.position, value)
				}
			}
			verdict, err := tgt.verdict(value)
			if err != nil {
				t.Fatalf("%s/%s(%q): %v", tgt.label, row.position, value, err)
			}
			if verdict != row.want {
				t.Fatalf("%s/%s(%q): verdict = %q, want %q (interval [%s, %s))", tgt.label, row.position, value, verdict, row.want, tgt.interval.From, tgt.interval.Until)
			}
			verdictsSeen[verdict] = true
			cells++
		}
	}
	if want := len(targets) * len(versionTotalityTable); cells != want {
		t.Fatalf("drove %d cells, want %d", cells, want)
	}
	// Every member of the closed verdict set is actually produced by the
	// table, so no member is declared and unreachable.
	for _, verdict := range provider.CompatibilityVerdicts() {
		if !verdictsSeen[verdict] {
			t.Fatalf("the totality table never produces the declared verdict %q", verdict)
		}
	}
	t.Logf("%d intervals x %d positions = %d cells; verdicts produced = %d of %d declared", len(targets), len(versionTotalityTable), cells, len(verdictsSeen), len(provider.CompatibilityVerdicts()))
}

// TestTheConjunctionIsUnknownWheneverEitherInputIsAbsent is A3's "verdict is
// the conjunction, unknown whenever either input is absent" clause, enumerated
// over the full 3 x 3 product of the closed verdict set.
func TestTheConjunctionIsUnknownWheneverEitherInputIsAbsent(t *testing.T) {
	want := map[provider.CompatibilityVerdict]map[provider.CompatibilityVerdict]provider.CompatibilityVerdict{
		provider.VerdictCompatible: {
			provider.VerdictCompatible:   provider.VerdictCompatible,
			provider.VerdictIncompatible: provider.VerdictIncompatible,
			provider.VerdictUnknown:      provider.VerdictUnknown,
		},
		provider.VerdictIncompatible: {
			provider.VerdictCompatible:   provider.VerdictIncompatible,
			provider.VerdictIncompatible: provider.VerdictIncompatible,
			provider.VerdictUnknown:      provider.VerdictIncompatible,
		},
		provider.VerdictUnknown: {
			provider.VerdictCompatible:   provider.VerdictUnknown,
			provider.VerdictIncompatible: provider.VerdictIncompatible,
			provider.VerdictUnknown:      provider.VerdictUnknown,
		},
	}
	cells := 0
	for _, first := range provider.CompatibilityVerdicts() {
		for _, second := range provider.CompatibilityVerdicts() {
			got := provider.ConjoinVerdicts(first, second)
			if got != want[first][second] {
				t.Fatalf("ConjoinVerdicts(%q, %q) = %q, want %q", first, second, got, want[first][second])
			}
			cells++
		}
	}
	if want := len(provider.CompatibilityVerdicts()) * len(provider.CompatibilityVerdicts()); cells != want {
		t.Fatalf("drove %d conjunction cells, want %d", cells, want)
	}

	// And through the whole-relation function: an absent Loop version alone
	// makes the conjunction unknown even when the CLI version is inside its
	// interval, and an absent CLI version alone does the same.
	for _, name := range provider.PoolNames() {
		interval, err := provider.SupportedCLIVersions(name)
		if err != nil {
			t.Fatal(err)
		}
		loop, err := provider.SupportedLoopVersions(provider.ContractVersion)
		if err != nil {
			t.Fatal(err)
		}
		inside := insideOf(interval)
		loopInside := insideOf(loop)
		both, err := provider.Compatibility(name, provider.ContractVersion, inside, loopInside)
		if err != nil || both != provider.VerdictCompatible {
			t.Fatalf("%s: both inside -> %q err=%v, want compatible", name, both, err)
		}
		noLoop, err := provider.Compatibility(name, provider.ContractVersion, inside, "")
		if err != nil || noLoop != provider.VerdictUnknown {
			t.Fatalf("%s: absent loop version -> %q err=%v, want unknown", name, noLoop, err)
		}
		noCLI, err := provider.Compatibility(name, provider.ContractVersion, "", loopInside)
		if err != nil || noCLI != provider.VerdictUnknown {
			t.Fatalf("%s: absent cli version -> %q err=%v, want unknown", name, noCLI, err)
		}
		outside, err := provider.Compatibility(name, provider.ContractVersion, interval.Until, "")
		if err != nil || outside != provider.VerdictIncompatible {
			t.Fatalf("%s: measured outside with an absent loop version -> %q err=%v; incompatible must not be diluted to unknown by the absence of the other input", name, outside, err)
		}
	}
}

// TestNoVerdictEnumSpellsCompatibleWithoutUnknownBesideIt is A3's last clause,
// asserted by PARSING the declaration rather than by reading it: the
// CompatibilityVerdict constants are read out of this package's own AST and
// compared against the declared list.
func TestNoVerdictEnumSpellsCompatibleWithoutUnknownBesideIt(t *testing.T) {
	declared := constantsOfDeclaredType(t, "CompatibilityVerdict")
	if len(declared) == 0 {
		t.Fatal("the AST scan found zero CompatibilityVerdict constants")
	}
	listed := map[string]bool{}
	for _, verdict := range provider.CompatibilityVerdicts() {
		listed[string(verdict)] = true
	}
	for value := range declared {
		if !listed[value] {
			t.Fatalf("CompatibilityVerdict %q is declared in the package but is not in CompatibilityVerdicts()", value)
		}
	}
	for value := range listed {
		if !declared[value] {
			t.Fatalf("CompatibilityVerdicts() names %q, which the package does not declare as a constant", value)
		}
	}
	if !declared["compatible"] {
		t.Fatal("the package declares no verdict spelled compatible; this guard is vacuous")
	}
	if !declared["unknown"] {
		t.Fatal("the verdict set spells compatible without unknown being a sibling member of the same closed set")
	}
	sortedNames := make([]string, 0, len(declared))
	for value := range declared {
		sortedNames = append(sortedNames, value)
	}
	sort.Strings(sortedNames)
	t.Logf("CompatibilityVerdict constants read from the AST = %v", sortedNames)
}

// constantsOfDeclaredType reads the string-literal values of every constant of
// one named type out of this package's non-test files, following the idiom
// TestFailureClassSetIsExactlyWhatTheASTDeclares already uses.
func constantsOfDeclaredType(t *testing.T, typeName string) map[string]bool {
	t.Helper()
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
				if currentType != typeName {
					continue
				}
				for _, v := range value.Values {
					lit, isLit := v.(*ast.BasicLit)
					if !isLit || lit.Kind != token.STRING {
						t.Fatalf("%s: a %s constant is not a string literal; the AST scan cannot enumerate it", f.path, typeName)
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
	return declared
}

// ---------------------------------------------------------------------------
// A4: authority, asserted rather than asserted-in-prose
// ---------------------------------------------------------------------------

// TestTheDeclaredIntervalIsNeverAssignedFromParsedFixtureBytes is A4's first
// clause. Three facts together: no non-test file in this package can reach the
// fixture manifest at all (none of them names the fixture directory, the
// manifest file, or a filesystem read), no non-test file names any of the parse
// entry points inside a function that also produces a VersionInterval, and the
// declared intervals are composite literals of string constants rather than
// values derived from anything. The matcher is verified against a synthetic
// known-positive first.
//
// The manifest's own declared-version field is deliberately NOT scanned for by
// name: this task adds Request.CLIVersionDeclared, whose json tag is that same
// spelling on purpose, because it is the same fact travelling the other way --
// a measured value the interval is asserted to CONTAIN. What must be
// unreachable is the FILE, and that is what is asserted.
func TestTheDeclaredIntervalIsNeverAssignedFromParsedFixtureBytes(t *testing.T) {
	// Anything a non-test file in this package would have to name to read the
	// manifest from disk: its directory, a read primitive, or an embed. The word
	// "manifest" itself is deliberately not on this list -- prose may name what
	// the code may not reach.
	manifestReach := []string{"testdata", "os.ReadFile", "os.Open", "ioutil", "embed", ".json"}
	parseEntryPoints := map[string]bool{
		"parseFixture": true, "Parse": true, "ParseOrClassify": true, "Unmarshal": true, "Decode": true, "NewDecoder": true,
	}

	files := nonTestGuardFiles(t)
	scanned, functions := 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s: %v", f.path, err)
		}
		scanned++
		for _, reach := range manifestReach {
			if strings.Contains(string(raw), reach) {
				t.Fatalf("%s names %q; no non-test file in this package may reach the fixture manifest, because the declared interval is read from source and from nowhere else", f.path, reach)
			}
		}
		for _, decl := range f.file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc {
				continue
			}
			functions++
			producesInterval, parses := false, []string{}
			ast.Inspect(fn, func(n ast.Node) bool {
				if ident, isIdent := n.(*ast.Ident); isIdent {
					if ident.Name == "VersionInterval" {
						producesInterval = true
					}
					if parseEntryPoints[ident.Name] {
						parses = append(parses, ident.Name)
					}
				}
				if sel, isSel := n.(*ast.SelectorExpr); isSel && parseEntryPoints[sel.Sel.Name] {
					parses = append(parses, sel.Sel.Name)
				}
				return true
			})
			if producesInterval && len(parses) != 0 {
				t.Fatalf("%s: %s both produces a VersionInterval and names a parse entry point %v; a supported interval may never be assigned from parsed bytes", f.path, fn.Name.Name, parses)
			}
		}
	}
	if scanned == 0 || functions == 0 {
		t.Fatalf("scanned files=%d functions=%d; the walk is broken", scanned, functions)
	}

	// The declared intervals are composite literals of string constants. Read
	// back through the exported accessors, every bound is a value the source
	// spells out, which is the only sense in which "declared in source" is
	// checkable from outside the package.
	for _, name := range provider.PoolNames() {
		interval, err := provider.SupportedCLIVersions(name)
		if err != nil {
			t.Fatal(err)
		}
		if !declaredIntervalBoundIsASourceLiteral(t, interval.From) || !declaredIntervalBoundIsASourceLiteral(t, interval.Until) {
			t.Fatalf("%s: interval [%s, %s) has a bound that appears as no string literal in this package's non-test source", name, interval.From, interval.Until)
		}
	}
	loopInterval, err := provider.SupportedLoopVersions(provider.ContractVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !declaredIntervalBoundIsASourceLiteral(t, loopInterval.From) || !declaredIntervalBoundIsASourceLiteral(t, loopInterval.Until) {
		t.Fatalf("R2 interval [%s, %s) has a bound that appears as no string literal in this package's non-test source", loopInterval.From, loopInterval.Until)
	}

	// Positive control: the scan must actually catch the shape it exists for.
	synthetic := "package provider\n\nfunc bad(b []byte) VersionInterval {\n\tvar f fixture\n\t_ = json.Unmarshal(b, &f)\n\treturn VersionInterval{}\n}\n"
	file := parseSyntheticProviderFile(t, synthetic)
	caught := false
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc {
			continue
		}
		producesInterval, parses := false, 0
		ast.Inspect(fn, func(n ast.Node) bool {
			if ident, isIdent := n.(*ast.Ident); isIdent && ident.Name == "VersionInterval" {
				producesInterval = true
			}
			if sel, isSel := n.(*ast.SelectorExpr); isSel && parseEntryPoints[sel.Sel.Name] {
				parses++
			}
			return true
		})
		if producesInterval && parses != 0 {
			caught = true
		}
	}
	if !caught {
		t.Fatal("positive control: a synthetic function assigning an interval from parsed bytes was not flagged")
	}
	t.Logf("scanned non-test files=%d functions=%d; no interval is assigned from parsed bytes, no non-test file can reach the fixture manifest, and every declared bound is a source literal", scanned, functions)
}

// declaredIntervalBoundIsASourceLiteral reports whether value appears as a
// string literal in this package's own non-test AST.
func declaredIntervalBoundIsASourceLiteral(t *testing.T, value string) bool {
	t.Helper()
	for _, f := range nonTestGuardFiles(t) {
		found := false
		ast.Inspect(f.file, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				return true
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err == nil && unquoted == value {
				found = true
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// parseSyntheticProviderFile parses an in-memory source file so a scan's
// positive control is a real AST rather than a hand-built one.
func parseSyntheticProviderFile(t *testing.T, source string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", source, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse synthetic control: %v", err)
	}
	return file
}

// TestTheAuthorityTableIsRecordedVerbatimInTheDocumentation is A4's last
// clause. The four rows of dp-v2-074 d2 must appear in the operations document
// an owner reads, not only in this task's evidence, and the row that says only
// a live exercise can establish that a declared interval is true must be one
// of them.
func TestTheAuthorityTableIsRecordedVerbatimInTheDocumentation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "operations", "provider-registry.md"))
	if err != nil {
		t.Fatalf("read provider-registry.md: %v", err)
	}
	text := string(raw)
	for _, phrase := range []string{
		"the source-declared interval in `internal/provider`",
		"the Loop's own release identity read through the existing `ReleaseObserver`",
		"the owner-approved, digest-bound provider-preflight record",
		"nothing in this repository; only a live exercise, and V2-028 owns it",
	} {
		if !strings.Contains(text, phrase) {
			t.Fatalf("docs/operations/provider-registry.md does not carry the authority row %q", phrase)
		}
	}
	if !strings.Contains(text, "V2-028") {
		t.Fatal("the document does not attribute the live half to V2-028")
	}
	t.Log("all four authority rows, including the row that attributes the truth of a declared interval to a live exercise only, are present in docs/operations/provider-registry.md")
}

// ---------------------------------------------------------------------------
// A5: incompatibility opens the EXISTING circuit and nothing new
// ---------------------------------------------------------------------------

// TestTheThreeCLIVersionIncompatibleFixturesOpenTheExistingContractCircuit is
// A5, driven through ParseOrClassify for all three adapters, through the
// existing opening table and through the existing breaker. Nothing about the
// breaker, the opening table or the FailureClass set is changed by this task,
// and this test asserts the behaviour it relies on rather than assuming it.
func TestTheThreeCLIVersionIncompatibleFixturesOpenTheExistingContractCircuit(t *testing.T) {
	adapters := allAdapters()
	for _, a := range adapters {
		raw := readFixture(t, a.Name(), "cli-version-incompatible")
		_, failure := provider.ParseOrClassify(a, raw)
		if failure.Class != provider.FailureContract {
			t.Fatalf("%s: cli-version-incompatible produced class %q, want contract-incompatible", a.Name(), failure.Class)
		}
		action, err := provider.ActionForFailureClass(failure.Class)
		if err != nil || action != provider.ActionOpenImmediately {
			t.Fatalf("%s: the opening table's action for %q is %q err=%v, want open-immediately", a.Name(), failure.Class, action, err)
		}

		pool := newPool(t)
		breaker := newBreaker(t)
		window := newWindow(t, a.Name(), 5, 100)
		observedVersion := "0.0.1"
		observation, err := provider.ApplyObservation(pool, breaker, window, provider.Observed{
			Provider:           a.Name(),
			Window:             "rolling-hour",
			DeclaredCLIVersion: observedVersion,
			Failure:            failure,
		}, base())
		if err != nil {
			t.Fatalf("%s: ApplyObservation: %v", a.Name(), err)
		}
		if observation.Action != provider.ActionOpenImmediately || observation.State != provider.CircuitNotSending {
			t.Fatalf("%s: observation = %#v, want open-immediately and not-sending", a.Name(), observation)
		}
		report, err := breaker.Report(a.Name())
		if err != nil {
			t.Fatalf("%s: Report: %v", a.Name(), err)
		}
		if report.State != provider.CircuitNotSending {
			t.Fatalf("%s: circuit state = %q, want not-sending", a.Name(), report.State)
		}
		carriesClass := false
		for _, class := range report.Because {
			if class == provider.FailureContract {
				carriesClass = true
			}
		}
		if !carriesClass {
			t.Fatalf("%s: the report's stated reason %v does not carry contract-incompatible", a.Name(), report.Because)
		}
		if report.ObservationScope != provider.ObservationScope {
			t.Fatalf("%s: the report carries no observation scope", a.Name())
		}

		// Probe refuses however much time passes. The wait is driven entirely
		// by the caller's time argument, so "however much" is enumerated
		// rather than slept through.
		for _, elapsed := range []int{0, 1, 24, 24 * 365, 24 * 365 * 100} {
			at := base().Add(time.Duration(elapsed) * time.Hour)
			if _, err := breaker.Probe(pool, window, a.Name(), at); !errors.Is(err, provider.ErrProbeNotEligible) {
				t.Fatalf("%s: Probe after %d hours = %v, want ErrProbeNotEligible; a contract-incompatible open is never probe-eligible", a.Name(), elapsed, err)
			}
		}

		// ObserveDeclaredCLIVersion closes it only for a DIFFERENT version.
		closed, err := breaker.ObserveDeclaredCLIVersion(pool, a.Name(), "")
		if err != nil || closed {
			t.Fatalf("%s: an empty observed version closed the circuit (closed=%v err=%v)", a.Name(), closed, err)
		}
		closed, err = breaker.ObserveDeclaredCLIVersion(pool, a.Name(), observedVersion)
		if err != nil || closed {
			t.Fatalf("%s: the same version closed the circuit (closed=%v err=%v)", a.Name(), closed, err)
		}
		closed, err = breaker.ObserveDeclaredCLIVersion(pool, a.Name(), "0.0.2")
		if err != nil || !closed {
			t.Fatalf("%s: a different version did not close the circuit (closed=%v err=%v)", a.Name(), closed, err)
		}
		after, err := breaker.Report(a.Name())
		if err != nil || after.State != provider.CircuitSending {
			t.Fatalf("%s: after an observed version change the circuit is %q err=%v, want sending", a.Name(), after.State, err)
		}
	}
	// No FailureClass was added: the declared set is exactly the nine the
	// opening table already covers.
	if len(provider.FailureClasses()) != len(provider.OpeningTableClasses()) {
		t.Fatalf("FailureClasses()=%d OpeningTableClasses()=%d", len(provider.FailureClasses()), len(provider.OpeningTableClasses()))
	}
	t.Logf("all three adapters: contract-incompatible -> open-immediately -> not-sending, never probe-eligible, closed only by an observed version change. %d declared failure classes, all tabled", len(provider.FailureClasses()))
}

// ---------------------------------------------------------------------------
// A6: the pre-invocation refusal, fail-closed on measured and fail-open on
// unknown
// ---------------------------------------------------------------------------

// TestTheSharedBuildHelperRefusesAMeasuredIncompatibleCLIVersion is A6. The
// refusal happens before any Invocation exists, so the incompatibility costs no
// invocation at all.
//
// PRODUCTION IS NOT ARMED. No production caller supplies
// Request.CLIVersionDeclared: internal/runner is deliberately not edited by
// this task, so the refusal below is exercised by tests and not by production.
// A reader who takes this test as proof that an incompatible CLI cannot be
// invoked would be wrong.
func TestTheSharedBuildHelperRefusesAMeasuredIncompatibleCLIVersion(t *testing.T) {
	refusals, acceptances := 0, 0
	for _, a := range allAdapters() {
		interval, err := provider.SupportedCLIVersions(a.Name())
		if err != nil {
			t.Fatal(err)
		}
		// argv[0] is the adapter's own name for all three adapters, which is
		// what makes the interval consulted inside build the interval of the
		// adapter being built. Asserted, not assumed.
		inv, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()})
		if err != nil {
			t.Fatalf("%s: build: %v", a.Name(), err)
		}
		if inv.Argv[0] != a.Name() {
			t.Fatalf("%s: argv[0] = %q, which is not the adapter name; the shared build helper's interval lookup would consult the wrong adapter", a.Name(), inv.Argv[0])
		}

		for _, bad := range []string{decrementedBelow(interval.From), interval.Until, incrementedAbove(interval.Until)} {
			out, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet(), CLIVersionDeclared: bad})
			if !errors.Is(err, provider.ErrInvalidRequest) {
				t.Fatalf("%s: Build with a measured-incompatible version %q returned err=%v, want ErrInvalidRequest", a.Name(), bad, err)
			}
			if len(out.Argv) != 0 || len(out.Stdin) != 0 || out.WorkingDirectory != "" {
				t.Fatalf("%s: a refused Build returned an Invocation %#v; no invocation may be produced", a.Name(), out)
			}
			refusals++
		}
		// Fail open on unknown: empty, and malformed.
		for _, unknown := range []string{"", "v2", "not-a-version", "2"} {
			out, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet(), CLIVersionDeclared: unknown})
			if err != nil {
				t.Fatalf("%s: Build refused an unknown version %q: %v; unknown must never be rounded to incompatible", a.Name(), unknown, err)
			}
			if len(out.Argv) == 0 {
				t.Fatalf("%s: Build produced no argv for an unknown version %q", a.Name(), unknown)
			}
			acceptances++
		}
		// And no refusal inside the interval.
		for _, good := range []string{interval.From, insideOf(interval)} {
			out, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet(), CLIVersionDeclared: good})
			if err != nil {
				t.Fatalf("%s: Build refused an in-interval version %q: %v", a.Name(), good, err)
			}
			if len(out.Argv) == 0 {
				t.Fatalf("%s: Build produced no argv for %q", a.Name(), good)
			}
			acceptances++
		}
	}
	if refusals != 3*len(allAdapters()) {
		t.Fatalf("refusals = %d, want %d", refusals, 3*len(allAdapters()))
	}
	t.Logf("refusals=%d acceptances=%d across %d adapters. The field is supplied by NO production caller: the refusal is exercised by tests and not by production", refusals, acceptances, len(allAdapters()))
}

// TestAddingTheOptionalFieldChangesNoExistingRequestOrInvocationVerdict is
// A6's "changes no existing assertion" half, stated as behaviour rather than
// as a claim about the diff.
func TestAddingTheOptionalFieldChangesNoExistingRequestOrInvocationVerdict(t *testing.T) {
	// Request.Validate's existing verdicts, one case per existing branch, with
	// the new field left empty AND set, so the field cannot have changed any
	// of them.
	cases := []struct {
		name    string
		request provider.Request
		refused bool
	}{
		{name: "valid", request: provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()}},
		{name: "no-operation-id", request: provider.Request{Workspace: "/workspace", Packet: packet()}, refused: true},
		{name: "no-workspace", request: provider.Request{OperationID: "op-1", Packet: packet()}, refused: true},
		{name: "invalid-packet", request: provider.Request{OperationID: "op-1", Workspace: "/workspace"}, refused: true},
		{name: "nul-in-workspace", request: provider.Request{OperationID: "op-1", Workspace: "/work\x00space", Packet: packet()}, refused: true},
		{name: "newline-in-workspace", request: provider.Request{OperationID: "op-1", Workspace: "/work\nspace", Packet: packet()}, refused: true},
		{name: "negative-timeout", request: provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet(), Timeout: -1}, refused: true},
	}
	for _, tc := range cases {
		for _, version := range []string{"", "2.1.5", "not-a-version"} {
			request := tc.request
			request.CLIVersionDeclared = version
			err := request.Validate()
			if (err != nil) != tc.refused {
				t.Fatalf("%s with CLIVersionDeclared=%q: Validate err = %v, want refused=%v", tc.name, version, err, tc.refused)
			}
		}
	}

	// The field is omitted from the marshalled Request when empty, so no
	// existing assertion about the marshalled shape gains a key.
	body, err := json.Marshal(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "cli_version") {
		t.Fatalf("an empty CLIVersionDeclared is emitted in the marshalled Request: %s", body)
	}

	// The Invocation an unchanged Request produces is byte-identical to the one
	// it produced before the field existed, which is asserted here against the
	// pinned argv surface the existing test declares.
	for _, a := range allAdapters() {
		inv, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet()})
		if err != nil {
			t.Fatal(err)
		}
		want := wantArgv[a.Name()]
		if len(inv.Argv) != len(want) {
			t.Fatalf("%s: argv = %v, want %v", a.Name(), inv.Argv, want)
		}
		for i := range want {
			if inv.Argv[i] != want[i] {
				t.Fatalf("%s: argv[%d] = %q, want %q", a.Name(), i, inv.Argv[i], want[i])
			}
		}
		// And the same Request with an in-interval version produces the SAME
		// argv: the field adds a refusal, never an argv element.
		interval, err := provider.SupportedCLIVersions(a.Name())
		if err != nil {
			t.Fatal(err)
		}
		withVersion, err := a.Build(provider.Request{OperationID: "op-1", Workspace: "/workspace", Packet: packet(), CLIVersionDeclared: interval.From})
		if err != nil {
			t.Fatal(err)
		}
		if len(withVersion.Argv) != len(inv.Argv) {
			t.Fatalf("%s: supplying a version changed the argv length", a.Name())
		}
		for i := range inv.Argv {
			if withVersion.Argv[i] != inv.Argv[i] {
				t.Fatalf("%s: supplying a version changed argv[%d] from %q to %q", a.Name(), i, inv.Argv[i], withVersion.Argv[i])
			}
		}
		if string(withVersion.Stdin) != string(inv.Stdin) {
			t.Fatalf("%s: supplying a version changed the stdin bytes", a.Name())
		}
	}
	t.Logf("Request.Validate: %d existing cases x 3 field values, every verdict unchanged; the argv and stdin of every adapter are unchanged", len(cases))
}
