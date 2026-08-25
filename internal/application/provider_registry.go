package application

// ===========================================================================
// The Provider registry (V2-067)
// ===========================================================================
//
// What this file is for. Provider state has been living in a cost ledger on a
// Runner machine, outside the repository, where nothing inside the system can
// read it. This file makes Provider operation observable from inside: for each
// of the three declared Providers, whether the owner's standing authorization
// covers it, whether the Loop has ever completed an invocation through it, a
// closed health value with its observation age and an explicit stale flag, the
// runner-local runaway detector's state by state alone, the control plane's own
// concurrency allocation, and the Executions currently assigned to it.
//
// Six things this file deliberately is not.
//
//  1. It is not a domain aggregate. A Provider has no state transition, no
//     Version, and no invariant that domain.Validate or domain.Permit
//     consults; no rule in internal/domain reads it. internal/domain is not
//     edited by this task at all (dp-v2-067 d1).
//
//  2. It is not a probe. Nothing here starts a CLI, opens a socket or polls a
//     timer. Every value is derived from observations the Loop's own execution
//     path already produced. An active probe would be an invocation, and every
//     invocation on the Loop's path passes internal/runner.CostLedger.Reserve,
//     which counts against the runaway detector's own thresholds -- so a
//     registry that probed on every read would consume the counters of the
//     detector it exists to report, and could trip the halt it is meant to
//     describe. internal/application/source_guard_test.go proves the absence
//     mechanically rather than promising it (dp-v2-067 d3).
//
//  3. It is not a monetary surface. No field name and no enum value in the
//     read model may contain budget, quota, billing, spend, cost or credit,
//     and no numeric runaway threshold appears anywhere in it. Reaching a
//     threshold is a stop for inspection, never a success, never a failure and
//     never a billing event; the numbers stay in the owner-approved,
//     digest-bound provider-preflight record, which this surface names and
//     does not copy (dp-v2-067 d6).
//
//  4. It is not a single connected flag. Authorization and authentication are
//     different facts with different owners, and today they disagree for two of
//     three Providers, so they are reported as two independent fields. A single
//     boolean would have to pick one and would then be wrong about the other
//     (dp-v2-067 d8).
//
//  5. It does not read silence as health. A Provider with no observation is
//     unknown and stale, never healthy, and a stale observation is reported
//     with its age rather than silently refreshed.
//
//  6. It observes only the Loop's own execution path -- the boundary V2-062
//     recorded for the cost ledger. A Provider a human drove by hand leaves no
//     trace here and reports unknown. unknown is an absence of observation,
//     never a fault.
//
// See docs/operations/provider-registry.md.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Declared bounds and declared references
// ---------------------------------------------------------------------------

// ProviderObservationWindow is the declared freshness window. An observation
// older than this makes the entry stale.
//
// It is deliberately much longer than the Runner heartbeat's staleness window
// (RunnerVersionReportStaleAfter, 15 minutes): a Runner heart-beats on a
// timer, so silence for 15 minutes really is news, while a Provider is invoked
// once per Execution and never on a timer, so an idle night is not a fault.
// It is deliberately not longer than a day either, because a day-old success
// is not evidence about now.
const ProviderObservationWindow = 24 * time.Hour

// MaxProviderObservations bounds the observation ring retained per Provider.
// The ring is what makes the read a single keyed document read per Provider
// rather than a scan whose cost grows with the Requirement count
// (docs/architecture/validation.md section 5).
const MaxProviderObservations = 32

// MaxProviderAssignments bounds the assignment ring retained per Provider. It
// is deliberately larger than ProviderConcurrencyDesignCeiling so that
// exceeding the ceiling is reportable -- active_assignments above the ceiling
// with exhausted true -- before the ring truncates and the reported count
// becomes a lower bound.
const MaxProviderAssignments = 24

// ProviderConcurrencyDesignCeiling is the concurrent-Execution ceiling of
// docs/architecture/validation.md section 5. It is reported with its source
// named rather than as though an owner had chosen it; V2-068 owns the
// installation-settable limit and whichever of the two lands second wires the
// registry to the shared accessor (dp-v2-067 d11).
const ProviderConcurrencyDesignCeiling = 20

// ProviderStandingAuthorizationRef is the id of the in-repository standing
// authorization record (.agents/v2/packets/provider-standing-authorization
// .json, kind provider-standing-authorization). It is a record id, not a
// credential and not a person: the record's approver is an email address and
// is deliberately never read into this package, never carried into a response
// and never written to a log line.
const ProviderStandingAuthorizationRef = "psa-foundation-001"

// ProviderRunawayScope names who owns the runaway detector. It is the local
// Runner machine: the ledger and the CLIs live there, so the control plane
// cannot read the counters and does not pretend to.
const ProviderRunawayScope = "runner-local"

