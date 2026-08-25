package domain

import (
	"errors"
	"fmt"
)

type ReleaseStatus string

const (
	ReleaseAssembling      ReleaseStatus = "assembling"
	ReleasePreviewDeployed ReleaseStatus = "preview-deployed"
	ReleaseExercising      ReleaseStatus = "exercising"
	ReleasePromotable      ReleaseStatus = "promotable"
	ReleasePromoting       ReleaseStatus = "promoting"
	ReleaseStable          ReleaseStatus = "stable"
	ReleaseRejected        ReleaseStatus = "rejected"
	ReleaseRollback        ReleaseStatus = "rollback"
)

type CapabilityEvidence struct {
	Capability      string
	CandidateID     ReleaseID
	Digest          string
	CandidateDigest string
	BundleDigest    string
	ContractDigest  string
	DocsDigest      string
	Verified        bool
	Provider        string
	Target          string
	Fresh           bool
}

type CapabilityTarget struct {
	Target   string
	Provider string
}

type ReleaseCandidate struct {
	ID                      ReleaseID
	Version                 Version
	Status                  ReleaseStatus
	Capabilities            []string
	Evidence                []CapabilityEvidence
	RollbackEvidence        bool
	ResumeEvidence          bool
	ExpectedControlRevision Revision
	FencingToken            FencingToken
	CandidateID             ReleaseID
	CandidateDigest         string
	BundleDigest            string
	ContractDigest          string
	DocsDigest              string
	EvidenceDigest          string
	CapabilityTargets       map[string]CapabilityTarget
}

func (r ReleaseCandidate) Clone() ReleaseCandidate {
	n := r
	n.Capabilities = append([]string(nil), r.Capabilities...)
	n.Evidence = append([]CapabilityEvidence(nil), r.Evidence...)
	if r.CapabilityTargets != nil {
		n.CapabilityTargets = make(map[string]CapabilityTarget, len(r.CapabilityTargets))
		for k, v := range r.CapabilityTargets {
			n.CapabilityTargets[k] = v
		}
	}
	return n
}

func (r ReleaseCandidate) CanPromote() error {
	if r.Status != ReleaseExercising && r.Status != ReleasePromotable {
		return fmt.Errorf("release is not ready for promotion: %s", r.Status)
	}
	if len(r.Capabilities) == 0 {
		return ErrEvidenceIncomplete
	}
	if r.CandidateID == "" || r.CandidateDigest == "" || r.BundleDigest == "" || r.ContractDigest == "" || r.DocsDigest == "" || r.EvidenceDigest == "" {
		return ErrEvidenceIncomplete
	}
	for _, capability := range r.Capabilities {
		target, ok := r.CapabilityTargets[capability]
		if !ok || target.Target == "" || target.Provider == "" {
			return fmt.Errorf("%w: capability target %q", ErrEvidenceIncomplete, capability)
		}
	}
	seen := make(map[string]bool, len(r.Evidence))
	for _, evidence := range r.Evidence {
		if evidence.Capability == "" || evidence.CandidateID != r.CandidateID || evidence.CandidateDigest != r.CandidateDigest || evidence.Digest == "" || evidence.BundleDigest != r.BundleDigest || evidence.ContractDigest != r.ContractDigest || evidence.DocsDigest != r.DocsDigest || !evidence.Verified || !evidence.Fresh || evidence.Target == "" || evidence.Provider == "" {
			continue
		}
		if requirement, ok := r.CapabilityTargets[evidence.Capability]; ok && (requirement.Target != evidence.Target || requirement.Provider != evidence.Provider) {
			continue
		}
		seen[evidence.Capability] = true
	}
	for _, capability := range r.Capabilities {
		if !seen[capability] {
			return fmt.Errorf("%w: capability %q", ErrEvidenceIncomplete, capability)
		}
	}
	if !r.RollbackEvidence || !r.ResumeEvidence {
		return ErrEvidenceIncomplete
	}
	return nil
}

type stableReleaseProofData struct {
	candidateID    ReleaseID
	version        Version
	bundleDigest   string
	evidenceDigest string
	fence          FencingToken
	revision       Revision
}
type StableReleaseProof struct{ data *stableReleaseProofData }

func (p StableReleaseProof) valid() bool {
	return p.data != nil && p.data.candidateID != "" && p.data.bundleDigest != "" && p.data.evidenceDigest != ""
}

