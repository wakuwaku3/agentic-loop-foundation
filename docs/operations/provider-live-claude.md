# claude CLI 実 Provider vertical slice（V2-017）

このドキュメントは、実 `claude` CLI を経由する live test suite の運用手順を記す。対象は `internal/runner/provider_live_test.go` の `TestProviderLiveVerticalSlice` である。

## live suite の実行方法

`AGENTIC_LOOP_LIVE_PROVIDER=1` を設定し、かつ `devbox run --pure` が環境変数を落とすため `-e` で明示的に渡す。`go` / `gcc` を解決できる devbox 由来の PATH に `claude` の実体（`/home/takushi/.local/bin`）を加えて渡すこと。

```sh
LIVE_PATH="$(devbox run --pure -- env | grep '^PATH=' | cut -d= -f2-):/home/takushi/.local/bin:/usr/bin:/bin"
devbox run --pure \
  -e AGENTIC_LOOP_LIVE_PROVIDER=1 \
  -e HOME=/home/takushi \
  -e PATH="$LIVE_PATH" \
  -- go test -count=1 -v -timeout 600s -run TestProviderLiveVerticalSlice ./internal/runner
```

live test は次の3条件をすべて満たさない限り必ず skip する（`t.Logf` でどの条件が失敗したかを記録する）:

1. `AGENTIC_LOOP_LIVE_PROVIDER=1`
2. `.agents/v2/provider-preflight/V2-017-provider-live-claude.json` が `contracts/schemas/provider-preflight.json` に対して valid、かつ `internal/contracts.CheckProviderPreflightLedger` のリポジトリ全体整合性チェックを通過する
3. 承認済み record の `limits.ledger_path` が書き込み可能である

`devbox run --pure -- go test ./...`（＝ `make check` の一部）はこの3条件のいずれも満たさないため、live invocation は常に 0 件である。

## ledger の場所と読み方

ledger は承認済み record の `limits.ledger_path` にあり、本タスクでは絶対パス:

```
/home/takushi/.local/state/agentic-loop/v2/V2-017-provider-cost.json
```

リポジトリの外、ワークスペースの外にあり、0600 のファイルが 0700 のディレクトリに置かれる。中身は `{schema_version, provider, task_id, preflight_digest, limits, halted, entries[]}` で、各 `entries[]` は `{sequence, purpose, state("reserved"|"settled"), reserved_usd, actual_usd, session_id, input_count, output_count, cache_read_count, cache_creation_count, duration_api_ms, duration_ms, num_turns, started_at, finished_at}` を持つ。`cat` で直接読める（`jq` があれば整形できる）。prompt・応答本文は一切含まれない。

## halted フラグの意味と解除できる者

`halted:true` は「単発の invocation の実測 `actual_usd` が 2.00 USD を超えた」ことを示す暴走検知シグナルである。billing 上の失敗ではなく、context 無限成長などの異常を疑うべき停止信号。

- `halted:true` になった後は、`internal/runner.CostLedger.Reserve` がどんな残余バジェットがあっても必ず失敗する（fail-closed）。
- **解除できるのはリポジトリの owner のみ**であり、ledger ファイルを手で編集して `halted` を書き換えることは禁止する（それは異常の証拠を消すことになる）。owner は、新しい調査を経てから、新しい `provider-preflight` record を発行し、必要なら新しい `ledger_path` を割り当てて再開させる。

## capacity exhaustion（上限到達）時の対応

`max_invocations`（16）または `max_total_cost_usd`（10.00 USD）に到達した場合も fail-closed で停止する。これは「暴走検知のための点検停止」であり、成功でも失敗でもない。

- 上限をその場で引き上げてはならない（record の `limits` を書き換えない）。
- 同じ ledger を初期化し直して回数をリセットしてはならない。
- 対応は owner に新しい `provider-preflight` record（新しい `id`、必要なら新しい `ledger_path`）の発行を依頼することである。

## 再承認の手順（work order 編集時）

`approval.subject_path`/`subject_digest` は `.agents/v2/packets/V2-017-work-order.json` の sha256 に固定的束縛されている。Work Order を1バイトでも編集すると、その時点で `subject_digest` は一致しなくなり、`internal/contracts.CheckProviderPreflightLedger` も `internal/runner.LoadPreflightRecord` も失敗するようになる。**これは意図的な設計である。**

- 対応: **`subject_digest` を新しい値に書き換えて辻褄を合わせてはならない。**
- 正しい対応: owner に新しい work order の sha256 に対する承認を依頼し、新しい `approval` block を持つ record を（必要なら新しい `id` で）発行する。

## 台帳が守る範囲の境界（記録されるのは Loop 自身の実行経路だけ）

台帳（`internal/runner.CostLedger`）が entries を残せるのは、invocation が `SupervisedInvocationRunner`（＝ Loop 自身の実行経路）を通り、`CostLedger.Reserve` を経由した場合に限る。この境界の外で何が起きても、台帳はそれを一切観測しない。

- **operator や agent が shell から直接 `claude` を手で実行した分は、台帳には一切現れない。** 台帳の `entries[]` の合計が表しているのは「Loop がこの ledger を通じて使った量」であり、「この machine で `claude` を使った総量」ではない。両者を同一視してはならない。
- したがって、subscription の枠の実際の残量を知りたい場合、この台帳を見ても分からない。台帳はそのための代替にはならず、Provider 側の使用量表示を直接確認する必要がある。
- `halted` フラグや `max_invocations` / `max_total_cost_usd` といった暴走検知のしきい値も、同じ境界の内側でしか機能しない。Loop の実行経路を通らずに動くループ（人や別プロセスが `claude` を直接繰り返し呼ぶ場合など）を、この台帳は検知することも止めることもできない。
- 具体例: V2-017 の実装時、ハーネス配線前の検証として bash 経由で `claude` CLI に対し2回（HOME/PATH 最小構成の確認、各 約$0.08相当）、加えて到達不能 URL への挙動確認を4回（いずれも API 到達前に hang したため $0）、計6回の invocation が実行されたことが実装者本人により開示されている。これらは `CostLedger.Reserve` を経由していないため、**台帳には一切記録されていない**。台帳に残っているのは、ハーネス配線後に `CostLedger` 経由で実行された8 invocation・11 entry のみである。
