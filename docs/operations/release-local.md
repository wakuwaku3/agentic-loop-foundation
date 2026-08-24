# Local release core

`internal/release` is the provider-neutral M5 core. `CompileContract` decodes the
canonical Release Contract surface (`id`, `kind`, `created_at`, `correlation_id`,
`release`, per-capability `name`/`status`/`evidence_ids`, `verification`, and
`rollback{procedure,target}`) with `DisallowUnknownFields`, so a schema-valid
contract compiles while any drifted or invented field is rejected. It also
refuses a contract that declares a capability `status: "stable"` with no
`evidence_ids`, then hashes the contract and documentation bytes; a `Bundle`
stores cloned candidate state so callers cannot mutate an immutable candidate
after `Put`.

Promotion is a pure gate over capability evidence. Every declared capability
must bind candidate digest, bundle/contract/docs digests, provider, target,
freshness, and verification, and rollback/resume evidence is mandatory. The
gate has no routing side effect. `Router` changes preview/stable only after
the gate succeeds and retains the previous stable digest for rollback.

The local journey tests missing capabilities, stale evidence, documentation
drift, immutable conflicts, preview promotion, and rollback. Production
storage and deploy adapters remain outside this package's persistence port.
