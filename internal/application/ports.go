package application

import (
	"context"
	"errors"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
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
	OutboxPending    OutboxStatus = "pending"
	OutboxDelivering OutboxStatus = "delivering"
	OutboxDelivered  OutboxStatus = "delivered"
	OutboxDead       OutboxStatus = "dead"
	OutboxWaiting    OutboxStatus = "waiting"
)

func (s OutboxStatus) Valid() bool {
	switch s {
	case "", OutboxPending, OutboxDelivering, OutboxDelivered, OutboxDead, OutboxWaiting:
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
type TargetRepository interface {
	CanonicalTarget(ctx context.Context, incrementID, runnerID string) (domain.ControlTarget, bool, error)
	SaveCanonicalTarget(ctx context.Context, incrementID string, target domain.ControlTarget) error
}
type ControlRepository interface {
	Controls(ctx context.Context) ([]domain.ControlIntent, error)
	SaveControl(ctx context.Context, value domain.ControlIntent, expected domain.Revision) error
	ControlRevision(ctx context.Context) (domain.Revision, error)
}
type ControlProgressRepository interface {
	ControlProgress(ctx context.Context, revision domain.Revision) (domain.ControlProgress, bool, error)
	SaveControlProgress(ctx context.Context, value domain.ControlProgress, expected domain.ControlState) error
}
type RunnerObservationRepository interface {
	RunnerObservation(ctx context.Context, runnerID string) (domain.RunnerObservation, bool, error)
	SaveRunnerObservation(ctx context.Context, value domain.RunnerObservation) error
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
	TargetRepository
	ControlRepository
	ControlProgressRepository
	RunnerObservationRepository
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
