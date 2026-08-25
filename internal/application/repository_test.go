package application_test

// Application-level closure of the Repository aggregate (V2-064). Every test
// here is deterministic: the clock is injected, no sleep is taken, no timer
// fires and no goroutine is started.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/store/memory"
)

func registerRepository(t *testing.T, s *application.Service, ctx context.Context, requestID, url string) application.RepositoryResponse {
	t.Helper()
	out, err := s.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: requestID, SourceURL: url, DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("RegisterRepository(%q): %v", url, err)
	}
	return out
}

func TestRegisterRepositoryRequiresAuthenticatedOwnerAndRequestID(t *testing.T) {
	s, _ := service()
	if _, err := s.RegisterRepository(context.Background(), application.RegisterRepositoryRequest{RequestID: "r", SourceURL: "o/n"}); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("unauthenticated accepted: %v", err)
	}
	if _, err := s.RegisterRepository(runner(context.Background(), "runner-1"), application.RegisterRepositoryRequest{RequestID: "r", SourceURL: "o/n"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner role accepted for register: %v", err)
	}
	if _, err := s.RegisterRepository(owner(context.Background()), application.RegisterRepositoryRequest{SourceURL: "o/n"}); err == nil {
		t.Fatal("missing request id accepted")
	}
	if _, err := s.RegisterRepository(owner(context.Background()), application.RegisterRepositoryRequest{RequestID: "r", SourceURL: "not a url"}); !errors.Is(err, domain.ErrInvalidSourceLocator) {
		t.Fatal("invalid locator accepted")
	}
	if _, err := s.RetireRepository(runner(context.Background(), "runner-1"), application.RetireRepositoryRequest{RequestID: "r", RepositoryID: "x"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner role accepted for retire: %v", err)
	}
	if _, err := s.ObserveRepository(owner(context.Background()), application.ObserveRepositoryRequest{RequestID: "r", RepositoryID: "x"}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("owner role accepted for observe: %v", err)
	}
	if _, err := s.ListRepositories(runner(context.Background(), "runner-1")); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner role accepted for list: %v", err)
	}
	if _, _, err := s.GetRepository(runner(context.Background(), "runner-1"), "x"); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("runner role accepted for detail: %v", err)
	}
}

func TestRegisterRepositoryNormalisesAndAttributes(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	out := registerRepository(t, s, ctx, "req-1", "git@github.com:Wakuwaku3/Agentic-Loop-Foundation.git")
	if out.Locator != (domain.SourceLocator{Forge: domain.ForgeGitHub, Host: "github.com", Owner: "wakuwaku3", Name: "agentic-loop-foundation", DefaultBranch: "main"}) {
		t.Fatalf("locator = %+v", out.Locator)
	}
	if out.Status != domain.RepositoryRegistered || out.Version != 1 || out.RepositoryID == "" {
		t.Fatalf("response = %+v", out)
	}
	if out.RequestedBy.ActorType != domain.ActorTypeOwner || out.RequestedBy.Subject != "actor-1" {
		t.Fatalf("requested_by = %+v", out.RequestedBy)
	}
}

// TestRegisterRepositoryRefusesDuplicateNormalisedLocator is acceptance A4's
// duplicate constraint: every spelling of the same repository collides, and
// the refusal creates no second Repository.
func TestRegisterRepositoryRefusesDuplicateNormalisedLocator(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	registerRepository(t, s, ctx, "req-1", "https://github.com/O/N")
	for i, spelling := range []string{
		"https://github.com/O/N",
		"https://github.com/O/N.git",
		"git@github.com:O/N.git",
		"https://GitHub.com/o/n",
		"O/N",
	} {
		requestID := "dup-" + string(rune('a'+i))
		_, err := s.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: requestID, SourceURL: spelling, DefaultBranch: "main"})
		if !errors.Is(err, application.ErrRepositoryAlreadyRegistered) {
			t.Fatalf("duplicate %q error = %v, want ErrRepositoryAlreadyRegistered", spelling, err)
		}
	}
	list, err := s.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Repositories) != 1 {
		t.Fatalf("duplicate refusals created %d repositories, want 1", len(list.Repositories))
	}
	// A different repository on the same forge is not a duplicate.
	registerRepository(t, s, ctx, "req-2", "https://github.com/O/other")
	if list, err = s.ListRepositories(ctx); err != nil || len(list.Repositories) != 2 {
		t.Fatalf("second repository not registered: %d %v", len(list.Repositories), err)
	}
}

func TestRegisterRepositoryIsIdempotentByRequestID(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	first := registerRepository(t, s, ctx, "same", "https://github.com/O/N")
	second := registerRepository(t, s, ctx, "same", "https://github.com/O/N")
	if first != second {
		t.Fatalf("replay differs: %+v vs %+v", first, second)
	}
	if _, err := s.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: "same", SourceURL: "https://github.com/O/other", DefaultBranch: "main"}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("differing fingerprint on the same request id was accepted")
	}
	list, err := s.ListRepositories(ctx)
	if err != nil || len(list.Repositories) != 1 {
		t.Fatalf("idempotent replay created extra rows: %d %v", len(list.Repositories), err)
	}
}

