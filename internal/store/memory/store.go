// Package memory provides a small transactional adapter used by application
// tests and by the M0/M1 vertical slice. It intentionally has no domain
// policy: policy remains in application and domain packages.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
)

// ErrOptimisticConflict is an adapter spelling of the domain's stale-version
// condition. Callers may use either errors.Is value at the boundary.
var ErrOptimisticConflict = domain.ErrStaleVersion

type state struct {
	requirements       map[string]domain.Requirement
	increments         map[string]domain.Increment
	executions         map[string]domain.Execution
	leases             map[string]domain.Lease
	controls           []domain.ControlIntent
	controlProgress    map[domain.Revision]domain.ControlProgress
	runnerObservations map[string]domain.RunnerObservation
	requests           map[string]application.IdempotentResponse
	texts              map[string]string
	targets            map[string]domain.ControlTarget
	events             []application.Event
	outbox             []application.OutboxItem
	quota              quota.Counter
}

func newState() state {
	return state{requirements: map[string]domain.Requirement{}, increments: map[string]domain.Increment{}, executions: map[string]domain.Execution{}, leases: map[string]domain.Lease{}, requests: map[string]application.IdempotentResponse{}, texts: map[string]string{}, targets: map[string]domain.ControlTarget{}, controlProgress: map[domain.Revision]domain.ControlProgress{}, runnerObservations: map[string]domain.RunnerObservation{}}
}
func (s state) clone() state {
	n := newState()
	for k, v := range s.requirements {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
		n.requirements[k] = v
	}
	for k, v := range s.increments {
		n.increments[k] = v
	}
	for k, v := range s.executions {
		n.executions[k] = v
	}
	for k, v := range s.leases {
		n.leases[k] = v
	}
	n.controls = append([]domain.ControlIntent(nil), s.controls...)
	for k, v := range s.controlProgress {
		n.controlProgress[k] = v
	}
	for k, v := range s.runnerObservations {
		v.Processes = append([]domain.ProcessObservation(nil), v.Processes...)
		n.runnerObservations[k] = v
	}
	for k, v := range s.requests {
		n.requests[k] = v
	}
	for k, v := range s.texts {
		n.texts[k] = v
	}
	for k, v := range s.targets {
		n.targets[k] = v
	}
	n.events = append([]application.Event(nil), s.events...)
	n.quota = s.quota
	for _, v := range s.outbox {
		v.Payload = append([]byte(nil), v.Payload...)
		n.outbox = append(n.outbox, v)
	}
	return n
}

type Store struct {
	mu   sync.Mutex
	data state
}

func New() *Store { return &Store{data: newState()} }

// NewStore is the descriptive spelling used by adapters and tests.
func NewStore() *Store { return New() }

type Clock struct{ NowValue time.Time }

func (c Clock) Now() time.Time { return c.NowValue }

type unit struct {
	s   *state
	ctx context.Context
}

func (u *unit) ReserveQuota(_ context.Context, key string, at time.Time, usage quota.Usage) error {
	return u.s.quota.Reserve(at, key, usage, quota.DefaultBudget)
}

func (m *Store) Transact(ctx context.Context, fn func(application.UnitOfWork) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	working := m.data.clone()
	u := &unit{s: &working, ctx: ctx}
	if err := fn(u); err != nil {
		return err
	}
	m.data = working
	return nil
}

// AuthorityContext is used by application event recording to carry the
// timestamp captured before a transaction callback.
func (u *unit) AuthorityContext() context.Context { return u.ctx }

func (u *unit) Requirement(_ context.Context, id string) (domain.Requirement, bool, error) {
	v, ok := u.s.requirements[id]
	if ok {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
	}
	return v, ok, nil
}
func (u *unit) Requirements(_ context.Context) ([]domain.Requirement, error) {
	out := make([]domain.Requirement, 0, len(u.s.requirements))
	for _, v := range u.s.requirements {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
		out = append(out, v)
	}
	return out, nil
}

