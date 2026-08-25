package update

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

// fakeCodec is the in-package fake behind the injected codec port. It models
// the measured codec's shape -- {record_schema, kind, payload} with
// unknown-field tolerance -- and nothing else: no emulator, no store, no
// network. What the four stages prove is therefore proven against this port,
// and not yet against the emulator; the envelope widening is V2-070's.
type fakeCodec struct {
	records map[string]Record
	puts    int
	// batch caps how many records one List call returns, so a test can
	// exercise a page smaller than the caller's limit -- which is what any
	// real paged query does.
	batch int
}

func newFakeCodec(count int, oldField string) *fakeCodec {
	c := &fakeCodec{records: map[string]Record{}}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("doc-%04d", i)
		c.records[id] = Record{ID: id, Envelope: "v1", Kind: "requirement", Payload: map[string]string{oldField: fmt.Sprintf("value-%d", i)}}
	}
	return c
}

func (c *fakeCodec) Get(id string) (Record, bool, error) {
	record, ok := c.records[id]
	if !ok {
		return Record{}, false, nil
	}
	return record.Clone(), true, nil
}

func (c *fakeCodec) Put(record Record) error {
	c.puts++
	c.records[record.ID] = record.Clone()
	return nil
}

func (c *fakeCodec) List(afterID string, limit int) ([]Record, error) {
	ids := make([]string, 0, len(c.records))
	for id := range c.records {
		if id > afterID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if c.batch > 0 && c.batch < limit {
		limit = c.batch
	}
	if limit < len(ids) {
		ids = ids[:limit]
	}
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.records[id].Clone())
	}
	return out, nil
}

func (c *fakeCodec) snapshot() map[string]Record {
	out := map[string]Record{}
	for id, record := range c.records {
		out[id] = record.Clone()
	}
	return out
}

