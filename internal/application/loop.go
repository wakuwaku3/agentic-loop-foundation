// The Loop's own bounded pass (V2-091).
//
// THE MEASUREMENT THIS FILE EXISTS FOR. Measured at 848d899:
// domain.RequirementWait, domain.RequirementRecover and
// domain.RequirementEvaluate each had ZERO non-test occurrences outside
// internal/domain/model.go; internal/application/service.go:993 was the only
// non-test line constructing a domain.Increment and its only non-test caller
// was internal/runner/orchestrator.go:59, whose Orchestrator is built at
// exactly two sites and both are in internal/runner/orchestrator_test.go; and
// release.Pipeline.Promote (internal/release/pipeline.go:38) had zero non-test
// callers, so domain.StableReleaseProof -- whose sole constructor chain is
// domain.CompletePromotionWithProof <- release.PromotionGate <-
// release.Pipeline.Promote -- was unobtainable in any running process. Four
// unreachable transition families, one cause: no component that HELD AN
// IDENTITY was asking.
//
// THE FRAMING CORRECTION THAT DECIDES THE SHAPE. The identity is not missing.
// internal/api/api.go:114 already mints it with application.LoopCaller and
// :119 already attaches it with application.ContextWithCaller to the context
// the reconcile branch authorises; cmd/control-plane/main.go:199-207 is that
// closure. What was missing is a PASS THAT USES IT. So this file adds no
// caller producer, no authority, no transport, no port and no domain command:
// it adds one method gated callerActor(ctx, RoleScheduler) and four stages,
// each carrying the observation that justified it.
//
// WHY NOT internal/reconciler. reconciler.recoverOne's actor is
// domain.NewActorID("reconciler"), a string the component makes up for itself,
// and internal/reconciler holds no application.Caller at all, so a Requirement
// transition placed there would be given an identity it is not, in a package
// that structurally cannot hold the right one. The reconciler's OUTPUT -- an
// Execution in domain.ExecutionLost -- is read here as an OBSERVATION instead,
// and internal/reconciler stays byte-unchanged.
//
// WHERE THE OBSERVATION IS RECORDED, and the boundary that forced it.
// application.Event has eight fields and no payload, and both
// internal/application/ports.go and internal/store/** are outside this task's
// allowed paths, so an Event field cannot be added and could not be persisted
// if it were. Every transition below therefore carries its justifying
// observation on the DURABLE RECORD the same s.record call writes -- the
// idempotency response, in the same transaction as the Event -- and the Event's
// own Type names the transition. A transition with no justification is
// visible in the record, not only in the code.
package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/release"
	"github.com/takushi/agentic-loop-foundation/v2/internal/scheduler"
)

// ---------------------------------------------------------------------------
// The caps, and why each is a named constant rather than a literal
// ---------------------------------------------------------------------------

// LoopStageCap bounds how many transitions ONE stage of ONE pass may issue.
// It is not the page bound: the page is scheduler.MaxCandidates (100) and this
// is the number of MUTATIONS a single tick is allowed to make, because every
// transition is its own mutation transaction and quota.MutationUsage reserves
// Reads 32 / Writes 16 for each one. A pass that transitioned a whole page
// would reserve 100 * 16 writes against quota.DefaultBudget's 10000 in one
// tick, which is 16% of a day's write budget per stage per tick.
const LoopStageCap = 8

// LoopPlanCap bounds the Increment-creation stage separately and lower,
// because ONE planned Increment costs TWO mutations (Service.Plan then
// Service.Prepare) rather than one.
const LoopPlanCap = 4

// LoopOfferedWorkCap bounds the runner-role offer read. It is a READ bound and
// deliberately independent of the transition caps: a Runner may be offered more
// than one tick's worth of planning, and the offer mutates nothing.
const LoopOfferedWorkCap = 16

// LoopIncrementReadBound bounds the ONE batched Increment read this pass makes.
//
// IT IS NOT scheduler.MaxCandidates, and the reason is a MEASURED ADAPTER FACT.
// The port is named IncrementsForRequirements, and its two pre-existing non-test
// callers disagree about what it takes: internal/application/readmodels.go:307
// passes INCREMENT ids (built from Requirement.Increments) and
// internal/reconciler/orphan.go:67 passes REQUIREMENT ids. The two adapters
// disagree too -- internal/store/memory/store.go:466 filters on EITHER the
// Increment's own id or its RequirementID, while
// internal/store/firestore/store.go:707 is a batch GET of one document per id
// from the `increments` collection, so it answers ONLY to Increment ids.
//
// This pass therefore passes INCREMENT ids, derived from the page's own
// Requirement.Increments exactly as readmodels.go does. That is the form both
// adapters answer, so a count measured against the in-memory adapter is the
// count the Firestore adapter reserves -- and it needs no port change, no store
// edit and no new read: internal/application/ports.go and internal/store/** are
// both outside this task's allowed paths.
//
// The bound is separate because the number of Increments on a page of
// Requirements is not bounded by the number of Requirements. A page whose
// Increments would exceed it is TRIMMED at a Requirement boundary and the cursor
// stops there, so every conclusion the pass draws about a Requirement is drawn
// from that Requirement's COMPLETE Increment set -- a partially-read set would
// make stage D plan a second Increment beside one it did not see, and would make
// stage P read "all released" off a truncated list.
const LoopIncrementReadBound = 200

// MaxDriverClaims is declared here, beside the offer it bounds, so the driver
// in internal/runner and the route that feeds it cannot disagree about how much
// work one bounded pass may take. internal/runner reads it through this
// package, which it already imports.
const MaxDriverClaims = 4

// ---------------------------------------------------------------------------
// The request, and what it deliberately cannot say
// ---------------------------------------------------------------------------

// LoopPassRequest carries a request-id NAMESPACE and an opaque CURSOR and
// nothing else. There is deliberately no control revision (domain.Permit
// requires the authoritative one, resolved inside the transaction), no
// repository id (ServiceConfig holds none for the reason its own comment
// gives), no timestamp (the pass reads its clock exactly once), no limit
// override and NO FIELD THAT LETS A CALLER PICK WHICH REQUIREMENT MOVES. A
// caller that could name a Requirement would be an owner wearing the
// scheduler's identity.
type LoopPassRequest struct {
	// RequestID is the namespace every per-transition idempotency key is
	// derived from, so a repeated tick with the same namespace REPLAYS rather
	// than re-transitions.
	RequestID string
	// Cursor is the opaque page cursor a previous pass's report handed back.
	// The empty string starts at the beginning.
	Cursor string
}

