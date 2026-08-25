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

## Forge adapter: gh subprocess, absolute path, credential as an absence (V2-064)

Measured facts about the read-only forge adapter (`internal/runner/forge.go`),
recorded alongside the V2-016/V2-017 measurements above because they are the
same class of environment-dependent fact:

- `gh` is **not** on the `PATH` inside `devbox run --pure`, while `git` **is**
  (devbox provides git from `.devbox/nix/profile/default/bin`). A bare
  `exec.Command("gh", ...)` therefore works on the host and fails in the
  validated environment. The adapter resolves the executable with the same
  `resolveTool` helper `confinement.go` uses: the caller's `PATH` first, then
  `/usr/bin`, `/bin`, `/usr/sbin`, `/sbin`. The measured resolution is
  `/usr/bin/gh`, `gh version 2.92.0 (2026-04-28)`.
- The resolved path must be absolute **and** `filepath.Base(path)` must equal
  `gh`, asserted before any process starts, mirroring the V2-017 argv[0]
  assertion. A basename mismatch is refused before the existence check, so a
  substituted binary is rejected whether or not it exists.
- Credential isolation is proven as an **absence**, not as a grant: the
  granted set is the empty slice and the Secret Broker is never consulted,
  because the CLI reads its own configuration store and takes no credential
  from argv or from the environment. The child receives the guarded base
  environment only, whose measured contents are `HOME` and `PATH`. Nothing in
  `internal/runner`'s non-test sources names that configuration store's path,
  which `source_guard_test.go` proves from the AST.
- The adapter is read-only. Its only argv forms are
  `gh api --method GET repos/{owner}/{name}` and `gh --version`; neither names
  a mutating method, a request field, an auth subcommand or a git operation.
  No stdout or stderr is returned, journaled or logged verbatim: only the
  parsed bounded fields (existence, default branch, viewer push permission,
  forge node id) leave the package, and every parse failure returns an error
  that carries none of the input.
- The Control Plane never invokes `gh` or `git`. That is asserted from the AST
  over `internal/api`, `internal/application`, `internal/domain`,
  `internal/store` and `cmd/control-plane`, with planted positive controls, so
  reachability can only enter as a Runner-submitted Observation.
- Recorded for the later push leg, not acted on here: no
  `git credential.helper` is configured at repository, global or system scope
  on this machine, so a bare `git push` over HTTPS could not authenticate.
  `gh auth git-credential` exists in gh 2.92.0 and implements the git
  credential helper protocol, so a per-invocation
  `git -c credential.helper=...` is the form to use. `gh auth setup-git` must
  not be run and git configuration must not be mutated at any scope.

## Bounded diagnostic log

`BoundedLog` is a per-execution `0600` file under the data root's `logs/`
directory -- never under the workspace, so the child process being
diagnosed cannot read or tamper with it. It has a hard byte cap and a hard
line cap; once either is reached, an explicit truncation marker is written
exactly once and every later write is a silent no-op, so the file never grows
past the cap plus the marker's own length. Every write passes through
`RedactLog` first: a secret-shaped value is replaced with `[REDACTED]`, while
a non-secret value of the same length survives byte for byte.

## Workspace confinement: convention-level path checks, plus OS-enforced writes (V2-046)

The runner asserts, and a test verifies, that it never hands a child a path
outside its workspace: `Invocation.WorkingDirectory` is the symlink-resolved
workspace path, no argv element resolves outside the workspace root, the
per-execution workspace is not a symlink and is not group- or world-
accessible, and a write path escaping the workspace root is detected before
use. This remains a convention the runner enforces on the paths it
constructs, not a guarantee about what the child process itself can do.

`runner.NamespaceConfinement` (`internal/runner/confinement.go`) closes that
gap for filesystem writes without any privileged container or VM: it runs
the child inside a rootless (unprivileged) Linux user namespace plus mount
namespace. `unshare --user --map-root-user --mount` maps the caller's own
uid to root *inside a brand-new user namespace only* -- no `sudo`, no
`CAP_SYS_ADMIN` requested from the host, no setuid helper -- which is
sufficient, since Linux 3.8 (see `user_namespaces(7)`), to also create a
mount namespace and perform bind mounts and remounts confined to that
namespace's own view. `ProcessSupervisor.Confine`, when set, wires this in:
`ProcessSupervisor.Run` calls `NamespaceConfinement.Probe` before ever
starting the child, and returns `runner.ErrNamespaceUnsupported` -- refusing
to start the child at all -- if this kernel/environment cannot actually
provide the confinement, rather than silently falling back to running it
unconfined. The TERM-then-KILL process-group semantics are unchanged: since
`unshare(1)` unshares the calling process's own namespaces in place (no
`--fork`, no PID namespace requested), the wrapped child keeps the exact PID
`ProcessSupervisor` already put in its own process group, so
`syscall.Kill(-pid, ...)` still reaches it and everything it spawns.

