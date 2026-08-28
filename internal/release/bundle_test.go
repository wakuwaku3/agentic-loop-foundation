package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// --- shared synthetic tree fixture, used by bundle_test.go, docs_test.go,
// pipeline_test.go and journey_test.go. Every test using it gets its own
// t.TempDir(); nothing here is package-level mutable state.

type syntheticTreeOptions struct {
	Release       string
	CapabilityIDs []string
	PreviewDocs   map[string]string // filename (under docs/preview/) -> content
	StableDocs    map[string]string // filename (under docs/stable/) -> content
	ExtraFiles    map[string]string // repo-relative path -> content, outside every role
}

func defaultSyntheticOptions() syntheticTreeOptions {
	return syntheticTreeOptions{
		Release:       "0.1.0-synthetic",
		CapabilityIDs: []string{"cap-alpha", "cap-beta"},
		PreviewDocs: map[string]string{
			"index.md": "# Preview\n\nSynthetic preview index.\nRelease: 0.1.0-synthetic\n",
			"stable-diff.md": "# Stableとの差分\n\n" +
				"## 差分\n\ntext\n\n" +
				"## 既知の問題\n\ntext\n\n" +
				"## Stableへ戻す方法\n\ntext\n\n" +
				"## 昇格に不足している実証\n\ntext\n",
		},
		StableDocs: map[string]string{
			"index.md": "# Stable\n\nSynthetic stable index.\nStable release: none\n",
		},
	}
}

func newSyntheticTree(t *testing.T, opts syntheticTreeOptions) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, data []byte) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	type rawCapability struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	caps := make([]rawCapability, 0, len(opts.CapabilityIDs))
	for _, id := range opts.CapabilityIDs {
		caps = append(caps, rawCapability{ID: id, Name: id, Status: "preview"})
	}
	contract := struct {
		SchemaVersion string          `json:"schema_version"`
		ID            string          `json:"id"`
		Kind          string          `json:"kind"`
		CreatedAt     string          `json:"created_at"`
		CorrelationID string          `json:"correlation_id"`
		Release       string          `json:"release"`
		Capabilities  []rawCapability `json:"capabilities"`
		Verification  []string        `json:"verification"`
		Rollback      struct {
			Procedure string `json:"procedure"`
			Target    string `json:"target"`
		} `json:"rollback"`
	}{
		SchemaVersion: "v1", ID: "rc-synthetic", Kind: "release-contract",
		CreatedAt: "2026-08-24T00:00:00Z", CorrelationID: "synthetic",
		Release: opts.Release, Capabilities: caps, Verification: []string{"go test"},
	}
	contract.Rollback.Procedure = "git revert"
	contract.Rollback.Target = "main"
	contractBytes, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	write("contracts/release-contract/foundation.json", contractBytes)
	write("contracts/schemas/dummy.json", []byte(`{"schema_version":"v1"}`))
	write("contracts/openapi/openapi-v1.yaml", []byte("openapi: 3.0.0\n"))
	write("ci/components.json", []byte(`{"version":1}`))
	write("go.mod", []byte("module synthetic\n"))
	write("go.sum", []byte("\n"))
	write("devbox.lock", []byte("{}"))
	write("firestore.indexes.json", []byte("{}"))
	write("devbox.json", []byte("{}"))
	write("firebase.json", []byte("{}"))
	for name, content := range opts.PreviewDocs {
		write("docs/preview/"+name, []byte(content))
	}
	for name, content := range opts.StableDocs {
		write("docs/stable/"+name, []byte(content))
	}
	for rel, content := range opts.ExtraFiles {
		write(rel, []byte(content))
	}
	return root
}

// mutateContractCorrelationID changes the contract's correlation_id in place
// while keeping the JSON well-formed, so the contract role's digest changes
// without the mutation being masked by a decode failure.
func mutateContractCorrelationID(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "contracts", "release-contract", "foundation.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(data), `"correlation_id":"synthetic"`, `"correlation_id":"synthetic-mutated"`, 1)
	if mutated == string(data) {
		t.Fatal("expected to find correlation_id field to mutate")
	}
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
}