// ---------------------------------------------------------------------------
// The report: what moved, what was examined, and what was NOT
// ---------------------------------------------------------------------------

// LoopStageReport is one stage's accounting. Deferred is the number of
// candidates the stage IDENTIFIED and did not act on because the cap was
// reached -- reported rather than dropped, so a saturated pass is visible.
type LoopStageReport struct {
	Transitions int `json:"transitions"`
	Cap         int `json:"cap"`
	Deferred    int `json:"deferred"`
}

// LoopPromotionOutcome is the closed three-value verdict of the promotion
// stage. skipped-not-configured is NOT a failure: a process with no explicitly
// configured release source root can report no release version at all, and
// defaulting a root would make it report a version it was not assembled from
// (internal/application/release_surface.go:17-23).
type LoopPromotionOutcome string

const (
	LoopPromotionPromoted             LoopPromotionOutcome = "promoted"
	LoopPromotionRefused              LoopPromotionOutcome = "refused"
	LoopPromotionSkippedNotConfigured LoopPromotionOutcome = "skipped-not-configured"
)

// LoopPromotionReport carries the verdict, the refusal reason when there is
// one, and the identity of the candidate that was promoted. It never carries
// the domain.StableReleaseProof: the proof does not leave the transaction that
// earned it.
type LoopPromotionReport struct {
	Outcome     LoopPromotionOutcome `json:"outcome"`
	Reason      string               `json:"reason,omitempty"`
	CandidateID string               `json:"candidate_id,omitempty"`
	Evaluations int                  `json:"evaluations"`
	Completions int                  `json:"completions"`
	Cap         int                  `json:"cap"`
	Deferred    int                  `json:"deferred"`
}

// LoopWaitObservation is the justification for exactly one waiting transition.
type LoopWaitObservation struct {
	RequirementID string         `json:"requirement_id"`
	Reason        string         `json:"scheduler_reason"`
	Rank          int            `json:"scheduler_rank"`
	Version       domain.Version `json:"version"`
}

// LoopRecoverObservation is the justification for exactly one recovering
// transition: the Increment and the Execution that were lost.
type LoopRecoverObservation struct {
	RequirementID string         `json:"requirement_id"`
	IncrementID   string         `json:"increment_id"`
	ExecutionID   string         `json:"execution_id"`
	Version       domain.Version `json:"version"`
}

// LoopPlanObservation is the justification for exactly one Increment: the
// scheduler ASSIGNED this Requirement.
type LoopPlanObservation struct {
	RequirementID string `json:"requirement_id"`
	IncrementID   string `json:"increment_id"`
	Rank          int    `json:"scheduler_rank"`
}

// LoopCompletionObservation is the justification for exactly one completion:
// the candidate a promotion earned a proof for, and that candidate's bundle
// digest, which is what the completed Requirement's StableSnapshot records.
type LoopCompletionObservation struct {
	RequirementID string         `json:"requirement_id"`
	CandidateID   string         `json:"candidate_id"`
	BundleDigest  string         `json:"bundle_digest"`
	Version       domain.Version `json:"version"`
}

// LoopPassReport is what one pass says about itself. RequirementsExamined,
// MoreRequirementsExist and NextCursor together answer the question a report
// that only counted transitions could not: WHAT DID THIS PASS NOT SEE. What
// one pass leaves unexamined is exactly the Requirements after NextCursor, and
// the way a later pass reaches them is the next tick carrying that cursor.
type LoopPassReport struct {
	At                    time.Time `json:"at"`
	PageSize              int       `json:"page_size"`
	RequirementsExamined  int       `json:"requirements_examined"`
	MoreRequirementsExist bool      `json:"more_requirements_exist"`
	NextCursor            string    `json:"next_cursor,omitempty"`
	// IncrementsExamined and IncrementReadBound report the second bounded read,
	// and TrimmedForIncrementBound says whether the page was cut short at a
	// Requirement boundary because the next Requirement's Increments would have
	// exceeded it. All three are reported so a trimmed page is auditable rather
	// than reading as "covered everything".
	IncrementsExamined       int  `json:"increments_examined"`
	IncrementReadBound       int  `json:"increment_read_bound"`
	TrimmedForIncrementBound bool `json:"trimmed_for_increment_bound"`

	Wait      LoopStageReport     `json:"wait"`
	Recover   LoopStageReport     `json:"recover"`
	Plan      LoopStageReport     `json:"plan"`
	Promotion LoopPromotionReport `json:"promotion"`

	WaitObservations       []LoopWaitObservation       `json:"wait_observations,omitempty"`
	RecoverObservations    []LoopRecoverObservation    `json:"recover_observations,omitempty"`
	PlanObservations       []LoopPlanObservation       `json:"plan_observations,omitempty"`
	CompletionObservations []LoopCompletionObservation `json:"completion_observations,omitempty"`
}

// ---------------------------------------------------------------------------
// Stage W's reason set: DERIVED from the scheduler's own closed set
// ---------------------------------------------------------------------------

// loopWaitReasonJustifiesWait is the closed switch over ALL SEVEN members of
// scheduler.AllReasons. The set is DERIVED, not chosen, and the derivation is
// what each arm's comment records. Writing it as an exhaustive switch with an
// explicit arm per member is the point: a reason added to internal/scheduler
// with no arm here reaches the final return and FAILS the closed-set table in
// loop_test.go, rather than silently joining or leaving the set.
//
// docs/architecture/domain-model.md:267 defines waiting as
// "dependency、capacity、time条件を自動待機" -- a Requirement
// awaiting a CONDITION. So the question each arm answers is: is this reason a
// condition being awaited?
func loopWaitReasonJustifiesWait(reason scheduler.Reason) bool {
	switch reason {
	case scheduler.ReasonResourceConflict:
		// YES. Two ready Requirements linked to the same Repository share the
		// contention key AllocationContentionKey builds
		// ("repository:"+id, internal/application/allocation.go:362), and the
		// second genuinely awaits the first's release of that resource. This is
		// the contention case the key was built to report.
		return true
	case scheduler.ReasonNoRunnerCapacity:
		// YES. internal/scheduler/scheduler.go:164 attaches this when
		// chooseRunner finds no spare capacity. This is the product's own
		// "capacity" condition, named at domain-model.md:267.
		return true
	case scheduler.ReasonNotReady:
		// NO, for two independent reasons. It is not a condition being awaited
		// at all -- it means the candidate fails scheduler.structurallyReady on
		// its own terms (internal/scheduler/scheduler.go:192). And every
		// Requirement it can be attached to whose domain status is NOT ready is
		// in a status domain.DecideRequirement refuses RequirementWait from:
		// internal/domain/model.go:580-583 admits ready, active and recovering
		// only.
		return false
	case scheduler.ReasonAlreadyOwned:
		// NO. The Requirement already owns a Claim -- work is IN FLIGHT, which
		// is the opposite of a wait. Moving it to waiting would report a
		// Requirement with a live Lease as awaiting a condition.
		return false
	case scheduler.ReasonUnmetDependency:
		// NO, and it is UNREACHABLE from this pass's snapshot: the rows
		// BuildAllocationSnapshot builds
		// (internal/application/allocation.go:455-464) carry no Dependencies
		// field at all, so scheduler.dependenciesMet is vacuously true. Were a
		// future snapshot to carry dependencies, domain-model.md:267 names
		// dependency FIRST among the conditions, so this arm is the one most
		// likely to become true -- which is exactly why it is written out.
		return false
	case scheduler.ReasonRepositoryUnavailable:
		// NO, and UNREACHABLE: Snapshot.Repositories is the single synthetic
		// Installation Repository with no FailureCount and no IsolatedUntil
		// (allocation.go:471), so scheduler.repositoriesAvailable cannot report
		// false for a candidate that names it.
		return false
	case scheduler.ReasonNotExecutable:
		// NO, and UNREACHABLE: BuildAllocationSnapshot never sets Assessment,
		// so the domain.PriorityAssessment.Executable branch at
		// internal/scheduler/scheduler.go cannot fire.
		return false
	}
	// A reason with no arm above. Refusing it is the fail-closed direction: an
	// unlisted reason must not become a transition.
	return false
}

