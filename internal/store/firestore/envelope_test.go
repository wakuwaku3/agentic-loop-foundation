package firestore

// This file proves the record envelope's EXPAND stage
// (docs/operations/self-update.md section 7.1, docs/operations/record-envelope.md):
// the persisted envelope is read against an accepted SET through one
// predicate that all five read-side refusal sites call, the written value is
// pinned so a bump cannot ride along, a non-native id additionally has to
// carry an interpretable payload, and no refusal that exists today got
// weaker.
//
// Determinism: no fixed sleep, no wall-clock timer, no goroutine. Every
// timestamp handed to the store comes from integrationClock (store_test.go).
// The only clock read is the Firestore installation namespace, exactly as the
// pre-existing emulatorStore helper does, and nothing is asserted about it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	cloudfirestore "cloud.google.com/go/firestore"
	"github.com/takushi/agentic-loop-foundation/v2/internal/application"
	"github.com/takushi/agentic-loop-foundation/v2/internal/domain"
)

// The second and third envelope ids exist ONLY in this file. They are never
// members of the production AcceptedRecordSchemas: the second is substituted
// into the set to prove a widened member is read by all five paths, and the
// third is deliberately left out of the substituted set to prove a
// non-member is refused by all five.
const (
	envelopeSecondProbe = "v2-expand-probe"
	envelopeThirdProbe  = "v3-refused-probe"
)

// recordEnvelopeIDShape is the shape every member of the accepted set must
// match: a lowercase "v", a decimal generation and optional lowercase
// hyphen-separated qualifiers. It rejects whitespace, uppercase and the empty
// string by construction.
var recordEnvelopeIDShape = regexp.MustCompile(`^v[0-9]+(-[a-z0-9]+)*$`)

var envelopeProbeClock = integrationClock{now: time.Unix(1700000000, 0).UTC()}

// substituteAcceptedRecordSchemas replaces the production accepted set for the
// duration of one test and returns the restore function, which every caller
// invokes with defer. This is the technique internal/scheduler/priority.go
// already establishes for scoreFn: a package-level variable exists so a test
// can substitute a variant and prove a positive and a negative control
// without shipping a production value the code cannot honour.
func substituteAcceptedRecordSchemas(t *testing.T, ids ...string) func() {
	t.Helper()
	if len(AcceptedRecordSchemas) != 1 {
		t.Fatalf("production accepted set has %d members before substitution, want exactly 1: this task ships the expand mechanism, not a widened set", len(AcceptedRecordSchemas))
	}
	original := AcceptedRecordSchemas
	AcceptedRecordSchemas = ids
	return func() { AcceptedRecordSchemas = original }
}