// ProviderRunawayThresholdsDeclaredIn names where the numbers live, without
// copying any of them. Copying a safety threshold into a read model would
// create a second copy that can silently disagree with the approved one.
//
// Recorded limitation: this names the approved standing authorization by id
// and the per-exercise record by kind and location rather than naming one
// provider-preflight record id, because the control plane cannot know which
// record a Runner invoked under -- the record and the ledger are both on the
// Runner machine (the V2-062 boundary), and learning the id would mean
// widening provider_observation past the three fields V2-067 A10 closes it to.
const ProviderRunawayThresholdsDeclaredIn = "psa-foundation-001 and the provider-preflight record under .agents/v2/provider-preflight/ that authorised the invocation"

// ---------------------------------------------------------------------------
// Identity: a closed set of three names, pinned twice and independently
// ---------------------------------------------------------------------------

// ProviderName is the identity of a Provider, and it is the adapter name
// alone. Identity is not name plus account: the subscription set is one
// account per CLI, internal/runner.CostLedger already keys on the name,
// contracts/schemas/provider-preflight.json keys provider.name, and
// contracts/schemas/provider-standing-authorization.json constrains providers
// to exactly this enum -- so keying on the name joins this registry to every
// record that already exists with no mapping table. Adding an account
// discriminator would mean moving owner identity into a read model, which is
// the class of value this repository keeps out of its schemas.
//
// The set is pinned twice, in two packages, and NOT shared by an import:
// internal/application must not import internal/provider (asserted by
// source_guard_test.go), which keeps the provider component a leaf in
// ci/components.json and adds no component edge. The cost is that the two
// declarations can drift; TestProviderRegistryNameTableIsExactlyThreeNames
// here and TestProviderIdentityIsExactlyThreeAdapterNames in
// internal/provider are what force them to change together.
type ProviderName string

const (
	ProviderCodex    ProviderName = "codex"
	ProviderClaude   ProviderName = "claude"
	ProviderOpenCode ProviderName = "opencode"
)

// Valid is the closed switch. TestProviderRegistryEnumsAreClosed parses this
// file and fails if a constant of this type has no case here, or a case here
// has no constant.
func (p ProviderName) Valid() bool {
	switch p {
	case ProviderCodex, ProviderClaude, ProviderOpenCode:
		return true
	}
	return false
}

// declaredProviders is the registry's own name table in its documented order:
// the order contracts/schemas/provider-standing-authorization.json declares
// its enum in, and the order the standing authorization record lists. The
// order is fixed so the response is byte-identical across repeated calls.
var declaredProviders = []ProviderName{ProviderCodex, ProviderClaude, ProviderOpenCode}

// DeclaredProviders returns a copy of the closed, ordered name table.
func DeclaredProviders() []ProviderName {
	return append([]ProviderName(nil), declaredProviders...)
}

// standingAuthorizedProviders is the set the in-repository standing
// authorization covers. It is declared separately from declaredProviders on
// purpose: "which Providers this system knows about" and "which Providers the
// owner has authorised" are two different facts that happen to coincide today,
// and a fourth declared Provider with no authorization must show up as
// authorized false rather than inherit an approval nobody gave.
var standingAuthorizedProviders = []ProviderName{ProviderCodex, ProviderClaude, ProviderOpenCode}

