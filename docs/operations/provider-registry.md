# Provider registry

The Provider registry is the surface that makes Provider operation observable
from inside the system: `GET /v1/providers` and the Providers section of the
owner console. Before it existed, what was known about a Provider lived in a
cost ledger file on a Runner machine, outside the repository, where nothing
inside the system could read it.

This document describes what the registry reports, what it deliberately does
not, and where each value comes from.

## 1. Identity is the adapter name, and nothing else

A Provider is identified by exactly one of three names: `codex`, `claude`,
`opencode`. Identity is not name plus account.

The reason is measured rather than aesthetic. Every record that already exists
keys on the name: `internal/runner.CostLedger` keys on the Provider name,
`contracts/schemas/provider-preflight.json` keys `provider.name`, and
`contracts/schemas/provider-standing-authorization.json` constrains `providers`
to exactly that three-value enum. Keying the registry on the name therefore
joins it to all of them with no mapping table. The subscription set is one
account per CLI, so the name is already unique; adding an account discriminator
would mean moving owner identity into a read model, which is the class of value
this repository keeps out of its schemas.

The consequence is recorded rather than hidden: if a second account of the same
CLI is ever introduced, the registry key must be revisited. That is a separate
change, not a speculative field added now.

The three-name set is pinned **twice, in two packages, and is not shared by an
import**:

- `TestProviderIdentityIsExactlyThreeAdapterNames` in `internal/provider`
  asserts `CodexAdapter.Name()`, `ClaudeAdapter.Name()` and
  `OpenCodeAdapter.Name()` are exactly those three, and that no fourth adapter
  struct exists in the package.
- `TestProviderRegistryNameTableIsExactlyThreeNames` in `internal/application`
  asserts the registry's own name table is the same three, in the same
  documented order.

`internal/application` must not import `internal/provider`, which
`internal/application/source_guard_test.go` asserts. That keeps the provider
component a leaf in `ci/components.json` and adds no component edge. The cost
is that the two declarations can drift, and those two tests are the only thing
that will catch it. **If a fourth Provider is ever added, both tables must
change together.**

## 2. Health is derived from observations, and nothing is probed

Health is derived only from observations the Loop's own execution path
produced. The registry has **no probe path of any kind**: it starts no CLI,
opens no socket, resolves no name and polls no timer.

This is not a preference. An active health probe *is* an invocation, and every
invocation on the Loop's path passes `internal/runner.CostLedger.Reserve`,
which counts against the runaway-detection thresholds and is settled against
the single-invocation anomaly threshold. A registry that probed on every read
would therefore consume the counters of the detector it exists to report, and
could itself trip the halt it is meant to describe: the observability surface
would become the runaway.

`docs/architecture/validation.md` section 5 lists "provider health probe does
not grow with Requirement count" as a growth rate to verify. A passive
derivation satisfies it **at zero**: the number of Provider CLI invocations
caused by one `GET /v1/providers` is exactly zero. That is a measured fact, not
a claim in a document, because `internal/application/source_guard_test.go`
parses every `*.go` file in `internal/application` with `go/parser`, fails
outright on a zero-file scan, and asserts that no file imports `os/exec`,
`net`, `net/http`, `net/url`, `internal/provider` or `internal/runner`. The
same scan is the only durable defence against a later change quietly adding a
probe.

Observations arrive on requests the Runner already makes:

- `POST /v1/executions/result` gains one additive optional object,
  `provider_observation: {name, failure_class?, stopped_for_inspection?}`.
- `POST /v1/executions/{execution_id}:start` gains one additive optional
  field, `provider`.

Both are optional, so every existing client keeps working unchanged, and both
request schemas carry `additionalProperties: false`, so neither addition can be
made informally.

The observation carries no prompt, no provider response, no session identifier
and no free text. There is no `message`, `detail`, `output`, `result`, `session`
or `text` field on the request DTO, the port type, the stored record or the read
model, so text is *structurally unrepresentable* rather than filtered: no
redaction step can be forgotten, because there is no field to forget.

### The six health values

