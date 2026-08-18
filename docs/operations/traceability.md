# 要求・変更・検証のトレーサビリティ（[ADR 0016](../decisions/0016-requirement-traceability.md)）

`bin/agentic-loop trace` と、workerの完了確定に組み込まれた`trace_gate`は、Issueの受け入れ条件が実際にどの変更・どの検証で満たされたかを、PR本文の自己申告とGitHubの再観測から記録・確認する。

## PR本文のrecord

PR本文に、fenced code blockとして次の形式のJSONを1つだけ書く。

````
```agentic-loop:traceability
{
  "schema": 1,
  "issue": 53,
  "criteria": [
    {
      "id": "ac-1a2b3c4d",
      "source": "issue-body",
      "status": "satisfied",
      "verification": "automated",
      "changes": [{"path": "bin/lib/agentic-loop/trace.sh", "anchor": "trace_gate"}],
      "checks": [{"name": "check", "result": "success"}]
    }
  ]
}
```
````

トップレベルは`schema`(=1固定)・`issue`（対象Issue番号、数値）・`criteria`(配列、1件以上)の3keyのみ。`criteria[]`の各要素:

| key | 必須 | 値 |
| --- | --- | --- |
| `id` | 必須 | `ac-`+8桁16進。Issue本文の受け入れ条件を`trace_normalize_criterion`で正規化した文字列のsha256先頭8桁。手で計算する必要はなく、`bin/agentic-loop trace ISSUE`が現在Issueから導出できるid一覧を確認できる。 |
| `source` | 必須 | `issue-body`（Issue本文の受け入れ条件から）または`plan`（Issueにはないがplan段階で追加した条件から）。 |
| `status` | 必須 | `satisfied`(充足) / `partial`(部分充足) / `unmet`(未充足) / `not-applicable`(対象外) / `superseded`(条件変更で置き換え)。 |
| `verification` | 省略可(既定`none`) | `automated`(CIなどの自動検証) / `manual`(人手での確認) / `external`(外部環境での確認、CIには現れない) / `none`。 |
| `changes` | 省略可 | `[{"path": "...", "anchor": "..."}]`。`path`はこのPRが実際に変更したファイルのpath（`anchor`は関数名・見出しなど任意の補足）。 |
| `checks` | 省略可 | `[{"name": "...", "result": "success\|failure\|skipped"}]`。`name`にPRのcheck-run名、または共有entry point全体を指す特別な名前`check`（`devbox run --pure check`/CIそのもの）を書く。 |
| `reason` | `status`が`satisfied`以外なら必須 | 300文字以内、改行・backtick禁止。なぜその状態か（部分実装の残作業、対象外の理由など）。 |
| `superseded_by` | `status: superseded`なら必須 | 置き換え先の新しい`id`。 |

## `status`と`verification`の使い分け

- ある条件が**コードで実装され、CIで検証できる**場合: `status: satisfied`, `verification: automated`, `checks`にCIのcheck名を書く。
- ある条件が**実装されたが自動検証手段がない**（UIの見た目、手動確認した外部サービスの挙動など）場合: `status: satisfied`, `verification: manual`または`external`, `reason`に何を確認したかを短く書く。`checks`は空でよい。
- ある条件が**この変更の対象外**（既に別Issueで対応済み、要求自体が誤りだったなど）の場合: `status: not-applicable`, `reason`必須。
- ある条件が**部分的にしか対応できていない**場合: `status: partial`, `reason`に残作業を書く。`trace_gate`はこの状態でも`criteria-missing`にはならないが、Issueの受け入れ条件をすべて`satisfied`/`not-applicable`にしないまま完了させたい場合は、その判断自体を`reason`に残す。

## 条件変更の検出（`superseded`の書き方）

Issue本文の受け入れ条件の文言を変更すると、そのcriterionの識別子（`ac-`+文言のsha256先頭8桁）が変わる。この新しい識別子をrecordが含まなければ`criteria-missing`になる。旧条件を書き換えたのではなく置き換えた場合は、両方をrecordに書く。

```json
{"id": "ac-旧id", "source": "issue-body", "status": "superseded", "superseded_by": "ac-新id", "reason": "受け入れ条件の文言変更により置き換え"},
{"id": "ac-新id", "source": "issue-body", "status": "satisfied", "verification": "automated", "checks": [{"name": "check", "result": "success"}]}
```

新しいidが何になるかは、変更後のIssue本文に対して`bin/agentic-loop trace ISSUE`を実行すれば確認できる（未mergeの間はPRが見つからず`no-pr`になるため、正確な確認は`trace_derive_criteria`のロジックに沿って自分で正規化するか、Issueの現在の受け入れ条件をworkerのexecプロンプトに含めて確認する）。