func PromoteRelease(current ReleaseCandidate, control EffectiveControlResult) (ReleaseCandidate, error) {
	if err := current.CanPromote(); err != nil {
		return current, err
	}
	if control.Mode == "" {
		control.Mode = ControlAllow
	}
	if control.Mode != ControlAllow || !control.Found || control.Revision != current.ExpectedControlRevision {
		return current, ErrControlDenied
	}
	next := current.Clone()
	next.Status = ReleasePromoting
	return next, nil
}

func CompletePromotion(current ReleaseCandidate, control EffectiveControlResult, permit PermitDecision) (ReleaseCandidate, error) {
	if current.Status != ReleasePromoting || !permit.Allowed() || permit.Kind() != PermitPromotion || permit.Revision() != current.ExpectedControlRevision || permit.FencingToken() != current.FencingToken {
		return current, ErrControlDenied
	}
	if !control.Found || control.Mode != ControlAllow || control.Revision != current.ExpectedControlRevision {
		return current, ErrControlDenied
	}
	next := current.Clone()
	next.Status = ReleaseStable
	return next, nil
}

func CompletePromotionWithProof(current ReleaseCandidate, control EffectiveControlResult, permit PermitDecision) (ReleaseCandidate, StableReleaseProof, error) {
	next, err := CompletePromotion(current, control, permit)
	if err != nil {
		return current, StableReleaseProof{}, err
	}
	proof := StableReleaseProof{data: &stableReleaseProofData{candidateID: next.CandidateID, version: next.Version, bundleDigest: next.BundleDigest, evidenceDigest: next.EvidenceDigest, fence: next.FencingToken, revision: next.ExpectedControlRevision}}
	return next.Clone(), proof, nil
}

func PromoteReleaseWithPermit(current ReleaseCandidate, control EffectiveControlResult, permit PermitDecision) (ReleaseCandidate, StableReleaseProof, error) {
	if err := current.CanPromote(); err != nil {
		return current, StableReleaseProof{}, err
	}
	if !permit.Allowed() || permit.Kind() != PermitPromotion || !control.Found || control.Mode != ControlAllow || control.Revision != current.ExpectedControlRevision || permit.Revision() != current.ExpectedControlRevision || permit.FencingToken() != current.FencingToken {
		return current, StableReleaseProof{}, ErrControlDenied
	}
	next := current.Clone()
	next.Status = ReleasePromoting
	return next, StableReleaseProof{}, nil
}

type FailureClass string

const (
	FailureInvalidInput      FailureClass = "invalid-input"
	FailurePolicyDenied      FailureClass = "policy-denied"
	FailureCapacity          FailureClass = "capacity-unavailable"
	FailureProviderTransport FailureClass = "provider-transport"
	FailureProviderModel     FailureClass = "provider-model"
	FailureProviderQuota     FailureClass = "provider-quota"
	FailureExecutionLost     FailureClass = "execution-lost"
	FailureProgressStalled   FailureClass = "progress-stalled"
	FailureVerification      FailureClass = "verification-failed"
	FailureExternalAmbiguous FailureClass = "external-ambiguous"
	FailureIntegration       FailureClass = "integration-conflict"
	FailurePreviewRegression FailureClass = "preview-regression"
	FailurePromotionPartial  FailureClass = "promotion-partial"
	FailureSecretSuspected   FailureClass = "secret-suspected"
	FailureBudgetExceeded    FailureClass = "budget-exceeded"
	FailureContractIncompat  FailureClass = "contract-incompatible"
	FailureUnknown           FailureClass = "unknown"
)

type RetryBudget struct {
	MaxAttempts        uint32
	Attempts           uint32
	LastFingerprint    string
	SameFingerprint    uint32
	MaxSameFingerprint uint32
	LastApproach       string
	SameApproach       uint32
	MaxSameApproach    uint32
}

func (b RetryBudget) Allow(fingerprint, approach string) bool {
	if b.MaxAttempts == 0 || b.Attempts >= b.MaxAttempts {
		return false
	}
	if b.MaxSameFingerprint > 0 && fingerprint == b.LastFingerprint && b.SameFingerprint >= b.MaxSameFingerprint {
		return false
	}
	if b.MaxSameApproach > 0 && approach == b.LastApproach && b.SameApproach >= b.MaxSameApproach {
		return false
	}
	return true
}

