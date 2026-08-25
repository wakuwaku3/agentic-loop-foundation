package firestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	cloudfirestore "cloud.google.com/go/firestore"
	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

type integrationIDs struct {
	mu sync.Mutex
	n  int
}

func (i *integrationIDs) Next(kind string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	return fmt.Sprintf("%s-%d", kind, i.n), nil
}

type integrationClock struct{ now time.Time }

func (c integrationClock) Now() time.Time { return c.now }
func integrationOwner(ctx context.Context) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "integration-owner"})
}
func integrationRunner(ctx context.Context, id string) context.Context {
	return application.ContextWithCaller(ctx, application.Caller{Role: application.RoleRunner, Subject: "integration-runner", RunnerID: id})
}

func emulatorStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is required for Firestore integration tests")
	}
	ctx := context.Background()
	c, err := cloudfirestore.NewClient(ctx, "agentic-loop-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := NewStore(c, "production-must-reject-emulator"); err == nil {
		t.Fatal("production constructor accepted emulator host")
	}
	s, err := NewEmulatorStore(c, "store-test-"+time.Now().Format("150405.000000000"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCodecRejectsCorruption(t *testing.T) {
	id, _ := domain.NewRequirementID("r")
	b, err := EncodeRecord("requirement", domain.Requirement{ID: id, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got domain.Requirement
	if err := DecodeRecord(b, "requirement", &got); err != nil || got.ID != id {
		t.Fatalf("decode: %v %#v", err, got)
	}
	for _, bad := range [][]byte{[]byte("{"), []byte(`{"record_schema":"old","value":{}}`), []byte(`{"record_schema":"v1","value":"bad"}`)} {
		if err := DecodeRecord(bad, "requirement", &got); !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("bad record accepted: %v", err)
		}
	}
	if err := DecodeRecord(b, "increment", &got); !errors.Is(err, ErrInvalidSchema) {
		t.Fatal("kind mismatch accepted")
	}
	if _, err := PathKey(string(make([]byte, MaxPathKeyBytes+1))); err == nil {
		t.Fatal("oversized path key accepted")
	}
}

func TestFirestoreRollbackAndAtomicRecords(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	id, _ := domain.NewRequirementID("rollback")
	err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0); err != nil {
			return err
		}
		if err := u.Record(application.Event{ID: "rollback-event", AggregateID: id.String()}, &application.OutboxItem{ID: "rollback-outbox"}); err != nil {
			return err
		}
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("rollback returned nil")
	}
	err = s.Transact(ctx, func(u application.UnitOfWork) error {
		_, ok, err := u.Requirement(ctx, id.String())
		if err != nil {
			return err
		}
		if ok {
			return errors.New("rolled back record visible")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events leaked: %d", len(events))
	}
	outbox, err := s.Outbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 0 {
		t.Fatalf("outbox leaked: %d", len(outbox))
	}

	requestID := "atomic-request"
	err = s.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0); err != nil {
			return err
		}
		if err := u.Record(application.Event{ID: "atomic-event", RequestID: requestID, AggregateID: id.String()}, &application.OutboxItem{ID: "atomic-outbox", RequestID: requestID}); err != nil {
			return err
		}
		return u.SaveIdempotency(ctx, application.IdempotentResponse{RequestID: requestID, Operation: "capture", Fingerprint: "fp", ResponseJSON: []byte(`{}`)})
	})
	if err != nil {
		t.Fatal(err)
	}
	var again application.IdempotentResponse
	err = s.Transact(ctx, func(u application.UnitOfWork) error {
		var ok bool
		var e error
		again, ok, e = u.Idempotency(ctx, requestID, "capture")
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("idempotency record missing")
		}
		return nil
	})
	if err != nil || again.Fingerprint != "fp" {
		t.Fatalf("idempotency: %v %#v", err, again)
	}
	events, err = s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("event count=%d", len(events))
	}
	outbox, err = s.Outbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 {
		t.Fatalf("outbox count=%d", len(outbox))
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		return u.Record(application.Event{ID: "atomic-event", RequestID: requestID}, &application.OutboxItem{ID: "atomic-outbox", RequestID: requestID})
	}); err == nil {
		t.Fatal("event/outbox ID collision was silently overwritten")
	}
}

