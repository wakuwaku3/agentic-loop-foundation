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

// TestRequirementRepositoryLinkIsWriteOnceAndBounded is the Firestore half of
// V2-071 A3/A5/A13, carrying the same name and the same assertions as
// internal/store/memory/store_test.go's test, so both implementations satisfy
// one contract. The write and the query are in separate transactions because a
// Firestore transaction's staged writes are not visible to a query inside the
// same transaction (only a keyed read sees them), which is the same reason
// ExecutionByLease is never called after SaveExecution in one callback.
//
// The per-repository read is a single-field equality query on
// index_repository_id. Firestore indexes every single field automatically, so
// firestore.indexes.json is unchanged: it declares only the composite indexes
// the outbox and lease queries need.
func TestRequirementRepositoryLinkIsWriteOnceAndBounded(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()
	link := func(requirementID, repositoryID string) domain.RequirementRepositoryLink {
		return domain.RequirementRepositoryLink{
			RequirementID: domain.RequirementID(requirementID),
			RepositoryID:  domain.RepositoryID(repositoryID),
			AssignedAt:    at,
			RequestedBy:   domain.RequestedBy{ActorType: domain.ActorTypeOwner, Subject: "owner-1"},
		}
	}

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(ctx, domain.RequirementRepositoryLink{RequirementID: "req-1", RepositoryID: "repo-1"})
	}); err == nil {
		t.Fatal("a link with no assignment instant was accepted")
	}

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRequirementRepositoryLink(ctx, link("req-1", "repo-1")); e != nil {
			return e
		}
		if e := u.SaveRequirementRepositoryLink(ctx, link("req-2", "repo-1")); e != nil {
			return e
		}
		return u.SaveRequirementRepositoryLink(ctx, link("req-3", "repo-2"))
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(ctx, link("req-1", "repo-1"))
	}); err != nil {
		t.Fatalf("identical re-write must be an idempotent replay: %v", err)
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(ctx, link("req-1", "repo-9"))
	}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a second, differing link = %v, want ErrStaleVersion", err)
	}

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		stored, ok, e := u.RequirementRepositoryLink(ctx, "req-1")
		if e != nil {
			return e
		}
		if !ok || stored.RepositoryID != "repo-1" || !stored.AssignedAt.Equal(at) {
			t.Fatalf("stored link = %+v ok=%v", stored, ok)
		}
		if _, ok, e = u.RequirementRepositoryLink(ctx, "req-absent"); e != nil {
			return e
		} else if ok {
			t.Fatal("an unlinked Requirement reported a link")
		}
		batch, e := u.RequirementRepositoryLinks(ctx, []string{"req-1", "req-3", "req-absent"})
		if e != nil {
			return e
		}
		if len(batch) != 2 || batch["req-1"].RepositoryID != "repo-1" || batch["req-3"].RepositoryID != "repo-2" {
			t.Fatalf("batch = %+v", batch)
		}
		if _, present := batch["req-absent"]; present {
			t.Fatal("the batch read invented an entry for an unlinked Requirement")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		ids, truncated, e := u.RequirementIDsForRepository(ctx, "repo-1", 10)
		if e != nil {
			return e
		}
		if truncated || len(ids) != 2 || ids[0] != "req-1" || ids[1] != "req-2" {
			t.Fatalf("per-repository read = %v truncated=%v", ids, truncated)
		}
		ids, truncated, e = u.RequirementIDsForRepository(ctx, "repo-1", 1)
		if e != nil {
			return e
		}
		if !truncated || len(ids) != 1 {
			t.Fatalf("bounded per-repository read = %v truncated=%v; the bound must be reported", ids, truncated)
		}
		ids, truncated, e = u.RequirementIDsForRepository(ctx, "repo-none", 10)
		if e != nil {
			return e
		}
		if truncated || len(ids) != 0 {
			t.Fatalf("a repository with no linked Requirement = %v truncated=%v", ids, truncated)
		}
		if _, _, e = u.RequirementIDsForRepository(ctx, "", 10); e == nil {
			t.Fatal("an empty repository id was accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	failure := errors.New("rollback")
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRequirementRepositoryLink(ctx, link("req-rolled-back", "repo-1")); e != nil {
			return e
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("rollback = %v", err)
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if _, ok, e := u.RequirementRepositoryLink(ctx, "req-rolled-back"); e != nil {
			return e
		} else if ok {
			t.Fatal("a rolled-back transaction committed a link")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ===========================================================================
// Runner version report (V2-069), Firestore adapter
// ===========================================================================

// TestRunnerVersionReportBehaviouralTableOnFirestore runs
// application.RunnerVersionReportCases() -- the very same table
// internal/store/memory runs -- against the real Firestore transaction, so
// the memory adapter cannot pass behaviour this adapter does not implement.
// Every read is a separate transaction from every write.
func TestRunnerVersionReportBehaviouralTableOnFirestore(t *testing.T) {
	cases := application.RunnerVersionReportCases()
	if len(cases) == 0 {
		t.Fatal("the shared table is empty; the assertion would be vacuous")
	}
	ctx := context.Background()
	for _, c := range cases {
		store := emulatorStore(t)
		for _, id := range c.Observations {
			observation := domain.RunnerObservation{RunnerID: domain.RunnerID(id), Reachable: true, ObservedAt: time.Unix(1700000000, 0).UTC()}
			if err := store.Transact(ctx, func(u application.UnitOfWork) error {
				return u.SaveRunnerObservation(ctx, observation)
			}); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
		}
		// The bounded case writes more rows than one transaction's write cap
		// would comfortably hold in a single batch, so each report is written
		// in its own transaction -- which is also what a real Runner does.
		for _, report := range c.Reports {
			value := report
			if err := store.Transact(ctx, func(u application.UnitOfWork) error {
				return u.SaveRunnerVersionReport(ctx, value)
			}); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
		}
		limit := c.Limit
		if limit == 0 {
			limit = application.MaxRunnerVersionReports
		}
		var got []application.RunnerVersionReport
		var truncated bool
		if err := store.Transact(ctx, func(u application.UnitOfWork) error {
			v, more, e := u.RunnerVersionReports(ctx, limit)
			got, truncated = v, more
			return e
		}); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if truncated != c.WantTruncated {
			t.Fatalf("%s: truncated=%v want %v", c.Name, truncated, c.WantTruncated)
		}
		if len(got) != len(c.Want) {
			t.Fatalf("%s: enumerated %d rows, want %d", c.Name, len(got), len(c.Want))
		}
		for i := range c.Want {
			if !got[i].ReportedAt.Equal(c.Want[i].ReportedAt) {
				t.Fatalf("%s: row %d reported_at = %s want %s", c.Name, i, got[i].ReportedAt, c.Want[i].ReportedAt)
			}
			a, b := got[i], c.Want[i]
			a.ReportedAt, b.ReportedAt = time.Time{}, time.Time{}
			if a != b {
				t.Fatalf("%s: row %d = %#v want %#v", c.Name, i, got[i], c.Want[i])
			}
		}
		for _, want := range c.Want {
			if !want.Reported() {
				continue
			}
			if err := store.Transact(ctx, func(u application.UnitOfWork) error {
				v, ok, e := u.RunnerVersionReport(ctx, want.RunnerID)
				if e != nil {
					return e
				}
				if !ok || v.Version != want.Version || v.BinarySHA256 != want.BinarySHA256 || v.SchemaMin != want.SchemaMin || v.SchemaMax != want.SchemaMax || !v.ReportedAt.Equal(want.ReportedAt) {
					t.Fatalf("%s: keyed read of %q = %#v ok=%v, want %#v", c.Name, want.RunnerID, v, ok, want)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
		}
	}
	t.Logf("firestore adapter satisfied %d shared cases", len(cases))
}

// TestRunnerVersionReportRollbackLeaksNothingOnFirestore is the other half of
// A9: a rolled-back transaction commits no report, in this adapter too.
func TestRunnerVersionReportRollbackLeaksNothingOnFirestore(t *testing.T) {
	store := emulatorStore(t)
	ctx := context.Background()
	failure := errors.New("rollback")
	report := application.RunnerVersionReport{RunnerID: "runner-x", Version: "1.0.0", BinarySHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", SchemaMin: 1, SchemaMax: 2, ReportedAt: time.Unix(1700000000, 0).UTC()}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRunnerVersionReport(ctx, report); e != nil {
			return e
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("rollback = %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if _, ok, e := u.RunnerVersionReport(ctx, "runner-x"); e != nil {
			return e
		} else if ok {
			t.Fatal("a rolled-back Firestore transaction committed a Runner version report")
		}
		rows, _, e := u.RunnerVersionReports(ctx, application.MaxRunnerVersionReports)
		if e != nil {
			return e
		}
		if len(rows) != 0 {
			t.Fatalf("a rolled-back transaction left %d enumerated rows", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRunnerVersionReport(ctx, application.RunnerVersionReport{Version: "1.0.0"})
	}); err == nil {
		t.Fatal("a report with no runner id was accepted")
	}
}

// TestRunnersReadCountDoesNotVaryWithRequirementCount is A8's measured half.
// The trued-up quota total is the adapter's own record of the documents a
// transaction actually read (trueUpQuota uses len(u.cache)), so the delta
// across one GET /v1/runners is the measured read count. It is measured twice,
// with two different Requirement counts in the store, and must be the same
// both times: the enumeration reads two per-machine collections and the quota
// document, and nothing that grows with the Requirement count.
func TestRunnersReadCountDoesNotVaryWithRequirementCount(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	measure := func(requirements int) (reads, writes int64, count int) {
		store := emulatorStore(t)
		svc, err := application.NewServiceWithConfig(store, integrationClock{now: at}, &integrationIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		// A fixture exactly at the enumeration bound: every machine known and
		// every machine reporting.
		for i := 0; i < application.MaxRunnerVersionReports; i++ {
			id := fmt.Sprintf("runner-%03d", i)
			if _, err := svc.Heartbeat(integrationRunner(ctx, id), application.HeartbeatRequest{
				RequestID:     "hb-" + id,
				RunnerVersion: &application.RunnerVersionInput{Version: "1.2.3", BinarySHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", SchemaMin: 2, SchemaMax: 7},
			}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < requirements; i++ {
			if _, err := svc.Capture(integrationOwner(ctx), application.CaptureRequest{RequestID: fmt.Sprintf("cap-%d", i), Text: "work"}); err != nil {
				t.Fatal(err)
			}
		}
		before, err := readQuotaRecord(ctx, store, at)
		if err != nil {
			t.Fatal(err)
		}
		view, err := svc.Runners(integrationOwner(ctx))
		if err != nil {
			t.Fatal(err)
		}
		after, err := readQuotaRecord(ctx, store, at)
		if err != nil {
			t.Fatal(err)
		}
		return after.Total.Reads - before.Total.Reads, after.Total.Writes - before.Total.Writes, view.RunnerCount
	}

	lowReads, lowWrites, lowCount := measure(3)
	highReads, highWrites, highCount := measure(11)
	if lowCount != application.MaxRunnerVersionReports || highCount != application.MaxRunnerVersionReports {
		t.Fatalf("the fixture is not at the bound: %d and %d rows", lowCount, highCount)
	}
	if lowReads != highReads {
		t.Fatalf("one GET /v1/runners read %d documents with 3 Requirements and %d with 11; the read count varies with the Requirement count", lowReads, highReads)
	}
	// The bound: two per-machine collections plus the quota document, and the
	// declared enumeration bound is the only thing that scales it.
	const bound = int64(2*application.MaxRunnerVersionReports + 1)
	if lowReads > bound {
		t.Fatalf("one GET /v1/runners read %d documents, above the declared bound %d", lowReads, bound)
	}
	// The only document written is the quota reservation every bounded owner
	// read already writes; no application record and no outbox item is
	// touched.
	if lowWrites != 1 || highWrites != 1 {
		t.Fatalf("one GET /v1/runners wrote %d and %d documents, want exactly the quota record", lowWrites, highWrites)
	}
	t.Logf("measured reads for one GET /v1/runners at the bound: %d with 3 Requirements, %d with 11 Requirements; declared bound %d; writes %d", lowReads, highReads, bound, lowWrites)
}

// ===========================================================================
// V2-067: the Provider registry on Firestore
// ===========================================================================

// TestProviderRegistryBehaviouralTableOnFirestore runs, by value, the same
// table internal/application runs against the memory adapter
// (TestProviderRegistryBehaviouralTableOnMemory). Sharing the table is what
// stops the memory store passing a retention, an ordering or a stickiness this
// adapter does not implement.
func TestProviderRegistryBehaviouralTableOnFirestore(t *testing.T) {
	cases := application.ProviderRegistryCases()
	if len(cases) == 0 {
		t.Fatal("the shared behavioural table is empty")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			store := emulatorStore(t)
			ctx := context.Background()
			for _, o := range c.Observations {
				value := o
				if err := store.Transact(ctx, func(u application.UnitOfWork) error {
					return u.SaveProviderObservation(ctx, value)
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, a := range c.Assignments {
				value := a
				if err := store.Transact(ctx, func(u application.UnitOfWork) error {
					return u.SaveProviderAssignment(ctx, value)
				}); err != nil {
					t.Fatal(err)
				}
			}
			var log application.ProviderObservationLog
			var assignments []application.ProviderAssignment
			if err := store.Transact(ctx, func(u application.UnitOfWork) error {
				var e error
				if log, e = u.ProviderObservations(ctx, c.Query); e != nil {
					return e
				}
				assignments, e = u.ProviderAssignments(ctx, c.Query)
				return e
			}); err != nil {
				t.Fatal(err)
			}
			if len(log.Observations) != len(c.WantObservations) {
				t.Fatalf("observations=%d want %d", len(log.Observations), len(c.WantObservations))
			}
			for i := range c.WantObservations {
				got, want := log.Observations[i], c.WantObservations[i]
				if got.Provider != want.Provider || got.FailureClass != want.FailureClass ||
					got.StoppedForInspection != want.StoppedForInspection || !got.ObservedAt.Equal(want.ObservedAt) {
					t.Fatalf("observation %d = %#v, want %#v", i, got, want)
				}
			}
			if got := !log.VerifiedAt.IsZero(); got != c.WantVerified {
				t.Fatalf("verified=%v want %v (VerifiedAt=%s)", got, c.WantVerified, log.VerifiedAt)
			}
			if len(assignments) != len(c.WantAssignments) {
				t.Fatalf("assignments=%d want %d: %#v", len(assignments), len(c.WantAssignments), assignments)
			}
			for i := range c.WantAssignments {
				got, want := assignments[i], c.WantAssignments[i]
				if got.ExecutionID != want.ExecutionID || got.IncrementID != want.IncrementID ||
					got.Provider != want.Provider || !got.Since.Equal(want.Since) {
					t.Fatalf("assignment %d = %#v, want %#v", i, got, want)
				}
			}
			// The side table really is keyed by Execution id on this adapter
			// too, not only in the per-Provider index.
			for _, a := range c.WantAssignments {
				value := a
				if err := store.Transact(ctx, func(u application.UnitOfWork) error {
					got, ok, e := u.ProviderAssignment(ctx, value.ExecutionID)
					if e != nil {
						return e
					}
					if !ok {
						return fmt.Errorf("no side-table record for execution %s", value.ExecutionID)
					}
					if got.Provider != value.Provider {
						return fmt.Errorf("side-table record for %s names provider %q, want %q", value.ExecutionID, got.Provider, value.Provider)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestProviderRegistryRollbackLeaksNothingOnFirestore asserts a failed
// transaction stages nothing: the observation ring and both halves of the
// assignment record are absent afterwards.
func TestProviderRegistryRollbackLeaksNothingOnFirestore(t *testing.T) {
	store := emulatorStore(t)
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()
	abort := errors.New("abort")
	err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveProviderObservation(ctx, application.ProviderObservation{Provider: application.ProviderClaude, ObservedAt: at}); e != nil {
			return e
		}
		if e := u.SaveProviderAssignment(ctx, application.ProviderAssignment{ExecutionID: "execution-a", IncrementID: "increment-1", Provider: application.ProviderClaude, Since: at}); e != nil {
			return e
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("expected the rollback error, got %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		log, e := u.ProviderObservations(ctx, application.ProviderClaude)
		if e != nil {
			return e
		}
		if len(log.Observations) != 0 || !log.VerifiedAt.IsZero() {
			return fmt.Errorf("a rolled-back observation leaked: %#v", log)
		}
		assignments, e := u.ProviderAssignments(ctx, application.ProviderClaude)
		if e != nil {
			return e
		}
		if len(assignments) != 0 {
			return fmt.Errorf("a rolled-back assignment leaked: %#v", assignments)
		}
		_, ok, e := u.ProviderAssignment(ctx, "execution-a")
		if ok {
			return errors.New("a rolled-back side-table record leaked")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// An undeclared provider name is refused by the adapter as well as by the
	// service, so no path can create a fourth registry document.
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveProviderObservation(ctx, application.ProviderObservation{Provider: "gemini", ObservedAt: at})
	}); !errors.Is(err, application.ErrProviderUnknown) {
		t.Fatalf("the adapter accepted an undeclared provider observation: %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveProviderAssignment(ctx, application.ProviderAssignment{ExecutionID: "execution-z", Provider: "gemini", Since: at})
	}); !errors.Is(err, application.ErrProviderUnknown) {
		t.Fatalf("the adapter accepted an undeclared provider assignment: %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveProviderObservation(ctx, application.ProviderObservation{Provider: application.ProviderClaude})
	}); err == nil {
		t.Fatal("the adapter accepted an observation with no observed instant")
	}
}

// TestProvidersReadCountDoesNotVaryWithRequirementCount is V2-067 A12's
// measured half. The trued-up quota total is the adapter's own record of the
// documents a transaction actually read (trueUpQuota uses len(u.cache)), so the
// delta across one GET /v1/providers is the measured read count. It is measured
// twice, with two different Requirement counts, against a fixture at the
// maximum bounded assignments and observations, and must be the same both
// times.
func TestProvidersReadCountDoesNotVaryWithRequirementCount(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	measure := func(requirements int) (reads, writes int64, assignments int) {
		store := emulatorStore(t)
		svc, err := application.NewServiceWithConfig(store, integrationClock{now: at}, &integrationIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		// A fixture exactly at both bounds, for every declared Provider: the
		// full observation ring and the full assignment index, with every
		// assigned Execution present and non-terminal.
		for _, name := range application.DeclaredProviders() {
			provider := name
			if err := store.Transact(ctx, func(u application.UnitOfWork) error {
				for i := 0; i < application.MaxProviderObservations; i++ {
					if e := u.SaveProviderObservation(ctx, application.ProviderObservation{
						Provider: provider, ObservedAt: at.Add(time.Duration(i) * time.Minute),
					}); e != nil {
						return e
					}
				}
				for i := 0; i < application.MaxProviderAssignments; i++ {
					id := fmt.Sprintf("%s-execution-%03d", provider, i)
					if e := u.SaveProviderAssignment(ctx, application.ProviderAssignment{
						ExecutionID: id, IncrementID: "increment-1", Provider: provider, Since: at,
					}); e != nil {
						return e
					}
					eid, e := domain.NewExecutionID(id)
					if e != nil {
						return e
					}
					iid, e := domain.NewIncrementID("increment-1")
					if e != nil {
						return e
					}
					rid, e := domain.NewRunnerID("runner-1")
					if e != nil {
						return e
					}
					if e = u.SaveExecution(ctx, domain.Execution{ID: eid, IncrementID: iid, RunnerID: rid, Status: domain.ExecutionRunning, Version: 1}, 0); e != nil {
						return e
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		// Captures both vary the Requirement count and create the quota record
		// the measurement reads.
		for i := 0; i < requirements; i++ {
			if _, err := svc.Capture(integrationOwner(ctx), application.CaptureRequest{RequestID: fmt.Sprintf("cap-%d", i), Text: "work"}); err != nil {
				t.Fatal(err)
			}
		}
		before, err := readQuotaRecord(ctx, store, at)
		if err != nil {
			t.Fatal(err)
		}
		view, err := svc.Providers(integrationOwner(ctx))
		if err != nil {
			t.Fatal(err)
		}
		after, err := readQuotaRecord(ctx, store, at)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, e := range view.Providers {
			total += len(e.Assignments)
		}
		return after.Total.Reads - before.Total.Reads, after.Total.Writes - before.Total.Writes, total
	}

	lowReads, lowWrites, lowAssignments := measure(3)
	highReads, highWrites, highAssignments := measure(11)
	wantAssignments := 3 * application.MaxProviderAssignments
	if lowAssignments != wantAssignments || highAssignments != wantAssignments {
		t.Fatalf("the fixture is not at the assignment bound: %d and %d assignments, want %d", lowAssignments, highAssignments, wantAssignments)
	}
	if lowReads != highReads {
		t.Fatalf("one GET /v1/providers read %d documents with 3 Requirements and %d with 11; the read count varies with the Requirement count", lowReads, highReads)
	}
	// The declared bound: one keyed registry document per declared Provider,
	// one keyed Execution read per retained assignment, and the quota document.
	// Nothing in it is a function of the Requirement count.
	bound := int64(len(application.DeclaredProviders())*(1+application.MaxProviderAssignments) + 1)
	if lowReads > bound {
		t.Fatalf("one GET /v1/providers read %d documents, above the declared bound %d", lowReads, bound)
	}
	// The only document written is the quota reservation every bounded owner
	// read already writes: no application record and no outbox item is touched.
	if lowWrites != 1 || highWrites != 1 {
		t.Fatalf("one GET /v1/providers wrote %d and %d documents, want exactly the quota record", lowWrites, highWrites)
	}
	t.Logf("measured reads for one GET /v1/providers at both bounds: %d with 3 Requirements, %d with 11 Requirements; declared bound %d; writes %d", lowReads, highReads, bound, lowWrites)
}
