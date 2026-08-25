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

// =============================================================================
// V2-058: Service.Claim's reclaim branch terminates the superseded Execution
// in the same transaction that reclaims its expired Lease (dp-v2-058 d1),
// closing the source of the orphan that previously only OrphanSweep could
// eventually converge. Acceptance A1's reproduction against the unmodified
// source (a real `go test` run showing the superseded Execution left at
// status=running, version=2 after reclaim) was captured before this fix
// landed and is recorded in .agents/v2/evidence/V2-058-application.json; it
// is not kept as a permanent test here because, once fixed, the same drive
// sequence no longer reproduces the bug -- that outcome is what
// TestClaimReclaimTerminatesSupersededExecutionAtSource below asserts
// instead (A5's positive invariant).
// =============================================================================

// isExecutionTerminalForTest mirrors domain.MarkExecutionLost's own notion
// of "already terminal" (internal/reconciler's executionIsTerminal uses the
// identical set) purely for test assertions; it duplicates no production
// logic and calls no unexported symbol.
func isExecutionTerminalForTest(status domain.ExecutionStatus) bool {
	switch status {
	case domain.ExecutionSucceeded, domain.ExecutionFailed, domain.ExecutionTerminated, domain.ExecutionLost:
		return true
	}
	return false
}

// TestClaimReclaimTerminatesSupersededExecutionAtSource is V2-058's core
// positive test (A2, A4's version/fencing assertions, A5). It drives a
// claim, start, clock-advance-past-TTL, reclaim sequence and asserts the
// superseded Execution is terminal (ExecutionLost) at the source of the
// reclaim itself -- not eventually, via OrphanSweep -- with its Version
// advanced by exactly one and its FencingToken retained, and that no
// non-terminal Execution is left behind an inactive Lease for the
// Increment.
func TestClaimReclaimTerminatesSupersededExecutionAtSource(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(runner(ctx, "r1"), application.StartRequest{RequestID: "start-1", ExecutionID: attempt1.ExecutionID, ExpectedExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	before, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("attempt 1 execution missing before reclaim")
	}

	cclock.now = t0.Add(2 * time.Minute) // > the 1-minute LeaseTTL
	attempt2, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatal(err)
	}
	if attempt2.ExecutionID == attempt1.ExecutionID {
		t.Fatal("reclaim reused the superseded execution id")
	}
	if attempt2.LeaseID == attempt1.LeaseID {
		t.Fatal("reclaim reused the superseded lease id")
	}
	if attempt2.FencingToken <= attempt1.FencingToken {
		t.Fatal("fencing token did not strictly increase on reclaim")
	}

	// A2/A5: the superseded Execution is terminal at the source, not
	// eventually via OrphanSweep.
	after, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("superseded execution disappeared")
	}
	if after.Status != domain.ExecutionLost {
		t.Fatalf("superseded execution status = %s, want %s", after.Status, domain.ExecutionLost)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("superseded execution version = %d, want exactly one more than %d", after.Version, before.Version)
	}
	if after.FencingToken != before.FencingToken {
		t.Fatalf("superseded execution fencing token changed: before=%d after=%d", before.FencingToken, after.FencingToken)
	}

	// A5: the positive invariant, checked directly against the store (never
	// by invoking OrphanSweep) -- no non-terminal Execution exists behind
	// an inactive Lease for this Increment.
	supersededLease, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("superseded lease missing")
	}
	if supersededLease.ActiveAt(cclock.now) {
		t.Fatal("superseded lease should be inactive after expiry")
	}
	if !isExecutionTerminalForTest(after.Status) {
		t.Fatalf("no non-terminal execution may exist behind an inactive lease for this increment; got status %s", after.Status)
	}
}