// envelopeStores returns a factory handing out a fresh, empty, uniquely-named
// installation on one emulator client, so that a scan assertion is never
// polluted by a document another case wrote.
func envelopeStores(t *testing.T) func() *Store {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is required for Firestore envelope integration tests")
	}
	client, err := cloudfirestore.NewClient(context.Background(), "agentic-loop-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	namespace := time.Now().Format("150405.000000000")
	seq := 0
	return func() *Store {
		seq++
		s, err := NewEmulatorStore(client, fmt.Sprintf("envelope-%s-%d", namespace, seq))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

// documentUnder builds exactly the document encodeDocument would write and
// then overrides only the envelope id, so the envelope is the single
// difference between a native document and a probe document.
func documentUnder(t *testing.T, envelope, kind string, value any) document {
	t.Helper()
	d, err := encodeDocument(kind, value)
	if err != nil {
		t.Fatal(err)
	}
	d.RecordSchema = envelope
	return d
}

func writeRawDocument(t *testing.T, s *Store, collection, id string, d document) {
	t.Helper()
	ref, err := s.path(collection, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Set(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func writeRawFields(t *testing.T, s *Store, collection, id string, fields map[string]any) {
	t.Helper()
	ref, err := s.path(collection, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Set(context.Background(), fields); err != nil {
		t.Fatal(err)
	}
}

// sameRequirement compares the identity fields a codec round-trip must
// preserve. domain.Requirement contains slice fields, so it is not comparable
// with ==.
func sameRequirement(a, b domain.Requirement) bool {
	return a.ID == b.ID && a.Version == b.Version && a.Status == b.Status
}

func validRequirementRecord(t *testing.T, id string) requirementRecord {
	t.Helper()
	rid, err := domain.NewRequirementID(id)
	if err != nil {
		t.Fatal(err)
	}
	return requirementRecord{Requirement: domain.Requirement{ID: rid, Version: 1, Status: domain.RequirementCaptured}}
}

func pendingOutbox() application.OutboxItem {
	return application.OutboxItem{ID: "envelope-outbox", Status: application.OutboxPending, NextAttemptAt: envelopeProbeClock.Now(), Version: 1}
}

// encodeRecordUnder builds the flat JSON codec envelope (the DecodeRecord
// shape) under an arbitrary envelope id.
func encodeRecordUnder(t *testing.T, envelope, kind string, value any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"record_schema": envelope, "kind": kind, "value": value})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ===========================================================================
// A5 / A3: the written value is pinned, and the shipped set has one member.
// ===========================================================================

// TestPinsTheWrittenRecordEnvelopeValueSoABumpCannotRideAlong is named for
// what it pins. It fails if the value this binary WRITES changes, which is
// the sequencing rule non_goal 1 states: a bump is a separate Increment from
// the expand this task performs.
func TestPinsTheWrittenRecordEnvelopeValueSoABumpCannotRideAlong(t *testing.T) {
	const pinnedWrittenEnvelope = "v1"
	if RecordSchema != pinnedWrittenEnvelope {
		t.Fatalf("the written record envelope changed from %q to %q. Bumping the written value is a SEPARATE Increment from widening what readers accept (docs/operations/self-update.md section 7.1, docs/operations/record-envelope.md). Widen AcceptedRecordSchemas first, ship it, then bump RecordSchema in its own change and update this pin deliberately.", pinnedWrittenEnvelope, RecordSchema)
	}

	// The shipped accepted set is exactly one member, and that member is the
	// value the encode paths write. This task widens the mechanism, not the set.
	if len(AcceptedRecordSchemas) != 1 {
		t.Fatalf("AcceptedRecordSchemas has %d members, want exactly 1 (%v)", len(AcceptedRecordSchemas), AcceptedRecordSchemas)
	}
	if AcceptedRecordSchemas[0] != RecordSchema {
		t.Fatalf("shipped accepted set %v does not consist of the written value %q", AcceptedRecordSchemas, RecordSchema)
	}
	if !RecordSchemaAccepted(RecordSchema) {
		t.Fatal("the store writes an envelope id it would refuse to read")
	}
	if !recordSchemaIsNative(RecordSchema) {
		t.Fatal("recordSchemaIsNative does not recognise the written value")
	}

	// Every member matches the declared id shape and the set is duplicate-free.
	seen := map[string]bool{}
	for i, member := range AcceptedRecordSchemas {
		if !recordEnvelopeIDShape.MatchString(member) {
			t.Fatalf("accepted set member %d = %q does not match the declared envelope id shape %s", i, member, recordEnvelopeIDShape)
		}
		if seen[member] {
			t.Fatalf("accepted set contains duplicate member %q", member)
		}
		seen[member] = true
	}

	// A store must not be able to write a value it would refuse to read:
	// round-trip every member of the accepted set through encode and decode.
	rid, err := domain.NewRequirementID("pin")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Requirement{ID: rid, Version: 1, Status: domain.RequirementCaptured}
	for _, member := range AcceptedRecordSchemas {
		b := encodeRecordUnder(t, member, "requirement", want)
		var got domain.Requirement
		if err := DecodeRecord(b, "requirement", &got); err != nil {
			t.Fatalf("accepted member %q failed to round-trip through the codec: %v", member, err)
		}
		if !sameRequirement(got, want) {
			t.Fatalf("accepted member %q round-tripped to %#v, want %#v", member, got, want)
		}
		if member == RecordSchema {
			native, err := EncodeRecord("requirement", want)
			if err != nil {
				t.Fatal(err)
			}
			var back domain.Requirement
			if err := DecodeRecord(native, "requirement", &back); err != nil || !sameRequirement(back, want) {
				t.Fatalf("native encode/decode round-trip broke: %v %#v", err, back)
			}
			d, err := encodeDocument("requirement", requirementRecord{Requirement: want})
			if err != nil {
				t.Fatal(err)
			}
			if d.RecordSchema != member {
				t.Fatalf("encodeDocument wrote envelope %q, want the accepted written value %q", d.RecordSchema, member)
			}
		}
	}
}

// ===========================================================================
// A3: exact membership. No normalisation of any kind.
// ===========================================================================

func TestRecordSchemaAcceptedRequiresExactMembership(t *testing.T) {
	// The near-miss table is deliberately a NEW test rather than an edit to
	// TestCodecRejectsCorruption, whose three inputs stay exactly as they are.
	nearMisses := []struct {
		name string
		id   string
	}{
		{"empty string", ""},
		{"prefix of an accepted id", "v"},
		{"suffix of an accepted id", "1"},
		{"upper-case variant", "V1"},
		{"leading space variant", " v1"},
		{"trailing space variant", "v1 "},
		{"leading tab variant", "\tv1"},
		{"trailing newline variant", "v1\n"},
		{"accepted id with a trailing dot", "v1."},
		{"accepted id embedded in a longer id", "xv1x"},
		{"an id no writer emits", "v2"},
	}
	for _, tc := range nearMisses {
		if RecordSchemaAccepted(tc.id) {
			t.Fatalf("%s: RecordSchemaAccepted(%q) accepted a non-member; membership must be exact string equality with no trimming, case folding or prefix matching", tc.name, tc.id)
		}
		b := encodeRecordUnder(t, tc.id, "requirement", validRequirementRecord(t, "near-miss").Requirement)
		var got domain.Requirement
		if err := DecodeRecord(b, "requirement", &got); !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("%s: DecodeRecord accepted envelope %q: %v", tc.name, tc.id, err)
		}
	}
	for _, member := range AcceptedRecordSchemas {
		if !RecordSchemaAccepted(member) {
			t.Fatalf("accepted set member %q is refused by the predicate", member)
		}
	}
}

// ===========================================================================
// A4 (site 1, no emulator required): DecodeRecord honours the accepted set.
// ===========================================================================

func TestDecodeRecordHonoursASubstitutedAcceptedSet(t *testing.T) {
	defer substituteAcceptedRecordSchemas(t, RecordSchema, envelopeSecondProbe)()
	want := validRequirementRecord(t, "codec-probe").Requirement

	for _, envelope := range []string{RecordSchema, envelopeSecondProbe} {
		var got domain.Requirement
		if err := DecodeRecord(encodeRecordUnder(t, envelope, "requirement", want), "requirement", &got); err != nil {
			t.Fatalf("DecodeRecord refused accepted envelope %q: %v", envelope, err)
		}
		if !sameRequirement(got, want) {
			t.Fatalf("envelope %q decoded to %#v, want %#v", envelope, got, want)
		}
	}
	var got domain.Requirement
	if err := DecodeRecord(encodeRecordUnder(t, envelopeThirdProbe, "requirement", want), "requirement", &got); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("DecodeRecord accepted non-member envelope %q: %v", envelopeThirdProbe, err)
	}
}

// ===========================================================================
// A4: all five read sites, proven by behaviour.
// ===========================================================================

// envelopeReadSite drives one of the five read-side refusal sites end to end:
// it writes a document under the given envelope id and performs the concrete
// store operation that reads it. Line numbers name the BASE tree
// (ff60533), which is where docs/operations/self-update.md section 7.3
// measured two of these five.
type envelopeReadSite struct {
	name string
	run  func(t *testing.T, newStore func() *Store, envelope string) error
}

func envelopeReadSites() []envelopeReadSite {
	return []envelopeReadSite{
		{
			name: "DecodeRecord (base store.go:79) via the JSON codec",
			run: func(t *testing.T, _ func() *Store, envelope string) error {
				var got domain.Requirement
				return DecodeRecord(encodeRecordUnder(t, envelope, "requirement", validRequirementRecord(t, "site-1").Requirement), "requirement", &got)
			},
		},
		{
			name: "decodeDocument (base store.go:173) via unit.Requirement",
			run: func(t *testing.T, newStore func() *Store, envelope string) error {
				s := newStore()
				ctx := context.Background()
				writeRawDocument(t, s, "requirements", "site-2", documentUnder(t, envelope, "requirement", validRequirementRecord(t, "site-2")))
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					v, ok, err := u.Requirement(ctx, "site-2")
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("document not visible")
					}
					if v.ID.String() != "site-2" {
						return fmt.Errorf("decoded requirement id %q", v.ID)
					}
					return nil
				})
			},
		},
		{
			name: "outbox scan (base store.go:1230) via unit.Outboxes",
			run: func(t *testing.T, newStore func() *Store, envelope string) error {
				s := newStore()
				ctx := context.Background()
				writeRawDocument(t, s, "outbox", "site-3", documentUnder(t, envelope, "outbox", pendingOutbox()))
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					items, err := u.Outboxes(ctx, envelopeProbeClock.Now(), 10)
					if err != nil {
						return err
					}
					if len(items) != 1 {
						return fmt.Errorf("outbox scan returned %d rows, want 1", len(items))
					}
					return nil
				})
			},
		},
		{
			name: "bounded collection scan (base store.go:1348) via unit.Requirements",
			run: func(t *testing.T, newStore func() *Store, envelope string) error {
				s := newStore()
				ctx := context.Background()
				writeRawDocument(t, s, "requirements", "site-4", documentUnder(t, envelope, "requirement", validRequirementRecord(t, "site-4")))
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					rows, err := u.Requirements(ctx)
					if err != nil {
						return err
					}
					if len(rows) != 1 {
						return fmt.Errorf("collection scan returned %d rows, want 1", len(rows))
					}
					return nil
				})
			},
		},
		{
			name: "bounded collection scan (base store.go:1484) via Store.Events",
			run: func(t *testing.T, newStore func() *Store, envelope string) error {
				s := newStore()
				ctx := context.Background()
				writeRawDocument(t, s, "events", "site-5", documentUnder(t, envelope, "event", application.Event{ID: "site-5"}))
				rows, err := s.Events(ctx)
				if err != nil {
					return err
				}
				if len(rows) != 1 {
					return fmt.Errorf("collection scan returned %d rows, want 1", len(rows))
				}
				return nil
			},
		},
	}
}

