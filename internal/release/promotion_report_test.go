package release

// Tests for the tri-state promotion report (V2-066).
//
// Determinism: every timestamp is injected, no test sleeps, starts a
// goroutine or reads the wall clock, and every enumeration below is over a
// closed set whose cell count is asserted against a named constant.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// injectedAssemblyInstant is the single injected timestamp every test here
// uses for "the instant the snapshot was assembled".
var injectedAssemblyInstant = time.Unix(1_700_000_000, 0).UTC()

const testEnvironmentClass = "preview-local"

// --- the eight conditions ---------------------------------------------------

func TestPromotionConditionsAreTheEightContractConditions(t *testing.T) {
	ids := AllConditionIDs()
	const wantConditionCount = 8
	if len(ids) != wantConditionCount {
		t.Fatalf("AllConditionIDs returned %d ids, want %d", len(ids), wantConditionCount)
	}
	seen := map[ConditionID]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("condition id %q is declared twice", id)
		}
		seen[id] = true
		if !strings.HasPrefix(string(id), "") || id == "" {
			t.Fatalf("condition id is empty")
		}
		if contractWording[id] == "" {
			t.Fatalf("condition %q carries no contract wording", id)
		}
	}
}

// TestEveryRefusalClassIsAttributedToExactlyOneCondition is the totality
// check that keeps the report honest when the authority grows a new refusal
// class: the attribution map must cover every kind the domain declares, and
// must land only on a declared condition id. The matcher itself is verified
// with a known-negative (an unattributed kind must be reported as unknown,
// not silently mapped).
func TestEveryRefusalClassIsAttributedToExactlyOneCondition(t *testing.T) {
	kinds := domain.AllPromotionRejectionKinds()
	if len(kinds) == 0 {
		t.Fatal("domain declares zero refusal classes; this assertion must not pass vacuously")
	}
	declared := map[ConditionID]bool{}
	for _, id := range AllConditionIDs() {
		declared[id] = true
	}
	hit := map[ConditionID]int{}
	for _, kind := range kinds {
		id, ok := ConditionForRejectionKind(kind)
		if !ok {
			t.Fatalf("refusal class %q is attributed to no promotion condition", kind)
		}
		if !declared[id] {
			t.Fatalf("refusal class %q is attributed to %q, which is not a declared condition id", kind, id)
		}
		hit[id]++
	}
	if len(conditionForRejectionKind) != len(kinds) {
		t.Fatalf("the attribution map holds %d entries for %d refusal classes", len(conditionForRejectionKind), len(kinds))
	}
	if _, ok := ConditionForRejectionKind(domain.PromotionRejectionKind("no-such-refusal-class")); ok {
		t.Fatal("known-negative: an unattributed refusal class was reported as attributed")
	}
	t.Logf("%d refusal classes attributed across %d conditions: %v", len(kinds), len(hit), hit)
}