func (c *fakeCodec) ids() []string {
	ids := make([]string, 0, len(c.records))
	for id := range c.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// stepClock is an injected clock that advances a fixed step on every read.
// There is no sleep, no timer and no goroutine anywhere in these tests.
func stepClock(start time.Time, step time.Duration) func() time.Time {
	current := start
	return func() time.Time {
		now := current
		current = current.Add(step)
		return now
	}
}

// TestFourStagesAreReversibleUntilContract walks expand, coexist, migrate
// and contract over the injected port and pins the reversibility boundary:
// the first three restore the store byte for byte, and contract is the first
// and only irreversible stage.
func TestFourStagesAreReversibleUntilContract(t *testing.T) {
	migration := FieldMigration{OldField: "requested_by", NewField: "requested_by_actor"}
	codec := newFakeCodec(5, migration.OldField)
	original := codec.snapshot()

	// EXPAND: the new field in addition to the old one, envelope unchanged.
	for _, id := range codec.ids() {
		if err := Expand(codec, id, migration); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range codec.records {
		if record.Envelope != "v1" {
			t.Fatalf("expand changed the envelope: %+v", record)
		}
		if record.Payload[migration.OldField] == "" || record.Payload[migration.NewField] != record.Payload[migration.OldField] {
			t.Fatalf("expand did not write both fields: %+v", record)
		}
	}
	// COEXIST: every reader resolves new-if-present, else old.
	both, _, _ := codec.Get("doc-0000")
	if value, ok := Resolve(both, migration); !ok || value != "value-0" {
		t.Fatalf("coexist resolution = %q %t", value, ok)
	}
	oldOnly := Record{ID: "x", Payload: map[string]string{migration.OldField: "legacy"}}
	if value, ok := Resolve(oldOnly, migration); !ok || value != "legacy" {
		t.Fatalf("coexist fallback = %q %t", value, ok)
	}
	if err := Reverse(codec, StageExpand, migration, codec.ids()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(codec.snapshot(), original) {
		t.Fatal("expand is not reversible")
	}
	t.Logf("verdict: expand and coexist reversed to the original store exactly")

	// MIGRATE: bounded, resumable and idempotent over 250 documents.
	big := newFakeCodec(250, migration.OldField)
	preMigrate := big.snapshot()
	budget := DefaultMigrateBudget(stepClock(baseTime, time.Second))
	cursor := ""
	ticks := 0
	rewritten := 0
	for {
		result, err := Migrate(big, migration, cursor, budget)
		if err != nil {
			t.Fatal(err)
		}
		ticks++
		rewritten += result.Rewritten
		cursor = result.Cursor
		if result.Scanned > budget.MaxDocuments {
			t.Fatalf("tick %d scanned %d, over the document bound", ticks, result.Scanned)
		}
		if result.Done {
			break
		}
		if ticks > 10 {
			t.Fatal("the backfill did not converge inside a bounded number of ticks")
		}
	}
	if ticks != 3 || rewritten != 250 {
		t.Fatalf("ticks=%d rewritten=%d, want 3 and 250", ticks, rewritten)
	}
	t.Logf("verdict: backfill converged in %d bounded ticks, %d documents rewritten", ticks, rewritten)
	// Idempotent: a repeated tick from the start rewrites nothing.
	repeat, err := Migrate(big, migration, "", DefaultMigrateBudget(stepClock(baseTime, time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Rewritten != 0 {
		t.Fatalf("a repeated tick rewrote %d documents", repeat.Rewritten)
	}
	// The duration bound stops a tick as surely as the document bound, and
	// on the injected clock only.
	paged := newFakeCodec(250, migration.OldField)
	paged.batch = 10
	bounded, err := Migrate(paged, migration, "", MigrateBudget{MaxDocuments: 100, MaxDuration: 30 * time.Second, Now: stepClock(baseTime, 20*time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if bounded.StoppedBy != "duration bound" || bounded.Scanned != 10 || bounded.Done {
		t.Fatalf("stopped by %q after %d documents (done=%t), want the duration bound after 10", bounded.StoppedBy, bounded.Scanned, bounded.Done)
	}
	// A budget with no bound or no injected clock is refused outright.
	for _, bad := range []MigrateBudget{{MaxDocuments: 0, MaxDuration: time.Second, Now: stepClock(baseTime, 0)}, {MaxDocuments: 1, MaxDuration: 0, Now: stepClock(baseTime, 0)}, {MaxDocuments: 1, MaxDuration: time.Second}} {
		if _, err := Migrate(paged, migration, "", bad); err == nil {
			t.Fatalf("unbounded or clock-free budget accepted: %+v", bad)
		}
	}
	if err := Reverse(big, StageMigrate, migration, big.ids()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(big.snapshot(), preMigrate) {
		t.Fatal("migrate is not reversible")
	}
	t.Logf("verdict: migrate reversed to the pre-backfill store exactly")

	// CONTRACT: the first irreversible stage.
	for i, stage := range Stages {
		want := i < 3
		if stage.Reversible() != want {
			t.Fatalf("stage %s reversible = %t, want %t", stage, stage.Reversible(), want)
		}
	}
	contractCodec := newFakeCodec(3, migration.OldField)
	for _, id := range contractCodec.ids() {
		if err := Expand(contractCodec, id, migration); err != nil {
			t.Fatal(err)
		}
	}
	retained := []VersionRetention{{Version: "2.0.0", SchemaMin: 3, Retention: RetentionInput{Version: "2.0.0", CurrentStable: "2.0.0"}}}
	if err := Contract(contractCodec, migration, 3, retained, contractCodec.ids()); err != nil {
		t.Fatal(err)
	}
	for _, record := range contractCodec.records {
		if _, present := record.Payload[migration.OldField]; present {
			t.Fatalf("contract did not delete the old field: %+v", record)
		}
		if record.Envelope != "v1" {
			t.Fatalf("contract touched the envelope, which belongs to V2-070: %+v", record)
		}
	}
	if err := Reverse(contractCodec, StageContract, migration, contractCodec.ids()); !errors.Is(err, ErrIrreversibleStage) {
		t.Fatalf("contract reversal err = %v, want ErrIrreversibleStage", err)
	}
	// And concretely irreversible: even the expand reversal, which worked
	// before, now has nothing to fall back to.
	if err := Reverse(contractCodec, StageExpand, migration, contractCodec.ids()); err == nil {
		t.Fatal("a post-contract store was reversed")
	} else {
		t.Logf("verdict: post-contract reversal refused: %v", err)
	}
	// Contract before expand is refused, because contract is always a
	// separate Increment from its expand.
	if err := Contract(newFakeCodec(1, migration.OldField), migration, 3, retained, []string{"doc-0000"}); err == nil {
		t.Fatal("contract ran on records that never carried the new field")
	}
}

// TestContractUsesTheSameRetentionPredicateAsGC pins section 7.2: "may I
// contract?" and "may I delete?" are one predicate used twice. Each row feeds
// the same RetentionInput to RetentionEligible and to ContractAllowed, and
// the contract verdict follows from the retention verdict plus the version's
// schema floor -- so the two cannot disagree.
func TestContractUsesTheSameRetentionPredicateAsGC(t *testing.T) {
	post := 3
	rows := []struct {
		name         string
		version      VersionRetention
		wantEligible bool
		wantContract bool
	}{
		{
			name:         "a current stable target below the post-contract schema blocks the contract",
			version:      VersionRetention{Version: "1.0.0", SchemaMin: 1, Retention: RetentionInput{Version: "1.0.0", CurrentStable: "1.0.0"}},
			wantEligible: false,
			wantContract: false,
		},
		{
			name:         "a previous stable target below the post-contract schema blocks it too",
			version:      VersionRetention{Version: "0.9.0", SchemaMin: 2, Retention: RetentionInput{Version: "0.9.0", PreviousStable: "0.9.0"}},
			wantEligible: false,
			wantContract: false,
		},
		{
			name:         "a retained version at or above the post-contract schema does not block it",
			version:      VersionRetention{Version: "2.0.0", SchemaMin: 3, Retention: RetentionInput{Version: "2.0.0", CurrentStable: "2.0.0"}},
			wantEligible: false,
			wantContract: true,
		},
		{
			name:         "a GC-eligible version constrains nothing, however low its floor",
			version:      VersionRetention{Version: "0.1.0", SchemaMin: 1, Retention: RetentionInput{Version: "0.1.0", CurrentStable: "2.0.0", PreviousStable: "1.0.0", Now: at(600)}},
			wantEligible: true,
			wantContract: true,
		},
		{
			name:         "a version still inside its rollback window blocks it",
			version:      VersionRetention{Version: "0.2.0", SchemaMin: 1, Retention: RetentionInput{Version: "0.2.0", CurrentStable: "2.0.0", RolledBackAt: at(0), RollbackWindow: time.Hour, Now: at(30)}},
			wantEligible: false,
			wantContract: false,
		},
		{
			name:         "a version a Requirement still references blocks it",
			version:      VersionRetention{Version: "0.3.0", SchemaMin: 1, Retention: RetentionInput{Version: "0.3.0", CurrentStable: "2.0.0", ReferencedByRequirement: true}},
			wantEligible: false,
			wantContract: false,
		},
		{
			name:         "a version a preview symlink still points at blocks it",
			version:      VersionRetention{Version: "0.4.0", SchemaMin: 1, Retention: RetentionInput{Version: "0.4.0", CurrentStable: "2.0.0", ChannelTargets: []string{"0.4.0"}}},
			wantEligible: false,
			wantContract: false,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			eligible, reason := RetentionEligible(row.version.Retention)
			if eligible != row.wantEligible {
				t.Fatalf("RetentionEligible = (%t, %q), want %t", eligible, reason, row.wantEligible)
			}
			allowed, why := ContractAllowed(post, []VersionRetention{row.version})
			if allowed != row.wantContract {
				t.Fatalf("ContractAllowed = (%t, %q), want %t", allowed, why, row.wantContract)
			}
			// The invariant that makes it one predicate rather than two:
			// contract is refused exactly when a version is NOT GC-eligible
			// and its floor is below the post-contract schema.
			expected := !(!eligible && row.version.SchemaMin < post)
			if allowed != expected {
				t.Fatalf("the contract verdict (%t) does not follow from the retention verdict (%t) and schema_min %d", allowed, eligible, row.version.SchemaMin)
			}
			t.Logf("verdict: eligible=%t contract-allowed=%t (%s)", eligible, allowed, why)
		})
	}

	// The whole set together: one blocking version is enough, and removing
	// its blocking cause -- by making it GC-eligible -- unblocks the stage
	// without any second rule being consulted.
	all := make([]VersionRetention, 0, len(rows))
	for _, row := range rows {
		all = append(all, row.version)
	}
	if allowed, why := ContractAllowed(post, all); allowed {
		t.Fatal("the contract stage ran with routable versions below the post-contract schema")
	} else {
		t.Logf("verdict whole-set: refused: %s", why)
	}
	if allowed, _ := ContractAllowed(0, all); allowed {
		t.Fatal("a non-positive post-contract schema was accepted")
	}
	migration := FieldMigration{OldField: "requested_by", NewField: "requested_by_actor"}
	codec := newFakeCodec(2, migration.OldField)
	for _, id := range codec.ids() {
		if err := Expand(codec, id, migration); err != nil {
			t.Fatal(err)
		}
	}
	before := codec.snapshot()
	if err := Contract(codec, migration, post, all, codec.ids()); !errors.Is(err, ErrContractRefused) {
		t.Fatalf("err = %v, want ErrContractRefused", err)
	}
	if !reflect.DeepEqual(codec.snapshot(), before) {
		t.Fatal("a refused contract still rewrote the store")
	}
}
