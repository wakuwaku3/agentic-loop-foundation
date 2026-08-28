# Provider adapters

Codex、Claude、OpenCode は `internal/provider` の交換可能な adapter として扱う。
adapter は bounded な Work Packet から固定 argv と stdin を構築し、provider 固有の
JSON/JSONL を共通 result に投影する。raw response、認証値、prompt は永続化しない。

実行時には `cmd/runner --real --provider <name>` が現在の session から CLI の絶対
path を解決し、memory 上の invocation policy と data root 配下の外部 ledger を
`SupervisedInvocationRunner` に渡す。repository 内の preflight、handoff、evidence
file は使わない。argv[0] の basename、workspace、environment allowlist、invocation
上限は process 起動前に検証され、ledger の予約後にだけ CLI を起動する。

Codex は `--json --skip-git-repo-check -C <workspace> exec -`、OpenCode は
`run --format json --dir <workspace> -` を使う。Claude は GCP/Claude の実環境確認が
可能になるまで Stable 判定の対象外とする。CLI version の宣言区間は
`internal/provider` にあり、実際の互換性は直接 live exercise で確認する。

2026-08-28 に Codex 0.149.1 と OpenCode 1.18.18 を実 CLI で起動し、両方で成功、
provider 発行 session ID、usage、ledger settlement を確認した。fixture/fake はこの
live 判定の代替にしない。