// TestPromotableIsTrueOnlyWhenAllEightConditionsAreMet enumerates the whole
// 3^8 state space of the eight conditions and asserts the aggregate flag is
// true in exactly one of the 6561 assignments, and false in every assignment
// containing a not-observable-here.
func TestPromotableIsTrueOnlyWhenAllEightConditionsAreMet(t *testing.T) {
	ids := AllConditionIDs()
	states := []ConditionState{ConditionMet, ConditionUnmet, ConditionNotObservableHere}
	const wantStateCount = 3
	if len(states) != wantStateCount {
		t.Fatalf("tri-state axis drifted: %d", len(states))
	}
	wantGridSize := 1
	for range ids {
		wantGridSize *= wantStateCount
	}
	const wantGridSizeLiteral = 6561 // 3^8
	if wantGridSize != wantGridSizeLiteral {
		t.Fatalf("condition grid size is %d, want %d", wantGridSize, wantGridSizeLiteral)
	}

	total, trueCells, notObservableCells := 0, 0, 0
	assignment := make([]ConditionReport, len(ids))
	var walk func(int)
	walk = func(depth int) {
		if depth == len(ids) {
			total++
			anyNotObservable, allMet := false, true
			for _, c := range assignment {
				if c.State == ConditionNotObservableHere {
					anyNotObservable = true
				}
				if c.State != ConditionMet {
					allMet = false
				}
			}
			got := Promotable(assignment)
			if got != allMet {
				t.Fatalf("Promotable=%v for assignment %v, want %v", got, assignment, allMet)
			}
			if got {
				trueCells++
			}
			if anyNotObservable {
				notObservableCells++
				if got {
					t.Fatalf("Promotable is true with a not-observable-here condition: %v", assignment)
				}
			}
			return
		}
		for _, s := range states {
			assignment[depth] = ConditionReport{ID: ids[depth], State: s}
			walk(depth + 1)
		}
	}
	walk(0)
	if total != wantGridSizeLiteral {
		t.Fatalf("enumerated %d assignments, want %d", total, wantGridSizeLiteral)
	}
	if trueCells != 1 {
		t.Fatalf("Promotable was true for %d assignments, want exactly 1", trueCells)
	}
	// A short condition list, a duplicated id and a missing id must all be
	// refused, so the aggregate cannot be made true by dropping a condition.
	if Promotable(assignment[:len(ids)-1]) {
		t.Fatal("Promotable accepted a report with one condition missing")
	}
	dup := make([]ConditionReport, len(ids))
	for i := range dup {
		dup[i] = ConditionReport{ID: ids[0], State: ConditionMet}
	}
	if Promotable(dup) {
		t.Fatal("Promotable accepted a report whose conditions are all the same id")
	}
	t.Logf("condition closure: %d assignments, 1 promotable, %d containing a not-observable-here (all refused)", total, notObservableCells)
}

// --- the real tree ----------------------------------------------------------

// realTreeReport assembles the Foundation candidate from the repository root
// with no fabricated evidence, exactly as
// TestRealTreeFoundationCandidateIsNotPromotable does, and reports it.
func realTreeReport(t *testing.T) (PromotionReport, AssembledBundle) {
	t.Helper()
	root := realRoot()
	assembled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatalf("assemble the real tree: %v", err)
	}
	input := buildFullCandidateInput(assembled, "candidate-foundation-real-tree")
	input.Evidence = nil
	_, candidate, err := AssembleCandidate(root, input)
	if err != nil {
		t.Fatalf("AssembleCandidate on the real tree: %v", err)
	}
	report, err := BuildPromotionReport(ReportInput{Root: root, Candidate: candidate, Assembled: assembled, AssembledAt: injectedAssemblyInstant, EnvironmentClass: testEnvironmentClass})
	if err != nil {
		t.Fatalf("BuildPromotionReport on the real tree: %v", err)
	}
	return report, assembled
}

// TestRealTreeReportIsNotPromotableAndNamesEveryCapabilityWithoutEvidence is
// the honest negative result. The number of unmet capabilities is derived
// from the compiled contract's own capability list, never hardcoded.
func TestRealTreeReportIsNotPromotableAndNamesEveryCapabilityWithoutEvidence(t *testing.T) {
	report, assembled := realTreeReport(t)
	wantCapabilities := len(assembled.Contract.Capabilities)
	if wantCapabilities == 0 {
		t.Fatal("the compiled contract declares zero capabilities; this assertion must not pass vacuously")
	}
	if report.Promotable {
		t.Fatal("the real tree report claims promotable; every baseline capability has empty evidence_ids")
	}
	if len(report.DeclaredCapabilities) != wantCapabilities {
		t.Fatalf("report declares %d capabilities, contract declares %d", len(report.DeclaredCapabilities), wantCapabilities)
	}
	if len(report.CapabilitiesWithoutEvidence) != wantCapabilities {
		t.Fatalf("report names %d capabilities without evidence, want the contract's %d", len(report.CapabilitiesWithoutEvidence), wantCapabilities)
	}
	cond := conditionByID(t, report, ConditionCapabilitiesExercised)
	if cond.State != ConditionUnmet {
		t.Fatalf("condition 2 state = %q, want %q", cond.State, ConditionUnmet)
	}
	evidenceRefusals := 0
	for _, rj := range cond.Rejections {
		if rj.Kind == domain.RejectCapabilityEvidenceMissing {
			evidenceRefusals++
		}
	}
	if evidenceRefusals != wantCapabilities {
		t.Fatalf("condition 2 carries %d capability-evidence refusals, want the contract's %d", evidenceRefusals, wantCapabilities)
	}
	if report.ReleaseVersion != assembled.Contract.Version {
		t.Fatalf("report release version = %q, want %q", report.ReleaseVersion, assembled.Contract.Version)
	}
	t.Logf("real tree: release %s, %d declared capabilities, %d without evidence, promotable=%v", report.ReleaseVersion, len(report.DeclaredCapabilities), len(report.CapabilitiesWithoutEvidence), report.Promotable)
}