func TestAllFiveReadSitesAcceptASubstitutedMemberAndRefuseANonMember(t *testing.T) {
	newStore := envelopeStores(t)
	sites := envelopeReadSites()
	if len(sites) != 5 {
		t.Fatalf("the read-side refusal site table has %d entries, want 5", len(sites))
	}
	defer substituteAcceptedRecordSchemas(t, RecordSchema, envelopeSecondProbe)()

	for _, site := range sites {
		t.Run("accepts/"+site.name, func(t *testing.T) {
			if err := site.run(t, newStore, envelopeSecondProbe); err != nil {
				t.Fatalf("%s refused the substituted accepted envelope %q: %v", site.name, envelopeSecondProbe, err)
			}
		})
		t.Run("accepts-native/"+site.name, func(t *testing.T) {
			if err := site.run(t, newStore, RecordSchema); err != nil {
				t.Fatalf("%s refused the native envelope %q: %v", site.name, RecordSchema, err)
			}
		})
		t.Run("refuses/"+site.name, func(t *testing.T) {
			err := site.run(t, newStore, envelopeThirdProbe)
			if !errors.Is(err, ErrInvalidSchema) {
				t.Fatalf("%s did not refuse non-member envelope %q with ErrInvalidSchema: %v", site.name, envelopeThirdProbe, err)
			}
		})
	}
}