| health | means | blocked_reason |
| --- | --- | --- |
| `unknown` | nothing has been observed, or the one failure observed carried no classified reason | `never-invoked-by-loop`, or `last-invocation-failed-without-a-classified-reason` |
| `healthy` | the newest observation is a completed invocation | empty |
| `degraded` | the newest observation is a retryable failure (transport, rate-limited, timeout) | `last-invocation-failed-retryably` |
| `unavailable` | the newest observation is a non-retryable failure (model, contract-incompatible) | `last-invocation-failed-non-retryably` |
| `unauthenticated` | the newest observation is `provider-unauthenticated` | `owner-must-authenticate-cli-on-runner-machine` |
| `stopped-for-inspection` | the newest observation reported a runaway-detection stop | `owner-must-clear-the-runaway-stop-with-a-new-approved-record` |

`blocked_reason` is empty **exactly** when health is `healthy`.

`stopped-for-inspection` is a value of its own and not a shade of
`unavailable`, for the reason `docs/operations/provider-live-claude.md`
records: reaching a runaway threshold is a stop for inspection and is neither a
success nor a failure. It is counted in no failure or degraded total anywhere,
and the response carries no such total at all, so there is nowhere for it to be
miscounted.

### Silence is never read as health

A Provider with no observation at all reports `health: unknown`, `stale: true`,
`observation_count: 0` and `blocked_reason: never-invoked-by-loop`. It is never
`healthy`, and it is never `unavailable` either: nothing was measured, so
nothing failed. `unknown` is an absence of observation, not a fault, and the
owner console renders it with that reason rather than as a problem.

This matters because it is the common case, not the exception: today two of the
three Providers have never been driven by the Loop, so a design that defaulted
to healthy would misreport most of the surface.

## 3. The observation window and what `stale` means

`ProviderObservationWindow` is the declared freshness window and is 24 hours.
`MaxProviderObservations` is the retained ring size per Provider and is 32.

- `last_observed_at` is the instant of the newest retained observation, or the
  empty string when the Provider has never been observed. No instant is ever
  synthesized for an absence.
- `observation_count` counts the retained observations that fall **inside** the
  window. It is 0 both for a Provider never observed and for one whose every
  retained observation has aged out.
- `stale` is true whenever the newest observation is older than the window, and
  whenever there is no observation at all.

A stale entry **keeps the health value it was last observed to have** and
reports its age. It is not silently refreshed, and it is not silently
downgraded either: "this Provider was healthy yesterday and has not been
exercised since" and "this Provider has never been exercised" are different
operational facts, and only the second is `unknown`.

The one place age does change a value is `runaway_detection.state`, in the
direction that cannot hide anything (section 4).

The ring is what keeps the read bounded: one keyed document per Provider, three
documents in total, so the read cost of `GET /v1/providers` does not grow with
the Requirement count.

`verified_by_loop_invocation` is deliberately **not** derived from the ring. It
is a sticky stored instant, set by the first observation that completed and
never cleared, because "has the Loop ever completed an invocation through this
Provider" is a monotone historical fact and deriving it from a bounded ring
would let 32 later failures silently un-verify a Provider that really was
exercised.

## 4. Two different limits, two different owners

Two limits were being conflated. They are reported separately, each by the
party that actually owns it.

**`runaway_detection` is runner-local and reports state only.**

```
runaway_detection: { scope, state, thresholds_declared_in }
state: within-thresholds | stopped-for-inspection | unknown
```

There is deliberately **no number** in this object, in the record behind it, or
in the OpenAPI schema that publishes it. The thresholds live in the
owner-approved, digest-bound `provider-preflight` record, and
`thresholds_declared_in` names the approved record instead of copying them.
Republishing them through an unverified Runner report would create a second
copy of a safety threshold that can silently disagree with the approved one -
a second, unapproved safety limit.

`state` is `within-thresholds` only when a *fresh* observation reported no
stop. It is `unknown` when nothing has been observed, and also when the newest
observation reporting no stop is stale, because "the detector was within its
thresholds a day ago" is not a statement about now. A **stop is preserved
whatever its age**: a stop that aged out into silence would be a stop nobody
was told about.

Recorded limitation: `thresholds_declared_in` names the approved standing
authorization by id and the per-exercise record by kind and location, rather
than naming one `provider-preflight` record id. The control plane cannot know
which record a Runner invoked under - the record and the ledger are both on the
Runner machine - and learning it would mean widening `provider_observation`
past the three fields it is closed to.

