package application

// V2-068 wires internal/scheduler into the running system and makes shared
// resource allocation observable. Two things live here and nothing else:
//
//  1. the installation-wide concurrency limit, carried in a side table keyed by
//     Control Intent revision -- NOT as a domain.ControlMode value and not as a
//     field on domain.ControlIntent, and
//  2. the allocation report GET /v1/queue/summary adds: what the scheduler
//     would allocate from the state read in this same transaction, why each
//     candidate it did not assign is waiting, and which limit is binding.
//
// WHY THE LIMIT IS NOT A ControlMode. domain.EffectFromPermit refuses unless
// the effective mode is domain.ControlAllow, so a new mode would deny every
// durable effect the Loop needs -- Claim, AcceptResult and all four outbox
// kinds. A "limit-concurrency" mode would therefore stop the Loop rather than
// throttle it, and making it not stop the Loop would mean widening the one gate
// the M1 gate proved closed. The seven mode values are unchanged, and
// stop_matrix_test.go's per-mode allowed-cell counts are re-measured, never
// edited. ControlRequestedByRepository is the precedent this side table copies:
// an attribute of the request, written at most once per revision, in the same
// transaction as the Control Intent it describes.
//
// WHY THE REPORT CALLS Decide AND NEVER Apply. Reporting what the scheduler
// decided is only truthful if the decision is a function of the state read in
// the same transaction. A value cached on a reconcile tick would describe an
// allocation the current state no longer supports, and calling Apply from a
// read path would change Requirement status and add Claims as a side effect of
// an owner pressing Refresh. The whole report is therefore computed inside the
// existing read transaction, performs no write, stages no outbox item and
// reserves no mutation quota.
//
// WHY A LOWERED LIMIT NEVER REVOKES ANYTHING. The limit is consulted at exactly
// one place: the capacity of the single installation pool entry in the Snapshot
// built for the NEXT allocation. Existing Leases and Claims enter that Snapshot
// as read-only scheduler.Claim values that Decide only reads, and
// scheduler.Apply -- which this package never calls -- only appends Claims and
// only moves a Requirement from ready to assigned. No code path in
// internal/scheduler removes a Claim or revokes a Lease. Lowering the limit
// below the current active count therefore changes nothing that was already
// granted; the installation converges to the new limit as work completes.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/scheduler"
)

// ---------------------------------------------------------------------------
// The limit: range, source and stored record
// ---------------------------------------------------------------------------

// AllocationLimitMinimum and AllocationLimitCeiling bound the settable
// installation concurrency limit.
//
// The ceiling is docs/architecture/validation.md section 5's "concurrent
// Execution: 20" design ceiling, not an invented number, so this surface cannot
// promise a concurrency the documented design does not support. It is the same
// figure ProviderConcurrencyDesignCeiling names for the per-Provider half.
//
// Zero is rejected rather than treated as a stop. A limit of zero would be a
// second way to halt the Loop that the control-verification pipeline does not
// observe, and domain.ControlPauseClaim already exists for that with a proven
// verification path. A stop must go through the mode whose blast radius the
// stop matrix measures.
const (
	AllocationLimitMinimum = 1
	AllocationLimitCeiling = 20
)

// ErrAllocationLimitOutOfRange is a malformed request, never a policy
// decision: the caller asked for a concurrency outside 1..20.
var ErrAllocationLimitOutOfRange = fmt.Errorf("installation_concurrent_executions must be between %d and %d inclusive", AllocationLimitMinimum, AllocationLimitCeiling)

// ErrAllocationCandidateBound is the fail-closed refusal of a candidate set
// larger than the scheduler's own bound. The builder refuses rather than
// truncating: silently dropping candidates would make the report describe a
// queue that is not the queue.
var ErrAllocationCandidateBound = fmt.Errorf("allocation snapshot candidates exceed the scheduler's own bound of %d", scheduler.MaxCandidates)

// ErrAllocationLeaseBound is the fail-closed refusal of more active Leases
// than one bounded read may describe. It mirrors Control's own safety limit.
var ErrAllocationLeaseBound = errors.New("active lease safety limit exceeded")

// AllocationLeaseBound is the bounded active-Lease read this report makes. It
// is the same bound Service.Control uses, so the two paths cannot disagree
// about how many active Leases an installation may have.
const AllocationLeaseBound = 100

// AllocationLimitSource names where the reported limit came from, so a design
// ceiling is never shown as though an owner had chosen it.
type AllocationLimitSource string

const (
	// AllocationLimitFromDesignCeiling means no Control Intent revision has
	// ever declared a limit, so the reported ceiling is
	// docs/architecture/validation.md section 5's design ceiling.
	AllocationLimitFromDesignCeiling AllocationLimitSource = "architecture-design-ceiling"
	// AllocationLimitFromControlRevision means an owner declared this limit on
	// the Control Intent revision the report names.
	AllocationLimitFromControlRevision AllocationLimitSource = "control-revision"
)

