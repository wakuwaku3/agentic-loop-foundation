# Shared resource allocation, locally

This document describes what `V2-068` wired up: the installation-wide
concurrency limit an owner can set, and the allocation report
`GET /v1/queue/summary` adds. It is the installation-wide half of
`cap-shared-resource-allocation` only. The per-Provider half is `V2-028`'s and is
surfaced by `V2-067`'s `GET /v1/providers`; the multi-Repository half is
`V2-030`'s local closure and `V2-031`'s M7 gate. **This task passes neither the
M6 nor the M7 gate.**

Run everything below with `make component-application` and
`make component-api`, or
`go test -run 'Allocation|QueueSummary|Waiting|Exhaustion|Ceiling' ./internal/application ./internal/api`.

## The limit is a side table, not a control mode

`POST /v1/controls` accepts one additive optional object:

```json
{"request_id":"...","scope_kind":"installation","scope_value":"install","mode":"allow",
 "allocation_limit":{"installation_concurrent_executions":6}}
```

The value is stored in `AllocationLimitRepository`, a side table keyed by the
Control Intent revision the request creates, written at most once per revision,
in the same transaction as the Control Intent itself. It follows
`ControlRequestedByRepository` exactly, and for the same reason:
`domain.ControlIntent` is immutable, proven-closed M1 surface, and the limit is
an attribute of the request rather than of the policy the permit gate evaluates.

**`domain.ControlMode` keeps exactly its seven values, and a new mode would have
been actively harmful rather than merely redundant.** `domain.EffectFromPermit`
refuses unless the effective mode is `domain.ControlAllow`, so any other mode
denies every durable effect the Loop needs: `Claim`, `AcceptResult` and all four
outbox kinds. A `limit-concurrency` mode would therefore **stop** the Loop
instead of throttling it, and making it not stop the Loop would mean widening
the one gate the M1 gate proved closed. No permit decision reads the limit at
all: it is an input to the allocation decision, which happens before any permit
is requested, and it appears in no `PermitRequest`, no `PermitDecision` and no
`Effect`.

## The effective limit, and why a later revision does not clear it

**The effective limit is the limit attached to the greatest Control Intent
revision that declared one. A later revision that declares no limit does not
clear it.**

A revision that declares no limit simply has no row in the side table, so there
is nothing for it to clear. The rule is what stops an unrelated `pause-claim`
intent from silently resetting the owner's allocation policy, which is exactly
what "the latest revision wins outright" would do. Resolution is deterministic
and reads no clock.

With no revision having ever declared a limit, the reported ceiling is
`docs/architecture/validation.md` section 5's design ceiling of 20 concurrent
Executions, and `limit_source` says `architecture-design-ceiling` so a number
nobody chose is never shown as a policy somebody set. Once a revision declares
one, `limit_source` is `control-revision` and `control_revision` names it.

## The accepted range is 1..20, and 0 is rejected

The upper bound is the architecture ceiling rather than an invented number, so
this surface cannot promise a concurrency the documented design does not
support. It is the same figure the per-Provider half reports as
`ProviderConcurrencyDesignCeiling`.

**Zero is rejected with 400 rather than treated as a stop.** A limit of zero
would be a second way to halt the Loop that the control-verification pipeline
does not observe, and `pause-claim` already exists for that with a proven
verification path. A stop must go through the mode whose blast radius the stop
matrix measures. A rejected limit is refused before any transaction opens, so it
creates no Control Intent, stores no row and records no event.

## A lowered limit revokes nothing

**Lowering the limit below the work already running does not cancel a lease,
drop a claim or re-fence anything.** The limit is consulted at exactly one
place: the capacity of the single installation pool entry in the `Snapshot` built
for the *next* allocation. Existing Leases and Claims enter that Snapshot as
read-only values that `scheduler.Decide` only reads, and `scheduler.Apply` --
which this package never calls -- only appends Claims and only moves a
Requirement from ready to assigned. **There is no code path in
`internal/scheduler` that removes a Claim or revokes a Lease at all**, which is
asserted by an identifier scan over that package rather than by review.

The installation therefore converges to the new limit as work completes. While
it has not, the report says so: `exhausted` is true and `binding_limit` is
`installation-concurrency`. Raising the limit again makes waiting work
assignable on the very next read, with no repair step, no reconcile tick and
nothing released by hand.

## The waiting reasons are the scheduler's own closed set

`waiting.by_reason` reports `V2-030`'s `Decision` reason constants projected,
never a second vocabulary invented here:

`not-ready`, `unmet-dependency`, `repository-unavailable`, `already-owned`,
`resource-conflict`, `no-runner-capacity`, `not-executable`.

All seven buckets are reported on every read, zero included, so the key order is
fixed and the closed set is visible in the response itself. A table test asserts
the projection is total in both directions and fails if `internal/scheduler`
adds a constant with no mapping here.

**Four of the seven can actually occur in the running system today, and the
other three cannot. That is a measured limitation of what the store holds, not
of the projection**, and each one names the durable input it lacks:

| reason | occurs today | why not |
| --- | --- | --- |
| `not-ready` | yes | |
| `already-owned` | yes | |
| `resource-conflict` | yes | |
| `no-runner-capacity` | yes | |
| `unmet-dependency` | no | `domain.Requirement` declares no dependency field, so `Snapshot.Requirements[i].Dependencies` can never be populated from stored state |
| `not-executable` | no | `domain.PriorityAssessment` has no persistence port anywhere in the repository, so `Snapshot.Requirements[i].Assessment` can never be populated from stored state |
| `repository-unavailable` | no | the Snapshot models the installation as one synthetic Repository with no failure count and no isolation deadline; making this reachable means modelling per-Repository availability, which is `V2-030`'s local closure and `V2-031`'s M7 gate |

## Exhaustion is counted from capacity, not read off a reason

`exhaustion` is computed from the capacity accounting the caller itself supplied
to the Snapshot, and never by re-interpreting a candidate's rejection reason.
That distinction is load-bearing: **work can be waiting on a resource conflict
while capacity remains**, and the report must not call that exhaustion.

- `exhausted` is `active >= limit`.
- `binding_limit` is `installation-concurrency` when the number reached is one an
  owner declared on a Control Intent revision.
- `binding_limit` is `runner-capacity` when it is the pool's own capacity, which
  today is the architecture design ceiling, because there is no enrolled-Runner
  registry to measure a real pool from: `RunnerObservationRepository` is keyed by
  id and has no list operation.
- `binding_limit` is `none` when capacity remains.

Splitting `no-runner-capacity` inside the scheduler was deliberately not done:
`chooseRunner` returns the same negative result whether a Runner's own capacity
is full or a provider-wide entry binds, and `V2-030`'s closed set expresses both
as that one reason. Changing it would be re-designing a scheduler decision rule.

## The report calls `Decide` and never `Apply`, and writes nothing

The whole report is computed on read, inside the same transaction that reads the
requirements, increments, executions and leases it describes. It is not cached on
a reconcile tick: a cached value would describe an allocation the current state
no longer supports. It calls `scheduler.Decide` and never `scheduler.Apply` --
calling `Apply` from a read path would change Requirement status and add Claims
as a side effect of an owner pressing Refresh.

Measured: one `GET /v1/queue/summary` performs **zero writes**, stages **zero
outbox items** and makes **no mutation quota reservation**. The only reservation
is the one bounded read-transaction reservation every owner read already makes.
`allocation.planned_assignments` is the length of the plan the scheduler returned
in that transaction and nothing else; it is never the active Execution count
reported as an allocation, and a read that plans nothing reports zero.

### Measured read cost

The candidate input is bounded at the scheduler's own `MaxCandidates` of 100, and
the builder fails closed above it rather than truncating silently. Against the
Firestore emulator, one `GET /v1/queue/summary`:

| stored Requirements | documents read | documents written |
| --- | --- | --- |
| 10 | 53 | 1 |
| 50 | 133 | 1 |
| 100 | 233 | 1 |
| 150 | 234 | 1 |
| 300 | 234 | 1 |

These are observations, not thresholds. The property that *is* asserted is the
last two rows being equal: beyond the candidate bound the cost stops growing with
the Requirement count. It settles one document above the figure at exactly the
bound, because the page read asks for `limit+1` rows to report whether more
exist, and beyond the bound there is exactly one such row and never more. One
read over 100 candidates completes inside a 30 second deadline, which is section
5's own reconcile tick deadline and is the assertion.

## What is modelled, and what is therefore not reported

There is one Installation, one implicit Repository and one runner pool in this
Snapshot:

- one synthetic Repository, with no failure count and no isolation deadline,
  because there is no durable source for either;
- one installation pool entry whose capacity is the effective limit and whose
  active count is the `active_executions` figure the queue counters already
  publish;
- one Claim per active Lease, owned by the Requirement that Lease's Increment
  belongs to.

Every one of those synthetic identifiers is asserted **absent** from the
marshalled response, so the modelling choice cannot leak into a surface an owner
would then read as a real entity.

The one value that is not synthetic is the contention key: a Requirement contends
for the Repository it is linked to through `V2-071`'s write-once
Requirement-to-Repository association, and a Requirement with no link contends
only with itself. Treating every unlinked Requirement as contending for one
installation-wide resource would serialise the whole installation to a single
Execution and make the concurrency limit unobservable, which is the opposite of
what this surface exists to show. This is a contention key only: no
per-Repository figure is reported anywhere, and `Snapshot.Repositories` stays the
single synthetic Installation Repository, so no per-Repository availability rule
is introduced here.

Not reported, and owned elsewhere: per-Provider availability and limits
(`V2-028`, surfaced by `V2-067`), per-Repository execution permission and
concurrency (`V2-030` local closure, `V2-031` M7 gate), and per-Requirement
priority with its stated reason (`V2-030` owns the assessment; nothing persists
`domain.PriorityAssessment` yet).

