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
- It cannot decide a handoff. `internal/provider.PrepareHandoff` already
  exists; wiring a handoff decision to this registry is not part of it.
- It cannot check version compatibility. The registry introduces no
  version-compatibility check of any kind.
- It performs no write and enqueues no outbox item. The only document a read
  writes is the per-transaction reservation every bounded owner read already
  writes.
