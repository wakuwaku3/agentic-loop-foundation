// The four schema stages -- expand, coexist, migrate, contract -- over an
// injected codec port (docs/operations/self-update.md section 7).
//
// The port is deliberately a port. The envelope half of the story belongs to
// internal/store/firestore, whose two readers compare record_schema with !=
// and whose widening to an accepted set is escalation E2, carved out as
// V2-070. V2-034 must not edit anything under internal/store/firestore, so
// the claim this file is entitled to is exactly: the four stages are proven
// against this port with an in-package fake, and not yet against the
// emulator.
//
// The measured codec this port models is {record_schema, kind, payload} with
// the payload stored as a JSON string: unknown payload fields are ignored and
// absent payload fields take their zero value, which is precisely why expand
// needs no reader change.
package update

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Record is one stored document in the shape the stages operate on.
type Record struct {
	ID       string
	Envelope string
	Kind     string
	Payload  map[string]string
}

// Clone is a deep copy, so a caller comparing a before/after snapshot is
// comparing values and not aliases.
func (r Record) Clone() Record {
	payload := make(map[string]string, len(r.Payload))
	for k, v := range r.Payload {
		payload[k] = v
	}
	return Record{ID: r.ID, Envelope: r.Envelope, Kind: r.Kind, Payload: payload}
}

// Codec is the injected port. Nothing in this package implements it: the
// production implementation is the store, and the test implementation is an
// in-package fake.
type Codec interface {
	// Get returns one record by id.
	Get(id string) (Record, bool, error)
	// Put replaces one record.
	Put(record Record) error
	// List returns up to limit records whose id sorts after afterID, in id
	// order. It is the bounded, cursor-resumable query the backfill needs;
	// there is no unbounded query in this file.
	List(afterID string, limit int) ([]Record, error)
}

// Stage is one of the four stages, in the only order they may occur.
type Stage string

const (
	StageExpand   Stage = "expand"
	StageCoexist  Stage = "coexist"
	StageMigrate  Stage = "migrate"
	StageContract Stage = "contract"
)

// Stages is the four stages in order.
var Stages = []Stage{StageExpand, StageCoexist, StageMigrate, StageContract}

// Reversible reports whether a stage can be undone. Contract is the first
// irreversible stage, and the only one: rollback is possible during expand,
// coexist and migrate, and impossible after contract, because a binary whose
// schema_max is below the post-contract schema can no longer read the store.
// No timer is involved in that fact.
func (s Stage) Reversible() bool { return s != StageContract }

// ErrIrreversibleStage refuses the reversal of a contract step.
var ErrIrreversibleStage = errors.New("update: the contract stage is irreversible; returning to a pre-contract version requires rebuilding and re-signing, and after a contract step even a rebuilt binary cannot be routed")

// FieldMigration names the old and new payload field one stage sequence
// moves a value between.
type FieldMigration struct {
	OldField string
	NewField string
}

func (m FieldMigration) valid() error {
	if m.OldField == "" || m.NewField == "" || m.OldField == m.NewField {
		return errors.New("field migration needs two distinct payload field names")
	}
	return nil
}

// Expand writes the new payload field in addition to the old one, under the
// unchanged envelope. No reader change is needed because unknown-field
// tolerance already holds, and the old binary keeps reading the old field,
// which is what makes this stage fully reversible.
func Expand(codec Codec, id string, m FieldMigration) error {
	if err := m.valid(); err != nil {
		return err
	}
	record, ok, err := codec.Get(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("record %s not found", id)
	}
	value, present := record.Payload[m.OldField]
	if !present {
		return fmt.Errorf("record %s carries no %s to expand from", id, m.OldField)
	}
	next := record.Clone()
	next.Payload[m.NewField] = value
	return codec.Put(next)
}

// Resolve is the coexist-stage read: the new field if present, else the old
// one. Every reader resolves this way for the whole mixed-version window.
func Resolve(record Record, m FieldMigration) (string, bool) {
	if v, ok := record.Payload[m.NewField]; ok {
		return v, true
	}
	v, ok := record.Payload[m.OldField]
	return v, ok
}

