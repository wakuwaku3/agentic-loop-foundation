# Release contract instances

`foundation.json` is the Foundation Repository's Release Contract, an instance
of `contracts/schemas/release-contract.json`. `foundation-capabilities.json`
is a sibling instance of `contracts/schemas/capability-declaration-set.json`
that declares, for the same capability ids in the same order, the fields
`docs/architecture/documentation.md` section 5 requires. The two files are
joined by `contract_ref` and a shared `release` string; they are validated and
cross-checked together, they are never merged into one document.

A `release` value ending in `-baseline` (for example `0.1.0-baseline`) marks a
release that has not been promoted from Preview: every capability in that
release is `status: "preview"`. See
[docs/architecture/release-contract.md](../../docs/architecture/release-contract.md)
for what the fields mean and what promotion requires; this file does not
restate it.

`provider_verification` は固定の代表Providerを持たない。Providerへの直接変更が
なければ宣言Providerのどれか1つ以上、直接変更があれば影響Providerすべての
実確認を要求する。利用可能Provider一覧に含まれることは、毎releaseでの契約を
要求する意味ではない。
