package application

// Outbox delivery deliberately lives in application, rather than in a store
// adapter. Stores only provide atomic claim/state transitions; an EffectSink
// is called after the transaction has committed. This is the boundary that
// prevents a Firestore transaction retry from repeating an external effect.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

var (
	ErrOutboxLeaseLost = errors.New("outbox delivery lease is no longer owned")
	ErrOutboxNotReady  = errors.New("outbox is not ready for delivery")
	ErrOutboxNoSink    = errors.New("outbox effect sink is required")
)

// outboxErrorCode is deliberately a closed vocabulary. Provider errors can
// contain credentials, request bodies, or other sensitive material and must
// never be copied into canonical state.
func outboxErrorCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrControlDenied):
		return "control_denied"
	case errors.Is(err, domain.ErrStaleFence):
		return "stale_fence"
	case errors.Is(err, domain.ErrLeaseExpired):
		return "lease_expired"
	case errors.Is(err, domain.ErrLeaseNotOwned):
		return "lease_not_owned"
	case errors.Is(err, ErrOutboxLeaseLost):
		return "lease_lost"
	case errors.Is(err, ErrOutboxNotReady):
		return "not_ready"
	default:
		return "delivery_failed"
	}
}

// EffectDelivery is the provider-neutral envelope sent to an external
// adapter. IdempotencyKey must be preserved by the adapter across retries.
type EffectDelivery struct {
	OutboxID        string
	OperationID     string
	IdempotencyKey  string
	RequestID       string
	Kind            string
	Target          string
	Payload         []byte
	ExpectedVersion domain.Version
	FencingToken    domain.FencingToken
	ControlRevision domain.Revision
}

// EffectSink is intentionally a single call outside a transaction. A sink
// should make OperationID/IdempotencyKey idempotent and return the same result
// on a duplicate call.
type EffectSink interface {
	Deliver(context.Context, EffectDelivery) error
}
type EffectObserver interface {
	Observe(context.Context, EffectDelivery) (OutboxObservation, error)
}

// EffectSinkFunc adapts a function to EffectSink, useful for small adapters
// and failure-injection tests.
type EffectSinkFunc func(context.Context, EffectDelivery) error

func (f EffectSinkFunc) Deliver(ctx context.Context, value EffectDelivery) error {
	return f(ctx, value)
}

type RetryPolicy struct {
	MaxAttempts uint32
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// Jitter is deterministic when supplied by a test or policy. It receives
	// the one-based attempt number and must return a non-negative duration.
	Jitter func(attempt uint32) time.Duration
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 5
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = time.Second
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = time.Minute
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}
	return p
}

func (p RetryPolicy) delay(attempt uint32) time.Duration {
	p = p.normalized()
	n := attempt
	if n == 0 {
		n = 1
	}
	if n > 62 {
		n = 62
	}
	seconds := p.BaseDelay
	for i := uint32(1); i < n; i++ {
		if seconds >= p.MaxDelay || seconds > time.Duration(math.MaxInt64/2) {
			seconds = p.MaxDelay
			break
		}
		seconds *= 2
	}
	if seconds > p.MaxDelay {
		seconds = p.MaxDelay
	}
	if p.Jitter != nil {
		j := p.Jitter(attempt)
		if j > 0 && seconds > time.Duration(math.MaxInt64)-j {
			return p.MaxDelay
		}
		seconds += j
		if seconds > p.MaxDelay {
			seconds = p.MaxDelay
		}
	}
	return seconds
}

type DispatcherConfig struct {
	Owner       string
	LeaseTTL    time.Duration
	BatchSize   int
	Retry       RetryPolicy
	AfterEffect func(OutboxItem) error
	Observer    EffectObserver
}

type DispatchReport struct {
	Claimed, Delivered, Waiting, Dead, Skipped, Failed int
}

type OutboxDispatcher struct {
	tx     Transactor
	clock  Clock
	sink   EffectSink
	config DispatcherConfig
}

// OutboxRepository is the persistence subset needed by a dispatcher. UnitOfWork
// embeds these methods so stores can be used directly as a Transactor.
type OutboxRepository interface {
	Outbox(context.Context, string) (OutboxItem, bool, error)
	Outboxes(context.Context, time.Time, int) ([]OutboxItem, error)
	SaveOutbox(context.Context, OutboxItem, domain.Version) error
}

