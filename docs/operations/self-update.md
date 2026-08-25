# Loop self-update

This document is the design of record for M8 (Loop self-update). It is
authored by V2-033, which is a design task: it changes no code. Every
statement about the present tree below was measured with the argv quoted
next to it on 2026-08-25, and every statement about future behaviour is a
requirement on V2-034 (local closure) or V2-035 (preview-local live), not a
claim that the behaviour exists.

Nothing here has been deployed, exercised in a Preview environment, signed
with a real key, or run against GCP, Cloud KMS, a real Provider CLI or a
credential. No key material, key value, token or credential appears in this
document; keys are described only as paths, modes and owners.

V2-034 does not start before V2-026 (the M5 gate) passes. The
`bundle_digest` join in section 3.3 depends on V2-021's bundle assembly
being gate-accepted rather than merely merged, so building on it earlier
would rest the manifest's identity on an ungated digest.

## 1. Measured baseline

`devbox run --pure -- go test -count=1 -v ./internal/update ./cmd/bootstrap`

- `internal/update` has exactly two top-level tests, both passing:
  `TestInstallSwitchAndRollbackKeepBothVersions` and
  `TestVerifyRejectsTamperAndIncompatibleSchema`.
- `cmd/bootstrap` reports `[no test files]`.

The package is two files: `internal/update/update.go` (5105 bytes) and
`internal/update/update_test.go` (2106 bytes). It exports
`ManifestSchema = "agentic-loop/module-manifest/v1"` (`update.go:21`),
`Manifest{Schema,Version,OS,Architecture,BinarySHA256,SchemaMin,SchemaMax}`
(`update.go:25`), `Bundle{Manifest,Binary,Signature}` (`update.go:35`),
`Verify` (`update.go:41`), `Install` (`update.go:71`) and `Switch`
(`update.go:116`).

`Verify` checks an Ed25519 signature over `manifest || sha256(binary)`,
decodes the manifest with `DisallowUnknownFields` (`update.go:52`) plus a
single-JSON-value check (`update.go:57`), and requires exact
`ManifestSchema` equality, a semver-shaped version, `GOOS`/`GOARCH`
equality (`update.go:60`), `BinarySHA256` agreement and `currentSchema`
inside the closed interval `[SchemaMin, SchemaMax]` (`update.go:63`).
`Install` writes `versions/<version>/{runner 0500, manifest.json 0400}`
through `MkdirTemp` + `Rename` and is idempotent only when the existing
bytes match exactly. `Switch` creates `.<channel>.next` and renames it over
`root/<channel>` for `channel` in `{stable, preview}`.

`cmd/bootstrap/main.go` has verbs `install | switch | --version` only.

`grep -rln 'internal/update' --include=*.go .` returns exactly two files:
`cmd/bootstrap/main.go` and `internal/release/source_guard_test.go`. The
Runner does not link `internal/update` today.

`grep -rn '"stable"' --include=*.go .` returns no reader of `root/stable`:
the only `internal/update` hit is the channel-name validation inside
`Switch` (`update.go:117`), and every other hit is the unrelated
`domain.ReleaseStable` status string or a release-package test. There is no
`exec` of an installed binary anywhere in the tree.

`ci/components.json`: the `update` component has roots `internal/update/**`
and zero declared dependencies; the `runner` component has roots
`cmd/runner/**`, `cmd/bootstrap/**`, `internal/runner/**` and declared
dependencies `domain` and `update`; `store-firestore` has roots
`internal/store/firestore/**` and `firestore.indexes.json` with declared
dependencies `domain` and `application`; `docs` has roots `docs/**`,
`README.md`, `AGENTS.md`. `make component-update` runs
`go test ./internal/update`; `make component-runner` runs
`go test ./cmd/runner ./cmd/bootstrap ./internal/domain ./internal/runner`,
so the launcher's package is already inside the runner component's check
target.

`grep -n 'RecordSchema' internal/store/firestore/store.go`: `RecordSchema`
is the constant `"v1"` (`store.go:31`); `DecodeRecord` refuses with
`ErrInvalidSchema` when `e.Schema != RecordSchema` (`store.go:79`) and
`decodeDocument` refuses when `d.RecordSchema != RecordSchema`
(`store.go:173`). The payload is a JSON string decoded with a plain
`json.Unmarshal`, so unknown payload fields are ignored and absent payload
fields take their zero value.

`grep -rn 'version' internal/api/api.go`: the only version-shaped response
value is the static `"schema_version": "v1"` (`api.go:84`, and the same
literal in the record and error writers at `api.go:528` and `api.go:769`).
Every other match is an optimistic-concurrency `Expected*Version` request
field (`api.go:367`, `:384`, `:628`, `:673`). No Runner-facing request
carries a binary version. `grep -rn 'type RunnerID' internal/domain/*.go`
returns `internal/domain/model.go:16: type RunnerID string`; there is no
Runner aggregate.

`internal/release/pipeline.go:83` defines `RetentionEligible` over
`RetentionInput` (`pipeline.go:67`) with four outcomes and no wall-clock
read. `internal/release/release.go:101` documents and `release.go:160`
implements the monotonic rollback established by dp-v2-021 d8: a rollback
clears `PreviousStableDigest`, so a second consecutive rollback is refused,
and `Promote` (`release.go:134`) refuses any digest that is not the current
preview route. `internal/release/bundle.go` frames the bundle digest as
`role\0path\0sha256\n` over the seven roles (`bundle.go:28`, `frame` at
`bundle.go:126`, `BundleDigestFromMembers` at `bundle.go:148`) and carries
`CandidateID domain.ReleaseID` (`bundle.go:415`).

`internal/runner/confinement.go` declares `NamespaceConfinement`
(`confinement.go:62`), `ErrNamespaceUnsupported` (`confinement.go:72`) and
the functional `Probe` (`confinement.go:127`), which refuses rather than
degrading to an unconfined run.

