# Local scheduler core

`internal/scheduler` is a pure local planning boundary for one installation
backlog. It accepts a bounded `Snapshot` (at most 100 candidates), a
repository registry, runner/provider capacities, resource claims, and the
dependency DAG, then returns a deterministic `Plan`. It performs no Firestore,
network, process, or provider operation.

Requirements are ordered by priority and age, with a stable ID tie-breaker.
Dependencies must be completed; cycles fail closed. A requirement related to
multiple repositories is assigned to one runner for every repository or not at
all. A repository with three or more failures, or an active isolation deadline,
is excluded until its failure state is cleared. Provider capacity is shared
across runners when `ProviderCapacity` is supplied.

Claims are repository-scoped by default: read/read may share, while a write
conflicts with any claim on the same resource and repository. Set
`ResourceRequest.Global` explicitly for an installation-wide resource.
`Apply` validates the complete repository set and returns the original snapshot
on any conflict, providing the cross-repository rollback boundary. Persistence
adapters should apply this result in their own transaction/compare-and-swap
boundary; the scheduler itself intentionally has no UnitOfWork dependency.

Run the focused gate with `make component-scheduler` or
`go test ./internal/scheduler`. The tests cover two repositories/two runners,
double-owner rejection, failure-storm isolation, dependency cycles, shared
capacity, claim modes, candidate bounds, and partial-plan rollback.
