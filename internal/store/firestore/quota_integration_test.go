package firestore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	cloudfirestore "cloud.google.com/go/firestore"

	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
	"github.com/takushi/agentic-loop-foundation/v2/internal/quota"
)

// NOTE (V2-013 / A13): every test in this file runs only against the
// Firestore emulator (emulatorStore skips otherwise). Emulator success is
// evidence about this adapter's transaction wiring, never about real
// Firestore's contention behaviour, real IAM, or real billing; V2-014 is the
// only task allowed to claim anything about a live Firestore project.

// A13(a): the worst-case reservation and its true-up commit atomically with
// the caller's mutation, in the same Firestore transaction, and the
// committed quota total reflects the actual cost, not the worst case.
func TestQuotaTrueUpCommitsAtomicallyWithCallerMutation(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	at := time.Unix(1700003000, 0).UTC()
	id, _ := domain.NewRequirementID("trueup")

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.ReserveQuota(ctx, "trueup-read", at, quota.ReadTransactionUsage); err != nil {
			return err
		}
		return u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0)
	}); err != nil {
		t.Fatal(err)
	}

	record, err := readQuotaRecord(ctx, s, at)
	if err != nil {
		t.Fatal(err)
	}
	if record.Total.Reads == 0 {
		t.Fatalf("expected the trued-up total to reflect the documents actually read, got %+v", record.Total)
	}
	if record.Total.Reads >= quota.ReadTransactionUsage.Reads {
		t.Fatalf("expected the committed total (%d) to be far below the worst-case reservation (%d); the true-up did not apply", record.Total.Reads, quota.ReadTransactionUsage.Reads)
	}

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		_, ok, err := u.Requirement(ctx, id.String())
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("requirement was not committed alongside the trued-up quota reservation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A13(c): when the worst-case reservation itself is over budget, the
// transaction is refused before it flushes, and no other staged mutation in
// the same callback is partially applied.
func TestQuotaOverBudgetTransactionLeavesNoPartialMutation(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	at := time.Unix(1700003500, 0).UTC()

	// Seed the day's committed total directly (bypassing ReserveQuota and
	// its true-up, which would otherwise correct a synthetic "fill"
	// reservation back down to its own tiny actual cost) so the next real
	// reservation's worst case is exactly one write over the line.
	if err := seedQuotaTotal(ctx, s, at, quota.Usage{Writes: quota.DefaultBudget.Writes - quota.MutationUsage.Writes + 1}); err != nil {
		t.Fatal(err)
	}

	id, _ := domain.NewRequirementID("over-budget")
	err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.ReserveQuota(ctx, "over-budget-mutation", at, quota.MutationUsage); err != nil {
			return err
		}
		// This mutation must never reach Firestore: ReserveQuota already
		// failed above and returned before this line runs, but the
		// assertion below independently confirms nothing was persisted.
		return u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0)
	})
	if !errors.Is(err, quota.ErrOverBudget) {
		t.Fatalf("expected an over-budget refusal, got %v", err)
	}

	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		_, ok, err := u.Requirement(ctx, id.String())
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("requirement was committed despite the over-budget refusal")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A13(b): a transaction callback that Firestore retries due to contention
// does not double-count quota.
//
// The emulator in this environment enforces per-document transaction locks
// (a concurrent transaction or plain write against a document an open
// transaction has read blocks, or fails with "Transaction lock timeout",
// rather than racing to an immediate ABORTED the way production Firestore's
// optimistic concurrency does), so a genuine network-level forced retry is
// not reliably reproducible here. This test instead proves the structural
// invariant that makes a real Firestore-forced retry safe: every
// RunTransaction callback invocation constructs a brand new unit with an
// empty cache and empty staged writes (see Store.Transact), and the true-up
// re-derives its correction purely from whatever is currently committed at
// that invocation. A callback that returns an error before flush() models
// exactly what a discarded, retried attempt looks like to Firestore: nothing
// it staged is ever sent, so it cannot contribute to the total a later,
// successful attempt commits.
func TestQuotaDiscardedAttemptContributesNothingToARetry(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	at := time.Unix(1700004000, 0).UTC()

	discardErr := errors.New("synthetic discard: models what Firestore does to an aborted, retried attempt")
	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *cloudfirestore.Transaction) error {
		u := &unit{store: s, tx: tx, ctx: ctx, cache: map[string]*cloudfirestore.DocumentSnapshot{}, values: map[string]pending{}}
		if err := u.ReserveQuota(ctx, "retried", at, quota.ReadTransactionUsage); err != nil {
			return err
		}
		// Never reaches flush(): exactly what an ABORTED commit discards.
		return discardErr
	})
	if !errors.Is(err, discardErr) {
		t.Fatalf("expected the synthetic discard error, got %v", err)
	}
	if _, err := readQuotaRecord(ctx, s, at); err == nil {
		t.Fatal("a discarded attempt's worst-case reservation must not be visible")
	}

	// A fresh attempt for the same key and day -- exactly what the Firestore
	// client invokes on retry -- re-reads whatever is currently committed
	// (nothing) and commits only its own trued-up cost.
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		return u.ReserveQuota(ctx, "retried", at, quota.ReadTransactionUsage)
	}); err != nil {
		t.Fatal(err)
	}
	record, err := readQuotaRecord(ctx, s, at)
	if err != nil {
		t.Fatal(err)
	}
	if record.Total.Reads == 0 || record.Total.Reads >= quota.ReadTransactionUsage.Reads {
		t.Fatalf("expected the committed total to reflect only the retried attempt's own trued-up cost, got %+v", record.Total)
	}
}

// seedQuotaTotal writes a quota record directly, outside ReserveQuota and
// its true-up, so a test can set up a precise pre-existing daily total
// without that seed itself being trued down to its own tiny actual cost.
func seedQuotaTotal(ctx context.Context, s *Store, at time.Time, total quota.Usage) error {
	ref, err := s.path("quota", quota.Day(at))
	if err != nil {
		return err
	}
	d, err := encodeDocument("quota", quotaRecord{Day: quota.Day(at), Total: total})
	if err != nil {
		return err
	}
	_, err = ref.Set(ctx, d)
	return err
}

func readQuotaRecord(ctx context.Context, s *Store, at time.Time) (quotaRecord, error) {
	var record quotaRecord
	err := s.Transact(ctx, func(u application.UnitOfWork) error {
		uw, ok := u.(*unit)
		if !ok {
			return errors.New("unexpected UnitOfWork implementation")
		}
		ref, err := uw.store.path("quota", quota.Day(at))
		if err != nil {
			return err
		}
		found, err := uw.value(ref, "quota", &record)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no quota record for day %s", quota.Day(at))
		}
		return nil
	})
	return record, err
}