func NewOutboxDispatcher(tx Transactor, clock Clock, sink EffectSink, config DispatcherConfig) (*OutboxDispatcher, error) {
	if tx == nil || clock == nil {
		return nil, errors.New("outbox transaction and clock are required")
	}
	if sink == nil {
		return nil, ErrOutboxNoSink
	}
	if strings.TrimSpace(config.Owner) == "" {
		return nil, errors.New("outbox dispatcher owner is required")
	}
	if config.LeaseTTL <= 0 {
		config.LeaseTTL = time.Minute
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	config.Retry = config.Retry.normalized()
	return &OutboxDispatcher{tx: tx, clock: clock, sink: sink, config: config}, nil
}

// NewDispatcher is kept as a concise constructor for scheduler integrations.
func NewDispatcher(tx Transactor, clock Clock, sink EffectSink, config DispatcherConfig) (*OutboxDispatcher, error) {
	return NewOutboxDispatcher(tx, clock, sink, config)
}

// Dispatch performs one bounded reconciliation pass. At most BatchSize
// records are claimed; every sink invocation is outside both transactions.
func (d *OutboxDispatcher) Dispatch(ctx context.Context) (DispatchReport, error) {
	var report DispatchReport
	now := d.clock.Now()
	if now.IsZero() {
		return report, errors.New("clock returned zero time")
	}
	var candidates []OutboxItem
	if err := d.tx.Transact(ctx, func(u UnitOfWork) error {
		var err error
		candidates, err = u.Outboxes(ctx, now, d.config.BatchSize)
		return err
	}); err != nil {
		return report, err
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	for _, candidate := range candidates {
		if candidate.Status == OutboxAmbiguous || candidate.Status == OutboxReconciling {
			if d.config.Observer == nil {
				report.Waiting++
				continue
			}
			// Step 1 of failure-model.md section 6: consult the local
			// record for the same Operation ID before reading the
			// external system. candidates above is a batch snapshot; by
			// the time this iteration runs, this item's own durable row
			// may already have resolved (for example a concurrent
			// dispatcher instance already observed and recorded the
			// outcome). Re-read it fresh and skip the external Observe
			// call entirely once it is no longer ambiguous, so an
			// already-confirmed or already-delivered item is never
			// re-observed (acceptance A15).
			current, found, err := d.localRecord(ctx, candidate.ID)
			if err != nil {
				return report, err
			}
			if found && current.Status != OutboxAmbiguous && current.Status != OutboxReconciling {
				switch current.Status {
				case OutboxConfirmed, OutboxDelivered, OutboxSuperseded:
					report.Delivered++
				case OutboxPending, OutboxWaiting, OutboxDelivering:
					report.Waiting++
				default:
					report.Failed++
				}
				continue
			}
			observation, observeErr := d.config.Observer.Observe(ctx, EffectDelivery{OutboxID: candidate.ID, OperationID: candidate.OperationID, IdempotencyKey: candidate.OperationID, RequestID: candidate.RequestID, Kind: candidate.Kind, Target: candidate.Target, Payload: append([]byte(nil), candidate.Payload...)})
			if observeErr != nil {
				report.Failed++
				continue
			}
			if err := d.resolveObservation(ctx, candidate.ID, observation); err != nil {
				return report, err
			}
			if observation == ObservationConfirmed {
				report.Delivered++
			} else if observation == ObservationNotObserved {
				report.Waiting++
			} else {
				report.Failed++
			}
			continue
		}
		claimed, err := d.claim(ctx, candidate.ID, now)
		if err != nil {
			if errors.Is(err, domain.ErrStaleVersion) || errors.Is(err, ErrOutboxLeaseLost) {
				report.Skipped++
				continue
			}
			return report, err
		}
		if !claimed {
			report.Skipped++
			continue
		}
		report.Claimed++
		item, err := d.beforeEffect(ctx, candidate.ID)
		if err != nil {
			statusErr := d.fail(ctx, candidate.ID, err, false)
			if statusErr != nil {
				return report, statusErr
			}
			if errors.Is(err, domain.ErrControlDenied) {
				report.Waiting++
			} else {
				report.Dead++
			}
			continue
		}
		key := item.OperationID
		if key == "" {
			key = item.ID
		}
		delivery := EffectDelivery{OutboxID: item.ID, OperationID: key, IdempotencyKey: key, RequestID: item.RequestID, Kind: item.Kind, Target: item.Target, Payload: append([]byte(nil), item.Payload...), ExpectedVersion: item.ExpectedVersion, FencingToken: item.FencingToken, ControlRevision: item.ControlRevision}
		err = d.deliver(ctx, delivery)
		if err == nil && d.config.AfterEffect != nil {
			err = d.config.AfterEffect(item)
		}
		if err != nil {
			if e := d.fail(ctx, item.ID, err, true); e != nil {
				return report, e
			}
			report.Failed++
			if item.Attempts+1 >= d.config.Retry.MaxAttempts {
				report.Dead++
			} else {
				report.Waiting++
			}
			continue
		}
		if err = d.ack(ctx, item.ID); err != nil {
			if errors.Is(err, domain.ErrStaleVersion) || errors.Is(err, ErrOutboxLeaseLost) {
				report.Skipped++
				continue
			}
			return report, err
		}
		report.Delivered++
	}
	return report, nil
}

// Reconcile is an explicit alias used by scheduler adapters.
func (d *OutboxDispatcher) Reconcile(ctx context.Context) (DispatchReport, error) {
	return d.Dispatch(ctx)
}

func (d *OutboxDispatcher) claim(ctx context.Context, id string, now time.Time) (bool, error) {
	claimed := false
	err := d.tx.Transact(ctx, func(u UnitOfWork) error {
		item, ok, err := u.Outbox(ctx, id)
		if err != nil || !ok {
			return err
		}
		status := item.Status
		if status == "" {
			status = OutboxPending
		}
		if status == OutboxDelivered || status == OutboxDead {
			return nil
		}
		if status == OutboxDelivering && item.DeliveryLeaseUntil.After(now) {
			return nil
		}
		if status == OutboxWaiting && !item.NextAttemptAt.IsZero() && item.NextAttemptAt.After(now) {
			return nil
		}
		if item.Version == 0 {
			item.Version = 1
		}
		item.Status, item.DeliveryOwner, item.DeliveryLeaseUntil = OutboxDelivering, d.config.Owner, now.Add(d.config.LeaseTTL)
		if err = u.SaveOutbox(ctx, item, item.Version); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}

func (d *OutboxDispatcher) beforeEffect(ctx context.Context, id string) (item OutboxItem, err error) {
	now := d.clock.Now()
	if now.IsZero() {
		return item, errors.New("clock returned zero time")
	}
	err = d.tx.Transact(ctx, func(u UnitOfWork) error {
		var ok bool
		item, ok, err = u.Outbox(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrOutboxNotReady
		}
		if item.Status != OutboxDelivering || item.DeliveryOwner != d.config.Owner || !item.DeliveryLeaseUntil.After(now) {
			return ErrOutboxLeaseLost
		}
		if item.Kind == "control-changed" {
			// Control propagation is the only intentionally non-fence-bound
			// outbox. It is allowed to proceed during a stop, but only while
			// its exact revision and scope intent are still current.
			if item.IncrementID != "" || item.LeaseID != "" || item.FencingToken != 0 || item.PermitKind != "" || item.ControlTarget != (domain.ControlTarget{}) || item.ControlScope.Value == "" || item.ControlRevision == 0 || item.Target == "" {
				return ErrOutboxNotReady
			}
			controls, e := u.Controls(ctx)
			if e != nil {
				return e
			}
			currentRevision := domain.Revision(0)
			for _, control := range controls {
				if control.Scope != item.ControlScope {
					continue
				}
				if control.Revision > currentRevision {
					currentRevision = control.Revision
				}
			}
			if currentRevision != item.ControlRevision {
				return domain.ErrControlDenied
			}
			return nil
		}
		if item.IncrementID == "" || item.LeaseID == "" || item.FencingToken == 0 || item.PermitKind == "" || item.ControlTarget.IncrementID == "" {
			return ErrOutboxNotReady
		}
		if item.ControlTarget.IncrementID.String() != item.IncrementID {
			return ErrOutboxNotReady
		}
		latest, found, e := u.LatestLeaseForIncrement(ctx, item.IncrementID)
		if e != nil {
			return e
		}
		if !found || latest.FencingToken != item.FencingToken || latest.Status != domain.LeaseActive || !latest.ActiveAt(now) {
			return domain.ErrStaleFence
		}
		if latest.ID.String() != item.LeaseID {
			return domain.ErrStaleFence
		}
		if item.RunnerID != "" && latest.RunnerID.String() != item.RunnerID {
			return domain.ErrLeaseNotOwned
		}
		if latest.ControlRevision != item.ControlRevision {
			return domain.ErrControlDenied
		}
		target, found, e := u.CanonicalTarget(ctx, item.IncrementID, item.RunnerID)
		if e != nil {
			return e
		}
		if !found || target != item.ControlTarget {
			return ErrOutboxNotReady
		}
		controls, e := u.Controls(ctx)
		if e != nil {
			return e
		}
		current := domain.EffectiveControl(controls, target)
		authoritative := domain.Revision(0)
		if current.Found {
			authoritative = current.Revision
		}
		if authoritative != item.ControlRevision {
			return domain.ErrControlDenied
		}
		_, e = domain.Permit(current, domain.PermitRequest{Kind: item.PermitKind, Target: target, ControlRevision: item.ControlRevision, FencingToken: item.FencingToken, ExpectedFencingToken: latest.FencingToken, Resource: item.Target})
		return e
	})
	return item, err
}

func (d *OutboxDispatcher) deliver(ctx context.Context, value EffectDelivery) error {
	return d.sink.Deliver(ctx, value)
}

func (d *OutboxDispatcher) ack(ctx context.Context, id string) error {
	now := d.clock.Now()
	if now.IsZero() {
		return errors.New("clock returned zero time")
	}
	return d.tx.Transact(ctx, func(u UnitOfWork) error {
		item, ok, err := u.Outbox(ctx, id)
		if err != nil || !ok {
			return err
		}
		if item.Status != OutboxDelivering || item.DeliveryOwner != d.config.Owner {
			return ErrOutboxLeaseLost
		}
		if !item.DeliveryLeaseUntil.After(now) {
			return ErrOutboxLeaseLost
		}
		item.Status, item.DeliveryOwner, item.DeliveryLeaseUntil, item.NextAttemptAt, item.DeliveredAt, item.LastError = OutboxDelivered, "", time.Time{}, time.Time{}, now, ""
		return u.SaveOutbox(ctx, item, item.Version)
	})
}

func (d *OutboxDispatcher) fail(ctx context.Context, id string, cause error, countAttempt bool) error {
	now := d.clock.Now()
	if now.IsZero() {
		return errors.New("clock returned zero time")
	}
	return d.tx.Transact(ctx, func(u UnitOfWork) error {
		item, ok, err := u.Outbox(ctx, id)
		if err != nil || !ok {
			return err
		}
		if item.Status != OutboxDelivering || item.DeliveryOwner != d.config.Owner {
			return ErrOutboxLeaseLost
		}
		if countAttempt {
			item.Attempts++
		}
		item.LastError = outboxErrorCode(cause)
		item.DeliveryOwner, item.DeliveryLeaseUntil = "", time.Time{}
		if isAmbiguous(cause) {
			item.Status = OutboxAmbiguous
			item.Observation = ObservationUnknown
		} else if errors.Is(cause, domain.ErrStaleFence) || errors.Is(cause, domain.ErrControlDenied) || errors.Is(cause, domain.ErrLeaseNotOwned) {
			item.Status = OutboxSuperseded
		} else if errors.Is(cause, ErrOutboxNotReady) {
			item.Status = OutboxDead
		} else if item.Attempts >= d.config.Retry.MaxAttempts {
			item.Status = OutboxDead
		} else {
			item.Status, item.NextAttemptAt = OutboxWaiting, now.Add(d.config.Retry.delay(item.Attempts))
		}
		return u.SaveOutbox(ctx, item, item.Version)
	})
}

func isAmbiguous(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var n net.Error
	return errors.As(err, &n) && (n.Timeout() || n.Temporary())
}

// localRecord re-reads one OutboxItem's own durable row, fresh, inside its
// own transaction. It is the "local record" of failure-model.md section 6
// step 1: consulted before any external read, so a resolution that already
// landed (by any path) since a batch snapshot was taken is never masked by
// a second, redundant external observation.
func (d *OutboxDispatcher) localRecord(ctx context.Context, id string) (OutboxItem, bool, error) {
	var item OutboxItem
	var found bool
	err := d.tx.Transact(ctx, func(u UnitOfWork) error {
		var err error
		item, found, err = u.Outbox(ctx, id)
		return err
	})
	return item, found, err
}

func (d *OutboxDispatcher) resolveObservation(ctx context.Context, id string, observation OutboxObservation) error {
	return d.tx.Transact(ctx, func(u UnitOfWork) error {
		item, ok, err := u.Outbox(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrOutboxNotReady
		}
		item.Observation = observation
		item.ObservedAt = d.clock.Now()
		switch observation {
		case ObservationConfirmed:
			item.Status = OutboxConfirmed
			item.DeliveredAt = item.ObservedAt
		case ObservationNotObserved:
			// Retry through the normal claim path with the same immutable
			// operation/idempotency ID only after absence was observed.
			item.Status = OutboxPending
			item.NextAttemptAt = time.Time{}
		default:
			item.Status = OutboxNeedsInput
		}
		return u.SaveOutbox(ctx, item, item.Version)
	})
}

func (d *OutboxDispatcher) String() string {
	return fmt.Sprintf("outbox-dispatcher(%s)", d.config.Owner)
}
