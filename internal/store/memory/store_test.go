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