// ===========================================================================
// A6: payload interpretability for non-native ids only.
// ===========================================================================

// interpretabilitySite is one of the three read sites whose record kind the
// domain declares a validity predicate for. The other two live sites read
// kinds (outbox, event) for which domain.Validate declares nothing, so the
// gate is present there but has no applicable predicate; that is recorded
// rather than asserted.
type interpretabilitySite struct {
	name string
	// wrap turns a requirement into the payload shape this site stores: the
	// flat codec carries a bare domain.Requirement, the document paths carry
	// the requirementRecord wrapper.
	wrap func(domain.Requirement) any
	read func(t *testing.T, newStore func() *Store, envelope string, payload any) error
}

func interpretabilitySites() []interpretabilitySite {
	return []interpretabilitySite{
		{
			name: "DecodeRecord (base store.go:79)",
			wrap: func(r domain.Requirement) any { return r },
			read: func(t *testing.T, _ func() *Store, envelope string, payload any) error {
				var got domain.Requirement
				return DecodeRecord(encodeRecordUnder(t, envelope, "requirement", payload), "requirement", &got)
			},
		},
		{
			name: "decodeDocument (base store.go:173) via unit.Requirement",
			wrap: func(r domain.Requirement) any { return requirementRecord{Requirement: r} },
			read: func(t *testing.T, newStore func() *Store, envelope string, payload any) error {
				s := newStore()
				ctx := context.Background()
				writeRawDocument(t, s, "requirements", "interpret", documentUnder(t, envelope, "requirement", payload))
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					_, _, err := u.Requirement(ctx, "interpret")
					return err
				})
			},
		},
		{
			name: "bounded collection scan (base store.go:1348) via unit.Requirements",
			wrap: func(r domain.Requirement) any { return requirementRecord{Requirement: r} },
			read: func(t *testing.T, newStore func() *Store, envelope string, payload any) error {
				s := newStore()
				ctx := context.Background()
				writeRawDocument(t, s, "requirements", "interpret", documentUnder(t, envelope, "requirement", payload))
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					_, err := u.Requirements(ctx)
					return err
				})
			},
		},
	}
}

