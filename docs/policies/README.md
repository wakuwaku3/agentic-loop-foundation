# v2 repository policies

These policies apply to every investigation, change, validation, release, and operation. Detailed product guarantees remain normative in [the principles](../principles.md); this page defines the engineering consequences without restoring v1's GitHub Issue workflow.

## Development environment

- Development and validation run through the committed `devbox.json` and `devbox.lock`.
- Package and toolchain selectors are pinned; changing them invalidates all component evidence keys.
- `devbox run --pure check` is the common deterministic entrypoint. A host-only success is not release evidence.
- Repository Contracts may select another reproducible environment for an application, but the Foundation itself continues to use Devbox.

## External environment

- Every external dependency has versioned desired state, an idempotent apply path, read-only drift detection, verification, rollback, and migration instructions.
- Console-only configuration is not accepted as the source of truth.
- External observations are inputs to domain decisions; they are not canonical state.

## Provider neutrality

- Codex, Claude, and opencode implement one provider-neutral port.
- Provider-specific commands, output, errors, usage, and cancellation stay inside their adapter.
- Work packets contain facts, decisions, bounded commands, and artifact references—not conversations or credentials.

## Testing and validation

- Changed components and the transitive consumers of changed public contracts run through the component entrypoint.
- Candidate readiness aggregates evidence for every current component key; it does not blindly rerun the repository.
- Stable promotion separately exercises every user capability in Preview. Provider-dependent behavior uses the affected real Provider.
- A retry may diagnose a failure but must not erase it. Flaky Preview evidence blocks promotion.

## Continuous delivery

- The first deployable or binary release includes its reproducible delivery and rollback path.
- Implementation, configuration, schema, Release Contract, and user documentation share one immutable candidate digest.
- Stable promotion is a domain transition after Preview evidence, not merely a Git merge or passing test.

## Secrets and permissions

- Secrets never enter Git, canonical records, task packets, prompts, artifacts, evidence summaries, or logs.
- A local Secret Broker grants the minimum credential to the minimum process for a bounded lifetime and can revoke it.
- Outputs are redacted and scanned. Suspected exposure stops the affected scope and rotates the credential before recovery.
- Runner access is outbound-only and repository, operation, revision, and fencing-token scoped.

## Cost and finite resources

- Unapproved monetary cost is forbidden. Budgets are enforced before the side effect, not only observed afterward.
- Polling, retries, leases, logs, history, input size, concurrency, Provider usage, storage, and external I/O are bounded.
- Every design records how throughput and I/O grow with Backlog, Repository, and Runner counts.
- Cost, quota, or capacity exhaustion becomes explicit `waiting` or `needs-input` state; it is never reported as success.

## Documentation

- Product behavior, architecture rationale, machine contracts, user guides, and runbooks have distinct owners.
- One fact has one source of truth; generated views link to it rather than copying it.
- Stable and Preview user documentation are release artifacts and promote or roll back with the implementation.

## Postmortems

- Incidents, near misses, repeated failures, and resource exhaustion receive a blameless causal analysis.
- Action items remain incomplete until their preventive evidence passes and the triggering scenario no longer recurs.
- Postmortems are canonical Backlog records in v2; they are not coupled to GitHub Issues.

## Language and integration

- User-facing repository operations, migration reports, and external collaboration records are written in Japanese unless a machine contract requires otherwise.
- PRs are optional integration adapters. Human review is not a completion gate.
- The coordinator is the only writer to the `v2` integration branch; task agents write isolated worktrees.
