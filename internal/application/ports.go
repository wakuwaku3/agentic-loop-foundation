package application

import (
	"context"
	"errors"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
	"github.com/takushi/agentic-loop-foundation/v2/internal/release"
)

var ErrInvalidOutbox = errors.New("invalid outbox item")

// Clock and IDGenerator are the only sources of time and identifiers used by
// application services.  Keeping them as ports makes retries deterministic.
type Clock interface{ Now() time.Time }
type IDGenerator interface {
	Next(kind string) (string, error)
}

type Event struct {
	ID            string
	RequestID     string
	AggregateType string
	AggregateID   string
	Type          string
	ActorID       string
	Version       domain.Version
	At            time.Time
}

type OutboxItem struct {
	ID              string
	RequestID       string
	OperationID     string
	Kind            string
	Target          string
	ExpectedVersion domain.Version
	FencingToken    domain.FencingToken
	ControlRevision domain.Revision
	Payload         []byte
	CreatedAt       time.Time
	// Delivery state is owned by the outbox dispatcher. Version is the
	// optimistic-concurrency version of this record (zero is the legacy
	// pending value and is normalised on first claim).
	Status             OutboxStatus
	Version            domain.Version
	Attempts           uint32
	NextAttemptAt      time.Time
	DeliveryOwner      string
	DeliveryLeaseUntil time.Time
	LastError          string
	DeliveredAt        time.Time
	Observation        OutboxObservation
	ObservedAt         time.Time
	// IncrementID is retained so dispatch can revalidate the latest lease and
	// fencing token immediately before an external effect. Legacy records may
	// omit it, but are rejected as malformed; only an explicit control-changed
	// item is allowed to be non-fence-bound.
	IncrementID   string
	LeaseID       string
	RunnerID      string
	ControlTarget domain.ControlTarget
	ControlScope  domain.ControlScope
	PermitKind    domain.PermitKind
}

type OutboxStatus string

const (
	OutboxPending     OutboxStatus = "pending"
	OutboxDelivering  OutboxStatus = "delivering"
	OutboxDelivered   OutboxStatus = "delivered"
	OutboxDead        OutboxStatus = "dead"
	OutboxWaiting     OutboxStatus = "waiting"
	OutboxAmbiguous   OutboxStatus = "ambiguous"
	OutboxReconciling OutboxStatus = "reconciling"
	OutboxConfirmed   OutboxStatus = "confirmed"
	OutboxNotObserved OutboxStatus = "not-observed"
	OutboxSuperseded  OutboxStatus = "superseded"
	OutboxNeedsInput  OutboxStatus = "needs-input"
)

type OutboxObservation string

const (
	ObservationUnknown     OutboxObservation = "unknown"
	ObservationConfirmed   OutboxObservation = "confirmed"
	ObservationNotObserved OutboxObservation = "not-observed"
)

func (s OutboxStatus) Valid() bool {
	switch s {
	case "", OutboxPending, OutboxDelivering, OutboxDelivered, OutboxDead, OutboxWaiting, OutboxAmbiguous, OutboxReconciling, OutboxConfirmed, OutboxNotObserved, OutboxSuperseded, OutboxNeedsInput:
		return true
	default:
		return false
	}
}

type IdempotentResponse struct {
	RequestID    string
	Operation    string
	Fingerprint  string
	ResponseJSON []byte
	Value        any // process-local typed value; ResponseJSON is the durable form.
}

type RequirementRepository interface {
	Requirement(ctx context.Context, id string) (domain.Requirement, bool, error)
	Requirements(ctx context.Context) ([]domain.Requirement, error)
	SaveRequirement(ctx context.Context, value domain.Requirement, expected domain.Version) error
}

// RequirementReadRepository is the bounded production read-side contract. Implementations
// must apply the bound in the storage query (rather than loading the whole
// collection and slicing it in the application). afterID is an internal,
// stable ordering key; the public service wraps it in an opaque cursor.
type RequirementReadRepository interface {
	RequirementsPage(ctx context.Context, afterID string, limit int) ([]domain.Requirement, bool, error)
	RequirementTexts(ctx context.Context, ids []string) (map[string]string, error)
	IncrementsForRequirements(ctx context.Context, ids []string) ([]domain.Increment, error)
	ExecutionsForIncrements(ctx context.Context, ids []string) ([]domain.Execution, error)
}

