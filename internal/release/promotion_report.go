// Promotion observability for the M5 local Release Candidate (V2-066).
//
// This file turns an assembled candidate plus its source facts into a
// structured, tri-state report of the eight promotion conditions of
// docs/architecture/release-contract.md section 4, so an owner can read
// which conditions are still unmet and why through the control plane rather
// than inferring it from the repository tree.
//
// Three rules shape it:
//
//   - It adds no rule. Every decision is delegated: the candidate/evidence
//     conditions to domain.ReleaseCandidate.PromotionRejections (the
//     authority, whose CanPromote this task leaves byte-for-byte unchanged),
//     the documentation conditions to the verifiers already in docs.go, and
//     the single-version condition to the seven bundle roles plus
//     VerifySource, VerifyCandidateDigests and VerifyCandidateAgainstContract.
//
//   - It never inflates an unmeasurable condition into a met one. Conditions
//     1 and 5 are reported not-observable-here with an explicit reason, and
//     the aggregate promotable flag is true only when all eight are met, so a
//     not-observable-here condition keeps it false.
//
//   - It never hides the real tree's negative answer. The Foundation
//     candidate verification has not satisfied every baseline capability,
//     so condition 2 is unmet for all twelve and the candidate is not
//     promotable. That is the correct answer and is reported as such.
//
// Like the rest of this package the file imports only the standard library
// and internal/domain, and reads no clock: the assembly instant is injected.
package release

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ConditionState is the tri-state a promotion condition is reported in.
// not-observable-here is a distinct third value on purpose: collapsing it
// into unmet would say the condition failed, and collapsing it into met
// would claim a measurement no in-process source made.
type ConditionState string

const (
	ConditionMet               ConditionState = "met"
	ConditionUnmet             ConditionState = "unmet"
	ConditionNotObservableHere ConditionState = "not-observable-here"
)

// ConditionID names one of the eight numbered promotion conditions of
// release-contract.md section 4. The numeric prefix is part of the id so a
// reader can line the report up with the contract without a lookup table.
type ConditionID string

const (
	ConditionDeterministicVerification ConditionID = "1-deterministic-tests-and-static-verification"
	ConditionCapabilitiesExercised     ConditionID = "2-every-contract-capability-succeeded-in-preview"
	ConditionRealExternalSystems       ConditionID = "3-succeeded-on-the-real-external-systems-and-providers"
	ConditionStopRollbackResume        ConditionID = "4-stop-rollback-and-resume-on-stable-confirmed"
	ConditionNoUnresolvedIncident      ConditionID = "5-no-unresolved-major-failure-leak-or-unapproved-cost"
	ConditionPreviewDocsComplete       ConditionID = "6-preview-documentation-describes-every-capability"
	ConditionStableDiffDocumented      ConditionID = "7-stable-diff-migration-and-rollback-documented"
	ConditionOneVersion                ConditionID = "8-implementation-schema-migration-configuration-and-documentation-as-one-version"
)

// AllConditionIDs returns the eight condition ids in contract order. Callers
// derive the number of conditions from this function rather than hardcoding
// eight.
func AllConditionIDs() []ConditionID {
	return []ConditionID{
		ConditionDeterministicVerification,
		ConditionCapabilitiesExercised,
		ConditionRealExternalSystems,
		ConditionStopRollbackResume,
		ConditionNoUnresolvedIncident,
		ConditionPreviewDocsComplete,
		ConditionStableDiffDocumented,
		ConditionOneVersion,
	}
}

// contractWording carries each condition's own sentence from
// release-contract.md section 4, verbatim, so the report shows the contract's
// text instead of a paraphrase and no layer above has to restate it.
var contractWording = map[ConditionID]string{
	ConditionDeterministicVerification: "決定的な自動testと静的検証が成功した",
	ConditionCapabilitiesExercised:     "Release Contractの全capabilityがPreview実環境で成功した",
	ConditionRealExternalSystems:       "対象となる実外部systemとAI Providerで成功した",
	ConditionStopRollbackResume:        "stop、rollback、Stableによる再開を確認した",
	ConditionNoUnresolvedIncident:      "未解決の重大な障害、秘密漏洩、許可されない費用がない",
	ConditionPreviewDocsComplete:       "Preview利用者文書が候補versionの全機能を説明している",
	ConditionStableDiffDocumented:      "Stableとの差分、migration、rollback方法が説明されている",
	ConditionOneVersion:                "実装、schema、migration、設定、利用者文書を同じversionとして昇格できる",
}

