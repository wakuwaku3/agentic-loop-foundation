// Component dependency derivation and the three manifest checks it feeds
// (dp-v2-045 d8, d9).
//
// The import graph is derived from the AST, never from a regular expression
// and never from `go list`. A text scan reports two edges that do not exist,
// because internal/release/source_guard_test.go carries the literals
// .../internal/ci and .../internal/update as assertion data for the
// dp-v2-021 d12 import guard; a text scan therefore turns that guard against
// itself. `go list` is correct for imports but blind to build-tag-excluded
// files and to internal/api/live_local_test.go's exec.Command("go","build",
// ..., ".../cmd/control-plane"), which is a real compile-time dependency
// expressed as a string. It is also a go-tool invocation nested inside
// `go test`, where GOFLAGS, GOCACHE and the environment stripping of
// `devbox run --pure` all become variables the result depends on, so no test
// in this package invokes it; the cross-check is a one-off recorded in the
// evidence.
package ci

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ModulePath is this repository's Go module path. An import or a string
// literal is module-relative iff it equals this or begins with this + "/".
const ModulePath = "github.com/takushi/agentic-loop-foundation/v2"

// JustifiedEdge records a declared component edge that the derivation cannot
// see, together with the reason it is nonetheless real. Without this table
// VerifyDependencyCoverage would be trivially satisfiable by declaring that
// every component depends on every other one.
type JustifiedEdge struct {
	From   string
	To     string
	Reason string
}

// JustifiedEdges holds every declared edge that no derivation source
// produces. All three are contract-file reads: the depended-on component
// owns contract files this component parses at test time, which is already
// expressed in contract_dependencies but is not a Go import, not a test
// import and not a check-target relation.
var JustifiedEdges = []JustifiedEdge{
	{From: "internal-contracts", To: "contracts", Reason: "contract-file read: internal/contracts parses contracts/** as data, expressed in contract_dependencies, never imported"},
	{From: "task-ledger", To: "contracts", Reason: "contract-file read: the ledger validates .agents/** against contracts/schemas/*.json, expressed in contract_dependencies, never imported"},
	{From: "release", To: "contracts", Reason: "contract-file read: internal/release reads contracts/release-contract/** and contracts/schemas/release-contract.json, expressed in contract_dependencies, never imported"},
}

// LiteralFalsePositive records a module-path string literal that is assertion
// data rather than a dependency.
type LiteralFalsePositive struct {
	File      string
	Component string
	Reason    string
}

// LiteralFalsePositives holds every module-path literal that must not be read
// as an edge. Every entry is an import guard's own test data: a source guard
// names the packages its package is forbidden to import, so counting those
// names as dependencies would invert the very guard they belong to. An entry
// is only ever justified this way; a literal that is actually reached at run
// time belongs in verification_dependencies instead.
var LiteralFalsePositives = []LiteralFalsePositive{
	{File: "internal/release/source_guard_test.go", Component: "ci", Reason: "assertion data for the dp-v2-021 d12 import guard: the literal names the package internal/release must not import"},
	{File: "internal/release/source_guard_test.go", Component: "update", Reason: "assertion data for the dp-v2-021 d12 import guard: the literal names the package internal/release must not import"},
	{File: "internal/scheduler/source_guard_test.go", Component: "reconciler", Reason: "assertion data for the dp-v2-030 A9 import guard: the literal names a package internal/scheduler must not import, and the guard's own positive control asserts it is rejected"},
	{File: "internal/scheduler/source_guard_test.go", Component: "store-memory", Reason: "assertion data for the dp-v2-030 A9 import guard: the literal names a package internal/scheduler must not import, and the guard's own positive control asserts it is rejected"},
}

// FileImports is everything the AST extractor reads out of one Go file:
// the module-relative package directories it imports (from f.Imports only),
// and the module-relative directories named by string literals that are not
// import specs. Build constraints are not applied, so an import inside a
// build-tag-excluded file is still reported.
type FileImports struct {
	Path     string
	Test     bool
	Imports  []string
	Literals []string
}

// moduleRelative maps a module-path import or literal to a repository
// directory, or returns "" when the string is not module-relative.
func moduleRelative(s string) string {
	if s == ModulePath {
		return "."
	}
	if !strings.HasPrefix(s, ModulePath+"/") {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(s, ModulePath+"/"), "/")
}