func (s AllocationLimitSource) Valid() bool {
	switch s {
	case AllocationLimitFromDesignCeiling, AllocationLimitFromControlRevision:
		return true
	}
	return false
}

// BindingLimit names which limit is binding when the installation is
// exhausted. It is computed from the Snapshot's own capacity accounting and is
// never derived by re-reading a candidate's rejection reason: measured,
// scheduler.chooseRunner returns the same negative result whether a Runner's
// own Capacity is full or a ProviderCapacity entry binds, and V2-030's closed
// reason set expresses both as the single ReasonNoRunnerCapacity. Splitting
// that inside the scheduler would be re-designing its decision rules.
type BindingLimit string

const (
	// BindingNone means capacity remains: nothing is binding.
	BindingNone BindingLimit = "none"
	// BindingInstallationConcurrency means the limit an owner declared on a
	// Control Intent revision is what the installation has reached.
	BindingInstallationConcurrency BindingLimit = "installation-concurrency"
	// BindingRunnerCapacity means the pool's own capacity is what the
	// installation has reached. Today the pool's capacity has exactly one
	// source when no owner limit exists -- the architecture design ceiling --
	// because there is no enrolled-Runner registry to measure a real pool from:
	// RunnerObservationRepository is keyed by id and has no list operation. So
	// this value is reported when the installation is exhausted and no owner
	// ever declared a limit.
	BindingRunnerCapacity BindingLimit = "runner-capacity"
)

func (b BindingLimit) Valid() bool {
	switch b {
	case BindingNone, BindingInstallationConcurrency, BindingRunnerCapacity:
		return true
	}
	return false
}

// AllocationLimitInput is the additive optional object on the control request.
// It carries exactly one field: how many Executions the installation may run at
// once. There is no mode, no scope, no actor and no reason on it -- those
// belong to the Control Intent, which is unchanged.
type AllocationLimitInput struct {
	InstallationConcurrentExecutions int `json:"installation_concurrent_executions"`
}

// Validate is the shape refusal. It is called before any transaction opens, so
// a rejected limit creates no Control Intent and stores nothing at all.
func (a AllocationLimitInput) Validate() error {
	if a.InstallationConcurrentExecutions < AllocationLimitMinimum || a.InstallationConcurrentExecutions > AllocationLimitCeiling {
		return ErrAllocationLimitOutOfRange
	}
	return nil
}

// AllocationLimit is the stored side-table row: one Control Intent revision and
// the limit that revision declared. It is not an aggregate, has no state
// transition and no Version, and no domain rule consults it.
type AllocationLimit struct {
	Revision                         domain.Revision `json:"revision"`
	InstallationConcurrentExecutions int             `json:"installation_concurrent_executions"`
}

// Validate is the same range refusal the request-shape check makes, restated at
// the storage boundary so an out-of-range row cannot be written by any path,
// including a caller that skipped AllocationLimitInput.Validate.
func (a AllocationLimit) Validate() error {
	return AllocationLimitInput{InstallationConcurrentExecutions: a.InstallationConcurrentExecutions}.Validate()
}

// ---------------------------------------------------------------------------
// The reported shape
// ---------------------------------------------------------------------------

// AllocationView is what the scheduler is allowed to allocate and what it
// actually planned for this read.
//
// PlannedAssignments is the length of the plan scheduler.Decide returned in
// this transaction, and nothing else: it is never the active Execution count
// dressed up as an allocation. A read that plans nothing reports zero here
// while Active keeps reporting the Executions that are genuinely running.
type AllocationView struct {
	Limit              int                   `json:"limit"`
	LimitSource        AllocationLimitSource `json:"limit_source"`
	ControlRevision    domain.Revision       `json:"control_revision"`
	Active             int                   `json:"active"`
	Remaining          int                   `json:"remaining"`
	PlannedAssignments int                   `json:"planned_assignments"`
}

// WaitingView is why the candidates the scheduler considered are not running.
//
// ByReason always carries one bucket per member of scheduler.AllReasons, zero
// included, so the reported vocabulary is visibly the scheduler's own closed
// set and the key order is fixed. Total is the number of considered candidates
// that were not assigned, and it equals the sum of the buckets.
type WaitingView struct {
	Total    int            `json:"total"`
	ByReason map[string]int `json:"by_reason"`
}

// ExhaustionView is the capacity verdict, kept separate from the per-candidate
// waiting reason because they answer different questions: a candidate can be
// waiting on a resource conflict while capacity remains.
type ExhaustionView struct {
	Exhausted    bool         `json:"exhausted"`
	BindingLimit BindingLimit `json:"binding_limit"`
}