// providerAuthorization reports the owner's standing authorization for one
// Provider. It returns the record id and never anything else from the record:
// not the approver, not the approval instant, not the scope text.
func providerAuthorization(name ProviderName) (bool, string) {
	for _, authorized := range standingAuthorizedProviders {
		if authorized == name {
			return true, ProviderStandingAuthorizationRef
		}
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// Health: six values, and silence is not one of them
// ---------------------------------------------------------------------------

// ProviderHealth is a closed set of six values.
//
// unknown is first-class and distinct from both healthy and unavailable,
// because today two of the three Providers are unexercised: a design that
// defaulted to healthy would misreport the common case, and one that defaulted
// to unavailable would report an absence of observation as a fault.
//
// stopped-for-inspection is separated from unavailable for the reason
// docs/operations/provider-live-claude.md records: reaching a runaway
// threshold is a stop for inspection and is neither a success nor a failure,
// so folding it into a failure value would misstate it. It is counted in no
// failure or degraded tally anywhere -- and this view reports no such tally at
// all, so there is nowhere for it to be miscounted.
type ProviderHealth string

const (
	ProviderHealthUnknown              ProviderHealth = "unknown"
	ProviderHealthHealthy              ProviderHealth = "healthy"
	ProviderHealthDegraded             ProviderHealth = "degraded"
	ProviderHealthUnavailable          ProviderHealth = "unavailable"
	ProviderHealthUnauthenticated      ProviderHealth = "unauthenticated"
	ProviderHealthStoppedForInspection ProviderHealth = "stopped-for-inspection"
)

func (h ProviderHealth) Valid() bool {
	switch h {
	case ProviderHealthUnknown, ProviderHealthHealthy, ProviderHealthDegraded,
		ProviderHealthUnavailable, ProviderHealthUnauthenticated, ProviderHealthStoppedForInspection:
		return true
	}
	return false
}

// ProviderBlockedReason names, in plain words, what a human would have to do.
// It is empty exactly when health is healthy, which is what lets the assignable
// question be answered by one field rather than by re-deriving health.
type ProviderBlockedReason string

const (
	// ProviderNotBlocked is the empty value. It is a declared constant rather
	// than a bare "" so the closed-enum scan covers it like any other member.
	ProviderNotBlocked                       ProviderBlockedReason = ""
	ProviderBlockedNeverInvoked              ProviderBlockedReason = "never-invoked-by-loop"
	ProviderBlockedOwnerMustAuthenticate     ProviderBlockedReason = "owner-must-authenticate-cli-on-runner-machine"
	ProviderBlockedLastInvocationRetryable   ProviderBlockedReason = "last-invocation-failed-retryably"
	ProviderBlockedLastInvocationPermanent   ProviderBlockedReason = "last-invocation-failed-non-retryably"
	ProviderBlockedLastInvocationUnclassed   ProviderBlockedReason = "last-invocation-failed-without-a-classified-reason"
	ProviderBlockedOwnerMustClearRunawayStop ProviderBlockedReason = "owner-must-clear-the-runaway-stop-with-a-new-approved-record"
)

func (r ProviderBlockedReason) Valid() bool {
	switch r {
	case ProviderNotBlocked, ProviderBlockedNeverInvoked, ProviderBlockedOwnerMustAuthenticate,
		ProviderBlockedLastInvocationRetryable, ProviderBlockedLastInvocationPermanent,
		ProviderBlockedLastInvocationUnclassed, ProviderBlockedOwnerMustClearRunawayStop:
		return true
	}
	return false
}

// ProviderFailureClass is the closed class of a failed invocation, as the
// Runner reports it. It is this package's own vocabulary, deliberately
// re-declared rather than imported:
//
//   - internal/provider.FailureClass cannot be imported, because
//     internal/application must not depend on internal/provider (dp-v2-067
//     d2/d3), and
//   - internal/domain/release.go's FailureClass is a wider release-failure
//     taxonomy with no unauthenticated member, and adding one would mean
//     editing internal/domain, which this task must not do.
//
// Two deliberate differences from internal/provider.FailureClass, recorded
// rather than hidden:
//
//   - provider-quota is re-spelled provider-rate-limited. The read model and
//     the openapi schema may not contain the substring "quota" (dp-v2-067 d6),
//     and rate limiting is what the class actually describes; the exhaustion
//     that d6 is about is reported by concurrency.exhausted and by
//     runaway_detection.state, not by a failure class.
//   - invalid-input and cancelled are absent. Both describe the Loop's own
//     behaviour, not the Provider's, and recording either as an observation
//     about a Provider's health would attribute a caller mistake or an
//     operator cancellation to the Provider.
//
// V2-027 owns the failure-class table that gives provider-unauthenticated its
// row; this enum is the wire vocabulary, not that table.
type ProviderFailureClass string

const (
	ProviderFailureUnauthenticated ProviderFailureClass = "provider-unauthenticated"
	ProviderFailureTransport       ProviderFailureClass = "provider-transport"
	ProviderFailureRateLimited     ProviderFailureClass = "provider-rate-limited"
	ProviderFailureTimeout         ProviderFailureClass = "timeout"
	ProviderFailureModel           ProviderFailureClass = "provider-model"
	ProviderFailureContract        ProviderFailureClass = "contract-incompatible"
	ProviderFailureUnclassified    ProviderFailureClass = "unknown"
)

func (c ProviderFailureClass) Valid() bool {
	switch c {
	case ProviderFailureUnauthenticated, ProviderFailureTransport, ProviderFailureRateLimited,
		ProviderFailureTimeout, ProviderFailureModel, ProviderFailureContract, ProviderFailureUnclassified:
		return true
	}
	return false
}

// ProviderRunawayState is the runner-local detector's state, and only its
// state. There is no threshold number in this type, in the record behind it or
// in the schema that publishes it.
type ProviderRunawayState string

const (
	ProviderRunawayWithinThresholds     ProviderRunawayState = "within-thresholds"
	ProviderRunawayStoppedForInspection ProviderRunawayState = "stopped-for-inspection"
	ProviderRunawayUnknown              ProviderRunawayState = "unknown"
)

func (s ProviderRunawayState) Valid() bool {
	switch s {
	case ProviderRunawayWithinThresholds, ProviderRunawayStoppedForInspection, ProviderRunawayUnknown:
		return true
	}
	return false
}

// ProviderCeilingSource names which ceiling is being reported, so a design
// ceiling is never shown as though an owner had chosen it.
//
// V2-068 added the second member this type's own comment anticipated. The
// installation concurrency limit is now settable through POST /v1/controls and
// stored in the AllocationLimitRepository side table (allocation.go), so the
// ceiling reported here has two possible sources and a reader must be able to
// tell them apart.
//
// The two surfaces report the same NUMBER for the same state -- that is the
// convergence dp-v2-067 d11 and wo-v2-068 A19 asked for, and
// TestProviderCeilingConvergesWithTheQueueSummaryAllocation asserts it. They
// name its source in their own vocabulary: this type says WHO chose the number
// (owner-declared), while the queue summary's limit_source says WHERE the
// number came from (control-revision) and reports control_revision beside it.
// Neither spelling is derived from the other, and neither is a placeholder:
// each is produced by real state.
type ProviderCeilingSource string

const (
	ProviderCeilingArchitectureDesign ProviderCeilingSource = "architecture-design-ceiling"
	// ProviderCeilingOwnerDeclared means an owner declared this ceiling on a
	// Control Intent revision. The name says who chose it rather than which
	// record carries it, because that is the distinction a reader of this
	// surface needs: a number nobody chose must never read as a policy.
	ProviderCeilingOwnerDeclared ProviderCeilingSource = "owner-declared"
)

func (s ProviderCeilingSource) Valid() bool {
	switch s {
	case ProviderCeilingArchitectureDesign, ProviderCeilingOwnerDeclared:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// The stored records
// ---------------------------------------------------------------------------

var (
	// ErrProviderUnknown is a malformed request: the named Provider is not one
	// of the three declared names. It is never a policy decision.
	ErrProviderUnknown = errors.New("provider is not one of the declared provider names")
	// ErrProviderObservationInvalid is the shape refusal for an observation.
	ErrProviderObservationInvalid = errors.New("provider observation is malformed")
)

// ProviderObservationInput is the additive optional object on the result the
// Runner already posts. It carries a name, an optional closed failure class
// and an optional boolean, and nothing else at all: there is no message,
// detail, output, result, session or text field anywhere on it, so provider
// text and credential material are structurally unrepresentable rather than
// filtered. No redaction step can be forgotten, because there is no field to
// forget to redact.
//
// It carries no timestamp either: observed_at is the transaction's authority
// time, following the rule service.go already states for process observations
// ("Runner clocks and caller-provided timestamps are not authoritative").
type ProviderObservationInput struct {
	Name                 ProviderName
	FailureClass         ProviderFailureClass
	StoppedForInspection bool
}

// Validate is shape only. Nothing here checks that the claim is true; no
// control-plane code can, because the invocation it describes happened on
// another machine.
func (o ProviderObservationInput) Validate() error {
	if !o.Name.Valid() {
		return fmt.Errorf("%w: %s", ErrProviderUnknown, "provider_observation.name must be one of codex, claude, opencode")
	}
	if o.FailureClass != "" && !o.FailureClass.Valid() {
		return fmt.Errorf("%w: failure_class is not one of the declared classes", ErrProviderObservationInvalid)
	}
	return nil
}

// ProviderObservation is one stored observation. ObservedAt is the
// transaction's authority time and never a value a Runner sent. An empty
// FailureClass means the invocation completed.
type ProviderObservation struct {
	Provider             ProviderName
	FailureClass         ProviderFailureClass
	StoppedForInspection bool
	ObservedAt           time.Time
}

// Completed reports an invocation that finished with no failure and no stop.
// It is the only observation that proves the Loop drove this Provider all the
// way through, which is why it -- and nothing weaker -- sets VerifiedAt.
func (o ProviderObservation) Completed() bool {
	return o.FailureClass == "" && !o.StoppedForInspection
}

// ProviderObservationLog is what one keyed read of a Provider's record
// returns: the bounded ring newest-first, and the sticky instant of the first
// completed invocation.
//
// VerifiedAt is sticky and stored rather than derived from the ring, because
// "has the Loop ever completed an invocation through this Provider" is a
// monotone historical fact and the ring is bounded: deriving it from the ring
// would let MaxProviderObservations later failures silently un-verify a
// Provider that really was exercised.
type ProviderObservationLog struct {
	Provider     ProviderName
	Observations []ProviderObservation
	VerifiedAt   time.Time
}

// ProviderAssignment is the side-table record: which Provider an Execution was
// started against. It is keyed by Execution id and domain.Execution gains no
// Provider field, following the precedent ControlRequestedByRepository already
// set for a label with no transition semantics (dp-v2-067 d7). It is written
// inside Start's existing transaction, so an assignment appears and disappears
// with the Execution it describes and the registry can never report an
// assignment to an Execution that was never started.
type ProviderAssignment struct {
	ExecutionID string
	IncrementID string
	Provider    ProviderName
	Since       time.Time
}

// SortProviderAssignments puts assignments in the fixed order every adapter
// must return them in: Execution id ascending. Adapters call it so the order
// is declared in one place rather than once per adapter.
func SortProviderAssignments(rows []ProviderAssignment) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].ExecutionID < rows[j].ExecutionID })
}

