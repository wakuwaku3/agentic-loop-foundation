# Runner local daemon

`cmd/runner` has no production control-plane or provider connection. It exits
unless the local mode is explicit:

```sh
go run ./cmd/runner --fake --runner-id runner-local
go run ./cmd/runner --fake --runner-id runner-local --data-root /absolute/path
```

Fake mode uses the in-memory application boundary and local durable journal
only. The temporary data root is removed on exit; a supplied root must be an
absolute directory with mode `0700`. The daemon handles `SIGINT` and `SIGTERM`
and then exits. It does not manufacture owner/scheduler credentials for a
production deployment.

The orchestrator obtains a process permit immediately before provider start
and a result permit immediately before acceptance. A stop or stale fence
therefore prevents the external action and leaves no accepted result.

## Provider-neutral Work Packet (no raw prompt)

`runner.ProviderRequest` carries a `provider.WorkPacket` plus the operation id
and workspace; it has no `Prompt` field at all, so a raw prompt is
structurally unrepresentable rather than merely asserted absent. A
`ProviderClient` composes a `provider.Adapter` (`Build`/`Parse`) with an
`InvocationRunner` seam: `FakeInvocationRunner` records the built
`provider.Invocation` and returns fixture bytes without starting a process;
`SupervisedInvocationRunner` is the seam a real Provider CLI fills (V2-017)
by running a real Invocation through `ProcessSupervisor` and the bounded
diagnostic log below. A `WorkPacket` failing `provider.WorkPacket.Validate` is
refused before any Invocation is built.

## Journal recovery key is the increment identity

The journal's recovery key for a pending provider result is the
increment/target identity plus an operation kind (`JournalProviderPending`,
keyed for lookup by `IncrementID` via `FindPendingProviderResult`), not the
per-attempt canonical request id. This is what lets a *different* Execution
resume a crashed attempt: the durable identity that survives the crash is the
Increment, while the canonical fence (Execution id, lease, fencing token) is
per attempt. Each `result_pending` journal event also records the digest of
the Work Packet it was produced for; a resuming attempt adopts the recovered
checkpoint only when its own Work Packet's digest matches and the recovered
payload re-passes `provider.CodexAdapter.Parse`, and otherwise re-runs the
provider exactly once. The crashed Execution's own late `AcceptResult` (its
old lease is inactive by the time the new attempt claims) is rejected by the
canonical domain with `domain.ErrLeaseExpired` or an equivalent domain error;
canonical state is never accepted under a stale fencing token.

## Secret Broker: two environment channels, five fail-closed refusals

`SecretBroker` is a local, in-process, scope-limited, revocable, single-use
credential boundary with two structurally separate environment channels:

- The **guarded base environment** (`GuardEnvironment`) is what the bounded
  log and the journal may observe; it still rejects any secret-shaped name or
  value exactly as before the Secret Broker existed.
- The **granted channel** is produced only by `SecretBroker.Lease`, for one
  `Scope` (execution id, repository, provider, fencing token, expiry) and one
  Invocation. It is merged onto that Invocation's `Environment` field and
  never returned to any code path that writes the journal, the Work Packet,
  the canonical store, or the bounded log.

`Lease` fails closed on five independent conditions, each covered by its own
test in `secret_broker_test.go`: an expiry at or before now; a
`domain.PermitCredential` denial under an effective stop (or a stale control
revision); a credential name outside the per-provider allowlist; a zero or
mismatched fencing token; and a second use of an already-consumed or revoked
grant (`Lease` is single-use per execution id; `Revoke` makes every future
`Lease` for an execution id fail closed).

## Bounded diagnostic log

`BoundedLog` is a per-execution `0600` file under the data root's `logs/`
directory -- never under the workspace, so the child process being
diagnosed cannot read or tamper with it. It has a hard byte cap and a hard
line cap; once either is reached, an explicit truncation marker is written
exactly once and every later write is a silent no-op, so the file never grows
past the cap plus the marker's own length. Every write passes through
`RedactLog` first: a secret-shaped value is replaced with `[REDACTED]`, while
a non-secret value of the same length survives byte for byte.

## Workspace confinement is convention-level, not OS-enforced

The runner asserts, and a test verifies, that it never hands a child a path
outside its workspace: `Invocation.WorkingDirectory` is the symlink-resolved
workspace path, no argv element resolves outside the workspace root, the
per-execution workspace is not a symlink and is not group- or world-
accessible, and a write path escaping the workspace root is detected before
use. This is a convention the runner enforces on the paths it constructs, not
an OS-enforced sandbox: no namespace or container confines the child process
itself. OS-level confinement is deferred to a privileged container or VM
integration test (V2-046, M4); it is not required at the M3 gate.

## Durable store: fsync'd JSONL journal, not SQLite

The Runner local durable store is the existing fsync-backed JSONL journal
(`runner.Journal`): durable append, fsync, idempotent replay, partial-tail
tolerance, corruption rejection, and a bounded size, all already implemented
and tested. `docs/architecture/technology.md` section 10 and
`docs/architecture/validation.md` section 2 name SQLite for this role; V2-016
does not introduce it, because doing so would add a third-party dependency
and change `go.mod`/`go.sum`, which are hashed into every component's
evidence key (`internal/ci/planner.go` `evidenceKey()`) and would invalidate
the whole evidence ledger for a storage-engine swap that changes no M3
completion condition. This is recorded here as a documentation-versus-
implementation divergence resolved by V2-044, not as a decision V2-016 made
on its own.

## Retry and idempotency

A `result_pending` journal record is recovered as described above, so retry
skips the provider and records one `result_accepted` event after the
idempotent application acceptance.
