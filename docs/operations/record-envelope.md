# The persisted record envelope: accepted set, predicate, payload gate

Owner: `store-firestore`. Companion to `docs/operations/self-update.md`
section 7 (schema expand / coexist / migrate / contract), which this document
does not modify.

This document describes the **expand stage of the record envelope only**. It
is the state after V2-070. Nothing here claims coexist, migrate or contract,
and nothing here satisfies roadmap M8 completion condition 3; see
"Correspondence to the four stages" below.

## 1. The envelope shape

Every application record in Firestore is stored inside a three-field
envelope, with the payload held as a JSON **string** so that Firestore cannot
implicitly coerce payload values:

```
{ record_schema, kind, payload }        # the Firestore document
{ record_schema, kind, value }          # the flat JSON codec used by export/import
```

`record_schema` is the envelope id. It is a *generation marker for the
envelope*, not a version of the payload: payload decoding is a plain
`json.Unmarshal`, so unknown payload fields are ignored and absent payload
fields default to their zero values.

## 2. The complete map of write sites and read sites

There are **three write sites** and **five read-side refusal sites**. The
count matters: `docs/operations/self-update.md` section 7.3 measured **two of
the five** (`store.go:79` and `store.go:173`, both in the base tree). The
other three are on **scan** paths, and they were the reason a two-site
widening would have been worse than none: it would have produced a store that
accepts a widened envelope for a Requirement and refuses it for the outbox
record written in the same transaction, with the failure appearing
per-collection, only on paths that scan rather than fetch, and therefore under
load rather than in the first test.

Write sites (all three write `RecordSchema`, unchanged):

| Site | Base tree | What it writes |
| --- | --- | --- |
| `EncodeRecord` | `internal/store/firestore/store.go:68` | the flat JSON codec envelope |
| `encodeDocument` | `internal/store/firestore/store.go:127` | the Firestore document envelope |
| `BootstrapInstallation` | `internal/store/firestore/client.go:59` | the installation record |

Read-side refusal sites (all five call the one predicate):

| Site | Base tree | Concrete store operation that reaches it |
| --- | --- | --- |
| `DecodeRecord` | `store.go:79` | the flat JSON codec, used by export and migration tooling |
| `decodeDocument` | `store.go:173` | any single-document read, e.g. `unit.Requirement` |
| outbox scan in `unit.Outboxes` | `store.go:1230` | `unit.Outboxes` |
| bounded collection scan in `unit.query` | `store.go:1348` | `unit.Requirements`, `unit.Controls`, `unit.ControlRevision`, `unit.Repositories`, `unit.queryWhere` |
| bounded collection scan in `readCollection` | `store.go:1484` | `Store.Events`, `Store.Outbox` |

The declaration of the envelope constant is `store.go:31`, and the document
struct's envelope field is `store.go:89`, in the base tree. Both are still
there; neither moved semantically.

That these five are the **complete** set is not asserted by review or by
grep. `internal/store/firestore/source_guard_test.go` parses every `*.go` file
in the package with `go/parser`, refuses to pass on a zero-file scan, asserts
that the constant's declaration was actually found and carries the expected
literal, and then asserts two properties of every non-test file:

- the `RecordSchema` **constant** is named only in its own declaration, in the
  accepted-set declaration, in the native-id predicate and in the three write
  sites; and
- **no `==` or `!=` comparison of an envelope value survives outside the
  predicates.** A sixth read-side comparison added later fails this test.

The matcher itself is verified against synthesized known-positive sources
(one comparing the document field, one comparing the constant) and
known-negative sources (one routing the same decision through the predicate,
one comparing an unrelated field), so a broken matcher cannot report zero
findings and pass vacuously. The classifier that separates the constant from
the identically-named struct field is controlled the same way, because a text
grep for `RecordSchema` matches the field declaration, its selector at every
read site and the constant, and cannot tell them apart.

## 3. The accepted set and the predicate

```go
const RecordSchema = "v1"                          // what this binary WRITES
var AcceptedRecordSchemas = []string{RecordSchema} // what this binary READS
func RecordSchemaAccepted(id string) bool          // the one read-side decision
```

The written value and the accepted set are **separate declarations**. Widening
what a reader accepts must be possible without changing what a writer emits,
and bumping what a writer emits must be a separate change.

**The shipped set has exactly one member**, the value the encode paths already
write. That is deliberate and it is what an expand stage is: expand's defining
property in `self-update.md` section 7.1 is that no reader change is needed
and the step is fully reversible. Shipping a second member before any writer
emits it would make the store accept a document shape no code in this
repository can produce — a record whose payload the running code cannot
interpret — and would make the refusal branch vacuous, since nothing would
ever exercise it. **Observable store behaviour today is therefore unchanged.**
The member that admits a second envelope id belongs to the change that
introduces the second writer.

`AcceptedRecordSchemas` is a package-level variable rather than a constant
expression for exactly one reason: a test substitutes a wider set and restores
it with `defer`, which is how the mechanism is proven without shipping a value
the code cannot honour. This is the technique
`internal/scheduler/priority.go` already establishes for `scoreFn`. Nothing
outside a test may reassign it.

### Why exact membership, and no normalization

Membership is **exact string equality**. No trimming, case folding, prefix
matching, suffix matching or normalization of any kind is applied. A predicate
is the natural place for a well-meant later contributor to add a
normalization step, and every normalization widens the accepted set
invisibly. `envelope_test.go` therefore asserts refusal for a near-miss table:
the empty string, a prefix of an accepted id, a suffix of one, an upper-case
variant, leading- and trailing-space variants, leading-tab and
trailing-newline variants, an accepted id with a trailing dot, an accepted id
embedded in a longer id, and an id no writer emits.

