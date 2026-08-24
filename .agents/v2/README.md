# v2 task state and evidence

`.agents/v2/task-state/` is the canonical task-state projection.  Each file
ends with its current transition; the transition records its actor, references,
reference digest, owner, successor, and retry budget.  The bootstrap records
under `historical/bootstrap-records/` are immutable historical input and are
not release evidence.

Active evidence is indexed by `evidence/index.json`; historical bootstrap
evidence is separately and non-release-eligibly indexed by
`evidence/historical/index.json`.  This directory is not a copy of the v1
agent loop.

## DAG growth (V2-010..V2-039)

`task-state/` now extends through V2-039 (V2-010 through V2-022 and V2-026
through V2-039), covering every milestone gate from M1 through M9. The
meaning of the DAG — which task belongs to which milestone, the critical
path, parallelizable groups, the common gate rules (G1-G5), the required
evidence per milestone, and how superseded failed tasks and
external-unavailable blocks are represented — is documented in
`docs/operations/v2-task-dag.md`. That document is the authoritative
interpretation layer; the JSON files here remain the machine-readable
source of truth.

## Ledger test structure

`internal/contracts/contracts_test.go` verifies this directory in two
layers. `TestCanonicalTaskStateMigration` pins the historical fact of the
v1 -> v2 migration: a floor of IDs that must remain present, and a
byte-exact sha256 digest pin on the three exhausted terminal failures
(V2-007, V2-023, V2-024). `TestCanonicalTaskStateInvariants` checks every
file under `task-state/` against structural invariants (schema validity,
filename/id/task_id agreement, an acyclic dependency graph across both
`dependencies` and `repair_of`, transition-to-projection consistency, retry
budget arithmetic, terminal `next_owner` nulling, `block_reason` semantics,
`release_eligible` staying false, and the superseded-failed-task rule) that
hold regardless of how large the DAG grows. Growing the DAG with new,
invariant-respecting task-state files requires no test change.
