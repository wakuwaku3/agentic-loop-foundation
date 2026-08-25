package domain

// This file proves, by exhaustive enumeration rather than by sampling, that
// ReleaseCandidate.PromotionRejections is equivalent to the promotion
// authority ReleaseCandidate.CanPromote, and that it separates the twelve
// refusal classes CanPromote collapses onto four error messages.
//
// Two things are deliberate:
//
//   - The capability closure below reuses invariant_model_test.go's own
//     allCapabilityCases and buildReleaseCandidateFor. That file is the
//     existing guard on CanPromote's behaviour (Invariant 4) and is not
//     edited by this task; calling its enumeration directly means the
//     equivalence is proven over literally the same closed grid the
//     authority is already pinned on, not over a lookalike.
//   - The second closure below varies the axes the capability closure holds
//     fixed (status, the six identity fields, capability-target presence),
//     so every one of the twelve refusal classes is reached by an
//     enumeration and not only by the hand-written table.
//
// No test here uses math/rand, crypto/rand or time.Now/Since/Until, and no
// test starts a goroutine or sleeps; source_guard_test.go enforces the
// import allowlist for the whole package.

import (
	"errors"
	"testing"
)

// ===========================================================================
// The twelve kinds
// ===========================================================================

// wantPromotionRejectionKindCount is the number of distinct refusals
// CanPromote can reach, counted by reading its body: one status check, one
// empty-capability-set check, six identity fields in a single condition, one
// per-capability target check, one per-capability evidence check, and the
// rollback and resume flags in a single condition.
const wantPromotionRejectionKindCount = 12

func TestPromotionRejectionKindsAreTwelveAndDistinct(t *testing.T) {
	kinds := AllPromotionRejectionKinds()
	if len(kinds) != wantPromotionRejectionKindCount {
		t.Fatalf("AllPromotionRejectionKinds returned %d kinds, want %d", len(kinds), wantPromotionRejectionKindCount)
	}
	seen := map[PromotionRejectionKind]bool{}
	for _, k := range kinds {
		if k == "" {
			t.Fatal("a rejection kind is the empty string")
		}
		if seen[k] {
			t.Fatalf("rejection kind %q is declared twice", k)
		}
		seen[k] = true
	}
}

// ===========================================================================
// A fully-satisfied fixture and the twelve one-refusal mutations
// ===========================================================================

// promotableFixture is the single fully-satisfied candidate shape: the one
// cell of invariant_model_test.go's 16384-cell capability grid that
// CanPromote accepts, written out explicitly so each mutation below changes
// exactly one fact.
func promotableFixture() ReleaseCandidate {
	caps := []string{"cap-a", "cap-b", "cap-c"}
	r := ReleaseCandidate{
		ID: "release", Version: 1, Status: ReleaseExercising,
		Capabilities:    append([]string(nil), caps...),
		CandidateID:     "candidate",
		CandidateDigest: "candidate-digest",
		BundleDigest:    "bundle",
		ContractDigest:  "contract",
		DocsDigest:      "docs",
		EvidenceDigest:  "evidence",

		RollbackEvidence: true, ResumeEvidence: true,
		ExpectedControlRevision: 3, FencingToken: 44,
		CapabilityTargets: map[string]CapabilityTarget{},
	}
	for _, c := range caps {
		r.CapabilityTargets[c] = CapabilityTarget{Target: "target-" + c, Provider: "provider-" + c}
		r.Evidence = append(r.Evidence, CapabilityEvidence{
			Capability: c, Digest: "evidence-digest-" + c,
			Provider: "provider-" + c, Target: "target-" + c,
			Verified: true, Fresh: true,
			CandidateID: r.CandidateID, CandidateDigest: r.CandidateDigest,
			BundleDigest: r.BundleDigest, ContractDigest: r.ContractDigest, DocsDigest: r.DocsDigest,
		})
	}
	return r
}

// mirrorCandidateID and friends keep the evidence records internally
// consistent with the candidate when an identity field is emptied, so that
// emptying one field reaches its own refusal class and does NOT also trip
// the per-capability evidence class.
func clearEvidenceCandidateID(r *ReleaseCandidate) {
	for i := range r.Evidence {
		r.Evidence[i].CandidateID = ""
	}
}

func TestPromotableFixtureIsAcceptedByBothTheAuthorityAndTheEnumeration(t *testing.T) {
	c := promotableFixture()
	if err := c.CanPromote(); err != nil {
		t.Fatalf("CanPromote refused the fully-satisfied fixture: %v", err)
	}
	if got := c.PromotionRejections(); len(got) != 0 {
		t.Fatalf("PromotionRejections returned %d refusals for the fully-satisfied fixture: %+v", len(got), got)
	}
}