// conditionForRejectionKind attributes every one of the authority's twelve
// refusal classes to exactly one condition. A test asserts the map is total
// over domain.AllPromotionRejectionKinds() and lands only in declared
// condition ids, so a new refusal class cannot appear in the domain without
// being attributed here.
var conditionForRejectionKind = map[domain.PromotionRejectionKind]ConditionID{
	// A candidate that declares nothing, or a declared capability with no
	// verified fresh evidence, is condition 2: not every contract capability
	// succeeded in the Preview environment.
	domain.RejectEmptyCapabilitySet:        ConditionCapabilitiesExercised,
	domain.RejectCapabilityEvidenceMissing: ConditionCapabilitiesExercised,
	// The capability target is what records which real external system and
	// which Provider exercised the capability, so its absence is condition 3.
	domain.RejectCapabilityTargetMissing: ConditionRealExternalSystems,
	// The two operational flags are exactly condition 4's three observations.
	domain.RejectRollbackEvidenceMissing: ConditionStopRollbackResume,
	domain.RejectResumeEvidenceMissing:   ConditionStopRollbackResume,
	// Status and the six identity fields are what bind implementation,
	// schema, migration, configuration and documentation to one version, so
	// they are condition 8.
	domain.RejectStatusNotPromotable:    ConditionOneVersion,
	domain.RejectCandidateIDMissing:     ConditionOneVersion,
	domain.RejectCandidateDigestMissing: ConditionOneVersion,
	domain.RejectBundleDigestMissing:    ConditionOneVersion,
	domain.RejectContractDigestMissing:  ConditionOneVersion,
	domain.RejectDocsDigestMissing:      ConditionOneVersion,
	domain.RejectEvidenceDigestMissing:  ConditionOneVersion,
}

// ConditionForRejectionKind returns the condition a refusal class belongs to.
func ConditionForRejectionKind(kind domain.PromotionRejectionKind) (ConditionID, bool) {
	id, ok := conditionForRejectionKind[kind]
	return id, ok
}

// The three Preview documents whose fixed-format content decides conditions 6
// and 7. Their paths are fixed by dp-v2-021 d10 and by
// release-contract.md section 5; a missing one is reported as an unmet
// condition, never skipped.
const (
	PreviewIndexDoc        = "docs/preview/index.md"
	PreviewCapabilitiesDoc = "docs/preview/capabilities.md"
	PreviewStableDiffDoc   = "docs/preview/stable-diff.md"
)

// NotObservableInProcess names, for the payload, every fact this surface
// cannot measure from inside the running process. The first five are the
// initial deploy gate D1's subject matter (release-contract.md section 3
// "Preview実環境の等級"): a process running on the owner's machine can
// observe neither the Cloud Run revision serving it nor the image it was
// deployed from, and preview-local is explicitly not a substitute for
// Cloud Run and IAP. Reporting the list is how the surface avoids implying
// otherwise by silence.
var NotObservableInProcess = []string{
	"cloud-run-running-revision",
	"deployed-image-digest",
	"deploy-path",
	"iap-authentication-boundary",
	"scale-to-zero",
	"real-firestore-permissions-and-contention",
}

// ResidualCapabilityGaps names the parts of cap-preview-operation and
// cap-stable-promotion that remain unwired after this surface exists. They
// are carried in the payload so reading the surface can never be mistaken
// for the capability having been exercised.
var ResidualCapabilityGaps = []string{
	"cap-preview-operation: the automatic Preview-failure response is not wired. Nothing stops new claims and returns routing to Stable on a Preview failure; this surface only reports the rollback target and the recorded rollback history.",
	"cap-stable-promotion: automatic promotion execution is not wired. Nothing promotes a candidate when the eight conditions are met; this surface only reports which conditions are still unmet.",
}

// ConditionReport is one condition's tri-state answer, its reason, the named
// sources that decided it, and (for the conditions the authority decides)
// the refusal records attributed to it.
type ConditionReport struct {
	ID         ConditionID
	Contract   string
	State      ConditionState
	Reason     string
	DecidedBy  []string
	Rejections []domain.PromotionRejection
}