// RequirementsPage keeps pagination work proportional to the requested page;
// the in-memory adapter mirrors Firestore's ordering and exclusive cursor.
func (u *unit) RequirementsPage(_ context.Context, afterID string, limit int) ([]domain.Requirement, bool, error) {
	rows := make([]domain.Requirement, 0, len(u.s.requirements))
	for _, v := range u.s.requirements {
		v.Increments = append([]domain.IncrementID(nil), v.Increments...)
		rows = append(rows, v)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID.String() < rows[j].ID.String() })
	start := 0
	for start < len(rows) && rows[start].ID.String() <= afterID {
		start++
	}
	if limit <= 0 {
		return nil, false, nil
	}
	end := start + limit
	more := end < len(rows)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], more, nil
}
func (u *unit) RequirementTexts(_ context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		if v, ok := u.s.texts[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}
func (u *unit) IncrementsForRequirements(_ context.Context, ids []string) ([]domain.Increment, error) {
	filter := map[string]bool{}
	for _, id := range ids {
		filter[id] = true
	}
	all := len(ids) == 0
	out := make([]domain.Increment, 0, len(u.s.increments))
	for _, v := range u.s.increments {
		if all || filter[v.ID.String()] || filter[v.RequirementID.String()] {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) ExecutionsForIncrements(_ context.Context, ids []string) ([]domain.Execution, error) {
	filter := map[string]bool{}
	for _, id := range ids {
		filter[id] = true
	}
	out := make([]domain.Execution, 0, len(u.s.executions))
	for _, v := range u.s.executions {
		if filter[v.IncrementID.String()] {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (u *unit) EventsPage(_ context.Context, afterID string, limit int) ([]application.Event, bool, error) {
	rows := append([]application.Event(nil), u.s.events...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	start := 0
	for start < len(rows) && rows[start].ID <= afterID {
		start++
	}
	end := start + limit
	more := end < len(rows)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end], more, nil
}
func (u *unit) QueueSummary(_ context.Context) (application.QueueSummary, error) {
	out := application.QueueSummary{ByRequirementStatus: map[string]int{}, ByIncrementStatus: map[string]int{}, Requirements: len(u.s.requirements), Increments: len(u.s.increments)}
	for _, v := range u.s.requirements {
		out.ByRequirementStatus[string(v.Status)]++
	}
	for _, v := range u.s.increments {
		out.ByIncrementStatus[string(v.Status)]++
	}
	for _, v := range u.s.executions {
		if v.Status == domain.ExecutionRunning || v.Status == domain.ExecutionStarting {
			out.ActiveExecutions++
		}
	}
	return out, nil
}
func (u *unit) SaveRequirement(_ context.Context, v domain.Requirement, expected domain.Version) error {
	key := v.ID.String()
	old, ok := u.s.requirements[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	v.Increments = append([]domain.IncrementID(nil), v.Increments...)
	u.s.requirements[key] = v
	return nil
}
func (u *unit) SaveRequirementText(_ context.Context, id, text string) error {
	u.s.texts[id] = text
	return nil
}
func (u *unit) RequirementText(_ context.Context, id string) (string, bool, error) {
	v, ok := u.s.texts[id]
	return v, ok, nil
}
func (u *unit) Increment(_ context.Context, id string) (domain.Increment, bool, error) {
	v, ok := u.s.increments[id]
	return v, ok, nil
}
func (u *unit) SaveIncrement(_ context.Context, v domain.Increment, expected domain.Version) error {
	key := v.ID.String()
	old, ok := u.s.increments[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.s.increments[key] = v
	return nil
}
func (u *unit) Execution(_ context.Context, id string) (domain.Execution, bool, error) {
	v, ok := u.s.executions[id]
	return v, ok, nil
}
func (u *unit) SaveExecution(_ context.Context, v domain.Execution, expected domain.Version) error {
	key := v.ID.String()
	old, ok := u.s.executions[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.s.executions[key] = v
	return nil
}
func (u *unit) Lease(_ context.Context, id string) (domain.Lease, bool, error) {
	v, ok := u.s.leases[id]
	return v, ok, nil
}
func (u *unit) SaveLease(_ context.Context, v domain.Lease, expected domain.Version) error {
	key := v.ID.String()
	old, ok := u.s.leases[key]
	if (!ok && expected != 0) || (ok && old.Version != expected) {
		return ErrOptimisticConflict
	}
	u.s.leases[key] = v
	return nil
}
func (u *unit) ActiveLeaseForIncrement(_ context.Context, id string) (domain.Lease, bool, error) {
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && v.Status == domain.LeaseActive {
			return v, true, nil
		}
	}
	return domain.Lease{}, false, nil
}
func (u *unit) ActiveLeaseForIncrementAt(_ context.Context, id string, at time.Time) (domain.Lease, bool, error) {
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && v.Status == domain.LeaseActive && v.ActiveAt(at) {
			return v, true, nil
		}
	}
	return domain.Lease{}, false, nil
}
func (u *unit) ExpiredActiveLeases(_ context.Context, at time.Time, cursor string, limit int) ([]domain.Lease, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows := make([]domain.Lease, 0)
	for _, lease := range u.s.leases {
		if lease.Status == domain.LeaseActive && !lease.ExpiresAt.After(at) && lease.ID.String() > cursor {
			rows = append(rows, lease)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID.String() < rows[j].ID.String() })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	next := ""
	if len(rows) == limit {
		next = rows[len(rows)-1].ID.String()
	}
	return rows, next, nil
}
func (u *unit) ActiveLeases(_ context.Context, limit int) ([]domain.Lease, error) {
	if limit <= 0 || limit > 101 {
		limit = 101
	}
	out := make([]domain.Lease, 0, limit)
	for _, lease := range u.s.leases {
		if lease.Status == domain.LeaseActive {
			out = append(out, lease)
			if len(out) > limit {
				break
			}
		}
	}
	return out, nil
}
func (u *unit) ExecutionByLease(_ context.Context, leaseID string) (domain.Execution, bool, error) {
	for _, execution := range u.s.executions {
		if execution.LeaseID.String() == leaseID {
			return execution, true, nil
		}
	}
	return domain.Execution{}, false, nil
}
func (u *unit) PendingControlProgresses(_ context.Context, limit int) ([]domain.ControlProgress, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	out := make([]domain.ControlProgress, 0, limit)
	for _, p := range u.s.controlProgress {
		if p.Verification == domain.VerificationPending {
			out = append(out, p)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (u *unit) OutboxResolution(_ context.Context, leaseID string) (application.OutboxResolution, error) {
	var result application.OutboxResolution
	for _, item := range u.s.outbox {
		if item.LeaseID != leaseID {
			continue
		}
		switch item.Status {
		case application.OutboxDelivered, application.OutboxConfirmed, application.OutboxSuperseded:
		case application.OutboxAmbiguous, application.OutboxReconciling, application.OutboxNeedsInput, application.OutboxDead:
			result.Ambiguous = true
		default:
			result.Pending = true
		}
	}
	return result, nil
}
func (u *unit) LatestLeaseForIncrement(_ context.Context, id string) (domain.Lease, bool, error) {
	var latest domain.Lease
	found := false
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && (!found || v.FencingToken > latest.FencingToken) {
			latest, found = v, true
		}
	}
	return latest, found, nil
}
func (u *unit) MaxFencingToken(_ context.Context, id string) (domain.FencingToken, error) {
	var n domain.FencingToken
	for _, v := range u.s.leases {
		if v.IncrementID.String() == id && v.FencingToken > n {
			n = v.FencingToken
		}
	}
	return n, nil
}
func (u *unit) CanonicalTarget(_ context.Context, incrementID, _ string) (domain.ControlTarget, bool, error) {
	v, ok := u.s.targets[incrementID]
	return v, ok, nil
}
func (u *unit) SaveCanonicalTarget(_ context.Context, incrementID string, target domain.ControlTarget) error {
	u.s.targets[incrementID] = target
	return nil
}
func (u *unit) Controls(_ context.Context) ([]domain.ControlIntent, error) {
	return append([]domain.ControlIntent(nil), u.s.controls...), nil
}
func (u *unit) SaveControl(_ context.Context, v domain.ControlIntent, expected domain.Revision) error {
	var cur domain.Revision
	for _, x := range u.s.controls {
		if x.Revision > cur {
			cur = x.Revision
		}
	}
	if cur != expected {
		return ErrOptimisticConflict
	}
	u.s.controls = append(u.s.controls, v)
	return nil
}
func (u *unit) ControlRevision(_ context.Context) (domain.Revision, error) {
	var n domain.Revision
	for _, v := range u.s.controls {
		if v.Revision > n {
			n = v.Revision
		}
	}
	return n, nil
}
func (u *unit) ControlProgress(_ context.Context, revision domain.Revision) (domain.ControlProgress, bool, error) {
	v, ok := u.s.controlProgress[revision]
	return v, ok, nil
}
func (u *unit) SaveControlProgress(_ context.Context, value domain.ControlProgress, expected domain.ControlState) error {
	old, ok := u.s.controlProgress[value.Revision]
	if !ok {
		if expected != "" {
			return domain.ErrStaleVersion
		}
	} else if old.State != expected {
		return domain.ErrStaleVersion
	}
	u.s.controlProgress[value.Revision] = value
	return nil
}
func (u *unit) RunnerObservation(_ context.Context, runnerID string) (domain.RunnerObservation, bool, error) {
	v, ok := u.s.runnerObservations[runnerID]
	v.Processes = append([]domain.ProcessObservation(nil), v.Processes...)
	return v, ok, nil
}
func (u *unit) SaveRunnerObservation(_ context.Context, value domain.RunnerObservation) error {
	value.Processes = append([]domain.ProcessObservation(nil), value.Processes...)
	u.s.runnerObservations[value.RunnerID.String()] = value
	return nil
}
func (u *unit) Idempotency(_ context.Context, id, op string) (application.IdempotentResponse, bool, error) {
	v, ok := u.s.requests[id]
	if ok && v.Operation != op {
		return v, true, application.ErrIdempotencyConflict
	}
	return v, ok, nil
}
func (u *unit) SaveIdempotency(_ context.Context, v application.IdempotentResponse) error {
	if old, ok := u.s.requests[v.RequestID]; ok && (old.Operation != v.Operation || old.Fingerprint != v.Fingerprint) {
		return application.ErrIdempotencyConflict
	}
	u.s.requests[v.RequestID] = v
	return nil
}
func (u *unit) Record(e application.Event, o *application.OutboxItem) error {
	u.s.events = append(u.s.events, e)
	if o != nil {
		v := *o
		if !v.Status.Valid() {
			return application.ErrInvalidOutbox
		}
		if v.Version == 0 {
			v.Version = 1
		}
		if v.Status == "" {
			v.Status = application.OutboxPending
		}
		v.Payload = append([]byte(nil), o.Payload...)
		u.s.outbox = append(u.s.outbox, v)
	}
	return nil
}

func (u *unit) Outbox(_ context.Context, id string) (application.OutboxItem, bool, error) {
	for _, v := range u.s.outbox {
		if v.ID == id {
			if !v.Status.Valid() {
				return application.OutboxItem{}, false, application.ErrInvalidOutbox
			}
			v.Payload = append([]byte(nil), v.Payload...)
			return v, true, nil
		}
	}
	return application.OutboxItem{}, false, nil
}

func (u *unit) Outboxes(_ context.Context, now time.Time, limit int) ([]application.OutboxItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows := make([]application.OutboxItem, 0, len(u.s.outbox))
	for _, v := range u.s.outbox {
		if !v.Status.Valid() {
			return nil, application.ErrInvalidOutbox
		}
		status := v.Status
		if status == "" {
			status = application.OutboxPending
		}
		ready := status == application.OutboxPending || status == application.OutboxAmbiguous || status == application.OutboxReconciling || (status == application.OutboxWaiting && (v.NextAttemptAt.IsZero() || !v.NextAttemptAt.After(now))) || (status == application.OutboxDelivering && !v.DeliveryLeaseUntil.IsZero() && !v.DeliveryLeaseUntil.After(now))
		if ready {
			v.Status = status
			v.Payload = append([]byte(nil), v.Payload...)
			rows = append(rows, v)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (u *unit) SaveOutbox(_ context.Context, value application.OutboxItem, expected domain.Version) error {
	if value.ID == "" || !value.Status.Valid() {
		return application.ErrInvalidOutbox
	}
	if value.Status == "" {
		value.Status = application.OutboxPending
	}
	for i, old := range u.s.outbox {
		if old.ID != value.ID {
			continue
		}
		if old.Version == 0 {
			old.Version = 1
		}
		if expected != old.Version {
			return domain.ErrStaleVersion
		}
		value.Version = old.Version + 1
		value.Payload = append([]byte(nil), value.Payload...)
		u.s.outbox[i] = value
		return nil
	}
	if expected != 0 {
		return domain.ErrStaleVersion
	}
	if value.Version == 0 {
		value.Version = 1
	}
	u.s.outbox = append(u.s.outbox, value)
	return nil
}

func (m *Store) Requirement(id string) (domain.Requirement, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.requirements[id]
	v.Increments = append([]domain.IncrementID(nil), v.Increments...)
	return v, ok
}
func (m *Store) Increment(id string) (domain.Increment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.increments[id]
	return v, ok
}
func (m *Store) RequirementText(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.texts[id]
	return v, ok
}
func (m *Store) Execution(id string) (domain.Execution, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.executions[id]
	return v, ok
}
func (m *Store) Lease(id string) (domain.Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.leases[id]
	return v, ok
}
func (m *Store) Events() []application.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]application.Event(nil), m.data.events...)
}
func (m *Store) Outbox() []application.OutboxItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]application.OutboxItem, 0, len(m.data.outbox))
	for _, v := range m.data.outbox {
		v.Payload = append([]byte(nil), v.Payload...)
		out = append(out, v)
	}
	return out
}

func (m *Store) OutboxByID(id string) (application.OutboxItem, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.data.outbox {
		if v.ID == id {
			v.Payload = append([]byte(nil), v.Payload...)
			return v, true
		}
	}
	return application.OutboxItem{}, false
}
func (m *Store) Controls() []domain.ControlIntent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.ControlIntent(nil), m.data.controls...)
}
func (m *Store) Idempotency(requestID string) (application.IdempotentResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data.requests[requestID]
	v.ResponseJSON = append([]byte(nil), v.ResponseJSON...)
	return v, ok
}
