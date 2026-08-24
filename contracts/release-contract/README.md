# Release contract instances

`foundation.json` is the Foundation Repository's Release Contract, an instance
of `contracts/schemas/release-contract.json`. `foundation-capabilities.json`
is a sibling instance of `contracts/schemas/capability-declaration-set.json`
that declares, for the same capability ids in the same order, the fields
`docs/architecture/documentation.md` section 5 requires. The two files are
joined by `contract_ref` and a shared `release` string; they are validated and
cross-checked together, they are never merged into one document.

A `release` value ending in `-baseline` (for example `0.1.0-baseline`) marks a
release that has not been exercised in Preview: every capability in that
release is `status: "preview"` with an empty `evidence_ids`. See
[docs/architecture/release-contract.md](../../docs/architecture/release-contract.md)
for what the fields mean and what promotion requires; this file does not
restate it.