// ParseGoFileImports parses one tracked Go file. rel is repository-relative.
func ParseGoFileImports(root, rel string) (FileImports, error) {
	out := FileImports{Path: rel, Test: strings.HasSuffix(rel, "_test.go")}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
	if err != nil {
		return out, fmt.Errorf("parse %s: %w", rel, err)
	}
	importLits := map[*ast.BasicLit]bool{}
	seenImport := map[string]bool{}
	for _, spec := range f.Imports {
		if spec.Path == nil {
			continue
		}
		importLits[spec.Path] = true
		v, e := strconv.Unquote(spec.Path.Value)
		if e != nil {
			continue
		}
		if d := moduleRelative(v); d != "" && !seenImport[d] {
			seenImport[d] = true
			out.Imports = append(out.Imports, d)
		}
	}
	seenLit := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || importLits[lit] {
			return true
		}
		v, e := strconv.Unquote(lit.Value)
		if e != nil {
			return true
		}
		if d := moduleRelative(v); d != "" && !seenLit[d] {
			seenLit[d] = true
			out.Literals = append(out.Literals, d)
		}
		return true
	})
	sort.Strings(out.Imports)
	sort.Strings(out.Literals)
	return out, nil
}

// LiteralRef is one module-path string literal occurrence, resolved to the
// component that owns the directory it names.
type LiteralRef struct {
	File      string
	Owner     string
	Directory string
	Component string
}

// DerivedGraph is the measured dependency surface. Every map is keyed by
// component id and every value is sorted, so no caller ever iterates a map
// to produce output.
type DerivedGraph struct {
	// NonTestImports is source for the dependencies field: cross-component
	// imports from files whose name does not end in _test.go.
	NonTestImports map[string][]string
	// TestImports is d7 source (a).
	TestImports map[string][]string
	// LiteralEdges is d7 source (b), with LiteralFalsePositives removed.
	LiteralEdges map[string][]string
	// CheckEdges is d7 source (c).
	CheckEdges map[string][]string
	// Literals is every module-path literal found, false positives included,
	// so VerifyNoUnjustifiedEdges can audit the table itself.
	Literals []LiteralRef
	// CheckPackages and CheckScripts record what each check target executes.
	CheckPackages map[string][]string
	CheckScripts  map[string][]string
}

func addEdge(m map[string][]string, from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	for _, x := range m[from] {
		if x == to {
			return
		}
	}
	m[from] = append(m[from], to)
}

func sortEdges(m map[string][]string) {
	for k := range m {
		sort.Strings(m[k])
	}
}

// ownerOf returns the single component that owns path, or "".
func ownerOf(m Manifest, p string) string {
	o := owner(m, p)
	if len(o) != 1 {
		return ""
	}
	return o[0]
}

// directoryOwners maps every repository directory that holds at least one
// tracked Go file to the component that owns those files, using the
// manifest's own match() so ownership can never disagree with make
// ownership. Only Go directories are mapped, because this map exists to
// resolve import paths; the repository root holds non-Go files split between
// the tooling and environment components and is not a Go package.
func directoryOwners(m Manifest, files []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		o := ownerOf(m, f)
		if o == "" {
			continue
		}
		d := path.Dir(filepath.ToSlash(f))
		if prev, ok := out[d]; ok && prev != o {
			return nil, fmt.Errorf("directory %s has conflicting owners %s and %s", d, prev, o)
		}
		out[d] = o
	}
	return out, nil
}

var (
	makePackageRe = regexp.MustCompile(`(?:^|[\s;=(])\./([A-Za-z0-9._/-]+)`)
	makeScriptRe  = regexp.MustCompile(`scripts/[A-Za-z0-9._-]+\.sh`)
)

// makeRecipes returns, for each target defined in the Makefile, the text of
// its recipe lines.
func makeRecipes(root string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	current := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "\t") {
			if current != "" {
				out[current] += line + "\n"
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if i := strings.Index(trimmed, ":"); i > 0 && !strings.HasPrefix(trimmed, ".PHONY") {
			name := strings.TrimSpace(trimmed[:i])
			if name != "" && !strings.ContainsAny(name, " \t") && !strings.Contains(name, "=") {
				current = name
				if _, ok := out[current]; !ok {
					out[current] = ""
				}
				continue
			}
		}
		current = ""
	}
	return out, nil
}

