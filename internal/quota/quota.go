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

// valid rejects a negative component and rejects an all-zero usage: a
// reservation that costs nothing is either a bug or a way to bypass the
// budget, so it is refused rather than silently accepted.
func (u Usage) valid() bool {
	if u.Reads < 0 || u.Writes < 0 || u.Deletes < 0 {
		return false
	}
	return u.Reads > 0 || u.Writes > 0 || u.Deletes > 0
}

// Budget is the hard daily ceiling enforced before any mutation is staged.
// It is fixed at 50% of the Firestore always-free daily allowance recorded
// in docs/operations/gcp-runbook.md (50,000 reads / 20,000 writes / 20,000
// deletes per day), leaving the remaining 50% as headroom the control plane
// never claims.
type Budget struct {
	Reads   int64
	Writes  int64
	Deletes int64
}

var DefaultBudget = Budget{Reads: 25_000, Writes: 10_000, Deletes: 10_000}

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
// The daily aggregate resets whenever the UTC day changes.
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

func clampComponent(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func subtractFloor(v, credit Usage) Usage {
	v.Reads -= credit.Reads
	v.Writes -= credit.Writes
	v.Deletes -= credit.Deletes
	if v.Reads < 0 {
		v.Reads = 0
	}
	if v.Writes < 0 {
		v.Writes = 0
	}
	if v.Deletes < 0 {
		v.Deletes = 0
	}
	return v
}

// TrueUp corrects an already-applied worst-case Reserve for key down to the
// actual usage a transaction incurred, crediting back the unused portion of
// the reservation before the transaction flushes. actual is clamped
// component-wise to [0, reserved], so the correction can only ever credit
// back unused reservation: the trued-up total can never fall below the
// pre-reservation total (it is floored at zero) and can never exceed the
// pre-checked worst case that Reserve already admitted (it never grows).
// key must be the same key used for the original Reserve call so the credit
// lands in the same accounting shard.
func (c *Counter) TrueUp(key string, reserved, actual Usage) {
	a := Usage{
		Reads:   clampComponent(actual.Reads, 0, reserved.Reads),
		Writes:  clampComponent(actual.Writes, 0, reserved.Writes),
		Deletes: clampComponent(actual.Deletes, 0, reserved.Deletes),
	}
	credit := Usage{
		Reads:   reserved.Reads - a.Reads,
		Writes:  reserved.Writes - a.Writes,
		Deletes: reserved.Deletes - a.Deletes,
	}
	c.Total = subtractFloor(c.Total, credit)
	shard := Shard(key)
	c.Shards[shard] = subtractFloor(c.Shards[shard], credit)
}

// StorageBudget is the ceiling on stored Firestore bytes. Unlike Budget, it
// bounds a level, not a daily flow. TotalBytes documents Firestore's always
// -free stored-data allowance, which also bills index storage; PayloadBytes
// is the enforced ceiling on document payload bytes because this design
// assumes at most a 2x index-storage overhead on top of payload bytes.
type StorageBudget struct {
	PayloadBytes int64
	TotalBytes   int64
}

var DefaultStorageBudget = StorageBudget{PayloadBytes: 268_435_456, TotalBytes: 536_870_912}

var ErrStorageOverBudget = errors.New("stored Firestore payload bytes exceed the hard budget")

// StorageLevel is the accumulated stored payload bytes across all documents.
// It is a level, not a flow: it is never reset at a UTC day change; only an
// explicit ReserveBytes or ReleaseBytes call changes it.
type StorageLevel struct {
	Bytes int64
}

// ReserveBytes fails closed if adding delta would push the level over
// budget.PayloadBytes. delta must be strictly positive; a negative or zero
// -valued reservation is rejected the same way Counter.Reserve rejects one.
func (l *StorageLevel) ReserveBytes(delta int64, budget StorageBudget) error {
	if delta <= 0 || budget.PayloadBytes < 0 {
		return errors.New("invalid storage reservation")
	}
	next := l.Bytes + delta
	if next > budget.PayloadBytes {
		return fmt.Errorf("%w: bytes=%d/%d", ErrStorageOverBudget, next, budget.PayloadBytes)
	}
	l.Bytes = next
	return nil
}

// ReleaseBytes credits back bytes freed by a delete. It is floored at zero
// and, like ReserveBytes, rejects a non-positive delta as invalid.
func (l *StorageLevel) ReleaseBytes(delta int64) error {
	if delta <= 0 {
		return errors.New("invalid storage release")
	}
	l.Bytes -= delta
	if l.Bytes < 0 {
		l.Bytes = 0
	}
	return nil
}