func (b RetryBudget) Consume(fingerprint, approach string) (RetryBudget, error) {
	if !b.Allow(fingerprint, approach) {
		return b, ErrBudgetExhausted
	}
	next := b
	next.Attempts++
	if fingerprint == b.LastFingerprint {
		next.SameFingerprint++
	} else {
		next.LastFingerprint = fingerprint
		next.SameFingerprint = 1
	}
	if approach == b.LastApproach {
		next.SameApproach++
	} else {
		next.LastApproach = approach
		next.SameApproach = 1
	}
	return next, nil
}

func ValidateRelease(r ReleaseCandidate) error {
	if _, err := NewReleaseID(r.ID.String()); err != nil {
		return err
	}
	if r.Version == 0 {
		return errors.New("release version must be positive")
	}
	if r.Status == ReleaseStable || r.Status == ReleasePromoting || r.Status == ReleasePromotable {
		if r.CandidateID == "" || r.CandidateDigest == "" || r.BundleDigest == "" || r.ContractDigest == "" || r.DocsDigest == "" || r.EvidenceDigest == "" {
			return ErrEvidenceIncomplete
		}
	}
	return nil
}

// ===========================================================================
// Structured promotion refusals (V2-066).
// ===========================================================================
//
// CanPromote above is the authority and is deliberately left byte-for-byte
// unchanged by V2-066: it returns the FIRST refusal it finds, as a single
// error value, and eight of its twelve distinct refusal classes collapse
// onto the same bare ErrEvidenceIncomplete value. That shape is right for a
// transition guard (a candidate is either promotable or it is not) and wrong
// for a report: an owner reading "evidence is incomplete" cannot tell an
// empty capability set from a missing DocsDigest from missing rollback
// evidence, and cannot see the second refusal at all.
//
// PromotionRejections therefore enumerates every refusal, each pinned to its
// own PromotionRejectionKind, without touching CanPromote. The two are
// proven equivalent -- len(PromotionRejections()) == 0 if and only if
// CanPromote() == nil -- by exhaustive enumeration over a closed grid in
// release_test.go, not by construction and not by sampling. The predicates
// below are deliberately a second expression of the same rules rather than a
// refactor of CanPromote's body, because refactoring the body would change
// the authority; the equivalence enumeration is what keeps the second
// expression honest.
//
// A single-capability projection of the candidate (cloning the candidate with
// one capability in Capabilities and calling CanPromote on it) was measured
// and rejected as the mechanism: projecting an empty capability set yields
// nil, reporting a candidate that declares nothing as fully met, and every
// projection still collapses the eight ErrEvidenceIncomplete classes.

// PromotionRejectionKind names one class of promotion refusal. The twelve
// kinds are exactly the twelve distinct refusals CanPromote can reach, one
// kind per reachable refusal, so two refusals that CanPromote reports with
// the same error value are still distinguishable here.
type PromotionRejectionKind string

const (
	// RejectStatusNotPromotable is CanPromote's first check: the candidate's
	// status is neither ReleaseExercising nor ReleasePromotable.
	RejectStatusNotPromotable PromotionRejectionKind = "status-not-promotable"
	// RejectEmptyCapabilitySet is CanPromote's second check. It is reported
	// as its own kind because a candidate that declares no capability is not
	// the same fact as a candidate whose declared capabilities lack
	// evidence, even though CanPromote returns the same error value for
	// both.
	RejectEmptyCapabilitySet PromotionRejectionKind = "empty-capability-set"
	// The next six kinds split CanPromote's single six-field identity check
	// into one kind per field.
	RejectCandidateIDMissing     PromotionRejectionKind = "candidate-id-missing"
	RejectCandidateDigestMissing PromotionRejectionKind = "candidate-digest-missing"
	RejectBundleDigestMissing    PromotionRejectionKind = "bundle-digest-missing"
	RejectContractDigestMissing  PromotionRejectionKind = "contract-digest-missing"
	RejectDocsDigestMissing      PromotionRejectionKind = "docs-digest-missing"
	RejectEvidenceDigestMissing  PromotionRejectionKind = "evidence-digest-missing"
	// RejectCapabilityTargetMissing and RejectCapabilityEvidenceMissing are
	// per-capability: the Capability field names which one.
	RejectCapabilityTargetMissing   PromotionRejectionKind = "capability-target-missing"
	RejectCapabilityEvidenceMissing PromotionRejectionKind = "capability-evidence-missing"
	// The last two kinds split CanPromote's combined
	// !RollbackEvidence || !ResumeEvidence check.
	RejectRollbackEvidenceMissing PromotionRejectionKind = "rollback-evidence-missing"
	RejectResumeEvidenceMissing   PromotionRejectionKind = "resume-evidence-missing"
)

