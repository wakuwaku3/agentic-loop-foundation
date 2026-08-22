package application_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

type ids struct {
	mu sync.Mutex
	n  int
}

func (i *ids) Next(kind string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	return kind + "-" + string(rune('a'+i.n)), nil
}

type clock struct{ now time.Time }

func (c clock) Now() time.Time {
	if c.now.IsZero() {
		return time.Unix(1700000000, 0).UTC()
	}
	return c.now
}

func service() (*application.Service, *memory.Store) {
	st := memory.New()
	s, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		panic(err)
	}
	return s, st
}
func owner(ctx context.Context) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "actor-1"})
}
func runner(ctx context.Context, id string) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: "runner-subject", RunnerID: id})
}

func TestCallerAuthenticationAndFailClosedAuthority(t *testing.T) {
	st := memory.New()
	if _, err := application.NewService(st, clock{}, &ids{}); err == nil {
		t.Fatal("empty installation authority accepted")
	}
	s, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Capture(context.Background(), application.CaptureRequest{RequestID: "unauth"}); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("got %v", err)
	}
	if _, err = s.Capture(application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleRunner, Subject: "runner", RunnerID: "r1"}), application.CaptureRequest{RequestID: "runner-capture"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner role mismatch: %v", err)
	}
}
func TestCaptureIsIdempotent(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	a, err := s.Capture(ctx, application.CaptureRequest{RequestID: "request-1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Capture(ctx, application.CaptureRequest{RequestID: "request-1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("retry changed response: %#v %#v", a, b)
	}
	if got := len(st.Events()); got != 1 {
		t.Fatalf("events=%d", got)
	}
	if got := len(st.Outbox()); got != 0 {
		t.Fatalf("outbox=%d", got)
	}
}

func TestTransactionRollsBackAllRecords(t *testing.T) {
	_, st := service()
	ctx := owner(context.Background())
	before := len(st.Events())
	err := st.Transact(ctx, func(u application.UnitOfWork) error {
		id, _ := domain.NewRequirementID("r")
		if e := u.SaveRequirement(ctx, domain.Requirement{ID: id, Status: domain.RequirementCaptured, Version: 1}, 0); e != nil {
			return e
		}
		_ = u.Record(application.Event{ID: "event", AggregateID: "r"}, &application.OutboxItem{ID: "outbox"})
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if _, ok := st.Requirement("r"); ok {
		t.Fatal("current state leaked")
	}
	if len(st.Events()) != before || len(st.Outbox()) != 0 {
		t.Fatal("event/outbox leaked")
	}
}

func TestOptimisticConflict(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	id, _ := domain.NewRequirementID("r")
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, domain.Requirement{ID: id, Status: domain.RequirementCaptured, Version: 1}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, domain.Requirement{ID: id, Status: domain.RequirementFraming, Version: 2}, 0)
	})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentClaimOnlyOneSucceeds(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	p, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: p.RequirementID, ExpectedRequirementVersion: p.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, plan.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		at := clock{}.Now()
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: at, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, e := s.Claim(runner(ctx, "runner-"+string(rune('0'+n))), application.ClaimRequest{RequestID: "claim-" + string(rune('0'+n)), IncrementID: plan.IncrementID, ExpectedIncrementVersion: 2})
			results <- e
		}(n)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful claims=%d", success)
	}
}