// TestPromotionRejectionsIsolateEachRefusalClass is the separation table the
// single-capability-projection mechanism could not provide: each row makes
// exactly one fact false and asserts exactly one refusal, of its own kind.
// empty-capability-set, each of the six identity fields, rollback-evidence
// and resume-evidence are therefore mutually distinguishable even though
// CanPromote reports all nine of them with the identical error value
// ErrEvidenceIncomplete.
func TestPromotionRejectionsIsolateEachRefusalClass(t *testing.T) {
	type row struct {
		kind       PromotionRejectionKind
		capability string
		mutate     func(*ReleaseCandidate)
	}
	rows := []row{
		{RejectStatusNotPromotable, "", func(r *ReleaseCandidate) { r.Status = ReleaseAssembling }},
		{RejectEmptyCapabilitySet, "", func(r *ReleaseCandidate) { r.Capabilities = nil }},
		{RejectCandidateIDMissing, "", func(r *ReleaseCandidate) { r.CandidateID = ""; clearEvidenceCandidateID(r) }},
		{RejectCandidateDigestMissing, "", func(r *ReleaseCandidate) {
			r.CandidateDigest = ""
			for i := range r.Evidence {
				r.Evidence[i].CandidateDigest = ""
			}
		}},
		{RejectBundleDigestMissing, "", func(r *ReleaseCandidate) {
			r.BundleDigest = ""
			for i := range r.Evidence {
				r.Evidence[i].BundleDigest = ""
			}
		}},
		{RejectContractDigestMissing, "", func(r *ReleaseCandidate) {
			r.ContractDigest = ""
			for i := range r.Evidence {
				r.Evidence[i].ContractDigest = ""
			}
		}},
		{RejectDocsDigestMissing, "", func(r *ReleaseCandidate) {
			r.DocsDigest = ""
			for i := range r.Evidence {
				r.Evidence[i].DocsDigest = ""
			}
		}},
		{RejectEvidenceDigestMissing, "", func(r *ReleaseCandidate) { r.EvidenceDigest = "" }},
		{RejectCapabilityTargetMissing, "cap-b", func(r *ReleaseCandidate) { delete(r.CapabilityTargets, "cap-b") }},
		{RejectCapabilityEvidenceMissing, "cap-b", func(r *ReleaseCandidate) {
			kept := r.Evidence[:0:0]
			for _, e := range r.Evidence {
				if e.Capability != "cap-b" {
					kept = append(kept, e)
				}
			}
			r.Evidence = kept
		}},
		{RejectRollbackEvidenceMissing, "", func(r *ReleaseCandidate) { r.RollbackEvidence = false }},
		{RejectResumeEvidenceMissing, "", func(r *ReleaseCandidate) { r.ResumeEvidence = false }},
	}
	if len(rows) != wantPromotionRejectionKindCount {
		t.Fatalf("the separation table has %d rows, want one per refusal class (%d)", len(rows), wantPromotionRejectionKindCount)
	}

	seenKinds := map[PromotionRejectionKind]bool{}
	authorityMessages := map[string]int{}
	for _, r := range rows {
		candidate := promotableFixture()
		r.mutate(&candidate)

		err := candidate.CanPromote()
		if err == nil {
			t.Fatalf("%s: CanPromote accepted a candidate the enumeration must refuse", r.kind)
		}
		authorityMessages[err.Error()]++

		got := candidate.PromotionRejections()
		if len(got) != 1 {
			t.Fatalf("%s: PromotionRejections returned %d refusals, want exactly 1: %+v", r.kind, len(got), got)
		}
		if got[0].Kind != r.kind {
			t.Fatalf("expected refusal kind %s, got %s (%+v)", r.kind, got[0].Kind, got[0])
		}
		if got[0].Capability != r.capability {
			t.Fatalf("%s: refusal names capability %q, want %q", r.kind, got[0].Capability, r.capability)
		}
		if got[0].Reason == "" {
			t.Fatalf("%s: refusal carries no reason", r.kind)
		}
		if seenKinds[r.kind] {
			t.Fatalf("%s appears twice in the separation table", r.kind)
		}
		seenKinds[r.kind] = true
	}
	if len(seenKinds) != wantPromotionRejectionKindCount {
		t.Fatalf("the table separated %d kinds, want %d", len(seenKinds), wantPromotionRejectionKindCount)
	}

	// The measurement that motivated the enumeration: the same twelve
	// classes reach the owner as only four distinct CanPromote messages,
	// nine of them the identical bare ErrEvidenceIncomplete value.
	const wantDistinctAuthorityMessages = 4
	const wantBareEvidenceIncompleteRows = 9
	if len(authorityMessages) != wantDistinctAuthorityMessages {
		t.Fatalf("CanPromote produced %d distinct messages across the twelve classes, want %d: %v", len(authorityMessages), wantDistinctAuthorityMessages, authorityMessages)
	}
	if got := authorityMessages[ErrEvidenceIncomplete.Error()]; got != wantBareEvidenceIncompleteRows {
		t.Fatalf("%d of the twelve classes collapse onto the bare ErrEvidenceIncomplete message, want %d", got, wantBareEvidenceIncompleteRows)
	}
	t.Logf("twelve refusal classes -> %d distinct CanPromote messages (%d of them the bare ErrEvidenceIncomplete value)", len(authorityMessages), authorityMessages[ErrEvidenceIncomplete.Error()])
}