func flipByte(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		data = []byte{0}
	} else {
		data = append([]byte(nil), data...)
		data[0] ^= 0xFF
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- A3: bundle manifest and digest framing.

func TestBundleAssemblyIsDeterministicAndOrderIndependent(t *testing.T) {
	root := newSyntheticTree(t, defaultSyntheticOptions())

	first, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("first assembly: %v", err)
	}
	second, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("second assembly: %v", err)
	}
	if first.BundleDigest != second.BundleDigest {
		t.Fatalf("BundleDigest not deterministic: %s vs %s", first.BundleDigest, second.BundleDigest)
	}
	if first.DocsDigest != second.DocsDigest {
		t.Fatalf("DocsDigest not deterministic: %s vs %s", first.DocsDigest, second.DocsDigest)
	}
	if first.ContractDigest != second.ContractDigest {
		t.Fatalf("ContractDigest not deterministic: %s vs %s", first.ContractDigest, second.ContractDigest)
	}
	if first.Contract.DocsDigest != first.DocsDigest {
		t.Fatalf("CompiledContract.DocsDigest = %s, want doc-set digest %s by construction", first.Contract.DocsDigest, first.DocsDigest)
	}

	shuffled := make([]Member, len(first.Members))
	for i, m := range first.Members {
		shuffled[len(first.Members)-1-i] = m
	}
	if got := BundleDigestFromMembers(shuffled); got != first.BundleDigest {
		t.Fatalf("shuffled member order produced a different digest: %s vs %s", got, first.BundleDigest)
	}
}

func TestBundleAssemblyRefusesSymlinkMember(t *testing.T) {
	root := newSyntheticTree(t, defaultSyntheticOptions())
	target := filepath.Join(root, "docs", "preview", "index.md")
	linkPath := filepath.Join(root, "docs", "preview", "linked.md")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	_, err := AssembleFromRoot(root)
	if err == nil {
		t.Fatal("expected symlinked documentation member to be refused")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error does not name a symlink refusal: %v", err)
	}
}

func TestBundleAssemblyRefusesMissingDeclaredMember(t *testing.T) {
	root := newSyntheticTree(t, defaultSyntheticOptions())
	if err := os.Remove(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal(err)
	}
	_, err := AssembleFromRoot(root)
	if err == nil {
		t.Fatal("expected assembly to refuse a missing declared member (go.mod)")
	}
	if !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("error does not name the missing member: %v", err)
	}
}

func TestBundleAssemblyRefusesRoleWithZeroMembers(t *testing.T) {
	opts := defaultSyntheticOptions()
	opts.PreviewDocs = nil
	opts.StableDocs = nil
	root := newSyntheticTree(t, opts)
	_, err := AssembleFromRoot(root)
	if err == nil {
		t.Fatal("expected assembly to refuse the documentation role with zero members")
	}
	if !strings.Contains(err.Error(), string(RoleDocumentation)) {
		t.Fatalf("error does not name the empty role: %v", err)
	}
}

func TestMemberPathEscapingRootIsRefused(t *testing.T) {
	if _, err := NewMember(RoleContract, "../outside.json", []byte("x")); err == nil {
		t.Fatal("expected a path escaping the root to be refused")
	}
	if _, err := NewMember(RoleContract, "a/../../outside.json", []byte("x")); err == nil {
		t.Fatal("expected a path escaping the root (via traversal) to be refused")
	}
	if _, err := NewMember(RoleContract, "contracts/release-contract/foundation.json", []byte("x")); err != nil {
		t.Fatalf("a normal repository-relative path must not be refused: %v", err)
	}
}

// --- A4: the guarded/unguarded partition is machine-checked and
// self-declaring, recomputed from ci/components.json directly (no import of
// internal/ci), per dp-v2-021 d5.

// keyClosureDocument is the published evidence-key closure, read from
// ci/key-closure.json (dp-v2-045 d10). internal/release must not import
// internal/ci (dp-v2-021 d12, enforced by source_guard_test.go), so the
// closure rule is consumed as data rather than reimplemented here: the
// previous local copy of the rule would have kept the pre-v2 semantics after
// V2-045 and turned this test into a test of a rule nobody uses.
type keyClosureEntry struct {
	Component string   `json:"component"`
	Closure   []string `json:"closure"`
	Patterns  []string `json:"patterns"`
}
type keyClosureDocument struct {
	Version       int               `json:"version"`
	KeyVersion    string            `json:"key_version"`
	Unconditional []string          `json:"unconditional"`
	Components    []keyClosureEntry `json:"components"`
}

// planMatch mirrors internal/ci/planner.go's unexported match(), duplicated
// here rather than imported, per dp-v2-021 d12 (internal/release must not
// import internal/ci).
func planMatch(pattern, path string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	path = filepath.ToSlash(path)
	if pattern == path {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "/**")+"/")
	}
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(path, pattern)
	}
	ok, _ := filepath.Match(pattern, path)
	return ok
}

