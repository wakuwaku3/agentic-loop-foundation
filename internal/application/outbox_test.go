package application_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

type outboxTestClock struct {
	mu  sync.Mutex
	now time.Time
}

type sequenceClock struct {
	mu     sync.Mutex
	values []time.Time
	last   time.Time
}

func (c *sequenceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.values) > 0 {
		c.last = c.values[0]
		c.values = c.values[1:]
	}
	return c.last
}

func (c *outboxTestClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *outboxTestClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type fakeEffectSink struct {
	mu    sync.Mutex
	calls []application.EffectDelivery
	fail  error
	keys  map[string]bool
}

func (s *fakeEffectSink) Deliver(_ context.Context, e application.EffectDelivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string]bool{}
	}
	if !s.keys[e.IdempotencyKey] {
		s.keys[e.IdempotencyKey] = true
		s.calls = append(s.calls, e)
	}
	return s.fail
}
func (s *fakeEffectSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }

func seedOutbox(t *testing.T, st *memory.Store, at time.Time, id string) {
	t.Helper()
	err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		if err := u.SaveControl(context.Background(), domain.ControlIntent{Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "installation"}, Mode: domain.ControlAllow, Revision: 1, At: at}, 0); err != nil {
			return err
		}
		return u.Record(application.Event{ID: "event-" + id, RequestID: "request-" + id}, &application.OutboxItem{ID: id, OperationID: "operation-" + id, RequestID: "request-" + id, Kind: "control-changed", Target: "installation", ControlScope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "installation"}, ControlRevision: 1, CreatedAt: at})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedLease(t *testing.T, st *memory.Store, at time.Time, increment string) domain.Lease {
	t.Helper()
	l, err := domain.IssueLease(domain.LeaseRequest{ID: domain.LeaseID("lease-" + increment), ExecutionID: domain.ExecutionID("execution-" + increment), IncrementID: domain.IncrementID(increment), RunnerID: domain.RunnerID("runner"), IssuedAt: at.Add(-time.Second), ExpiresAt: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error { return u.SaveLease(context.Background(), l, 0) }); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestOutboxDispatcherAtLeastOnceAndCrashBeforeAck(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	seedOutbox(t, st, clock.Now(), "outbox-crash")
	sink := &fakeEffectSink{}
	crash := true
	d, err := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "one", LeaseTTL: time.Minute, Retry: application.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second}, AfterEffect: func(application.OutboxItem) error {
		if crash {
			crash = false
			return errors.New("simulated crash after effect")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if _, err = d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("idempotent sink calls=%d", got)
	}
	item, ok := st.OutboxByID("outbox-crash")
	if !ok || item.Status != application.OutboxDelivered {
		t.Fatalf("outbox=%#v found=%v", item, ok)
	}
}

func TestOutboxDispatcherRequiresUniqueOwner(t *testing.T) {
	clock := &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	for _, owner := range []string{"", "   "} {
		if _, err := application.NewOutboxDispatcher(memory.New(), clock, &fakeEffectSink{}, application.DispatcherConfig{Owner: owner}); err == nil {
			t.Fatalf("empty owner %q accepted", owner)
		}
	}
}

func TestOutboxDispatcherConcurrentClaimHasOneOwner(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	seedOutbox(t, st, clock.Now(), "outbox-concurrent")
	sink := &fakeEffectSink{}
	first := make(chan struct{})
	release := make(chan struct{})
	// A blocking sink makes the ownership window observable. The second
	// dispatcher sees delivering + an unexpired lease and cannot claim it.
	blocking := &blockingSink{entered: first, release: release, sink: sink}
	d1, _ := application.NewOutboxDispatcher(st, clock, blocking, application.DispatcherConfig{Owner: "owner-a", LeaseTTL: time.Minute})
	d2, _ := application.NewOutboxDispatcher(st, clock, blocking, application.DispatcherConfig{Owner: "owner-b", LeaseTTL: time.Minute})
	result := make(chan application.DispatchReport, 2)
	go func() { r, _ := d1.Dispatch(context.Background()); result <- r }()
	<-first
	go func() { r, _ := d2.Dispatch(context.Background()); result <- r }()
	time.Sleep(10 * time.Millisecond)
	close(release)
	<-result
	<-result
	if got := sink.count(); got != 1 {
		t.Fatalf("sink calls=%d", got)
	}
}

type fakeObserver struct {
	status application.OutboxObservation
	calls  int
}

func (o *fakeObserver) Observe(context.Context, application.EffectDelivery) (application.OutboxObservation, error) {
	o.calls++
	return o.status, nil
}

type deadlineSink struct{ calls int }

func (s *deadlineSink) Deliver(context.Context, application.EffectDelivery) error {
	s.calls++
	return context.DeadlineExceeded
}

func TestOutboxAmbiguousRequiresObservationBeforeAnyRedelivery(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	seedOutbox(t, st, clock.Now(), "outbox-ambiguous")
	sink := &deadlineSink{}
	first, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "first"})
	if _, err := first.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	item, _ := st.OutboxByID("outbox-ambiguous")
	if item.Status != application.OutboxAmbiguous || sink.calls != 1 {
		t.Fatalf("status=%s deliveries=%d", item.Status, sink.calls)
	}
	// Without an observer an ambiguous effect must remain parked and must not
	// be delivered a second time.
	second, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "second"})
	if _, err := second.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 {
		t.Fatalf("ambiguous operation was redelivered: %d", sink.calls)
	}
	observer := &fakeObserver{status: application.ObservationConfirmed}
	third, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "third", Observer: observer})
	if _, err := third.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	item, _ = st.OutboxByID("outbox-ambiguous")
	if observer.calls != 1 || sink.calls != 1 || item.Status != application.OutboxConfirmed {
		t.Fatalf("observe-first failed: observer=%d deliveries=%d status=%s", observer.calls, sink.calls, item.Status)
	}
}