## 2. The eleven-item gap list

Each item names the file and symbol it was measured from. This list is the
acceptance skeleton for V2-034; items 7 and 8 are escalations, not V2-034
work.

1. **No launcher and no exec path.** `cmd/bootstrap/main.go` has verbs
   `install | switch | --version` only, and nothing in the tree reads
   `root/stable`. The M8 completion condition "recover from the Stable
   launcher" therefore has no implementation to recover with.
2. **The trust anchor comes from argv.** `cmd/bootstrap/main.go:37` takes
   `--public-key <path>` and hands whatever it reads to `update.Install`
   (`main.go:54`, `main.go:58`). There is no fixed path, no ownership
   check, no mode check, no key set and no key id.
3. **No re-verification of on-disk bytes.** `Verify` runs at install time
   over in-memory bundle bytes only; nothing verifies
   `versions/<v>/runner` against `versions/<v>/manifest.json` afterwards.
4. **`Switch` is an unguarded symlink flip.** `update.go:116` accepts any
   installed version for either channel with no record, no monotonicity and
   no requirement that a forward move name a gate-passed digest.
5. **No GC and no retention state.** `versions/` grows without bound; there
   is no `rolled_back_at`, no window state and no installed-version record
   anywhere in `internal/update`.
6. **The manifest is closed and single-module.** Exact `ManifestSchema`
   equality (`update.go:60`) plus `DisallowUnknownFields` (`update.go:52`),
   with no accepted set, and no `bundle_digest`, `candidate_id`,
   `contract_release`, `contract_digest`, Runner API range, `key_id` or
   `algorithm` field. Nothing joins a running binary to a gated source
   tree.
7. **No schema migration machinery and no owner for the canonical schema
   counter**, while both Firestore readers compare `record_schema` with
   `!=` (`store.go:79`, `store.go:173`). Escalation E2, carved out as
   V2-070.
8. **No version reporting.** No domain Runner aggregate
   (`internal/domain/model.go:16` is a bare `RunnerID string`) and no
   version field on any Runner-facing request (`internal/api/api.go`), so a
   mixed-version period is invisible to canonical state. Escalation E1,
   carved out as V2-069.
9. **No launcher tests.** `cmd/bootstrap` has no test files although
   `make component-runner` runs `./cmd/bootstrap`, and `internal/update`
   has exactly two tests with no fail-closed negative case beyond tamper
   and schema interval.
10. **The launcher does not consult `runner.NamespaceConfinement.Probe`**
    (`internal/runner/confinement.go:127`), so a child could be started
    outside the V2-046 confinement.
11. **No audit record of install or switch**, so "which version ran when"
    is unanswerable after an incident.

## 3. Decision 1: signed Runner bundle

### 3.1 Keys: one private key, a per-machine anchor set

**Decision.** The private half and the trust anchor are separate artifacts
and neither lives in the repository, the workspace, any data root, any
bundle or any evidence record.

- **Release private key.** In the local phase exactly one Ed25519 private
  key exists, on the owner's release machine only, at an owner-only `0600`
  path under the owner's configuration directory (for example
  `${XDG_CONFIG_HOME}/agentic-loop/signing/release-ed25519.key`). It is
  never placed on a Runner machine that only consumes bundles.
- **Trust anchor.** The anchor is the public half, replicated out of band
  to each machine as a `0400` file at a fixed absolute path derived from
  the machine configuration root (for example
  `${AGENTIC_LOOP_ROOT}/trust/release-keys`). It holds one accepted entry
  per line, `key_id algorithm base64(pubkey)`, and is therefore a **set**,
  not a single key.
- **Rotation.** Because the two machines are never shared, rotation is
  inherently two-step per machine: add the new `key_id` line on both
  machines, start signing with it, then remove the old line. A single-key
  anchor would make rotation a flag day in which one machine rejects valid
  bundles.
- **Cloud KMS phase.** Only the signer moves. The signer calls KMS
  asymmetric-sign instead of reading a file; the anchor on each machine
  stays a public key file and `internal/update`'s verify path does not
  change at all. The only migration artifacts are that the manifest's
  `key_id` selects which anchor entry verifies, and that each anchor entry
  carries an `algorithm` label so a KMS key whose algorithm is not Ed25519
  can be added as a second accepted entry during the overlap. Whether
  Cloud KMS offers Ed25519 asymmetric-sign in the target project is a
  D1-time fact, not a now-fact (escalation E3), which is exactly why the
  anchor set admits a second algorithm.

**Mechanism.** The manifest names `key_id` and `algorithm`; the launcher
looks that pair up in the anchor set and verifies with that entry only. A
manifest naming a `key_id` absent from the local anchor set is refused.
Keeping the anchor a file in both phases is what makes the KMS migration a
signer-side change rather than a Bootstrapper change, which matters because
the Bootstrapper is the one component that cannot self-update (section 4.3).

**Violation.** A key file inside the repository, the workspace, a bundle or
an evidence record; a private key on a consuming machine; an anchor holding
one key with no `key_id`; a verify path that changes when the signer moves
to KMS.

### 3.2 Fail-closed verification: the measured defect and the refusal list

**The measured defect.** `cmd/bootstrap install --public-key <path>` asks
its caller for its own trust anchor and passes the bytes straight to
`update.Install` (`cmd/bootstrap/main.go:37`, `:54`, `:58`). A caller who
can choose the key can sign anything with a key of their own and install
it. The signature check is therefore decorative at the point where it
matters: everything under it is sound, and the one input that decides what
"valid" means is supplied by the untrusted side. Note what this is *not*:
it is not a tamper hole (`TestVerifyRejectsTamperAndIncompatibleSchema`
covers tamper) and not a schema-interval hole. It is a substituted-key
hole, and no current code path or test prevents it.