// TestClaimReclaimAtomicWithSupersededTermination is A4: the superseded
// Execution's termination and the rest of the reclaim are one unit. It
// forces Claim's reclaim transaction to fail AFTER the termination work has
// already been staged inside the same closure (by requesting a Target whose
// RequirementID cannot match the canonical target that Plan already saved,
// which Claim only checks later in the same mutate closure via sameTarget)
// and asserts that neither the new Lease nor the superseded Execution's
// terminal status is visible; a subsequent successful reclaim then makes
// both visible together.
func TestClaimReclaimAtomicWithSupersededTermination(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(runner(ctx, "r1"), application.StartRequest{RequestID: "start-1", ExecutionID: attempt1.ExecutionID, ExpectedExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	before, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("attempt 1 execution missing before reclaim")
	}
	leaseBefore, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("attempt 1 lease missing before reclaim")
	}

	cclock.now = t0.Add(2 * time.Minute)

	bogusReqID, err := domain.NewRequirementID("does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	_, failErr := s.Claim(runner(ctx, "r2"), application.ClaimRequest{
		RequestID: "claim-2-fails", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version,
		Target: domain.ControlTarget{RequirementID: bogusReqID},
	})
	if !errors.Is(failErr, domain.ErrControlDenied) {
		t.Fatalf("expected the forced failure to be ErrControlDenied, got %v", failErr)
	}

	// Neither the new Lease nor the superseded Execution's terminal status
	// is visible after the failed transaction.
	afterFailedExec, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("superseded execution missing after failed reclaim")
	}
	if afterFailedExec != before {
		t.Fatalf("superseded execution changed despite the reclaim transaction failing: before=%#v after=%#v", before, afterFailedExec)
	}
	afterFailedLease, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("superseded lease missing after failed reclaim")
	}
	if afterFailedLease != leaseBefore {
		t.Fatal("superseded lease changed despite the reclaim transaction failing")
	}

	// A successful reclaim then makes both visible together.
	attempt2, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2-succeeds", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatal(err)
	}
	if attempt2.ExecutionID == "" || attempt2.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}
	afterExec, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("superseded execution missing after successful reclaim")
	}
	if afterExec.Status != domain.ExecutionLost {
		t.Fatalf("superseded execution status = %s, want %s", afterExec.Status, domain.ExecutionLost)
	}
	afterLease, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("superseded lease missing after successful reclaim")
	}
	if afterLease.ActiveAt(cclock.now) {
		t.Fatal("superseded lease should be inactive after a successful reclaim")
	}
}

// TestClaimReclaimSkipsWhenSupersededExecutionIsAbsent is A3's first skip
// branch: the expired Lease's ExecutionID has no corresponding Execution
// record at all (manufactured directly against the store, since
// Service.Claim itself never leaves a Lease pointing at nothing). Claim
// still succeeds and returns a usable new Lease/Execution.
func TestClaimReclaimSkipsWhenSupersededExecutionIsAbsent(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	iid, err := domain.NewIncrementID(p.IncrementID)
	if err != nil {
		t.Fatal(err)
	}
	ghostLeaseID, err := domain.NewLeaseID("ghost-lease")
	if err != nil {
		t.Fatal(err)
	}
	ghostExecID, err := domain.NewExecutionID("ghost-execution-never-saved")
	if err != nil {
		t.Fatal(err)
	}
	ghostRunnerID, err := domain.NewRunnerID("r1")
	if err != nil {
		t.Fatal(err)
	}

	var leasedVersion domain.Version
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		inc, ok, e := u.Increment(ctx, p.IncrementID)
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("increment not found")
		}
		aid, _ := domain.NewActorID("a")
		leased, e := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementLease, Actor: aid, At: t0, ExpectedVersion: inc.Version})
		if e != nil {
			return e
		}
		if e := u.SaveIncrement(ctx, leased, inc.Version); e != nil {
			return e
		}
		leasedVersion = leased.Version
		ghostLease := domain.Lease{ID: ghostLeaseID, IncrementID: iid, ExecutionID: ghostExecID, RunnerID: ghostRunnerID, FencingToken: 1, IssuedAt: t0, ExpiresAt: t0.Add(30 * time.Second), Status: domain.LeaseActive, Version: 1}
		return u.SaveLease(ctx, ghostLease, 0)
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok := st.Execution(ghostExecID.String()); ok {
		t.Fatal("test setup bug: the ghost execution unexpectedly exists")
	}

	cclock.now = t0.Add(2 * time.Minute) // past both the ghost lease's own expiry and LeaseTTL
	reclaim, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "reclaim", IncrementID: p.IncrementID, ExpectedIncrementVersion: leasedVersion})
	if err != nil {
		t.Fatalf("claim should succeed when the superseded execution is absent (skip branch), got %v", err)
	}
	if reclaim.ExecutionID == "" || reclaim.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}
}