func TestNonNativeEnvelopeRequiresAnInterpretablePayload(t *testing.T) {
	newStore := envelopeStores(t)
	defer substituteAcceptedRecordSchemas(t, RecordSchema, envelopeSecondProbe)()

	marshallableID, err := domain.NewRequirementID("marshallable")
	if err != nil {
		t.Fatal(err)
	}
	invalid := []struct {
		name        string
		requirement domain.Requirement
	}{
		{"empty requirement id", domain.Requirement{Version: 1, Status: domain.RequirementCaptured}},
		{"unknown requirement status", domain.Requirement{ID: marshallableID, Version: 1, Status: domain.RequirementStatus("not-a-status")}},
	}
	// A payload that is valid JSON of the wrong shape decodes into a
	// zero-valued struct and looks like a legitimate record: that is exactly
	// the silent acceptance the gate exists to refuse.
	wrongShape := map[string]any{"unexpected": "field"}

	for _, site := range interpretabilitySites() {
		uninterpretable := make([]struct {
			name    string
			payload any
		}, 0, len(invalid)+1)
		for _, tc := range invalid {
			uninterpretable = append(uninterpretable, struct {
				name    string
				payload any
			}{tc.name, site.wrap(tc.requirement)})
		}
		uninterpretable = append(uninterpretable, struct {
			name    string
			payload any
		}{"valid JSON of the wrong shape", wrongShape})

		for _, tc := range uninterpretable {
			t.Run("refuses/"+site.name+"/"+tc.name, func(t *testing.T) {
				err := site.read(t, newStore, envelopeSecondProbe, tc.payload)
				if !errors.Is(err, ErrInvalidSchema) {
					t.Fatalf("%s accepted a non-native document whose payload the running code cannot interpret (%s): %v", site.name, tc.name, err)
				}
			})
			t.Run("native-unaffected/"+site.name+"/"+tc.name, func(t *testing.T) {
				// The same payload under the NATIVE envelope keeps today's
				// behaviour: the interpretability gate is not applied there.
				if err := site.read(t, newStore, RecordSchema, tc.payload); err != nil {
					t.Fatalf("%s changed behaviour on the native envelope for %s: %v; the native read path must be unchanged", site.name, tc.name, err)
				}
			})
		}
		t.Run("accepts/"+site.name+"/an interpretable payload", func(t *testing.T) {
			interpretable := site.wrap(validRequirementRecord(t, "interpretable").Requirement)
			if err := site.read(t, newStore, envelopeSecondProbe, interpretable); err != nil {
				t.Fatalf("%s refused a non-native document with an interpretable payload: %v", site.name, err)
			}
		})
	}
}