## The capture-time limitation, restated as measured

`domain.Requirement` **does** carry a capture time: `CapturedAt`, added by
`V2-073`, with `CaptureRecorded()` distinguishing a real value from a legacy
record that predates the field. The Snapshot builder uses it directly.

**The mapping rule for a Requirement that has none: it is ordered as if captured
at the Snapshot's `Now` -- age zero -- never as an unbounded age.** `V2-073`
declared that rule and left its application here. It is a rule and not a
workaround: `now.Sub(time.Time{})` saturates at the maximum `time.Duration`,
measured at 9223372036 seconds -- about 292 years, because `time.Duration` is
int64 nanoseconds -- while the largest priority term in the score is 30000, so a
candidate handed the zero instant would outrank everything by a factor of
307445 regardless of its priority. Age zero is the conservative direction: a
missing value makes a Requirement the least privileged rather than the most, so
the failure mode of an absent value is a delayed Requirement rather than a
starved queue. No substitute timestamp is manufactured anywhere: the value is
the Requirement's own recorded instant or the Snapshot's `Now`, never a second
clock read and never a scan of the event log.

**The limitation that remains is not a missing field. It is that nothing in
production bounds the spread between competing Requirements' capture times.**
`V2-030` proved its starvation bound with every candidate sharing one instant, so
the age term cancelled out of every comparison; with real capture times it does
not. Writing `D` for the number of seconds by which a flood's capture time
precedes a waiter's, that bound is a true statement exactly for
**`D` in [-6500, +3499] seconds**, and the one-tick margin
`StarvationBoundTicks` records exists only for **`D` below 1000**. The
derivation and the measurements at both endpoints are in
`internal/scheduler/capture_time_test.go` and
`docs/operations/scheduler-local.md`; `V2-073` escalated the gap to the
tech_lead, and this task does not close it. Enforcing a chosen guarantee would
be a change to a scheduler decision rule, and choosing which guarantee is a
scheduling policy decision the tech_lead owns.

## Owner console

The console has a `Shared resource allocation` section. It carries its own
control form -- including the bounded numeric limit input -- and renders the
allocation, the waiting breakdown in words per scheduler reason, and the
exhaustion state. The limit is sent only when the field is non-empty, so
submitting with it empty changes nothing about the limit. It renders no raw JSON,
adds no timer and references no external asset, script or font.

The section carries its own form rather than adding a field to the pre-existing
Control form because that form's submit handler is part of the first, single-line
block of `owner.js`; adding an input there without rewriting that handler would
render a field the page never sends. Every block in `owner.js` and `owner.html`
is appended and self-contained so that no task rewrites another's.

## Hand-off: `dp-v2-068` d9's measured premise is stale, and what that unlocks

`dp-v2-068` d9 justifies the single synthetic Repository with a measurement:
"There is no Repository aggregate and no enrolled-Runner registry in the store
today: `RunnerObservationRepository` is keyed by id with no list operation, and
`domain.Requirement` carries no repository."

**Half of that is false at this commit, and this note is for whoever owns the
next step.** Measured:

- `domain.Repository` exists, with a status, a source locator and a bounded forge
  Observation, and `RepositoryRepository` is a member of
  `application.UnitOfWork` -- `V2-064` landed it.
- A Requirement's Repository is durably recorded, in the write-once
  Requirement-to-Repository link -- `V2-071` landed it. This task already reads
  it, as the contention key.
- Only the enrolled-Runner half of d9's premise still holds:
  `RunnerObservationRepository` is still keyed by id with no list operation, so
  there is still nothing to enumerate a real pool from.

**What that unlocks, and why it was deliberately not taken here.** Feeding the
real link and the real `domain.Repository` records into `Snapshot.Repositories`
-- one entry per referenced Repository, omitted or isolated when the Repository
is retired or its executability is not `executable` -- would make
`repository-unavailable` observable, taking the reachable reason count from four
to five, and would let the report distinguish "this Repository cannot be worked"
from "this Requirement is not ready". That is **per-Repository availability
modelling**, which is `V2-030`'s local closure and `V2-031`'s M7 gate, not this
task's. Claiming it here would either duplicate or re-open another task's scope,
so it was measured, recorded and left. The synthetic Repository stays, and
`repository-unavailable` stays unreachable, until the owner of that half takes it.

This is the fourth premise in `wo-v2-068`/`dp-v2-068` that later work falsified.
The others: `A14`'s "`domain.Requirement` carries no capture timestamp" (false
since `V2-073`); `A20`'s "`ci/components.json` declares application's
dependencies as `[domain]` and scheduler's as `[]`" (measured `[domain, infra,
release]` and `[domain]`); and `d13`'s "`internal/application` imports
`internal/quota` whose roots belong to the infra component with no declared
edge" (the `infra` edge is declared). A Work Order's measurements age; measure
again before relying on one.