// TestClaimReclaimSkipsWhenSupersededExecutionAlreadyTerminal is A3's
// second skip branch: the superseded Execution already reached a terminal
// outcome (ExecutionSucceeded here, via the ordinary happy path) before its
// Lease's TTL elapsed. Claim still succeeds and leaves the terminal
// Execution untouched. It also proves the negative direction A3 requires:
// an unconditional domain.MarkExecutionLost call on this exact
// already-terminal Execution/Lease pair -- what Claim would do without the
// guard -- fails with domain.ErrInvalidTransition.
func TestClaimReclaimSkipsWhenSupersededExecutionAlreadyTerminal(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	start1, err := s.Start(runner(ctx, "r1"), application.StartRequest{RequestID: "start-1", ExecutionID: attempt1.ExecutionID, ExpectedExecutionVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Checkpoint(runner(ctx, "r1"), application.CheckpointRequest{RequestID: "checkpoint-1", ExecutionID: attempt1.ExecutionID, LeaseID: attempt1.LeaseID, FencingToken: attempt1.FencingToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Permit(runner(ctx, "r1"), application.PermitRequest{RequestID: "permit-1", Kind: domain.PermitExternalEffect, FencingToken: attempt1.FencingToken, ExpectedFencingToken: attempt1.FencingToken, Resource: attempt1.ExecutionID}); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.AcceptResult(runner(ctx, "r1"), application.AcceptResultRequest{RequestID: "accept-1", ExecutionID: attempt1.ExecutionID, LeaseID: attempt1.LeaseID, ExpectedExecutionVersion: start1.Version, FencingToken: attempt1.FencingToken, Succeeded: true})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != domain.ExecutionSucceeded {
		t.Fatalf("expected terminal success, got %s", accepted.Status)
	}

	terminalBefore, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("terminal execution missing")
	}
	leaseAfterAccept, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("lease missing")
	}

	// Negative direction (dp-v2-058 d5, A3): without the guard, Claim's
	// unconditional MarkExecutionLost call on this already-terminal
	// Execution would fail with exactly this error.
	if _, mlErr := domain.MarkExecutionLost(terminalBefore, leaseAfterAccept); !errors.Is(mlErr, domain.ErrInvalidTransition) {
		t.Fatalf("expected an unconditional MarkExecutionLost on an already-terminal execution to fail with ErrInvalidTransition, got %v", mlErr)
	}

	// With the guard restored (the actual production code), Claim still
	// succeeds and the terminal Execution is left untouched.
	cclock.now = t0.Add(2 * time.Minute)
	reclaim, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatalf("claim should succeed when the superseded execution is already terminal (skip branch), got %v", err)
	}
	if reclaim.ExecutionID == "" || reclaim.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}

	terminalAfter, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("terminal execution disappeared")
	}
	if terminalAfter != terminalBefore {
		t.Fatalf("already-terminal execution changed during reclaim: before=%#v after=%#v", terminalBefore, terminalAfter)
	}
}

// TestClaimReclaimSkipsWhenSupersededExecutionLinkageMismatches is A3's
// third skip branch: the superseded Execution's linkage to its own Lease is
// corrupted (its FencingToken no longer matches, manufactured directly
// against the store -- Service.Claim itself never produces this). Claim
// still succeeds and leaves the mismatched Execution untouched. It also
// proves the negative direction: an unconditional domain.MarkExecutionLost
// call on this exact mismatched pair fails with domain.ErrStaleFence.
func TestClaimReclaimSkipsWhenSupersededExecutionLinkageMismatches(t *testing.T) {
	t0 := clock{}.Now()
	st := memory.New()
	cclock := &clock{now: t0}
	s, err := application.NewServiceWithConfig(st, cclock, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	c, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan", RequirementID: c.RequirementID, ExpectedRequirementVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, application.PrepareRequest{RequestID: "prep", IncrementID: p.IncrementID, ExpectedVersion: p.Version}); err != nil {
		t.Fatal(err)
	}

	attempt1, err := s.Claim(runner(ctx, "r1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: p.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}

	var mismatched domain.Execution
	if err := st.Transact(ctx, func(u application.UnitOfWork) error {
		exec, ok, e := u.Execution(ctx, attempt1.ExecutionID)
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("execution not found")
		}
		exec.FencingToken = exec.FencingToken + 100
		if e := u.SaveExecution(ctx, exec, exec.Version); e != nil {
			return e
		}
		mismatched = exec
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	leaseBefore, ok := st.Lease(attempt1.LeaseID)
	if !ok {
		t.Fatal("lease missing")
	}

	// Negative direction: an unconditional MarkExecutionLost call on this
	// mismatched pair fails.
	if _, mlErr := domain.MarkExecutionLost(mismatched, leaseBefore); !errors.Is(mlErr, domain.ErrStaleFence) {
		t.Fatalf("expected an unconditional MarkExecutionLost with mismatched linkage to fail with ErrStaleFence, got %v", mlErr)
	}

	cclock.now = t0.Add(2 * time.Minute)
	reclaim, err := s.Claim(runner(ctx, "r2"), application.ClaimRequest{RequestID: "claim-2", IncrementID: p.IncrementID, ExpectedIncrementVersion: attempt1.Version})
	if err != nil {
		t.Fatalf("claim should succeed when the superseded execution's linkage mismatches (skip branch), got %v", err)
	}
	if reclaim.ExecutionID == "" || reclaim.LeaseID == "" {
		t.Fatal("expected a usable new lease/execution")
	}

	after, ok := st.Execution(attempt1.ExecutionID)
	if !ok {
		t.Fatal("mismatched execution disappeared")
	}
	if after != mismatched {
		t.Fatalf("mismatched execution changed during reclaim: before=%#v after=%#v", mismatched, after)
	}
}
