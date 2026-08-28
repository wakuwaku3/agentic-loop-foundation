# Agentic Loop v2

Agentic Loop is a self-hostable control plane and exchangeable runner for
continuously solving application requirements. This branch is the white-slate
v2 foundation; it intentionally does not contain the v1 implementation.

## Development

Use the pinned Devbox environment, then run:

```sh
make check
```

Individual read-only gates are available as `make environment format lint test
contracts docs`. `make smoke` starts no external service and checks the three
binary version entrypoints; HTTP health behavior is covered at the API boundary.

## Product status

The `v2` branch is not Stable or a replacement for `main`. It currently has the
self-hosted Control Plane/owner UI, Firestore adapter, enrolled local Runner,
lease fencing and recovery, verified stop controls, selective CI, cost-gated GCP
IaC, local release gates, provider-neutral Codex/Claude/opencode contracts, and
signed side-by-side Runner update primitives. Real Preview deployment and every
affected Provider exercise remain mandatory before Stable promotion.

Start with [the product definition](docs/product/definition.md) and [the current
user-facing specification](docs/product/user-facing-spec.md). Operators should use
the runbooks under `docs/operations/`; neither GitHub Issues nor pull requests
are the v2 product queue.

The design source of truth is [`docs/principles.md`](docs/principles.md).