// ApplyProviderObservation is the whole write rule of the observation ring,
// declared once here and called by both store adapters so neither can
// implement a retention or a stickiness the other does not.
//
// Two rules, both properties of the record rather than of a caller:
//
//   - the ring keeps at most MaxProviderObservations entries, newest first,
//     and drops the oldest, so the read is one bounded keyed document, and
//   - VerifiedAt is sticky: the first observation that Completed() sets it and
//     nothing ever clears it. "Has the Loop ever completed an invocation
//     through this Provider" is a monotone historical fact; deriving it from a
//     bounded ring would let MaxProviderObservations later failures silently
//     un-verify a Provider that really was exercised.
func ApplyProviderObservation(log ProviderObservationLog, value ProviderObservation) ProviderObservationLog {
	log.Provider = value.Provider
	rows := append([]ProviderObservation{value}, log.Observations...)
	if len(rows) > MaxProviderObservations {
		rows = rows[:MaxProviderObservations]
	}
	log.Observations = rows
	if log.VerifiedAt.IsZero() && value.Completed() {
		log.VerifiedAt = value.ObservedAt
	}
	return log
}

// AppendProviderAssignment is the whole write rule of the per-Provider
// assignment index, declared once here and called by both store adapters.
//
// Writing the same Execution id again replaces the existing entry rather than
// adding a second one, so a replay cannot make one Execution look like two
// assignments; and the index keeps at most MaxProviderAssignments entries in
// insertion order, dropping the oldest, so the enumeration is bounded by a
// constant instead of by the Requirement count.
func AppendProviderAssignment(existing []ProviderAssignment, value ProviderAssignment) []ProviderAssignment {
	out := make([]ProviderAssignment, 0, len(existing)+1)
	for _, a := range existing {
		if a.ExecutionID == value.ExecutionID {
			continue
		}
		out = append(out, a)
	}
	out = append(out, value)
	if len(out) > MaxProviderAssignments {
		out = out[len(out)-MaxProviderAssignments:]
	}
	return out
}

