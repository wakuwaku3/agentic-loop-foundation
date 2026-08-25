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

## Git working copy: real clone, closed argv, no remote (V2-071)

Measured facts about the Source Control port (`internal/runner/sourcecontrol.go`)
and its one adapter (`internal/runner/git.go`), recorded in the same class as
the forge measurements above.

- **Location and owner.** A working copy is
  `<workspace_root>/<execution_id>/source`, produced by the existing
  `Workspace.Create(execution_id)` followed by the clone into the `source`
  child. The owner is the **Execution**, not the Increment:
  `domain.Execution` carries `IncrementID`, so an Execution belongs to exactly
  one Increment for its whole life, and keying the directory on the Execution
  makes "a copy that outlives its Increment" unrepresentable rather than
  merely discouraged. A retry is a fresh clone at its own path; a copy is
  never shared between two Executions. Nothing keyed by increment id alone is
  created on disk.
- **Resolution.** `git` **is** on the `PATH` inside `devbox run --pure` (while
  `gh` is not), so the adapter resolves it with the same `resolveTool` helper
  `confinement.go` and `forge.go` use. The measured resolution in this
  environment is `<worktree>/.devbox/nix/profile/default/bin/git`,
  `git version 2.55.0`. The resolved path must be absolute, its
  `filepath.Base` must equal `git`, and it must stat to a regular file -- all
  asserted before any process could exist, with an injectable `Stat` hook so
  the refusals touch no filesystem.
- **Clone, never `worktree add`.** `git clone --no-hardlinks` was measured
  inside the namespace to give `git rev-parse --git-common-dir == ".git"`: a
  fully independent repository whose every write lands inside the workspace.
  A linked worktree's `.git` is a *file* pointing into the outer repository's
  `.git`, i.e. a write path outside the confinement, so `worktree` is absent
  from the argv allowlist.
- **Closed argv allowlist.** The subcommand of every argv the adapter can
  build comes from one closed set: `version`, `init`, `clone`, `checkout`,
  `switch`, `add`, `commit`, `rev-parse`, `status`, `diff`, `fsck`,
  `ls-files`, `cat-file`, `symbolic-ref`. `push`, `fetch`, `remote`,
  `submodule`, `config`, `credential` and `worktree` are **absent**, so no
  code path can construct such an argv. The only pre-subcommand options
  admitted are `-C <absolute path>` and `-c user.name=` / `-c user.email=`;
  `ProcessSupervisor` has no `Dir` field, so the working directory is
  expressed as `-C` in argv.
- **Guarded environment, no `HOME`.** The child receives exactly `PATH`,
  `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_SYSTEM=/dev/null` and
  `GIT_TERMINAL_PROMPT=0`, plus `GIT_AUTHOR_DATE` and `GIT_COMMITTER_DATE` on
  the commit call only, from the injected clock. `HOME` is deliberately **not**
  allowlisted: it was measured that clone, `checkout -b`, `add` and `commit`
  all succeed without it under git 2.55.0, so excluding it is the strongest
  available property -- with no `HOME` the adapter cannot reach the invoking
  user's own tool configuration store even in principle. That is strictly
  stronger than the forge adapter, which must allowlist `HOME`. No `git config`
  is written at repository, global or system scope, and `gh auth setup-git` is
  never run.
- **Kernel refusals, measured.** Inside `NamespaceConfinement` with
  `Workspace = <workspace_root>/<execution_id>` (unshare `--user
  --map-root-user --mount`, kernel `6.18.33.2-microsoft-standard-WSL2`,
  x86_64): the clone from a local bare origin succeeded; `checkout -b`, `add`
  and `commit` succeeded; a write to the sealed top-level ancestor, a write
  into the origin directory and a write into a **sibling Execution's**
  workspace all failed with `EROFS`; and a hand-built `git push` to that same
  file-path origin was refused by the kernel with
  `remote unpack failed: unable to create temporary object directory`. That
  kernel refusal is the second, independent guarantee beside the argv
  allowlist. The identical outside writes, run **without** the namespace,
  all succeeded -- the positive control
  `docs/architecture/validation.md` requires by name, without which the
  `EROFS` results would prove nothing about the confinement.
