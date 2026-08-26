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
	// 4. Requirement Backlog: the repository scope is MEASURED since V2-071
	// (A11). This assertion is the one deliberate change to this test: it
	// previously demanded ObservedUnobserved with a reason, because no
	// Requirement-to-Repository association existed. The association now
	// exists, so the honest answer is the count that was actually read --
	// zero here, because this Repository has no linked Requirement yet, and a
	// measured zero is a measurement, not an absence. The Installation-scoped
	// count keeps being reported under its own name, unchanged.
	if detail.RequirementBacklog.State != application.ObservedMeasured || detail.RequirementBacklog.Reason == "" {
		t.Fatalf("backlog = %+v", detail.RequirementBacklog)
	}
	if detail.RequirementBacklog.RequirementCount != 0 || detail.RequirementBacklog.Truncated {
		t.Fatalf("backlog count = %d truncated=%v; no Requirement is linked to this Repository in this test", detail.RequirementBacklog.RequirementCount, detail.RequirementBacklog.Truncated)
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

// ---------------------------------------------------------------------------
// V2-071: the Requirement-to-Repository association, and the repair of the
// repository Control scope. Every test below is deterministic: the clock is
// injected, no sleep is taken, no timer fires and no goroutine is started.
// ---------------------------------------------------------------------------

// TestCaptureWritesTheWriteOnceRequirementRepositoryLink is acceptance A3 and
// A4: Capture writes the link with Capture's own ceremony, the link is
// write-once, an unlinked Capture keeps working exactly as before, and a
// Capture naming a Repository that does not exist or has been retired is
// refused and creates no Requirement.
func TestCaptureWritesTheWriteOnceRequirementRepositoryLink(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	registered := registerRepository(t, s, ctx, "reg-1", "https://github.com/O/N")

	captured, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-1", Text: "linked", RepositoryID: registered.RepositoryID})
	if err != nil {
		t.Fatalf("linked capture: %v", err)
	}
	if captured.RepositoryID != registered.RepositoryID {
		t.Fatalf("capture response repository_id = %q, want %q", captured.RepositoryID, registered.RepositoryID)
	}
	readLink := func(requirementID string) (domain.RequirementRepositoryLink, bool) {
		t.Helper()
		var link domain.RequirementRepositoryLink
		var found bool
		if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
			var x error
			link, found, x = u.RequirementRepositoryLink(context.Background(), requirementID)
			return x
		}); e != nil {
			t.Fatalf("reading the link: %v", e)
		}
		return link, found
	}
	link, found := readLink(captured.RequirementID)
	if !found {
		t.Fatal("Capture with a repository_id wrote no link")
	}
	if link.RepositoryID.String() != registered.RepositoryID || link.RequirementID.String() != captured.RequirementID {
		t.Fatalf("link = %+v", link)
	}
	if link.AssignedAt.IsZero() {
		t.Fatal("the link carries no assignment instant; the injected clock was not read")
	}
	if link.RequestedBy.ActorType != domain.ActorTypeOwner || link.RequestedBy.Subject != "actor-1" {
		t.Fatalf("link attribution = %+v; Capture's own requestedBy must be carried", link.RequestedBy)
	}

	// A Capture with no repository_id keeps working exactly as today and
	// writes no link at all.
	unlinked, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-2", Text: "unlinked"})
	if err != nil {
		t.Fatalf("unlinked capture: %v", err)
	}
	if unlinked.RepositoryID != "" {
		t.Fatalf("unlinked capture reported repository_id %q", unlinked.RepositoryID)
	}
	if _, found = readLink(unlinked.RequirementID); found {
		t.Fatal("a Capture with no repository_id wrote a link")
	}

	// A3, case 1: an identical link written again is an idempotent replay,
	// not a second record.
	if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(context.Background(), link)
	}); e != nil {
		t.Fatalf("identical re-write must be an idempotent replay: %v", e)
	}
	replayed, found := readLink(captured.RequirementID)
	if !found || replayed != link {
		t.Fatalf("idempotent replay changed the record: %+v vs %+v", replayed, link)
	}
	// A3, case 2: a second link for the same Requirement naming a different
	// Repository is a conflict refusal.
	other := registerRepository(t, s, ctx, "reg-2", "https://github.com/O/Other")
	conflict := link
	conflict.RepositoryID = domain.RepositoryID(other.RepositoryID)
	err = st.Transact(context.Background(), func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(context.Background(), conflict)
	})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a second, differing link must be a conflict refusal, got %v", err)
	}
	if still, _ := readLink(captured.RequirementID); still.RepositoryID != link.RepositoryID {
		t.Fatalf("the refused write changed the stored link: %+v", still)
	}

	// A4: naming a Repository that does not exist is refused, and no
	// Requirement is created.
	if _, err = s.Capture(ctx, application.CaptureRequest{RequestID: "cap-missing", Text: "x", RequirementID: "req-missing-repo", RepositoryID: "no-such-repository"}); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("capture naming an unregistered repository = %v, want ErrNotFound", err)
	}
	if _, ok, e := s.GetRequirement(ctx, "req-missing-repo"); e != nil || ok {
		t.Fatalf("the refused capture created a Requirement: ok=%v err=%v", ok, e)
	}
	// A4: naming a retired Repository is refused too.
	if _, err = s.RetireRepository(ctx, application.RetireRepositoryRequest{RequestID: "retire-1", RepositoryID: other.RepositoryID}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err = s.Capture(ctx, application.CaptureRequest{RequestID: "cap-retired", Text: "x", RequirementID: "req-retired-repo", RepositoryID: other.RepositoryID}); !errors.Is(err, application.ErrRepositoryNotAvailable) {
		t.Fatalf("capture naming a retired repository = %v, want ErrRepositoryNotAvailable", err)
	}
	if _, ok, e := s.GetRequirement(ctx, "req-retired-repo"); e != nil || ok {
		t.Fatalf("the refused capture created a Requirement: ok=%v err=%v", ok, e)
	}
}

