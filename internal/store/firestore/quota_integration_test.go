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

// ===========================================================================
// V2-087: the two adapters agreeing about what a reservation costs
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
// internal/store/memory/store_test.go, which carries the same table under the
// same test name over the other adapter. That duplication is deliberate and is
// the whole point: the two adapters must not disagree about what a reservation
// costs, and two literals differing is a diff a reader can see, where a shared
// helper would need a third package and a range-valued expectation would hide
// exactly the drift being guarded.
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
// are the same ids the memory package's copy of this test seeds. The two
// adapters order a page differently -- this one by the base64url document key,
// that one by the raw domain id -- so the two pages may hold different rows;
// the COUNT of documents a Limit(limit+1) query touches is what this table is
// about, and that is the same on both.
var crossAdapterWorkloadIDs = []string{"wl-a", "wl-b", "wl-c"}

// crossAdapterWorkloadInstant is the single fixed injected instant every row
// runs on, identical in both packages. Nothing advances it.
func crossAdapterWorkloadInstant() time.Time { return time.Unix(1700000000, 0).UTC() }

// workloadFixture seeds the shared fixture and returns the store, a service on
// the fixed instant, and an owner context. The seeding transactions call no
// ReserveQuota at all -- they go straight through the port -- so they settle
// nothing and leave no quota document: the committed total read back after a
// workload row is exactly that row's own settled cost.
func workloadFixture(t *testing.T) (*Store, *application.Service, context.Context) {
	t.Helper()
	s := emulatorStore(t)
	ctx := context.Background()
	for _, raw := range crossAdapterWorkloadIDs {
		id, err := domain.NewRequirementID(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Transact(ctx, func(u application.UnitOfWork) error {
			if e := u.SaveRequirement(ctx, domain.Requirement{ID: id, Version: 1, Status: domain.RequirementCaptured}, 0); e != nil {
				return e
			}
			return u.SaveRequirementText(ctx, raw, "text-"+raw)
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := application.NewServiceWithConfig(s, integrationClock{now: crossAdapterWorkloadInstant()}, &integrationIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s, svc, application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "workload-owner"})
}

// committedWorkloadUsage reads the day's committed total back out of the quota
// document. A day with no document at all is a failure, not a zero total: it
// would mean the workload reserved nothing.
func committedWorkloadUsage(t *testing.T, s *Store) quota.Usage {
	t.Helper()
	record, err := readQuotaRecord(context.Background(), s, crossAdapterWorkloadInstant())
	if err != nil {
		t.Fatalf("no quota record for the workload day: the transaction reserved nothing (%v)", err)
	}
	return record.Total
}

// TestBothAdaptersSettleTheSameReservationForTheSameWorkload is A6. The memory
// package carries this exact test name over its own adapter, with the same four
// rows and the same literal expectations. This half runs only under
// scripts/firestore-emulator.sh: without the emulator it skips and measures
// nothing, and a skipped case is never a pass (gate rule G7).
func TestBothAdaptersSettleTheSameReservationForTheSameWorkload(t *testing.T) {
	runners := []func(t *testing.T) quota.Usage{
		func(t *testing.T) quota.Usage {
			s, svc, owner := workloadFixture(t)
			if _, err := svc.ListRequirementsPage(owner, "", 1); err != nil {
				t.Fatal(err)
			}
			return committedWorkloadUsage(t, s)
		},
		func(t *testing.T) quota.Usage {
			s, svc, owner := workloadFixture(t)
			if _, err := svc.ListRequirementsPage(owner, "", 2); err != nil {
				t.Fatal(err)
			}
			return committedWorkloadUsage(t, s)
		},
		func(t *testing.T) quota.Usage {
			s, svc, owner := workloadFixture(t)
			if _, ok, err := svc.GetRequirementDetail(owner, crossAdapterWorkloadIDs[0]); err != nil {
				t.Fatal(err)
			} else if !ok {
				t.Fatal("the seeded Requirement was not found")
			}
			return committedWorkloadUsage(t, s)
		},
		func(t *testing.T) quota.Usage {
			s := emulatorStore(t)
			ctx := context.Background()
			svc, err := application.NewServiceWithConfig(s, integrationClock{now: crossAdapterWorkloadInstant()}, &integrationIDs{}, application.ServiceConfig{InstallationID: "install", LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			owner := application.ContextWithCaller(ctx, application.Caller{Role: application.RoleOwner, Subject: "workload-owner"})
			if _, err := svc.Capture(owner, application.CaptureRequest{RequestID: "workload-capture", Text: "x"}); err != nil {
				t.Fatal(err)
			}
			return committedWorkloadUsage(t, s)
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
				t.Fatalf("%s settled to %+v on the Firestore adapter, want %+v. If the memory half of this test measured %+v for the same row, the two adapters disagree about what a reservation costs: report both measurements and stop -- do not adjust either literal, delete the row, or widen the expectation into a range", row.name, got, row.usage, got)
			}
			if got.Reads >= quota.ReadTransactionUsage.Reads {
				t.Fatalf("%s settled to %+v, which is not below the worst-case read reservation (%d): nothing was credited back", row.name, got, quota.ReadTransactionUsage.Reads)
			}
		})
	}
}