**`concurrency` is control-plane-owned and reports real quantities.**

```
concurrency: { active_assignments, declared_ceiling, ceiling_source, remaining, exhausted }
```

`declared_ceiling` is the concurrent-Execution ceiling of
`docs/architecture/validation.md` section 5, and `ceiling_source` is
`architecture-design-ceiling` so a design ceiling is never shown as though an
owner had chosen it. V2-068 owns the installation-settable limit and introduces
the second `ceiling_source` value; whichever of the two lands second wires the
registry to the shared accessor. No enum value is declared for a source no code
can produce, because that would be a placeholder.

`remaining` is never negative. The assignment retention bound
(`MaxProviderAssignments`, 24) is deliberately larger than the ceiling (20), so
exceeding the ceiling is reportable - `active_assignments` above the ceiling
with `exhausted: true` - before the bound truncates.

**No field name and no enum value** in the read model or the schema may contain
`budget`, `quota`, `billing`, `spend`, `cost` or `credit`. This is enforced
mechanically over the marshalled JSON and the schema block, with the matcher
itself verified against a known-positive and a known-negative first, because
the requirement is otherwise unenforceable: a later contributor adding a
monetary-sounding field would reintroduce exactly the billing reading the
standing authorization denies.

For the same reason, this package's failure-class vocabulary spells the
rate-limit class `provider-rate-limited` rather than copying
`internal/provider`'s `provider-quota`. The exhaustion that matters
operationally is reported by `concurrency.exhausted` and
`runaway_detection.state`, not by a failure class.

## 5. An unauthenticated Provider, and the exact human action

A not-yet-authenticated Provider is represented as **two separate,
separately-sourced facts**, never as one blurred `connected` flag:

- `authorized` plus `authorization_ref` report the owner's in-repository
  standing authorization.
- `verified_by_loop_invocation` reports whether the Loop has ever completed an
  invocation through this Provider.

Today they disagree for two of the three Providers: the standing authorization
`psa-foundation-001` covers `codex`, `claude` and `opencode`, while only
`claude` has ever completed a Loop invocation. A single `connected` boolean
would have to pick one of the two facts and would then be wrong about the
other.

When `authorized` is true and `verified_by_loop_invocation` is false, health is
`unauthenticated` or `unknown` and `blocked_reason` names the action in plain
words:

- `owner-must-authenticate-cli-on-runner-machine` - an invocation was attempted
  and the CLI reported that it has no session.
- `never-invoked-by-loop` - no invocation has been attempted at all.

**No agent can perform that action.** Signing in to a Provider CLI uses the
owner's own identity, on the Runner machine. The Loop cannot close the gap
itself, and the only way to observe the state without a probe - which section 2
forbids - is the failure class of a real invocation. Reporting the gap
explicitly, with the required human action named, is what makes the
authentication wait observable instead of appearing as an unexplained absence
of activity.

The standing authorization record requires an `approver`, and that value is an
email address. It is **never** carried into the read model, never into a
response, and never into a log line this surface adds. `authorization_ref`
carries the record id and nothing else. The read models in this repository
carry attribution as an actor type plus an opaque subject only
(`domain.RequestedBy`), and an owner console field is not a reason to start
moving a personal identifier through the API.

## 6. Assignment

`ProviderAssignmentRepository` is a side table keyed by Execution id, written
inside `Start`'s existing transaction from the additive optional `provider`
field. `domain.Execution` gains no Provider field and `internal/domain` is not
edited at all.

This follows the precedent `ControlRequestedByRepository` already set:
`domain.Execution` sits inside the transition functions the M1 gate proved, and
widening it would put a label with no transition semantics inside the structure
every state assertion reads, for no gain, since the join Execution id to
Provider name is all the read model needs. Writing it in `Start`'s existing
transaction makes the assignment appear and disappear with the Execution it
describes, so the registry can never report an assignment to an Execution that
was never started.

An Execution that has reached a terminal status is no longer reported as an
active assignment. The terminal rule is applied once, in
`internal/application`, by the same predicate the `Claim` reclaim path uses -
not once per store adapter.

