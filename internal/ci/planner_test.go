package ci

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture() Manifest {
	return Manifest{Version: 1, Components: []Component{{ID: "a", Roots: []string{"a/**"}, PublicContracts: []string{"a/api.json"}, Check: Check{Runner: "make", Target: "a"}}, {ID: "b", Roots: []string{"b/**"}, ContractDependencies: []string{"a/api.json"}, Dependencies: []string{"a"}, Check: Check{Runner: "make", Target: "b"}}}}
}
func TestAffectedReverseClosure(t *testing.T) {
	p, e := Affected(fixture(), []string{"a/api.json"})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Selected) != 2 || p.Selected[0] != "a" || p.Selected[1] != "b" {
		t.Fatalf("selected=%v", p.Selected)
	}
}
func TestManifestRejectsCycle(t *testing.T) {
	m := fixture()
	m.Components[0].Dependencies = []string{"b"}
	if err := Validate(m); err == nil {
		t.Fatal("expected cycle")
	}
}

// TestVerificationDependenciesAreExemptFromAcyclicity pins dp-v2-045 d7/A1:
// the real runner <-> reconciler pair (their test files import each other)
// must be accepted in verification_dependencies and rejected in
// dependencies. Without the exemption these edges could not be declared at
// all, and the evidence key would not cover them.
func TestVerificationDependenciesAreExemptFromAcyclicity(t *testing.T) {
	base := Manifest{Version: 1, Components: []Component{
		{ID: "runner", Roots: []string{"internal/runner/**"}, Check: Check{Runner: "make", Target: "component-runner"}},
		{ID: "reconciler", Roots: []string{"internal/reconciler/**"}, Check: Check{Runner: "make", Target: "component-reconciler"}},
	}}

	cyclicVerification := base
	cyclicVerification.Components = append([]Component(nil), base.Components...)
	cyclicVerification.Components[0].VerificationDependencies = []string{"reconciler"}
	cyclicVerification.Components[1].VerificationDependencies = []string{"runner"}
	if err := Validate(cyclicVerification); err != nil {
		t.Fatalf("cyclic verification_dependencies must be accepted: %v", err)
	}
	if got := KeyClosure(cyclicVerification, "runner"); len(got) != 2 || got[0] != "reconciler" || got[1] != "runner" {
		t.Fatalf("verification_dependencies must be inside the key closure, got %v", got)
	}

	cyclicDependencies := base
	cyclicDependencies.Components = append([]Component(nil), base.Components...)
	cyclicDependencies.Components[0].Dependencies = []string{"reconciler"}
	cyclicDependencies.Components[1].Dependencies = []string{"runner"}
	if err := Validate(cyclicDependencies); err == nil {
		t.Fatal("the same pair in dependencies must be rejected as a cycle")
	}
}

// TestAffectedIgnoresVerificationDependencies pins the non-goal: the
// affected-closure selection logic does not read verification_dependencies.
func TestAffectedIgnoresVerificationDependencies(t *testing.T) {
	m := Manifest{Version: 1, Components: []Component{
		{ID: "a", Roots: []string{"a/**"}, Check: Check{Runner: "make", Target: "a"}},
		{ID: "b", Roots: []string{"b/**"}, VerificationDependencies: []string{"a"}, Check: Check{Runner: "make", Target: "b"}},
	}}
	p, err := Affected(m, []string{"a/file"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Selected) != 1 || p.Selected[0] != "a" {
		t.Fatalf("verification_dependencies must not widen the affected closure, selected=%v", p.Selected)
	}
}

func TestTrackedOwnership(t *testing.T) {
	m := fixture()
	if err := ValidateTracked(m, []string{"a/file"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTracked(m, []string{"other"}); err == nil {
		t.Fatal("expected unowned")
	}
}

func TestCandidateComponentsDoesNotRequireUnchangedEvidence(t *testing.T) {
	root := "."
	m := fixture()
	key, err := EvidenceKeyWithManifest(root, m, m.Components[0])
	if err != nil {
		t.Fatal(err)
	}
	evidence := t.TempDir()
	if err := os.MkdirAll(evidence, 0755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"result":"passed","evidence_key":"` + key + `"}`)
	if err := os.WriteFile(filepath.Join(evidence, "a-"+key+".json"), body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := CandidateComponents(m, root, evidence, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := CandidateComponents(m, root, evidence, []string{"b"}); err == nil {
		t.Fatal("missing affected evidence accepted")
	}
}