// DeriveGraph builds the derived dependency surface from the given tracked
// file list.
func DeriveGraph(root string, m Manifest, files []string) (DerivedGraph, error) {
	g := DerivedGraph{
		NonTestImports: map[string][]string{},
		TestImports:    map[string][]string{},
		LiteralEdges:   map[string][]string{},
		CheckEdges:     map[string][]string{},
		CheckPackages:  map[string][]string{},
		CheckScripts:   map[string][]string{},
	}
	dirOwner, err := directoryOwners(m, files)
	if err != nil {
		return g, err
	}
	falsePositive := func(file, component string) bool {
		for _, fp := range LiteralFalsePositives {
			if fp.File == file && fp.Component == component {
				return true
			}
		}
		return false
	}

	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	for _, f := range sorted {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		from := ownerOf(m, f)
		if from == "" {
			continue
		}
		fi, err := ParseGoFileImports(root, f)
		if err != nil {
			return g, err
		}
		for _, dir := range fi.Imports {
			to := dirOwner[dir]
			if to == "" {
				continue
			}
			if fi.Test {
				addEdge(g.TestImports, from, to)
			} else {
				addEdge(g.NonTestImports, from, to)
			}
		}
		for _, dir := range fi.Literals {
			to := dirOwner[dir]
			g.Literals = append(g.Literals, LiteralRef{File: f, Owner: from, Directory: dir, Component: to})
			if to == "" || falsePositive(f, to) {
				continue
			}
			addEdge(g.LiteralEdges, from, to)
		}
	}

	recipes, err := makeRecipes(root)
	if err != nil {
		return g, err
	}
	for _, c := range m.Components {
		recipe := recipes[c.Check.Target]
		if recipe == "" {
			continue
		}
		pkgs := map[string]bool{}
		for _, mm := range makePackageRe.FindAllStringSubmatch(recipe, -1) {
			pkgs[mm[1]] = true
		}
		var pkgList []string
		for p := range pkgs {
			pkgList = append(pkgList, p)
		}
		sort.Strings(pkgList)
		for _, p := range pkgList {
			to, ok := dirOwner[p]
			if !ok || to == "" {
				continue
			}
			g.CheckPackages[c.ID] = append(g.CheckPackages[c.ID], p)
			addEdge(g.CheckEdges, c.ID, to)
		}
		scripts := map[string]bool{}
		for _, s := range makeScriptRe.FindAllString(recipe, -1) {
			scripts[s] = true
		}
		var scriptList []string
		for s := range scripts {
			scriptList = append(scriptList, s)
		}
		sort.Strings(scriptList)
		for _, s := range scriptList {
			to := ownerOf(m, s)
			if to == "" {
				continue
			}
			g.CheckScripts[c.ID] = append(g.CheckScripts[c.ID], s)
			addEdge(g.CheckEdges, c.ID, to)
		}
	}
	sortEdges(g.NonTestImports)
	sortEdges(g.TestImports)
	sortEdges(g.LiteralEdges)
	sortEdges(g.CheckEdges)
	sortEdges(g.CheckPackages)
	sortEdges(g.CheckScripts)
	return g, nil
}

// DeriveGraphFromRoot is DeriveGraph over the repository's tracked files.
func DeriveGraphFromRoot(root string, m Manifest) (DerivedGraph, error) {
	files, err := ListTracked(root)
	if err != nil {
		return DerivedGraph{}, err
	}
	return DeriveGraph(root, m, files)
}

func setOf(lists ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, l := range lists {
		for _, x := range l {
			out[x] = true
		}
	}
	return out
}

// VerifyDependencyCoverage asserts that dependencies is a superset of the
// derived non-test import edges, and that dependencies union
// verification_dependencies is a superset of the derived d7 (a)+(b)+(c)
// edges.
func VerifyDependencyCoverage(m Manifest, g DerivedGraph) error {
	for _, c := range m.Components {
		declared := setOf(c.Dependencies)
		for _, to := range g.NonTestImports[c.ID] {
			if !declared[to] {
				return fmt.Errorf("component %s has a non-test import of %s that ci/components.json does not declare in dependencies", c.ID, to)
			}
		}
		verified := setOf(c.Dependencies, c.VerificationDependencies)
		for _, src := range []struct {
			name  string
			edges []string
		}{
			{"test import", g.TestImports[c.ID]},
			{"module-path literal", g.LiteralEdges[c.ID]},
			{"check target", g.CheckEdges[c.ID]},
		} {
			for _, to := range src.edges {
				if !verified[to] {
					return fmt.Errorf("component %s has a %s dependency on %s that is in neither dependencies nor verification_dependencies", c.ID, src.name, to)
				}
			}
		}
	}
	return nil
}