// ---------------------------------------------------------------------------
// The Increment terminality question stage D asks
// ---------------------------------------------------------------------------

// loopIncrementIsTerminal reports whether an Increment can no longer be worked
// on, so stage D can tell "this Requirement has work in flight" from "this
// Requirement has nothing planned". The switch is exhaustive over all TWELVE
// declared domain.IncrementStatus values (internal/domain/model.go's
// validIncrementStatus), with a reason per arm, and loop_test.go asserts the
// axis is complete by deriving it from internal/domain by go/ast.
func loopIncrementIsTerminal(status domain.IncrementStatus) bool {
	switch status {
	case domain.IncrementReleased:
		// Terminal: the work reached Stable's route. It is also the status the
		// promotion stage requires of ALL of a Requirement's Increments.
		return true
	case domain.IncrementFailed:
		// Terminal for the purpose of planning: a failed Increment is not work
		// in flight. What a Requirement with a failed Increment should BECOME
		// (domain-model.md:275 says recovering, waiting or needs-input) is
		// recorded in this task's boundary table and is not decided here.
		return true
	case domain.IncrementAbandoned, domain.IncrementCancelled:
		// Terminal by name.
		return true
	case domain.IncrementProposed, domain.IncrementReady, domain.IncrementLeased,
		domain.IncrementExecuting, domain.IncrementVerifying, domain.IncrementIntegrated,
		domain.IncrementPreviewValidating, domain.IncrementAccepted:
		// NOT terminal: each of these can still advance, so a Requirement
		// holding one already has work and gets no second Increment.
		return false
	}
	// An undeclared status. Treating it as NON-terminal is the fail-closed
	// direction here, because the consequence is "plan nothing" rather than
	// "plan a second Increment beside an unknown one".
	return false
}

// ---------------------------------------------------------------------------
// The bounded observation this pass acts on
// ---------------------------------------------------------------------------

// loopLostExecution is the recovering stage's observation for one Requirement.
type loopLostExecution struct {
	incrementID string
	executionID string
}

// loopObservation is everything ONE pass read, in ONE read transaction. It is
// a value: no stage re-reads the store to decide, and every stage's write
// transaction re-verifies the fact it is about to act on, so a race produces no
// transition rather than a wrong one.
type loopObservation struct {
	page       []domain.Requirement
	examined   int
	more       bool
	nextCursor string
	// trimmedForIncrementBound records that the page was cut short at a
	// Requirement boundary because the next Requirement's Increments would have
	// exceeded LoopIncrementReadBound. It is reported, never hidden.
	trimmedForIncrementBound bool
	incrementsExamined       int
	decisions                map[string]scheduler.Decision
	lost                     map[string]loopLostExecution
	inFlight                 map[string]bool
	increments               map[string][]domain.Increment
	readyOffered             []LoopOfferedIncrement
}

