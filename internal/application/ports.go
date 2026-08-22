package application

import (
	"context"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

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
type IdempotencyRepository interface {
	Idempotency(ctx context.Context, requestID string, operation string) (IdempotentResponse, bool, error)
	SaveIdempotency(ctx context.Context, value IdempotentResponse) error
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
	IdempotencyRepository
	Record(event Event, outbox *OutboxItem) error
}
type Transactor interface {
	Transact(context.Context, func(UnitOfWork) error) error
}
