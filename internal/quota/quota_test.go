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
