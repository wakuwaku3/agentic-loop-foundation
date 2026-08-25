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