// TestRetireIsTheRollbackAndTouchesNoOtherRepository is acceptance A12.
func TestRetireIsTheRollbackAndTouchesNoOtherRepository(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	first := registerRepository(t, s, ctx, "req-1", "https://github.com/O/first")
	second := registerRepository(t, s, ctx, "req-2", "https://github.com/O/second")

	before, ok, err := s.GetRepository(ctx, second.RepositoryID)
	if err != nil || !ok {
		t.Fatalf("second not readable: %v", err)
	}

	retired, err := s.RetireRepository(ctx, application.RetireRepositoryRequest{RequestID: "retire-1", RepositoryID: first.RepositoryID, ExpectedVersion: first.Version})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if retired.Status != domain.RepositoryRetired || retired.Version != first.Version+1 {
		t.Fatalf("retire produced %+v", retired)
	}

	// The other Repository is byte-identical, version included.
	after, ok, err := s.GetRepository(ctx, second.RepositoryID)
	if err != nil || !ok {
		t.Fatalf("second not readable after retire: %v", err)
	}
	if after.Version != before.Version || after.Status != before.Status || after.Locator != before.Locator {
		t.Fatalf("retiring one Repository altered another: before=%+v after=%+v", before, after)
	}

	// The retired Repository is excluded from the list but still readable,
	// so the rollback is observable rather than a disappearance.
	list, err := s.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Repositories) != 1 || list.Repositories[0].RepositoryID != second.RepositoryID {
		t.Fatalf("list after retire = %+v", list.Repositories)
	}
	detail, ok, err := s.GetRepository(ctx, first.RepositoryID)
	if err != nil || !ok {
		t.Fatalf("retired repository is no longer readable: %v", err)
	}
	if detail.Status != domain.RepositoryRetired || detail.Executability.State != "retired" || detail.Executability.Executable {
		t.Fatalf("retired detail = %+v", detail)
	}

	// Retire is idempotent by request id, and a second, different retire of
	// an already-retired Repository is refused.
	replay, err := s.RetireRepository(ctx, application.RetireRepositoryRequest{RequestID: "retire-1", RepositoryID: first.RepositoryID, ExpectedVersion: first.Version})
	if err != nil || replay != retired {
		t.Fatalf("retire replay = %+v err=%v", replay, err)
	}
	if _, err = s.RetireRepository(ctx, application.RetireRepositoryRequest{RequestID: "retire-2", RepositoryID: first.RepositoryID, ExpectedVersion: retired.Version}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("second retire error = %v, want ErrInvalidTransition", err)
	}
	if _, err = s.RetireRepository(ctx, application.RetireRepositoryRequest{RequestID: "retire-3", RepositoryID: "missing"}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("retire of an unknown repository = %v", err)
	}
	// Retiring frees the locator: the rollback returns the Installation to
	// its pre-registration state for that coordinate.
	if _, err = s.RegisterRepository(ctx, application.RegisterRepositoryRequest{RequestID: "re-register", SourceURL: "https://github.com/O/first", DefaultBranch: "main"}); err != nil {
		t.Fatalf("re-registering a retired locator was refused: %v", err)
	}
}