// ---------------------------------------------------------------------------
// 優先度とその根拠: a projection of what the scheduler already decided
// ---------------------------------------------------------------------------

// PriorityView is cap-backlog-visibility's declared confirmation item
// "優先度とその根拠", reported as a PROJECTION of decisions the read already
// made and then threw away (V2-095 A7).
//
// allocationReport already calls scheduler.Decide over ONE bounded page of at
// most scheduler.MaxCandidates Requirements and keeps only a reason histogram.
// plan.Decisions already carries RequirementID, Rank, Score, Assigned, Reason
// and Inputs for every candidate it ranked. This view consumes exactly that.
// IT ADDS NO READ AT ALL: no second page, no second Decide call, no per-row
// read. That is the property which keeps GET /v1/queue/summary bounded.
//
// A Requirement the scheduler did not rank carries NO entry. Bounded says so
// and Reason says why, following the idiom the owner console already states: a
// bounded count is reported as a lower bound, never as an exact total, and an
// unranked Requirement is never given a plausible rank.
type PriorityView struct {
	// Bounded is true when the candidate page the scheduler ranked did not
	// cover the whole Backlog, so entries below is a prefix of the ranking and
	// not the ranking.
	Bounded bool `json:"bounded"`
	// Reason states, in prose, what Bounded and the two counts mean.
	Reason string `json:"reason"`
	// CandidatesRanked is len(plan.Decisions): how many Requirements the
	// scheduler actually produced a decision for in this read.
	CandidatesRanked int `json:"candidates_ranked"`
	// CandidateBound is scheduler.MaxCandidates, the scheduler's own bound,
	// and never a bound this package invents.
	CandidateBound int `json:"candidate_bound"`
	// AssessmentSupplied reports whether ANY candidate carried a
	// domain.PriorityAssessment. It is measured false at this commit for every
	// candidate, because BuildAllocationSnapshot supplies neither Priority nor
	// Assessment, so the score in force is the legacy age-only branch. That
	// fact is REPORTED rather than hidden: a rationale that also says what the
	// rationale is NOT is the only honest way to report a priority the product
	// intends to be multi-factor while the code ranks by age.
	AssessmentSupplied bool `json:"assessment_supplied"`
	// AssessmentNote states the above in prose and names where the connection
	// belongs.
	AssessmentNote string              `json:"assessment_note"`
	Entries        []PriorityEntryView `json:"entries"`
}

// PriorityEntryView is one candidate's own decision, exactly as the scheduler
// recorded it.
type PriorityEntryView struct {
	RequirementID string `json:"requirement_id"`
	// Rank is scheduler.Decision.Rank: 0 is highest, in the sorted candidate
	// order the scheduler itself produced.
	Rank     int  `json:"rank"`
	Assigned bool `json:"assigned"`
	// Reason is present only when Assigned is false, and is always a member of
	// scheduler.AllReasons: it is projected through the SAME WaitingReasonFor
	// mapping the waiting histogram uses, so an unknown reason is a refusal of
	// the whole read rather than a silent bucket, and the two can never
	// disagree about the vocabulary.
	Reason      string                  `json:"reason,omitempty"`
	Score       int64                   `json:"score"`
	ScoreInputs PriorityScoreInputsView `json:"score_inputs"`
}

// PriorityScoreInputsView is scheduler.ScoreInputs: the factors Decide
// ACTUALLY used for this candidate, not the factors it could have used.
type PriorityScoreInputsView struct {
	UsedAssessment bool  `json:"used_assessment"`
	Priority       int64 `json:"priority"`
	AgeSeconds     int64 `json:"age_seconds"`
	// Factors is OMITTED ENTIRELY when UsedAssessment is false. The seven
	// factor fields are zero in that branch and reporting seven zeroes would
	// read as seven measured zeroes; absent means absent.
	Factors *PriorityFactorsView `json:"factors,omitempty"`
}

// PriorityFactorsView is the seven domain.PriorityAssessment factors, already
// clamped to [0,100] by the scheduler.
type PriorityFactorsView struct {
	ValueScore      int `json:"value_score"`
	UrgencyScore    int `json:"urgency_score"`
	RiskScore       int `json:"risk_score"`
	DependencyScore int `json:"dependency_score"`
	LearningScore   int `json:"learning_score"`
	ResourceCost    int `json:"resource_cost"`
	StarvationRisk  int `json:"starvation_risk"`
}

const priorityAssessmentAbsentNote = "no candidate carried a domain.PriorityAssessment in this read, so every score below came from the legacy age-only branch and used_assessment is false throughout. The type and its scorer both exist (domain.PriorityAssessment and internal/scheduler/priority.go's multiFactorScore); what is absent is the supply path -- BuildAllocationSnapshot sets neither Priority nor Assessment on any row. Section 5 assigns that connection to V2-030 in M7. This response reports the absence instead of fabricating a factor."