// loopObserve makes the pass's fixed set of bounded reads. NONE of them grows
// with the Requirement count:
//
//   - one u.RequirementsPage(ctx, cursor, scheduler.MaxCandidates), KEEPING
//     BOTH RETURN VALUES. allocationReport at
//     internal/application/allocation.go:603 discards the `more` flag, so the
//     owner queue read cannot say whether it saw everything; that file is
//     outside this task's allowed paths, so this pass makes its own call rather
//     than repairing that line, and the discarded flag is recorded as a
//     measured defect this task does not fix.
//   - one u.RequirementRepositoryLinks batch over exactly those ids.
//   - one u.ActiveLeases(ctx, AllocationLeaseBound+1) through allocationClaims,
//     which FAILS CLOSED above 100 with ErrAllocationLeaseBound.
//   - one u.IncrementsForRequirements batch over the same ids.
//   - one u.ExecutionsForIncrements batch over exactly those Increments' ids.
//
// BuildAllocationSnapshot fails closed above scheduler.MaxCandidates with
// ErrAllocationCandidateBound rather than truncating; that error is RETURNED,
// never caught and continued past.
func loopObserve(ctx context.Context, u UnitOfWork, now time.Time, cursor string) (loopObservation, error) {
	var obs loopObservation
	limit, source, _, err := effectiveAllocationLimit(ctx, u)
	if err != nil {
		return obs, err
	}
	_ = source
	base, err := u.QueueSummary(ctx)
	if err != nil {
		return obs, err
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return obs, err
	}
	page, more, err := u.RequirementsPage(ctx, after, scheduler.MaxCandidates)
	if err != nil {
		return obs, err
	}
	// THE INCREMENT-ID ACCOUNTING COMES FIRST, before anything is derived from
	// the page, because it can TRIM the page. Trimming after the snapshot was
	// built would leave decisions attached to Requirements the pass then refused
	// to examine.
	incrementIDs := make([]string, 0, LoopIncrementReadBound)
	included := 0
	for _, r := range page {
		own := r.Increments
		if len(own) > LoopIncrementReadBound {
			// ONE Requirement whose own Increment set exceeds the whole bound
			// cannot be examined completely by any page size, so this fails
			// CLOSED rather than drawing a conclusion from a partial set.
			return obs, fmt.Errorf("requirement %s holds %d Increments, which exceeds the bounded Increment read of %d", r.ID, len(own), LoopIncrementReadBound)
		}
		if len(incrementIDs)+len(own) > LoopIncrementReadBound {
			obs.trimmedForIncrementBound = true
			break
		}
		for _, id := range own {
			incrementIDs = append(incrementIDs, id.String())
		}
		included++
	}
	if obs.trimmedForIncrementBound {
		page = page[:included]
		// The remainder is genuinely unexamined, so the report must say so and
		// the cursor must stop here -- exactly the same truth the page's own
		// `more` flag carries.
		more = true
	}
	obs.page = page
	obs.examined = len(page)
	obs.more = more
	obs.incrementsExamined = len(incrementIDs)
	if more && len(page) != 0 {
		obs.nextCursor = encodeCursor(page[len(page)-1].ID.String())
	}
	ids := make([]string, 0, len(page))
	for _, r := range page {
		ids = append(ids, r.ID.String())
	}
	links, err := u.RequirementRepositoryLinks(ctx, ids)
	if err != nil {
		return obs, err
	}
	claims, err := allocationClaims(ctx, u)
	if err != nil {
		return obs, err
	}
	snapshot, err := BuildAllocationSnapshot(now, limit, base.ActiveExecutions, page, links, claims)
	if err != nil {
		return obs, err
	}
	plan, err := scheduler.Decide(snapshot)
	if err != nil {
		return obs, err
	}
	obs.decisions = make(map[string]scheduler.Decision, len(plan.Decisions))
	for _, d := range plan.Decisions {
		obs.decisions[d.RequirementID] = d
	}
	obs.lost = map[string]loopLostExecution{}
	obs.inFlight = map[string]bool{}
	obs.increments = map[string][]domain.Increment{}
	if len(ids) == 0 {
		return obs, nil
	}
	onPage := map[string]bool{}
	for _, id := range ids {
		onPage[id] = true
	}
	incrementOwner := make(map[string]string, len(incrementIDs))
	if len(incrementIDs) != 0 {
		sort.Strings(incrementIDs)
		increments, e := u.IncrementsForRequirements(ctx, incrementIDs)
		if e != nil {
			return obs, e
		}
		for _, inc := range increments {
			rid := inc.RequirementID.String()
			if !onPage[rid] {
				continue
			}
			obs.increments[rid] = append(obs.increments[rid], inc)
			if !loopIncrementIsTerminal(inc.Status) {
				obs.inFlight[rid] = true
			}
			incrementOwner[inc.ID.String()] = rid
		}
		for rid := range obs.increments {
			sort.Slice(obs.increments[rid], func(i, j int) bool {
				return obs.increments[rid][i].ID.String() < obs.increments[rid][j].ID.String()
			})
		}
	}
	// The offer read is derived HERE, from the same page, so the route a Runner
	// reads and the pass that plans the work cannot disagree about what is
	// claimable. The parent-admits-work filter is V2-089's own switch,
	// requirementStatusAdmitsClaim, read and not re-expressed: offering an
	// Increment whose parent refuses a claim would offer work the very next
	// call refuses.
	parents := make(map[string]domain.Requirement, len(page))
	for _, r := range page {
		parents[r.ID.String()] = r
	}
	for _, rid := range ids {
		if !requirementStatusAdmitsClaim(parents[rid].Status) {
			continue
		}
		for _, inc := range obs.increments[rid] {
			if inc.Status != domain.IncrementReady {
				continue
			}
			if len(obs.readyOffered) >= LoopOfferedWorkCap {
				break
			}
			obs.readyOffered = append(obs.readyOffered, LoopOfferedIncrement{
				RequirementID:            rid,
				IncrementID:              inc.ID.String(),
				ExpectedIncrementVersion: inc.Version,
			})
		}
	}
	if len(incrementIDs) == 0 {
		return obs, nil
	}
	// ExecutionsForIncrements takes INCREMENT ids on BOTH adapters -- measured:
	// internal/store/memory/store.go:485 filters on IncrementID and
	// internal/store/firestore/store.go:738 queries the index_increment_id
	// field -- so the same list serves both reads.
	executions, err := u.ExecutionsForIncrements(ctx, incrementIDs)
	if err != nil {
		return obs, err
	}
	for _, e := range executions {
		if e.Status != domain.ExecutionLost {
			continue
		}
		rid := incrementOwner[e.IncrementID.String()]
		if rid == "" {
			continue
		}
		if _, seen := obs.lost[rid]; seen {
			continue
		}
		obs.lost[rid] = loopLostExecution{incrementID: e.IncrementID.String(), executionID: e.ID.String()}
	}
	return obs, nil
}

// ---------------------------------------------------------------------------
// The pass
// ---------------------------------------------------------------------------

// loopSkippable reports whether one candidate's refusal is a fact about that
// candidate rather than a failure of the pass. A stop mode denying a plan, a
// Requirement whose version moved between the read and the write, and a
// transition the domain refuses are all legitimate NON-events: the pass reports
// zero for that candidate and carries on. Anything else -- a store error, a
// quota refusal, a bound exceeded -- aborts the pass and is returned, because
// continuing past it would make the report a claim about state the pass could
// not read.
func loopSkippable(err error) bool {
	return errors.Is(err, domain.ErrStaleVersion) ||
		errors.Is(err, domain.ErrInvalidTransition) ||
		errors.Is(err, domain.ErrControlDenied) ||
		errors.Is(err, ErrNotFound)
}

// NOTE ON WHAT loopSkippable DELIBERATELY OMITS. ErrRequirementNotClaimable is
// NOT listed: V2-089's own guard,
// TestTheNotClaimableRefusalHasExactlyOneIssuerAndOneDefinition in
// internal/application/claimable_test.go, asserts that error is returned from
// exactly ONE non-test function, and this pass never calls Service.Claim, so it
// can never see it. Listing it would have widened that closed set for no
// measured reason -- which is the shape v2-task-dag.md 12.12 forbids.