// ---------------------------------------------------------------------------
// The read model
// ---------------------------------------------------------------------------

// ProviderAssignmentView is one currently-assigned Execution.
type ProviderAssignmentView struct {
	ExecutionID string `json:"execution_id"`
	IncrementID string `json:"increment_id"`
	Since       string `json:"since"`
}

// ProviderRunawayDetectionView reports the runner-local detector by state
// alone. There is no number in it and there is no mutation path to it: the
// registry cannot clear a stop, because clearing one requires the owner to
// issue a new approved record.
type ProviderRunawayDetectionView struct {
	Scope                string               `json:"scope"`
	State                ProviderRunawayState `json:"state"`
	ThresholdsDeclaredIn string               `json:"thresholds_declared_in"`
}

// ProviderConcurrencyView reports the allocation limit the control plane
// genuinely owns, with the source of the ceiling named so a reader can tell a
// design ceiling from a limit an owner chose.
type ProviderConcurrencyView struct {
	ActiveAssignments int                   `json:"active_assignments"`
	DeclaredCeiling   int                   `json:"declared_ceiling"`
	CeilingSource     ProviderCeilingSource `json:"ceiling_source"`
	Remaining         int                   `json:"remaining"`
	Exhausted         bool                  `json:"exhausted"`
}

// ProviderEntryView is one Provider's row.
//
// Authorized and VerifiedByLoopInvocation are two separately-sourced facts and
// never one blurred flag: authorized is the owner's in-repository standing
// authorization, verified is whether the Loop has ever completed an invocation
// through this Provider. Today they disagree for two of the three, and a
// single boolean would have to pick one and be wrong about the other.
//
// LastObservedAt is the empty string when this Provider has never been
// observed. No instant is synthesized for an absence.
type ProviderEntryView struct {
	Provider                 ProviderName                 `json:"provider"`
	Authorized               bool                         `json:"authorized"`
	AuthorizationRef         string                       `json:"authorization_ref"`
	VerifiedByLoopInvocation bool                         `json:"verified_by_loop_invocation"`
	Health                   ProviderHealth               `json:"health"`
	BlockedReason            ProviderBlockedReason        `json:"blocked_reason"`
	LastObservedAt           string                       `json:"last_observed_at"`
	ObservationCount         int                          `json:"observation_count"`
	Stale                    bool                         `json:"stale"`
	RunawayDetection         ProviderRunawayDetectionView `json:"runaway_detection"`
	Concurrency              ProviderConcurrencyView      `json:"concurrency"`
	Assignments              []ProviderAssignmentView     `json:"assignments"`
}

// ProviderRegistryView is the whole GET /v1/providers response.
//
// There is deliberately no cursor and no page_size: the collection is a
// three-element closed set and cannot grow, and MaxPageSize and cursors exist
// for collections that can.
//
// There is deliberately no tally of any kind -- no failure count, no degraded
// count, no assignable count, no eligible count. Two reasons, both measured
// rather than aesthetic. A stop for inspection is neither a success nor a
// failure, so any failure tally would have to carve out an exception for it and
// would eventually forget to; and an assignable count would be a second,
// derived statement of what each entry's own health and blocked_reason already
// say exactly, which is how two numbers come to disagree. A reader counts rows.
type ProviderRegistryView struct {
	Providers []ProviderEntryView `json:"providers"`
}

