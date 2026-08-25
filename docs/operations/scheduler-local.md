# Local scheduler core

`internal/scheduler` is a pure local planning boundary for one installation
backlog. It accepts a bounded `Snapshot` (at most 100 candidates), a
repository registry, runner/provider capacities, resource claims, and the
dependency DAG, then returns a deterministic `Plan`. It performs no Firestore,
network, process, or provider operation. This is mechanically enforced, not
just asserted in prose: `source_guard_test.go` parses every `*.go` file in the
package with `go/parser` and fails if any file imports anything other than the
Go standard library or `internal/domain`, and fails outright if the scan reads
zero files.

Requirements are ordered by priority and age, with a stable ID tie-breaker.
Dependencies must be completed; cycles fail closed. A requirement related to
multiple repositories is assigned to one runner for every repository or not at
all. A repository with three or more failures, or an active isolation deadline,
is excluded until its failure state is cleared. Provider capacity is shared
across runners when `ProviderCapacity` is supplied.

Claims are repository-scoped by default: read/read may share, while a write
conflicts with any claim on the same resource and repository. Set
`ResourceRequest.Global` explicitly for an installation-wide resource: a
global claim's recorded repository id is empty, so it excludes a same-named
global request raised from any repository, while a repository-scoped claim
never excludes a request in a different repository.
`Apply` validates the complete repository set and returns the original snapshot
on any conflict, providing the cross-repository rollback boundary. Persistence
adapters should apply this result in their own transaction/compare-and-swap
boundary; the scheduler itself intentionally has no UnitOfWork dependency.
Rollback converges rather than oscillates: the Decide/Apply cycle immediately
following a rolled-back conflict reaches a complete assignment from the
unmodified pre-conflict snapshot, and a third cycle from that assigned state
is a no-op.

## Priority: multi-factor assessment, with an identical scalar fallback

A `Requirement` may optionally carry a `domain.PriorityAssessment`. When it
does not, the score is the pre-existing scalar formula
(`clamp(Priority, 0, 100) * 300 + age_seconds`), unchanged bit-for-bit from
before this multi-factor connection existed. When it does, the score is a
weighted sum of the assessment's `ValueScore`, `UrgencyScore`, `RiskScore`,
`DependencyScore`, and `LearningScore` (all positive contributions, in
descending weight), minus a weighted `ResourceCost`, plus a weighted
`StarvationRisk` (the single largest weight, because it is what the
starvation-prevention bound below depends on), plus the same 1-point-per-second
age term. Every raw factor is clamped to `[0,100]` before weighting. The exact
weights and the reasoning behind each factor's sign and magnitude are written
down as a comment in `priority.go`. `Assessment.Executable == false` excludes a
candidate from assignment regardless of its score.

## Decisions: one auditable record per candidate

`Plan.Decisions` carries one record per candidate Decide considered: the
factor inputs actually used, the computed score, its rank in the sorted
order, whether it was assigned, and -- when it was not -- exactly one reason
drawn from a closed set of exported constants (`ReasonNotReady`,
`ReasonUnmetDependency`, `ReasonRepositoryUnavailable`, `ReasonAlreadyOwned`,
`ReasonResourceConflict`, `ReasonNoRunnerCapacity`, `ReasonNotExecutable`).
When the candidate carried a `domain.PriorityAssessment`, that assessment's
`Version`, `Reason`, and `ReevaluateWhen` are carried through into the
Decision unchanged.

## Starvation prevention

`StarvationBoundTicks` (currently 5) is the maximum number of Decide/Apply
ticks a waiting, high-value Requirement in one Repository may go unassigned
while a second Repository's failure-and-retry flood keeps consuming all
runner capacity, once the waiting Requirement's `StarvationRisk` is being
raised on each re-assessment. It is derived from the weight table above
(`StarvationRisk` carries the largest weight of any factor), not tuned to fit
a scenario after the fact; see `starvation_test.go` for the worked numbers
and the negative control proving the same scenario never converges once the
`StarvationRisk` term is neutralised. This is a different property from
failure-storm isolation (an unhealthy repository being excluded while a
healthy one stays schedulable): starvation prevention is about one *healthy*
repository's mass demand not permanently starving another healthy
repository's important work.

## Reconcile budget

A 100-candidate tick spread over multiple repositories and runners completes
Decide+Apply well inside the 30-second budget `docs/architecture/validation.md`
section 5 fixes for one reconcile tick; the 30-second `context.WithTimeout` is
the assertion, and the measured duration at 10/50/100 candidates is logged as
an observation only, never as a threshold (so this budget check cannot become
flaky). The 100-item candidate bound itself (`MaxCandidates`, or an explicit
`Snapshot.CandidateLimit`) still fails closed with `ErrCandidateLimit` one
item over the limit.

Run the focused gate with `make component-scheduler` or
`go test ./internal/scheduler`. The tests cover two repositories/two runners,
double-owner rejection, failure-storm isolation, dependency cycles, shared
capacity, claim modes, candidate bounds, partial-plan rollback and its
convergence, the multi-factor priority connection, the closed Decision reason
set, the installation-wide `Global` resource scope, the starvation bound with
its negative control, the reconcile budget, and the package's own import
purity.

## Scope: this is the local closure, not the M7 gate

Every property above is proven inside one process and one test binary, with
injected clocks and no second OS process, container, or namespace: it is a
local closure over in-memory `Snapshot`/`Plan` records. The M7 completion
condition additionally requires two or more independent Runner execution
entities across two or more Repositories with injected clock skew between
them -- that is `internal/scheduler`'s own planning purity plus an execution
substrate this package structurally cannot witness on its own, and it remains
V2-031's scope.

