# 変更影響とリスクのpreflight（[ADR 0020](../decisions/0020-change-risk-preflight.md)）

workerのplan段は、実装に着手する前に、変更影響とリスクを10軸固定のpreflight recordとして自己申告する。`preflight_gate`（`bin/lib/agentic-loop/preflight.sh`）はこのrecordを単独では信用せず、`.agentic-loop/capabilities.toml`とIssueの変更scopeから機械的に導出した`signal`と照合し、破壊的・不可逆・重大costまたはsecurity上のリスクだけを`agent:needs-input`へgateする。

## plan出力のrecord

plan段の最終メッセージ（scope markerより前）に、fenced code blockとして次の形式のJSONを1つだけ書く。

````
```agentic-loop:preflight
{
  "schema": 1,
  "issue": 58,
  "risks": [
    {"axis": "security", "level": "low"},
    {"axis": "confidentiality", "level": "low"},
    {"axis": "integrity", "level": "medium", "reason": "既存configの読み込み経路を変更する"},
    {"axis": "availability", "level": "low"},
    {"axis": "data_migration", "level": "low"},
    {"axis": "external_environment", "level": "low"},
    {"axis": "cost", "level": "low"},
    {"axis": "compatibility", "level": "low"},
    {"axis": "release_deploy", "level": "low"},
    {"axis": "rollback", "level": "low"}
  ],
  "change": {
    "scope": "paths=bin/lib/agentic-loop/preflight.sh",
    "tests": ["devbox run --pure check"],
    "external_operations": [],
    "rollback": "PRのrevertで完全復旧（永続状態なし）"
  },
  "approval": {"required": false, "triggers": []}
}
```
````

- `risks`は次の10軸を**過不足なく1回ずつ**含めなければならない: `security`（セキュリティ）、`confidentiality`（機密性）、`integrity`（完全性）、`availability`（可用性）、`data_migration`（データ移行）、`external_environment`（外部環境操作）、`cost`（費用）、`compatibility`（互換性）、`release_deploy`（release/deploy）、`rollback`（rollback困難性）。1軸でも欠落・重複・未知の軸名があれば`schema-invalid`。
- `level`は`low`/`medium`/`high`/`unknown`の4値。
  - `low`以外は`reason`が必須（200文字以内、改行・backtick禁止）。
  - `unknown`（判定に必要な情報が無く、判定できない）は追加で`missing`（不足している情報。200文字以内）が必須。`unknown`を`low`として扱う経路は存在しない。
- `change.scope`/`change.rollback`は200文字以内。`change.tests`/`change.external_operations`は各10件までの文字列配列（各要素200文字以内）。いずれも改行・backtick禁止。
- `approval.required`は真偽値。`approval.triggers`は次のenumの配列: `destructive`（破壊的）、`irreversible`（不可逆）、`cost`（重大費用）、`security`（重大security判断）、`permission`（権限変更）、`external-deploy`（外部deploy）、`data-migration`（データ移行）、`rollback-blocked`（rollback不能）。
- いずれかの軸が`high`、またはtriggerが1件以上あるのに`approval.required=false`のrecordは`claim-mismatch`として拒否する（自己矛盾する自己申告を素通りさせない）。
- record全体は4096byte以内。`.agentic-loop/guard-secrets.sh --text`による秘密走査を通過しないrecordは`secret-like`として拒否し、**Issueへは一切転記しない**。

## signal（recordを単独で信用しないための照合対象）

`preflight_signal_class`は、追加のGitHub API呼び出しをせず、次の2つだけから`signal`を導出する。

1. `.agentic-loop/capabilities.toml`の`[[protected]]`（`change_requires = "approval"`のpathが、Issueの変更scopeと重なる、または変更scopeが`paths=*`）→ `approval`
2. 上記に該当せず、`[[external_environment]].name`のいずれかがIssueの変更scopeの`env=`宣言と一致する → `medium`
3. どちらにも該当しない → `none`

recordが`low`/`medium`のみを自己申告していても、signalが`approval`ならverdictは`signal-mismatch`になる。recordが全く無い場合も、signalが`approval`なら同様に`signal-mismatch`になる（record不在はgate回避の抜け道にならない）。

## verdictとmode

| verdict | 意味 |
| --- | --- |
| `autonomous` | 全軸`low`/`medium`、triggerなし、signalとも矛盾しない。 |
| `approval-required` | いずれかの軸が`high`、または`approval.triggers`に1件以上ある。 |
| `undetermined` | いずれかの軸が`unknown`（根拠と不足情報がcommentに残る）。 |
| `signal-mismatch` | recordまたはrecord不在が、capabilities.tomlとscopeから導いたsignalと矛盾する。 |
| `invalid` | record欠落以外の理由で検証できない（`schema-invalid`/`record-too-large`/`secret-like`/`guard-unavailable`/`claim-mismatch`）。 |
| `missing` | recordが無く、signalも`approval`を示さない。 |

`.agentic-loop.toml`の`queue.preflight`（既定`warn`）:

| verdict | `off` | `warn` | `require` |
| --- | --- | --- | --- |
| `autonomous` | 続行（評価しない） | 続行（監査commentを1件残す） | 続行 |
| `approval-required` / `undetermined` / `signal-mismatch` | 続行 | **`agent:needs-input`へgate** | **`agent:needs-input`へgate** |
| `missing` | 続行 | 助言commentのみ・続行 | **gate** |
| `invalid` | 続行 | 助言commentのみ・続行（record本体は転記しない） | **gate** |

このrepositoryは`warn`を出荷する。導入直後に既存のqueueを自己lockさせないためで、`require`へ切り替える場合は、plan段が一貫してrecordを出すことを`agentic-loop:preflight`監査commentで数件確認してから`.agentic-loop.toml`の`preflight = "require"`へ変更する。

## `agent:needs-input`と承認

gate時のcommentは`<!-- agentic-loop:needs-input worker=preflight reason=<reason> token=<12桁16進> -->`を含み（`worker_state.sh`の`requeue_answered`が既存どおり検出できる）、各軸の水準・根拠・不足情報・対象scope・必要test・外部操作・rollback案を人間可読に併記する（秘密・攻撃手順は含めない。recordがinvalidな場合は本体を一切転記しない）。`reason`は`preflight-approval`（approval-required）/`preflight-undetermined`/`preflight-signal-mismatch`/`preflight-missing`/`preflight-invalid`/`preflight-escalation`（後述）のいずれか。

承認は自由記述のIssue返信では成立しない。認可済みの運用者（repository write/maintain/admin権限）が次を実行する。

```sh
bin/agentic-loop preflight ISSUE --approve --token TOKEN [--note TEXT]
```

これは`<!-- agentic-loop:preflight-approved schema=1 actor=... token=... at=... -->`markerを投稿し、Issueが`agent:needs-input`であれば`agent:queued`へ戻す。tokenは「`low`以外の軸+水準」と`approval.triggers`の集合から導く安定な12桁16進値で、再planでrecordの文言が変わっても宣言リスクが同じなら失効しない。record不在でsignalだけがgateした場合はsignalの理由から同様のtokenを計算する。

読み取り専用の確認:

```sh
bin/agentic-loop preflight ISSUE [--format json]
```

現在の変更scopeとsignalを表示する（signalが`approval`なら終了code 1）。GitHubへの書き込みは行わない。

## 実装中のscope・リスク増大の再評価

execは1 turnで実装からPRのmergeまでを完遂するため（[worker resume](issue-queue.md#中断からの再開)を参照）、`preflight_reevaluate_diff`はmerge前にexecそのものを止めることはできない。その代わり、**完了の確定（cleanup・`agent:completed`遷移・close）の直前**（通常のexec完了経路と、`pr-merged`resume経路の両方）で、実測したdiffから`signal`を再評価する。承認されていない`approval`signalを検出した場合、Issueはcloseせず、worktree・branch・PRを保持したまま`agent:needs-input`（`reason=preflight-escalation`）へ移す。承認後、`agent:queued`を再度付与すると、次のworkerがこの再評価を通過して完了処理を続行する。

## `category`・`needs-input`・検証ハーネスポリシー・継続的デリバリーポリシーとの関係

- **`category:*`Label**（要求の種別）とpreflightのリスク軸は独立である。`category:improvement`の変更でも`release_deploy`や`security`がhighになりうるし、逆に`category:confidentiality-incident`のような重大Labelでも実際の変更が軽微な文書修正ならリスクは低い。preflightはincidentの詳細やLabelの重大性そのものを転記しない。
- **`agent:needs-input`**は既存のIssue状態（人間の判断を待つ）を再利用する。preflight専用の新しいLabelは無い。`requeue_answered`（自由記述の返信での再queue）と`bin/agentic-loop preflight --approve`（承認そのもの）は別の入口であり、返信だけではrecordは承認されない。
- **[検証ハーネスポリシー](../policies/validation-harness.md)**: `change.tests`は共通入口（`devbox run --pure check`）を正本として記述する。preflight自体は指定されたtestを実行しない。実行するのはあくまでexec段のAI CLIである。
- **[継続的デリバリーポリシー](../policies/continuous-delivery.md)**: `release_deploy`軸は、CD pipelineが実際にdeployを行うか（`.agentic-loop/capabilities.toml`の`release.deploy`/`binary_release`）を踏まえて自己申告する。deployを伴う変更は`release_deploy`をhighにするか対応するtrigger（`external-deploy`）を含めるべきで、CD gate自体の設定はpreflightの対象外である。

## 費用・秘密

`preflight_signal_class`はローカルファイルだけを読み、追加のGitHub API呼び出しはゼロ。gateが発火した場合のみ、承認marker検索（REST(core) 1回、paginated）とcomment投稿（1回）が発生する。GraphQLは使わない。recordは秘密走査・サイズ上限・改行/backtick禁止のreason/missingを通過したものだけがcommentに転記され、invalidなrecordの本体は一切転記しない。
