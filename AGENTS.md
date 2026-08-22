# Agentic Loop v2

`docs/principles.md` is the normative source for invariants. Read it before
changing domain, control, runner, release, or security behavior.
All investigation, implementation, validation, and operation must also follow
the policy index at `docs/policies/README.md`.

Routing:

- Product behavior: `docs/product/definition.md` and `docs/product/user-facing-spec.md`
- Architecture and domain: `docs/architecture/`
- Stable user documentation: `docs/stable/`
- Preview behavior and differences: `docs/preview/`
- Machine-readable contracts: `contracts/`
- Work packets and evidence: `.agents/v2/`

The v2 tree is independent of the v1 GitHub Issue/PR loop. Do not restore v1
source or add routing through legacy Issues. Changes must use a dedicated task
worktree and the common `make` validation entrypoint.

Secrets must never enter Git, task records, work packets, evidence summaries,
prompts, or logs. Destructive, irreversible, credential-scope, deployment, or
cost-bearing actions require a recorded preflight before their side effect.