**Decision.** V2-034 removes `--public-key` and resolves the anchor from
the fixed machine path of section 3.1. There is no default key, no
`--insecure` flag, and no environment override that is not itself an
absolute path whose ownership and mode are checked by the same code.

**Mechanism — the refusal list.** The launcher exits non-zero *before*
`versions/` is touched and *before* any child process is started when the
anchor:

1. is absent at the fixed path;
2. is not a regular file (directory, symlink to a non-regular target,
   device, FIFO, socket);
3. is not owned by the invoking user;
4. has a mode wider than `0600` — the canonical mode is `0400`, and any
   group bit, other bit or execute bit is a refusal;
5. is empty;
6. parses to zero accepted entries.

To that list V2-034 adds the manifest-side refusals: a `key_id` not present
in the anchor set, an `algorithm` the named entry does not declare, and a
signature that does not verify under the named entry.

The refusal *shape* is the one V2-046 already established on the Runner
side: `internal/runner/confinement.go`'s `NamespaceConfinement.Probe`
returns `ErrNamespaceUnsupported` (`confinement.go:72`, `:127`) and no child
is launched, rather than degrading to an unconfined run. Reusing it keeps
one fail-closed idiom in the Runner-side code instead of two, and makes the
launcher's two preconditions — a resolvable anchor and a working
confinement — refuse in the same way.

A fail-closed claim is worth exactly what its negative tests prove, so
these refusals are the negative-case list V2-034's tests must enumerate one
by one.

**Violation.** Any code path that reaches `Install`, `Switch` or an `exec`
with an anchor that failed one of the six checks; any refusal that writes
into `versions/` first; any refusal that logs a warning and continues.

### 3.3 Three digests, three questions, and the hole they leave

**Decision.** `Manifest.BinarySHA256`, the Ed25519 signature and V2-021's
`BundleDigest` are three different claims. All three are required, and
their union is still not sufficient.

- `Manifest.BinarySHA256` binds *this manifest* to *these binary bytes*.
  It is the only thing that lets a launcher re-verify bytes that are
  already on disk (gap item 3), because it is the only claim that can be
  re-evaluated without the original bundle.
- The **signature** over `manifest || sha256(binary)` binds that pair to
  the owner's key. It answers "who authorised this", which no digest can
  answer: an attacker who replaces the binary replaces its digest too, and
  a manifest is not self-authenticating.
- V2-021's **`BundleDigest`** is `sha256` over the framing
  `role\0path\0sha256\n` of the seven source roles
  (`internal/release/bundle.go:126`, `:148`). It contains no binary at all.
  It answers "which source tree was gated" and cannot answer "which bytes
  will execute".

**The measured hole.** Nothing joins them. A correctly signed bundle today
can carry any binary built from any tree, including a tree the promotion
gate never saw, because the manifest has no field naming the gated source.
Conversely the release gate's identity at M5-local is a source manifest
with no binary in it — dp-v2-021 d3 says so explicitly and defers the
binary digest to V2-022 and V2-034.

**Mechanism.** V2-034 adds `bundle_digest`, `candidate_id` and
`contract_release` to the module manifest, so the signature covers them and
the launcher can state that the binary it is about to exec came from the
source tree the promotion gate approved. These are **values copied into the
manifest at build time, not an import edge**: `internal/release` must still
never import `internal/update`, and `internal/update` must stay
standard-library-only. That preserves dp-v2-021 d12 and the `go/ast` guard
in `internal/release/source_guard_test.go`, and it avoids creating a
component edge that `ci/components.json` would then have to record — which
is V2-045's territory and prohibited here.

**Violation.** Removing any of the three claims as "redundant"; computing
`bundle_digest` inside `internal/update`; importing `internal/release` from
`internal/update` (or the reverse) to obtain it; a launcher that reports
provenance it cannot verify from the signed manifest alone.

### 3.4 Manifest schema evolution: one bump now, an accepted set forever

**Measured constraint.** `Verify` requires `manifest.Schema` to equal
`ManifestSchema` exactly (`update.go:60`) and decodes with
`DisallowUnknownFields` (`update.go:52`). Adding any field therefore makes
every already-installed Bootstrapper reject every new bundle — and the
Bootstrapper is precisely the one component that cannot be auto-updated
(section 4.3). This is the infinite-regress trap in concrete form: the only
way to change the bundle format is through the component only a human can
change.

**Decision.** Take the one breaking change now, then be additive forever.

1. V2-034 bumps the schema id once to
   `agentic-loop/module-manifest/v2`, carrying the section 3.3 fields and
   the section 5.1 coordinates.
2. At the same time it replaces the single constant with an ordered
   accepted set (`AcceptedManifestSchemas`), so later evolution is itself
   expand / coexist / migrate / contract at the manifest level: a new id is
   added to the accepted set in a Bootstrapper release, bundles start
   declaring it only after both machines accept it, and the old id is
   removed from the set only after no installed version still declares it.
3. The accepted set is data in `internal/update`, asserted by tests that a
   manifest declaring an unaccepted id is refused and that a manifest
   declaring an accepted older id still verifies.

**Expiry of the licence.** The bump is free only because no Stable release
exists and no Bootstrapper is installed on any machine. That licence
expires the moment a Bootstrapper is placed on a machine: from then on the
accepted-set change must land **first**, in its own release, with the old
id still accepted, before any bundle declares a new id.

**Violation.** Adding fields to the v1 struct without introducing the
accepted set — a design that can never be changed again without an
owner-attended visit to two machines; shipping a bundle that declares an id
no installed Bootstrapper accepts; removing an id from the set while an
installed version still declares it.

## 4. Decision 2: independent Bootstrapper

