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
	FailureBudgetExceeded    FailureClass = "budget-exceeded"
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