// LoopPass is the Loop's own bounded pass. It is gated on RoleScheduler and on
// that role ALONE: not RoleOwner, because an owner driving it would be the
// owner's authority wearing the Loop's identity and the whole point of V2-086's
// LoopCaller is that the Loop acts as itself; and not RoleRunner, because a
// Runner deciding which Requirement waits is the self-naming defect
// internal/runner/orchestrator.go:54 already commits. It constructs no
// application.Caller of its own -- application.LoopCaller stays the single
// sanctioned producer, and internal/application/caller.go is outside this
// task's allowed paths so it cannot be duplicated here.
//
// It reads its clock EXACTLY ONCE and passes that one instant to every stage
// and to every domain command's At, so two stages of one pass cannot disagree
// about when the pass happened.
func (s *Service) LoopPass(ctx context.Context, req LoopPassRequest) (LoopPassReport, error) {
	_, actor, err := callerActor(ctx, RoleScheduler)
	if err != nil {
		return LoopPassReport{}, err
	}
	if err = requireRequest(req.RequestID); err != nil {
		return LoopPassReport{}, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return LoopPassReport{}, errors.New("clock returned zero time")
	}
	report := LoopPassReport{At: now.UTC(), PageSize: scheduler.MaxCandidates}
	report.Wait.Cap = LoopStageCap
	report.Recover.Cap = LoopStageCap
	report.Plan.Cap = LoopPlanCap
	report.Promotion.Cap = LoopStageCap
	report.Promotion.Outcome = LoopPromotionSkippedNotConfigured

	var obs loopObservation
	if err = s.transact(ctx, func(u UnitOfWork) error {
		var e error
		obs, e = loopObserve(ctx, u, now, req.Cursor)
		return e
	}); err != nil {
		return report, err
	}
	report.RequirementsExamined = obs.examined
	report.MoreRequirementsExist = obs.more
	report.NextCursor = obs.nextCursor
	report.IncrementsExamined = obs.incrementsExamined
	report.IncrementReadBound = LoopIncrementReadBound
	report.TrimmedForIncrementBound = obs.trimmedForIncrementBound

	if err = s.loopStageWait(ctx, req, actor, now, obs, &report); err != nil {
		return report, err
	}
	if err = s.loopStageRecover(ctx, req, actor, now, obs, &report); err != nil {
		return report, err
	}
	if err = s.loopStagePlan(ctx, req, obs, &report); err != nil {
		return report, err
	}
	if err = s.loopStagePromote(ctx, req, actor, now, obs, &report); err != nil {
		return report, err
	}
	return report, nil
}

// loopStageWait issues domain.RequirementWait for a Requirement whose domain
// status is domain.RequirementReady and whose scheduler.Decision is unassigned
// with a Reason loopWaitReasonJustifiesWait admits.
//
// WHY `ready` AND NOTHING ELSE, as a measurement rather than a choice.
// AllocationSchedulerStatus (internal/application/allocation.go:382) maps
// domain ready to scheduler.StatusReady, active to StatusAssigned, completed to
// StatusCompleted and EVERY OTHER status to the empty status; and
// scheduler.structurallyReady (internal/scheduler/scheduler.go:192) returns
// false unless Status == StatusReady. So the scheduler can only ever attach
// resource-conflict or no-runner-capacity to a Requirement whose domain status
// is ready. domain.DecideRequirement admits RequirementWait from ready, active
// AND recovering (internal/domain/model.go:580-583); the other two stay
// UNISSUED, which is recorded here and asserted nowhere.
func (s *Service) loopStageWait(ctx context.Context, req LoopPassRequest, actor domain.ActorID, now time.Time, obs loopObservation, report *LoopPassReport) error {
	for _, r := range obs.page {
		id := r.ID.String()
		if r.Status != domain.RequirementReady {
			continue
		}
		decision, ok := obs.decisions[id]
		if !ok || decision.Assigned {
			continue
		}
		if !loopWaitReasonJustifiesWait(decision.Reason) {
			continue
		}
		if report.Wait.Transitions >= LoopStageCap {
			report.Wait.Deferred++
			continue
		}
		observation := LoopWaitObservation{RequirementID: id, Reason: string(decision.Reason), Rank: decision.Rank}
		moved, version, err := s.loopRequirementTransition(ctx, req, actor, now, "wait", "loop-wait", "requirement.waiting", id,
			domain.RequirementWait, domain.RequirementReady, func(v domain.Version) any {
				o := observation
				o.Version = v
				return o
			})
		if err != nil {
			if loopSkippable(err) {
				continue
			}
			return err
		}
		if !moved {
			continue
		}
		observation.Version = version
		report.Wait.Transitions++
		report.WaitObservations = append(report.WaitObservations, observation)
	}
	return nil
}

// loopStageRecover issues domain.RequirementRecover for a Requirement in
// domain.RequirementActive or domain.RequirementWaiting one of whose Increments
// has an Execution in domain.ExecutionLost -- the artefact the shipped
// reconciler already writes when it expires an Active Lease past its TTL and
// calls domain.MarkExecutionLost. docs/product/user-facing-spec.md:275 defines
// recovering as "failureまたはRunner消失後、安全な再開を行って
// いる", and a lost Execution is exactly the Runner-disappeared half.
//
// An Execution in domain.ExecutionFailed rather than lost produces NO
// transition. docs/architecture/domain-model.md:275 says such a Requirement
// should become recovering, waiting or needs-input; that is this task's
// recorded boundary, not a defect it fixes.
func (s *Service) loopStageRecover(ctx context.Context, req LoopPassRequest, actor domain.ActorID, now time.Time, obs loopObservation, report *LoopPassReport) error {
	for _, r := range obs.page {
		id := r.ID.String()
		if r.Status != domain.RequirementActive && r.Status != domain.RequirementWaiting {
			continue
		}
		lost, ok := obs.lost[id]
		if !ok {
			continue
		}
		if report.Recover.Transitions >= LoopStageCap {
			report.Recover.Deferred++
			continue
		}
		observation := LoopRecoverObservation{RequirementID: id, IncrementID: lost.incrementID, ExecutionID: lost.executionID}
		moved, version, err := s.loopRequirementTransition(ctx, req, actor, now, "recover", "loop-recover", "requirement.recovering", id,
			domain.RequirementRecover, r.Status, func(v domain.Version) any {
				o := observation
				o.Version = v
				return o
			})
		if err != nil {
			if loopSkippable(err) {
				continue
			}
			return err
		}
		if !moved {
			continue
		}
		observation.Version = version
		report.Recover.Transitions++
		report.RecoverObservations = append(report.RecoverObservations, observation)
	}
	return nil
}