Starting without the field records no assignment and changes no Provider's
`active_assignments`. A `provider` value outside the closed set of three is
refused with 400 **before the transaction opens**, so it records nothing at
all.

Note, recorded rather than hidden: the request fingerprint covers the whole
request, so replaying one `request_id` with a different `provider` is an
idempotency conflict. That is existing semantics, not new behaviour.

## 7. The V2-062 boundary: what the registry cannot see

The registry observes **only the Loop's own execution path**. This is the same
boundary V2-062 recorded for the cost ledger.

A Provider a human drove by hand, outside the Loop, leaves no trace here and is
reported as `unknown` - and a reader who takes `unknown` to mean "broken" has
traded one wrong reading for another. That is why `unknown` is documented here,
rendered with its reason in the owner console, and never presented as a fault.

## 8. What this surface cannot do

- It cannot clear a stop for inspection. There is no mutation path to
  `runaway_detection` at all; clearing a stop requires the owner to issue a new
  approved record, and neither this API nor the console can do it.
- It cannot authenticate a CLI, and no agent can (section 5).
- ~~It cannot decide a handoff. `internal/provider.PrepareHandoff` already
  exists; wiring a handoff decision to this registry is not part of it.~~
  **Corrected by V2-074.** The historical statement is kept above rather than
  deleted, because the design that followed was reached from it. As of V2-074
  the registry does decide: `provider.DecideHandoff` is the single declared
  decision function, it is the only thing that may produce a handoff target,
  and the registry reports its disposition per Provider (section 11). What the
  registry still cannot do is *execute* one: no selection loop exists on the
  production path, so the disposition is a proposal an owner reads and not
  something production acts on today.
- ~~It cannot check version compatibility. The registry introduces no
  version-compatibility check of any kind.~~ **Corrected by V2-074.** The
  historical statement is kept above for the same reason. As of V2-074 the
  registry reports two declared compatibility relations and two verdicts
  (section 9). What it still cannot do is establish that a declared interval is
  *true* of a real CLI; that is a live exercise and V2-028 owns it.
- It performs no write and enqueues no outbox item. The only document a read
  writes is the per-transaction reservation every bounded owner read already
  writes.

## 9. Version compatibility is two relations, not one (V2-074)

Compatibility is defined as **two named relations**, and neither is collapsed
into the other.

| | relation | what it is | capability clause |
|---|---|---|---|
| **R1** | Provider CLI version ↔ **adapter** | the declared, half-open interval of Provider CLI versions one adapter supports | 対応version |
| **R2** | adapter contract ↔ **Loop version** | the declared, half-open interval of Loop release identities that carry that adapter contract | 対応Loop Version |

A verdict is the **conjunction** of the two, and it is `unknown` whenever either
input is absent. `incompatible` wins over `unknown`, because a version measured
outside a declared interval is information and must not be diluted by the
absence of the other input.

**Why compatibility is not defined Provider-CLI-to-Loop directly.** The relation
has to be between the version and the thing that breaks, and the thing that
breaks is measured: the argv each adapter emits, and the envelope shape
`parseFixture` accepts with `DisallowUnknownFields`. A Loop version does not
parse an envelope; an adapter does. Defining the relation directly would force a
new compatibility row for every Loop release that changed nothing about any
adapter, and would make two Loop versions carrying byte-identical adapters
report *different* compatibility - which is false, and would eventually be
"corrected" by widening some interval, that is, by weakening the statement. R2
is keyed to the adapter contract version rather than to a per-adapter value
because that value is already the one a Work Packet and a Handoff refuse a
mismatch on, so it is already the contract identity that crosses a handoff.

**How the interval bounds are chosen.** One stated rule for all three adapters:
the interval starts at the minor floor of the version the adapter's surface was
measured at, and ends at the **narrower** of (a) the next boundary at which that
CLI's own versioning permits a breaking change - the next minor for a 0.x CLI,
the next major otherwise - and (b) the boundary beyond which this repository has
measured nothing at all. codex is 0.x, so both halves give the next minor.
claude's four arguments are live-proven wire-compatible against a real CLI by
three separate exercises, so (b) does not narrow it and the interval runs to the
next major. opencode's argv surface was read from help only and never exercised,
so (b) narrows it to the measured minor line; widening it is V2-028's to earn
rather than this task's to assume.

