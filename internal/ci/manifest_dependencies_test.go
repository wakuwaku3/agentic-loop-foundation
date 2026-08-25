package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func realManifest(t *testing.T) (string, Manifest) {
	t.Helper()
	root := filepath.Join("..", "..")
	m, err := Load(filepath.Join(root, "ci", "components.json"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return root, m
}

func realGraph(t *testing.T) (Manifest, DerivedGraph) {
	t.Helper()
	root, m := realManifest(t)
	g, err := DeriveGraphFromRoot(root, m)
	if err != nil {
		t.Fatalf("derive graph: %v", err)
	}
	return m, g
}

// copyManifest returns a deep copy so a positive control can edit an
// in-memory manifest without touching any tracked file.
func copyManifest(m Manifest) Manifest {
	out := m
	out.AllOnChange = append([]string(nil), m.AllOnChange...)
	out.Components = make([]Component, len(m.Components))
	for i, c := range m.Components {
		cp := c
		cp.Roots = append([]string(nil), c.Roots...)
		cp.PublicContracts = append([]string(nil), c.PublicContracts...)
		cp.ContractDependencies = append([]string(nil), c.ContractDependencies...)
		cp.Dependencies = append([]string(nil), c.Dependencies...)
		cp.VerificationDependencies = append([]string(nil), c.VerificationDependencies...)
		out.Components[i] = cp
	}
	return out
}

func withoutDependency(m Manifest, from, to string) Manifest {
	out := copyManifest(m)
	for i := range out.Components {
		if out.Components[i].ID != from {
			continue
		}
		var kept []string
		for _, d := range out.Components[i].Dependencies {
			if d != to {
				kept = append(kept, d)
			}
		}
		out.Components[i].Dependencies = kept
	}
	return out
}

// TestManifestDependencySurface asserts the three manifest checks against the
// real ci/components.json, then runs the three manifest-side positive
// controls (PC1, PC2, PC3) required by dp-v2-045 d9.
func TestManifestDependencySurface(t *testing.T) {
	m, g := realGraph(t)

	t.Run("VerifyDependencyCoverage", func(t *testing.T) {
		if err := VerifyDependencyCoverage(m, g); err != nil {
			t.Fatalf("real manifest fails dependency coverage: %v", err)
		}
	})
	t.Run("VerifyNoUnjustifiedEdges", func(t *testing.T) {
		if err := VerifyNoUnjustifiedEdges(m, g); err != nil {
			t.Fatalf("real manifest has an unjustified edge: %v", err)
		}
	})
	t.Run("VerifyCheckTargetInsideClosure", func(t *testing.T) {
		if err := VerifyCheckTargetInsideClosure(m, g); err != nil {
			t.Fatalf("real manifest has a check target outside its closure: %v", err)
		}
	})

	// PC1: the coverage assertion must be able to fail. Removing runner's
	// declared dependency on application, in an in-memory copy only, must be
	// caught and named.
	t.Run("PC1_removed_dependency_fails_coverage", func(t *testing.T) {
		broken := withoutDependency(m, "runner", "application")
		err := VerifyDependencyCoverage(broken, g)
		if err == nil {
			t.Fatal("PC1: removing runner -> application did not fail dependency coverage")
		}
		msg := err.Error()
		if !strings.Contains(msg, "runner") || !strings.Contains(msg, "application") {
			t.Fatalf("PC1: error must name both runner and application, got %q", msg)
		}
		// The real manifest must still pass, proving the copy was isolated.
		if err := VerifyDependencyCoverage(m, g); err != nil {
			t.Fatalf("PC1: the real manifest was mutated: %v", err)
		}
	})

	// PC2: the justification assertion must be able to fail. Dropping one
	// JustifiedEdges entry must be reported, naming that edge.
	t.Run("PC2_removed_justification_fails", func(t *testing.T) {
		const from, to = "release", "contracts"
		var reduced []JustifiedEdge
		for _, j := range JustifiedEdges {
			if j.From == from && j.To == to {
				continue
			}
			reduced = append(reduced, j)
		}
		if len(reduced) != len(JustifiedEdges)-1 {
			t.Fatalf("PC2: expected to drop exactly one entry, table has %d", len(JustifiedEdges))
		}
		err := verifyNoUnjustifiedEdges(m, g, reduced, LiteralFalsePositives)
		if err == nil {
			t.Fatalf("PC2: dropping the %s -> %s justification did not fail", from, to)
		}
		if !strings.Contains(err.Error(), from+" -> "+to) {
			t.Fatalf("PC2: error must name %s -> %s, got %q", from, to, err.Error())
		}
		if err := VerifyNoUnjustifiedEdges(m, g); err != nil {
			t.Fatalf("PC2: the real table was mutated: %v", err)
		}
	})

	// PC3: the AST extractor itself, over a hermetic fixture module. It must
	// find a non-test import, classify a _test.go-only import as test, find
	// an import inside a build-tag-excluded file, and must NOT report a
	// string literal shaped like a module import path as an import.
	t.Run("PC3_ast_extractor_over_fixture_module", func(t *testing.T) {
		dir := t.TempDir()
		write := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
		write("plain.go", "package fixture\n\nimport _ \""+ModulePath+"/internal/domain\"\n")
		write("plain_test.go", "package fixture\n\nimport _ \""+ModulePath+"/internal/provider\"\n")
		write("excluded.go", "//go:build never_built\n\npackage fixture\n\nimport _ \""+ModulePath+"/internal/update\"\n")
		write("literal.go", "package fixture\n\nconst notAnImport = \""+ModulePath+"/internal/web\"\n")

		cases := []struct {
			file        string
			wantTest    bool
			wantImport  string
			wantLiteral string
		}{
			{file: "plain.go", wantImport: "internal/domain"},
			{file: "plain_test.go", wantTest: true, wantImport: "internal/provider"},
			{file: "excluded.go", wantImport: "internal/update"},
			{file: "literal.go", wantLiteral: "internal/web"},
		}
		for _, tc := range cases {
			fi, err := ParseGoFileImports(dir, tc.file)
			if err != nil {
				t.Fatalf("PC3: parse %s: %v", tc.file, err)
			}
			if fi.Test != tc.wantTest {
				t.Fatalf("PC3: %s test classification = %v, want %v", tc.file, fi.Test, tc.wantTest)
			}
			if tc.wantImport != "" {
				if len(fi.Imports) != 1 || fi.Imports[0] != tc.wantImport {
					t.Fatalf("PC3: %s imports = %v, want [%s]", tc.file, fi.Imports, tc.wantImport)
				}
				if len(fi.Literals) != 0 {
					t.Fatalf("PC3: %s must report no literals, got %v", tc.file, fi.Literals)
				}
			}
			if tc.wantLiteral != "" {
				if len(fi.Imports) != 0 {
					t.Fatalf("PC3: %s must report no imports, a module-path string literal is not an import, got %v", tc.file, fi.Imports)
				}
				if len(fi.Literals) != 1 || fi.Literals[0] != tc.wantLiteral {
					t.Fatalf("PC3: %s literals = %v, want [%s]", tc.file, fi.Literals, tc.wantLiteral)
				}
			}
		}
	})
}

// TestDerivedGraphMatchesDeclaredNarrowings pins the three measurements that
// dp-v2-045 d6/d22 require for the edges this task deletes: neither
// reconciler -> store-firestore nor infra -> store-firestore is an import, a
// contract read or a check-target relation, and the real store-firestore ->
// infra direction is present.
func TestDerivedGraphMatchesDeclaredNarrowings(t *testing.T) {
	m, g := realGraph(t)
	has := func(edges []string, id string) bool {
		for _, e := range edges {
			if e == id {
				return true
			}
		}
		return false
	}
	for _, from := range []string{"reconciler", "infra"} {
		for name, edges := range map[string][]string{
			"non-test import":     g.NonTestImports[from],
			"test import":         g.TestImports[from],
			"module-path literal": g.LiteralEdges[from],
			"check target":        g.CheckEdges[from],
		} {
			if has(edges, "store-firestore") {
				t.Fatalf("%s -> store-firestore is derived as a %s; the deletion is not justified", from, name)
			}
		}
	}
	if !has(g.NonTestImports["store-firestore"], "infra") {
		t.Fatal("store-firestore -> infra must be a derived non-test import; it is the real direction of the deleted infra -> store-firestore edge")
	}
	for _, c := range m.Components {
		if c.ID != "infra" {
			continue
		}
		if len(c.Dependencies) != 0 {
			t.Fatalf("infra must declare no dependencies, got %v", c.Dependencies)
		}
	}
}