## The Requirement's capture time, and the interval in which the starvation bound is true (V2-073)

A `domain.Requirement` now carries `CapturedAt`, the instant it was captured.
The value is set exactly once, at intake, from the transaction's authority
time -- the same instant the `requirement.captured` event carries -- so the
record and its own event can never disagree, and a Firestore retry of the
transaction callback cannot move it. It is not read from a second clock call,
it is not derived from a read time, it is not derived from a scan of the event
log, and `CaptureRequest` carries no field for it, so a caller cannot supply
it. `GET /v1/requirements` and `GET /v1/requirements/{id}` report it as
`captured_at`.

### A Requirement with no recorded capture time

A Requirement recorded before the field existed carries the zero value.
`Validate` accepts it, and the read views omit `captured_at` **entirely**
rather than emitting `0001-01-01T00:00:00Z`: the zero instant reads as a real
instant in the year 1, and an ordering rule that rewards age would read every
such record as maximally old and therefore maximally privileged.

**The mapping rule, in one sentence: a Requirement with no recorded capture
time is ordered as if captured at the snapshot's `Now` -- age zero -- never as
an unbounded age.** The rule is declared and proven here
(`internal/scheduler/capture_time_test.go`); the task that builds the
`Snapshot` applies it. Nothing in production constructs a `scheduler.Snapshot`
at this commit, and V2-068 owns the builder, so wiring `CapturedAt` into
`Snapshot.Requirements[i].CreatedAt` -- with this rule for the absent case --
is V2-068's item, not this one's.

Why age zero and not the zero instant: `ageSeconds` clamps only the negative
side, and `now.Sub(time.Time{})` saturates at the maximum `time.Duration`,
which is 9223372036 seconds (about 292 years). `legacyScore` is
`legacyPriority(p)*300 + age`, so a priority-100 candidate scores at most
30000 from priority; a candidate presented with the zero instant scores
9223372036 from age alone and outranks everything regardless of priority. Age
zero is the conservative direction: a missing value makes a Requirement the
least privileged rather than the most, so the failure mode of an absent value
is a delayed Requirement rather than a starved queue.

### The capture-time spread interval

V2-030 proved the starvation bound with **every candidate sharing one
`CreatedAt`**, and its own comments say so three times: the age term is
identical across the scenario and cancels out of every comparison. Feeding
real capture times removes that cancellation, and the cancellation is what the
proof rested on. The bound is therefore a conditional statement.

Let **D** be the number of seconds by which the flood candidates' capture time
precedes the waiter's. From the declared numbers only:

- flood score = `legacyPriority(100)*300 + age` = `30000 + age_waiter + D`
- waiter score = `50*weightValue(400) + 10*weightUrgency(350) + StarvationRisk*weightStarvationRisk(500) + age` = `23500 + 2500*(tick-1) + age_waiter`
- flood ids sort before `important`, and `Decide`'s comparator gives a tie to
  the lexically smaller id, so the waiter must be **strictly** greater:
  `2500*(tick-1) > 6500 + D`
- with `StarvationBoundTicks = 5`, convergence inside the bound requires
  `D <= 3499`
- the negative control (the same scenario with the `StarvationRisk` term
  neutralised) stays non-convergent only while `23500 <= 30000 + D`, that is
  `D >= -6500`

**V2-030's two starvation tests are true statements exactly for
D in [-6500, +3499] seconds.** Measured, at one second per tick:

| D (seconds) | converges on tick | margin vs the 5-tick bound | verdict |
| --- | --- | --- | --- |
| -6501 | 1 (negative control, StarvationRisk neutralised) | -- | **positive control: the proof stops attributing convergence to the StarvationRisk term** |
| -6500 | 2 (negative control: never, in 20 ticks) | 3 | inside the interval, floor endpoint |
| -1 | 4 | 1 | inside |
| 0 | 4 | 1 | inside; identical to V2-030's own scenario |
| +999 | 4 | 1 | inside; last D with a margin |
| +1000 | 5 | 0 | inside, no margin |
| +3499 | 5 | 0 | inside, ceiling endpoint |
| +3500 | 6 | -1 | **positive control: the declared bound is exceeded** |

The "one-tick margin" `StarvationBoundTicks`'s own comment records therefore
exists **only for D below 1000 seconds**. From 1000 to 3499 seconds
convergence lands on the bound itself with no margin at all.

### Escalation: nothing in production bounds D

A Requirement's capture time is whatever instant its intake transaction
happened at. **Nothing in production constrains the spread between the capture
times of competing Requirements**, so a flood whose candidates were captured
more than 3499 seconds (58 minutes 19 seconds) before the waiter can exceed
the declared starvation bound in a running system. This is recorded, not
fixed. Every available remedy -- raising the `StarvationRisk` step,
normalizing or capping the age term, raising `StarvationBoundTicks` -- is a
change to a scheduler decision rule, which V2-073's non-goals forbid, and
choosing among them means deciding what production should guarantee about
capture-time spread, which is a scheduling policy question. **The tech_lead
owns that follow-up decision; V2-068, as the owner of the `Snapshot` builder,
is where a chosen guarantee would be enforced.** No weight, clamp, comparator
or bound was changed here, `starvation_test.go` was not edited, and no
assertion was weakened to make the limit disappear.

Run the derivation and the endpoint measurements with
`make component-scheduler`, or
`go test -run 'CaptureSpread|MissingCaptureTime' ./internal/scheduler`.