Neither interval bound is a threshold and neither is a ceiling.

### The authority table

No self-claim is authority anywhere. This is the same rule V2-069 applied to a
Runner's version report, applied to a second kind of claim one machine further
away.

| fact | authority |
|---|---|
| which Provider CLI versions an adapter supports | the source-declared interval in `internal/provider`, and nothing a CLI reports about itself |
| which Loop versions carry that adapter contract | the Loop's own release identity read through the existing `ReleaseObserver`, and not a Runner's self-report |
| what version a CLI actually is on a machine | the owner-approved, digest-bound provider-preflight record's own `--version` measurement, and not a version string inside a Provider response envelope |
| whether the declared support statement is **true** of a real CLI | nothing in this repository; only a live exercise, and V2-028 owns it |

The last row is the one a reader must not skip. Everything this surface reports
is a property of a **declaration** and of the code that reads it. That a real
CLI at a version inside a declared interval actually accepts the argv and
produces the envelope is unmeasured here.

### No observed CLI version reaches the control plane, and the DTO was not widened

`ProviderObservationInput` carries exactly three fields - a name, a closed
failure class and a boolean - and an existing test pins that list. A fourth
field would turn that test red, and a sibling object on the same request would
satisfy the letter of the test while defeating its purpose. It would also have
no producer: no Runner in this repository posts anything.

So the observed-CLI side of the verdict reaches the registry only through the
failure class the Loop's own execution path **already** reports:
`provider_observation.failure_class = contract-incompatible`, which already maps
to health `unavailable` and blocked reason
`last-invocation-failed-non-retryably`. `cli_compatibility` is therefore
`unknown` for every Provider until such a failure is observed, and there is
deliberately no `observed_cli_version` field at all.

The consequence, stated rather than implied: **a reader may take `unknown` as
compatible, and would be wrong.** The only honest mitigation is the one applied -
`unknown` is a sibling value of `compatible` in the same closed set, it is
rendered as `unknown` and never as blank, and it is never a default a missing
input silently falls into. Closing it needs a Runner client, and that owner is
named in section 12.

### The pre-invocation refusal, and why it is unarmed in production

`provider.Request` gained exactly one optional field, `CLIVersionDeclared`, and
the shared `build` helper refuses fail-closed when it is non-empty and measured
outside the adapter's declared interval - before any argv exists, so an
incompatibility costs no invocation. It fails **open** on an empty or unreadable
value, because rounding `unknown` to `incompatible` would take a Provider out of
service on an absence of information.

**No production caller supplies the field.** `internal/runner` is deliberately
not edited, so the refusal is exercised by tests and not by production. A reader
who takes the surface as proof that an incompatible CLI cannot be invoked would
be wrong. Arming it is named in section 12.

## 10. When work moves: the `Sendable` predicate and the trigger (V2-074)

The trigger is **not** a `FailureClass` and **not** a usage-window state alone.
It is the negation of a declared `Sendable` predicate over three inputs,
evaluated for the source Provider.

```
Sendable(circuit, window, slot)
circuit: sending | not-sending | probing
window:  within-window | exhausted | unknown
slot:    available | in-use | cooling-down | unauthenticated | quarantined | stopped-for-inspection
```

| axis | value | Sendable | why |
|---|---|---|---|
| circuit | `sending` | yes | |
| circuit | `not-sending` | no | |
| circuit | `probing` | no - but it **waits**, it does not hand off | one invocation is already being spent to get the answer; moving work would discard it |
| window | `within-window` | yes | |
| window | `unknown` | yes | an invocation reported no usage at all; that is an absence of information, not an exhaustion |
| window | `exhausted` | no | our own attempt ceiling is reached |
| slot | `available` | yes | |
| slot | `in-use` | yes | a lease is outstanding; the Loop is already using this Provider |
| slot | `cooling-down` | no | |
| slot | `unauthenticated` | no | the required action is the owner's |
| slot | `quarantined` | no | the required action is the owner's |
| slot | `stopped-for-inspection` | no | the required action is the owner's |

The predicate is total over the full 3 × 3 × 6 cross product with no default
branch: an undeclared member of any axis is refused, not defaulted. The test
enumerates the product rather than sampling it.