// PromotionReport is the whole observable surface of the release machinery
// for one assembled candidate.
type PromotionReport struct {
	ReleaseVersion   string
	CandidateID      string
	CandidateDigest  string
	BundleDigest     string
	ContractDigest   string
	DocsDigest       string
	EvidenceDigest   string
	AssembledAt      time.Time
	EnvironmentClass string

	DeclaredCapabilities        []string
	CapabilitiesWithoutEvidence []string

	Conditions []ConditionReport
	Promotable bool

	// Rejections is the authority's complete, unfiltered enumeration, kept
	// alongside the per-condition attribution so nothing is lost by grouping.
	Rejections []domain.PromotionRejection

	NotObserved  []string
	ResidualGaps []string
}

// ReportInput is everything BuildPromotionReport needs. AssembledAt is
// injected; nothing in this file reads a clock.
type ReportInput struct {
	Root             string
	Candidate        domain.ReleaseCandidate
	Assembled        AssembledBundle
	AssembledAt      time.Time
	EnvironmentClass string
}

// Promotable reports whether every one of the eight declared conditions is
// present exactly once and met. It is deliberately a total function over the
// condition list rather than a field computed inline, so the "true only when
// all eight are met" rule can be enumerated exhaustively in a test.
func Promotable(conditions []ConditionReport) bool {
	ids := AllConditionIDs()
	if len(conditions) != len(ids) {
		return false
	}
	seen := map[ConditionID]bool{}
	for _, c := range conditions {
		if c.State != ConditionMet {
			return false
		}
		if seen[c.ID] {
			return false
		}
		seen[c.ID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			return false
		}
	}
	return true
}

// documentationMembers splits the assembled documentation role into the
// Preview and Stable document sets. The sets are derived from the members
// actually read out of the source tree, never from a second hardcoded list.
func documentationMembers(members []Member) (preview, stable []string) {
	for _, m := range members {
		if m.Role != RoleDocumentation {
			continue
		}
		switch {
		case strings.HasPrefix(m.Path, "docs/preview/"):
			preview = append(preview, m.Path)
		case strings.HasPrefix(m.Path, "docs/stable/"):
			stable = append(stable, m.Path)
		}
	}
	sort.Strings(preview)
	sort.Strings(stable)
	return preview, stable
}

func hasMember(members []Member, path string) bool {
	for _, m := range members {
		if m.Path == path {
			return true
		}
	}
	return false
}

// verdict is one named check inside a condition: the function that decided it
// and the error it returned, if any.
type verdict struct {
	source string
	err    error
}

// stateFromVerdicts folds a condition's checks into a tri-state answer plus a
// reason naming the first failing source. Every source is recorded in
// DecidedBy whether it passed or failed, so the report says what decided a
// met condition too.
func stateFromVerdicts(verdicts []verdict, metReason string) (ConditionState, string, []string) {
	sources := make([]string, 0, len(verdicts))
	for _, v := range verdicts {
		sources = append(sources, v.source)
	}
	for _, v := range verdicts {
		if v.err != nil {
			return ConditionUnmet, fmt.Sprintf("%s refused: %v", v.source, v.err), sources
		}
	}
	return ConditionMet, metReason, sources
}

