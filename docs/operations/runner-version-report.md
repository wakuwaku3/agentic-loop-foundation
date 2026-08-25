# Runner version reporting

What the Control Plane knows about which binary each Runner is running, how it
came to know it, and — just as important — what it is not allowed to do with
that knowledge.

This document describes the surface V2-069 built. It does not restate
`docs/operations/self-update.md`, which V2-034 and V2-035 own; it names the
sections it depends on and stops there.

## 1. Why this exists at all

`docs/operations/self-update.md` section 6 states an invariant over every
machine at once:

> At every instant, the current canonical schema lies inside the
> **intersection** of the `[schema_min, schema_max]` intervals of every version
> any machine can currently route to.

The same section splits enforcement honestly. `internal/update` enforces the
half a single machine can see. The cross-machine half needs the Control Plane
to know which binary and which interval each Runner is running, and section 6
records the measurement that it could not: `internal/api/api.go:84` emits a
static `"schema_version": "v1"`, the only other version-shaped fields in
`internal/api` are the optimistic-concurrency `Expected*Version` fields, and
`internal/domain/model.go:16` declares a bare `type RunnerID string` with no
Runner aggregate anywhere.

V2-069 supplies that missing input. It supplies **only** the input.

## 2. Which coordinates are reported, and which are not

Section 5.1 of `self-update.md` enumerates five coordinate groups. Two are
reported here.

| Group | Coordinates | Reported by V2-069 | Why |
| --- | --- | --- | --- |
| 1. binary | `version`, `binary_sha256` | yes | These identify the bytes that are running. `version` is a value a running binary genuinely has (`cmd/runner/main.go:13` carries `const version`). |
| 2. canonical schema | `schema_min`, `schema_max` | yes | Section 5.1 item 2 states the interval is *the coexist mechanism*, and section 6's invariant is written over intervals and nothing else. It is the one coordinate the invariant actually consumes. |
| 3. contract | `contract_release`, `contract_digest`, `runner_api_min`, `runner_api_max` | no | Owned elsewhere: see below. |
| 4. bundle | `bundle_digest`, `candidate_id` | no | Owned elsewhere: see below. |
| 5. provenance | `key_id`, `algorithm` | no | Owned elsewhere: see below. |

Two further coordinates a manifest does carry — `os` and `architecture` — are
also excluded, for the opposite reason: they are real values, but no rule
reads them, and adding them would widen this surface past the
version-and-compatibility question it exists to answer.

### Who owns groups 3, 4 and 5

- **V2-034 owns the manifest fields**, and has landed:
  `internal/update/update.go:78-79` and `:90-95` now declare `key_id`,
  `algorithm`, `bundle_digest`, `candidate_id`, `contract_release`,
  `contract_digest`, `runner_api_min` and `runner_api_max` on `Manifest`, and
  `internal/update/state.go:183` copies three of them into the machine-local
  `installed.json`.
- **V2-074 owns the compatibility decision** — what a control plane should do
  with a reported contract release or Runner API range. Reporting a coordinate
  before anything is allowed to read it would be a surface that reads as
  informative while knowing nothing, which is the exact defect the static
  `schema_version: "v1"` above already is.