const priorityAssessmentPresentNote = "at least one candidate carried a domain.PriorityAssessment, so the multi-factor branch fed its score and its seven clamped factors are reported under score_inputs.factors."

// priorityScoreInputsView projects scheduler.ScoreInputs onto the wire shape.
func priorityScoreInputsView(in scheduler.ScoreInputs) PriorityScoreInputsView {
	out := PriorityScoreInputsView{UsedAssessment: in.UsedAssessment, Priority: in.Priority, AgeSeconds: in.Age}
	if in.UsedAssessment {
		out.Factors = &PriorityFactorsView{
			ValueScore:      in.ValueScore,
			UrgencyScore:    in.UrgencyScore,
			RiskScore:       in.RiskScore,
			DependencyScore: in.DependencyScore,
			LearningScore:   in.LearningScore,
			ResourceCost:    in.ResourceCost,
			StarvationRisk:  in.StarvationRisk,
		}
	}
	return out
}

// priorityReason states what the bound means, in the two cases it has.
func priorityReason(ranked, bound int, bounded bool) string {
	if bounded {
		return fmt.Sprintf("the scheduler ranked ONE bounded page of %d candidates against its own bound of %d and more Requirements exist beyond it, so entries is a PREFIX of the ranking; a Requirement with no entry was not ranked in this read and is deliberately given no rank", ranked, bound)
	}
	return fmt.Sprintf("the scheduler ranked %d candidates within its own bound of %d and no Requirement exists beyond that page, so entries covers every Requirement this read considered; a Requirement with no entry carries no rank because none was computed for it", ranked, bound)
}

// QueueSummaryResponse is the GET /v1/queue/summary body: the five existing
// counters, unchanged in name, type and meaning, plus the three allocation
// objects.
//
// MEASURED DEVIATION FROM A8, RECORDED RATHER THAN PAPERED OVER. A8 asks for
// the three objects on QueueSummary itself. QueueSummary is also embedded as
// RepositoryBacklogView.InstallationScope by internal/application/repository.go
// (GET /v1/repositories/{id}), which is outside this task's allowed paths and
// therefore cannot populate them. Putting required, closed-enum fields on the
// shared counter type would make that response emit limit_source:"" and
// binding_limit:"" -- two values outside their own enums, in a schema declared
// with additionalProperties:false. A separate response type keeps the shared
// counters byte-identical where they are already published and keeps every new
// field required where it is actually computed. dp-v2-068's own risk list names
// this exact hazard.
type QueueSummaryResponse struct {
	QueueSummary
	Allocation AllocationView `json:"allocation"`
	Waiting    WaitingView    `json:"waiting"`
	Exhaustion ExhaustionView `json:"exhaustion"`
	// Priority is cap-backlog-visibility's 優先度とその根拠 (V2-095 A7). It is
	// on this response type and not on the shared QueueSummary for the same
	// reason the three objects above are: QueueSummary is embedded as
	// RepositoryBacklogView.InstallationScope by
	// internal/application/repository.go, which cannot populate it.
	Priority PriorityView `json:"priority"`
}

// ---------------------------------------------------------------------------
// The waiting-reason projection
// ---------------------------------------------------------------------------

// WaitingReasonFor projects one scheduler.Reason onto the value the summary
// reports. The projection is the identity on V2-030's closed set and is written
// as an exhaustive switch on purpose: a constant added to internal/scheduler
// with no case here returns false, and the two-directional table test in
// allocation_test.go fails. This package defines no reason vocabulary of its
// own -- two independently maintained vocabularies would drift, and the drift
// would show up as an owner console explaining a wait with a reason the
// scheduler never gave.
func WaitingReasonFor(reason scheduler.Reason) (string, bool) {
	switch reason {
	case scheduler.ReasonNotReady:
		return string(scheduler.ReasonNotReady), true
	case scheduler.ReasonUnmetDependency:
		return string(scheduler.ReasonUnmetDependency), true
	case scheduler.ReasonRepositoryUnavailable:
		return string(scheduler.ReasonRepositoryUnavailable), true
	case scheduler.ReasonAlreadyOwned:
		return string(scheduler.ReasonAlreadyOwned), true
	case scheduler.ReasonResourceConflict:
		return string(scheduler.ReasonResourceConflict), true
	case scheduler.ReasonNoRunnerCapacity:
		return string(scheduler.ReasonNoRunnerCapacity), true
	case scheduler.ReasonNotExecutable:
		return string(scheduler.ReasonNotExecutable), true
	}
	return "", false
}