// loopRequirementTransition issues ONE domain Requirement command in ONE
// mutation transaction, through the existing s.mutate/s.record discipline, with
// an idempotency key derived from the pass's request namespace, the stage name
// and the aggregate id -- so a repeated tick with the same namespace REPLAYS
// rather than re-transitions.
//
// expectStatus is the status the observation was made against, re-verified
// INSIDE the transaction. A status that moved between the read and the write
// means the observation is stale, and the answer is (false, 0, nil): no
// transition, not a transition on a stale fact.
//
// The domain command's At is the pass's single clock instant, not a second
// read. The Event's At is the transaction's own authority time, because
// s.record derives it and internal/application/service.go is outside this
// task's allowed paths.
func (s *Service) loopRequirementTransition(ctx context.Context, req LoopPassRequest, actor domain.ActorID, now time.Time, stage, operation, eventType, requirementID string, kind domain.RequirementCommandKind, expectStatus domain.RequirementStatus, observation func(domain.Version) any) (bool, domain.Version, error) {
	requestID := req.RequestID + ":" + stage + ":" + requirementID
	fingerprint, err := requestFingerprint(operation, requestID)
	if err != nil {
		return false, 0, err
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return false, 0, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return false, 0, err
	}
	moved := false
	var version domain.Version
	err = s.mutate(ctx, operation+":"+requestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, requestID, operation); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			// A replay is not a second transition. It reports the recorded
			// version and moves nothing.
			var replay struct {
				Version domain.Version `json:"version"`
			}
			if e = restoreResponse(prior, &replay); e != nil {
				return e
			}
			version = replay.Version
			return nil
		}
		r, ok, e := u.Requirement(ctx, requirementID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if r.Status != expectStatus {
			// The observation is stale. Nothing is staged: no aggregate, no
			// event, no idempotency record.
			return nil
		}
		next, e := domain.DecideRequirement(r, domain.RequirementCommand{
			Kind:            kind,
			Actor:           actor,
			At:              now,
			ExpectedVersion: r.Version,
		})
		if e != nil {
			return e
		}
		if e = u.SaveRequirement(ctx, next, r.Version); e != nil {
			return e
		}
		moved = true
		version = next.Version
		// The nil OutboxItem is the assertion in code that this transition
		// stages no durable side effect: nothing outside the control plane is
		// asked to act by a Requirement noting that it is waiting or
		// recovering.
		return s.record(ctx, u, eventID, operationID, fingerprint, requestID, operation, "requirement", requirementID, next.Version, eventType, actor.String(), nil, observation(next.Version))
	})
	if err != nil {
		return false, 0, err
	}
	return moved, version, nil
}

// loopStagePlan is the Increment-creation path, and it is the stage that makes
// the whole claim surface reachable at all. Measured: Service.Plan is the only
// non-test code that creates a domain.Increment, its only non-test caller was
// internal/runner/orchestrator.go, and no Orchestrator is constructed outside a
// test -- so a running Control Plane could hold NO Increment, and
// POST /v1/runner/claims:acquire and /v1/executions/* were reachable routes
// that could never succeed.
//
// It calls the EXISTING Service.Plan and Service.Prepare with the SAME
// scheduler context. Both already admit RoleScheduler
// (internal/application/service.go:888 and :1023), so no authority changes and
// internal/application/service.go is not edited.
//
// The observation is the scheduler's ASSIGNMENT. A ready Requirement the
// scheduler did NOT assign gets no Increment, and a Requirement that already
// holds a non-terminal Increment gets no second one.
func (s *Service) loopStagePlan(ctx context.Context, req LoopPassRequest, obs loopObservation, report *LoopPassReport) error {
	for _, r := range obs.page {
		id := r.ID.String()
		if r.Status != domain.RequirementReady {
			continue
		}
		decision, ok := obs.decisions[id]
		if !ok || !decision.Assigned {
			continue
		}
		if obs.inFlight[id] {
			continue
		}
		if report.Plan.Transitions >= LoopPlanCap {
			report.Plan.Deferred++
			continue
		}
		planned, err := s.Plan(ctx, PlanRequest{
			RequestID:                  req.RequestID + ":plan:" + id,
			RequirementID:              id,
			ExpectedRequirementVersion: r.Version,
		})
		if err != nil {
			if loopSkippable(err) {
				continue
			}
			return err
		}
		prepared, err := s.Prepare(ctx, PrepareRequest{
			RequestID:       req.RequestID + ":prepare:" + id,
			IncrementID:     planned.IncrementID,
			ExpectedVersion: planned.Version,
		})
		if err != nil {
			if loopSkippable(err) {
				continue
			}
			return err
		}
		_ = prepared
		report.Plan.Transitions++
		report.PlanObservations = append(report.PlanObservations, LoopPlanObservation{
			RequirementID: id, IncrementID: planned.IncrementID, Rank: decision.Rank,
		})
	}
	return nil
}

// ---------------------------------------------------------------------------
// Stage P: promotion, and the proof that does not leave the transaction
// ---------------------------------------------------------------------------