func releaseEvidenceKeyClosure(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "ci", "key-closure.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc keyClosureDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.KeyVersion == "" || len(doc.Unconditional) == 0 {
		t.Fatalf("ci/key-closure.json is not a v1 closure document: %+v", doc)
	}
	for _, e := range doc.Components {
		if e.Component != "release" {
			continue
		}
		return append(append([]string(nil), e.Patterns...), doc.Unconditional...)
	}
	t.Fatal("ci/key-closure.json has no release component")
	return nil
}

func closureCovers(patterns []string, path string) bool {
	for _, p := range patterns {
		if planMatch(p, path) {
			return true
		}
	}
	return false
}

func isUnguardedAllowlisted(path string) (bool, string) {
	for _, u := range UnguardedMembers {
		if globMatch(u.Glob, path) {
			return true, u.Reason
		}
	}
	return false, ""
}

func TestReleaseEvidenceKeyClosureAndUnguardedAllowlist(t *testing.T) {
	root := filepath.Join("..", "..")
	patterns := releaseEvidenceKeyClosure(t, root)

	for _, must := range []string{
		"internal/release/bundle.go",
		"contracts/release-contract/foundation.json",
		"contracts/schemas/release-contract.json",
		"ci/components.json",
		"go.mod",
		"go.sum",
		"devbox.lock",
		"Makefile",
		"devbox.json",
		"ci/key-closure.json",
	} {
		if !closureCovers(patterns, must) {
			t.Fatalf("expected release evidence-key closure to cover %s", must)
		}
	}

	// Every allowlisted glob must currently be unguarded. If ci/components.json
	// changes so that a glob becomes covered, this fails and the allowlist
	// entry must be deleted (escalation E1), not the assertion relaxed.
	for _, u := range UnguardedMembers {
		example := strings.TrimSuffix(strings.TrimSuffix(u.Glob, "**.md"), "**")
		if example == "" {
			example = u.Glob
		} else if strings.HasSuffix(u.Glob, "**.md") {
			example += "example.md"
		}
		if closureCovers(patterns, example) {
			t.Fatalf("allowlisted member %q (example %q) is now covered by the release evidence-key closure; delete the allowlist entry (E1)", u.Glob, example)
		}
	}

	assembled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("assemble real tree: %v", err)
	}
	guardedRoles := map[Role]bool{
		RoleContract:               true,
		RoleSchema:                 true,
		RoleAPIContract:            true,
		RoleImplementationManifest: true,
	}
	for _, m := range assembled.Members {
		covered := closureCovers(patterns, m.Path)
		allowlisted, _ := isUnguardedAllowlisted(m.Path)
		if !covered && !allowlisted {
			t.Fatalf("bundle member %s (role %s) is neither inside the evidence-key closure nor in UnguardedMembers", m.Path, m.Role)
		}
		if guardedRoles[m.Role] && !covered {
			t.Fatalf("guarded role %s member %s is unexpectedly outside the evidence-key closure", m.Role, m.Path)
		}
		// The clause that failed when an unguarded-role member IS covered was
		// deleted by V2-045 (dp-v2-045 d11): it encoded a fact about the old,
		// narrower closure, and coverage widening is the improvement. devbox.json
		// is now covered through the unconditional set and is no longer
		// allowlisted. The surviving requirement below is the one that matters:
		// a member outside the closure must be named in UnguardedMembers.
		if !guardedRoles[m.Role] && !covered && !allowlisted {
			t.Fatalf("unguarded role %s member %s must be named in UnguardedMembers", m.Role, m.Path)
		}
	}
}

// --- A6: evidence freshness follows one subtest per role plus a negative
// control, computed from digests rather than from a boolean the test sets.

func evidenceFor(candidate domain.ReleaseCandidate, bundleDigest string) []domain.CapabilityEvidence {
	evidence := make([]domain.CapabilityEvidence, 0, len(candidate.Capabilities))
	for _, cap := range candidate.Capabilities {
		evidence = append(evidence, domain.CapabilityEvidence{
			Capability: cap, CandidateID: candidate.CandidateID, CandidateDigest: candidate.CandidateDigest,
			BundleDigest: bundleDigest, ContractDigest: candidate.ContractDigest, DocsDigest: candidate.DocsDigest,
			Digest: "ev-" + cap, Verified: true, Fresh: true, Target: "preview", Provider: "fake",
		})
	}
	return evidence
}