// VerifyNoUnjustifiedEdges asserts that every declared edge is either derived
// or carries a written justification, that every justification is still
// needed, and that every module-path literal outside the owning component's
// key closure is a declared false positive.
func VerifyNoUnjustifiedEdges(m Manifest, g DerivedGraph) error {
	return verifyNoUnjustifiedEdges(m, g, JustifiedEdges, LiteralFalsePositives)
}

// verifyNoUnjustifiedEdges takes both tables as parameters so a positive
// control can remove one entry without mutating package state.
func verifyNoUnjustifiedEdges(m Manifest, g DerivedGraph, justifiedEdges []JustifiedEdge, falsePositives []LiteralFalsePositive) error {
	justified := map[string]string{}
	for _, j := range justifiedEdges {
		if strings.TrimSpace(j.Reason) == "" {
			return fmt.Errorf("justified edge %s -> %s has no written reason", j.From, j.To)
		}
		justified[j.From+" -> "+j.To] = j.Reason
	}
	derivedFor := func(id string) map[string]bool {
		return setOf(g.NonTestImports[id], g.TestImports[id], g.LiteralEdges[id], g.CheckEdges[id])
	}
	used := map[string]bool{}
	for _, c := range m.Components {
		derived := derivedFor(c.ID)
		for _, to := range append(append([]string(nil), c.Dependencies...), c.VerificationDependencies...) {
			if derived[to] {
				continue
			}
			edge := c.ID + " -> " + to
			if _, ok := justified[edge]; !ok {
				return fmt.Errorf("component edge %s is declared but not derived from any import, literal or check target, and has no JustifiedEdges entry", edge)
			}
			used[edge] = true
		}
	}
	for _, j := range justifiedEdges {
		edge := j.From + " -> " + j.To
		if !used[edge] {
			return fmt.Errorf("JustifiedEdges entry %s is stale: the edge is either no longer declared or now derived; delete the entry", edge)
		}
	}

	usedFP := map[string]bool{}
	for _, ref := range g.Literals {
		if ref.Component == "" || ref.Component == ref.Owner {
			continue
		}
		inClosure := false
		for _, id := range KeyClosure(m, ref.Owner) {
			if id == ref.Component {
				inClosure = true
				break
			}
		}
		key := ref.File + " -> " + ref.Component
		declaredFP := false
		for _, fp := range falsePositives {
			if fp.File == ref.File && fp.Component == ref.Component {
				if strings.TrimSpace(fp.Reason) == "" {
					return fmt.Errorf("literal false positive %s has no written reason", key)
				}
				declaredFP = true
				usedFP[key] = true
			}
		}
		if !inClosure && !declaredFP {
			return fmt.Errorf("module-path literal %s names a component outside %s's key closure and is not declared in LiteralFalsePositives", key, ref.Owner)
		}
	}
	for _, fp := range falsePositives {
		if !usedFP[fp.File+" -> "+fp.Component] {
			return fmt.Errorf("LiteralFalsePositives entry %s -> %s is stale: no such literal was found; delete the entry", fp.File, fp.Component)
		}
	}
	return nil
}

// VerifyCheckTargetInsideClosure asserts that every package and every script
// a component's check target executes is owned by a component inside that
// component's evidence-key closure. Without it a check could compile sources
// the key does not hash.
func VerifyCheckTargetInsideClosure(m Manifest, g DerivedGraph) error {
	for _, c := range m.Components {
		inClosure := setOf(KeyClosure(m, c.ID))
		for _, to := range g.CheckEdges[c.ID] {
			if !inClosure[to] {
				return fmt.Errorf("component %s check target %s executes sources owned by %s, which is outside its evidence-key closure", c.ID, c.Check.Target, to)
			}
		}
	}
	return nil
}
