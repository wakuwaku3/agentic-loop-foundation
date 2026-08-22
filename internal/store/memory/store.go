// Package memory provides a small transactional adapter used by application
// tests and by the M0/M1 vertical slice. It intentionally has no domain
// policy: policy remains in application and domain packages.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// ErrOptimisticConflict is an adapter spelling of the domain's stale-version
// condition. Callers may use either errors.Is value at the boundary.
var ErrOptimisticConflict = domain.ErrStaleVersion

type state struct {
	requirements map[string]domain.Requirement
	increments   map[string]domain.Increment
	executions   map[string]domain.Execution
	leases       map[string]domain.Lease
	controls     []domain.ControlIntent
	requests     map[string]application.IdempotentResponse
	texts        map[string]string
	targets      map[string]domain.ControlTarget
	events       []application.Event
	outbox       []application.OutboxItem
}

func newState() state {
	return state{requirements: map[string]domain.Requirement{}, increments: map[string]domain.Increment{}, executions: map[string]domain.Execution{}, leases: map[string]domain.Lease{}, requests: map[string]application.IdempotentResponse{}, texts: map[string]string{}, targets: map[string]domain.ControlTarget{}}
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
		v.Payload = append([]byte(nil), o.Payload...)
		u.s.outbox = append(u.s.outbox, v)
	}
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