func TestNativeEnvelopeReadPathInvokesNoValidityPredicate(t *testing.T) {
	s := emulatorStore(t)
	ctx := context.Background()
	rid, err := domain.NewRequirementID("native-no-validate")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if err := u.SaveRequirement(ctx, domain.Requirement{ID: rid, Version: 1, Status: domain.RequirementCaptured}, 0); err != nil {
			return err
		}
		return u.Record(application.Event{ID: "native-event", AggregateID: rid.String()}, &application.OutboxItem{ID: "native-outbox", NextAttemptAt: envelopeProbeClock.Now()})
	}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	original := validateRecordPayload
	validateRecordPayload = func(v any) error {
		calls++
		return original(v)
	}
	defer func() { validateRecordPayload = original }()

	// Every one of the five read sites, driven with native documents only.
	var decoded domain.Requirement
	encoded, err := EncodeRecord("requirement", domain.Requirement{ID: rid, Version: 1, Status: domain.RequirementCaptured})
	if err != nil {
		t.Fatal(err)
	}
	if err := DecodeRecord(encoded, "requirement", &decoded); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(u application.UnitOfWork) error {
		if _, _, err := u.Requirement(ctx, rid.String()); err != nil {
			return err
		}
		if _, err := u.Requirements(ctx); err != nil {
			return err
		}
		_, err := u.Outboxes(ctx, envelopeProbeClock.Now(), 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Events(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Outbox(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("the native read path invoked the domain validity predicate %d times; it must invoke it zero times so today's read path is unchanged", calls)
	}

	// Positive control: the counter is wired, so the zero above is a measured
	// zero rather than a broken probe.
	defer substituteAcceptedRecordSchemas(t, RecordSchema, envelopeSecondProbe)()
	var probe domain.Requirement
	if err := DecodeRecord(encodeRecordUnder(t, envelopeSecondProbe, "requirement", domain.Requirement{ID: rid, Version: 1, Status: domain.RequirementCaptured}), "requirement", &probe); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("validity-predicate counter recorded %d calls on a non-native read, want exactly 1; the probe is not wired", calls)
	}
}

// ===========================================================================
// A7: no refusal got weaker. Four reasons at each of the five sites.
// ===========================================================================

type refusalReadSite struct {
	name string
	// refuse writes one raw document built from fields and performs the read.
	refuse func(t *testing.T, newStore func() *Store, fields map[string]any) error
	// kind is the record kind the site's read expects.
	kind string
	// collection is where the site's read looks.
	collection string
	// projections are the indexed fields the site's query needs to see the
	// document at all.
	projections map[string]any
}

func refusalReadSites() []refusalReadSite {
	return []refusalReadSite{
		{
			name:       "decodeDocument (base store.go:173) via unit.Requirement",
			kind:       "requirement",
			collection: "requirements",
			refuse: func(t *testing.T, newStore func() *Store, fields map[string]any) error {
				s := newStore()
				ctx := context.Background()
				writeRawFields(t, s, "requirements", "refusal", fields)
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					_, _, err := u.Requirement(ctx, "refusal")
					return err
				})
			},
		},
		{
			name:        "outbox scan (base store.go:1230) via unit.Outboxes",
			kind:        "outbox",
			collection:  "outbox",
			projections: map[string]any{"outbox_status": string(application.OutboxPending), "outbox_next_attempt_at": envelopeProbeClock.Now().Format(time.RFC3339Nano)},
			refuse: func(t *testing.T, newStore func() *Store, fields map[string]any) error {
				s := newStore()
				ctx := context.Background()
				writeRawFields(t, s, "outbox", "refusal", fields)
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					_, err := u.Outboxes(ctx, envelopeProbeClock.Now(), 10)
					return err
				})
			},
		},
		{
			name:       "bounded collection scan (base store.go:1348) via unit.Requirements",
			kind:       "requirement",
			collection: "requirements",
			refuse: func(t *testing.T, newStore func() *Store, fields map[string]any) error {
				s := newStore()
				ctx := context.Background()
				writeRawFields(t, s, "requirements", "refusal", fields)
				return s.Transact(ctx, func(u application.UnitOfWork) error {
					_, err := u.Requirements(ctx)
					return err
				})
			},
		},
		{
			name:       "bounded collection scan (base store.go:1484) via Store.Events",
			kind:       "event",
			collection: "events",
			refuse: func(t *testing.T, newStore func() *Store, fields map[string]any) error {
				s := newStore()
				writeRawFields(t, s, "events", "refusal", fields)
				_, err := s.Events(context.Background())
				return err
			},
		},
	}
}

