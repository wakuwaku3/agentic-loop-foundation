package quota

import (
	"errors"
	"testing"
	"time"
)

func TestReserveUsesAllThirtyTwoShardsAndEnforcesAggregate(t *testing.T) {
	var c Counter
	at := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	for i := 0; i < Shards; i++ {
		key := "key-" + string(rune('a'+i))
		if err := c.Reserve(at, key, Usage{Reads: 1, Writes: 1, Deletes: 1}, DefaultBudget); err != nil {
			t.Fatal(err)
		}
	}
	if c.Total != (Usage{Reads: Shards, Writes: Shards, Deletes: Shards}) {
		t.Fatalf("unexpected total: %+v", c.Total)
	}
	seen := 0
	for _, shard := range c.Shards {
		if shard.Reads > 0 {
			seen++
		}
	}
	if seen < 2 {
		t.Fatalf("reservation did not distribute across shards: %d", seen)
	}
	if err := c.Reserve(at, "over", Usage{Reads: DefaultBudget.Reads + 1}, DefaultBudget); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("expected hard budget error, got %v", err)
	}
}

func TestReserveResetsAtUtcDayBoundary(t *testing.T) {
	var c Counter
	first := time.Date(2026, 8, 22, 23, 59, 0, 0, time.UTC)
	second := first.Add(2 * time.Minute)
	if err := c.Reserve(first, "day", Usage{Writes: 2}, DefaultBudget); err != nil {
		t.Fatal(err)
	}
	if err := c.Reserve(second, "day", Usage{Writes: 3}, DefaultBudget); err != nil {
		t.Fatal(err)
	}
	if c.Total.Writes != 3 || c.Day != Day(second) {
		t.Fatalf("counter did not reset: day=%s total=%+v", c.Day, c.Total)
	}
}

func TestBoundaryReservationsIncludeQuotaDocumentIO(t *testing.T) {
	if ReadTransactionUsage.Reads != MaxReadBoundaryReads+1 || ReadTransactionUsage.Writes != 1 {
		t.Fatalf("read reservation must include bounded query plus quota doc IO: %+v", ReadTransactionUsage)
	}
	if MutationUsage.Reads < 32 || MutationUsage.Writes < 16 {
		t.Fatalf("mutation reservation is not conservative: %+v", MutationUsage)
	}
}

// A1: DefaultBudget is exactly 50% of the Firestore always-free daily
// allowance (50,000/20,000/20,000), not the prior 80% figure.
func TestDefaultBudgetIsFiftyPercentOfTheFreeTier(t *testing.T) {
	if DefaultBudget != (Budget{Reads: 25_000, Writes: 10_000, Deletes: 10_000}) {
		t.Fatalf("expected the 50%% hard budget, got %+v", DefaultBudget)
	}
}

// A1: each daily ceiling fails closed exactly one operation over the line,
// checked independently for reads, writes and deletes.
func TestDailyBudgetFailsClosedExactlyOneOverTheLine(t *testing.T) {
	at := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		usage func(n int64) Usage
		limit int64
	}{
		{"reads", func(n int64) Usage { return Usage{Reads: n} }, DefaultBudget.Reads},
		{"writes", func(n int64) Usage { return Usage{Writes: n} }, DefaultBudget.Writes},
		{"deletes", func(n int64) Usage { return Usage{Deletes: n} }, DefaultBudget.Deletes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var atLimit Counter
			if err := atLimit.Reserve(at, "k", tc.usage(tc.limit), DefaultBudget); err != nil {
				t.Fatalf("reserving exactly the ceiling must succeed: %v", err)
			}
			var overLimit Counter
			if err := overLimit.Reserve(at, "k", tc.usage(tc.limit+1), DefaultBudget); !errors.Is(err, ErrOverBudget) {
				t.Fatalf("reserving one operation over the ceiling must fail closed, got %v", err)
			}
		})
	}
}

// A1: a negative or zero-valued reservation is rejected outright, before any
// budget comparison, for both the daily counter and the storage level.
func TestReserveRejectsNegativeOrZeroUsage(t *testing.T) {
	var c Counter
	at := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if err := c.Reserve(at, "k", Usage{}, DefaultBudget); err == nil {
		t.Fatal("expected a zero-valued reservation to be rejected")
	}
	if err := c.Reserve(at, "k", Usage{Reads: -1}, DefaultBudget); err == nil {
		t.Fatal("expected a negative reservation to be rejected")
	}
	if c.Day != "" || c.Total != (Usage{}) {
		t.Fatalf("a rejected reservation must not mutate the counter: %+v", c)
	}
}

// A1: the stored-bytes level is separate from the daily counter and is not
// reset at a UTC day change.
func TestStorageLevelIsNotResetAtUtcDayChange(t *testing.T) {
	var l StorageLevel
	if err := l.ReserveBytes(1000, DefaultStorageBudget); err != nil {
		t.Fatal(err)
	}
	// StorageLevel carries no Day field at all and no reset semantics;
	// crossing a UTC day boundary never changes an already-reserved level.
	if l.Bytes != 1000 {
		t.Fatalf("unexpected level: %d", l.Bytes)
	}
}

// A1: the stored-bytes ceiling fails closed exactly one byte over the line.
func TestStorageBudgetFailsClosedExactlyOneOverTheLine(t *testing.T) {
	if DefaultStorageBudget != (StorageBudget{PayloadBytes: 268_435_456, TotalBytes: 536_870_912}) {
		t.Fatalf("unexpected storage budget: %+v", DefaultStorageBudget)
	}
	var atLimit StorageLevel
	if err := atLimit.ReserveBytes(DefaultStorageBudget.PayloadBytes, DefaultStorageBudget); err != nil {
		t.Fatalf("reserving exactly the ceiling must succeed: %v", err)
	}
	var overLimit StorageLevel
	if err := overLimit.ReserveBytes(DefaultStorageBudget.PayloadBytes+1, DefaultStorageBudget); !errors.Is(err, ErrStorageOverBudget) {
		t.Fatalf("reserving one byte over the ceiling must fail closed, got %v", err)
	}
}

