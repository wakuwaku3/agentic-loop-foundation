# Local release core

`internal/release` is the provider-neutral M5 core. `CompileContract` hashes the
versioned release contract and documentation bytes; a `Bundle` stores cloned
candidate state so callers cannot mutate an immutable candidate after `Put`.

Promotion is a pure gate over capability evidence. Every declared capability
must bind candidate digest, bundle/contract/docs digests, provider, target,
freshness, and verification, and rollback/resume evidence is mandatory. The
gate has no routing side effect. `Router` changes preview/stable only after
the gate succeeds and retains the previous stable digest for rollback.

The local journey tests missing capabilities, stale evidence, documentation
drift, immutable conflicts, preview promotion, and rollback. Production
storage and deploy adapters remain outside this package's persistence port.
