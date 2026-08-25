package memory

// Memory-adapter closure of the Repository port (V2-064). This package had
// no test file before this task; these tests are deterministic and need no
// external process, so they are admissible under the repository's standing
// determinism rule.

import (
	"context"
	"errors"
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