### Each rejected alternative, and the row it contradicts

- **A `FailureClass` alone** contradicts three rows of the breaker's own opening
  table. `cancelled` and `invalid-input` are declared there as *not observations
  about the Provider* and must never move work. `provider-transport`, `timeout`
  and `unknown` are `count-toward-windowed-threshold`, so handing off on a first
  occurrence would defeat the threshold that exists precisely to avoid reacting
  to one blip. `provider-model` opens only the narrower Provider-and-model
  circuit, whose stated prescribed action is *to evaluate other model candidates
  in the same pool* - impossible if the whole Provider moved.
- **The circuit alone** is necessary but not sufficient: exhausting our own
  attempt ceiling is not a Provider fault and produces no failure class at all,
  so the window's `exhausted` state would reach nothing.
- **The window alone** is insufficient and dangerous, because `unknown` means an
  invocation reported no usage, and rounding it to `exhausted` would move work
  on an absence of information.

### The window's exhaustion is not routed through the breaker

Opening a circuit **requires** a failure class: the open records the observed
class and every report carries those classes as its stated reason. So routing an
own-ceiling exhaustion through the breaker would require inventing a class for
it, and every candidate is a lie - a quota class would claim the Provider
signalled exhaustion when it did not, `unknown` would claim we could not
classify something we classified exactly, and a new class would need a row in
the opening table and would then appear in the circuit's stated reason as though
the Provider had done something. `Sendable` therefore reads the window directly,
alongside the circuit. No failure class is synthesized anywhere, the opening
table is not edited, and `ApplyObservation`'s behaviour is unchanged.

### The measured defect this closed, and the residual that stands

A Provider whose pool slot is `unauthenticated` had - and still has - a
**sending** circuit: the breaker creates every circuit closed regardless of the
slot, a closed circuit reports `sending`, and the opening table's action for
`provider-unauthenticated` moves the pool slot *without* opening the circuit. So
the existing conversion path, which consults only the breaker, accepted a
Provider with no authenticated session as a handoff target. A test reproduces
that as a **positive control** and keeps it, because it is why the decision
consults the slot.

The fix is at the decision, not at the conversion: `Sendable` reads the slot and
`DecideHandoff` is the only thing that may produce a handoff target. The
conversion's signature and every one of its verdicts are unchanged, because
widening its parameters would stop the existing tests compiling under their own
names. **Residual:** a caller who hand-picks a target and calls the conversion
directly can still reach such a Provider. An AST guard asserting exactly one
non-test caller is what makes that unreachable in this tree, and **a guard is
weaker than a signature.** The owner is named in section 12.

## 11. The disposition, its target filters and its waiting reasons (V2-074)

One declared decision function is the **only** thing that may produce a handoff
target. It returns a closed disposition with either a target or exactly one
closed waiting reason.

```
handoff: { disposition, target, waiting_reason }
disposition: none | handoff-proposed | waiting
```

The decision table is declared over a normalised input tuple - the source state,
whether the chain bound is reached, and the highest-precedence obstacle observed
across the candidates - and is enumerated over the full 3 × 2 × 5 cross product:

| source | chain bound reached | candidate obstacle | disposition | waiting reason |
|---|---|---|---|---|
| `sendable` | either | any | `none` | - |
| `probing` | either | any | `waiting` | `source-is-probing` |
| `not-sendable` | yes | any | `waiting` | `chain-bound-reached` |
| `not-sendable` | no | `none` | `handoff-proposed` | - |
| `not-sendable` | no | `owner-action-needed` | `waiting` | `candidate-needs-an-owner-action` |
| `not-sendable` | no | `measured-incompatible` | `waiting` | `candidate-is-measured-incompatible` |
| `not-sendable` | no | `already-tried-for-this-increment` | `waiting` | `candidate-already-tried-for-this-increment` |
| `not-sendable` | no | `not-sendable` | `waiting` | `candidate-is-not-sendable` |

Candidates are scanned in the declared `codex, claude, opencode` order - the
order the standing authorization record's enum uses - so the same state twice
produces byte-identical results. The obstacle precedence, most actionable first,
is `owner-action-needed`, `measured-incompatible`,
`already-tried-for-this-increment`, `not-sendable`: an owner action is first
because it is the only one a person can clear.