The mechanism, once the namespace exists: the top-level directory under `/`
that contains the workspace (e.g. `/tmp` for a workspace at
`/tmp/x/y/workspace`) is bind-mounted onto itself and then remounted
read-only (two steps, because the kernel's bind-mount syscall ignores every
flag but `MS_BIND` on its first call); the workspace itself is then
bind-mounted onto itself and explicitly remounted read-write, strictly
*after* the ancestor is sealed, because a mount created before its ancestor
becomes its own mount object is invisible once that happens, and a bind
mount created while its source already sits under a read-only mount
inherits that read-only flag. Since read-only is a whole-mount property,
sealing the ancestor makes every path under it read-only in one operation,
except the workspace's own separately pinned mount.

This was measured, not assumed: with the ancestor sealed and the workspace
re-punched writable inside a fresh namespace, a write into the workspace
succeeds, and a write to a sibling path under the same sealed ancestor fails
at the syscall boundary with `EROFS` ("Read-only file system") -- observed
verbatim in a child shell's own error output, not inferred from an exit
code. A positive control (the identical write, same paths, run without
`unshare`) succeeds, ruling out "it just can't write there anyway" as the
explanation. After the confined process exits and its ephemeral mount
namespace is torn down by the kernel, the outside path is exactly as it was
before: the failed write never touched the host filesystem at all, and the
successful workspace write is an ordinary, real, persisted write (mount
namespaces do not create copies; the workspace directory is the same inode
throughout). `internal/runner/confinement_test.go` is this proof: it never
counts a `t.Skip` as a pass -- when `Probe` reports the kernel/environment
cannot provide unprivileged namespaces, the test skips and emits no
evidence, rather than reporting success for confinement it never exercised.

What this does **not** provide: the mount namespace's own root (`/`) cannot
itself be bind-mounted by an unprivileged caller -- the kernel's
`do_loopback` rejects binding a mount namespace's root dentry onto any
target with `EINVAL`, confirmed empirically here, not merely inferred from
documentation -- so directories *outside* the sealed top-level ancestor are
left exactly as the host's ordinary DAC permissions already left them. On
any standard deployment where the runner's own uid is not the real root,
everything above that top-level ancestor was already unwritable to it, so
this is not an exploitable gap in practice; it would be one if the runner
were ever run as real root, which it is not designed to be. This mechanism
is a filesystem-write boundary only: it says nothing about network access,
process visibility/interference with other processes owned by the same
uid, or resource limits -- those remain out of scope for V2-046 and are not
claimed here. It is also a convention-independent proof only for the
specific mount operations exercised (bind, remount-bind-ro/rw, tmpfs); it
does not itself replace the path-construction checks described above, which
still run first and independently.

## Durable store: fsync'd JSONL journal, not SQLite — resolved (V2-044)

The Runner local durable store is the existing fsync-backed JSONL journal
(`runner.Journal`): durable append, fsync, idempotent replay, partial-tail
tolerance, corruption rejection, and a bounded size, all already implemented
and tested. `docs/architecture/technology.md` section 10 and
`docs/architecture/validation.md` section 2 previously named SQLite for this
role; V2-016 did not introduce it, because doing so would add a third-party
dependency and change `go.mod`/`go.sum`, which are hashed into every
component's evidence key (`internal/ci/planner.go` `evidenceKey()`) and would
invalidate the whole evidence ledger for a storage-engine swap that changes
no M3 completion condition. This was recorded here as a documentation-
versus-implementation divergence to be resolved by V2-044. V2-044 has now
resolved it by rewriting both documents to state the required guarantees and
name the implemented journal instead of SQLite, so this note records a
closed divergence, not an open one.

## Retry and idempotency

A `result_pending` journal record is recovered as described above, so retry
skips the provider and records one `result_accepted` event after the
idempotent application acceptance.