// loopStagePromote calls release.Pipeline.Promote and uses the
// domain.StableReleaseProof it returns in GateResult.Proof. It does NOT call
// release.PromotionGate directly and does NOT call
// domain.CompletePromotionWithProof directly; it does not store the proof, does
// not put it in a report field, does not log it and passes it across no package
// boundary other than into domain.CompleteRequirementFromRelease. A go/ast
// guard, internal/application/completion_proof_guard_test.go, asserts that this
// file holds the ONLY call site of each of those three, and that guard has been
// OBSERVED RED by deleting the real call site.
//
// A Requirement completes only on the CONJUNCTION of two independent
// observations: a proof the promotion path earned, and this Requirement's own
// Increments having all reached domain.IncrementReleased.
// docs/architecture/domain-model.md:101 -- "1 Requirementに複数
// Incrementを許す。Incrementが成功してもRequirementは
// 自動完了しない。" -- is why neither half alone may move a
// Requirement, and docs/product/definition.md:130 --
// "完了を自己申告だけで決めず、要求と不変条件を検証
// する" -- is why the proof rather than a boolean is what carries it.
func (s *Service) loopStagePromote(ctx context.Context, req LoopPassRequest, actor domain.ActorID, now time.Time, obs loopObservation, report *LoopPassReport) error {
	source, configured := s.releaseSource()
	if !configured {
		// An absent configuration is a SKIP, never a default root: a defaulted
		// root would make the process report a version it was not assembled
		// from (internal/application/release_surface.go:17-23).
		report.Promotion.Outcome = LoopPromotionSkippedNotConfigured
		return nil
	}
	// The candidates for completion, identified BEFORE the promotion so the
	// report can distinguish "the promotion refused" from "no Requirement was
	// eligible".
	type completable struct {
		id      string
		version domain.Version
	}
	var eligible []completable
	for _, r := range obs.page {
		id := r.ID.String()
		if r.Status != domain.RequirementActive {
			continue
		}
		increments := obs.increments[id]
		if len(increments) == 0 {
			continue
		}
		all := true
		for _, inc := range increments {
			if inc.Status != domain.IncrementReleased {
				all = false
			}
		}
		if !all {
			continue
		}
		eligible = append(eligible, completable{id: id, version: r.Version})
	}

	// The control state and the permit come from canonical state read inside a
	// transaction, never from the request: LoopPassRequest carries no control
	// revision at all, and domain.Permit requires the revision to be exactly
	// the authoritative one.
	target := domain.ControlTarget{InstallationID: s.config.InstallationID}
	var effective domain.EffectiveControlResult
	if err := s.transact(ctx, func(u UnitOfWork) error {
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		effective = domain.EffectiveControl(controls, target)
		return nil
	}); err != nil {
		return err
	}
	revision := domain.Revision(0)
	if effective.Found {
		revision = effective.Revision
	}
	fence := source.bundle.Candidate.FencingToken
	permit, err := domain.Permit(effective, domain.PermitRequest{
		Kind:                 domain.PermitPromotion,
		Target:               target,
		ControlRevision:      revision,
		FencingToken:         fence,
		ExpectedFencingToken: fence,
		Resource:             source.bundle.Candidate.CandidateID.String(),
	})
	if err != nil {
		report.Promotion.Outcome = LoopPromotionRefused
		report.Promotion.Reason = err.Error()
		return nil
	}
	result, err := source.pipeline.Promote(source.bundle, effective, permit)
	if err != nil {
		// A refused promotion is a fact about the candidate, not a failure of
		// the pass: zero evaluations, zero completions, and the reason recorded.
		report.Promotion.Outcome = LoopPromotionRefused
		report.Promotion.Reason = err.Error()
		return nil
	}
	report.Promotion.Outcome = LoopPromotionPromoted
	report.Promotion.CandidateID = result.Candidate.CandidateID.String()
	for _, candidate := range eligible {
		if report.Promotion.Completions >= LoopStageCap {
			report.Promotion.Deferred++
			continue
		}
		observation := LoopCompletionObservation{
			RequirementID: candidate.id,
			CandidateID:   result.Candidate.CandidateID.String(),
			BundleDigest:  result.Candidate.BundleDigest,
		}
		version, evaluated, completed, err := s.loopCompleteRequirement(ctx, req, actor, now, candidate.id, result, observation)
		if err != nil {
			if loopSkippable(err) {
				continue
			}
			return err
		}
		if evaluated {
			report.Promotion.Evaluations++
		}
		if !completed {
			continue
		}
		observation.Version = version
		report.Promotion.Completions++
		report.CompletionObservations = append(report.CompletionObservations, observation)
	}
	return nil
}

// loopCompleteRequirement issues domain.RequirementEvaluate and then
// domain.CompleteRequirementFromRelease IN THE SAME TRANSACTION, so a failure
// between them leaves the Requirement in NEITHER evaluating nor completed.
// Only ONE SaveRequirement is staged -- the completed value at the version two
// above the observed one -- because staging the evaluating value first and the
// completed value second inside one transaction would be two writes describing
// one atomic step, and a store that flushed the first and refused the second
// would leave exactly the half-state this signature exists to make impossible.
func (s *Service) loopCompleteRequirement(ctx context.Context, req LoopPassRequest, actor domain.ActorID, now time.Time, requirementID string, result release.GateResult, observation LoopCompletionObservation) (domain.Version, bool, bool, error) {
	requestID := req.RequestID + ":complete:" + requirementID
	const operation = "loop-complete"
	fingerprint, err := requestFingerprint(operation, requestID)
	if err != nil {
		return 0, false, false, err
	}
	eventID, err := s.ids.Next("event")
	if err != nil {
		return 0, false, false, err
	}
	operationID, err := s.ids.Next("operation")
	if err != nil {
		return 0, false, false, err
	}
	evaluated, completed := false, false
	var version domain.Version
	err = s.mutate(ctx, operation+":"+requestID, func(u UnitOfWork) error {
		if prior, ok, e := u.Idempotency(ctx, requestID, operation); e != nil {
			return e
		} else if ok {
			if prior.Fingerprint != fingerprint {
				return ErrIdempotencyConflict
			}
			var replay LoopCompletionObservation
			if e = restoreResponse(prior, &replay); e != nil {
				return e
			}
			version = replay.Version
			return nil
		}
		r, ok, e := u.Requirement(ctx, requirementID)
		if e != nil {
			return e
		}
		if !ok {
			return ErrNotFound
		}
		if r.Status != domain.RequirementActive {
			return nil
		}
		evaluating, e := domain.DecideRequirement(r, domain.RequirementCommand{
			Kind:            domain.RequirementEvaluate,
			Actor:           actor,
			At:              now,
			ExpectedVersion: r.Version,
		})
		if e != nil {
			return e
		}
		evaluated = true
		next, e := domain.CompleteRequirementFromRelease(evaluating, result.Proof, actor, now)
		if e != nil {
			return e
		}
		if e = u.SaveRequirement(ctx, next, r.Version); e != nil {
			return e
		}
		completed = true
		version = next.Version
		observation.Version = next.Version
		return s.record(ctx, u, eventID, operationID, fingerprint, requestID, operation, "requirement", requirementID, next.Version, "requirement.completed", actor.String(), nil, observation)
	})
	if err != nil {
		return 0, false, false, err
	}
	return version, evaluated, completed, nil
}

// ---------------------------------------------------------------------------
// The one runner-role read this task adds, and why it is unavoidable
// ---------------------------------------------------------------------------

// LoopOfferedIncrement is one claimable Increment, named by the three
// identifiers POST /v1/runner/claims:acquire requires and by NOTHING else. It
// carries no text, no work packet, no digest, and no field whose name contains
// password, credential, token, secret, raw_prompt or raw_provider_output.
type LoopOfferedIncrement struct {
	RequirementID            string         `json:"requirement_id"`
	IncrementID              string         `json:"increment_id"`
	ExpectedIncrementVersion domain.Version `json:"expected_increment_version"`
	RequirementSummary       string         `json:"requirement_summary"`
}

// LoopOfferedWork is the offer. Cap is reported so a Runner can tell a full
// page from an exhausted one without guessing.
type LoopOfferedWork struct {
	SchemaVersion string                 `json:"schema_version"`
	Cap           int                    `json:"cap"`
	Increments    []LoopOfferedIncrement `json:"increments"`
}