### 4.1 Structural independence and the guards that enforce it

**Decision.** "The process that updates X must not be X" is satisfied
structurally, not by convention. The Bootstrapper is a separate binary
(`cmd/bootstrap`) in a separate directory from the Runner, linking only
`internal/update`, and the Runner must not link `internal/update` at all.

Measured, it does not (section 1). V2-034 turns that from luck into a
guard: a `go/ast` import test, shaped like
`internal/release/source_guard_test.go`, asserting that

- no file under `cmd/runner/**` or `internal/runner/**` imports
  `internal/update`, and
- `internal/update`'s non-test files import only the standard library.

**Mechanism.** There is no code path in which the Runner can install or
switch, because the symbols are not in its binary. An update is: the parent
installs the new version side by side, flips the symlink, stops the child,
starts a new child from the new symlink target. **The process being
replaced never performs the install and never execs itself.**

**Violation.** Any import edge from the Runner to `internal/update`; any
non-standard-library import in `internal/update`; a Runner that shells out
to `bootstrap` in order to update itself.

### 4.2 `bootstrap run --channel`: the missing launcher

**Measured gap.** `cmd/bootstrap` has no `run` verb and nothing in the tree
reads `root/stable`, so the M8 completion condition "break the Preview
Control Plane and Runner and recover from the Stable launcher" has no
implementation on either side of the recovery: there is no launcher to
recover with, and no code that resolves the Stable pointer.

**Decision.** V2-034 adds `bootstrap run --channel stable|preview`, which:

1. resolves `root/<channel>` to `versions/<v>`;
2. **re-verifies** `versions/<v>/runner` against
   `versions/<v>/manifest.json` before **every** exec — install-time
   verification says nothing about the bytes on disk now;
3. probes confinement via `runner.NamespaceConfinement.Probe` and refuses
   if it fails (gap item 10, refusal shape per section 3.2);
4. starts the Runner as a child in its own process group.

**Who touches what, for "recover from the Stable launcher".**

| Actor | Reads | Writes |
| --- | --- | --- |
| Launcher (parent) | anchor set, `root/<channel>`, `versions/<v>/manifest.json`, `versions/<v>/runner` | `versions/**`, channel symlinks, `root/state/installed.json` |
| Runner (child) | its own configuration, canonical store | canonical store |

Canonical state is touched by **none** of the launcher's steps: the
launcher has no canonical store client at all. Recovery is a filesystem and
process operation, which is why a broken Preview Runner cannot prevent the
Stable launcher from starting a Stable child.

**Violation.** An exec without re-verification; an exec after a failed
confinement probe; a child started in the parent's process group; a
launcher that reads or writes canonical state.

### 4.3 Where the regress stops

**Decision.** The Bootstrapper's own update stays outside self-update, and
the regress stops there explicitly. The bundle format has **no**
`bootstrapper` role, and the launcher refuses to install any bundle that
declares one, so a compromised or buggy release cannot replace the
component that verifies releases. Replacing the launcher, or changing the
anchor set, is an owner-attended out-of-band procedure **per machine**:
place the new binary or anchor line, verify its digest by hand against the
release record, restart the launcher. A launcher change is a human decision
that does not pass through the Loop's own promotion gate.

**Why stopping there is acceptable.** The launcher's attack surface and
change rate are deliberately minimal: no network, no Provider, no canonical
store, no workspace. Its entire input set is (anchor set, bundle bytes,
root path, channel). A component with four inputs and no network is one a
human can re-verify by hand in minutes; a self-updating verifier is not.

This decision is not reopened here. `docs/architecture/technology.md`
section 7 already fixes it
("Bootstrapper自身は自動更新対象から外し、人間承認の別手順でのみ更新する"), and this
document's job is to say where the regress stops and why stopping there is
safe.

**Violation.** A bundle role that installs a launcher; an automated anchor
update; a launcher that fetches anything over the network.

## 5. Decision 3: module version manifest

### 5.1 Five coordinates, signed

**Decision.** One signed record per version, with five coordinate groups:

1. **binary** — `version` (semver) and `binary_sha256`.
2. **canonical schema** — the closed interval `[schema_min, schema_max]`
   this binary can operate against. This already exists (`update.go:31`)
   and is **the coexist mechanism**, not a validation detail; section 6
   depends on reading it that way.
3. **contract** — `contract_release` and `contract_digest`, plus the Runner
   API compatibility range `runner_api_min` / `runner_api_max`.
4. **bundle** — `bundle_digest` and `candidate_id` from V2-021's assembly.
5. **provenance** — `key_id` and `algorithm` from section 3.1.

`docs/architecture/release-contract.md` section 4 item 8 requires
implementation, schema, migration, configuration and user documentation to
be promotable as one version. A version record carrying only a binary
digest cannot express that, and one carrying the source bundle digest but
not the schema interval cannot express coexistence.

### 5.2 `root/state/installed.json` is machine-local and never canonical

**Decision.** A single machine-local record holds: every installed version
with its install time and its `[schema_min, schema_max]`; the current
`stable` and `preview` targets; the previous stable target; the last switch
with its reason; `rolled_back_at`; and `window_closed_at` together with the
criterion that closed it. It is written **after** the filesystem operation
it describes, so a crash leaves it stale-but-safe rather than describing a
state that does not exist.

**Why machine-local.** Machines are not shared. A canonical "what is
installed" record would be wrong on both machines for most of the duration
of every update, and would invite a reader to treat one machine's
filesystem as authoritative for the other's.

`installed.json` plus an injected clock is the entire input set of the GC
predicate (section 8) and the rollback window (section 9).

**Violation.** Writing `installed.json` before the operation it describes;
promoting it to canonical state; a GC or window decision that reads
anything else, especially the wall clock.

## 6. The mixed-version period is unavoidable, legal and bounded