func TestFirestoreApplicationAtomicityConflictAndControl(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	clock := integrationClock{now: time.Unix(1700000000, 0).UTC()}
	svc, err := application.NewServiceWithConfig(s, clock, &integrationIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := integrationOwner(ctx)
	captured, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: "capture-1", Text: "atomic"})
	if err != nil {
		t.Fatal(err)
	}
	retried, err := svc.Capture(ownerCtx, application.CaptureRequest{RequestID: "capture-1", Text: "atomic"})
	if err != nil || retried != captured {
		t.Fatalf("idempotent capture: %v %#v %#v", err, captured, retried)
	}
	view, found, err := svc.GetRequirement(ownerCtx, captured.RequirementID)
	if err != nil || !found || view.Text != "atomic" {
		t.Fatalf("co-located requirement text: %v %v %#v", err, found, view)
	}
	events, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("capture events=%d", len(events))
	}
	control, err := svc.Control(ownerCtx, application.ControlRequest{RequestID: "control-1", Scope: domain.ControlScope{Kind: domain.ScopeRequirement, Value: "blocked-requirement"}, Mode: domain.ControlPauseIntake, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := s.Outbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 {
		t.Fatalf("control outbox=%d", len(outbox))
	}
	permit, err := svc.Permit(ownerCtx, application.PermitRequest{RequestID: "permit-1", Kind: domain.PermitIntake, Target: domain.ControlTarget{InstallationID: "install", RequirementID: "blocked-requirement"}, ControlRevision: control.Revision, Resource: "capture"})
	if !errors.Is(err, domain.ErrControlDenied) || permit.Allowed {
		t.Fatalf("pause control permit: %#v %v", permit, err)
	}

	id, _ := domain.NewRequirementID("conflict")
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.Transact(ctx, func(u application.UnitOfWork) error {
				v, ok, err := u.Requirement(ctx, id.String())
				if err != nil || !ok {
					return err
				}
				v.Version++
				return u.SaveRequirement(ctx, v, 1)
			})
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	conflicts := 0
	for e := range results {
		if e == nil {
			successes++
		} else if errors.Is(e, domain.ErrStaleVersion) {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("optimistic conflict successes=%d conflicts=%d", successes, conflicts)
	}

	prepared, err := svc.Plan(ownerCtx, application.PlanRequest{RequestID: "plan-1", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		inc, ok, err := u.Increment(ctx, prepared.IncrementID)
		if err != nil || !ok {
			return err
		}
		actor, _ := domain.NewActorID("planner")
		next, err := domain.DecideIncrement(inc, domain.IncrementCommand{Kind: domain.IncrementPrepare, Actor: actor, At: clock.now, ExpectedVersion: inc.Version})
		if err != nil {
			return err
		}
		return u.SaveIncrement(ctx, next, inc.Version)
	}); err != nil {
		t.Fatal(err)
	}
	claims := make(chan error, 2)
	for n := 0; n < 2; n++ {
		go func(n int) {
			_, e := svc.Claim(integrationRunner(ctx, fmt.Sprintf("runner-%d", n)), application.ClaimRequest{RequestID: fmt.Sprintf("claim-%d", n), IncrementID: prepared.IncrementID, ExpectedIncrementVersion: 2})
			claims <- e
		}(n)
	}
	successes = 0
	var claimErrors []error
	for n := 0; n < 2; n++ {
		if e := <-claims; e == nil {
			successes++
		} else {
			claimErrors = append(claimErrors, e)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent claims=%d errors=%v", successes, claimErrors)
	}
}

func TestFirestoreExpiredLeaseAndExecutionIndexes(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	now := time.Unix(1700000200, 0).UTC()
	for _, suffix := range []string{"a", "b"} {
		lease := domain.Lease{ID: domain.LeaseID("lease-" + suffix), ExecutionID: domain.ExecutionID("exec-" + suffix), IncrementID: domain.IncrementID("inc-" + suffix), RunnerID: domain.RunnerID("runner"), FencingToken: 1, Status: domain.LeaseActive, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(-time.Second), Version: 1}
		execution := domain.Execution{ID: lease.ExecutionID, IncrementID: lease.IncrementID, RunnerID: lease.RunnerID, LeaseID: lease.ID, FencingToken: lease.FencingToken, Status: domain.ExecutionRunning, Version: 1}
		if err := s.Transact(ctx, func(u application.UnitOfWork) error {
			if err := u.SaveLease(ctx, lease, 0); err != nil {
				return err
			}
			return u.SaveExecution(ctx, execution, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var first, second []domain.Lease
	var cursor string
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		var err error
		first, cursor, err = u.ExpiredActiveLeases(ctx, now, "", 1)
		return err
	}); err != nil || len(first) != 1 || cursor == "" {
		t.Fatalf("first page=%#v cursor=%q err=%v", first, cursor, err)
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		var err error
		second, _, err = u.ExpiredActiveLeases(ctx, now, cursor, 1)
		return err
	}); err != nil || len(second) != 1 || second[0].ID == first[0].ID {
		t.Fatalf("second page=%#v err=%v", second, err)
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		execution, ok, err := u.ExecutionByLease(ctx, first[0].ID.String())
		if err != nil || !ok || execution.LeaseID != first[0].ID {
			t.Fatalf("execution lookup=%#v ok=%v err=%v", execution, ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --- V2-064: Repository aggregate on a real Firestore transaction. ---

func repositoryFixture(t *testing.T, id, owner, name string, status domain.RepositoryStatus, version domain.Version) domain.Repository {
	t.Helper()
	locator, err := domain.NormalizeSourceLocator(domain.SourceLocator{Owner: owner, Name: name, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return domain.Repository{ID: domain.RepositoryID(id), Locator: locator, Status: status, Version: version}
}

// TestRepositoryOptimisticConcurrencyOnFirestore mirrors SaveRequirement's
// contract on the real adapter: a create must declare expected version 0, a
// conflicting concurrent save is refused, and the normalised locator key of
// every stored row is visible so the application can enforce the duplicate
// constraint.
func TestRepositoryOptimisticConcurrencyOnFirestore(t *testing.T) {
	store := emulatorStore(t)
	ctx := context.Background()

	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repositoryFixture(t, "repository-1", "o", "n", domain.RepositoryRegistered, 1), 3)
	}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("create at a non-zero expected version = %v, want ErrStaleVersion", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repositoryFixture(t, "repository-1", "o", "n", domain.RepositoryRegistered, 1), 0)
	}); err != nil {
		t.Fatalf("create at expected version 0: %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repositoryFixture(t, "repository-1", "o", "n", domain.RepositoryRetired, 2), 0)
	}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("conflicting concurrent save = %v, want ErrStaleVersion", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repositoryFixture(t, "repository-2", "o", "other", domain.RepositoryRegistered, 1), 0)
	}); err != nil {
		t.Fatalf("second create: %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		stored, ok, e := u.Repository(ctx, "repository-1")
		if e != nil || !ok {
			t.Fatalf("stored repository missing: %v", e)
		}
		if stored.Status != domain.RepositoryRegistered || stored.Version != 1 || stored.Locator.Key() != "github/o/n" {
			t.Fatalf("stored = %+v", stored)
		}
		rows, e := u.Repositories(ctx)
		if e != nil {
			return e
		}
		if len(rows) != 2 || rows[0].ID != "repository-1" || rows[1].ID != "repository-2" {
			t.Fatalf("rows = %+v", rows)
		}
		keys := map[string]int{}
		for _, row := range rows {
			keys[row.Locator.Key()]++
		}
		if keys["github/o/n"] != 1 || keys["github/o/other"] != 1 {
			t.Fatalf("normalised keys = %v", keys)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRepositoryRegistrationThroughTheServiceOnFirestore drives the same
// duplicate-locator refusal and the same rollback the memory adapter proves,
// but through the real Firestore transaction, so the constraint is not an
// artefact of the in-memory map.
func TestRepositoryRegistrationThroughTheServiceOnFirestore(t *testing.T) {
	store := emulatorStore(t)
	svc, err := application.NewServiceWithConfig(store, integrationClock{now: time.Unix(1700000000, 0).UTC()}, &integrationIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := integrationOwner(context.Background())

	first, err := svc.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: "req-1", SourceURL: "https://github.com/O/N", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if first.Status != domain.RepositoryRegistered || first.Version != 1 || first.Locator.Key() != "github/o/n" {
		t.Fatalf("register = %+v", first)
	}
	if _, err = svc.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: "req-2", SourceURL: "git@github.com:O/N.git", DefaultBranch: "main"}); !errors.Is(err, application.ErrRepositoryAlreadyRegistered) {
		t.Fatalf("duplicate locator on firestore = %v, want ErrRepositoryAlreadyRegistered", err)
	}
	second, err := svc.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: "req-3", SourceURL: "https://github.com/O/second", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("second register: %v", err)
	}

	observed, err := svc.ObserveRepository(integrationRunner(context.Background(), "runner-1"), application.ObserveRepositoryRequest{RequestID: "obs-1", RepositoryID: first.RepositoryID, Reachable: true, CanPush: true, DefaultBranch: "main", AdapterVersion: "integration"})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if !observed.Executability.Executable {
		t.Fatalf("observe = %+v", observed)
	}

	retired, err := svc.RetireRepository(ctx, application.RetireRepositoryRequest{RequestID: "retire-1", RepositoryID: first.RepositoryID, ExpectedVersion: first.Version})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if retired.Status != domain.RepositoryRetired {
		t.Fatalf("retire = %+v", retired)
	}
	// The other Repository is untouched, version included.
	if err = store.Transact(context.Background(), func(u application.UnitOfWork) error {
		other, ok, e := u.Repository(context.Background(), second.RepositoryID)
		if e != nil || !ok {
			t.Fatalf("second repository missing: %v", e)
		}
		if other.Version != second.Version || other.Status != domain.RepositoryRegistered || other.Locator != second.Locator {
			t.Fatalf("retiring one repository altered another: %+v vs %+v", other, second)
		}
		obs, ok, e := u.RepositoryObservation(context.Background(), first.RepositoryID)
		if e != nil || !ok {
			t.Fatalf("observation missing: %v", e)
		}
		if !obs.Reachable || obs.DefaultBranch != "main" || obs.ObservedAt.IsZero() {
			t.Fatalf("observation = %+v", obs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