// ===========================================================================
// Two-way equivalence, closure 1: the existing capability grid
// ===========================================================================

// TestPromotionRejectionsAgreeWithCanPromoteOverTheCapabilityClosure runs
// the identical 16384-cell grid TestInvariant4ReleaseCapabilityEvidenceClosure
// runs, reusing that file's own axis and builder, and asserts the biconditional
// on every cell.
func TestPromotionRejectionsAgreeWithCanPromoteOverTheCapabilityClosure(t *testing.T) {
	cases := allCapabilityCases()
	if len(cases) != wantCapabilityCaseCount {
		t.Fatalf("capability case axis drifted: %d", len(cases))
	}
	const wantGridSize = wantCapabilityCaseCount * wantCapabilityCaseCount * wantCapabilityCaseCount * 2 * 2
	const wantGridSizeLiteral = 16384
	if wantGridSize != wantGridSizeLiteral {
		t.Fatalf("capability grid size constant drifted: %d", wantGridSize)
	}

	total, agreed, accepted := 0, 0, 0
	kindHits := map[PromotionRejectionKind]int{}
	for _, c0 := range cases {
		for _, c1 := range cases {
			for _, c2 := range cases {
				for _, rollback := range []bool{false, true} {
					for _, resume := range []bool{false, true} {
						total++
						candidate := buildReleaseCandidateFor([3]capabilityCase{c0, c1, c2}, rollback, resume)
						err := candidate.CanPromote()
						rejections := candidate.PromotionRejections()
						if (len(rejections) == 0) != (err == nil) {
							t.Fatalf("disagreement at cell %v/%v/%v rollback=%v resume=%v: CanPromote=%v, %d rejections %+v", c0, c1, c2, rollback, resume, err, len(rejections), rejections)
						}
						agreed++
						if err == nil {
							accepted++
							continue
						}
						if !errors.Is(err, ErrEvidenceIncomplete) {
							t.Fatalf("unexpected refusal value at cell %v/%v/%v: %v", c0, c1, c2, err)
						}
						for _, rj := range rejections {
							kindHits[rj.Kind]++
						}
					}
				}
			}
		}
	}
	if total != wantGridSizeLiteral || agreed != wantGridSizeLiteral {
		t.Fatalf("enumerated %d cells and agreed on %d, want %d for both", total, agreed, wantGridSizeLiteral)
	}
	if accepted != 1 {
		t.Fatalf("%d cells were accepted by both, want exactly 1", accepted)
	}
	// This closure holds status and the six identity fields satisfied, so it
	// can only reach the three capability/rollback/resume classes. Asserting
	// that keeps the second closure below honest about what it must add.
	wantReachable := []PromotionRejectionKind{RejectCapabilityEvidenceMissing, RejectRollbackEvidenceMissing, RejectResumeEvidenceMissing}
	if len(kindHits) != len(wantReachable) {
		t.Fatalf("the capability closure reached %d refusal classes %v, want exactly %v", len(kindHits), kindHits, wantReachable)
	}
	for _, k := range wantReachable {
		if kindHits[k] == 0 {
			t.Fatalf("the capability closure never reached %s", k)
		}
	}
	t.Logf("capability closure: %d cells, 1 accepted, refusal-class hits %v", total, kindHits)
}

// ===========================================================================
// Two-way equivalence, closure 2: status, identity and target axes
// ===========================================================================

// identityGridCell is one cell of the second closure: every ReleaseStatus,
// the capability set present or empty, each of the six identity fields
// independently present or empty, capability targets complete or absent,
// capability evidence complete or absent, and both operational flags.
type identityGridCell struct {
	status          ReleaseStatus
	emptyCaps       bool
	identityMask    int // bit i set => identity field i is empty
	completeTargets bool
	presentEvidence bool
	rollback        bool
	resume          bool
}

const identityFieldCount = 6