**Decision.** Because machines are not shared, two Runners cannot be
switched atomically, so there is always an interval in which machine A runs
version N+1 and machine B runs version N. This is not a risk to mitigate;
it is a state to make legal and bound.

**The invariant.** At every instant, the current canonical schema lies
inside the **intersection** of the `[schema_min, schema_max]` intervals of
every version any machine can currently route to — stable, preview and
previous stable, on both machines. Consequences:

- a version may be switched onto a machine only if the canonical schema is
  inside its interval;
- the canonical schema advances only into the intersection;
- an advance that would empty the intersection is **refused, not
  scheduled**.

**Split of enforcement, stated honestly.**

- V2-034 enforces the half a single machine can see: the launcher refuses
  to switch onto a version whose interval excludes the canonical schema it
  reads, and records the interval of every installed version in
  `installed.json`.
- The cross-machine half needs the Control Plane to know which binary
  version and interval each Runner is running. Measured, it cannot:
  `internal/api/api.go` carries only the static `"schema_version": "v1"`
  (`api.go:84`) and optimistic-concurrency `Expected*Version` fields, with
  no version on any Runner-facing request, and `internal/domain` has no
  Runner aggregate (`model.go:16` is a bare `RunnerID string`). **The input
  to the cross-machine side of this invariant is prepared by V2-069**
  (escalation E1). It is not V2-034's work and must not be invented inside
  an update-gated task.

**One distinction that must not be conflated.** Preview/Stable *routing* is
per Repository (`docs/architecture/release-contract.md` section 6), while
binary *version* is per machine. A Runner API compatibility break is
expressed by `runner_api_min` / `runner_api_max`, never by treating a
routing channel as a version.

**Violation.** Scheduling a schema advance that empties the intersection
"to be fixed by the next release"; switching onto a version whose interval
excludes the canonical schema; deriving a machine's running version from
its Repository's channel.

## 7. Decision 4: schema expand / coexist / migrate / contract

### 7.1 The four stages as concrete operations on the measured codec

The codec is `{record_schema, kind, payload}` with the payload stored as a
JSON string; `RecordSchema` is `"v1"`; both readers refuse on envelope
inequality; payload decoding is a plain `json.Unmarshal`, so unknown
payload fields are ignored and absent payload fields default (section 1).

- **EXPAND.** Write the new payload field *in addition to* the old one,
  under the unchanged `record_schema`. No reader change is needed, because
  unknown-field tolerance already holds. The canonical schema counter goes
  N to N+1 and the new binary declares `[N, N+1]`. Fully reversible: the
  old binary keeps reading the old field.
- **COEXIST.** Both binaries run and every reader resolves a value as "new
  field if present, else old field". **This period is exactly the
  mixed-version window of section 6**, and lasts at least until both
  machines are switched. Fully reversible.
- **MIGRATE.** A bounded, resumable, idempotent backfill that rewrites old
  documents to carry the new field, shaped like a reconcile tick: at most
  100 documents and 30 s per tick, restartable from a stored cursor, no
  unbounded query, no wall-clock dependence
  (`docs/architecture/validation.md` section 5). Still reversible, because
  expand never removed the old field.
- **CONTRACT.** Stop writing the old field and delete it, bumping
  `record_schema` only where the envelope itself must change. Contract is
  **always a separate Increment from its expand**.

**Violation.** Collapsing expand and contract into one release — this
destroys the rollback path; a reader that requires the new field; an
unbounded or clock-dependent backfill.

### 7.2 The reversibility boundary: contract is the first irreversible stage

Rollback is possible during expand, coexist and migrate, and **impossible
after contract**: a binary whose `schema_max` is below the post-contract
schema can no longer read the store, so the contract step itself closes
that version's rollback window — no timer is involved.

Therefore **contract may run only when, for every version still routable on
any machine and for the current and previous stable, `schema_min` is at or
above the post-contract schema.** This is the *same predicate* as GC
eligibility (section 8), used a second time. That is deliberate: expressing
"may I contract?" and "may I delete?" as one predicate used twice is what
stops the two from disagreeing.

### 7.3 The envelope half is blocked: E2 and V2-070

Because both readers compare `record_schema` with `!=` (`store.go:79`,
`store.go:173`), documents written under a new envelope id are invisible to
the current reader. Envelope-level coexistence is impossible today, and a
`record_schema` bump would be an immediate outage rather than a stage.
Widening the readers to an accepted set `{v1, v2}` *is* the expand step for
the envelope and must land before any bump.