// TestRepositoryIsInstallationOwnedNotPersonOwned is acceptance A7: a
// Repository registered by one owner subject is visible to any other
// authenticated owner caller, and no runner identifier is persisted on it.
func TestRepositoryIsInstallationOwnedNotPersonOwned(t *testing.T) {
	st := memory.New()
	s, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	first := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner-one"})
	other := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "owner-two"})
	registered := registerRepository(t, s, first, "req-1", "https://github.com/O/N")

	list, err := s.ListRepositories(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Repositories) != 1 || list.Repositories[0].RepositoryID != registered.RepositoryID {
		t.Fatalf("a Repository registered by another owner is not visible: %+v", list.Repositories)
	}
	if list.Repositories[0].RequestedBy.Subject != "owner-one" {
		t.Fatalf("attribution lost: %+v", list.Repositories[0].RequestedBy)
	}
	// The stored record carries no runner identifier under any field.
	err = st.Transact(context.Background(), func(u application.UnitOfWork) error {
		stored, ok, e := u.Repository(context.Background(), registered.RepositoryID)
		if e != nil || !ok {
			t.Fatalf("stored repository missing: %v", e)
		}
		if stored.RequestedBy.ActorType != domain.ActorTypeOwner {
			t.Fatalf("stored requested_by = %+v", stored.RequestedBy)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestObserveRepositoryRecordsBoundedForgeEvidence(t *testing.T) {
	s, _ := service()
	ownerCtx := owner(context.Background())
	runnerCtx := runner(context.Background(), "runner-1")
	registered := registerRepository(t, s, ownerCtx, "req-1", "https://github.com/O/N")

	// Before any Observation the answer is explicitly unobserved.
	detail, ok, err := s.GetRepository(ownerCtx, registered.RepositoryID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if detail.Executability.State != "unobserved" || detail.Executability.Executable || detail.Executability.Reason == "" {
		t.Fatalf("pre-observation executability = %+v", detail.Executability)
	}
	if detail.Observation != nil {
		t.Fatalf("pre-observation detail carries an observation: %+v", detail.Observation)
	}

	out, err := s.ObserveRepository(runnerCtx, application.ObserveRepositoryRequest{RequestID: "obs-1", RepositoryID: registered.RepositoryID, Reachable: true, CanPush: true, DefaultBranch: "main", ForgeNodeID: "node-1", AdapterVersion: "gh-test"})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if !out.Accepted || out.ObservedAt.IsZero() || !out.Executability.Executable {
		t.Fatalf("observe response = %+v", out)
	}

	detail, ok, err = s.GetRepository(ownerCtx, registered.RepositoryID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if detail.Executability.State != "executable" || !detail.Executability.Executable {
		t.Fatalf("post-observation executability = %+v", detail.Executability)
	}
	if detail.Observation == nil || detail.Observation.DefaultBranch != "main" || detail.Observation.ForgeNodeID != "node-1" {
		t.Fatalf("post-observation detail = %+v", detail.Observation)
	}

	// Idempotent by request id, and a differing fingerprint on the same id
	// is refused.
	replay, err := s.ObserveRepository(runnerCtx, application.ObserveRepositoryRequest{RequestID: "obs-1", RepositoryID: registered.RepositoryID, Reachable: true, CanPush: true, DefaultBranch: "main", ForgeNodeID: "node-1", AdapterVersion: "gh-test"})
	if err != nil || replay.ObservedAt != out.ObservedAt {
		t.Fatalf("observe replay = %+v err=%v", replay, err)
	}
	if _, err = s.ObserveRepository(runnerCtx, application.ObserveRepositoryRequest{RequestID: "obs-1", RepositoryID: registered.RepositoryID, Reachable: false}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("differing observation on the same request id accepted")
	}
	if _, err = s.ObserveRepository(runnerCtx, application.ObserveRepositoryRequest{RequestID: "obs-2", RepositoryID: "missing", Reachable: true}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("observation for an unknown repository = %v", err)
	}

	// An unreachable Observation blocks rather than silently allowing.
	if _, err = s.ObserveRepository(runnerCtx, application.ObserveRepositoryRequest{RequestID: "obs-3", RepositoryID: registered.RepositoryID, Reachable: false, Reason: "not found"}); err != nil {
		t.Fatal(err)
	}
	detail, _, err = s.GetRepository(ownerCtx, registered.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Executability.State != "blocked" || detail.Executability.Executable {
		t.Fatalf("unreachable executability = %+v", detail.Executability)
	}
}

// TestRepositoryDetailRendersAllSixDeclaredObservables is acceptance A10:
// every one of cap-repository-registration's six observable_result fields is
// rendered, and every field without a data source at this commit carries an
// explicit machine-readable state plus a reason rather than a plausible value.
func TestRepositoryDetailRendersAllSixDeclaredObservables(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	registered := registerRepository(t, s, ctx, "req-1", "https://github.com/O/N")
	detail, ok, err := s.GetRepository(ctx, registered.RepositoryID)
	if err != nil || !ok {
		t.Fatal(err)
	}

	// 1. Identity: measured.
	if detail.RepositoryID != registered.RepositoryID || detail.Locator.Owner != "o" || detail.Locator.Name != "n" || detail.Status != domain.RepositoryRegistered || detail.Version != 1 {
		t.Fatalf("identity = %+v", detail)
	}
	// 2. Application Preview/Stable state, 3. policy, 5. runners and AI
	// resources: explicitly not-implemented with a reason each.
	for name, field := range map[string]application.ObservedState{
		"application_release":      detail.ApplicationRelease,
		"policy":                   detail.Policy,
		"runners_and_ai_resources": detail.RunnersAndAIResources,
	} {
		if field.State != application.ObservedNotImplemented {
			t.Fatalf("%s state = %q, want %q", name, field.State, application.ObservedNotImplemented)
		}
		if len(field.Reason) < 40 {
			t.Fatalf("%s reason is not a stated reason: %q", name, field.Reason)
		}
	}
	// 4. Requirement Backlog: repository scope unobserved with a reason, plus
	// the Installation-scoped count that actually was measured.
	if detail.RequirementBacklog.State != application.ObservedUnobserved || detail.RequirementBacklog.Reason == "" {
		t.Fatalf("backlog = %+v", detail.RequirementBacklog)
	}
	if detail.RequirementBacklog.InstallationScope == nil {
		t.Fatal("backlog installation_scope is absent; the measured Installation-wide count must be reported under its own scope name")
	}
	// 6. Executability: unobserved, with the reason naming the missing
	// measurement.
	if detail.Executability.State != "unobserved" || detail.Executability.Reason == "" {
		t.Fatalf("executability = %+v", detail.Executability)
	}
	// The measured Control input is reported too.
	if detail.EffectiveControlMode != domain.ControlAllow {
		t.Fatalf("effective control mode = %q", detail.EffectiveControlMode)
	}

	if _, ok, err = s.GetRepository(ctx, "missing"); err != nil || ok {
		t.Fatalf("unknown repository: ok=%v err=%v", ok, err)
	}
	if _, _, err = s.GetRepository(ctx, ""); err == nil {
		t.Fatal("empty repository id accepted")
	}
}

// TestRepositoryControlGateDeniesUnderStop asserts the domain.PermitIntake
// gate actually runs on the repository-scoped target: a repository-scoped
// Control Intent must be able to deny registration-side work. This is also
// the first call site in the codebase where ControlTarget.RepositoryID is
// non-empty, which is what makes a domain.ScopeRepository Intent match.
func TestRepositoryControlGateDeniesUnderStop(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	registered := registerRepository(t, s, ctx, "req-1", "https://github.com/O/N")

	if _, err := s.Control(ctx, application.ControlRequest{RequestID: "ctl-1", Scope: domain.ControlScope{Kind: domain.ScopeRepository, Value: registered.RepositoryID}, Mode: domain.ControlEmergencyStop, Reason: "test"}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if _, err := s.RetireRepository(ctx, application.RetireRepositoryRequest{RequestID: "retire-1", RepositoryID: registered.RepositoryID}); !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("retire under emergency stop error = %v, want ErrControlDenied", err)
	}
	if _, err := s.ObserveRepository(runner(context.Background(), "runner-1"), application.ObserveRepositoryRequest{RequestID: "obs-1", RepositoryID: registered.RepositoryID, Reachable: true}); !errors.Is(err, domain.ErrControlDenied) {
		t.Fatalf("observe under emergency stop error = %v, want ErrControlDenied", err)
	}
	// The detail view still reports the Repository, now blocked with the
	// measured reason.
	detail, ok, err := s.GetRepository(ctx, registered.RepositoryID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if detail.Executability.State != "blocked" || detail.EffectiveControlMode != domain.ControlEmergencyStop {
		t.Fatalf("detail under stop = %+v", detail.Executability)
	}
}

// TestRegisterRepositoryTouchesNothingExternal is the local half of
// acceptance A11: the Service that performs registration is constructed from
// a Transactor, a Clock and an IDGenerator and nothing else, so there is no
// seam through which it could reach GitHub, create a branch or a PR, or read
// a credential. The structural half (no exec.Command anywhere in the Control
// Plane) is asserted from the AST in internal/api/repositories_test.go.
func TestRegisterRepositoryTouchesNothingExternal(t *testing.T) {
	st := memory.New()
	s, err := application.NewServiceWithConfig(st, clock{}, &ids{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := owner(context.Background())
	before, err := s.Export(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	registered := registerRepository(t, s, ctx, "req-1", "https://github.com/O/N")
	after, err := s.Export(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Registration adds a Repository and nothing else: no Requirement and no
	// Control record appears, so no existing Application record was touched.
	// The one new export row is the repository.registered event, which is
	// this task's own audit trail rather than a change to anything existing.
	count := func(rows []application.ExportRecord, kind string) int {
		n := 0
		for _, row := range rows {
			if row.Kind == kind {
				n++
			}
		}
		return n
	}
	for _, kind := range []string{"requirement", "control"} {
		if count(before, kind) != count(after, kind) {
			t.Fatalf("registration changed the exported %s metadata: %d -> %d", kind, count(before, kind), count(after, kind))
		}
	}
	if count(after, "event") != count(before, "event")+1 {
		t.Fatalf("registration recorded %d events, want exactly one", count(after, "event")-count(before, "event"))
	}
	if registered.Status != domain.RepositoryRegistered {
		t.Fatalf("registered = %+v", registered)
	}
	// The outbox is the only path by which this codebase performs an
	// external effect, and registration stages nothing on it.
	if err = st.Transact(context.Background(), func(u application.UnitOfWork) error {
		items, e := u.Outboxes(context.Background(), time.Unix(1700000000, 0).UTC().Add(time.Hour), 100)
		if e != nil {
			return e
		}
		if len(items) != 0 {
			t.Fatalf("registration staged %d outbox items; it must stage none", len(items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