// BuildPromotionReport assembles the tri-state report. It reads the three
// fixed-format Preview documents and re-verifies the recorded members against
// the source tree, so it performs filesystem access: callers cache the result
// and do not rebuild it per request.
func BuildPromotionReport(in ReportInput) (PromotionReport, error) {
	if strings.TrimSpace(in.Root) == "" {
		return PromotionReport{}, fmt.Errorf("promotion report requires an explicitly configured source root")
	}
	if in.AssembledAt.IsZero() {
		return PromotionReport{}, fmt.Errorf("promotion report requires the instant the snapshot was assembled")
	}
	if strings.TrimSpace(in.EnvironmentClass) == "" {
		return PromotionReport{}, fmt.Errorf("promotion report requires a declared Preview environment class")
	}
	if len(in.Assembled.Members) == 0 {
		return PromotionReport{}, fmt.Errorf("promotion report requires an assembled bundle")
	}

	rejections := in.Candidate.PromotionRejections()
	byCondition := map[ConditionID][]domain.PromotionRejection{}
	for _, rj := range rejections {
		id, ok := ConditionForRejectionKind(rj.Kind)
		if !ok {
			return PromotionReport{}, fmt.Errorf("refusal class %q is not attributed to any promotion condition", rj.Kind)
		}
		byCondition[id] = append(byCondition[id], rj)
	}

	previewDocs, stableDocs := documentationMembers(in.Assembled.Members)
	allDocs := append(append([]string(nil), previewDocs...), stableDocs...)

	readDoc := func(rel string) (string, error) {
		if !hasMember(in.Assembled.Members, rel) {
			return "", fmt.Errorf("%w: %s is not an assembled documentation member", ErrDeclaredMemberMissing, rel)
		}
		data, err := os.ReadFile(filepath.Join(in.Root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	previewIndex, previewIndexErr := readDoc(PreviewIndexDoc)
	capabilitiesDoc, capabilitiesErr := readDoc(PreviewCapabilitiesDoc)
	stableDiff, stableDiffErr := readDoc(PreviewStableDiffDoc)

	firstErr := func(readErr error, check func() error) error {
		if readErr != nil {
			return readErr
		}
		return check()
	}

	conditions := []ConditionReport{
		{
			ID:        ConditionDeterministicVerification,
			Contract:  contractWording[ConditionDeterministicVerification],
			State:     ConditionNotObservableHere,
			Reason:    "the deterministic test and static-verification result is produced by `make check` in CI against a commit; a running process holds no record of that run, so this surface reports it as unobservable rather than inferring it from its own successful start",
			DecidedBy: []string{"not-observable-in-process: make check (CI)"},
		},
		conditionFromRejections(ConditionCapabilitiesExercised, byCondition,
			"every declared capability carries verified, fresh evidence bound to this candidate's own digests",
			"domain.ReleaseCandidate.PromotionRejections"),
		conditionFromRejections(ConditionRealExternalSystems, byCondition,
			"every declared capability records the target and Provider it was exercised against",
			"domain.ReleaseCandidate.PromotionRejections"),
		conditionFromRejections(ConditionStopRollbackResume, byCondition,
			"rollback and resumption on Stable are both recorded for this candidate",
			"domain.ReleaseCandidate.PromotionRejections"),
		{
			ID:        ConditionNoUnresolvedIncident,
			Contract:  contractWording[ConditionNoUnresolvedIncident],
			State:     ConditionNotObservableHere,
			Reason:    "an unresolved major failure, a secret leak or an unapproved cost is recorded in an incident and secret-scan ledger; no in-process source holds that ledger, so this surface reports it as unobservable rather than asserting the absence of something it cannot see",
			DecidedBy: []string{"not-observable-in-process: incident and secret-scan ledger"},
		},
		docCondition(ConditionPreviewDocsComplete, []verdict{
			{source: "release.VerifyPreviewReleaseMarker", err: firstErr(previewIndexErr, func() error {
				return VerifyPreviewReleaseMarker(previewIndex, in.Assembled.Contract.Version)
			})},
			{source: "release.VerifyCapabilityAnchorBijection", err: firstErr(capabilitiesErr, func() error {
				return VerifyCapabilityAnchorBijection(capabilitiesDoc, in.Assembled.Contract.Capabilities)
			})},
			{source: "release.VerifyLinksResolve", err: VerifyLinksResolve(in.Root, allDocs)},
		}, "the Preview index declares this release once, the capability anchors are a bijection onto the contract's capability ids, and every relative link in the document set resolves"),
		docCondition(ConditionStableDiffDocumented, []verdict{
			{source: "release.VerifyRequiredSections over release.RequiredPreviewSections", err: firstErr(stableDiffErr, func() error {
				return VerifyRequiredSections(stableDiff, RequiredPreviewSections)
			})},
			{source: "release.VerifyNoStableToPreviewLinks", err: VerifyNoStableToPreviewLinks(in.Root, stableDocs)},
		}, "the Preview diff document carries all four required sections and no Stable document links into Preview"),
		oneVersionCondition(in, byCondition),
	}

	capsWithoutEvidence := make([]string, 0, len(in.Candidate.Capabilities))
	for _, rj := range byCondition[ConditionCapabilitiesExercised] {
		if rj.Kind == domain.RejectCapabilityEvidenceMissing {
			capsWithoutEvidence = append(capsWithoutEvidence, rj.Capability)
		}
	}

	report := PromotionReport{
		ReleaseVersion:              in.Assembled.Contract.Version,
		CandidateID:                 string(in.Candidate.CandidateID),
		CandidateDigest:             in.Candidate.CandidateDigest,
		BundleDigest:                in.Candidate.BundleDigest,
		ContractDigest:              in.Candidate.ContractDigest,
		DocsDigest:                  in.Candidate.DocsDigest,
		EvidenceDigest:              in.Candidate.EvidenceDigest,
		AssembledAt:                 in.AssembledAt.UTC(),
		EnvironmentClass:            in.EnvironmentClass,
		DeclaredCapabilities:        append([]string(nil), in.Candidate.Capabilities...),
		CapabilitiesWithoutEvidence: capsWithoutEvidence,
		Conditions:                  conditions,
		Rejections:                  rejections,
		NotObserved:                 append([]string(nil), NotObservableInProcess...),
		ResidualGaps:                append([]string(nil), ResidualCapabilityGaps...),
	}
	report.Promotable = Promotable(conditions)
	if len(report.Conditions) != len(AllConditionIDs()) {
		return PromotionReport{}, fmt.Errorf("report carries %d conditions, want %d", len(report.Conditions), len(AllConditionIDs()))
	}
	return report, nil
}

// conditionFromRejections builds a condition whose whole decision is the
// authority's refusal enumeration: met when no refusal is attributed to it,
// unmet otherwise, with the refusals carried verbatim.
func conditionFromRejections(id ConditionID, byCondition map[ConditionID][]domain.PromotionRejection, metReason, decidedBy string) ConditionReport {
	rejections := byCondition[id]
	report := ConditionReport{ID: id, Contract: contractWording[id], DecidedBy: []string{decidedBy}, Rejections: rejections}
	if len(rejections) == 0 {
		report.State, report.Reason = ConditionMet, metReason
		return report
	}
	report.State = ConditionUnmet
	names := make([]string, 0, len(rejections))
	for _, rj := range rejections {
		if rj.Capability != "" {
			names = append(names, string(rj.Kind)+"("+rj.Capability+")")
			continue
		}
		names = append(names, string(rj.Kind))
	}
	report.Reason = fmt.Sprintf("%d refusal(s) from the promotion authority: %s", len(rejections), strings.Join(names, ", "))
	return report
}

func docCondition(id ConditionID, verdicts []verdict, metReason string) ConditionReport {
	state, reason, sources := stateFromVerdicts(verdicts, metReason)
	return ConditionReport{ID: id, Contract: contractWording[id], State: state, Reason: reason, DecidedBy: sources}
}

// oneVersionCondition decides condition 8 from the seven bundle roles plus
// VerifySource, VerifyCandidateDigests and VerifyCandidateAgainstContract,
// and from the candidate-identity refusal classes the authority attributes to
// this condition. No second implementation of any of those checks exists here.
func oneVersionCondition(in ReportInput, byCondition map[ConditionID][]domain.PromotionRejection) ConditionReport {
	counts := map[Role]int{}
	for _, m := range in.Assembled.Members {
		counts[m.Role]++
	}
	var roleErr error
	var emptyRoles []string
	for _, spec := range roleSpecs {
		if counts[spec.role] == 0 {
			emptyRoles = append(emptyRoles, string(spec.role))
		}
	}
	if len(emptyRoles) != 0 {
		roleErr = fmt.Errorf("%w: %s", ErrRoleHasNoMembers, strings.Join(emptyRoles, ", "))
	}

	verdicts := []verdict{
		{source: "release.roleSpecs (the seven bundle roles)", err: roleErr},
		{source: "release.VerifySource", err: VerifySource(in.Assembled.Members, in.Root)},
		{source: "release.VerifyCandidateDigests", err: VerifyCandidateDigests(in.Candidate, in.Assembled.Members)},
		{source: "release.VerifyCandidateAgainstContract", err: VerifyCandidateAgainstContract(in.Candidate, in.Assembled.Contract)},
	}
	report := docCondition(ConditionOneVersion, verdicts, "all seven bundle roles resolved, the recorded members still match the source tree, the candidate's digests are the source-derived ones and its capability set is exactly the contract's")
	report.Rejections = byCondition[ConditionOneVersion]
	if report.State == ConditionMet && len(report.Rejections) != 0 {
		names := make([]string, 0, len(report.Rejections))
		for _, rj := range report.Rejections {
			names = append(names, string(rj.Kind))
		}
		report.State = ConditionUnmet
		report.Reason = fmt.Sprintf("the source tree agrees with the recorded bundle, but the candidate is not bound to one version: %s", strings.Join(names, ", "))
	}
	report.DecidedBy = append(report.DecidedBy, "domain.ReleaseCandidate.PromotionRejections")
	return report
}