func TestOutboxDispatcherRejectsStaleFenceAndStop(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	l := seedLease(t, st, clock.Now(), "increment-stale")
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		return u.Record(application.Event{ID: "event-stale"}, &application.OutboxItem{ID: "outbox-stale", OperationID: "operation-stale", Kind: "fake", Target: "target", IncrementID: l.IncrementID.String(), FencingToken: l.FencingToken - 1, CreatedAt: clock.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	sink := &fakeEffectSink{}
	d, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "stale", Retry: application.RetryPolicy{MaxAttempts: 3}})
	if _, err := d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 0 {
		t.Fatal("stale fence reached sink")
	}
	item, _ := st.OutboxByID("outbox-stale")
	if item.Status != application.OutboxDead {
		t.Fatalf("stale status=%s", item.Status)
	}

	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		if err := u.SaveControl(context.Background(), domain.ControlIntent{Scope: domain.ControlScope{Kind: domain.ScopeIncrement, Value: l.IncrementID.String()}, Mode: domain.ControlImmediateStop, Revision: 1, At: clock.Now()}, 0); err != nil {
			return err
		}
		target := domain.ControlTarget{IncrementID: l.IncrementID, RunnerID: l.RunnerID}
		if err := u.SaveCanonicalTarget(context.Background(), l.IncrementID.String(), target); err != nil {
			return err
		}
		return u.Record(application.Event{ID: "event-stop"}, &application.OutboxItem{ID: "outbox-stop", OperationID: "operation-stop", Kind: "fake", Target: "target", IncrementID: l.IncrementID.String(), LeaseID: l.ID.String(), RunnerID: l.RunnerID.String(), FencingToken: l.FencingToken, ControlRevision: 0, ControlTarget: target, PermitKind: domain.PermitExternalEffect, CreatedAt: clock.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 0 {
		t.Fatal("stop-protected effect reached sink")
	}
	item, _ = st.OutboxByID("outbox-stop")
	if item.Status != application.OutboxSuperseded {
		t.Fatalf("stop status=%s", item.Status)
	}
}

func TestOutboxDispatcherUsesFreshTimeBeforeEffect(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	clock := &sequenceClock{values: []time.Time{base, base.Add(2 * time.Second)}, last: base}
	st := memory.New()
	l := seedLease(t, st, base, "increment-time")
	// Bound the lease so it is valid at candidate/claim time but expired by
	// the fresh pre-effect transaction.
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		l.ExpiresAt = base.Add(time.Second)
		return u.SaveLease(context.Background(), l, l.Version)
	}); err != nil {
		t.Fatal(err)
	}
	target := domain.ControlTarget{IncrementID: l.IncrementID, RunnerID: l.RunnerID}
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		if err := u.SaveCanonicalTarget(context.Background(), l.IncrementID.String(), target); err != nil {
			return err
		}
		return u.Record(application.Event{ID: "event-time"}, &application.OutboxItem{ID: "outbox-time", OperationID: "operation-time", Kind: "effect", Target: "target", IncrementID: l.IncrementID.String(), LeaseID: l.ID.String(), RunnerID: l.RunnerID.String(), FencingToken: l.FencingToken, ControlTarget: target, PermitKind: domain.PermitExternalEffect, CreatedAt: base})
	}); err != nil {
		t.Fatal(err)
	}
	sink := &fakeEffectSink{}
	d, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "fresh-time"})
	if _, err := d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	item, _ := st.OutboxByID("outbox-time")
	if sink.count() != 0 || item.Status != application.OutboxSuperseded || item.LastError != "stale_fence" {
		t.Fatalf("stale pre-effect sink/status=%d/%#v", sink.count(), item)
	}
}

