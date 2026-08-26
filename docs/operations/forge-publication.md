# Forge publication — publishing one verified commit as one reviewable branch

This document is the record of how the Loop puts an Increment's result somewhere
a person can read it, and of exactly what that does and does not guarantee. It
describes the code in `internal/runner/forgepublish.go`,
`internal/runner/publishsource.go` and `internal/application/publication.go`.

## What is published, and from where

What is published comes from the **verified commit** in the Execution's working
copy, never from the caller's intended change set. A change set is what a caller
meant; the committed tree is what the Git-level verification actually measured.
Publishing the former would mean the reviewable artefact was never the verified
one.

The publication target comes only from a **registered Repository**, reached
through the Requirement's repository link:

    Execution -> Increment -> Requirement -> RequirementRepositoryLink -> Repository

There is no field on the publication request, and no environment variable in
production, through which a caller can name an owner, a name, a host or a
locator. A Requirement with no link, a Repository that does not exist and a
Repository that has been retired are each refused, and a refused publication
creates no Outbox Item at all.

## The reserved branch prefix and the ref naming rule

Every published ref is

    refs/heads/agentic-loop/<increment_id>/<execution_id>

produced by one pure function (`domain.PublicationRefName`) that is the only
producer of a ref name anywhere in the repository. Two consequences are
structural rather than promised: retrying one Execution's publication targets
exactly one ref, and a second Execution of the same Increment gets its own ref
instead of moving the first one.

`agentic-loop/` is a **reserved** first segment. Measured on this repository,
the only two push-triggered workflows filter branches `[v2]`
(`.github/workflows/ci.yml`) and `[main]` (`.github/workflows/deploy.yml`), so a
ref under the reserved prefix matches neither trigger. The gated live check does
not stop at that argument: after the write it reads the workflow-run count for
the published branch and requires zero.

## The four content-addressed equalities

A publication is confirmed only when all four of these hold. Nothing else
confirms it — no HTTP status is parsed, no error body is read, and no status
text is recorded anywhere.

1. **Blob.** Each blob object name the forge returns equals the object name the
   local repository reported for the same path (`ls-files --stage`).
2. **Tree.** The created tree object name equals the locally verified head tree.
   That is achieved by sending the base commit's tree as the base tree and only
   the changed entries, so the forge composes the same tree the local repository
   composed.
3. **Commit.** The created ref's commit carries that same tree, and its parent
   list is exactly the one base commit.
4. **Ref.** The ref read back is exactly the ref this operation intended.

The local commit's own object name and the published commit's object name are
**recorded and compared as a measurement, and their agreement is not required**.
The forge constructs the commit object, so any difference in how author or
committer fields are serialised changes its name; requiring equality would be
asserting an unmeasured fact about another system's encoding. A recorded
difference there is not a defect.

Only added and modified files with mode `100644` or `100755` are
representable. A symlink (`120000`), a submodule gitlink (`160000`), a change
that would require deleting a path and a change with zero changed paths are each
refused **before any forge call could exist**, with a named sentinel, so no
object is created for a publication that could not agree with the verified tree.
File content travels base64-encoded, so a binary file cannot corrupt a request
body.

## Create-only, never updated, never forced

The observable step is the creation of the ref. It is a compare-and-set against
the state "the ref is absent": creating a ref that already exists fails, which is
the conditional form the forge offers and needs no separate idempotency key.

- No code path updates a ref, deletes a ref or forces anything. The set of API
  operations the adapter can **name** is a closed list read from exactly one
  place, and that list itself is the refusal: it has four writes (create blob,
  create tree, create commit, create ref), two reads (read ref, read commit) and
  one measurement read (the workflow-run count for a branch), and nothing else.
- The string `force` appears as a key in no request body. Every body is a typed
  struct with declared fields — there is no map-shaped body anywhere in the
  adapter — so no key can be added at run time, and a test asserts the absence
  over every constructed body.
- A fast-forward update with force disabled would also be a compare-and-set, and
  it is still excluded: it widens the operation from "produce a new reviewable
  branch" to "move a branch a person may be reading".

## The three outcomes, and where needs-input goes

Duplicate detection is a **read of the ref performed before the create**, and a
read of the ref performed again after any failure. The ref is read through the
forge's matching-refs form, so "this ref does not exist" is a successful read
with an empty list rather than an error the adapter would have to tell apart
from a transport failure by parsing a status code.

| Read before the create | Outcome | Outbox Item |
| --- | --- | --- |
| absent, then created, all four equalities hold | published and observed | delivered |
| already present, its commit carries the intended tree | converged on the existing ref; no create call is made at all | delivered |
| present, its commit carries a different tree | undecidable | ambiguous, then **needs-input** |
| the write's outcome is unknown | undecidable; the read after the write decides | ambiguous, then confirmed or needs-input |