// ---------------------------------------------------------------------------
// Derivation
// ---------------------------------------------------------------------------

// providerHealthFromFailure maps one failure class onto a health value and the
// human action it implies. The mapping is total over the closed enum: every
// declared class has a case, and TestProviderRegistryFailureClassMapIsTotal
// fails if one does not.
func providerHealthFromFailure(class ProviderFailureClass) (ProviderHealth, ProviderBlockedReason) {
	switch class {
	case ProviderFailureUnauthenticated:
		// The one case where the blocked reason names a human action no agent
		// can perform: authenticating a CLI uses the owner's own identity.
		return ProviderHealthUnauthenticated, ProviderBlockedOwnerMustAuthenticate
	case ProviderFailureTransport, ProviderFailureRateLimited, ProviderFailureTimeout:
		return ProviderHealthDegraded, ProviderBlockedLastInvocationRetryable
	case ProviderFailureModel, ProviderFailureContract:
		return ProviderHealthUnavailable, ProviderBlockedLastInvocationPermanent
	case ProviderFailureUnclassified:
		return ProviderHealthUnknown, ProviderBlockedLastInvocationUnclassed
	}
	// An undeclared class never reaches here: Validate refuses it at the edge
	// and the closed-enum test refuses it in source. Reporting unknown rather
	// than a plausible value is the fail-closed answer if it ever did.
	return ProviderHealthUnknown, ProviderBlockedLastInvocationUnclassed
}

// providerEntryView projects one Provider's stored state onto the wire shape
// at the instant now.
//
// The order of the three branches below is the whole of "silence is not
// health":
//
//   - No observation at all is unknown and stale, with the reason
//     never-invoked-by-loop. It is never healthy, and it is never unavailable
//     either: nothing was measured, so nothing failed.
//   - A stop for inspection is its own health value and its own runaway state.
//     It is reported whatever its age, because a stop that ages out into
//     silence would be a stop nobody was told about.
//   - Otherwise the newest observation decides health, and the age of that
//     observation decides stale. A stale entry keeps the health it was last
//     observed to have and reports its age; it is not silently refreshed, and
//     it is not silently downgraded either.
//
// runaway_detection.state is within-thresholds only when a fresh observation
// reported no stop. A stale observation that reported no stop yields unknown,
// because "the detector was within its thresholds a day ago" is not a
// statement about now -- while a stop is preserved regardless of age.
func providerEntryView(name ProviderName, log ProviderObservationLog, assignments []ProviderAssignment, now time.Time, ceiling int, ceilingSource ProviderCeilingSource) ProviderEntryView {
	authorized, ref := providerAuthorization(name)
	entry := ProviderEntryView{
		Provider:                 name,
		Authorized:               authorized,
		AuthorizationRef:         ref,
		VerifiedByLoopInvocation: !log.VerifiedAt.IsZero(),
		Health:                   ProviderHealthUnknown,
		BlockedReason:            ProviderBlockedNeverInvoked,
		Stale:                    true,
		RunawayDetection: ProviderRunawayDetectionView{
			Scope:                ProviderRunawayScope,
			State:                ProviderRunawayUnknown,
			ThresholdsDeclaredIn: ProviderRunawayThresholdsDeclaredIn,
		},
		Assignments: make([]ProviderAssignmentView, 0, len(assignments)),
	}

	for _, a := range assignments {
		entry.Assignments = append(entry.Assignments, ProviderAssignmentView{
			ExecutionID: a.ExecutionID,
			IncrementID: a.IncrementID,
			Since:       a.Since.UTC().Format(time.RFC3339Nano),
		})
	}
	active := len(entry.Assignments)
	// The ceiling is the effective installation concurrency limit, read through
	// the same accessor GET /v1/queue/summary uses, so the two surfaces cannot
	// report two different ceilings for one state (V2-068 A19). With no Control
	// Intent revision having ever declared a limit it is the architecture design
	// ceiling and the source says so.
	remaining := ceiling - active
	if remaining < 0 {
		remaining = 0
	}
	entry.Concurrency = ProviderConcurrencyView{
		ActiveAssignments: active,
		DeclaredCeiling:   ceiling,
		CeilingSource:     ceilingSource,
		Remaining:         remaining,
		Exhausted:         active >= ceiling,
	}

	if len(log.Observations) == 0 {
		return entry
	}

	newest := log.Observations[0]
	entry.LastObservedAt = newest.ObservedAt.UTC().Format(time.RFC3339Nano)
	entry.Stale = now.Sub(newest.ObservedAt) > ProviderObservationWindow
	for _, o := range log.Observations {
		if now.Sub(o.ObservedAt) <= ProviderObservationWindow {
			entry.ObservationCount++
		}
	}

	switch {
	case newest.StoppedForInspection:
		entry.Health = ProviderHealthStoppedForInspection
		entry.BlockedReason = ProviderBlockedOwnerMustClearRunawayStop
		entry.RunawayDetection.State = ProviderRunawayStoppedForInspection
	case newest.FailureClass == "":
		entry.Health = ProviderHealthHealthy
		entry.BlockedReason = ProviderNotBlocked
		if !entry.Stale {
			entry.RunawayDetection.State = ProviderRunawayWithinThresholds
		}
	default:
		entry.Health, entry.BlockedReason = providerHealthFromFailure(newest.FailureClass)
		if !entry.Stale {
			entry.RunawayDetection.State = ProviderRunawayWithinThresholds
		}
	}
	return entry
}