That widening touches `internal/store/firestore/**`, which is the
`store-firestore` component (declared dependencies `domain` and
`application`), not `update`; pulling it into an update-gated task would
add components to the task and to the M8 gate row. **The envelope expand
stage is owned by V2-070**, which is a dependency of both V2-034 and
V2-035, and it blocks M8 completion condition 3 ("roll back without losing
shared state").

**V2-034 must not edit anything under `internal/store/firestore`.** It proves the four
stages against an **injected codec port with an in-package fake**, and the
claim it is entitled to is exactly that: the four stages are proven against
that port, and not yet against the emulator.

## 8. Decision 5: old binary and document GC

**Decision.** The predicate for deleting `versions/<v>` on a machine
reproduces `release.RetentionEligible` **by value** and adds two refusals
that only exist once binaries are real. Refuse if:

1. `v` is the current stable route target;
2. `v` is the previous stable route target;
3. `v`'s rollback window is still open — evaluated against an **injected
   clock** and a contract-declared duration, never the clock alone, because
   `docs/architecture/validation.md` section 8 forbids clock-only expiry;
4. any Requirement's `StableSnapshot` still references `v`;
5. `v` is the target of **any** channel symlink on this machine, including
   `preview` — `RetentionEligible` models stable and previous-stable but
   not preview, which is a live pointer;
6. deleting `v` would leave no retained version whose
   `[schema_min, schema_max]` contains the current canonical schema,
   because that would make the rollback window unsatisfiable while it is
   still open.

Refusals 1–4 are route- and reference-shaped; refusal 6 is the one that
ties deletability to the version manifest, which is what makes "cannot
delete while the rollback window is open" **follow from the manifest**
rather than from a timer.

**Re-expressed, not imported.** dp-v2-021 d12 forbids the edge between
`internal/release` and `internal/update` in both directions, so the
predicate is re-expressed in `internal/update` over the same inputs, and a
test asserts case-by-case agreement with `release.RetentionEligible`'s four
outcomes on identical inputs. That is a value-level equivalence, not a code
edge. V2-034 adds the **mirror-image** `go/ast` guard inside
`internal/update`: the existing guard lives in `internal/release` and would
not catch an edge pointing the other way.

**Crash-safe deletion order.** Re-read every channel symlink immediately
before deleting; refuse if the target is reachable from any of them; rename
the directory aside; remove it; update `installed.json` afterwards.

**One predicate, not two.** The bundle carries that version's Stable and
Preview documents (`docs/architecture/technology.md` section 7), so this
single predicate is also what satisfies
`docs/architecture/release-contract.md` section 5's requirement to retain a
rollback target's user documents until the window closes. There is no
second document-retention rule, and M8's fourth completion condition is
therefore one check rather than two that can disagree.

**Violation.** A GC pass that reads the wall clock; deleting a preview
target because it is neither current nor previous stable; importing
`internal/release` to "avoid duplication"; deleting the last version whose
interval contains the canonical schema; a separate document-retention
timer.

## 9. Decision 6: the rollback window

### 9.1 Four conditions

**Decision.** A version stays inside its rollback window while **any** of
the following still holds:

1. **generation** — it is the previous stable route target; the window
   opens when a successor becomes Stable and cannot close while nothing
   newer has been Stable;
2. **schema stage** — no contract-stage step has moved the canonical schema
   outside its `[schema_min, schema_max]`;
3. **time** — a contract-declared minimum dwell has not yet elapsed **on an
   injected clock** since the successor became Stable, so a rapid
   succession of releases cannot evict the only rollback target;
4. **evidence** — the successor does not yet hold a passed Preview
   capability exercise for the whole Release Contract, so a rollback target
   is never dropped in favour of an unproven successor.

Closure is the **logical conjunction of all four** having ceased: the
window is open while the disjunction of the four "still holds" clauses is
true, and closes only when every one of them is false.

A purely time-based window would contradict
`docs/architecture/validation.md` section 8; a purely generation-based
window would close at the instant a successor is promoted, which is
precisely when rollback is most likely to be needed.

### 9.2 Closure is recorded, never recomputed

**Decision.** Closure is written as `window_closed_at` plus the criterion
that closed it, into `installed.json` and the release record. **Nothing
recomputes closure from the wall clock at read time.** A recorded closure
is what makes the GC predicate reproducible after the fact: a reviewer can
ask "why was this deletable?" and get an answer that does not depend on
when the question is asked.

**What becomes irreversible at closure.**

- The binary and its bundled documents become GC-eligible. After deletion,
  returning to that version requires rebuilding and re-signing from source;
  the cheap route rollback — a symlink flip — is gone.
- If closure was caused by a schema contract step, the store no longer
  serves a shape that version can read, so even a rebuilt binary cannot be
  routed. **This is the only one-way transition in M8.**

**Violation.** Recomputing the window from the wall clock at read time;
closing the window on one criterion; deleting a version with no recorded
`window_closed_at`.

### 9.3 The known defect in `update.Switch`, and the fix to mirror

**Measured.** dp-v2-021 d8 removed the ping-pong defect from
`release.Router`: a rollback clears `PreviousStableDigest`
(`internal/release/release.go:160`), so a second consecutive rollback is
refused, and `Promote` refuses any digest that is not the current preview
route (`release.go:134`). `update.Switch` (`internal/update/update.go:116`)
still has that defect in full: it flips `stable` to **any** installed
version, with no record, no monotonicity and no gate requirement. Left
alone, the update layer would re-introduce a defect the release layer
already paid to remove.

**Decision.** V2-034 mirrors the fix:

- record every switch with its reason, into `installed.json` (gap item 11);
- refuse a second consecutive rollback;
- require the forward direction to name a gate-passed candidate digest.

**Violation.** A switch with no recorded reason; two consecutive rollbacks;
a forward switch onto a version with no gate-passed candidate.

## 10. Environment classification and the M8 completion conditions

### 10.1 Classification

**Local closure — V2-034, hermetic, no deploy.**

- the fail-closed anchor and signature refusals (section 3.2);
- the three-digest join as manifest data (section 3.3);
- the accepted-manifest-schema set (section 3.4);
- the import guards making the Bootstrapper independent (section 4.1);
- `bootstrap run` with per-launch re-verification and child restart
  (section 4.2);
- the manifest fields and the single-machine interval check (sections 5.1
  and 6);
- the four stages and the reversibility boundary over the injected codec
  port (section 7);
- the GC predicate and crash-safe deletion (section 8);
- the rollback window predicate and its recorded closure (section 9);
- the monotonic, recorded `Switch` (section 9.3).

**preview-local — V2-035, real processes, real CLI, Firestore emulator,
owner machine.**

- the new Loop deploying its own next version to Preview;
- breaking a real Preview Control Plane and Runner and recovering through
  the Stable launcher;
- rolling back without losing shared canonical state;
- two roots or machines on different versions both making progress.

**preview-gcp — D1 and V2-054, beyond M8.**

- KMS-backed signing, Artifact Registry distribution, and the deploy path
  itself. Neither V2-034 nor V2-035 may claim any of these.

### 10.2 Mapping onto roadmap M8's four completion conditions

| M8 completion condition | Grade | Owner | Note |
| --- | --- | --- | --- |
| 1. 新Loopが自分自身の新versionをPreviewへdeployする | preview-local | V2-035 | |
| 2. Preview Control Plane／Runnerを壊し、Stable launcherから復旧する | preview-local | V2-035 | Loop-own-behaviour condition |
| 3. shared stateを失わずrollbackする | preview-local | V2-035 | blocked on E2, i.e. V2-070 |
| 4. rollback window中の旧binary／docsを削除しない | local closure | V2-034 | one predicate, section 8 |

Condition 2 is a **Loop-own-behaviour** condition.
`docs/architecture/release-contract.md` section 3 defines `preview-local` as
real processes, a real Provider CLI and real GitHub on the owner's machine
with a Firestore emulator as the canonical store, and states that this is
the real environment for a capability exercise whose subject is the Loop's
own behaviour, not a substitute for one.
`docs/operations/v2-task-dag.md` G3 clause 2 says the same in gate terms.
Condition 2 therefore does not belong to D1.

**M8 closes at preview-local.** `docs/architecture/release-contract.md`
section 3 names exactly four things `preview-local` cannot prove and that
D1 must prove with `preview-gcp`: (i) the IAP authentication boundary,
(ii) scale-to-zero, (iii) real Firestore permissions and contention, and
(iv) the deploy path. **None of the four is named by any M8 completion
condition.** Saying so plainly is what stops V2-035 from being blocked on
GCP work the owner deliberately deferred, and stops V2-034 from claiming a
live result.

## 11. The V2-034 / V2-035 boundary, claim by claim

**V2-034 owns** `internal/update` and `cmd/bootstrap` plus this runbook:
the accepted schema set, the manifest v2 fields, trust anchor resolution
and refusals, `bootstrap run`, per-launch re-verification, the confinement
probe at launch, the monotonic recorded `Switch`, `installed.json`, the GC
predicate and its executor, the stage machinery and the bounded resumable
migrator over an injected port, and the import guards.

**V2-034 must not** deploy anything, start a real Provider CLI, touch GCP,
a KMS key or a credential, generate a real signing key pair, edit
`internal/store/firestore/**`, or report any of M8's conditions 1 to 3 as
met.

**V2-034's honest claim** is that the local closure refuses the wrong
bundle, launches only a re-verified binary, keeps a rollback target and its
documents while the window is open, and cannot delete or contract past a
still-routable version.

**The trap to name explicitly.** A launcher restarting a child in a unit
test is **not** M8 condition 2, which names a Preview Control Plane and
Runner. The most V2-034 may claim is: *"the launcher re-execs a re-verified
binary after the child exits."* Recovering a broken Preview is V2-035's
work.

**V2-035 adds no update semantics.** If it finds it needs a new refusal or
a new manifest field, it stops and escalates rather than editing
`internal/update`: a semantics change discovered during a live exercise
invalidates the local evidence it was supposed to build on. V2-035's
evidence must record environment class `preview-local`, machine identifiers
for both roots or machines, the emulator name and version, the kernel
version, Provider CLI versions where one is used, and a skip count of zero.

**E4, resolved.** `cmd/bootstrap` is a `runner` component root and
`make component-runner` already runs `./cmd/bootstrap`, so V2-034's launcher
work moves the `runner` component key as well as the `update` one. V2-034
therefore emits **two** evidence records, one for `update` and one for
`runner`, as V2-058 did. None of E1 to E4 may be resolved by editing
`ci/components.json`, the `Makefile`, `go.mod`, `go.sum` or `devbox.*`;
component-manifest and evidence-key repair is V2-045's task.

## 12. Determinism required of V2-034

- no fixed sleep, no wall-clock dependence, no background timer, no
  goroutine in tests;
- bounded deadline polling only, against an injected clock;
- one top-level test function per journey;
- `t.TempDir` fixtures, and no dependence on another test's order.

A result that needed a retry to pass is a **stop-and-escalate** condition,
not a pass. A skipped check is recorded with status `skipped` and is never
counted as a pass (`docs/operations/v2-task-dag.md` G7).

## 13. Escalations

| Id | Subject | Disposition | Blocking relation |
| --- | --- | --- | --- |
| E1 | No canonical Runner version reporting: no domain Runner aggregate (`internal/domain/model.go:16`), no version field on any Runner-facing request (`internal/api/api.go`) | carved out as **V2-069**, which prepares the input to the cross-machine side of the section 6 invariant | not V2-034's work; the single-machine half is enforceable without it |
| E2 | `internal/store/firestore`'s readers accept exactly one `record_schema` (`store.go:79`, `store.go:173`), so envelope coexist is impossible | carved out as **V2-070**, which owns the envelope expand stage | dependency of V2-034 and V2-035; blocks M8 condition 3 |
| E3 | Whether Cloud KMS offers Ed25519 asymmetric-sign in the target project is a D1-time fact | approved with no additional work: the anchor set admits a second `algorithm` (section 3.1) | none |
| E4 | The launcher lives in `cmd/bootstrap`, a `runner` component root | approved: V2-034 emits both an `update` and a `runner` evidence record | none; the DAG row is updated by the coordinator, not by this task |
| E5 | The cross-machine half of the section 6 invariant is not implementable inside V2-034: the Control Plane cannot learn which binary version and interval each Runner is running until V2-069 lands | **escalated by V2-034, no work attempted.** V2-034 implemented only the single-machine half (section 15.7): the launcher refuses to route a version whose interval excludes the canonical schema it reads. The cross-machine half stays unimplemented and unclaimed | not blocking the local closure; blocking any claim about two machines at once |

## 14. How this document is verified

This document is a design artifact. The only checks V2-033 executed are the
read-only measurements quoted above, each with its argv, and, on the
committed tree, `make docs`, `make component-docs`,
`go test -count=1 ./internal/contracts`, `make check` and
`make evidence-keys`. V2-033 changes no file under `internal/update/**`, so
the `update` component evidence key is unmoved by design; the claim of the
accompanying evidence record is "the design document exists, agrees with
the measured tree, and the `update` component check is green", not that any
new behaviour was proven.

## 15. Implementation notes from V2-034 (the local closure as built)

Sections 1 to 14 are V2-033's design. This section records where the built
local closure is narrower, stricter or more explicit than the design text,
so a reader of the code is not left inferring which is authoritative. Every
statement below is about `internal/update/**` and `cmd/bootstrap/**` on this
tree, and about nothing else.

**15.1 What V2-034 does NOT claim.** M8 completion conditions 1, 2 and 3 are
**not** met and are not reported as met anywhere in this task's code, tests
or evidence. They are preview-local and belong to V2-035. In particular:
"the launcher re-execs a re-verified binary after the child exits" **is not
M8 completion condition 2**, which names breaking a real Preview Control
Plane and Runner and recovering from the Stable launcher. A launcher
restarting a child in a unit test is not a recovery of a Preview
environment. Nothing here was deployed, no Provider CLI was started, no GCP
project, Cloud KMS key, credential or real signing key was used, and no key
pair intended for any machine was generated: every key in a test is
generated inside that test and dies with it.

**15.2 The anchor is stricter than the six-item list.** Section 3.2 item 2
names "symlink to a non-regular target"; the implementation uses `Lstat` and
refuses **any** symlink at the fixed path, because the anchor's identity must
be the fixed path itself and not something a link can retarget. A seventh
refusal is added: a line that is not exactly `key_id algorithm
base64(pubkey)`, or that repeats a `key_id`, is a refusal rather than a line
to skip. The mode bound is expressed as "no permission bit outside `0600`",
so the canonical `0400` and a plain `0600` are accepted and any group bit,
other bit or execute bit is refused. The invoking uid is an injected field
rather than a call to `os.Getuid` inside the resolver, which is what makes
the ownership refusal reachable in a test without a second real uid; the
production constructor supplies `os.Getuid()`.

**15.3 Provenance is required by every accepted manifest id.** Section 3.4
keeps the older id in the accepted set. The implementation requires `key_id`
and `algorithm` under **both** accepted ids, because selecting an entry out
of a set is a launch-time requirement rather than a v2 feature, and refuses
a manifest declaring the older id that carries any v2 coordinate. A
consequence worth stating: a version whose manifest carries no
`candidate_id` — which is every manifest declaring the older id — can never
be moved forward on any channel, because a forward move must name the
candidate the signed manifest carries. The licence to require this is the
one section 3.4 already names: no Stable release exists and no Bootstrapper
is installed on any machine.

**15.4 The detached signature is persisted, and the launcher re-checks it.**
Section 4.2 lists the anchor set among the launcher's reads, which is only
meaningful if the launcher can re-run a signature check. `Install`
therefore writes `versions/<v>/signature` (mode `0400`) beside `runner` and
`manifest.json`, and the launcher's per-launch re-verification is **the same
`Verify`** the install ran, over freshly read bytes: there is no weaker
on-disk check. A missing signature file is a refusal, not a fallback to a
digest-only comparison. The manifest also gains a `roles` field so section
4.3's rule is executable: a bundle declaring a `bootstrapper` role is
refused.

**15.5 `bootstrap run` is bounded, and the restart count is an argument.**
The launcher performs up to `--launches N` verified launches, re-resolving
the anchor set, re-reading the channel pointer and re-verifying the bytes on
disk before each one. There is no timer, no backoff and no unbounded loop.
The child is started through an injected process port; the production port
sets `Setpgid`, and a test executes a real child and reads its process group
from the kernel to show the launcher is not in it.

**15.6 The window's criterion is always a schema movement.** Section 9.1's
closure is the conjunction of all four clauses having ceased, and clause 2
holds while the canonical schema is inside the version's interval.
Therefore a closure always requires the canonical schema to have left that
interval: the generation, dwell and evidence clauses can **delay** a
closure but can never cause one. The recorded criterion accordingly
distinguishes `schema-contract` (a contract step moved it, which is the one
one-way transition in M8) from `schema-advance` (an expand-stage bump moved
it, after which the shape is still in the store and the refusal is the
declared interval). Closure is recorded once, is idempotent, and is never
recomputed: a later evaluation with every clause holding again and the clock
moved backwards returns the same recorded closure.

**15.7 The single-machine half of the section 6 invariant, and only that.**
`Switch` refuses to route a version whose `[schema_min, schema_max]`
excludes the canonical schema the machine reads, and records the interval of
every installed version. The cross-machine half is escalation E5 above: it
needs V2-069's Runner version reporting, and no part of it was invented
here.

**15.8 A third GC refusal.** Section 8 lists six. The implementation adds a
seventh, which section 9.2 already required as a violation rather than as a
refusal: a version whose rollback window opened and whose closure was never
recorded is not deletable. Refusals 5 to 7 are all inert on the inputs the
release-parity table uses, so the case-by-case agreement with
`release.RetentionEligible`'s four outcomes is exact and not approximate.

**15.9 The stage machinery's port, and what it proves.** The four stages run
against an injected codec port with an in-package fake. Nothing under
`internal/store/firestore/**` or `internal/store/memory/**` was read or
changed. `Contract` deliberately does not touch the envelope: bumping
`record_schema` is the store's expand step and belongs to V2-070. The claim
is exactly the one section 7.3 allows — the four stages are proven against
that port, and not yet against the emulator.