An undecidable outcome is reported as exactly that, through
`application.ErrEffectUndecidable`, which the outbox protocol's ambiguity
predicate recognises beside the deadline, cancellation and network cases. That is
what lets the Outbox Item reach **needs-input** instead of a guess, without
wrapping a deadline error the adapter never saw.

V2-072 stops at the Outbox Item's own needs-input status plus the recorded
reason. It does **not** move the Requirement to needs-input and does not create
an owner question: that surface belongs to the Requirement-level needs-input
vocabulary, and duplicating any of it here would produce two ways to ask a person
the same question.

The publication Observation is keyed by the operation identifier and is written
**at most once**. Its state vocabulary is closed and contains no value meaning
completed, resolved, accepted or done: the terminal success value means
*published and observed*, and nothing more. A published branch is not a finished
Requirement, and a confirmed publication changes no Requirement and no Increment
status.

## Credentials, output and confinement

- No credential value enters argv, the environment or any file. The forge CLI
  reads its own configuration store; the granted set is empty and a non-empty one
  is refused before any process could exist. Request bodies travel on the child's
  standard input, never in argv.
- The child receives exactly the guarded base environment of `HOME` and `PATH`.
- The child's standard error is discarded outright and its standard output is
  bounded by a hard byte cap. No raw forge response, no child standard error and
  no status text is recorded, returned, logged or journaled.
- `git push` is never attempted, and no push argv is constructible: the git
  subcommand allowlist has exactly fourteen entries and `push`, `fetch`,
  `remote`, `submodule`, `config`, `credential` and `worktree` are all absent
  from it. The git child still receives no `HOME`.
- The forge child is **not** filesystem-confined, and that is stated rather than
  hidden: it writes nothing into the Execution workspace, so a mount namespace
  would bound nothing it does. It is bounded instead by a closed argv shape, an
  empty granted set, a bounded output cap and a context deadline. The namespace
  confinement every git child runs under is not relaxed by one bit.

## Deleting a published ref, and the two residues deletion does not remove

A published branch is the artefact, so nothing in the code deletes it. An owner
removes one explicitly:

    gh api --method DELETE repos/<owner>/<name>/git/heads/agentic-loop/<increment_id>/<execution_id>

That command removes the reviewable artefact completely. It does **not** remove
two residues, and neither can be removed:

1. The blob, tree and commit objects created remain reachable by object name
   after the ref is gone.
2. The ref-creation entry in the repository's own activity record cannot be
   deleted at all.

"Reversible" for this operation therefore means exactly: a deletable ref, plus
undeletable objects, plus an undeletable activity entry. A failed publication can
also leave objects behind — unreferenced but reachable by object name. That
window is narrowed by refusing every unrepresentable case before the first write
and by creating the ref only after every object name agrees; it is not closed.

## The gate variables, and the fact that the target is owner-designated

The single live publication is gated on two environment variables:

| Variable | Meaning |
| --- | --- |
| `AGENTIC_LOOP_LIVE_FORGE_WRITE` | `1` enables the live check. Anything else skips it, and the skip is recorded with its reason, never as a pass. |
| `AGENTIC_LOOP_LIVE_FORGE_WRITE_REPOSITORY` | The owner's designation, as `<owner>/<name>`. **There is no default**, and in particular no default to the Loop's own registered repository. |
| `AGENTIC_LOOP_LIVE_FORGE_WRITE_ORIGIN` | Optional clone source for the live working copy. It is never a write target. |

With the gate set and the designation absent the check **fails** naming the
missing designation; it does not skip and it does not fall back. Hardcoding a
target would make the check's meaning depend on the unmeasured fact that the
owner accepts agent-written refs in that repository. Creating a dedicated sandbox
repository is an owner action.

`devbox run --pure` strips the environment, so the gate cannot fire by accident
and `-e` is required to set it. Inside `make check` nothing resolves a hostname,
opens a socket or starts the forge CLI: every origin is a bare repository created
at test time by the real git binary in a temporary directory, and the only
injected thing in the protocol tests is the transport, as a function.

## What is deliberately not wired

**No production loop calls `Dispatch` yet.** There is no dispatcher construction
anywhere outside tests: the sink and the observer are wired through the real
dispatcher in the deterministic tests and in the gated live check, and a
scheduled or supervised dispatcher process is a separate, named follow-on. Who
owns that loop, what its interval is and how it interacts with the Runner's own
lease are decisions this work has no basis for, so the gap is named here rather
than left to be discovered.

There is also no Pull Request, no tag, no release, no issue, no comment and no
HTTP or console surface for a publication. The human-readable pointer is the
branch on the forge plus the recorded Observation.
