// Package quota contains the local, deterministic policy used to reserve the
// Firestore daily free-tier budget before a mutation is committed.
package quota

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const Shards = 32

var ErrOverBudget = errors.New("daily Firestore quota reservation exceeds the hard budget")

// Usage is the reserved Firestore operation cost of one transaction. The
// reservation is deliberately made before the first mutation is staged.
type Usage struct {
	Reads   int64
	Writes  int64
	Deletes int64
}

func (u Usage) valid() bool { return u.Reads >= 0 && u.Writes >= 0 && u.Deletes >= 0 }

// Budget is the 80% reservation of Google's default Firestore free tier. The
// remaining 20% is an operational safety margin for indexes, retries, and
// manual inspection.
type Budget struct {
	Reads   int64
	Writes  int64
	Deletes int64
}

var DefaultBudget = Budget{Reads: 40_000, Writes: 16_000, Deletes: 16_000}

// Firestore adapters cap a bounded query at 1000 documents and fetch one
// extra document to determine whether another page exists. A detail read can
// fan out to one increment query and up to four execution IN queries, so the
// read boundary reserves 6,000 application reads plus the quota document IO.
// This is intentionally conservative even when the actual page is small.
const MaxBoundedQueryReads int64 = 1001
const MaxReadBoundaryReads int64 = 6000

var ReadTransactionUsage = Usage{Reads: MaxReadBoundaryReads + 1, Writes: 1}

// A mutation may read the idempotency record, aggregate/control state,
// canonical target, lease/execution, and bounded query state, then write the
// aggregate, event, outbox, idempotency record, and quota record. These are
// intentionally conservative boundary maxima, not averages.
var MutationUsage = Usage{Reads: 32, Writes: 16}

type Counter struct {
	Day    string
	Total  Usage
	Shards [Shards]Usage
}

func Day(at time.Time) string { return at.UTC().Format("2006-01-02") }

func Shard(key string) int {
	h := sha256.Sum256([]byte(key))
	return int(binary.BigEndian.Uint32(h[:4]) % Shards)
}

// Reserve atomically accounts for usage in the daily aggregate and one of 32
// accounting buckets. The aggregate is the sole hard-budget source of truth;
// buckets are audit dimensions in the same document, not contention shards.
func (c *Counter) Reserve(at time.Time, key string, usage Usage, budget Budget) error {
	if at.IsZero() || key == "" || !usage.valid() || budget.Reads < 0 || budget.Writes < 0 || budget.Deletes < 0 {
		return errors.New("invalid quota reservation")
	}
	day := Day(at)
	if c.Day != day {
		*c = Counter{Day: day}
	}
	next := Usage{Reads: c.Total.Reads + usage.Reads, Writes: c.Total.Writes + usage.Writes, Deletes: c.Total.Deletes + usage.Deletes}
	if next.Reads > budget.Reads || next.Writes > budget.Writes || next.Deletes > budget.Deletes {
		return fmt.Errorf("%w: day=%s reads=%d/%d writes=%d/%d deletes=%d/%d", ErrOverBudget, day, next.Reads, budget.Reads, next.Writes, budget.Writes, next.Deletes, budget.Deletes)
	}
	c.Total = next
	shard := Shard(key)
	c.Shards[shard].Reads += usage.Reads
	c.Shards[shard].Writes += usage.Writes
	c.Shards[shard].Deletes += usage.Deletes
	return nil
}
