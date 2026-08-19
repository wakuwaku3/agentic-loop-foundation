# 有限資源とスケーラビリティのworkload budget（[ADR 0025](../decisions/0025-resource-scalability-budget.md)）

workerのplan段は、外部I/O・pagination・探索・retry・並列処理を追加/変更する場合、処理量モデルを`agentic-loop:workload` recordとして自己申告する。`workload_gate`（`bin/lib/agentic-loop/workload.sh`）はこのrecordを検証し、`.agentic-loop.toml`の`queue.workload`設定に応じて続行またはgateする。加えて、共通入口は`bin/agentic-loop workload`による静的検査を実行し、共通境界の迂回や無境界paginationをコードレベルで検出する。

## plan出力のrecord

外部I/Oを追加・変更しない計画は次のように1行で宣言できる。

````
```agentic-loop:workload
{"schema": 1, "issue": 130, "external_io": "none"}
```
````

外部I/Oを追加・変更する計画は、次の形式で処理量モデルを宣言する。

````
```agentic-loop:workload
{
  "schema": 1,
  "issue": 130,
  "external_io": "added",
  "units": [
    {
      "operation": "supervisor 1 poll",
      "per_unit": "REST list 1回",
      "growth": "O(1) in N(queued件数)",
      "stop_condition": "per_page=100 + --paginate、open Issue数で有界",
      "reuse": "open-issue snapshotを全state maintenanceで共有"
    }
  ],
  "amplification": {"idle": "書き込みゼロ", "failure": "retry上限まで", "multi_host": "host毎に独立"},
  "verification": ["tests/test-agentic-loop.sh: queue group T1/T2"],
  "exceptions": []
}
```
````

- `schema`は1、`issue`は対象Issue番号と一致しなければならない。
- `external_io`は`none`/`added`/`changed`の3値。`none`以外は`units`を1件以上、`verification`を1件以上含めなければならない。
- `units`は10件まで。各要素は`operation`・`per_unit`・`growth`・`stop_condition`・`reuse`（すべて200文字以内、改行・backtick禁止）。
- `amplification`は任意だが、含める場合は`idle`/`failure`/`multi_host`のみを許容するobjectで、各値は200文字以内。
- `verification`は10件までの文字列配列（各200文字以内）。呼び出し回数の上限testまたは入力規模を変えたfixtureのtest名を書く。
- `exceptions`は任意の配列で、各要素は`site`（`path:行番号`または`path`）・`reason`・任意の`track`（`#数字`のIssue参照）を持つ。
- record全体は4096byte以内。`.agentic-loop/guard-secrets.sh --text`による秘密走査を通過しないrecordは`secret-like`として拒否し、**Issueへは一切転記しない**。

## verdictとmode

| verdict | 意味 |
| --- | --- |
| `declared` | recordが有効で、`external_io`に応じた必須項目を満たしている。 |
| `not-applicable` | `external_io: "none"`が有効に宣言されている。 |
| `missing` | recordが無い。 |
| `invalid` | recordが検証できない（`schema-invalid`/`record-too-large`/`secret-like`など）。 |

`.agentic-loop.toml`の`queue.workload`（既定`warn`）:

| verdict | `off` | `warn` | `require` |
| --- | --- | --- | --- |
| `declared` / `not-applicable` | 続行（評価しない） | 続行（`declared`は監査commentを1件残す） | 続行 |
| `missing` / `invalid` | 続行 | 助言commentのみ・続行 | **`agent:needs-input`へgate**（`reason=workload-missing`/`workload-invalid`） |

このrepositoryは`warn`を出荷する。導入直後に既存のqueueを自己lockさせないためで、`require`へ切り替える場合は、plan段が一貫してrecordを出すことを監査commentで数件確認してから`.agentic-loop.toml`の`workload = "require"`へ変更する。gateは追加のGitHub API呼び出しを発生させない（recordはplan出力ファイルから読み、gate発火時のみ既存の`comment_post`で1件投稿する）。

## 実装中の再宣言

execは`WORKLOAD_VERDICT`を実装prompt経由で受け取る。実装中に宣言した処理量モデル・停止条件を超える外部呼び出しが必要になった場合、workerはplanへ戻して再宣言し、宣言なしに全件取得・無制限pagination・無制限retryを追加してはならない。

## 注釈文法（静的検査`workload_scan`が参照する）

`bin/agentic-loop workload`は次の3種類の違反を検出する。対象は`bin/agentic-loop`・`bin/lib/agentic-loop/*.sh`・`bin/agentic-loop-diagnose`・`.agentic-loop/*.sh`。

### W1: 共通境界（`repo_api`）の迂回

`bin/lib/agentic-loop/api.sh`以外で`gh api`を直接呼ぶ行は、直前の非空行に次の注釈が無ければ違反。

```sh
# workload-boundary: 理由（例: GraphQLはbest-effortのProjects操作、非GitHub hostのusage API等）
```

### W2: 無境界pagination

`--paginate`を含む行が、同一行に境界filter（`-f labels=`/`-f since=`/`-f sha=`/`-f head=`のいずれか）を持たない場合、直前の非空行に次の注釈が無ければ違反。

```sh
# workload-unbounded: 理由 bound=上限の根拠 [track=#N]
```

`track=#N`は、既存実装として発見したが本Issueではその場で最適化しない無境界I/Oを、別の追跡Issueへ切り出したことを示す（例: `track=#197`）。

### W3: 集約の退行

loop本体（`while`/`for`の内側）でIssue一覧の再取得を行う行を検出する。「N件の入力に対して同じ一覧をN回取得する」退行を防ぐ。`snapshot_state_rows`（1 pollあたり1回のopen Issueスナップショットを全state maintenanceで共有する集約取得）を経由する既存経路は該当しない。

## 発見時の追跡導線

`bin/agentic-loop workload`は`track=#N`注釈の棚卸しも一覧表示する。定期診断（`diagnose-codebase`）や実装中に新たな無境界I/Oや同一データの重複取得を発見した場合、その場で全面最適化せず、`track=#N`注釈を付けたうえで別のGitHub Issueとして起票し、[submit-requirement](../../.agents/skills/submit-requirement/SKILL.md)のqueue-first intakeへ載せる。

## testの書き方

`tests/test-agentic-loop.sh`は、fake ghの呼び出しlog（`$FAKE_GH_ROOT/calls`）に対して`calls_before`からのdelta抽出パターンを使い、次を検証する。

- **呼び出し回数の上限**: 1 pollまたは1操作あたりのAPI呼び出し回数が、期待する定数を超えないこと。
- **入力規模に対する増加率**: 同一条件で入力規模Nを変えた複数のfixture（例: 2件と8件のqueued Issue）を用意し、呼び出し回数がNに対して期待した増加率（多くの場合O(1)）であることを確認する。
- **静的検査の検出力**: 注釈の無い違反を一時fixtureへ書き込み、`bin/agentic-loop workload`がそれぞれを検出して終了code 1を返すこと、注釈を付ければ0を返すことを確認する。

## 費用・秘密

`workload_gate`と`workload_scan`はローカルファイルだけを読み、追加のGitHub API呼び出しはゼロ。gate発火時のみ1件のcomment投稿が発生する。recordは秘密走査・サイズ上限・改行/backtick禁止のフィールドを通過したものだけがcommentに転記され、invalidなrecordの本体は一切転記しない。