// EventReadRepository is intentionally metadata-only. Provider output and
// credentials never cross this port.
type EventReadRepository interface {
	EventsPage(ctx context.Context, afterID string, limit int) ([]Event, bool, error)
}
type QueueSummaryRepository interface {
	QueueSummary(ctx context.Context) (QueueSummary, error)
}
type RequirementTextRepository interface {
	SaveRequirementText(ctx context.Context, id, text string) error
	RequirementText(ctx context.Context, id string) (string, bool, error)
}
type IncrementRepository interface {
	Increment(ctx context.Context, id string) (domain.Increment, bool, error)
	SaveIncrement(ctx context.Context, value domain.Increment, expected domain.Version) error
}
type ExecutionRepository interface {
	Execution(ctx context.Context, id string) (domain.Execution, bool, error)
	SaveExecution(ctx context.Context, value domain.Execution, expected domain.Version) error
}
type LeaseRepository interface {
	Lease(ctx context.Context, id string) (domain.Lease, bool, error)
	SaveLease(ctx context.Context, value domain.Lease, expected domain.Version) error
	ActiveLeaseForIncrement(ctx context.Context, incrementID string) (domain.Lease, bool, error)
	ActiveLeaseForIncrementAt(ctx context.Context, incrementID string, at time.Time) (domain.Lease, bool, error)
	LatestLeaseForIncrement(ctx context.Context, incrementID string) (domain.Lease, bool, error)
	MaxFencingToken(ctx context.Context, incrementID string) (domain.FencingToken, error)
}
type LeaseReconcileRepository interface {
	ActiveLeases(ctx context.Context, limit int) ([]domain.Lease, error)
	ExpiredActiveLeases(ctx context.Context, at time.Time, cursor string, limit int) ([]domain.Lease, string, error)
	ExecutionByLease(ctx context.Context, leaseID string) (domain.Execution, bool, error)
}
type VerificationRepository interface {
	PendingControlProgresses(ctx context.Context, limit int) ([]domain.ControlProgress, error)
	OutboxResolution(ctx context.Context, leaseID string) (OutboxResolution, error)
}

// OutboxResolution distinguishes work that may still settle from an external
// effect whose outcome cannot safely be inferred. Control verification must
// wait for the former and fail closed for the latter.
type OutboxResolution struct {
	Pending   bool
	Ambiguous bool
}
type TargetRepository interface {
	CanonicalTarget(ctx context.Context, incrementID, runnerID string) (domain.ControlTarget, bool, error)
	SaveCanonicalTarget(ctx context.Context, incrementID string, target domain.ControlTarget) error
}
type ControlRepository interface {
	Controls(ctx context.Context) ([]domain.ControlIntent, error)
	SaveControl(ctx context.Context, value domain.ControlIntent, expected domain.Revision) error
	ControlRevision(ctx context.Context) (domain.Revision, error)
}

// ControlRequestedByRepository is a side table keyed by Control Intent
// revision. domain.ControlIntent itself is immutable, proven-closed M1
// surface (internal/domain/control.go), so who requested a given revision is
// tracked here rather than as a new field on that struct. Each revision is
// written at most once, at the same time as the Control Intent it describes.
type ControlRequestedByRepository interface {
	SaveControlRequestedBy(ctx context.Context, revision domain.Revision, value domain.RequestedBy) error
	ControlRequestedBy(ctx context.Context, revision domain.Revision) (domain.RequestedBy, bool, error)
}

// AllocationLimitRepository is the side table of installation concurrency
// limits (V2-068), keyed by Control Intent revision. It follows
// ControlRequestedByRepository's precedent exactly, for the same reason:
// domain.ControlIntent is immutable, proven-closed M1 surface, and the limit is
// an attribute of the request rather than of the policy the permit gate
// evaluates -- domain.ControlMode keeps exactly its seven values and no permit
// decision ever consults this table.
//
// SaveAllocationLimit writes at most one row per revision, in the same
// transaction as the Control Intent it describes. A second write for the same
// revision naming a different limit is a conflict (domain.ErrStaleVersion); an
// identical re-write is an idempotent replay rather than a second record.
//
// EffectiveAllocationLimit returns the row with the greatest revision that
// declared a limit, and false when no revision ever has. That resolution rule is
// deliberate and belongs to the store contract rather than to a caller: a later
// revision that declares no limit must not clear the owner's allocation policy,
// so an unrelated pause-claim intent cannot silently reset it. The read is
// deterministic and clock-free, and its cost grows with the number of Control
// Intent revisions that declared a limit -- never with the Requirement count.
type AllocationLimitRepository interface {
	SaveAllocationLimit(ctx context.Context, value AllocationLimit) error
	AllocationLimit(ctx context.Context, revision domain.Revision) (AllocationLimit, bool, error)
	EffectiveAllocationLimit(ctx context.Context) (AllocationLimit, bool, error)
}
type ControlProgressRepository interface {
	ControlProgress(ctx context.Context, revision domain.Revision) (domain.ControlProgress, bool, error)
	SaveControlProgress(ctx context.Context, value domain.ControlProgress, expected domain.ControlState) error
}
type RunnerObservationRepository interface {
	RunnerObservation(ctx context.Context, runnerID string) (domain.RunnerObservation, bool, error)
	SaveRunnerObservation(ctx context.Context, value domain.RunnerObservation) error
}