// AllPromotionRejectionKinds returns every kind, in refusal order. Callers
// derive the number of refusal classes from this function rather than
// hardcoding it.
func AllPromotionRejectionKinds() []PromotionRejectionKind {
	return []PromotionRejectionKind{
		RejectStatusNotPromotable,
		RejectEmptyCapabilitySet,
		RejectCandidateIDMissing,
		RejectCandidateDigestMissing,
		RejectBundleDigestMissing,
		RejectContractDigestMissing,
		RejectDocsDigestMissing,
		RejectEvidenceDigestMissing,
		RejectCapabilityTargetMissing,
		RejectCapabilityEvidenceMissing,
		RejectRollbackEvidenceMissing,
		RejectResumeEvidenceMissing,
	}
}

// PromotionRejection is one refusal: its kind, the capability it concerns
// (empty for the candidate-wide kinds) and a reason in plain prose. No
// digest value is carried here; a missing digest is reported by field name.
type PromotionRejection struct {
	Kind       PromotionRejectionKind
	Capability string
	Reason     string
}

// PromotionRejections returns every reason this candidate cannot be
// promoted, in refusal order, with per-capability refusals in the
// candidate's own declared capability order. The result is deterministic:
// no map is iterated to produce it.
//
// It is a read: it mutates nothing and reads no clock.
func (r ReleaseCandidate) PromotionRejections() []PromotionRejection {
	var out []PromotionRejection
	add := func(kind PromotionRejectionKind, capability, reason string) {
		out = append(out, PromotionRejection{Kind: kind, Capability: capability, Reason: reason})
	}

	if r.Status != ReleaseExercising && r.Status != ReleasePromotable {
		add(RejectStatusNotPromotable, "", fmt.Sprintf("candidate status is %q; promotion is only considered from %q or %q", string(r.Status), string(ReleaseExercising), string(ReleasePromotable)))
	}
	if len(r.Capabilities) == 0 {
		add(RejectEmptyCapabilitySet, "", "the candidate declares no capability at all, so there is nothing to promote")
	}
	for _, field := range []struct {
		kind  PromotionRejectionKind
		name  string
		value string
	}{
		{RejectCandidateIDMissing, "CandidateID", string(r.CandidateID)},
		{RejectCandidateDigestMissing, "CandidateDigest", r.CandidateDigest},
		{RejectBundleDigestMissing, "BundleDigest", r.BundleDigest},
		{RejectContractDigestMissing, "ContractDigest", r.ContractDigest},
		{RejectDocsDigestMissing, "DocsDigest", r.DocsDigest},
		{RejectEvidenceDigestMissing, "EvidenceDigest", r.EvidenceDigest},
	} {
		if field.value == "" {
			add(field.kind, "", fmt.Sprintf("candidate identity field %s is empty, so the candidate is not bound to one assembled version", field.name))
		}
	}
	for _, capability := range r.Capabilities {
		target, ok := r.CapabilityTargets[capability]
		if !ok || target.Target == "" || target.Provider == "" {
			add(RejectCapabilityTargetMissing, capability, "no capability target declaring both a target and a Provider is recorded, so which real external system and Provider exercised it is unknown")
		}
	}
	seen := make(map[string]bool, len(r.Evidence))
	for _, evidence := range r.Evidence {
		if evidence.Capability == "" || evidence.CandidateID != r.CandidateID || evidence.CandidateDigest != r.CandidateDigest || evidence.Digest == "" || evidence.BundleDigest != r.BundleDigest || evidence.ContractDigest != r.ContractDigest || evidence.DocsDigest != r.DocsDigest || !evidence.Verified || !evidence.Fresh || evidence.Target == "" || evidence.Provider == "" {
			continue
		}
		if requirement, ok := r.CapabilityTargets[evidence.Capability]; ok && (requirement.Target != evidence.Target || requirement.Provider != evidence.Provider) {
			continue
		}
		seen[evidence.Capability] = true
	}
	for _, capability := range r.Capabilities {
		if !seen[capability] {
			add(RejectCapabilityEvidenceMissing, capability, "no verified, fresh evidence record binds this capability to the candidate's own digests and declared target")
		}
	}
	if !r.RollbackEvidence {
		add(RejectRollbackEvidenceMissing, "", "rollback was not confirmed for this candidate")
	}
	if !r.ResumeEvidence {
		add(RejectResumeEvidenceMissing, "", "resumption on Stable after a stop was not confirmed for this candidate")
	}
	return out
}