// WaitingReasonBuckets is every bucket key the summary reports, sorted, one per
// member of scheduler.AllReasons. Reporting all of them on every read -- zero
// included -- is what makes the key order fixed and the closed set visible.
func WaitingReasonBuckets() []string {
	out := make([]string, 0, len(scheduler.AllReasons))
	for _, reason := range scheduler.AllReasons {
		value, ok := WaitingReasonFor(reason)
		if !ok {
			// Unreachable while the projection is total, which the table test
			// asserts. Failing closed here rather than dropping the bucket
			// keeps an untranslated reason visible instead of invisible.
			value = string(reason)
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// emptyWaitingBuckets returns the fixed bucket map with every count at zero.
func emptyWaitingBuckets() map[string]int {
	out := map[string]int{}
	for _, key := range WaitingReasonBuckets() {
		out[key] = 0
	}
	return out
}

// ---------------------------------------------------------------------------
// The Snapshot builder
// ---------------------------------------------------------------------------

// The three named placeholders this modelling needs, and the reason each one
// exists. Every one of them is asserted absent from the marshalled response:
// the modelling choice must not leak into a surface an owner would then read as
// a real entity.
//
//   - AllocationSnapshotRepositoryID: there is one Installation and one
//     implicit Repository from the scheduler's point of view here. This task
//     deliberately does not model per-Repository allocation: that half belongs
//     to V2-030's local closure and V2-031's M7 gate.
//   - AllocationSnapshotPoolID and AllocationSnapshotProvider: there is no
//     enrolled-Runner registry to enumerate -- RunnerObservationRepository is
//     keyed by id with no list operation -- so the installation is modelled as
//     one pool. The per-Provider half belongs to V2-028 and is surfaced by
//     V2-067's GET /v1/providers.
const (
	AllocationSnapshotRepositoryID = "synthetic-installation-repository"
	AllocationSnapshotPoolID       = "synthetic-installation-pool"
	AllocationSnapshotProvider     = "synthetic-installation-provider"
)

// AllocationSnapshotPlaceholders is the complete list, so a scan over a
// response body can be written against the list rather than against three
// literals a later change could add a fourth to.
func AllocationSnapshotPlaceholders() []string {
	return []string{AllocationSnapshotRepositoryID, AllocationSnapshotPoolID, AllocationSnapshotProvider}
}

// The two contention-key namespaces. A Requirement contends for the Repository
// it is linked to; a Requirement with no link contends only with itself.
const (
	AllocationRepositoryResourcePrefix  = "repository:"
	AllocationRequirementResourcePrefix = "requirement:"
)

// AllocationContentionKey names the resource one Requirement contends for, and
// it is the one modelling choice in this builder that is not a placeholder.
//
// WHY THE LINKED REPOSITORY. The durable shared resource this system actually
// contends for is a Repository's working tree, and V2-071's write-once
// Requirement-to-Repository link is the only durable statement of which
// Requirement belongs to which Repository. Two ready Requirements linked to the
// same Repository therefore genuinely conflict, and the second is reported as
// resource-conflict rather than silently double-booked.
//
// WHY A REQUIREMENT WITH NO LINK CONTENDS ONLY WITH ITSELF. An absent link is an
// absence, not a shared repository: treating every unlinked Requirement as
// contending for one installation-wide resource would serialise the whole
// installation to a single Execution and make the concurrency limit
// unobservable, which is the opposite of what this surface exists to show.
//
// This is a contention key and nothing more. Snapshot.Repositories stays the
// single synthetic Installation Repository, so no per-Repository availability
// rule is introduced here, and no per-Repository figure is reported anywhere:
// the multi-Repository half belongs to V2-030's local closure and V2-031's M7
// gate.
func AllocationContentionKey(requirementID, repositoryID string) string {
	if repositoryID != "" {
		return AllocationRepositoryResourcePrefix + repositoryID
	}
	return AllocationRequirementResourcePrefix + requirementID
}

// AllocationSchedulerStatus maps a domain.RequirementStatus onto the scheduler's
// own three-value status. It is total over the eleven declared domain statuses
// and the mapping is deliberately conservative:
//
//   - ready maps to scheduler.StatusReady: this is the only schedulable status.
//   - completed maps to scheduler.StatusCompleted, which is what lets a
//     dependency be satisfied.
//   - active maps to scheduler.StatusAssigned: work is already in flight.
//   - every other status maps to the empty status. The scheduler declares no
//     value for "captured", "waiting", "paused" or "cancelled", and inventing
//     one would be adding a decision rule. The empty status fails
//     structurallyReady, so such a candidate is reported as not-ready, which is
//     exactly what it is.
func AllocationSchedulerStatus(status domain.RequirementStatus) scheduler.RequirementStatus {
	switch status {
	case domain.RequirementReady:
		return scheduler.StatusReady
	case domain.RequirementCompleted:
		return scheduler.StatusCompleted
	case domain.RequirementActive:
		return scheduler.StatusAssigned
	}
	return ""
}

// AllocationCreatedAt applies V2-073's declared mapping rule for a candidate
// with no recorded capture time, which V2-073 declared and left to this task to
// apply (docs/operations/scheduler-local.md, "The Requirement's capture time").
//
// THE RULE: a Requirement with no recorded capture time is ordered as if
// captured at the Snapshot's Now -- age zero -- never as an unbounded age.
//
// This is a rule, not a workaround. scheduler.ageSeconds clamps only the
// negative side, and now.Sub(time.Time{}) saturates at the maximum
// time.Duration, which is 9223372036 seconds -- about 292 years, because
// time.Duration is int64 nanoseconds. scheduler.legacyScore is
// legacyPriority(p)*300 + age, so a priority-100 candidate scores at most 30000
// from priority; a candidate handed the zero instant scores 9223372036 from age
// alone and outranks everything regardless of priority. Age zero is the
// conservative direction: a missing value makes a Requirement the least
// privileged rather than the most, so the failure mode of an absent value is a
// delayed Requirement rather than a starved queue.
//
// No substitute timestamp is manufactured here. The value is the Requirement's
// own recorded CapturedAt or the Snapshot's Now; it is never read from a second
// clock call and never derived from a scan of the event log.
func AllocationCreatedAt(r domain.Requirement, now time.Time) time.Time {
	if r.CaptureRecorded() {
		return r.CapturedAt
	}
	return now
}

// BuildAllocationSnapshot is the Snapshot builder. internal/scheduler has no
// production code that constructs a Snapshot, so the builder's owner is this
// package (V2-068), and this is it.
//
// It is a pure function of its arguments: no clock read, no store read, no
// package state. now must be the instant the caller's transaction was opened
// with.
//
// Shape, and why each part is what it is:
//
//   - Requirements: the candidates, each mapped by AllocationSchedulerStatus
//     and AllocationCreatedAt, each naming the one synthetic Repository and
//     requesting one Write resource named by AllocationContentionKey.
//   - Repositories: exactly one entry, the synthetic Installation Repository,
//     with no FailureCount and no IsolatedUntil. There is no durable source for
//     either today, and inventing one would be inventing a decision rule.
//   - Runners: exactly one pool entry whose Capacity is the effective limit and
//     whose Active is the ActiveExecutions figure the queue counters already
//     report.
//   - Claims: one per active Lease, owned by the Requirement the Lease's
//     Increment belongs to. These are read-only inputs; Decide only reads them.
//   - CandidateLimit: scheduler.MaxCandidates, the scheduler's own bound, and
//     never a bound this package invents.
//
// It fails closed above the bound rather than truncating.
func BuildAllocationSnapshot(now time.Time, limit, active int, candidates []domain.Requirement, links map[string]domain.RequirementRepositoryLink, claims []scheduler.Claim) (scheduler.Snapshot, error) {
	if now.IsZero() {
		return scheduler.Snapshot{}, errors.New("allocation snapshot requires a time")
	}
	if len(candidates) > scheduler.MaxCandidates {
		return scheduler.Snapshot{}, fmt.Errorf("%w: %d candidates", ErrAllocationCandidateBound, len(candidates))
	}
	if limit < 0 || active < 0 {
		return scheduler.Snapshot{}, errors.New("allocation snapshot requires a non-negative limit and active count")
	}
	rows := make([]scheduler.Requirement, 0, len(candidates))
	for _, r := range candidates {
		id := r.ID.String()
		rows = append(rows, scheduler.Requirement{
			ID:            id,
			RepositoryIDs: []string{AllocationSnapshotRepositoryID},
			CreatedAt:     AllocationCreatedAt(r, now),
			Status:        AllocationSchedulerStatus(r.Status),
			Resources:     []scheduler.ResourceRequest{{Name: AllocationContentionKey(id, links[id].RepositoryID.String()), Mode: scheduler.Write}},
		})
	}
	return scheduler.Snapshot{
		Requirements:   rows,
		Repositories:   []scheduler.Repository{{ID: AllocationSnapshotRepositoryID}},
		Runners:        []scheduler.Runner{{ID: AllocationSnapshotPoolID, Provider: AllocationSnapshotProvider, Capacity: limit, Active: active}},
		Claims:         append([]scheduler.Claim(nil), claims...),
		Now:            now,
		CandidateLimit: scheduler.MaxCandidates,
	}, nil
}

// AllocationExhaustion is the capacity verdict, computed from the accounting
// the caller itself supplied to the Snapshot and from nothing else. It never
// re-reads a candidate's reason value, so a candidate waiting on a resource
// conflict while capacity remains leaves exhausted false.
//
// Which limit is named when the installation is exhausted follows the source of
// the number that was reached: a limit an owner declared on a Control Intent
// revision is installation-concurrency; the design ceiling the pool falls back
// to when no owner ever declared one is the pool's own capacity, which is
// runner-capacity. Naming the design ceiling as an installation policy would
// report a number nobody chose as though somebody had.
// A non-positive pool capacity cannot admit anything. It is not reachable from
// a stored limit -- 1..20 is enforced on the way in -- and is treated as
// exhausted rather than as unlimited.
func AllocationExhaustion(active, limit int, source AllocationLimitSource) ExhaustionView {
	if limit > 0 && active < limit {
		return ExhaustionView{Exhausted: false, BindingLimit: BindingNone}
	}
	return ExhaustionView{Exhausted: true, BindingLimit: bindingFor(source)}
}

func bindingFor(source AllocationLimitSource) BindingLimit {
	if source == AllocationLimitFromControlRevision {
		return BindingInstallationConcurrency
	}
	return BindingRunnerCapacity
}

// ---------------------------------------------------------------------------
// Reading the effective limit
// ---------------------------------------------------------------------------

// EffectiveAllocationLimit resolves the limit in force, deterministically and
// without reading a clock: it is the limit attached to the greatest Control
// Intent revision that declared one, and a later revision that declared none
// does not clear it. An unrelated pause-claim intent must not silently reset the
// owner's allocation policy, which is exactly what "latest revision wins
// outright" would do.
//
// With no revision having ever declared one, the ceiling is
// docs/architecture/validation.md section 5's design ceiling and the source
// says so.
func (s *Service) EffectiveAllocationLimit(ctx context.Context) (int, AllocationLimitSource, domain.Revision, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return 0, "", 0, err
	}
	var limit int
	var source AllocationLimitSource
	var revision domain.Revision
	err := s.transact(ctx, func(u UnitOfWork) error {
		var e error
		limit, source, revision, e = effectiveAllocationLimit(ctx, u)
		return e
	})
	return limit, source, revision, err
}

func effectiveAllocationLimit(ctx context.Context, u UnitOfWork) (int, AllocationLimitSource, domain.Revision, error) {
	row, found, err := u.EffectiveAllocationLimit(ctx)
	if err != nil {
		return 0, "", 0, err
	}
	if !found {
		return AllocationLimitCeiling, AllocationLimitFromDesignCeiling, 0, nil
	}
	return row.InstallationConcurrentExecutions, AllocationLimitFromControlRevision, row.Revision, nil
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// QueueSummary answers GET /v1/queue/summary. The five existing counters come
// from QueueSummaryRepository exactly as before; the three allocation objects
// are computed in the same transaction from the state they describe.
//
// It calls scheduler.Decide and never scheduler.Apply, writes nothing, stages
// no outbox item and makes no mutation quota reservation. The only reservation
// is the one bounded read-transaction reservation every owner read already
// makes.
func (s *Service) QueueSummary(ctx context.Context) (QueueSummaryResponse, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return QueueSummaryResponse{}, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return QueueSummaryResponse{}, errors.New("clock returned zero time")
	}
	var out QueueSummaryResponse
	err := s.transact(ctx, func(u UnitOfWork) error {
		base, e := u.QueueSummary(ctx)
		if e != nil {
			return e
		}
		if base.ByRequirementStatus == nil {
			base.ByRequirementStatus = map[string]int{}
		}
		if base.ByIncrementStatus == nil {
			base.ByIncrementStatus = map[string]int{}
		}
		allocation, waiting, exhaustion, priority, e := allocationReport(ctx, u, now, base.ActiveExecutions)
		if e != nil {
			return e
		}
		out = QueueSummaryResponse{QueueSummary: base, Allocation: allocation, Waiting: waiting, Exhaustion: exhaustion, Priority: priority}
		return nil
	})
	if err != nil {
		return QueueSummaryResponse{}, err
	}
	return out, nil
}

// allocationReport is the whole derivation, inside the caller's transaction.
//
// Its read cost is bounded by constants that do not grow with the Requirement
// count beyond the scheduler's own candidate bound: one keyed side-table read
// for the effective limit, one bounded page of at most scheduler.MaxCandidates
// Requirements, one bounded active-Lease read, and one keyed Increment read per
// active Lease.
func allocationReport(ctx context.Context, u UnitOfWork, now time.Time, active int) (AllocationView, WaitingView, ExhaustionView, PriorityView, error) {
	limit, source, revision, err := effectiveAllocationLimit(ctx, u)
	if err != nil {
		return AllocationView{}, WaitingView{}, ExhaustionView{}, PriorityView{}, err
	}
	// moreCandidates was previously discarded. It is kept now because it is the
	// only truthful answer to "was the ranking below the whole Backlog?", and
	// V2-095 A7 forbids reporting a bounded projection as if it were complete.
	candidates, moreCandidates, err := u.RequirementsPage(ctx, "", scheduler.MaxCandidates)
	if err != nil {
		return AllocationView{}, WaitingView{}, ExhaustionView{}, PriorityView{}, err
	}
	ids := make([]string, 0, len(candidates))
	for _, r := range candidates {
		ids = append(ids, r.ID.String())
	}
	// One batch link read for exactly the ids on this page, the same bounded
	// read ListRequirementsPage already makes.
	links, err := u.RequirementRepositoryLinks(ctx, ids)
	if err != nil {
		return AllocationView{}, WaitingView{}, ExhaustionView{}, PriorityView{}, err
	}
	claims, err := allocationClaims(ctx, u)
	if err != nil {
		return AllocationView{}, WaitingView{}, ExhaustionView{}, PriorityView{}, err
	}
	snapshot, err := BuildAllocationSnapshot(now, limit, active, candidates, links, claims)
	if err != nil {
		return AllocationView{}, WaitingView{}, ExhaustionView{}, PriorityView{}, err
	}
	plan, err := scheduler.Decide(snapshot)
	if err != nil {
		return AllocationView{}, WaitingView{}, ExhaustionView{}, PriorityView{}, err
	}
	waiting := WaitingView{ByReason: emptyWaitingBuckets()}
	priority := PriorityView{
		Bounded:          moreCandidates,
		CandidatesRanked: len(plan.Decisions),
		CandidateBound:   scheduler.MaxCandidates,
		Entries:          make([]PriorityEntryView, 0, len(plan.Decisions)),
	}
	priority.Reason = priorityReason(len(plan.Decisions), scheduler.MaxCandidates, moreCandidates)
	for _, decision := range plan.Decisions {
		entry := PriorityEntryView{
			RequirementID: decision.RequirementID,
			Rank:          decision.Rank,
			Assigned:      decision.Assigned,
			Score:         decision.Score,
			ScoreInputs:   priorityScoreInputsView(decision.Inputs),
		}
		if decision.Inputs.UsedAssessment {
			priority.AssessmentSupplied = true
		}
		if !decision.Assigned {
			// ONE mapping, shared with the histogram below, so the projected
			// reason and the bucket key can never name different vocabularies.
			// An unknown reason refuses the whole read: the pre-existing
			// fail-closed behaviour at the waiting-bucket mapping is preserved
			// exactly, and it now also guards the per-candidate projection.
			bucket, ok := WaitingReasonFor(decision.Reason)
			if !ok {
				return AllocationView{}, WaitingView{}, ExhaustionView{}, PriorityView{}, fmt.Errorf("scheduler reported reason %q, which this package has no bucket for", decision.Reason)
			}
			entry.Reason = bucket
			waiting.ByReason[bucket]++
			waiting.Total++
		}
		priority.Entries = append(priority.Entries, entry)
	}
	if priority.AssessmentSupplied {
		priority.AssessmentNote = priorityAssessmentPresentNote
	} else {
		priority.AssessmentNote = priorityAssessmentAbsentNote
	}
	remaining := limit - active
	if remaining < 0 {
		remaining = 0
	}
	allocation := AllocationView{
		Limit:              limit,
		LimitSource:        source,
		ControlRevision:    revision,
		Active:             active,
		Remaining:          remaining,
		PlannedAssignments: len(plan.Assignments),
	}
	return allocation, waiting, AllocationExhaustion(active, limit, source), priority, nil
}

// allocationClaims builds one read-only scheduler.Claim per active Lease. The
// owner is the Requirement the Lease's Increment belongs to, read inside the
// same transaction, so a Requirement that already holds a Lease is reported as
// already-owned rather than as competing with itself. A Lease whose Increment
// is gone yields a Claim with no owner: the resource is still held, and saying
// so is more honest than dropping the Claim and reporting free capacity.
func allocationClaims(ctx context.Context, u UnitOfWork) ([]scheduler.Claim, error) {
	leases, err := u.ActiveLeases(ctx, AllocationLeaseBound+1)
	if err != nil {
		return nil, err
	}
	if len(leases) > AllocationLeaseBound {
		return nil, ErrAllocationLeaseBound
	}
	out := make([]scheduler.Claim, 0, len(leases))
	for _, lease := range leases {
		owner := ""
		repository := ""
		increment, found, e := u.Increment(ctx, lease.IncrementID.String())
		if e != nil {
			return nil, e
		}
		if found {
			owner = increment.RequirementID.String()
			link, linked, x := u.RequirementRepositoryLink(ctx, owner)
			if x != nil {
				return nil, x
			}
			if linked {
				repository = link.RepositoryID.String()
			}
		}
		out = append(out, scheduler.Claim{
			Resource:     AllocationContentionKey(owner, repository),
			Owner:        owner,
			RepositoryID: AllocationSnapshotRepositoryID,
			Mode:         scheduler.Write,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out, nil
}