The pre-existing `TestCodecRejectsCorruption` keeps its own three inputs
unchanged; the near-miss table is a **new** test rather than an edit to that
one, so the refactor can be shown not to have moved the boundary.

## 4. The payload-interpretability gate, for non-native ids only

Unknown-field tolerance is precisely what makes expand reversible and
precisely what makes silent acceptance possible: a payload written under a
later envelope that renamed a field decodes into a zero-valued struct and
looks like a legitimate record rather than an error.

So for any accepted envelope id **other than the one this binary writes**, the
read additionally requires that the payload decodes *and* that the decoded
value satisfies the domain's own validity predicate where the repository
declares one — `domain.Validate`, which describes `Requirement`, `Increment`,
`Execution` and `Lease`. A payload that fails is refused with
`ErrInvalidSchema`.

**The gate is not applied to the native id.** That is what keeps today's read
path unchanged, which in turn is the only form in which every pre-existing
store test keeps passing under its own name with its own assertions. A test
substitutes the validity predicate for a counting wrapper, drives all five
read sites with native documents, and asserts the count is **zero**; a
positive control on the same counter proves the probe is wired by observing
exactly one call on a non-native read.

Two consequences worth stating plainly:

- **Production exposure is zero today.** There are no non-native ids, so the
  gate is exercised only through the test-only set substitution. Its coverage
  is real; its production exposure is nil until a second writer exists.
- **Two of the five sites read kinds the domain declares no predicate for.**
  `store.go:1230` reads `outbox` and `store.go:1484` reads `event` and
  `outbox`; `domain.Validate` describes neither, so at those two sites the
  gate is present and reached but has no applicable predicate, and the
  effective refusals there remain the envelope check and the JSON decode. The
  gate has a live predicate at `store.go:79`, `store.go:173` and
  `store.go:1348` for the requirement, increment, execution and lease kinds.

## 5. Scan failure semantics

The three scan sites are inside bounded loops whose failure mode is a
**whole-scan error**, not a per-document one. A document that fails the
envelope check or the payload check fails the entire scan with
`ErrInvalidSchema`; it is **not** skipped, and the call returns no rows
alongside the failure.

This is unchanged from today for the envelope and JSON checks, and it is the
deliberate choice for the new payload gate as well. The consequence is real
and is stated here rather than discovered later: once a second writer exists,
a single non-native document whose payload fails the gate makes
`unit.Outboxes`, `unit.Requirements` (and every other `unit.query` caller) and
`Store.Events` / `Store.Outbox` fail for the whole collection until that
document is fixed. A partial-scan or skip-the-bad-document alternative would
be a **new failure contract**, and creating one was outside this change's
mandate. `envelope_test.go` asserts the whole-scan behaviour at each of
`store.go:1230`, `store.go:1348` and `store.go:1484`, including the case where
the failure comes from the payload gate rather than the envelope.

## 6. The memory adapter has no envelope

`internal/store/memory` holds typed Go values in maps and has **no
serialization boundary**: no `record_schema` field, no envelope constant and
no comparison to widen. The rule "both stores obey the same envelope rule"
therefore cannot mean copying the predicate into it — that would create a
second envelope implementation carrying a rule with no data to apply it to,
and two implementations are exactly how two adapters come to disagree.

The requirement's real content is that **there is exactly one envelope
implementation in the repository**, and the checkable form of that is a guard
in the second adapter:
`internal/store/memory/source_guard_test.go` parses the package with
`go/parser`, refuses to pass on a zero-file scan, and asserts that no struct
field, struct tag, constant, variable, type, function name or string literal
in the package names or carries `record_schema` under any spelling. The guard
excludes only its own file, which necessarily spells the name, and asserts
that exactly one file was excluded.

If the memory adapter ever gains a serialization boundary, that test fails and
forces the author to route it through `RecordSchemaAccepted` rather than write
a second predicate.

## 7. Sequencing: a bump is a separate change

The rule is `self-update.md` section 7.1's: contract is always a separate
Increment from its expand, and collapsing the two destroys the rollback path.
A sequencing rule that lives only in a work order is unenforceable in the next
change, so it is enforced by a test whose **name says what it pins**:

- `TestPinsTheWrittenRecordEnvelopeValueSoABumpCannotRideAlong`
  (`internal/store/firestore/envelope_test.go`)

It asserts that `RecordSchema` equals a literal string in the test body, that
`AcceptedRecordSchemas` contains it, that the shipped set has exactly one
member which is that value, that every member matches a declared id shape,
that the set is duplicate-free, and that every member round-trips through the
encode and decode paths — because a store that writes a value it would refuse
to read is broken in a way no single-direction test catches.

Changing the written value therefore fails a test whose name explains why. The
order is: widen `AcceptedRecordSchemas`, ship it, *then* bump `RecordSchema`
in its own change and update the pin deliberately.

## 8. Correspondence to the four stages of `self-update.md` section 7

| Stage | Status after V2-070 | What is deliberately absent |
| --- | --- | --- |
| EXPAND | **Done, for the envelope.** One accepted set, one predicate, five converted read sites, a pinned written value, a payload gate for non-native ids. | Nothing; this is the whole of this change. |
| COEXIST | **Not started.** | It is the mixed-version window of section 6 and is not observable until the Runner version report exists (V2-069). |
| MIGRATE | **Not started.** | No backfill, no cursor, no tick, no resumable rewrite of any document. |
| CONTRACT | **Not started.** | The section 7.2 predicate — the same predicate as GC eligibility in section 8 — is not implemented. No field is deleted and no writer stopped writing anything. |

**M8 completion condition 3.** Section 7.3 states that the envelope expand
stage *blocks* M8 completion condition 3. This change removes that block. It
does **not** satisfy the condition: blocking a condition and satisfying it are
different claims, and only the first is made here. No release gate is claimed
by this change.