func conditionByID(t *testing.T, report PromotionReport, id ConditionID) ConditionReport {
	t.Helper()
	for _, c := range report.Conditions {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("report carries no condition %q", id)
	return ConditionReport{}
}

// TestConditionsOneAndFiveAreNotObservableHereWithAReason pins the two
// conditions no in-process source can decide, and asserts every condition
// carries a state, a reason and at least one named deciding source.
func TestConditionsOneAndFiveAreNotObservableHereWithAReason(t *testing.T) {
	report, _ := realTreeReport(t)
	if len(report.Conditions) != len(AllConditionIDs()) {
		t.Fatalf("report carries %d conditions, want %d", len(report.Conditions), len(AllConditionIDs()))
	}
	for _, c := range report.Conditions {
		switch c.State {
		case ConditionMet, ConditionUnmet, ConditionNotObservableHere:
		default:
			t.Fatalf("condition %q has state %q, which is not one of the three", c.ID, c.State)
		}
		if strings.TrimSpace(c.Reason) == "" {
			t.Fatalf("condition %q carries no reason", c.ID)
		}
		if len(c.DecidedBy) == 0 {
			t.Fatalf("condition %q names no deciding source", c.ID)
		}
		if strings.TrimSpace(c.Contract) == "" {
			t.Fatalf("condition %q carries no contract wording", c.ID)
		}
	}
	for _, id := range []ConditionID{ConditionDeterministicVerification, ConditionNoUnresolvedIncident} {
		c := conditionByID(t, report, id)
		if c.State != ConditionNotObservableHere {
			t.Fatalf("condition %q state = %q, want %q", id, c.State, ConditionNotObservableHere)
		}
	}
	// No other condition may hide behind not-observable-here.
	for _, c := range report.Conditions {
		if c.State != ConditionNotObservableHere {
			continue
		}
		if c.ID != ConditionDeterministicVerification && c.ID != ConditionNoUnresolvedIncident {
			t.Fatalf("condition %q is reported not-observable-here; only conditions 1 and 5 may be", c.ID)
		}
	}
}

// TestReportCarriesTheNotObservedListAndTheResidualGaps is A9/A15 at the
// report boundary: the D1 exclusions and the two unwired capability parts are
// carried as data, not left implicit.
func TestReportCarriesTheNotObservedListAndTheResidualGaps(t *testing.T) {
	report, _ := realTreeReport(t)
	if report.EnvironmentClass != testEnvironmentClass {
		t.Fatalf("environment class = %q, want %q", report.EnvironmentClass, testEnvironmentClass)
	}
	for _, want := range []string{"cloud-run-running-revision", "deployed-image-digest", "deploy-path", "iap-authentication-boundary", "scale-to-zero"} {
		found := false
		for _, got := range report.NotObserved {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("not-observed list %v does not name %q", report.NotObserved, want)
		}
	}
	if len(report.ResidualGaps) != 2 {
		t.Fatalf("report names %d residual gaps, want 2", len(report.ResidualGaps))
	}
	gaps := strings.Join(report.ResidualGaps, "\n")
	for _, want := range []string{"cap-preview-operation", "cap-stable-promotion"} {
		if !strings.Contains(gaps, want) {
			t.Fatalf("residual gaps do not name %q: %s", want, gaps)
		}
	}
	// The surface must never claim the capability was exercised.
	for _, forbidden := range []string{"capability exercised", "preview journey passed"} {
		if strings.Contains(strings.ToLower(gaps), forbidden) {
			t.Fatalf("residual gap text contains the forbidden claim %q", forbidden)
		}
	}
	if report.AssembledAt != injectedAssemblyInstant {
		t.Fatalf("assembled-at = %v, want the injected instant %v", report.AssembledAt, injectedAssemblyInstant)
	}
}

// --- a fully evidenced candidate -------------------------------------------

// TestFullyEvidencedCandidateMeetsEverySixObservableConditionsAndIsStillNotPromotable
// shows the report reaches met on all six conditions an in-process source can
// decide, and still refuses to report promotable, because conditions 1 and 5
// remain unobservable here.
func TestFullyEvidencedCandidateMeetsSixObservableConditionsAndIsStillNotPromotable(t *testing.T) {
	root := realRoot()
	bundle, assembled, _ := fullyEvidencedBundle(t, root, "repo", "candidate-v2-066-full")
	report, err := BuildPromotionReport(ReportInput{Root: root, Candidate: bundle.Candidate, Assembled: assembled, AssembledAt: injectedAssemblyInstant, EnvironmentClass: testEnvironmentClass})
	if err != nil {
		t.Fatalf("BuildPromotionReport: %v", err)
	}
	met, notObservable, unmet := 0, 0, []ConditionID{}
	for _, c := range report.Conditions {
		switch c.State {
		case ConditionMet:
			met++
		case ConditionNotObservableHere:
			notObservable++
		default:
			unmet = append(unmet, c.ID)
			t.Errorf("condition %q is unmet on a fully evidenced candidate: %s", c.ID, c.Reason)
		}
	}
	if len(unmet) != 0 {
		t.Fatalf("unmet conditions on a fully evidenced candidate: %v", unmet)
	}
	if met != 6 || notObservable != 2 {
		t.Fatalf("met=%d not-observable=%d, want 6 and 2", met, notObservable)
	}
	if report.Promotable {
		t.Fatal("promotable is true while two conditions are not observable here")
	}
	if len(report.Rejections) != 0 {
		t.Fatalf("a fully evidenced candidate produced %d refusals: %+v", len(report.Rejections), report.Rejections)
	}
	if report.BundleDigest != assembled.BundleDigest || report.DocsDigest != assembled.DocsDigest || report.ContractDigest != assembled.ContractDigest {
		t.Fatal("the report's digests are not the source-derived ones")
	}
}

// TestEveryRefusalIsNamedByItsOwnConditionOverAClosedGrid is the second half
// of the two-way agreement: whenever the authority refuses, the report names
// the condition that refusal belongs to, and that condition is unmet. The
// grid is the closed cross product of five independent mutations of a fully
// evidenced candidate.
func TestEveryRefusalIsNamedByItsOwnConditionOverAClosedGrid(t *testing.T) {
	root := realRoot()
	bundle, assembled, _ := fullyEvidencedBundle(t, root, "repo", "candidate-v2-066-grid")

	const wantGridSize = 32 // 2^5: status, targets, evidence, rollback, resume
	total, refusalCells := 0, 0
	for _, badStatus := range []bool{false, true} {
		for _, dropTargets := range []bool{false, true} {
			for _, dropEvidence := range []bool{false, true} {
				for _, dropRollback := range []bool{false, true} {
					for _, dropResume := range []bool{false, true} {
						total++
						candidate := bundle.Candidate.Clone()
						if badStatus {
							candidate.Status = domain.ReleaseAssembling
						}
						if dropTargets {
							candidate.CapabilityTargets = map[string]domain.CapabilityTarget{}
						}
						if dropEvidence {
							candidate.Evidence = nil
						}
						if dropRollback {
							candidate.RollbackEvidence = false
						}
						if dropResume {
							candidate.ResumeEvidence = false
						}
						report, err := BuildPromotionReport(ReportInput{Root: root, Candidate: candidate, Assembled: assembled, AssembledAt: injectedAssemblyInstant, EnvironmentClass: testEnvironmentClass})
						if err != nil {
							t.Fatalf("BuildPromotionReport: %v", err)
						}
						authority := candidate.CanPromote()
						rejections := candidate.PromotionRejections()
						if len(report.Rejections) != len(rejections) {
							t.Fatalf("report carries %d refusals, the authority enumerates %d", len(report.Rejections), len(rejections))
						}
						if (len(rejections) == 0) != (authority == nil) {
							t.Fatalf("authority disagreement: CanPromote=%v with %d refusals", authority, len(rejections))
						}
						if len(rejections) == 0 {
							continue
						}
						refusalCells++
						for _, rj := range rejections {
							id, ok := ConditionForRejectionKind(rj.Kind)
							if !ok {
								t.Fatalf("refusal %q is attributed to no condition", rj.Kind)
							}
							cond := conditionByID(t, report, id)
							if cond.State != ConditionUnmet {
								t.Fatalf("refusal %q belongs to condition %q, whose state is %q, want %q", rj.Kind, id, cond.State, ConditionUnmet)
							}
							found := false
							for _, carried := range cond.Rejections {
								if carried == rj {
									found = true
									break
								}
							}
							if !found {
								t.Fatalf("condition %q does not carry the refusal %+v it was attributed", id, rj)
							}
						}
						if report.Promotable {
							t.Fatal("promotable is true while the authority refuses")
						}
					}
				}
			}
		}
	}
	if total != wantGridSize {
		t.Fatalf("enumerated %d cells, want %d", total, wantGridSize)
	}
	if refusalCells != wantGridSize-1 {
		t.Fatalf("%d of %d cells produced a refusal, want %d", refusalCells, total, wantGridSize-1)
	}
}

// --- documentation conditions name the deciding function -------------------

// TestDocumentationConditionsNameTheDecidingVerifier drifts one document at a
// time in a copied tree and asserts the report attributes the failure to the
// existing docs.go verifier that decided it, with no new prose check.
func TestDocumentationConditionsNameTheDecidingVerifier(t *testing.T) {
	cases := []struct {
		name       string
		doc        string
		replace    string
		with       string
		condition  ConditionID
		wantSource string
	}{
		{"preview release marker", PreviewIndexDoc, "Release: ", "Released: ", ConditionPreviewDocsComplete, "release.VerifyPreviewReleaseMarker"},
		{"capability anchor bijection", PreviewCapabilitiesDoc, `<a id="cap-loop-control"></a>`, `<a id="cap-not-in-the-contract"></a>`, ConditionPreviewDocsComplete, "release.VerifyCapabilityAnchorBijection"},
		{"required diff sections", PreviewStableDiffDoc, "## Stableへ戻す方法", "## どうにかする方法", ConditionStableDiffDocumented, "release.VerifyRequiredSections over release.RequiredPreviewSections"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := copyTreeForReport(t, realRoot())
			rewriteDoc(t, root, tc.doc, tc.replace, tc.with)
			assembled, err := AssembleFromRoot(root)
			if err != nil {
				t.Fatalf("assemble the drifted tree: %v", err)
			}
			input := buildFullCandidateInput(assembled, "candidate-drift")
			_, candidate, err := AssembleCandidate(root, input)
			if err != nil {
				t.Fatal(err)
			}
			candidate.Evidence = evidenceFor(candidate, assembled.BundleDigest)
			candidate.EvidenceDigest = computeEvidenceDigest(candidate.Evidence)
			report, err := BuildPromotionReport(ReportInput{Root: root, Candidate: candidate, Assembled: assembled, AssembledAt: injectedAssemblyInstant, EnvironmentClass: testEnvironmentClass})
			if err != nil {
				t.Fatalf("BuildPromotionReport: %v", err)
			}
			cond := conditionByID(t, report, tc.condition)
			if cond.State != ConditionUnmet {
				t.Fatalf("condition %q state = %q, want %q (reason %q)", tc.condition, cond.State, ConditionUnmet, cond.Reason)
			}
			if !strings.Contains(cond.Reason, tc.wantSource) {
				t.Fatalf("condition %q reason %q does not name the deciding verifier %q", tc.condition, cond.Reason, tc.wantSource)
			}
			named := false
			for _, s := range cond.DecidedBy {
				if s == tc.wantSource {
					named = true
					break
				}
			}
			if !named {
				t.Fatalf("condition %q DecidedBy %v does not list %q", tc.condition, cond.DecidedBy, tc.wantSource)
			}
			if report.Promotable {
				t.Fatal("promotable is true with a drifted document")
			}
		})
	}
}