func buildIdentityGridCandidate(cell identityGridCell) ReleaseCandidate {
	r := promotableFixture()
	r.Status = cell.status
	if cell.emptyCaps {
		r.Capabilities = nil
	}
	// Emptying an identity field also empties the matching evidence field, so
	// the evidence stays internally consistent and only the identity class is
	// reached.
	if cell.identityMask&1 != 0 {
		r.CandidateID = ""
		clearEvidenceCandidateID(&r)
	}
	if cell.identityMask&2 != 0 {
		r.CandidateDigest = ""
		for i := range r.Evidence {
			r.Evidence[i].CandidateDigest = ""
		}
	}
	if cell.identityMask&4 != 0 {
		r.BundleDigest = ""
		for i := range r.Evidence {
			r.Evidence[i].BundleDigest = ""
		}
	}
	if cell.identityMask&8 != 0 {
		r.ContractDigest = ""
		for i := range r.Evidence {
			r.Evidence[i].ContractDigest = ""
		}
	}
	if cell.identityMask&16 != 0 {
		r.DocsDigest = ""
		for i := range r.Evidence {
			r.Evidence[i].DocsDigest = ""
		}
	}
	if cell.identityMask&32 != 0 {
		r.EvidenceDigest = ""
	}
	if !cell.completeTargets {
		r.CapabilityTargets = map[string]CapabilityTarget{}
	}
	if !cell.presentEvidence {
		r.Evidence = nil
	}
	r.RollbackEvidence = cell.rollback
	r.ResumeEvidence = cell.resume
	return r
}

func allReleaseStatuses() []ReleaseStatus {
	return []ReleaseStatus{
		ReleaseAssembling, ReleasePreviewDeployed, ReleaseExercising, ReleasePromotable,
		ReleasePromoting, ReleaseStable, ReleaseRejected, ReleaseRollback,
	}
}

const wantReleaseStatusCount = 8

func TestPromotionRejectionsAgreeWithCanPromoteOverTheIdentityClosure(t *testing.T) {
	statuses := allReleaseStatuses()
	if len(statuses) != wantReleaseStatusCount {
		t.Fatalf("ReleaseStatus axis drifted: %d", len(statuses))
	}
	const identityMaskCount = 1 << identityFieldCount
	const wantGridSize = wantReleaseStatusCount * 2 * identityMaskCount * 2 * 2 * 2 * 2
	const wantGridSizeLiteral = 16384
	if wantGridSize != wantGridSizeLiteral {
		t.Fatalf("identity grid size constant drifted: %d", wantGridSize)
	}

	total, accepted := 0, 0
	kindHits := map[PromotionRejectionKind]int{}
	for _, status := range statuses {
		for _, emptyCaps := range []bool{false, true} {
			for mask := 0; mask < identityMaskCount; mask++ {
				for _, targets := range []bool{false, true} {
					for _, evidence := range []bool{false, true} {
						for _, rollback := range []bool{false, true} {
							for _, resume := range []bool{false, true} {
								total++
								cell := identityGridCell{status: status, emptyCaps: emptyCaps, identityMask: mask, completeTargets: targets, presentEvidence: evidence, rollback: rollback, resume: resume}
								candidate := buildIdentityGridCandidate(cell)
								err := candidate.CanPromote()
								rejections := candidate.PromotionRejections()
								if (len(rejections) == 0) != (err == nil) {
									t.Fatalf("disagreement at cell %+v: CanPromote=%v, %d rejections %+v", cell, err, len(rejections), rejections)
								}
								if err == nil {
									accepted++
									continue
								}
								for _, rj := range rejections {
									kindHits[rj.Kind]++
								}
							}
						}
					}
				}
			}
		}
	}
	if total != wantGridSizeLiteral {
		t.Fatalf("enumerated %d identity grid cells, want %d", total, wantGridSizeLiteral)
	}
	// Exactly two cells are promotable: status exercising or promotable, with
	// every other axis at its satisfied value.
	if accepted != 2 {
		t.Fatalf("%d identity grid cells were accepted by both, want exactly 2", accepted)
	}
	// Together with the capability closure this reaches every refusal class.
	for _, k := range AllPromotionRejectionKinds() {
		if kindHits[k] == 0 {
			t.Fatalf("the identity closure never reached refusal class %s", k)
		}
	}
	if len(kindHits) != wantPromotionRejectionKindCount {
		t.Fatalf("the identity closure reached %d refusal classes, want all %d: %v", len(kindHits), wantPromotionRejectionKindCount, kindHits)
	}
	t.Logf("identity closure: %d cells, 2 accepted, refusal-class hits %v", total, kindHits)
}