- **Fail-closed confinement.** `ProcessSupervisor.Run` calls `Confine.Probe`
  on every invocation, so a working-copy sequence pays one namespace probe per
  git command. A probe failure is returned as a hard error and no child is
  ever started unconfined. Only `git --version` runs unconfined, and only
  because it writes nothing. A kernel that cannot provide the namespace is a
  stop-and-escalate with the kernel identifier and the probe's reason, never a
  skip counted as a pass.
- **Two verification stages, one executed.** The Git-level verification is
  **executed**: `status --porcelain=v2`, `diff --exit-code HEAD`, `rev-parse`
  for HEAD, its tree and the base, `symbolic-ref --short HEAD`,
  `fsck --no-progress --connectivity-only`, `cat-file -t` on the committed
  tree, and `diff --name-only` for the changed-path count. It returns a
  bounded observation (branch, head commit, tree name, base commit, clean
  flag, changed-path count) and nothing else. Project-level verification --
  running the cloned project's own build or test command -- is **declared and
  fails closed** with `ErrProjectVerificationNotWired`, starting no process:
  it is unbounded in cost and duration and is the surface the `CostLedger`,
  the provider preflight and the standing-authorisation records exist to
  govern, so wiring it here would create a second execution path past those
  gates. It is never reported as passed.
- **Bounded output, nothing verbatim.** stdout is captured into a bounded
  buffer with a hard byte cap and stderr is discarded outright, so no error
  the adapter returns can carry a child's bytes, a path outside the workspace
  or anything credential-shaped. The one preserved detail is
  `ErrNamespaceUnsupported`'s own reason, which is the confinement
  machinery's diagnostic about this kernel rather than git's output.
- **Lifetime and crash cleanup.** A plain-text descriptor beside the copy names
  `increment_id`, `execution_id`, `repository_id` and `created_at` and nothing
  else, so an orphan can be attributed without consulting the Control Plane --
  which is exactly the state a crashed Runner leaves behind. `Discard` is
  idempotent, re-applies the `Workspace.Path` escape check and is called on
  Materialize's error path too. `SweepWorkingCopies(active)` removes every
  workspace child whose execution id is not in the active set, in a single
  pass at Runner start: no goroutine, no sleep and no timer, and it refuses to
  remove anything that is not a validated single-segment directory child.
- **Two clone-source regimes.** Inside `make check` the origin is always a bare
  repository created at test time by the real git binary in `t.TempDir()`; no
  git fixture, pack file or bare directory is committed to this repository
  (gitleaks scans every ref, so a probe commit reaching this repository's refs
  would keep the gate red), and no test in `make check` resolves a hostname or
  opens a socket. The one live clone is gated on `AGENTIC_LOOP_LIVE_GIT=1`
  **together with** an owner-supplied `AGENTIC_LOOP_LIVE_GIT_CLONE_URL`, and
  clones anonymously with `--depth 1 --no-tags`. The URL is not hardcoded on
  purpose: a hardcoded default would make the check's meaning depend on an
  unmeasured fact about that repository's visibility. If the gate is set and
  the URL is absent the check **fails** naming the missing designation; it
  does not skip.
- **Nothing reaches a remote in this Increment.** No clone target is a remote
  this Loop writes to, no push argv is constructible, and the adapter has no
  remote-mutating code path at all. Reaching the real forge with a push is a
  separate Increment with its own credential design; the note in the forge
  section above records what was already measured for it.

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

## The declared working directory reaches the child (V2-077)

Until 2026-08-25 the working directory an `Invocation` declared was set by the
adapter and read by nobody: `ProcessSupervisor.Run` took only a context and an
argv and never assigned a child directory, and `SupervisedInvocationRunner`
never read `Invocation.WorkingDirectory`. A Provider whose command line does
not carry the workspace -- claude carries no path in argv at all -- therefore
ran in whatever directory the Runner happened to be started from. That is now
fixed, in one place, additively.