func TestEveryReadSiteKeepsAllFourRefusalReasons(t *testing.T) {
	// Site 1 (DecodeRecord) is a pure function and is asserted first, without
	// the emulator, so the four reasons are covered even where the emulator is
	// unavailable. TestCodecRejectsCorruption keeps its own three inputs
	// untouched; these are additional cases in a separate test.
	validRequirement := validRequirementRecord(t, "refusal").Requirement
	codecCases := []struct {
		reason string
		data   []byte
	}{
		{"malformed JSON", []byte("{")},
		{"envelope id outside the accepted set", encodeRecordUnder(t, "v0-not-accepted", "requirement", validRequirement)},
		{"kind mismatch", encodeRecordUnder(t, RecordSchema, "increment", validRequirement)},
		{"empty value", []byte(`{"record_schema":"v1","kind":"requirement"}`)},
	}
	for _, tc := range codecCases {
		var got domain.Requirement
		if err := DecodeRecord(tc.data, "requirement", &got); !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("DecodeRecord (base store.go:79) stopped refusing %s: %v", tc.reason, err)
		}
	}

	newStore := envelopeStores(t)
	payload, err := json.Marshal(validRequirementRecord(t, "refusal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range refusalReadSites() {
		base := func() map[string]any {
			f := map[string]any{"record_schema": RecordSchema, "kind": site.kind, "payload": string(payload)}
			for k, v := range site.projections {
				f[k] = v
			}
			return f
		}
		cases := []struct {
			reason string
			build  func() map[string]any
		}{
			{"a DataTo failure", func() map[string]any { f := base(); f["payload"] = 42; return f }},
			{"an envelope id outside the accepted set", func() map[string]any {
				f := base()
				f["record_schema"] = "v0-not-accepted"
				return f
			}},
			{"a kind mismatch", func() map[string]any { f := base(); f["kind"] = "not-" + site.kind; return f }},
			{"an empty payload", func() map[string]any { f := base(); f["payload"] = ""; return f }},
		}
		for _, tc := range cases {
			t.Run(site.name+"/"+tc.reason, func(t *testing.T) {
				err := site.refuse(t, newStore, tc.build())
				if !errors.Is(err, ErrInvalidSchema) {
					t.Fatalf("%s stopped refusing %s: %v", site.name, tc.reason, err)
				}
			})
		}
	}
}