// ---------------------------------------------------------------------------
// The owner read
// ---------------------------------------------------------------------------

// Providers is the owner-role read behind GET /v1/providers.
//
// It performs no mutation of any application record and enqueues no outbox
// item; the only document it writes is the per-transaction quota reservation
// every bounded owner read already writes. It starts no process, opens no
// socket and consults no timer: every value comes from two keyed reads per
// Provider plus one keyed read per retained assignment, all bounded by
// constants that do not grow with the Requirement count.
func (s *Service) Providers(ctx context.Context) (ProviderRegistryView, error) {
	if _, _, err := callerActor(ctx, RoleOwner); err != nil {
		return ProviderRegistryView{}, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return ProviderRegistryView{}, errors.New("clock returned zero time")
	}
	var out ProviderRegistryView
	err := s.transact(ctx, func(u UnitOfWork) error {
		// One keyed side-table read, shared by all three rows: the effective
		// installation concurrency limit and where it came from.
		ceiling, limitSource, _, e := effectiveAllocationLimit(ctx, u)
		if e != nil {
			return e
		}
		ceilingSource := ProviderCeilingArchitectureDesign
		if limitSource == AllocationLimitFromControlRevision {
			ceilingSource = ProviderCeilingOwnerDeclared
		}
		rows := make([]ProviderEntryView, 0, len(declaredProviders))
		for _, name := range declaredProviders {
			log, e := u.ProviderObservations(ctx, name)
			if e != nil {
				return e
			}
			assignments, e := u.ProviderAssignments(ctx, name)
			if e != nil {
				return e
			}
			active, e := activeProviderAssignments(ctx, u, assignments)
			if e != nil {
				return e
			}
			rows = append(rows, providerEntryView(name, log, active, now, ceiling, ceilingSource))
		}
		out = ProviderRegistryView{Providers: rows}
		return nil
	})
	return out, err
}