// MigrateBudget bounds one backfill tick, shaped like a reconcile tick per
// docs/architecture/validation.md section 5: at most MaxDocuments documents
// and MaxDuration per tick, restartable from a stored cursor, no unbounded
// query and no wall-clock dependence -- Now is injected.
type MigrateBudget struct {
	MaxDocuments int
	MaxDuration  time.Duration
	Now          func() time.Time
}

// DefaultMigrateBudget is the bound section 7.1 names: 100 documents and
// 30 s per tick. Now must still be injected.
func DefaultMigrateBudget(now func() time.Time) MigrateBudget {
	return MigrateBudget{MaxDocuments: 100, MaxDuration: 30 * time.Second, Now: now}
}

// MigrateResult reports one tick. Cursor is where the next tick resumes.
type MigrateResult struct {
	Cursor    string
	Scanned   int
	Rewritten int
	Done      bool
	StoppedBy string
}

// Migrate is the bounded, resumable, idempotent backfill: it rewrites old
// records to carry the new field, stops on either bound, and is a no-op on
// records that already carry the field, so re-running a tick converges.
func Migrate(codec Codec, m FieldMigration, cursor string, budget MigrateBudget) (MigrateResult, error) {
	if err := m.valid(); err != nil {
		return MigrateResult{}, err
	}
	if budget.MaxDocuments <= 0 || budget.MaxDuration <= 0 || budget.Now == nil {
		return MigrateResult{}, errors.New("migration budget needs a document bound, a duration bound and an injected clock")
	}
	start := budget.Now()
	deadline := start.Add(budget.MaxDuration)
	result := MigrateResult{Cursor: cursor}
	for result.Scanned < budget.MaxDocuments {
		if !budget.Now().Before(deadline) {
			result.StoppedBy = "duration bound"
			return result, nil
		}
		batch, err := codec.List(result.Cursor, budget.MaxDocuments-result.Scanned)
		if err != nil {
			return result, err
		}
		if len(batch) == 0 {
			result.Done = true
			result.StoppedBy = "no documents left"
			return result, nil
		}
		for _, record := range batch {
			result.Cursor = record.ID
			result.Scanned++
			if _, ok := record.Payload[m.NewField]; ok {
				continue
			}
			value, present := record.Payload[m.OldField]
			if !present {
				continue
			}
			next := record.Clone()
			next.Payload[m.NewField] = value
			if err := codec.Put(next); err != nil {
				return result, err
			}
			result.Rewritten++
			if result.Scanned >= budget.MaxDocuments {
				break
			}
		}
	}
	result.StoppedBy = "document bound"
	return result, nil
}

// Contract stops writing the old field and deletes it. It runs only when
// ContractAllowed permits it, which is RetentionEligible used a second time,
// and it deliberately does not touch the envelope: bumping record_schema is
// the store's expand step and belongs to V2-070.
func Contract(codec Codec, m FieldMigration, postContractSchema int, versions []VersionRetention, ids []string) error {
	if err := m.valid(); err != nil {
		return err
	}
	if allowed, reason := ContractAllowed(postContractSchema, versions); !allowed {
		return fmt.Errorf("%w: %s", ErrContractRefused, reason)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		record, ok, err := codec.Get(id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("record %s not found", id)
		}
		if _, ok := record.Payload[m.NewField]; !ok {
			return fmt.Errorf("record %s does not carry %s yet: contract is always a separate Increment from its expand", id, m.NewField)
		}
		next := record.Clone()
		delete(next.Payload, m.OldField)
		if err := codec.Put(next); err != nil {
			return err
		}
	}
	return nil
}

// Reverse undoes a stage. Expand, coexist and migrate are undone by dropping
// the new field, which restores the records byte for byte because expand
// never removed the old one. Contract has no reversal at all.
func Reverse(codec Codec, stage Stage, m FieldMigration, ids []string) error {
	if err := m.valid(); err != nil {
		return err
	}
	if !stage.Reversible() {
		return fmt.Errorf("%w (stage %s)", ErrIrreversibleStage, stage)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		record, ok, err := codec.Get(id)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, present := record.Payload[m.OldField]; !present {
			return fmt.Errorf("record %s has no %s to fall back to, so this stage is not reversible after all", id, m.OldField)
		}
		next := record.Clone()
		delete(next.Payload, m.NewField)
		if err := codec.Put(next); err != nil {
			return err
		}
	}
	return nil
}