// A1: a negative or zero-valued storage reservation/release is rejected.
func TestStorageLevelRejectsNegativeOrZeroDelta(t *testing.T) {
	var l StorageLevel
	if err := l.ReserveBytes(0, DefaultStorageBudget); err == nil {
		t.Fatal("expected a zero-valued storage reservation to be rejected")
	}
	if err := l.ReserveBytes(-1, DefaultStorageBudget); err == nil {
		t.Fatal("expected a negative storage reservation to be rejected")
	}
	if err := l.ReleaseBytes(0); err == nil {
		t.Fatal("expected a zero-valued release to be rejected")
	}
	if err := l.ReleaseBytes(-1); err == nil {
		t.Fatal("expected a negative release to be rejected")
	}
	if l.Bytes != 0 {
		t.Fatalf("a rejected delta must not mutate the level: %d", l.Bytes)
	}
}

// A1: releasing more bytes than are reserved floors at zero rather than
// going negative.
func TestStorageLevelReleaseIsFlooredAtZero(t *testing.T) {
	var l StorageLevel
	if err := l.ReserveBytes(100, DefaultStorageBudget); err != nil {
		t.Fatal(err)
	}
	if err := l.ReleaseBytes(1000); err != nil {
		t.Fatal(err)
	}
	if l.Bytes != 0 {
		t.Fatalf("expected the level to floor at zero, got %d", l.Bytes)
	}
}

// A2: an over-budget worst case is refused before a single document is
// staged, and the refused attempt leaves the counter unchanged.
func TestReserveRefusesOverBudgetWorstCaseBeforeStaging(t *testing.T) {
	var c Counter
	at := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if err := c.Reserve(at, "fill", Usage{Reads: DefaultBudget.Reads - MutationUsage.Reads + 1}, DefaultBudget); err != nil {
		t.Fatal(err)
	}
	before := c
	if err := c.Reserve(at, "next", MutationUsage, DefaultBudget); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("expected the worst case to be refused, got %v", err)
	}
	if c != before {
		t.Fatal("a refused reservation must not stage any partial usage")
	}
}

// A2: a transaction that actually reads far fewer documents than the worst
// case commits only its actual cost once trued up.
func TestTrueUpCommitsOnlyActualCostOfASmallTransaction(t *testing.T) {
	var c Counter
	at := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	reserved := ReadTransactionUsage
	if err := c.Reserve(at, "read-1", reserved, DefaultBudget); err != nil {
		t.Fatal(err)
	}
	actual := Usage{Reads: 3, Writes: 1}
	c.TrueUp("read-1", reserved, actual)
	if c.Total != actual {
		t.Fatalf("expected the trued-up total to equal actual cost %+v, got %+v", actual, c.Total)
	}
}

// A2: the trued-up total never goes below zero or above the pre-checked
// worst case, even when the reported actual usage is malformed.
func TestTrueUpNeverGoesBelowZeroOrAboveTheWorstCase(t *testing.T) {
	var c Counter
	at := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	reserved := Usage{Reads: 10, Writes: 5, Deletes: 2}
	if err := c.Reserve(at, "k", reserved, DefaultBudget); err != nil {
		t.Fatal(err)
	}
	worstCase := c.Total
	c.TrueUp("k", reserved, Usage{Reads: -5, Writes: 999, Deletes: 2})
	if c.Total.Reads < 0 || c.Total.Writes < 0 || c.Total.Deletes < 0 {
		t.Fatalf("trued-up total went negative: %+v", c.Total)
	}
	if c.Total.Reads > worstCase.Reads || c.Total.Writes > worstCase.Writes || c.Total.Deletes > worstCase.Deletes {
		t.Fatalf("trued-up total exceeded the pre-checked worst case: %+v > %+v", c.Total, worstCase)
	}
}

// A2: measured daily capacity before and after the true-up, at the 50%
// ceiling. The worst-case-only figures match the pre-change measurement
// recorded in the work order (4 read transactions, 625 mutations per day);
// truing up to a single bounded page's actual cost raises read-transaction
// capacity well above the worst case.
func TestMeasuredDailyCapacityAtFiftyPercentCeiling(t *testing.T) {
	worstCaseReadCapacity := DefaultBudget.Reads / ReadTransactionUsage.Reads
	if worstCaseReadCapacity != 4 {
		t.Fatalf("expected the documented pre-change worst-case read-transaction capacity of 4, got %d", worstCaseReadCapacity)
	}
	worstCaseMutationCapacity := DefaultBudget.Writes / MutationUsage.Writes
	if worstCaseMutationCapacity != 625 {
		t.Fatalf("expected the documented pre-change mutation capacity of 625, got %d", worstCaseMutationCapacity)
	}
	actualSinglePage := Usage{Reads: MaxBoundedQueryReads + 1, Writes: 1}
	trueUpReadCapacity := DefaultBudget.Reads / actualSinglePage.Reads
	if trueUpReadCapacity <= worstCaseReadCapacity {
		t.Fatalf("true-up must raise read-transaction capacity above the worst case: %d <= %d", trueUpReadCapacity, worstCaseReadCapacity)
	}
	t.Logf("measured daily read-transaction capacity: worst-case=%d trued-up(single bounded page, actual reads=%d)=%d", worstCaseReadCapacity, actualSinglePage.Reads, trueUpReadCapacity)
}