// TestRepositoryScopedControlIntentActuallyMatchesAPlannedIncrement is
// acceptance A10 and A7: the dead repository Control scope is proven alive.
// It carries both controls A10 demands -- the positive control (the canonical
// target's RepositoryID is non-empty and equal to the registered id, which is
// the assertion that fails on the starting tree, where the value came from the
// deleted ServiceConfig.RepositoryID and was always empty) and the negative
// control (a different repository id neither matches nor appears in the
// ControlProgress snapshot).
func TestRepositoryScopedControlIntentActuallyMatchesAPlannedIncrement(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	registered := registerRepository(t, s, ctx, "reg-1", "https://github.com/O/N")

	captured, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-1", Text: "scoped", RepositoryID: registered.RepositoryID})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := s.Plan(ctx, application.PlanRequest{RequestID: "plan-1", RequirementID: captured.RequirementID, ExpectedRequirementVersion: captured.Version})
	if err != nil {
		t.Fatal(err)
	}

	// (i) The positive control: A7's assertion. The stored canonical target
	// carries the registered Repository's own id.
	var stored domain.ControlTarget
	if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		target, ok, x := u.CanonicalTarget(context.Background(), planned.IncrementID, "")
		if x != nil {
			return x
		}
		if !ok {
			t.Fatal("Plan stored no canonical target")
		}
		stored = target
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if stored.RepositoryID == "" {
		t.Fatal("positive control: the canonical target's RepositoryID is empty; the repository Control scope is still dead")
	}
	if stored.RepositoryID != registered.RepositoryID {
		t.Fatalf("canonical target repository = %q, want %q", stored.RepositoryID, registered.RepositoryID)
	}

	// (ii) The domain's own matcher holds for a repository-scoped intent
	// naming that id. internal/domain/control.go is untouched: matching
	// always worked, it had simply never been given a value to match.
	scope := domain.ControlScope{Kind: domain.ScopeRepository, Value: registered.RepositoryID}
	if !domain.ControlApplies(scope, stored) {
		t.Fatalf("ControlApplies(%+v, %+v) = false", scope, stored)
	}
	// (iii) The negative control: another repository id does not match.
	otherScope := domain.ControlScope{Kind: domain.ScopeRepository, Value: "some-other-repository"}
	if domain.ControlApplies(otherScope, stored) {
		t.Fatalf("negative control: ControlApplies(%+v, %+v) = true", otherScope, stored)
	}

	// The same proof, end to end through Control's own snapshot: claim the
	// Increment so a lease exists, then request a repository-scoped graceful
	// stop and read the ControlProgress targets.
	if _, err = s.Prepare(ctx, application.PrepareRequest{RequestID: "prep-1", IncrementID: planned.IncrementID, ExpectedVersion: planned.Version}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(runner(ctx, "runner-1"), application.ClaimRequest{RequestID: "claim-1", IncrementID: planned.IncrementID, ExpectedIncrementVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	at := clock{}.Now()
	matching, err := s.Control(ctx, application.ControlRequest{RequestID: "ctl-1", Scope: scope, Mode: domain.ControlGracefulStop, Reason: "repository-scoped stop", At: at})
	if err != nil {
		t.Fatal(err)
	}
	nonMatching, err := s.Control(ctx, application.ControlRequest{RequestID: "ctl-2", Scope: otherScope, Mode: domain.ControlGracefulStop, Reason: "unrelated repository", At: at})
	if err != nil {
		t.Fatal(err)
	}
	progressTargets := func(revision domain.Revision) []domain.ControlTargetSnapshot {
		t.Helper()
		var targets []domain.ControlTargetSnapshot
		if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
			progress, ok, x := u.ControlProgress(context.Background(), revision)
			if x != nil {
				return x
			}
			if !ok {
				t.Fatalf("no ControlProgress for revision %d", revision)
			}
			targets = progress.Targets
			return nil
		}); e != nil {
			t.Fatal(e)
		}
		return targets
	}
	targets := progressTargets(matching.Revision)
	if len(targets) != 1 {
		t.Fatalf("a repository-scoped intent naming the registered repository matched %d targets, want 1", len(targets))
	}
	if targets[0].LeaseID.String() != claimed.LeaseID || targets[0].Target.RepositoryID != registered.RepositoryID {
		t.Fatalf("matched target = %+v", targets[0])
	}
	if unrelated := progressTargets(nonMatching.Revision); len(unrelated) != 0 {
		t.Fatalf("negative control: an intent naming another repository matched %d targets: %+v", len(unrelated), unrelated)
	}
}

// TestHeartbeatTargetCarriesNoRepository is acceptance A9. Heartbeat is the
// deliberate exception: its target names a Runner and nothing else, so a
// repository-scoped stop must NOT match it.
func TestHeartbeatTargetCarriesNoRepository(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	registered := registerRepository(t, s, ctx, "reg-1", "https://github.com/O/N")
	runnerCtx := runner(context.Background(), "runner-1")
	if _, err := s.Heartbeat(runnerCtx, application.HeartbeatRequest{RequestID: "hb-1"}); err != nil {
		t.Fatal(err)
	}
	var observed domain.RunnerObservation
	if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		v, ok, x := u.RunnerObservation(context.Background(), "runner-1")
		if x != nil {
			return x
		}
		if !ok {
			t.Fatal("no RunnerObservation was recorded")
		}
		observed = v
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if observed.Target.RepositoryID != "" {
		t.Fatalf("a heartbeat target carries a repository %q; a Runner is not repository-bound", observed.Target.RepositoryID)
	}
	if observed.Target.RunnerID.String() != "runner-1" || observed.Target.InstallationID != "install" {
		t.Fatalf("heartbeat target = %+v", observed.Target)
	}
	scope := domain.ControlScope{Kind: domain.ScopeRepository, Value: registered.RepositoryID}
	if domain.ControlApplies(scope, observed.Target) {
		t.Fatal("a repository-scoped Control Intent matches a heartbeat target; a repository-scoped stop would apply to unrelated work")
	}
	// The runner-scoped intent that IS meant to reach it still does.
	if !domain.ControlApplies(domain.ControlScope{Kind: domain.ScopeRunner, Value: "runner-1"}, observed.Target) {
		t.Fatal("a runner-scoped Control Intent no longer matches a heartbeat target")
	}
}

// TestControlSnapshotResolvesTheRepositoryFromTheLeasesIncrement is
// acceptance A8: with no canonical target stored, the snapshot path still
// carries a real RepositoryID, resolved lease -> Increment -> Requirement ->
// link, and the fill-only-when-empty RunnerID merge is unchanged.
//
// The lease is written directly rather than through Plan/Prepare/Claim
// precisely because Plan is what stores a canonical target: a lease with none
// is exactly the state a lease created before this change is in, and it
// cannot be produced by running Plan.
func TestControlSnapshotResolvesTheRepositoryFromTheLeasesIncrement(t *testing.T) {
	s, st := service()
	ctx := owner(context.Background())
	at := clock{}.Now()
	registered := registerRepository(t, s, ctx, "reg-1", "https://github.com/O/N")
	linked, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-1", Text: "fallback", RepositoryID: registered.RepositoryID})
	if err != nil {
		t.Fatal(err)
	}
	unlinked, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-2", Text: "no repository"})
	if err != nil {
		t.Fatal(err)
	}
	seedLease := func(u application.UnitOfWork, incrementID, leaseID, requirementID string) error {
		inc := domain.Increment{ID: domain.IncrementID(incrementID), RequirementID: domain.RequirementID(requirementID), Status: domain.IncrementProposed, Version: 1}
		if x := u.SaveIncrement(context.Background(), inc, 0); x != nil {
			return x
		}
		lease := domain.Lease{
			ID: domain.LeaseID(leaseID), IncrementID: domain.IncrementID(incrementID),
			ExecutionID: domain.ExecutionID("exec-" + incrementID), RunnerID: "runner-1",
			FencingToken: 1, IssuedAt: at, ExpiresAt: at.Add(time.Minute),
			Status: domain.LeaseActive, Version: 1,
		}
		return u.SaveLease(context.Background(), lease, 0)
	}
	if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		if x := seedLease(u, "inc-linked", "lease-linked", linked.RequirementID); x != nil {
			return x
		}
		return seedLease(u, "inc-unlinked", "lease-unlinked", unlinked.RequirementID)
	}); e != nil {
		t.Fatal(e)
	}
	// Neither Increment has a canonical target: nothing called Plan.
	if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		for _, id := range []string{"inc-linked", "inc-unlinked"} {
			if _, ok, x := u.CanonicalTarget(context.Background(), id, ""); x != nil {
				return x
			} else if ok {
				t.Fatalf("increment %s already has a canonical target; the fallback path would not be exercised", id)
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	scope := domain.ControlScope{Kind: domain.ScopeRepository, Value: registered.RepositoryID}
	out, err := s.Control(ctx, application.ControlRequest{RequestID: "ctl-1", Scope: scope, Mode: domain.ControlGracefulStop, Reason: "fallback path", At: at})
	if err != nil {
		t.Fatal(err)
	}
	var targets []domain.ControlTargetSnapshot
	if e := st.Transact(context.Background(), func(u application.UnitOfWork) error {
		progress, ok, x := u.ControlProgress(context.Background(), out.Revision)
		if x != nil {
			return x
		}
		if !ok {
			t.Fatal("no ControlProgress")
		}
		targets = progress.Targets
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	// The negative control is structural: two active leases exist and only
	// the one whose Requirement carries a link matches.
	if len(targets) != 1 {
		t.Fatalf("the fallback path matched %d targets, want exactly the linked one: %+v", len(targets), targets)
	}
	got := targets[0]
	if got.Target.RepositoryID != registered.RepositoryID {
		t.Fatalf("fallback target repository = %q, want %q", got.Target.RepositoryID, registered.RepositoryID)
	}
	if got.Target.IncrementID.String() != "inc-linked" || got.Target.RunnerID.String() != "runner-1" {
		t.Fatalf("fallback target = %+v; the fill-only-when-empty RunnerID merge must still apply", got.Target)
	}
	if got.LeaseID.String() != "lease-linked" || got.ExecutionID.String() != "exec-inc-linked" {
		t.Fatalf("fallback snapshot = %+v", got)
	}
}

// TestRepositoryScopedBacklogIsMeasured is acceptance A11's positive side: the
// Repository detail reports a Requirement count that was actually read and
// scoped to this Repository, while the Installation-wide summary keeps its own
// name and its own (larger) value, so the two can never be confused.
func TestRepositoryScopedBacklogIsMeasured(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	first := registerRepository(t, s, ctx, "reg-1", "https://github.com/O/First")
	second := registerRepository(t, s, ctx, "reg-2", "https://github.com/O/Second")

	if _, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-1", Text: "a", RepositoryID: first.RepositoryID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-2", Text: "b", RepositoryID: first.RepositoryID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-3", Text: "c", RepositoryID: second.RepositoryID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-4", Text: "d"}); err != nil {
		t.Fatal(err)
	}

	detail, ok, err := s.GetRepository(ctx, first.RepositoryID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if detail.RequirementBacklog.State != application.ObservedMeasured {
		t.Fatalf("backlog state = %q, want %q", detail.RequirementBacklog.State, application.ObservedMeasured)
	}
	if detail.RequirementBacklog.RequirementCount != 2 || detail.RequirementBacklog.Truncated {
		t.Fatalf("backlog = %+v; two Requirements name this Repository", detail.RequirementBacklog)
	}
	if detail.RequirementBacklog.InstallationScope == nil || detail.RequirementBacklog.InstallationScope.Requirements != 4 {
		t.Fatalf("installation scope = %+v; all four Requirements are Installation-wide", detail.RequirementBacklog.InstallationScope)
	}
	secondDetail, ok, err := s.GetRepository(ctx, second.RepositoryID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if secondDetail.RequirementBacklog.RequirementCount != 1 {
		t.Fatalf("second repository backlog = %+v", secondDetail.RequirementBacklog)
	}

}

// TestRequirementRowCarriesItsRepository is acceptance A12: a linked
// Requirement row reports its repository and an unlinked one omits the field
// entirely, on both the page row and the detail view. It is a separate test
// from the backlog above for a reason that no longer holds: each read crosses
// one bounded read-quota reservation (quota.ReadTransactionUsage, 6,001 reads
// as the worst case), and until V2-087 the memory adapter never settled that
// reservation, so the two tests together exceeded the daily budget. V2-087 made
// that adapter credit back the unused part of the reservation the way the
// Firestore adapter always has, so the split is no longer required by the
// budget. It is left alone here because merging the two back together is a
// judgement about test shape, not part of that repair.
func TestRequirementRowCarriesItsRepository(t *testing.T) {
	s, _ := service()
	ctx := owner(context.Background())
	first := registerRepository(t, s, ctx, "reg-1", "https://github.com/O/First")
	linkedA, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-1", Text: "a", RepositoryID: first.RepositoryID})
	if err != nil {
		t.Fatal(err)
	}
	unlinked, err := s.Capture(ctx, application.CaptureRequest{RequestID: "cap-4", Text: "d"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListRequirementsPage(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]application.RequirementView{}
	for _, row := range page.Requirements {
		byID[row.RequirementID] = row
	}
	if got := byID[linkedA.RequirementID].RepositoryID; got != first.RepositoryID {
		t.Fatalf("linked row repository_id = %q, want %q", got, first.RepositoryID)
	}
	if got := byID[unlinked.RequirementID].RepositoryID; got != "" {
		t.Fatalf("unlinked row repository_id = %q, want the field absent", got)
	}
	linkedDetail, ok, err := s.GetRequirementDetail(ctx, linkedA.RequirementID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if linkedDetail.RepositoryID != first.RepositoryID {
		t.Fatalf("detail repository_id = %q, want %q", linkedDetail.RepositoryID, first.RepositoryID)
	}
	unlinkedDetail, ok, err := s.GetRequirementDetail(ctx, unlinked.RequirementID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if unlinkedDetail.RepositoryID != "" {
		t.Fatalf("unlinked detail repository_id = %q, want the field absent", unlinkedDetail.RepositoryID)
	}
}