func TestOutboxDispatcherBoundedRetry(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	seedOutbox(t, st, clock.Now(), "outbox-retry")
	sink := &fakeEffectSink{fail: errors.New("temporary")}
	d, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "retry", Retry: application.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 2 * time.Second, Jitter: func(uint32) time.Duration { return 0 }}})
	for i := 0; i < 2; i++ {
		if _, err := d.Dispatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Second)
	}
	item, _ := st.OutboxByID("outbox-retry")
	if item.Status != application.OutboxDead || item.Attempts != 2 {
		t.Fatalf("retry=%#v", item)
	}
}

func TestOutboxDispatcherSanitizesProviderError(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	seedOutbox(t, st, clock.Now(), "outbox-secret")
	sink := &fakeEffectSink{fail: errors.New("provider token=super-secret payload=private")}
	d, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "sanitize", Retry: application.RetryPolicy{MaxAttempts: 2}})
	if _, err := d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	item, _ := st.OutboxByID("outbox-secret")
	if item.LastError != "delivery_failed" || strings.Contains(item.LastError, "super-secret") || strings.Contains(item.LastError, "private") {
		t.Fatalf("unsanitized error=%q", item.LastError)
	}
}

func TestOutboxDispatcherRejectsMalformedNonBoundEffect(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		return u.Record(application.Event{ID: "event-malformed"}, &application.OutboxItem{ID: "outbox-malformed", OperationID: "operation-malformed", Kind: "fake", Target: "target", CreatedAt: clock.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	sink := &fakeEffectSink{}
	d, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "malformed"})
	if _, err := d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	item, _ := st.OutboxByID("outbox-malformed")
	if sink.count() != 0 || item.Status != application.OutboxDead || item.LastError != "not_ready" {
		t.Fatalf("malformed sink/status=%d/%#v", sink.count(), item)
	}
}

func TestOutboxDispatcherAllowsControlPropagationDuringStop(t *testing.T) {
	st, clock := memory.New(), &outboxTestClock{now: time.Unix(1700000000, 0).UTC()}
	if err := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		if err := u.SaveControl(context.Background(), domain.ControlIntent{Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "installation"}, Mode: domain.ControlImmediateStop, Revision: 1, At: clock.Now()}, 0); err != nil {
			return err
		}
		return u.Record(application.Event{ID: "event-stop-propagation"}, &application.OutboxItem{ID: "outbox-stop-propagation", OperationID: "operation-stop-propagation", Kind: "control-changed", Target: "installation", ControlScope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "installation"}, ControlRevision: 1, CreatedAt: clock.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	sink := &fakeEffectSink{}
	d, _ := application.NewOutboxDispatcher(st, clock, sink, application.DispatcherConfig{Owner: "control"})
	if _, err := d.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	item, _ := st.OutboxByID("outbox-stop-propagation")
	if sink.count() != 1 || item.Status != application.OutboxDelivered {
		t.Fatalf("control propagation sink/status=%d/%#v", sink.count(), item)
	}
}

type blockingSink struct {
	entered, release chan struct{}
	sink             *fakeEffectSink
}

func (s *blockingSink) Deliver(ctx context.Context, e application.EffectDelivery) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.sink.Deliver(ctx, e)
}
