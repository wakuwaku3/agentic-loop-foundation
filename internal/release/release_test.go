package release

import (
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

func candidate(t *testing.T) domain.ReleaseCandidate {
	t.Helper()
	id, _ := domain.NewReleaseID("release-1")
	cid, _ := domain.NewReleaseID("candidate-1")
	return domain.ReleaseCandidate{ID: id, CandidateID: cid, CandidateDigest: "candidate", Version: 1, Status: domain.ReleaseExercising, BundleDigest: "bundle", ContractDigest: "contract", DocsDigest: "docs", EvidenceDigest: "evidence", Capabilities: []string{"healthz"}, CapabilityTargets: map[string]domain.CapabilityTarget{"healthz": {Target: "preview", Provider: "fake"}}, Evidence: []domain.CapabilityEvidence{{Capability: "healthz", CandidateID: cid, CandidateDigest: "candidate", BundleDigest: "bundle", ContractDigest: "contract", DocsDigest: "docs", Digest: "ev", Target: "preview", Provider: "fake", Fresh: true, Verified: true}}, RollbackEvidence: true, ResumeEvidence: true, ExpectedControlRevision: 1, FencingToken: 2}
}

func TestBundleImmutableRouteAndRollback(t *testing.T) {
	b, err := NewBundle("repo", candidate(t), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	if err := store.Put(b); err != nil {
		t.Fatal(err)
	}
	changed := b.Snapshot()
	changed.Candidate.CandidateDigest = "changed"
	if err := store.Put(changed); err != ErrImmutableConflict {
		t.Fatalf("got %v", err)
	}
	router := NewRouter()
	if err := router.SetPreview("repo", "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := router.Promote("repo", "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := router.SetPreview("repo", "next"); err != nil {
		t.Fatal(err)
	}
	if err := router.Promote("repo", "next"); err != nil {
		t.Fatal(err)
	}
	if err := router.Rollback("repo"); err != nil {
		t.Fatal(err)
	}
	route, _ := router.Get("repo")
	if route.StableDigest != "candidate" {
		t.Fatalf("route=%#v", route)
	}
}

func TestContractAndPromotionGateRejectDriftAndStaleEvidence(t *testing.T) {
	contract, err := CompileContract([]byte(`{"schema_version":"v1","release":"1.0.0","capabilities":[{"id":"healthz"}]}`), []byte("docs-v1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBundle("repo", candidate(t), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	b.Candidate.ContractDigest = contract.Digest
	b.Candidate.DocsDigest = contract.DocsDigest
	b.Candidate.Evidence[0].ContractDigest = contract.Digest
	b.Candidate.Evidence[0].DocsDigest = contract.DocsDigest
	control := domain.EffectiveControlResult{Found: true, Mode: domain.ControlAllow, Revision: 1}
	permit, err := domain.Permit(control, domain.PermitRequest{Kind: domain.PermitPromotion, ControlRevision: 1, FencingToken: 2, ExpectedFencingToken: 2, Resource: "release"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PromotionGate(b, control, permit); err != nil {
		t.Fatal(err)
	}
	stale := b.Snapshot()
	stale.Candidate.Evidence[0].Fresh = false
	if _, err := PromotionGate(stale, control, permit); err == nil {
		t.Fatal("stale evidence promoted")
	}
	drift := b.Snapshot()
	drift.Candidate.DocsDigest = "drift"
	if _, err := PromotionGate(drift, control, permit); err == nil {
		t.Fatal("docs drift promoted")
	}
}
