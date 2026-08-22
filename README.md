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
contracts docs`. `make smoke` starts no external service and checks the local
health and version commands.

The design source of truth is [`docs/principles.md`](docs/principles.md).