`ProcessSupervisor` gains exactly one field, `Dir string`, and is the only
place in the package where a child's directory is assigned: it is the only type
that constructs an `exec.Cmd`, so no other type physically can. Its zero value
is byte-identical to the previous behaviour (the child inherits the calling
process's own directory), which is what lets every pre-existing construction
site keep compiling and passing unchanged. Deciding *which* directory is not
that type's job: the policy lives in `SupervisedInvocationRunner`, which reads
`Invocation.WorkingDirectory`, validates it, and sets `Dir` on its own value
copy of the supervisor.

**Five fail-closed refusal properties, and one more under confinement.** The
value must be non-empty, absolute, already canonical (`filepath.Clean` leaves
it unchanged, which rejects a `..` segment, a trailing separator and a doubled
separator alike), an existing directory, and not a symlink. When the supervisor
is confined it must additionally be the confinement workspace itself or a path
beneath it. Every failure returns
`runner.ErrInvocationWorkingDirectoryUnusable` with its own wrapped reason and
starts no process. Nothing is ever repaired, and the Runner's own directory is
never substituted -- that substitution *is* the defect this removes, and making
it the documented fallback would make the correct and the broken case
indistinguishable from outside.

**The refusal happens before `LoadPreflightRecord` and before
`Ledger.Reserve`.** The position is load-bearing, not tidy: a reservation
debited before a failure stays charged at worst case forever (dp-v2-017 d9), so
a refusal placed after `Reserve` would spend a real reservation to report a
caller's own malformed request. The ordering is asserted two ways -- by driving
the refusal with a `RecordPath` that names a nonexistent file and getting the
working-directory reason back, and by asserting that no ledger file exists on
disk afterwards, since `Reserve` persists a reservation before anything may
execute.

**Under confinement the chdir is expressed inside the namespace, not through
`cmd.Dir`.** `Cmd.Dir` is applied by `chdir` in the forked child *before* the
exec, and the program exec'd under confinement is `unshare`, so a `cmd.Dir`
can only ever take effect before the namespace exists and therefore before
either of the confinement's two mount pairs -- it is structurally impossible
for it to land after the read-write remount of the workspace. So
`NamespaceConfinement.wrap` emits the directory as a single `shQuote`'d `cd`
after both mount pairs and immediately before `exec "$@"`, and
`ProcessSupervisor` assigns `cmd.Dir` only on the unconfined path (asserted
structurally, by an AST check that the single assignment is guarded by
`s.Confine == nil`, not by a comment). **No mount is added, removed or
reordered to express the directory**, and no path becomes reachable that was
not reachable before: a directory that is not the workspace and not beneath it
is refused, never accommodated by relaxing the confinement.

**The rejected ordering was measured, and it is worse than merely unprovable.**
On kernel `6.18.33.2-microsoft-standard-WSL2` (linux/amd64), a cwd inherited
across namespace creation -- exactly what `cmd.Dir` can do when `Confine` is
non-nil -- **actually permitted a relative upward write (`../`) that the seal
was meant to stop, and the file reached the host filesystem**: the write exited
0 with empty stderr, and the path existed outside the workspace after the
namespace was gone. The positive control on the same kernel, the chosen
in-namespace `cd`, refused the identical write with `Read-only file system` and
left nothing behind. So the in-namespace ordering is not merely the ordering
whose after-the-remount property is provable; on this kernel it is the only one
that holds. The measurement is recorded as a measurement: the design was chosen
from the structural argument above and is not changed on the basis of it.

**The working directory is now the resolution base for every relative path the
child resolves.** Nothing in this repository passes a relative path to a child
today -- the adapter guards refuse a traversal segment and V2-027 measured that
only absolute workspaces appear in argv -- but the change is silent for
anything that does, including any relative path a Provider CLI writes into its
own state. That is a deliberate consequence, not a side effect: it is what
makes an Increment's workspace, rather than the Runner's directory, the place a
child's own relative work lands.

**The directory flags stay.** codex keeps `-C` and opencode keeps `--dir`,
because each CLI's own help declares it. The resulting double expression is
harmless for the values that actually occur, and that is now proven rather than
assumed: one call to the adapters' shared build helper produces the flag's
argument and `WorkingDirectory` from the same request workspace, so they are
the same string by construction, and `SupervisedInvocationRunner` refuses
fail-closed if they ever disagree. See
`docs/operations/provider-adapters.md`.

**What this does not change.** `Invocation.Environment` is still read by
nobody on the production path: `Grant.Apply` writes it and only
`FakeInvocationRunner` observes it, while `Run` builds the child's environment
itself from the approved record's base names. That is the same-shaped defect,
recorded and escalated by V2-077 rather than fixed, because making a Secret
Broker grant reach a real child widens what the child may reach and needs its
own credential-isolation acceptance. It is latent rather than active only
because every live exercise so far ran with `granted_names` empty.

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