// TestOneVersionConditionIsDecidedByTheSourceVerifiers drifts a non-document
// bundle member after assembly and asserts condition 8 is refused by
// VerifySource rather than by a new check.
func TestOneVersionConditionIsDecidedByTheSourceVerifiers(t *testing.T) {
	root := copyTreeForReport(t, realRoot())
	assembled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	input := buildFullCandidateInput(assembled, "candidate-source-drift")
	_, candidate, err := AssembleCandidate(root, input)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Evidence = evidenceFor(candidate, assembled.BundleDigest)
	candidate.EvidenceDigest = computeEvidenceDigest(candidate.Evidence)
	// Change a member's bytes after the members were recorded.
	rewriteDoc(t, root, "contracts/schemas/task-state.json", "\"schema_version\"", "\"schema_version\" ")
	report, err := BuildPromotionReport(ReportInput{Root: root, Candidate: candidate, Assembled: assembled, AssembledAt: injectedAssemblyInstant, EnvironmentClass: testEnvironmentClass})
	if err != nil {
		t.Fatalf("BuildPromotionReport: %v", err)
	}
	cond := conditionByID(t, report, ConditionOneVersion)
	if cond.State != ConditionUnmet {
		t.Fatalf("condition 8 state = %q, want %q", cond.State, ConditionUnmet)
	}
	if !strings.Contains(cond.Reason, "release.VerifySource") {
		t.Fatalf("condition 8 reason %q does not name release.VerifySource", cond.Reason)
	}
	for _, want := range []string{"release.roleSpecs (the seven bundle roles)", "release.VerifySource", "release.VerifyCandidateDigests", "release.VerifyCandidateAgainstContract", "domain.ReleaseCandidate.PromotionRejections"} {
		found := false
		for _, s := range cond.DecidedBy {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("condition 8 DecidedBy %v does not list %q", cond.DecidedBy, want)
		}
	}
}

