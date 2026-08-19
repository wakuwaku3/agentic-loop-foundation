---
name: postmortem
description: Investigate an incident, near miss, repeated failure, or resource exhaustion; write a blameless analysis into a `postmortem`-labeled Issue; link its action items so the Issue tracks their completion; verify recurrence is prevented once they close. Use when a `postmortem` Issue is claimed, or when an explicit postmortem is requested for an event that does not already have one.
---

# Postmortem

Follow [ポストモーテムポリシー](../../../docs/policies/postmortem.md)
（非難しない原則、起動基準、重大度、分析項目、action item、完了条件の正本）と
[ADR 0026](../../../docs/decisions/0026-postmortem-closed-loop.md)（設計判断）。

## 明示要求（起動基準に当たらない事象を含む）

事象がまだポストモーテムIssueを持たない場合、次で作成する（重複はfingerprintで自動的に抑止される）。

```sh
bin/agentic-loop postmortem create --kind KIND --subject SUBJECT --title TITLE \
  [--summary TEXT] [--evidence-file PATH] [--category category:loop-continuity|category:confidentiality-incident|category:integrity-incident|category:availability-incident]
```

`--evidence-file`の内容は`.agentic-loop/guard-secrets.sh`で走査され、秘密様の内容ならIssue本文へ転記されない（作成自体は続行される）。confidentiality/integrity/availabilityへの実害がある場合だけ対応するincident categoryを指定し、実害のない反復失敗・資源枯渇・near missは既定の`category:loop-continuity`のままにする。

## `postmortem`+`agent:queued` Issueの処理

1. Issue本文（自動検出なら`kind`/`subject`とevidenceの雛形、明示要求なら`--summary`/`--evidence-file`の内容）を起点に、関連するログ・commit・過去のIssue・comment・[loop-metrics](../../../docs/operations/loop-metrics.md)・[trace --audit](../../../docs/operations/traceability.md)を調査する。
2. 事実（観測できたこと）と推測（まだ確認していない仮説）を明確に分けて記録する。個人・利用者・特定のAIプロバイダの評価や非難を書かない。「注意不足だった」で終わらせず、再現可能な仕組みの変更に落とす。
3. Issue本文の各見出し（事象、時系列、影響範囲・検出方法・復旧内容、直接の引き金、構造的原因と寄与要因、なぜ事前検出できなかったか、機能した防御・機能しなかった防御、残余リスク）を埋める。秘密情報・脆弱性の悪用詳細・個人情報・不要なログ本文は書かない。
4. 個別の不具合修正と、他機能にも適用できる一般化された改善（`AGENTS.md`、`docs/policies/`、skill、worker prompt、テスト、観測性への反映）を区別し、それぞれをaction itemとして特定する。既存Issueで対応可能なら既存Issueを使い、無ければ通常の[submit-requirement](../submit-requirement/SKILL.md)のqueue-first intakeで新規Issueを作成する（重複検索を経て`category:*`+`agent:queued`を付与）。
5. 特定したaction item Issueをこのポストモーテムへ紐付ける。

   ```sh
   bin/agentic-loop postmortem link ISSUE ACTION_ISSUE...
   ```

   これはaction itemをnative sub-issue・native dependency（`blocked_by`）として登録し、ポストモーテムIssueを`agent:blocked`へ移す。以後は既存の依存関係機構がaction itemの完了・検証を監視し、すべて完了すると自動的に`agent:queued`へ戻す。この turn にはcommitもPRも無い（分析の記入とGitHub API呼び出しだけ）。それでも通常どおり最終応答を`AGENTIC_LOOP_RESULT=completed`で終えてよい: `postmortem link`が書き込んだ内部マーカーをworker.sh終端処理が読み、この turn を「PRなしで意図的にagent:blockedへ移した」turnとして扱う（merge済みPRを要求する通常の完了経路には回らない）。ポストモーテムIssue自体のGitHub上の状態（`agent:blocked`）は、紐付け操作が既に正しく設定している。

6. action itemが1件も必要ない（記録だけで十分、または既存の対応で十分）と判断した場合は、`postmortem link`を呼ばず、残余リスクを明記したうえで通常どおり完了させてよい（この場合は通常の完了経路が使われ、PRを伴わない完了処理の対象にはならない）。

## 再キュー後（action itemが全て完了・検証された）の処理

`bin/agentic-loop postmortem status ISSUE`で各action itemの完了・検証状態を確認する。すべて完了していることを前提に:

1. 原因と対策が対応しているか、再現fixtureまたは同等の検証手段があるか、回帰防止（テスト・lint・観測性）が入っているかを確認する。
2. 残余リスク（対応しなかった項目がある場合はその理由）をIssue本文へ追記する（雛形の`（記入してください）`をすべて実際の記述に置き換える）。
3. `bin/agentic-loop postmortem complete ISSUE`を実行する。これはaction itemの完了・検証、本文プレースホルダの記入済み、残余リスク節の非空を機械的に確認したうえで完了マーカーを書く。いずれか不成立なら非ゼロで終了し、日本語で理由を返す（Issueはcloseされない）。
4. コマンドが成功したら通常どおり最終応答を`AGENTIC_LOOP_RESULT=completed`で終える。worker.sh終端処理がこの turn にcommitが無いこと（branchがdefault branchより先に進んでいないこと）を確認したうえでIssueをcloseし、専用worktree/branchを削除する。

`postmortem complete`が失敗した場合は、その理由（未完了のaction item、未記入のプレースホルダ、空の残余リスク節）を解消してから再実行するか、追加のaction item Issueを作成し、再度`postmortem link`で紐付ける。