// RunnerVersionReportRepository is the side table of Runner version reports
// (V2-069), keyed by RunnerID. It follows ControlRequestedByRepository's
// precedent exactly: the value is not an aggregate, has no state transition
// and no Version, and no domain rule consults it, so it is tracked here
// rather than as a new field on domain.RunnerObservation -- which is rebuilt
// in full on every heartbeat (service.go's SaveRunnerObservation call), so a
// version stored there would be zeroed by the next heartbeat that omitted the
// report.
//
// RunnerVersionReports is the one bounded enumeration the cross-machine
// question needs, and RunnerObservationRepository above is deliberately not
// widened to provide it. It returns one row per Runner the Control Plane has
// heard from -- joined with that Runner's report when one exists -- in
// RunnerID-ascending order, at most limit rows, with the bool reporting that
// the bound truncated the answer. A Runner with no report appears with a zero
// ReportedAt and zero coordinates: implementations must never synthesize an
// interval, a version or a digest for it, because "has not reported" and "is
// compatible" must stay distinguishable. The row count is the machine count
// (machines are not shared, docs/operations/self-update.md section 5.2) and
// is not a function of the Requirement count, which is why there is no
// cursor.
type RunnerVersionReportRepository interface {
	SaveRunnerVersionReport(ctx context.Context, value RunnerVersionReport) error
	RunnerVersionReport(ctx context.Context, runnerID string) (RunnerVersionReport, bool, error)
	RunnerVersionReports(ctx context.Context, limit int) ([]RunnerVersionReport, bool, error)
}

// RepositoryRepository is the persistence contract of the Repository
// aggregate and of its bounded forge Observation. It is one member interface
// (V2-064 A5): the Observation lives here rather than behind a second port
// because it is keyed by, and only meaningful for, a Repository, exactly as
// RunnerObservationRepository is keyed by a Runner.
//
// The Observation is bounded evidence, never canonical state: it is written
// only by a Runner submitting a measurement and is never treated as the
// Repository's own status.
type RepositoryRepository interface {
	Repository(ctx context.Context, id string) (domain.Repository, bool, error)
	Repositories(ctx context.Context) ([]domain.Repository, error)
	SaveRepository(ctx context.Context, value domain.Repository, expected domain.Version) error
	RepositoryObservation(ctx context.Context, repositoryID string) (domain.RepositoryObservation, bool, error)
	SaveRepositoryObservation(ctx context.Context, value domain.RepositoryObservation) error
}

// RequirementRepositoryLinkRepository is the persistence contract of the
// write-once Requirement-to-Repository association (V2-071). It follows
// ControlRequestedByRepository's precedent exactly: domain.Requirement lives
// in internal/domain/model.go, the proven-closed M1 surface, so which
// Repository a Requirement belongs to is tracked here as its own keyed
// record rather than as a new field on that struct.
//
// The record is written at most once per Requirement. An implementation must
// refuse a second link for the same Requirement naming a different
// Repository (a conflict, surfaced as domain.ErrStaleVersion) and must treat
// an identical re-write as an idempotent replay rather than a second record.
//
// The batch read and the per-repository read are bounded reads and must apply
// their bound in the storage query, matching RequirementReadRepository's
// stated contract above. RequirementIDsForRepository's bool reports that the
// bound truncated the answer, so a caller can say "at least n" rather than
// reporting a wrong total as if it were exact.
type RequirementRepositoryLinkRepository interface {
	SaveRequirementRepositoryLink(ctx context.Context, value domain.RequirementRepositoryLink) error
	RequirementRepositoryLink(ctx context.Context, requirementID string) (domain.RequirementRepositoryLink, bool, error)
	RequirementRepositoryLinks(ctx context.Context, ids []string) (map[string]domain.RequirementRepositoryLink, error)
	RequirementIDsForRepository(ctx context.Context, repositoryID string, limit int) ([]string, bool, error)
}

// ProviderObservationRepository is the persistence contract of the Provider
// observation ring (V2-067). It is keyed by the Provider name -- one document
// per declared Provider, three in total -- and never by Execution or
// Requirement, so the read cost of GET /v1/providers is a constant that does
// not grow with the Requirement count (docs/architecture/validation.md section
// 5). It follows ControlRequestedByRepository's precedent: the value is not an
// aggregate, has no state transition and no Version, and no domain rule
// consults it.
//
// SaveProviderObservation appends one observation and enforces two rules that
// belong to the record rather than to a caller:
//
//   - the ring keeps at most MaxProviderObservations entries per Provider,
//     newest first, dropping the oldest (TrimProviderObservations), and
//   - VerifiedAt is sticky: the first observation that Completed() sets it, and
//     nothing ever clears it. "Has the Loop ever completed an invocation
//     through this Provider" is a monotone historical fact, and deriving it
//     from a bounded ring would let later failures silently un-verify a
//     Provider that really was exercised.
//
// ProviderObservations returns the ring newest-first for one Provider, at most
// MaxProviderObservations entries, and the zero VerifiedAt when the Loop has
// never completed an invocation. An implementation must never synthesize an
// observation or an instant for a Provider it holds no record for: "not
// observed" and "healthy" must stay distinguishable, which is the whole reason
// this surface exists.
type ProviderObservationRepository interface {
	SaveProviderObservation(ctx context.Context, value ProviderObservation) error
	ProviderObservations(ctx context.Context, name ProviderName) (ProviderObservationLog, error)
}