Measured, and the reason a placeholder would be dishonest rather than merely
premature: **no code in this repository constructs a `Manifest`.** A search for
`Manifest{` across every non-test `.go` file finds only zero values in error
returns; every real manifest is decoded from bundle bytes that something
outside this repository assembled. And nothing on the Runner side reads a
manifest at all: neither `cmd/runner` nor `internal/runner` imports
`internal/update` (only `cmd/bootstrap/process.go:10` does, which is the
asymmetry `internal/update`'s own source guard enforces). So a running Runner
in this repository can state its `version` and nothing else about itself. A
report field for a coordinate the reporting process cannot obtain could only be
filled by inventing a value at the report site — a self-claim dressed as a
measurement.

Nothing in this surface satisfies any part of section 6's invariant beyond
supplying its input, and nothing here is a release gate.

## 3. Why the heartbeat carries it, and not enrollment

The report rides on the request a Runner already sends:
`POST /v1/runner/heartbeat` gained one additive optional object,
`runner_version`. There is no new endpoint, no new authenticator and no new
idempotency story.

**Enrollment was rejected on a measured argument.** Enrollment happens once per
Runner identity — `internal/runner/protocol.go`'s `IssueEnrollment` is consumed
once — while the binary is switched many times per identity; section 5.2 has
`installed.json` record every switch with its reason. A version captured at
enrollment is therefore **permanently stale after the first self-update**: it
would report the version the machine ran on the day it enrolled, which is
precisely the reading section 6 must not be given.

**The per-Execution requests were rejected for a second measured reason.**
`POST /v1/executions/{id}:start` and `POST /v1/executions/result` only happen
when a machine has work. The mixed-version window of section 6 is exactly the
period in which a machine may be idle, so those paths can fall silent for the
whole interval the invariant is about.

Heartbeat is the only recurring Runner-to-Control-Plane request, already
carries the runner session scheme, already runs through the idempotency path,
and already writes a per-Runner record.

### One consequence, recorded rather than hidden

`requestFingerprint("heartbeat", req)` covers the whole request. Replaying one
`request_id` with a *changed* `runner_version` therefore returns the
idempotency conflict (HTTP 409), and replaying it with an *identical* one
restores the prior response. This is existing semantics for every field of
every request, not new behaviour, and a heartbeat loop already needs a fresh
`request_id` per heartbeat. It is asserted deliberately rather than assumed,
because a client that reused request ids would experience it as a new failure.

The same mechanism has one cross-version effect worth knowing during an
upgrade: adding a field to the request struct changes the fingerprint of an
otherwise identical request, so an idempotency record written by an older
binary and replayed against a newer one is a conflict. That is a mixed-version
consequence of the fingerprint's definition, and it is why a Runner must not
reuse a `request_id` across a restart.

## 4. What is validated: shape, and only shape

The object is **all-present or wholly absent**. Present, it must carry all four
fields:

- `version` matches the same semver shape `internal/update/update.go:64` pins;
- `binary_sha256` is exactly 64 lowercase hex characters, the shape
  `internal/update/update.go:65` pins;
- `schema_min` is at least 1;
- `schema_max` is at least `schema_min`;
- both endpoints are at or below a declared ceiling.

A **partial object is refused with 400 and stores nothing.** This is the
load-bearing refusal. An object carrying `schema_max` with no `schema_min`
would otherwise be stored as an interval starting at zero, which reads as wider
than anything the machine can actually do — and a too-wide interval is the one
error that makes an intersection look non-empty when it is not.

Omitting `runner_version` entirely is a 200 that stores no report and, crucially,
**does not erase a report already stored**. That is why the report lives in its
own `RunnerID`-keyed record rather than as a field on the Runner observation:
that observation is rebuilt in full on every heartbeat, so a version stored
there would be zeroed by the next heartbeat that omitted the object — which
would destroy exactly the distinction section 5 below exists to preserve.

`reported_at` is the transaction's authority time. The request object carries
**no timestamp field at all**, so a Runner clock is structurally unable to
enter the record rather than being filtered out of it. This follows the rule
`internal/application/service.go:450` already states for process observations:
"Runner clocks and caller-provided timestamps are not authoritative."

Nothing here checks that the claim is *true*, and the design says so plainly:
`internal/update.Verify` runs on the Runner machine against bytes the Control
Plane never receives, so no control-plane code can confirm a version. The
honest uses of an unverifiable claim are to report it and to refuse to advance
on it. Permitting is not one of them.

## 5. Three report states, and what stale means

`GET /v1/runners` reports one row per Runner the Control Plane has heard from,
each with a `report_state`:

| `report_state` | Meaning | Coordinates in the row |
| --- | --- | --- |
| `not-reported` | This machine has contacted the Control Plane but has never reported a version. | none at all — no version, no digest, **no interval** |
| `reported` | The newest report is inside the declared staleness window. | all four, echoed exactly, plus `reported_at` |
| `stale` | A report exists but is older than the declared staleness window. | the previously reported coordinates, preserved and marked stale |

`stale` is a first-class state and not a shade of `reported`, because a machine
that reported and then stopped heart-beating is a different operational fact
from one that never reported — and only the first tells an operator that a
machine went quiet. A stale row's coordinates are neither silently refreshed
nor dropped: they are the last thing that machine actually said.

There is **no default interval anywhere.** A row that carries no report carries
no interval field in the JSON at all, rather than `[0,0]` or any other
synthesized value.

## 6. The intersection is reported and gates nothing

The response also carries `intersection_state`:

| `intersection_state` | When |
| --- | --- |
| `unknown` | any enumerated Runner is not in state `reported`; or no Runner is known at all; or the bounded enumeration truncated, so not every machine was seen |
| `empty` | every enumerated Runner reported, and the maximum of the minima exceeds the minimum of the maxima |
| `non-empty` | every enumerated Runner reported and the endpoints overlap; `intersection_schema_min` and `intersection_schema_max` are then the maximum of the minima and the minimum of the maxima |

The endpoints appear **only** when the state is `non-empty`: an empty or
unknown intersection is not an interval and is never given endpoints.

The explicit `unknown` is what makes section 6's refusal sound. "An advance that
would empty the intersection is refused, not scheduled" is a real refusal only
if *I do not know* cannot be silently rounded to *non-empty*. Today, with zero
Runners reporting anything, a design whose default read as compatible would
have been wrong about every machine on the day it shipped.

**The intersection gates nothing, and cannot.** Measured: no canonical schema
counter exists anywhere in this repository. `currentSchema` appears only as a
parameter of `update.Verify` (`internal/update/update.go:127`),
`update.Install` (`:220`) and `update.VerifyInstalled` (`:296`); no constant,
field, record or store owns the value. There is therefore nothing to compare an
intersection against and no operation whose refusal would consume one. This
task introduces no canonical schema counter and no advance operation: the input
to the invariant is what it owes, and the refusal that consumes it belongs to
whoever owns the advance (V2-070's four-stage migration).

A reader must not take a reported `non-empty` intersection as proof that an
advance is safe. The view reports endpoints and a state; it names no advance.

## 7. The report is never authority over what a Runner may read

A Runner's session proves *which* Runner spoke. It proves nothing about whether
the sentence is true. Turning the claim into a read gate would let a merely
mis-built binary — not even a hostile one — widen or narrow its own access by
changing a build constant, and would make the Control Plane's access decision
depend on a value it cannot check.

So: **no authentication, authorization or admission path reads the report, and
no request is ever refused because of a reported version.** Two checks prove
it, and they are checks rather than review notes:

1. **A mechanical scan.** A `go/ast` guard asserts that
   `internal/api/auth.go` — the transport authentication boundary — and
   `internal/application/caller.go` — which declares `CallerFromContext`,
   `callerActor` and `runnerCaller` — name no report type and no report port.
   Both guards fail outright on a zero-declaration scan, and both are verified
   against a known-positive synthetic fixture before the real scan is trusted.
2. **A behavioural test, which matters more.** A scan can be satisfied by
   indirection. So a Runner posts a report whose interval sits at the declared
   ceiling — excluding every plausible canonical schema — and then runs
   `claims:acquire`, `permits:check`, `executions:start`, `checkpoints`,
   `heartbeat` and `executions/result`. All six return 200, with response
   bodies byte-identical to the same scripted sequence run by a Runner that
   reported nothing.

## 8. No Runner in this repository reports anything

**The production report count is zero.**

Measured: `cmd/runner/main.go:27-30` refuses to run without `--fake`, printing
"runner: no external control-plane wiring is enabled"; a search for `net/http`
across `internal/runner` returns nothing. There is no heartbeat client anywhere
in this repository, so nothing populates `runner_version` in production.

V2-069 built and exercised the accept, validate, store and report path through
the API with a runner session, exactly as `internal/api`'s existing runner
tests do. It did not write the Runner-side code that would fill the field; that
is deliberately out of its scope.

Describing this outcome as though production Runners were populating the field
would be the same class of error as the static `schema_version: "v1"` it exists
to replace: a surface that reads as informative while knowing nothing. Until a
heartbeat client exists, `GET /v1/runners` will report `not-reported` for every
machine that contacts the Control Plane, and `intersection_state: unknown` —
which is the true answer.

## 9. Bounds

The enumeration is bounded by a declared constant and has no cursor and no
`page_size`. Machines are not shared (section 5.2), so the row count is the
machine count and not a function of the Requirement count; cursors exist for
collections that grow. When more Runners are known than the bound, `truncated`
is true, `runner_count` is a lower bound, and `intersection_state` is
`unknown`, because the invariant is a statement about *every* machine and a
truncated read did not see every machine.

One `GET /v1/runners` performs no application write and enqueues no outbox
record. The only document it writes is the per-transaction quota reservation
that every bounded owner read already writes.

## 10. Known stale prose elsewhere in the tree

`internal/application/repository.go:577` still explains an unobserved
Repository field with "no Runner registry and no Provider Account aggregate
exist at this commit: RunnerObservationRepository is keyed by a single runner id
with no enumeration port". The second clause is now narrower than it reads: a
bounded enumeration exists, on the version report port, and
`RunnerObservationRepository` itself is deliberately unchanged. That file is
outside this task's allowed paths and was not edited; the correction belongs to
whoever next owns that read model.
