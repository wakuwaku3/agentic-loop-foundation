package scheduler

// This file gives Decide's per-candidate comparison an auditable record
// (V2-030 / A4). Before this, Decide silently `continue`d past every
// rejection path, so a Requirement that never ran was indistinguishable from
// one that was never considered; a closed, exhaustively enumerated reason set
// is also what the starvation proof in starvation_test.go needs to attribute
// a wait to a cause.

import "time"

// Reason is the closed set of rejection causes Decide can attach to a
// Decision. An unassigned candidate has exactly one Reason; an assigned
// candidate has none (the zero value, "").
type Reason string

const (
	// ReasonNotReady covers a candidate that fails the structural
	// eligibility check on its own terms: empty ID, Status other than
	// StatusReady, no RepositoryIDs, or an invalid ResourceRequest (empty
	// name or a Mode other than Read/Write).
	ReasonNotReady Reason = "not-ready"
	// ReasonUnmetDependency covers a candidate whose Dependencies are not
	// all StatusCompleted yet.
	ReasonUnmetDependency Reason = "unmet-dependency"
	// ReasonRepositoryUnavailable covers a candidate naming a Repository
	// that is missing from the Snapshot, mid failure-storm isolation
	// (FailureCount >= FailureStormThreshold), or under an active
	// IsolatedUntil deadline.
	ReasonRepositoryUnavailable Reason = "repository-unavailable"
	// ReasonAlreadyOwned covers a candidate that already owns a Claim,
	// whether from the Snapshot's existing Claims or from a Claim already
	// planned earlier in this same Decide call.
	ReasonAlreadyOwned Reason = "already-owned"
	// ReasonResourceConflict covers a candidate whose requested Resources
	// conflict (same scope, same name, and at least one side is Write)
	// with an existing or already-planned Claim.
	ReasonResourceConflict Reason = "resource-conflict"
	// ReasonNoRunnerCapacity covers a candidate that clears every other
	// check but finds no Runner with spare Capacity and, if
	// ProviderCapacity is set, spare provider-wide capacity either.
	ReasonNoRunnerCapacity Reason = "no-runner-capacity"
	// ReasonNotExecutable covers a candidate whose
	// domain.PriorityAssessment.Executable is explicitly false.
	ReasonNotExecutable Reason = "not-executable"
)

// AllReasons is the closed, exhaustively enumerated set of every rejection
// reason Decide can produce. scheduler_test.go's exhaustiveness table test
// fails if a constant is added here without a matching test scenario, or a
// scenario claims a reason that is not a member of this set.
var AllReasons = []Reason{
	ReasonNotReady,
	ReasonUnmetDependency,
	ReasonRepositoryUnavailable,
	ReasonAlreadyOwned,
	ReasonResourceConflict,
	ReasonNoRunnerCapacity,
	ReasonNotExecutable,
}

// ScoreInputs is the factor set Decide actually used when it ranked one
// candidate. When UsedAssessment is false, the candidate carried no
// domain.PriorityAssessment and only Priority/Age fed its score (the legacy
// scalar branch). When UsedAssessment is true, the seven
// domain.PriorityAssessment factors (already clamped to [0,100], see
// clampFactor) plus Age fed it instead.
type ScoreInputs struct {
	UsedAssessment bool
	Priority       int64
	Age            int64

	ValueScore      int
	UrgencyScore    int
	RiskScore       int
	DependencyScore int
	LearningScore   int
	ResourceCost    int
	StarvationRisk  int
}

// scoreInputsFor reports the factor inputs Decide actually used for q at
// now, mirroring exactly the branch computeScore/scoreFn takes.
func scoreInputsFor(q Requirement, now time.Time) ScoreInputs {
	age := ageSeconds(q.CreatedAt, now)
	if q.Assessment == nil {
		return ScoreInputs{UsedAssessment: false, Priority: legacyPriority(q.Priority), Age: age}
	}
	a := *q.Assessment
	return ScoreInputs{
		UsedAssessment:  true,
		Age:             age,
		ValueScore:      int(clampFactor(a.ValueScore)),
		UrgencyScore:    int(clampFactor(a.UrgencyScore)),
		RiskScore:       int(clampFactor(a.RiskScore)),
		DependencyScore: int(clampFactor(a.DependencyScore)),
		LearningScore:   int(clampFactor(a.LearningScore)),
		ResourceCost:    int(clampFactor(a.ResourceCost)),
		StarvationRisk:  int(clampFactor(a.StarvationRisk)),
	}
}

// Decision is Decide's auditable record for exactly one candidate it
// considered: the factor inputs actually used, the computed score, its rank
// in the sorted candidate order (0 is highest), whether it was assigned, and
// -- only when it was not -- exactly one Reason. When the candidate carried a
// domain.PriorityAssessment, that assessment's Version, Reason and
// ReevaluateWhen are carried through unchanged so a caller can act on the
// assessment's own re-evaluation trigger without re-deriving it.
type Decision struct {
	RequirementID string
	Inputs        ScoreInputs
	Score         int64
	Rank          int
	Assigned      bool
	Reason        Reason // "" when Assigned is true

	AssessmentVersion        uint64
	AssessmentReason         string
	AssessmentReevaluateWhen string
}

// newDecision builds the Decision record for q at the given rank and
// outcome. reason must be "" when assigned is true.
func newDecision(q Requirement, now time.Time, rank int, assigned bool, reason Reason) Decision {
	d := Decision{
		RequirementID: q.ID,
		Inputs:        scoreInputsFor(q, now),
		Score:         scoreFn(q, now),
		Rank:          rank,
		Assigned:      assigned,
		Reason:        reason,
	}
	if q.Assessment != nil {
		d.AssessmentVersion = uint64(q.Assessment.Version)
		d.AssessmentReason = q.Assessment.Reason
		d.AssessmentReevaluateWhen = q.Assessment.ReevaluateWhen
	}
	return d
}

// decideReason evaluates every rejection path Decide has except runner
// capacity, which depends on mutable per-tick Runner/provider state and is
// therefore checked by the caller after this returns true. It returns the
// Reason that would explain non-assignment and whether q clears every one of
// these checks.
func decideReason(q Requirement, done map[string]bool, repos map[string]Repository, now time.Time, existingClaims, plannedClaims []Claim) (Reason, bool) {
	switch {
	case !structurallyReady(q):
		return ReasonNotReady, false
	case !dependenciesMet(q, done):
		return ReasonUnmetDependency, false
	case q.Assessment != nil && !q.Assessment.Executable:
		return ReasonNotExecutable, false
	case !repositoriesAvailable(q, repos, now):
		return ReasonRepositoryUnavailable, false
	case owned(q, existingClaims, plannedClaims):
		return ReasonAlreadyOwned, false
	case resourceConflict(q, existingClaims, plannedClaims):
		return ReasonResourceConflict, false
	default:
		return "", true
	}
}