// ProviderAssignmentRepository is the side table of Provider assignments
// (V2-067), keyed by Execution id. domain.Execution gains no Provider field:
// it sits inside the transition functions the M1 gate proved, and widening it
// would put a label with no transition semantics inside the structure every
// state assertion reads, for no gain -- the join Execution id -> Provider name
// is all the read model needs (dp-v2-067 d7).
//
// SaveProviderAssignment writes the keyed record and updates the per-Provider
// index the bounded enumeration reads. Re-writing the same Execution id
// replaces the record rather than adding a second one.
//
// ProviderAssignments enumerates one Provider's retained assignments, at most
// MaxProviderAssignments of them, in Execution-id ascending order
// (SortProviderAssignments). The bound is applied in the storage layer, not by
// slicing a full scan. Terminal filtering is deliberately NOT done here: the
// terminal rule is stated once in internal/application, by the same predicate
// the Claim reclaim path uses, rather than once per adapter.
type ProviderAssignmentRepository interface {
	SaveProviderAssignment(ctx context.Context, value ProviderAssignment) error
	ProviderAssignment(ctx context.Context, executionID string) (ProviderAssignment, bool, error)
	ProviderAssignments(ctx context.Context, name ProviderName) ([]ProviderAssignment, error)
}

type IdempotencyRepository interface {
	Idempotency(ctx context.Context, requestID string, operation string) (IdempotentResponse, bool, error)
	SaveIdempotency(ctx context.Context, value IdempotentResponse) error
}

// QuotaRepository reserves the bounded daily Firestore budget inside the same
// transaction as the caller's mutation. Implementations must fail closed when
// the reservation would exceed the 80% budget.
type QuotaRepository interface {
	ReserveQuota(ctx context.Context, key string, at time.Time, usage quota.Usage) error
}

// UnitOfWork is deliberately expressed in application terms. Persistence
// adapters can map these records to SQL, Firestore, or another store without
// exposing storage DTOs to the domain package.
type UnitOfWork interface {
	RequirementRepository
	RequirementTextRepository
	IncrementRepository
	ExecutionRepository
	LeaseRepository
	LeaseReconcileRepository
	VerificationRepository
	TargetRepository
	ControlRepository
	ControlRequestedByRepository
	AllocationLimitRepository
	ControlProgressRepository
	RunnerObservationRepository
	RunnerVersionReportRepository
	ProviderObservationRepository
	ProviderAssignmentRepository
	RepositoryRepository
	RequirementRepositoryLinkRepository
	RequirementReadRepository
	EventReadRepository
	QueueSummaryRepository
	IdempotencyRepository
	QuotaRepository
	Outbox(ctx context.Context, id string) (OutboxItem, bool, error)
	Outboxes(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error)
	SaveOutbox(ctx context.Context, value OutboxItem, expected domain.Version) error
	Record(event Event, outbox *OutboxItem) error
}
type Transactor interface {
	Transact(context.Context, func(UnitOfWork) error) error
}

// ReleaseObserver is the read-only port over the release machinery
// (V2-066). It is deliberately not part of UnitOfWork: none of it is
// canonical state in the store. It is what one running process can say
// about the Preview release it was itself assembled from.
//
// The import direction is internal/application -> internal/release, and it
// stops here: internal/api imports neither internal/release nor
// internal/update, which a go/ast guard in internal/api asserts.
//
// Every method is a read. Nothing behind this port promotes, rolls back or
// changes a route.
type ReleaseObserver interface {
	// ReleaseSnapshot returns the promotion report assembled once, when the
	// observer was constructed, together with the instant it was assembled.
	// Implementations must not walk the source tree per call.
	ReleaseSnapshot() (release.PromotionReport, time.Time)
	// ObservedRepository names the repository this observer was configured
	// for. It is never inferred.
	ObservedRepository() string
	// RecordedRoute returns this process's own recorded route and true, or
	// the zero Route and false when this process has recorded no route. An
	// implementation must never default or infer a route.
	RecordedRoute() (release.Route, bool)
	// RollbackHistory returns every rollback this process recorded for the
	// observed repository, in the order they were recorded.
	RollbackHistory() []release.RollbackRecord
}