// activeProviderAssignments drops the assignments whose Execution has reached a
// terminal status, and the assignments whose Execution no longer exists. The
// join lives here rather than in the store adapters so the terminal rule is
// stated once, by the same executionAlreadyTerminal predicate the Claim
// reclaim path uses, instead of once per adapter.
//
// The read is one keyed Execution read per retained assignment, so it is
// bounded by MaxProviderAssignments per Provider -- a constant, independent of
// the Requirement count.
func activeProviderAssignments(ctx context.Context, u UnitOfWork, assignments []ProviderAssignment) ([]ProviderAssignment, error) {
	out := make([]ProviderAssignment, 0, len(assignments))
	for _, a := range assignments {
		exec, ok, err := u.Execution(ctx, a.ExecutionID)
		if err != nil {
			return nil, err
		}
		if !ok || executionAlreadyTerminal(exec.Status) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// One behavioural table, two adapters
// ---------------------------------------------------------------------------
//
// This table is data, not test machinery: it imports nothing and asserts
// nothing. It lives here so internal/store/memory and internal/store/firestore
// run the same cases by value and the memory store cannot pass behaviour the
// Firestore store does not implement. Each adapter's test supplies the driver.

// ProviderRegistryCase is one case of that table. Observations and assignments
// are saved in order, each in its own transaction.
type ProviderRegistryCase struct {
	Name             string
	Observations     []ProviderObservation
	Assignments      []ProviderAssignment
	Query            ProviderName
	WantObservations []ProviderObservation
	WantVerified     bool
	WantAssignments  []ProviderAssignment
}

// ProviderRegistryCases is the shared behavioural table. Every case is about
// persistence only: retention, ordering, stickiness and keying. Health
// derivation is not in it, because health is a pure function of a log and an
// instant and belongs in one place, not in both adapters.
func ProviderRegistryCases() []ProviderRegistryCase {
	at := time.Unix(1700000000, 0).UTC()
	obs := func(name ProviderName, class ProviderFailureClass, stopped bool, when time.Time) ProviderObservation {
		return ProviderObservation{Provider: name, FailureClass: class, StoppedForInspection: stopped, ObservedAt: when}
	}
	assign := func(execID, incID string, name ProviderName, when time.Time) ProviderAssignment {
		return ProviderAssignment{ExecutionID: execID, IncrementID: incID, Provider: name, Since: when}
	}

	// More observations than the retention bound, newest last on the way in.
	over := make([]ProviderObservation, 0, MaxProviderObservations+5)
	overWant := make([]ProviderObservation, 0, MaxProviderObservations)
	for i := 0; i < MaxProviderObservations+5; i++ {
		over = append(over, obs(ProviderClaude, ProviderFailureTransport, false, at.Add(time.Duration(i)*time.Minute)))
	}
	for i := len(over) - 1; i >= len(over)-MaxProviderObservations; i-- {
		overWant = append(overWant, over[i])
	}

	// More assignments than the assignment bound.
	overAssign := make([]ProviderAssignment, 0, MaxProviderAssignments+3)
	overAssignWant := make([]ProviderAssignment, 0, MaxProviderAssignments)
	for i := 0; i < MaxProviderAssignments+3; i++ {
		a := assign(fmt.Sprintf("execution-%03d", i), "increment-1", ProviderCodex, at)
		overAssign = append(overAssign, a)
		if i >= 3 {
			overAssignWant = append(overAssignWant, a)
		}
	}

	return []ProviderRegistryCase{
		{
			Name:             "one observation round-trips through a separate read",
			Observations:     []ProviderObservation{obs(ProviderClaude, "", false, at)},
			Query:            ProviderClaude,
			WantObservations: []ProviderObservation{obs(ProviderClaude, "", false, at)},
			WantVerified:     true,
		},
		{
			Name: "observations come back newest first",
			Observations: []ProviderObservation{
				obs(ProviderClaude, "", false, at),
				obs(ProviderClaude, ProviderFailureTransport, false, at.Add(time.Minute)),
				obs(ProviderClaude, ProviderFailureModel, false, at.Add(2*time.Minute)),
			},
			Query: ProviderClaude,
			WantObservations: []ProviderObservation{
				obs(ProviderClaude, ProviderFailureModel, false, at.Add(2*time.Minute)),
				obs(ProviderClaude, ProviderFailureTransport, false, at.Add(time.Minute)),
				obs(ProviderClaude, "", false, at),
			},
			WantVerified: true,
		},
		{
			Name: "a later failure never un-verifies a provider the loop did complete",
			Observations: []ProviderObservation{
				obs(ProviderClaude, "", false, at),
				obs(ProviderClaude, ProviderFailureUnauthenticated, false, at.Add(time.Minute)),
			},
			Query: ProviderClaude,
			WantObservations: []ProviderObservation{
				obs(ProviderClaude, ProviderFailureUnauthenticated, false, at.Add(time.Minute)),
				obs(ProviderClaude, "", false, at),
			},
			WantVerified: true,
		},
		{
			Name:             "a stop for inspection does not verify and does not prevent a later completion",
			Observations:     []ProviderObservation{obs(ProviderCodex, "", true, at)},
			Query:            ProviderCodex,
			WantObservations: []ProviderObservation{obs(ProviderCodex, "", true, at)},
			WantVerified:     false,
		},
		{
			Name:             "failures alone never set verified",
			Observations:     []ProviderObservation{obs(ProviderOpenCode, ProviderFailureUnauthenticated, false, at)},
			Query:            ProviderOpenCode,
			WantObservations: []ProviderObservation{obs(ProviderOpenCode, ProviderFailureUnauthenticated, false, at)},
			WantVerified:     false,
		},
		{
			Name: "one provider's observations are invisible to another",
			Observations: []ProviderObservation{
				obs(ProviderClaude, "", false, at),
				obs(ProviderCodex, ProviderFailureUnauthenticated, false, at),
			},
			Query:            ProviderOpenCode,
			WantObservations: nil,
			WantVerified:     false,
		},
		{
			Name:             "more observations than the retention bound keep the newest and drop the oldest",
			Observations:     over,
			Query:            ProviderClaude,
			WantObservations: overWant,
			WantVerified:     false,
		},
		{
			Name:            "assignments come back execution-id ascending and only for their own provider",
			Assignments:     []ProviderAssignment{assign("execution-c", "increment-1", ProviderClaude, at), assign("execution-a", "increment-2", ProviderClaude, at), assign("execution-b", "increment-3", ProviderCodex, at)},
			Query:           ProviderClaude,
			WantAssignments: []ProviderAssignment{assign("execution-a", "increment-2", ProviderClaude, at), assign("execution-c", "increment-1", ProviderClaude, at)},
		},
		{
			Name:            "more assignments than the bound keep the newest and drop the oldest",
			Assignments:     overAssign,
			Query:           ProviderCodex,
			WantAssignments: overAssignWant,
		},
		{
			Name:            "re-assigning the same execution id replaces rather than duplicates",
			Assignments:     []ProviderAssignment{assign("execution-a", "increment-1", ProviderClaude, at), assign("execution-a", "increment-1", ProviderClaude, at.Add(time.Minute))},
			Query:           ProviderClaude,
			WantAssignments: []ProviderAssignment{assign("execution-a", "increment-1", ProviderClaude, at.Add(time.Minute))},
		},
	}
}