// --- refusals of an unusable input -----------------------------------------

func TestBuildPromotionReportRefusesAnUnusableInput(t *testing.T) {
	root := realRoot()
	assembled, err := AssembleFromRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	input := buildFullCandidateInput(assembled, "candidate-refusal")
	_, candidate, err := AssembleCandidate(root, input)
	if err != nil {
		t.Fatal(err)
	}
	base := ReportInput{Root: root, Candidate: candidate, Assembled: assembled, AssembledAt: injectedAssemblyInstant, EnvironmentClass: testEnvironmentClass}

	noRoot := base
	noRoot.Root = "  "
	if _, err := BuildPromotionReport(noRoot); err == nil {
		t.Fatal("a report was built with no configured source root")
	}
	noTime := base
	noTime.AssembledAt = time.Time{}
	if _, err := BuildPromotionReport(noTime); err == nil {
		t.Fatal("a report was built with no assembly instant")
	}
	noClass := base
	noClass.EnvironmentClass = ""
	if _, err := BuildPromotionReport(noClass); err == nil {
		t.Fatal("a report was built with no declared environment class")
	}
	noMembers := base
	noMembers.Assembled = AssembledBundle{}
	if _, err := BuildPromotionReport(noMembers); err == nil {
		t.Fatal("a report was built with no assembled bundle")
	}
}

// --- helpers ---------------------------------------------------------------

// copyTreeForReport copies the bundle-relevant parts of a tree into a
// temporary directory so a test can drift one member without touching the
// repository.
func copyTreeForReport(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	members, err := assembleMembers(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		data, err := readFileHelper(filepath.Join(src, filepath.FromSlash(m.Path)))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, dst, m.Path, string(data))
	}
	return dst
}

func rewriteDoc(t *testing.T, root, rel, old, new string) {
	t.Helper()
	data, err := readFileHelper(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s does not contain %q, so the drift fixture would be vacuous", rel, old)
	}
	writeFile(t, root, rel, strings.Replace(string(data), old, new, 1))
}