func TestEvidenceFreshnessInvalidatesPerRole(t *testing.T) {
	touchByRole := map[Role]func(t *testing.T, root string){
		RoleContract: func(t *testing.T, root string) { mutateContractCorrelationID(t, root) },
		RoleSchema: func(t *testing.T, root string) {
			flipByte(t, filepath.Join(root, "contracts", "schemas", "dummy.json"))
		},
		RoleAPIContract: func(t *testing.T, root string) {
			flipByte(t, filepath.Join(root, "contracts", "openapi", "openapi-v1.yaml"))
		},
		RoleImplementationManifest: func(t *testing.T, root string) { flipByte(t, filepath.Join(root, "go.sum")) },
		RoleMigration:              func(t *testing.T, root string) { flipByte(t, filepath.Join(root, "firestore.indexes.json")) },
		RoleConfiguration:          func(t *testing.T, root string) { flipByte(t, filepath.Join(root, "devbox.json")) },
		RoleDocumentation:          func(t *testing.T, root string) { flipByte(t, filepath.Join(root, "docs", "preview", "index.md")) },
	}

	for role, touch := range touchByRole {
		role, touch := role, touch
		t.Run(string(role), func(t *testing.T) {
			root := newSyntheticTree(t, defaultSyntheticOptions())
			before, err := AssembleFromRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := domain.NewReleaseID("release-1")
			cid, _ := domain.NewReleaseID("candidate-1")
			targets := map[string]domain.CapabilityTarget{}
			for _, cap := range before.Contract.Capabilities {
				targets[cap] = domain.CapabilityTarget{Target: "preview", Provider: "fake"}
			}
			input := CandidateInput{
				ReleaseID: id, CandidateID: cid, CandidateDigest: "candidate-1", Version: 1,
				Status: domain.ReleaseExercising, CapabilityTargets: targets,
				RollbackEvidence: true, ResumeEvidence: true, ExpectedControlRevision: 1, FencingToken: 1,
			}
			_, beforeCandidate, err := AssembleCandidate(root, input)
			if err != nil {
				t.Fatal(err)
			}
			beforeCandidate.Evidence = evidenceFor(beforeCandidate, before.BundleDigest)
			if err := beforeCandidate.CanPromote(); err != nil {
				t.Fatalf("candidate before mutation must be promotable: %v", err)
			}

			touch(t, root)

			after, err := AssembleFromRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			if after.BundleDigest == before.BundleDigest {
				t.Fatalf("role %s: BundleDigest did not change after mutating a %s member", role, role)
			}

			_, afterCandidate, err := AssembleCandidate(root, input)
			if err != nil {
				t.Fatal(err)
			}
			// Evidence recorded against the pre-mutation bundle digest must no
			// longer satisfy CanPromote once the bundle digest has moved.
			afterCandidate.Evidence = beforeCandidate.Evidence
			if err := afterCandidate.CanPromote(); err == nil {
				t.Fatalf("role %s: stale evidence (bound to old BundleDigest) still promoted after mutation", role)
			}
		})
	}
}

func TestEvidenceFreshnessNegativeControlUnrelatedFileInvalidatesNothing(t *testing.T) {
	opts := defaultSyntheticOptions()
	opts.ExtraFiles = map[string]string{"NOTES.txt": "unrelated note\n"}
	root := newSyntheticTree(t, opts)
	before, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.NewReleaseID("release-1")
	cid, _ := domain.NewReleaseID("candidate-1")
	targets := map[string]domain.CapabilityTarget{}
	for _, cap := range before.Contract.Capabilities {
		targets[cap] = domain.CapabilityTarget{Target: "preview", Provider: "fake"}
	}
	input := CandidateInput{
		ReleaseID: id, CandidateID: cid, CandidateDigest: "candidate-1", Version: 1,
		Status: domain.ReleaseExercising, CapabilityTargets: targets,
		RollbackEvidence: true, ResumeEvidence: true, ExpectedControlRevision: 1, FencingToken: 1,
	}
	_, candidate, err := AssembleCandidate(root, input)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Evidence = evidenceFor(candidate, before.BundleDigest)
	if err := candidate.CanPromote(); err != nil {
		t.Fatalf("candidate before mutation must be promotable: %v", err)
	}

	flipByte(t, filepath.Join(root, "NOTES.txt"))

	after, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.BundleDigest != before.BundleDigest {
		t.Fatal("BundleDigest changed after mutating a file matching no role glob")
	}
	if after.DocsDigest != before.DocsDigest {
		t.Fatal("DocsDigest changed after mutating a file matching no role glob")
	}
	_, afterCandidate, err := AssembleCandidate(root, input)
	if err != nil {
		t.Fatal(err)
	}
	afterCandidate.Evidence = candidate.Evidence
	if err := afterCandidate.CanPromote(); err != nil {
		t.Fatalf("unrelated file mutation must not invalidate evidence: %v", err)
	}
}