// OfferedWork is the bounded runner-role read behind GET /v1/runner/work.
//
// IT EXISTS BECAUSE OF A MEASUREMENT, not for symmetry.
// POST /v1/runner/claims:acquire requires a CALLER-SUPPLIED increment_id and
// expected_increment_version (internal/api/api.go's claimBody at :1277), the
// runner-role POST routes are the five cases at internal/api/api.go:384-397,
// and there is NO runner-readable GET route at all: GET /v1/queue/summary and
// GET /v1/export are owner-only. So a separate-process Runner could not
// discover work by ANY means, and no amount of client code fixes that. The
// tempting repair -- having a test tell the driver which Increment to claim --
// is the fake journey wearing a different hat.
//
// It is gated runnerCaller(ctx) (internal/application/caller.go:110), so only a
// verified runner session may read it: an owner is ErrForbidden and an absent
// caller is ErrUnauthenticated. It is derived from the SAME bounded page the
// pass uses, filtered to Increments in domain.IncrementReady whose parent
// admits work by V2-089's own requirementStatusAdmitsClaim, and capped at
// LoopOfferedWorkCap. It mutates nothing and stages nothing.
func (s *Service) OfferedWork(ctx context.Context) (LoopOfferedWork, error) {
	if _, _, _, err := runnerCaller(ctx); err != nil {
		return LoopOfferedWork{}, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return LoopOfferedWork{}, errors.New("clock returned zero time")
	}
	out := LoopOfferedWork{SchemaVersion: "v1", Cap: LoopOfferedWorkCap, Increments: []LoopOfferedIncrement{}}
	err := s.transact(ctx, func(u UnitOfWork) error {
		obs, e := loopObserve(ctx, u, now, "")
		if e != nil {
			return e
		}
		ids := make([]string, 0, len(obs.readyOffered))
		for _, offered := range obs.readyOffered {
			ids = append(ids, offered.RequirementID)
		}
		texts, e := u.RequirementTexts(ctx, ids)
		if e != nil {
			return e
		}
		for _, offered := range obs.readyOffered {
			offered.RequirementSummary = texts[offered.RequirementID]
			if strings.TrimSpace(offered.RequirementSummary) == "" {
				return errors.New("claimable requirement has no work summary")
			}
			out.Increments = append(out.Increments, offered)
		}
		return nil
	})
	if err != nil {
		return LoopOfferedWork{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The release source: one Router, shared by the owner's read and the promotion
// ---------------------------------------------------------------------------

// ReleaseSourceConfig is what wiring a process's own release source requires.
// Every field is explicit and there is NO DEFAULT ROOT, for the reason
// internal/application/release_surface.go:17-23 already records: a defaulted
// root would make the process report a version it was not assembled from.
//
// Candidate is the part assembly cannot derive from source bytes -- identity,
// operational evidence and control state. The SHIPPED wiring leaves it ZERO,
// which is why the shipped configuration cannot assemble a fully-evidenced
// candidate and why the completion transition's claim stops at component grade.
// cmd/control-plane never mentions the field, so it needs no internal/release
// import and declares no new component edge.
type ReleaseSourceConfig struct {
	Root             string
	Repository       string
	EnvironmentClass string
	AssembledAt      time.Time
	Candidate        release.CandidateInput
}

// loopReleaseSource is one process's assembled release source. The Router is
// the SAME object the observer reports, so the owner's GET /v1/release/state and
// the promotion path cannot disagree about the recorded route by construction.
type loopReleaseSource struct {
	pipeline   *release.Pipeline
	bundle     release.Bundle
	repository string
}

var loopReleaseSources = struct {
	mu sync.RWMutex
	by map[*Service]loopReleaseSource
}{by: map[*Service]loopReleaseSource{}}

// AttachReleaseSource assembles this process's release source ONCE, from an
// explicitly configured root, and binds it to this Service. It builds
// release.NewRouter and release.NewMemoryStore INTERNALLY and attaches the
// observer with Service.AttachReleaseObserver, so a caller in cmd/** needs no
// internal/release import: measured in ci/components.json, cmd/control-plane's
// declared dependencies are api, application, reconciler, runner and
// store-firestore, and adding release would be a new component edge that moves
// all 23 evidence keys.
//
// The preview route is recorded for the assembled candidate's own digest,
// because release.Router.Promote refuses to promote anything but the recorded
// preview route -- so a process that never recorded a preview route cannot
// promote, which is the correct default.
func (s *Service) AttachReleaseSource(cfg ReleaseSourceConfig) error {
	if s == nil {
		return errors.New("cannot attach a release source to a nil Service")
	}
	if strings.TrimSpace(cfg.Root) == "" {
		return errors.New("release source requires an explicitly configured source root")
	}
	if strings.TrimSpace(cfg.Repository) == "" {
		return errors.New("release source requires an explicitly configured repository")
	}
	if strings.TrimSpace(cfg.EnvironmentClass) == "" {
		return errors.New("release source requires a declared Preview environment class")
	}
	if cfg.AssembledAt.IsZero() {
		return errors.New("release source requires an injected assembly instant")
	}
	router := release.NewRouter()
	bundle, _, err := release.AssembleBundle(cfg.Root, cfg.Repository, cfg.Candidate, cfg.AssembledAt)
	if err != nil {
		return fmt.Errorf("assemble the release source root: %w", err)
	}
	if digest := bundle.Candidate.CandidateDigest; digest != "" {
		if err = router.SetPreview(cfg.Repository, digest); err != nil {
			return err
		}
	}
	observer, err := NewSourceReleaseObserver(ReleaseObserverConfig{
		Root:             cfg.Root,
		Repository:       cfg.Repository,
		EnvironmentClass: cfg.EnvironmentClass,
		Candidate:        cfg.Candidate,
		Router:           router,
		AssembledAt:      cfg.AssembledAt,
	})
	if err != nil {
		return err
	}
	if err = s.AttachReleaseObserver(observer); err != nil {
		return err
	}
	loopReleaseSources.mu.Lock()
	defer loopReleaseSources.mu.Unlock()
	loopReleaseSources.by[s] = loopReleaseSource{
		pipeline:   release.NewPipeline(release.NewMemoryStore(), router, cfg.Root),
		bundle:     bundle,
		repository: cfg.Repository,
	}
	return nil
}

// DetachReleaseSource removes the binding. It exists so a test can restore the
// not-configured state deterministically, exactly as DetachReleaseObserver does.
func (s *Service) DetachReleaseSource() {
	loopReleaseSources.mu.Lock()
	delete(loopReleaseSources.by, s)
	loopReleaseSources.mu.Unlock()
	s.DetachReleaseObserver()
}

func (s *Service) releaseSource() (loopReleaseSource, bool) {
	loopReleaseSources.mu.RLock()
	defer loopReleaseSources.mu.RUnlock()
	v, ok := loopReleaseSources.by[s]
	return v, ok
}
