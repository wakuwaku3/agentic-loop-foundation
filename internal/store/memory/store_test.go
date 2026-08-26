package memory

// Memory-adapter closure of the Repository port (V2-064). This package had
// no test file before this task; these tests are deterministic and need no
// external process, so they are admissible under the repository's standing
// determinism rule.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
)

func repository(t *testing.T, id, owner, name string, status domain.RepositoryStatus, version domain.Version) domain.Repository {
	t.Helper()
	locator, err := domain.NormalizeSourceLocator(domain.SourceLocator{Owner: owner, Name: name, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return domain.Repository{ID: domain.RepositoryID(id), Locator: locator, Status: status, Version: version}
}

// TestSaveRepositoryOptimisticConcurrency mirrors SaveRequirement's contract
// exactly: a create must declare expected version 0, and a save must declare
// the stored version.
func TestSaveRepositoryOptimisticConcurrency(t *testing.T) {
	store := New()
	ctx := context.Background()

	// A create that claims a non-zero expected version is refused.
	err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repository(t, "repository-1", "o", "n", domain.RepositoryRegistered, 1), 3)
	})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("create at a non-zero expected version = %v, want ErrStaleVersion", err)
	}

	if err = store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repository(t, "repository-1", "o", "n", domain.RepositoryRegistered, 1), 0)
	}); err != nil {
		t.Fatalf("create at expected version 0: %v", err)
	}

	// A conflicting concurrent save (a writer that still believes the record
	// is at version 0) is refused.
	if err = store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repository(t, "repository-1", "o", "n", domain.RepositoryRetired, 2), 0)
	}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("conflicting concurrent save = %v, want ErrStaleVersion", err)
	}

	if err = store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepository(ctx, repository(t, "repository-1", "o", "n", domain.RepositoryRetired, 2), 1)
	}); err != nil {
		t.Fatalf("save at the stored version: %v", err)
	}

	if err = store.Transact(ctx, func(u application.UnitOfWork) error {
		stored, ok, e := u.Repository(ctx, "repository-1")
		if e != nil || !ok {
			t.Fatalf("stored repository missing: %v", e)
		}
		if stored.Status != domain.RepositoryRetired || stored.Version != 2 {
			t.Fatalf("stored = %+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRepositoryRollbackDoesNotLeak covers the copy-on-write clone: a
// transaction that returns an error must leave no trace of the Repository or
// the Observation it staged.
func TestRepositoryRollbackDoesNotLeak(t *testing.T) {
	store := New()
	ctx := context.Background()
	sentinel := errors.New("rollback")
	err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRepository(ctx, repository(t, "repository-1", "o", "n", domain.RepositoryRegistered, 1), 0); e != nil {
			return e
		}
		if e := u.SaveRepositoryObservation(ctx, domain.RepositoryObservation{RepositoryID: "repository-1", Reachable: true, ObservedAt: time.Unix(1700000000, 0).UTC()}); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("transaction error = %v", err)
	}
	if err = store.Transact(ctx, func(u application.UnitOfWork) error {
		if _, ok, e := u.Repository(ctx, "repository-1"); e != nil || ok {
			t.Fatalf("rolled-back repository leaked: ok=%v err=%v", ok, e)
		}
		if _, ok, e := u.RepositoryObservation(ctx, "repository-1"); e != nil || ok {
			t.Fatalf("rolled-back observation leaked: ok=%v err=%v", ok, e)
		}
		rows, e := u.Repositories(ctx)
		if e != nil || len(rows) != 0 {
			t.Fatalf("rolled-back list = %v %v", rows, e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRepositoriesAreOrderedAndDuplicateLocatorIsVisible proves the adapter
// gives the application what it needs to enforce the duplicate-locator
// constraint: a deterministic ordering and the normalised key of every row.
func TestRepositoriesAreOrderedAndDuplicateLocatorIsVisible(t *testing.T) {
	store := New()
	ctx := context.Background()
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRepository(ctx, repository(t, "repository-2", "o", "n", domain.RepositoryRegistered, 1), 0); e != nil {
			return e
		}
		return u.SaveRepository(ctx, repository(t, "repository-1", "o", "other", domain.RepositoryRegistered, 1), 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		rows, e := u.Repositories(ctx)
		if e != nil {
			return e
		}
		if len(rows) != 2 || rows[0].ID != "repository-1" || rows[1].ID != "repository-2" {
			t.Fatalf("rows = %+v; the list must be ordered by repository id", rows)
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

func TestRepositoryObservationIsLastWriterWins(t *testing.T) {
	store := New()
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepositoryObservation(ctx, domain.RepositoryObservation{RepositoryID: "repository-1", Reachable: false, ObservedAt: at})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRepositoryObservation(ctx, domain.RepositoryObservation{RepositoryID: "repository-1", Reachable: true, DefaultBranch: "main", ObservedAt: at.Add(time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		got, ok, e := u.RepositoryObservation(ctx, "repository-1")
		if e != nil || !ok {
			t.Fatalf("observation missing: %v", e)
		}
		if !got.Reachable || got.DefaultBranch != "main" || !got.ObservedAt.Equal(at.Add(time.Minute)) {
			t.Fatalf("observation = %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRequirementRepositoryLinkIsWriteOnceAndBounded is the memory half of
// V2-071 A3/A5/A13. The Firestore adapter carries a test with the same name
// and the same assertions (internal/store/firestore/store_test.go), so both
// implementations satisfy one contract.
func TestRequirementRepositoryLinkIsWriteOnceAndBounded(t *testing.T) {
	store := New()
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

	// An invalid link is refused by the adapter, not silently stored.
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(ctx, domain.RequirementRepositoryLink{RequirementID: "req-1", RepositoryID: "repo-1"})
	}); err == nil {
		t.Fatal("a link with no assignment instant was accepted")
	}

	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
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

	// Write-once: the identical link replays, a differing one conflicts.
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(ctx, link("req-1", "repo-1"))
	}); err != nil {
		t.Fatalf("identical re-write must be an idempotent replay: %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRequirementRepositoryLink(ctx, link("req-1", "repo-9"))
	}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a second, differing link = %v, want ErrStaleVersion", err)
	}

	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
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
		// The batch read returns exactly the ids that have a link.
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
		// The per-repository read is bounded and reports truncation.
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

	// A rolled-back transaction leaks no link into the committed state.
	failure := errors.New("rollback")
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRequirementRepositoryLink(ctx, link("req-rolled-back", "repo-1")); e != nil {
			return e
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("rollback = %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
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
// Runner version report (V2-069), memory adapter
// ===========================================================================
//
// The table below is application.RunnerVersionReportCases(), the same cases
// internal/store/firestore runs against the emulator. The table is shared by
// value so this adapter cannot pass behaviour the Firestore adapter does not
// implement; only the driver is local.

func runnerObservation(id string) domain.RunnerObservation {
	return domain.RunnerObservation{RunnerID: domain.RunnerID(id), Reachable: true, ObservedAt: time.Unix(1700000000, 0).UTC()}
}

func TestRunnerVersionReportBehaviouralTable(t *testing.T) {
	cases := application.RunnerVersionReportCases()
	if len(cases) == 0 {
		t.Fatal("the shared table is empty; the assertion would be vacuous")
	}
	ctx := context.Background()
	for _, c := range cases {
		store := New()
		for _, id := range c.Observations {
			observation := runnerObservation(id)
			if err := store.Transact(ctx, func(u application.UnitOfWork) error {
				return u.SaveRunnerObservation(ctx, observation)
			}); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
		}
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
		// The read happens in a separate transaction from every write, so a
		// row that survives here really was committed.
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
			if got[i] != c.Want[i] {
				t.Fatalf("%s: row %d = %#v want %#v", c.Name, i, got[i], c.Want[i])
			}
		}
		// Every reported row is also readable by key.
		for _, want := range c.Want {
			if !want.Reported() {
				continue
			}
			if err := store.Transact(ctx, func(u application.UnitOfWork) error {
				v, ok, e := u.RunnerVersionReport(ctx, want.RunnerID)
				if e != nil {
					return e
				}
				if !ok || v != want {
					t.Fatalf("%s: keyed read of %q = %#v ok=%v, want %#v", c.Name, want.RunnerID, v, ok, want)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s: %v", c.Name, err)
			}
		}
	}
	t.Logf("memory adapter satisfied %d shared cases", len(cases))
}

func TestRunnerVersionReportRollbackLeaksNothing(t *testing.T) {
	store := New()
	ctx := context.Background()
	failure := errors.New("rollback")
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRunnerVersionReport(ctx, application.RunnerVersionReport{RunnerID: "runner-x", Version: "1.0.0", BinarySHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", SchemaMin: 1, SchemaMax: 2, ReportedAt: time.Unix(1700000000, 0).UTC()}); e != nil {
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
			t.Fatal("a rolled-back transaction committed a Runner version report")
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
	// An empty runner id is refused rather than stored under the empty key.
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SaveRunnerVersionReport(ctx, application.RunnerVersionReport{Version: "1.0.0"})
	}); err == nil {
		t.Fatal("a report with no runner id was accepted")
	}
}

// ===========================================================================
// V2-073 A10: the Requirement's capture time, one behavioural table, two
// adapters.
// ===========================================================================
//
// internal/store/firestore/store_test.go carries a table with the SAME name
// and the same four cases. The design's claim is that neither adapter needs a
// production change to carry the field -- this adapter copies the value with
// the rest of the Requirement in state.clone(), and the Firestore adapter
// serializes it through the existing plain json.Marshal -- so neither
// store.go is edited and these tests are what makes that claim checkable
// rather than asserted.
//
// Every instant is a literal or comes from an injected clock. There is no
// sleep, no timer and no goroutine.

type captureClock struct{ at time.Time }

func (c captureClock) Now() time.Time { return c.at }

type captureIDs struct{ n int }

func (i *captureIDs) Next(kind string) (string, error) {
	i.n++
	return fmt.Sprintf("%s-%d", kind, i.n), nil
}

func captureRequirement(t *testing.T, id string, capturedAt time.Time, version domain.Version) domain.Requirement {
	t.Helper()
	rid, err := domain.NewRequirementID(id)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Requirement{ID: rid, Status: domain.RequirementCaptured, Version: version, CapturedAt: capturedAt}
}

// TestRequirementCaptureTimeBehaviouralTable is the shared table. Case names
// and meanings are identical in both adapters' copies.
func TestRequirementCaptureTimeBehaviouralTable(t *testing.T) {
	recordedAt := time.Unix(1_650_000_000, 987_654_321).UTC()

	t.Run("a captured Requirement round-trips its capture time unchanged", func(t *testing.T) {
		store := New()
		ctx := context.Background()
		if err := store.Transact(ctx, func(u application.UnitOfWork) error {
			return u.SaveRequirement(ctx, captureRequirement(t, "round-trip", recordedAt, 1), 0)
		}); err != nil {
			t.Fatal(err)
		}
		got, ok := store.Requirement("round-trip")
		if !ok {
			t.Fatal("the Requirement was not committed")
		}
		if !got.CaptureRecorded() {
			t.Fatal("the round-tripped Requirement reports no capture time")
		}
		if !got.CapturedAt.Equal(recordedAt) {
			t.Fatalf("capture time = %v, want %v", got.CapturedAt, recordedAt)
		}
		if got.CapturedAt.Unix() != recordedAt.Unix() || got.CapturedAt.Nanosecond() != recordedAt.Nanosecond() {
			t.Fatalf("capture time representation = %d.%09d, want %d.%09d", got.CapturedAt.Unix(), got.CapturedAt.Nanosecond(), recordedAt.Unix(), recordedAt.Nanosecond())
		}
	})

	t.Run("a rolled-back transaction leaks no capture time into committed state", func(t *testing.T) {
		store := New()
		ctx := context.Background()
		if err := store.Transact(ctx, func(u application.UnitOfWork) error {
			return u.SaveRequirement(ctx, captureRequirement(t, "committed", recordedAt, 1), 0)
		}); err != nil {
			t.Fatal(err)
		}
		rolledBackAt := time.Unix(1_777_777_777, 0).UTC()
		abort := errors.New("abort")
		err := store.Transact(ctx, func(u application.UnitOfWork) error {
			if e := u.SaveRequirement(ctx, captureRequirement(t, "rolled-back", rolledBackAt, 1), 0); e != nil {
				return e
			}
			// Also try to move the committed record's capture time.
			moved := captureRequirement(t, "committed", rolledBackAt, 2)
			if e := u.SaveRequirement(ctx, moved, 1); e != nil {
				return e
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatalf("Transact = %v, want the abort error", err)
		}
		if _, ok := store.Requirement("rolled-back"); ok {
			t.Fatal("a rolled-back Requirement leaked into committed state")
		}
		got, ok := store.Requirement("committed")
		if !ok {
			t.Fatal("the committed Requirement disappeared")
		}
		if !got.CapturedAt.Equal(recordedAt) {
			t.Fatalf("the rolled-back capture time leaked: %v, want %v", got.CapturedAt, recordedAt)
		}
		if got.Version != 1 {
			t.Fatalf("version = %d, want 1", got.Version)
		}
	})

	t.Run("a record written before the field existed reads back as a legacy record", func(t *testing.T) {
		store := New()
		ctx := context.Background()
		// This adapter holds typed Go values, so a record that predates the
		// field is exactly a Requirement whose CapturedAt was never set.
		if err := store.Transact(ctx, func(u application.UnitOfWork) error {
			return u.SaveRequirement(ctx, captureRequirement(t, "legacy", time.Time{}, 1), 0)
		}); err != nil {
			t.Fatalf("a record with no capture time was refused: %v", err)
		}
		got, ok := store.Requirement("legacy")
		if !ok {
			t.Fatal("the legacy Requirement was not committed")
		}
		if got.CaptureRecorded() {
			t.Fatalf("a legacy record reports a capture time: %v", got.CapturedAt)
		}
		if !got.CapturedAt.IsZero() {
			t.Fatalf("capture time = %v, want the zero value", got.CapturedAt)
		}
	})

	t.Run("the value is unchanged by every lifecycle transition applied through the service", func(t *testing.T) {
		store := New()
		clockAt := time.Unix(1_650_000_000, 987_654_321).UTC()
		svc, err := application.NewServiceWithConfig(store, captureClock{at: clockAt}, &captureIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := application.ContextWithCaller(context.Background(), application.Caller{Role: application.RoleOwner, Subject: "capture-owner"})
		captured, err := svc.Capture(ctx, application.CaptureRequest{RequestID: "lifecycle-capture", Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		first, ok := store.Requirement(captured.RequirementID)
		if !ok {
			t.Fatal("the captured Requirement is missing")
		}
		if !first.CapturedAt.Equal(clockAt) {
			t.Fatalf("capture time = %v, want the injected clock's instant %v", first.CapturedAt, clockAt)
		}
		// Plan is the only other service path that writes a Requirement (it
		// appends an Increment and bumps the version); the application-side
		// AST assertion that these two are the complete set lives in
		// internal/application.
		for i, request := range []string{"lifecycle-plan-1", "lifecycle-plan-2"} {
			previous, ok := store.Requirement(captured.RequirementID)
			if !ok {
				t.Fatal("the Requirement disappeared mid-lifecycle")
			}
			if _, e := svc.Plan(ctx, application.PlanRequest{RequestID: request, RequirementID: captured.RequirementID, ExpectedRequirementVersion: previous.Version}); e != nil {
				t.Fatalf("Plan %d: %v", i+1, e)
			}
			got, ok := store.Requirement(captured.RequirementID)
			if !ok {
				t.Fatal("the Requirement disappeared mid-lifecycle")
			}
			if got.Version != previous.Version+1 {
				t.Fatalf("Plan %d left the Requirement at version %d, want %d; the transition did not happen and the assertion below would be vacuous", i+1, got.Version, previous.Version+1)
			}
			if len(got.Increments) != i+1 {
				t.Fatalf("Plan %d left %d increments, want %d", i+1, len(got.Increments), i+1)
			}
			if !got.CapturedAt.Equal(clockAt) {
				t.Fatalf("after Plan %d the capture time is %v, want %v", i+1, got.CapturedAt, clockAt)
			}
			if got.CapturedAt.Unix() != clockAt.Unix() || got.CapturedAt.Nanosecond() != clockAt.Nanosecond() {
				t.Fatalf("after Plan %d the capture time representation changed: %d.%09d", i+1, got.CapturedAt.Unix(), got.CapturedAt.Nanosecond())
			}
		}
	})
}

// TestRequirementCaptureTimeRollbackLeaksNothing is the rollback half stated
// as its own top-level test, matching this package's existing
// ...RollbackLeaksNothing naming, and asserting the stronger property that a
// rolled-back Capture leaves neither Requirement nor event behind.
func TestRequirementCaptureTimeRollbackLeaksNothing(t *testing.T) {
	store := New()
	ctx := context.Background()
	before := len(store.Events())
	abort := errors.New("abort")
	err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SaveRequirement(ctx, captureRequirement(t, "leaky", time.Unix(1_650_000_000, 0).UTC(), 1), 0); e != nil {
			return e
		}
		if e := u.Record(application.Event{ID: "event", AggregateID: "leaky", Type: "requirement.captured", At: time.Unix(1_650_000_000, 0).UTC()}, nil); e != nil {
			return e
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("Transact = %v, want the abort error", err)
	}
	if _, ok := store.Requirement("leaky"); ok {
		t.Fatal("the Requirement leaked")
	}
	if got := len(store.Events()); got != before {
		t.Fatalf("events = %d, want %d", got, before)
	}
}

// ===========================================================================
// Publication Observation (V2-072)
// ===========================================================================
//
// The same assertions run against both adapters, under the same test name, as
// TestRequirementRepositoryLinkIsWriteOnceAndBounded already does: write-once
// per operation identifier, an identical re-write is an idempotent replay, a
// differing re-write is a conflict, and the per-increment read applies its
// bound in the storage query and reports truncation. No sleep, no timer, no
// goroutine: every instant is an explicit value.

func publicationObservation(operationID, incrementID, executionID string, at time.Time) domain.PublicationObservation {
	ref, err := domain.PublicationRefName(domain.IncrementID(incrementID), domain.ExecutionID(executionID))
	if err != nil {
		panic(err)
	}
	const tree = "cccccccccccccccccccccccccccccccccccccccc"
	return domain.PublicationObservation{
		OperationID:     domain.OperationID(operationID),
		RepositoryID:    "repo-1",
		Ref:             ref,
		PublishedCommit: "dddddddddddddddddddddddddddddddddddddddd",
		PublishedTree:   tree,
		LocalCommit:     "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		LocalTree:       tree,
		TreesAgree:      true,
		State:           domain.PublicationPublishedAndObserved,
		Reason:          "the ref was created and all four content-addressed equalities held",
		ObservedAt:      at,
	}
}

func TestPublicationObservationIsWriteOncePerOperationAndBoundedPerIncrement(t *testing.T) {
	store := New()
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()

	// An invalid Observation is refused by the adapter, not silently stored.
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SavePublicationObservation(ctx, domain.PublicationObservation{OperationID: "op-bad", RepositoryID: "repo-1", Ref: "refs/heads/main", State: domain.PublicationUnobserved, Reason: "x", ObservedAt: at})
	}); err == nil {
		t.Fatal("an Observation whose ref is outside the reserved prefix was accepted")
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		bad := publicationObservation("op-bad-2", "inc-1", "exe-1", at)
		bad.State = "completed"
		return u.SavePublicationObservation(ctx, bad)
	}); err == nil {
		t.Fatal("an Observation with a state outside the closed set was accepted")
	}

	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SavePublicationObservation(ctx, publicationObservation("op-1", "inc-1", "exe-1", at)); e != nil {
			return e
		}
		if e := u.SavePublicationObservation(ctx, publicationObservation("op-2", "inc-1", "exe-2", at)); e != nil {
			return e
		}
		return u.SavePublicationObservation(ctx, publicationObservation("op-3", "inc-2", "exe-3", at))
	}); err != nil {
		t.Fatal(err)
	}

	// Write-once: the identical Observation replays, a differing one conflicts.
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		return u.SavePublicationObservation(ctx, publicationObservation("op-1", "inc-1", "exe-1", at))
	}); err != nil {
		t.Fatalf("identical re-write must be an idempotent replay: %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		differing := publicationObservation("op-1", "inc-1", "exe-1", at)
		differing.State = domain.PublicationConvergedOnExistingRef
		return u.SavePublicationObservation(ctx, differing)
	}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a second, differing Observation for one operation = %v, want ErrStaleVersion", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		differing := publicationObservation("op-1", "inc-1", "exe-1", at.Add(time.Hour))
		return u.SavePublicationObservation(ctx, differing)
	}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a re-write with a different instant = %v, want ErrStaleVersion", err)
	}

	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		stored, ok, e := u.PublicationObservation(ctx, "op-1")
		if e != nil {
			return e
		}
		if !ok || stored.State != domain.PublicationPublishedAndObserved || !stored.ObservedAt.Equal(at) {
			t.Fatalf("stored Observation = %+v ok=%v", stored, ok)
		}
		if _, ok, e = u.PublicationObservation(ctx, "op-absent"); e != nil {
			return e
		} else if ok {
			t.Fatal("an operation with no row reported an Observation")
		}
		// The per-increment read is bounded and reports truncation.
		rows, truncated, e := u.PublicationObservationsForIncrement(ctx, "inc-1", 10)
		if e != nil {
			return e
		}
		if truncated || len(rows) != 2 || rows[0].OperationID != "op-1" || rows[1].OperationID != "op-2" {
			t.Fatalf("per-increment read = %+v truncated=%v", rows, truncated)
		}
		rows, truncated, e = u.PublicationObservationsForIncrement(ctx, "inc-1", 1)
		if e != nil {
			return e
		}
		if !truncated || len(rows) != 1 {
			t.Fatalf("bounded per-increment read = %+v truncated=%v; the bound must be reported", rows, truncated)
		}
		rows, truncated, e = u.PublicationObservationsForIncrement(ctx, "inc-none", 10)
		if e != nil {
			return e
		}
		if truncated || len(rows) != 0 {
			t.Fatalf("an Increment with no publication = %+v truncated=%v", rows, truncated)
		}
		if _, _, e = u.PublicationObservationsForIncrement(ctx, "", 10); e == nil {
			t.Fatal("an empty increment id was accepted")
		}
		if rows, truncated, e = u.PublicationObservationsForIncrement(ctx, "inc-1", 0); e != nil {
			return e
		} else if truncated || len(rows) != 0 {
			t.Fatalf("a non-positive bound = %+v truncated=%v", rows, truncated)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A rolled-back transaction leaks no Observation into the committed state.
	failure := errors.New("rollback")
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if e := u.SavePublicationObservation(ctx, publicationObservation("op-rolled-back", "inc-3", "exe-4", at)); e != nil {
			return e
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("rollback = %v", err)
	}
	if err := store.Transact(ctx, func(u application.UnitOfWork) error {
		if _, ok, e := u.PublicationObservation(ctx, "op-rolled-back"); e != nil {
			return e
		} else if ok {
			t.Fatal("a rolled-back transaction committed an Observation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// backlogCursorWalkIDs are the seeded Requirement ids for the cursor walk. They
// are the same ids the Firestore package's identically named test seeds, and
// they are chosen so the two adapters genuinely disagree about order: this
// adapter orders by the raw domain id, so "req-aa" < "req-at", while the
// Firestore adapter orders by the base64url document key, which inverts that
// pair ("cmVxLWFh" > "cmVxLWF0"). base64url is not order-preserving.
var backlogCursorWalkIDs = []string{"req-aa", "req-at", "req-b0", "req-cz", "req-d9", "req-eZ", "req-f_"}

// eventCursorWalkIDs are the seeded Event ids, chosen the same way: raw
// "ev-aa" < "ev-am" while base64url("ev-aa") = "ZXYtYWE" > base64url("ev-am") =
// "ZXYtYW0".
var eventCursorWalkIDs = []string{"ev-aa", "ev-am", "ev-b0", "ev-cz", "ev-d9", "ev-eZ", "ev-f_"}

// TestThePageCursorTheCallerWasHandedIsAcceptedByTheNextPageUntilEveryRequirementIsCoveredOnce
// walks the Backlog cursor to exhaustion over this adapter: every next_cursor
// the read model hands out is fed straight back into the next call until a page
// reports none. It is driven through Service.ListRequirementsPage rather than
// through the port so the opaque v1 cursor envelope is exercised, which is the
// layer the observable outcome is stated about. The Firestore package carries
// this exact test name over its own adapter.
//
// This test asserts NOTHING about the order the ids arrive in, and the reason
// is not laziness. This adapter orders by the raw domain id; the Firestore
// adapter, under this very same test name, orders by the document id, which is
// base64url of the raw id. base64url is not order-preserving, so the two orders
// differ (see backlogCursorWalkIDs). An order assertion would pass here and
// fail on Firestore for a reason that has nothing to do with the cursor.
// Coverage is therefore asserted as a MULTISET -- no duplicate and no omission
// -- which is exactly the promise a stable total order plus an exclusive-after
// cursor makes, and which fails on refusal, on a duplicated row, on a skipped
// row and on a walk that does not terminate.
func TestThePageCursorTheCallerWasHandedIsAcceptedByTheNextPageUntilEveryRequirementIsCoveredOnce(t *testing.T) {
	store := New()
	ctx := context.Background()
	for _, raw := range backlogCursorWalkIDs {
		id, err := domain.NewRequirementID(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Transact(ctx, func(u application.UnitOfWork) error {
			return u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The clock is injected and FIXED, exactly as the Firestore package's copy
	// of this test fixes integrationClock at the same instant.
	//
	// It was not always fixed. When V2-079 wrote this walk, the clock was a
	// dayAdvancingClock the test stepped forward 25 hours PER PAGE, because
	// Service.transact reserves quota.ReadTransactionUsage (6,001 reads) as the
	// worst case for every read transaction and this adapter never settled that
	// reservation, unlike the Firestore adapter's countReads/trueUpQuota pair.
	// quota.DefaultBudget allows 25,000 reads per UTC day, so exactly four read
	// transactions fitted in one simulated day while a full walk of 7 records at
	// page_size 1 needs seven, and the only way to walk this adapter at all was
	// to cross midnight between pages and reset quota.Counter's daily
	// aggregate. V2-087 made this adapter settle its reservation the way the
	// real one does, so the workaround is gone and the two adapters' copies of
	// this test now hold fixture-identical clocks. Paging was always
	// clock-independent, so nothing this walk asserts has changed.
	clock := captureClock{at: time.Unix(1700000000, 0).UTC()}
	svc, err := application.NewServiceWithConfig(store, clock, &captureIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	owner := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "cursor-walk-owner"})
	// 1 pages one row at a time; 2 does not divide the seeded count of 7, which
	// is what catches an off-by-one in the exclusive boundary.
	for _, pageSize := range []int{1, 2} {
		t.Run(fmt.Sprintf("page_size=%d", pageSize), func(t *testing.T) {
			seen := map[string]int{}
			cursor := ""
			// The walk is bounded by the seeded record count, never by a timer.
			bound := len(backlogCursorWalkIDs)/pageSize + 2
			for pages := 0; ; pages++ {
				if pages > bound {
					t.Fatalf("walk did not terminate within %d pages", bound)
				}
				page, err := svc.ListRequirementsPage(owner, cursor, pageSize)
				if err != nil {
					t.Fatalf("page %d with cursor %q: %v", pages, cursor, err)
				}
				for _, r := range page.Requirements {
					seen[r.RequirementID]++
				}
				if page.NextCursor == "" {
					if len(page.Requirements) == 0 || len(page.Requirements) > pageSize {
						t.Fatalf("terminal page carried %d rows, want 1..%d", len(page.Requirements), pageSize)
					}
					break
				}
				if len(page.Requirements) != pageSize {
					t.Fatalf("non-terminal page %d carried %d rows, want exactly %d", pages, len(page.Requirements), pageSize)
				}
				cursor = page.NextCursor
			}
			assertMultisetCoversOnce(t, backlogCursorWalkIDs, seen)
		})
	}
}

// TestTheEventPageCursorTheCallerWasHandedIsAcceptedByTheNextPageUntilEveryEventIsCoveredOnce
// is the EventsPage counterpart, driven through the port because no route
// exposes it. It asserts no order, for the reason spelled out above.
func TestTheEventPageCursorTheCallerWasHandedIsAcceptedByTheNextPageUntilEveryEventIsCoveredOnce(t *testing.T) {
	store := New()
	ctx := context.Background()
	for _, raw := range eventCursorWalkIDs {
		id := raw
		if err := store.Transact(ctx, func(u application.UnitOfWork) error {
			return u.Record(application.Event{ID: id, AggregateType: "requirement", AggregateID: "req-aa", Type: "requirement.captured", Version: 1}, nil)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, pageSize := range []int{1, 2} {
		t.Run(fmt.Sprintf("page_size=%d", pageSize), func(t *testing.T) {
			seen := map[string]int{}
			after := ""
			bound := len(eventCursorWalkIDs)/pageSize + 2
			for pages := 0; ; pages++ {
				if pages > bound {
					t.Fatalf("walk did not terminate within %d pages", bound)
				}
				var rows []application.Event
				var more bool
				if err := store.Transact(ctx, func(u application.UnitOfWork) error {
					var e error
					rows, more, e = u.EventsPage(ctx, after, pageSize)
					return e
				}); err != nil {
					t.Fatalf("page %d after %q: %v", pages, after, err)
				}
				for _, e := range rows {
					seen[e.ID]++
				}
				if !more {
					if len(rows) == 0 || len(rows) > pageSize {
						t.Fatalf("terminal page carried %d rows, want 1..%d", len(rows), pageSize)
					}
					break
				}
				if len(rows) != pageSize {
					t.Fatalf("non-terminal page %d carried %d rows, want exactly %d", pages, len(rows), pageSize)
				}
				after = rows[len(rows)-1].ID
			}
			assertMultisetCoversOnce(t, eventCursorWalkIDs, seen)
		})
	}
}

// assertMultisetCoversOnce is the coverage assertion both walks share: the
// concatenation of every page equals the seeded ids as a multiset, so a
// duplicated row and an omitted row are both failures, and no order is named.
func assertMultisetCoversOnce(t *testing.T, want []string, got map[string]int) {
	t.Helper()
	for _, id := range want {
		if got[id] != 1 {
			t.Fatalf("id %q appeared %d times across the walk, want exactly 1 (seen=%v)", id, got[id], got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("walk covered %d distinct ids, want %d (seen=%v)", len(got), len(want), got)
	}
}

// ===========================================================================
// V2-087: the settled reservation, and the two adapters agreeing about it
// ===========================================================================

// workloadExpectation is one row of the cross-adapter workload table
// (dp-v2-087 d5 / A6): a named piece of work, and the committed daily
// quota.Usage the adapter must hold after that work's single transaction has
// settled its reservation.
type workloadExpectation struct {
	name  string
	usage quota.Usage
}

// crossAdapterWorkload is that table.
//
// THE LITERALS BELOW MUST STAY BYTE-IDENTICAL TO THE COPY IN
// internal/store/firestore/quota_integration_test.go, which carries the same
// table under the same test name over the other adapter. That duplication is
// deliberate and is the whole point: the two adapters must not disagree about
// what a reservation costs, and two literals differing is a diff a reader can
// see, where a shared helper would need a third package and a range-valued
// expectation would hide exactly the drift being guarded.
//
// If a row's two measurements ever disagree, neither literal is adjusted, the
// row is not deleted and the expectation is not widened into a range or a
// tolerance: both measurements are reported and the work stops.
var crossAdapterWorkload = []workloadExpectation{
	{name: "a bounded first page at page_size 1", usage: quota.Usage{Reads: 4, Writes: 1}},
	{name: "the same page at page_size 2", usage: quota.Usage{Reads: 6, Writes: 1}},
	{name: "a Requirement detail read", usage: quota.Usage{Reads: 4, Writes: 1}},
	{name: "a capture mutation", usage: quota.Usage{Reads: 4, Writes: 5}},
}

// crossAdapterWorkloadIDs are the Requirement ids the workload seeds, and they
// are the same ids the Firestore package's copy of this test seeds. The two
// adapters order a page differently -- this one by the raw domain id, that one
// by the base64url document key -- so the two pages may hold different rows;
// the COUNT of documents a Limit(limit+1) query touches is what this table is
// about, and that is the same on both.
var crossAdapterWorkloadIDs = []string{"wl-a", "wl-b", "wl-c"}

// crossAdapterWorkloadInstant is the single fixed injected instant every row
// runs on, identical in both packages. Nothing advances it: the point of this
// task is that no test has to buy budget by moving a clock.
func crossAdapterWorkloadInstant() time.Time { return time.Unix(1700000000, 0).UTC() }

// workloadFixture seeds the shared fixture and returns the store, a service on
// the fixed instant, and an owner context. The seeding transactions call no
// ReserveQuota at all -- they go straight through the port -- so they settle
// nothing and leave no quota record: the committed total read back after a
// workload row is exactly that row's own settled cost.
func workloadFixture(t *testing.T) (*Store, *application.Service, context.Context) {
	t.Helper()
	store := New()
	ctx := context.Background()
	for _, raw := range crossAdapterWorkloadIDs {
		id, err := domain.NewRequirementID(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Transact(ctx, func(u application.UnitOfWork) error {
			if e := u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0); e != nil {
				return e
			}
			return u.SaveRequirementText(ctx, raw, "text-"+raw)
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := application.NewServiceWithConfig(store, captureClock{at: crossAdapterWorkloadInstant()}, &captureIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return store, svc, application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "workload-owner"})
}

// committedWorkloadUsage reads the day's committed total back through the
// exported inspection seam. A day with no record at all is a failure, not a
// zero total: it would mean the workload reserved nothing.
func committedWorkloadUsage(t *testing.T, store *Store) quota.Usage {
	t.Helper()
	got, ok := store.QuotaTotal(crossAdapterWorkloadInstant())
	if !ok {
		t.Fatal("no quota record for the workload day: the transaction reserved nothing")
	}
	return got
}

// TestBothAdaptersSettleTheSameReservationForTheSameWorkload is A6. The
// Firestore package carries this exact test name over its own adapter, with the
// same four rows and the same literal expectations.
func TestBothAdaptersSettleTheSameReservationForTheSameWorkload(t *testing.T) {
	runners := []func(t *testing.T) quota.Usage{
		func(t *testing.T) quota.Usage {
			store, svc, owner := workloadFixture(t)
			if _, err := svc.ListRequirementsPage(owner, "", 1); err != nil {
				t.Fatal(err)
			}
			return committedWorkloadUsage(t, store)
		},
		func(t *testing.T) quota.Usage {
			store, svc, owner := workloadFixture(t)
			if _, err := svc.ListRequirementsPage(owner, "", 2); err != nil {
				t.Fatal(err)
			}
			return committedWorkloadUsage(t, store)
		},
		func(t *testing.T) quota.Usage {
			store, svc, owner := workloadFixture(t)
			if _, ok, err := svc.GetRequirementDetail(owner, crossAdapterWorkloadIDs[0]); err != nil {
				t.Fatal(err)
			} else if !ok {
				t.Fatal("the seeded Requirement was not found")
			}
			return committedWorkloadUsage(t, store)
		},
		func(t *testing.T) quota.Usage {
			store, svc, owner := workloadFixture(t)
			if _, err := svc.Capture(owner, application.CaptureRequest{RequestID: "workload-capture", Text: "x"}); err != nil {
				t.Fatal(err)
			}
			return committedWorkloadUsage(t, store)
		},
	}
	if len(runners) != len(crossAdapterWorkload) {
		t.Fatalf("%d workload runners for %d table rows; the table and the work must stay paired", len(runners), len(crossAdapterWorkload))
	}
	for i, row := range crossAdapterWorkload {
		i, row := i, row
		t.Run(row.name, func(t *testing.T) {
			got := runners[i](t)
			if got != row.usage {
				t.Fatalf("%s settled to %+v on the in-memory adapter, want %+v. If the Firestore half of this test measured %+v for the same row, the two adapters disagree about what a reservation costs: report both measurements and stop -- do not adjust either literal, delete the row, or widen the expectation into a range", row.name, got, row.usage, got)
			}
			if got.Reads >= quota.ReadTransactionUsage.Reads {
				t.Fatalf("%s settled to %+v, which is not below the worst-case read reservation (%d): nothing was credited back", row.name, got, quota.ReadTransactionUsage.Reads)
			}
		})
	}
}

// TestManyReadTransactionsFitInOneDayOnTheInMemoryAdapter is A7: the outcome
// asserted directly, on ONE fixed injected instant, with the committed total
// asserted and not merely the absence of an error.
//
// Asserting the committed total is what closes the two loopholes at once. A
// raised budget (which internal/quota being prohibited and byte-unchanged also
// forbids) would leave the total unchanged; a reservation moved to after the
// work, or removed, would leave a different total or none at all. Only a
// settled reservation produces exactly the measured cost per transaction.
func TestManyReadTransactionsFitInOneDayOnTheInMemoryAdapter(t *testing.T) {
	// The pre-repair capacity, derived and not asserted about: the worst case
	// divided into the budget is how many read transactions used to fit.
	worstCaseCapacity := quota.DefaultBudget.Reads / quota.ReadTransactionUsage.Reads
	if worstCaseCapacity <= 0 {
		t.Fatalf("worst-case read capacity = %d; the budget arithmetic is broken", worstCaseCapacity)
	}

	// Measure one read transaction's trued-up cost rather than assuming it.
	oneStore, oneSvc, oneOwner := workloadFixture(t)
	if _, err := oneSvc.ListRequirementsPage(oneOwner, "", 1); err != nil {
		t.Fatal(err)
	}
	perTransaction := committedWorkloadUsage(t, oneStore)
	if perTransaction.Reads <= 0 || perTransaction.Writes <= 0 {
		t.Fatalf("one read transaction settled to %+v; a transaction that costs nothing is not a measurement", perTransaction)
	}

	t.Run("a handful of read transactions costs a fraction of the day", func(t *testing.T) {
		// Five is the run that used to fail: the fifth read transaction was
		// refused with quota.ErrOverBudget because the worst case had already
		// been charged four times.
		const handful = 5
		if int64(handful) <= worstCaseCapacity {
			t.Fatalf("a handful of %d is not more than the worst-case capacity of %d, so it proves nothing", handful, worstCaseCapacity)
		}
		store, svc, owner := workloadFixture(t)
		for i := 0; i < handful; i++ {
			if _, err := svc.ListRequirementsPage(owner, "", 1); err != nil {
				t.Fatalf("read transaction %d of %d was refused: %v", i+1, handful, err)
			}
		}
		got := committedWorkloadUsage(t, store)
		want := quota.Usage{Reads: perTransaction.Reads * handful, Writes: perTransaction.Writes * handful}
		if got != want {
			t.Fatalf("%d read transactions committed %+v, want %+v", handful, got, want)
		}
		// Far below the day's budget: two orders of magnitude of headroom.
		if got.Reads*100 >= quota.DefaultBudget.Reads {
			t.Fatalf("%d read transactions committed %d reads against a budget of %d; that is not a fraction of the day", handful, got.Reads, quota.DefaultBudget.Reads)
		}
	})

	t.Run("every read transaction the settled arithmetic admits succeeds", func(t *testing.T) {
		// The bound is derived from the measured cost and the budget, not
		// chosen: the worst case is still reserved before each transaction, so
		// what fits is the headroom the worst case leaves, divided by the
		// settled cost, plus the last transaction that consumes that headroom.
		headroom := quota.DefaultBudget.Reads - quota.ReadTransactionUsage.Reads
		admitted := headroom/perTransaction.Reads + 1
		if admitted <= worstCaseCapacity {
			t.Fatalf("the settled arithmetic admits %d read transactions, no more than the %d that fitted unsettled", admitted, worstCaseCapacity)
		}
		store, svc, owner := workloadFixture(t)
		for i := int64(0); i < admitted; i++ {
			if _, err := svc.ListRequirementsPage(owner, "", 1); err != nil {
				t.Fatalf("read transaction %d of %d was refused: %v", i+1, admitted, err)
			}
		}
		got := committedWorkloadUsage(t, store)
		want := quota.Usage{Reads: perTransaction.Reads * admitted, Writes: perTransaction.Writes * admitted}
		if got != want {
			t.Fatalf("%d read transactions committed %+v, want %+v", admitted, got, want)
		}
		if got.Reads >= quota.DefaultBudget.Reads {
			t.Fatalf("the committed total %d is not below the budget %d", got.Reads, quota.DefaultBudget.Reads)
		}
		// The reservation is still made BEFORE the work, and the budget was
		// not raised: the next transaction's worst case no longer fits.
		if _, err := svc.ListRequirementsPage(owner, "", 1); !errors.Is(err, quota.ErrOverBudget) {
			t.Fatalf("read transaction %d = %v, want quota.ErrOverBudget: the worst case must still be reserved before the work", admitted+1, err)
		}
	})

	t.Run("mutations settle their reservation the same way", func(t *testing.T) {
		// The same derivation on the write component: the number of mutations
		// that used to be a whole day's capacity is now what it costs, and the
		// committed writes prove it.
		worstCaseMutations := quota.DefaultBudget.Writes / quota.MutationUsage.Writes
		if worstCaseMutations <= 0 {
			t.Fatalf("worst-case mutation capacity = %d; the budget arithmetic is broken", worstCaseMutations)
		}
		store, svc, owner := workloadFixture(t)
		var settled quota.Usage
		for i := int64(0); i < worstCaseMutations; i++ {
			if _, err := svc.Capture(owner, application.CaptureRequest{RequestID: fmt.Sprintf("many-capture-%d", i), Text: "x"}); err != nil {
				t.Fatalf("mutation %d of %d was refused: %v", i+1, worstCaseMutations, err)
			}
			if i == 0 {
				settled = committedWorkloadUsage(t, store)
			}
		}
		got := committedWorkloadUsage(t, store)
		want := quota.Usage{Reads: settled.Reads * worstCaseMutations, Writes: settled.Writes * worstCaseMutations}
		if got != want {
			t.Fatalf("%d mutations committed %+v, want %+v", worstCaseMutations, got, want)
		}
		if got.Writes >= quota.DefaultBudget.Writes {
			t.Fatalf("the committed write total %d is not below the budget %d", got.Writes, quota.DefaultBudget.Writes)
		}
		if settled.Writes >= quota.MutationUsage.Writes {
			t.Fatalf("one mutation settled to %+v, which credits nothing back against the worst case of %d writes", settled, quota.MutationUsage.Writes)
		}
	})
}