// ===========================================================================
// A10: a bad document fails the WHOLE bounded scan; nothing is skipped.
// ===========================================================================

func TestABadDocumentFailsTheWholeBoundedScan(t *testing.T) {
	newStore := envelopeStores(t)
	payload, err := json.Marshal(validRequirementRecord(t, "scan-good"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("outbox scan (base store.go:1230)", func(t *testing.T) {
		s := newStore()
		ctx := context.Background()
		writeRawDocument(t, s, "outbox", "scan-good", documentUnder(t, RecordSchema, "outbox", pendingOutbox()))
		writeRawFields(t, s, "outbox", "scan-bad", map[string]any{
			"record_schema": "v0-not-accepted", "kind": "outbox", "payload": string(payload),
			"outbox_status": string(application.OutboxPending), "outbox_next_attempt_at": envelopeProbeClock.Now().Format(time.RFC3339Nano),
		})
		var rows []application.OutboxItem
		err := s.Transact(ctx, func(u application.UnitOfWork) error {
			var e error
			rows, e = u.Outboxes(ctx, envelopeProbeClock.Now(), 10)
			return e
		})
		if !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("one bad document did not fail the whole outbox scan: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("the scan returned %d rows alongside the failure; partial-scan semantics must not exist", len(rows))
		}
	})

	t.Run("bounded collection scan (base store.go:1348)", func(t *testing.T) {
		s := newStore()
		ctx := context.Background()
		writeRawDocument(t, s, "requirements", "scan-good", documentUnder(t, RecordSchema, "requirement", validRequirementRecord(t, "scan-good")))
		writeRawFields(t, s, "requirements", "scan-bad", map[string]any{"record_schema": "v0-not-accepted", "kind": "requirement", "payload": string(payload)})
		var rows []domain.Requirement
		err := s.Transact(ctx, func(u application.UnitOfWork) error {
			var e error
			rows, e = u.Requirements(ctx)
			return e
		})
		if !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("one bad document did not fail the whole collection scan: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("the scan returned %d rows alongside the failure; partial-scan semantics must not exist", len(rows))
		}
	})

	t.Run("bounded collection scan (base store.go:1484)", func(t *testing.T) {
		s := newStore()
		ctx := context.Background()
		writeRawDocument(t, s, "events", "scan-good", documentUnder(t, RecordSchema, "event", application.Event{ID: "scan-good"}))
		writeRawFields(t, s, "events", "scan-bad", map[string]any{"record_schema": "v0-not-accepted", "kind": "event", "payload": string(payload)})
		rows, err := s.Events(ctx)
		if !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("one bad document did not fail the whole collection scan: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("the scan returned %d rows alongside the failure; partial-scan semantics must not exist", len(rows))
		}
	})

	// The same three scans, with a non-native envelope whose PAYLOAD fails the
	// interpretability gate, fail the whole scan too: the gate does not
	// introduce a skip-the-bad-document path either.
	t.Run("interpretability failure also fails the whole scan (base store.go:1348)", func(t *testing.T) {
		defer substituteAcceptedRecordSchemas(t, RecordSchema, envelopeSecondProbe)()
		s := newStore()
		ctx := context.Background()
		writeRawDocument(t, s, "requirements", "scan-good", documentUnder(t, RecordSchema, "requirement", validRequirementRecord(t, "scan-good")))
		writeRawDocument(t, s, "requirements", "scan-bad", documentUnder(t, envelopeSecondProbe, "requirement", map[string]any{"unexpected": "field"}))
		var rows []domain.Requirement
		err := s.Transact(ctx, func(u application.UnitOfWork) error {
			var e error
			rows, e = u.Requirements(ctx)
			return e
		})
		if !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("an uninterpretable non-native document did not fail the whole scan: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("the scan returned %d rows alongside the failure", len(rows))
		}
	})
}