Three target filters, each a refusal and not a preference:

- a candidate the owner's standing authorization does not cover, **and** one
  whose slot needs an owner's action, are both refused;
- a candidate whose declared version is measured incompatible is refused, while
  one whose compatibility is **unknown** is *not*;
- a candidate already tried for this Increment is refused, folded out of the
  bounded assignment ring the read already returns.

`handoff-proposed` is a **proposal an owner reads**. Nothing in this repository
executes it: no Provider selection loop exists on the production path, so this
surface does not mean that work has moved or will move. Reading it changes
nothing.

### The two waiting vocabularies share no member

The word "waiting" now exists twice in the system with two meanings. The two
vocabularies are disjoint, and a test asserts it in both directions, so an owner
cannot read one as the other.

| V2-074 handoff waiting reason | V2-068 queue waiting reason |
|---|---|
| `source-is-probing` | `not-ready` |
| `chain-bound-reached` | `unmet-dependency` |
| `candidate-needs-an-owner-action` | `repository-unavailable` |
| `candidate-is-measured-incompatible` | `already-owned` |
| `candidate-already-tried-for-this-increment` | `resource-conflict` |
| `candidate-is-not-sendable` | `no-runner-capacity` |
| | `not-executable` |

The left column answers "why is this Provider not receiving work". The right
column answers "why is this Requirement not being allocated a slot in the shared
concurrency limit". Neither is derived from the other, and each is produced by
its own real state.

### One measured asymmetry between the two declarations

The table is declared twice - in `internal/provider` and in
`internal/application` - because the application layer may not import the
provider layer. Each side maps its own observations onto the shared tuple, and a
pinning test compares the two cell by cell.

The `probing` source state is **unreachable from the control-plane side**. A
probing circuit is a state of the breaker on the Runner machine, and no
observation carries a circuit state to the control plane. So this surface reports
`sendable` and `not-sendable` but never `probing`, and the shared table's
`probing` rows are exercised only on the provider side. Widening the observation
to carry it would turn an existing test red.

## 12. What a fixture can show, what only V2-028 can, and the residuals (V2-074)

**Established here, by fixtures and unit tests, in process, with no Provider CLI
started and no invocation of any kind made:**

- the compatibility decision total over its inputs with an explicit `unknown`;
- the three `cli-version-incompatible` fixtures producing `contract-incompatible`
  for all three adapters, and the resulting open being immediate, never
  probe-eligible, and closable only by an explicitly observed version change;
- the disposition table total and deterministic;
- the target filters enforcing authorization, declared compatibility and the
  attempt history;
- a chosen disposition followed by the existing conversion preserving the
  carried content digest across all six ordered Provider pairs, with a refusal
  on any single-field loss.

**Established by V2-028 alone, and by nothing here:**

- that a declared interval is **true** of a real CLI;
- that a real exhaustion or a real contract break produces the class this table
  assumes;
- that a real handoff preserves the Increment, Artifact and Evidence through a
  real journal, a real lease and a real Evidence record;
- that the waiting reason an owner sees in a Preview journey is the one the
  capability's journey requires.

Contract-fixture evidence satisfies no real-environment condition at any grade.
Nothing above is a release gate, an M6 completion condition, or a claim that
production hands off today.

**Residuals, each with an owner:**

| residual | owner |
|---|---|
| the pre-invocation refusal is unarmed: no production caller supplies `CLIVersionDeclared`, because `internal/runner` was deliberately not edited | the task that adds a Runner-side Provider selection path (V2-028's successor); until then, tests only |
| the conversion keeps its signature, so a caller who hand-picks a target can still reach a Provider whose slot needs an owner action; an AST single-caller guard is weaker than a signature | the task that introduces the first real selection loop, which is the first point at which widening the signature costs nothing |
| the observed-CLI verdict is permanently `unknown` until a Runner client exists, because no observed version reaches the control plane and widening the observation DTO would turn an existing test red | the task that adds a Runner HTTP client |
| two of the three Provider CLIs have no authenticated session on this machine and no agent can create one, so a real handoff between two real Providers is **owner-blocked**, not merely deferred: authenticating a CLI uses the owner's own identity | the repository owner |