## `trace_gate`の3 mode

`.agentic-loop.toml`の`queue.traceability`で選択する。既定は配布物では`warn`（Foundation自身のcode側fallbackは`off`。導入直後に無警告で挙動が変わらないための安全策。詳細は[ADR 0016](../decisions/0016-requirement-traceability.md)）。

| mode | record不在・検証失敗時の動作 |
| --- | --- |
| `require` | 完了処理をせず、Issueを`agent:failed`にする。**closeしない。worktree/local branchも保持する**。PR本文を修正後、`agent:queued`を再付与すると再試行できる。 |
| `warn` | 完了処理は継続する（Issueをclose）。失敗理由を含む助言commentを1件投稿する。 |
| `off` | 評価しない。追加のGitHub API呼び出しは発生しない。 |

## 失敗理由コード（`TRACE_INVALID_REASON`）

| コード | 意味 |
| --- | --- |
| `missing-record` | PR本文に`agentic-loop:traceability`のfenced blockが無い。 |
| `record-too-large` | recordが8192byteを超えている。 |
| `secret-like` | recordが`.agentic-loop/guard-secrets.sh`の秘密パターンに一致した。 |
| `guard-unavailable` | `.agentic-loop/guard-secrets.sh`が実行できない（fail-closedで拒否。「スキャンできないので通す」は選ばない）。 |
| `schema-invalid` | トップレベルkeyや各criterionの形状・enum値が仕様と一致しない。 |
| `criteria-missing` | Issue本文から導出できる識別子のうち、recordに含まれないものがある（条件変更の未対応を含む）。 |
| `evidence-mismatch` | recordが主張する`checks[].result`または`changes[].path`が、GitHubの観測結果（PRのhead commitのcheck-runs、PRの変更ファイル一覧）と一致しない。 |

## `bin/agentic-loop trace`（読み取り専用CLI）

```sh
bin/agentic-loop trace ISSUE [--format json]
bin/agentic-loop trace --audit [--days N] [--format json]
```

- `trace ISSUE`: そのIssueに対応するPR（mergeされたものがあればそれを、なければ最新のopen PR）を探し、recordを評価して結果を表示する。対応するPRが無ければ`reason: no-pr`で終了code 1。record評価がfailなら終了code 1。
- `trace --audit`（既定30日）: リポジトリ全体のverdict commentを1回のpaginated読み取りで走査し、次のいずれかに該当するIssueだけを列挙する。常に終了code 0（自己診断であり合否判定ではない）。
  - `unmet`（未充足の条件がある）
  - `partial`（部分充足の条件がある）
  - `unreferenced_paths`（PRの変更ファイルのうち、どの受け入れ条件からも参照されていないものがある＝**実装はあるが要求が無い**方向の不整合）
  - `checks`が`success`以外
- いずれも`--format json`で機械可読な1行JSONを返す。GitHubへの書き込みは一切行わない。

`trace --audit`が拾う`unreferenced_paths>0`は、`diagnose-codebase`の要求・実装の双方向比較（[SKILL.md](../../.agents/skills/diagnose-codebase/SKILL.md)）の証拠源としても使う。

## verdict comment

`trace_gate`が評価に成功した場合、Issueへ1つのcommentをupsertする（同一Issueに複数枚できないよう、ローカルの`$STATE_ROOT/workers/<issue>.trace`にcomment idを保存し、無ければ既存commentを1ページ検索してから新規作成する。複数ホストでの再開にも対応する）。

```
<!-- agentic-loop:traceability schema=1 issue=53 pr=200 merge_commit=<sha> base=main checks=success criteria=3 satisfied=2 partial=0 not-applicable=1 unmet=0 superseded=0 manual=0 external=0 unreferenced_paths=0 verdict=pass -->
### トレーサビリティ検証結果

| 条件ID | 状態 | 検証方法 | 対象path | 理由 |
|---|---|---|---|---|
| ac-1a2b3c4d | 充足 | 自動 | bin/lib/agentic-loop/trace.sh | — |
```

markerは1行（`metrics_field`など既存の`agentic-loop:*`marker解析と同じ前提）。

## 費用・秘密

`off`では追加のREST呼び出しはゼロ。`warn`/`require`ではIssue完了1件あたり最大4回のREST(core)呼び出し（PR本文取得・check-runs取得・変更ファイル一覧取得・verdict commentのGET/POST/PATCH）が増える。GraphQLは使わない。recordは秘密走査・サイズ上限・改行禁止のreasonを通過したものだけがverdict commentに転記され、生のlog・prompt全文は要求も保存もしない。
