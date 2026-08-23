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