func TestStopDeniesPermit(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	if _, err := s.Control(ctx, application.ControlRequest{RequestID: "stop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlImmediateStop, At: clock{}.Now()}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Permit(ctx, application.PermitRequest{RequestID: "permit-1", Kind: domain.PermitClaim, Target: domain.ControlTarget{InstallationID: "install"}, ControlRevision: 1})
	if !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("expected control denial, got %v", err)
	}
	if result.Allowed {
		t.Fatal("stop allowed claim")
	}
}

func TestRequestFingerprintAndRawText(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	if _, err := s.Capture(owner(ctx), application.CaptureRequest{}); err == nil {
		t.Fatal("empty request id accepted")
	}
	got, err := s.Capture(owner(ctx), application.CaptureRequest{RequestID: "same", Text: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.RequirementText(got.RequirementID); !ok {
		t.Fatal("raw text was not persisted")
	}
	if _, err = s.Capture(owner(ctx), application.CaptureRequest{RequestID: "same", Text: "changed"}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("got %v", err)
	}
	if len(st.Events()) != 1 {
		t.Fatalf("events=%d", len(st.Events()))
	}
	record, ok := st.Idempotency("same")
	if !ok || record.Fingerprint == "" || len(record.ResponseJSON) == 0 {
		t.Fatal("canonical idempotency record missing")
	}
}

func TestCaptureHonorsCanonicalInstallationIntakeControl(t *testing.T) {
	st := memory.New()
	s, _ := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	ctx := context.Background()
	now := clock{}.Now()
	if _, err := s.Control(owner(ctx), application.ControlRequest{RequestID: "pause", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlPauseIntake, At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(owner(ctx), application.CaptureRequest{RequestID: "capture-paused", Text: "blocked"}); !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("capture bypassed pause-intake: %v", err)
	}
}

func TestControlAndResultWriteOutbox(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	now := clock{}.Now()
	control, err := s.Control(owner(ctx), application.ControlRequest{RequestID: "ctl", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "i"}, Mode: domain.ControlPauseClaim, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Outbox()) != 1 || st.Outbox()[0].ControlRevision != control.Revision {
		t.Fatal("control outbox metadata missing")
	}
	if _, err := s.Control(owner(ctx), application.ControlRequest{RequestID: "bad", Scope: domain.ControlScope{Kind: domain.ControlScopeKind("unknown"), Value: "i"}, Mode: domain.ControlAllow, At: now}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

func TestExpiredLeaseCanBeReclaimed(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, _ := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	ctx := context.Background()
	ctx = owner(ctx)
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "p", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		ready, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: t0, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, ready, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	first, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "cl1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	cclock.now = t0.Add(2 * time.Minute)
	second, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "cl2", IncrementID: p.IncrementID, ExpectedIncrementVersion: first.Version})
	if err != nil {
		t.Fatal(err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Fatal("fencing token did not advance")
	}
}

func TestClaimCannotBypassCanonicalStop(t *testing.T) {
	st := memory.New()
	s, _ := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	ctx := context.Background()
	now := clock{}.Now()
	ctx = owner(ctx)
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: now, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Control(owner(ctx), application.ControlRequest{RequestID: "stop", Scope: domain.ControlScope{Kind: domain.ScopeInstallation, Value: "install"}, Mode: domain.ControlImmediateStop, At: now}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Claim(runner(ctx, "r"), application.ClaimRequest{RequestID: "claim", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("claim bypassed stop: %v", err)
	}
}

func TestAcceptResultWritesEffectOutbox(t *testing.T) {
	s, st := service()
	ctx := context.Background()
	now := clock{}.Now()
	ctx = owner(ctx)
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-r"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan-r", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, _, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		aid, _ := domain.NewActorID("a")
		next, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: aid, At: now, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(runner(ctx, "runner-r"), application.ClaimRequest{RequestID: "claim-r", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Transact(ctx, func(u application.UnitOfWork) error {
		exec, _, e := u.Execution(ctx, claim.ExecutionID)
		if e != nil {
			return e
		}
		exec.Status = domain.ExecutionRunning
		return u.SaveExecution(ctx, exec, exec.Version)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptResult(runner(ctx, "cross-runner"), application.AcceptResultRequest{RequestID: "cross-result", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: 1, FencingToken: claim.FencingToken, Succeeded: true}); !errors.Is(err, domain.ErrLeaseNotOwned) {
		t.Fatalf("cross-runner result accepted: %v", err)
	}
	if _, err = s.AcceptResult(runner(ctx, "runner-r"), application.AcceptResultRequest{RequestID: "result-r", ExecutionID: claim.ExecutionID, LeaseID: claim.LeaseID, ExpectedExecutionVersion: 1, FencingToken: claim.FencingToken, Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	outbox := st.Outbox()
	if len(outbox) < 2 {
		t.Fatalf("outbox=%d", len(outbox))
	}
	last := outbox[len(outbox)-1]
	if last.OperationID == "" || last.ExpectedVersion == 0 || last.FencingToken == 0 {
		t.Fatalf("incomplete result effect: %#v", last)
	}
}
