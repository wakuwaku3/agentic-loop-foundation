# ポストモーテムの運用（[ADR 0026](../decisions/0026-postmortem-closed-loop.md)）

正本は[ポストモーテムポリシー](../policies/postmortem.md)（目的・非難しない原則・起動基準・重大度・分析項目・action item・完了条件）。この文書はCLI詳細・設定・検証手順・トラブルシュートを扱う。手順の実務は[postmortem skill](../../.agents/skills/postmortem/SKILL.md)を参照。

## CLI

```sh
bin/agentic-loop postmortem create --kind KIND --subject SUBJECT --title TITLE \
  [--summary TEXT] [--evidence-file PATH] [--category category:loop-continuity|category:confidentiality-incident|category:integrity-incident|category:availability-incident]
bin/agentic-loop postmortem link ISSUE ACTION_ISSUE...
bin/agentic-loop postmortem status ISSUE
bin/agentic-loop postmortem complete ISSUE
```

- `create`: 事象を新規のポストモーテムIssueとして作成する（利用者・workerどちらも呼べる明示要求）。同一`kind`+`subject`のfingerprintに一致する**open**なポストモーテムが既に存在する場合は新規作成せず、既存Issueへ再発commentを追記して既存Issue番号を返す。`--evidence-file`の内容は`.agentic-loop/guard-secrets.sh`でfail-closedに走査され、秘密様なら本文へ転記せず「省略」注記だけを残す（作成自体は継続する）。titleは常に`ポストモーテム: `接頭辞を付けて作成し、キュー一覧（`bin/agentic-loop status`）上でポストモーテムだと判別できるようにする。終了code: 成功時0（stdoutにIssue番号）、GitHub API失敗時1。**日次自動作成上限では拒否されない**（下記参照）。
- `link`: action item Issueをnative sub-issue・native dependency（`blocked_by`）として登録し、ポストモーテムIssueを`agent:blocked`へ移す。既存の依存関係機構（[Issueキュー運用](issue-queue.md)）がそのまま完了・検証を監視し、全action item完了で自動的に`agent:queued`へ戻す。
- `status`: 紐付いたaction item（native + 本文`Blocked by:`の合併）ごとに、完了・検証済み／未完了／確認不能のいずれかを日本語で報告する（読み取り専用）。
- `complete`: action itemが全て検証済みで、かつIssue本文の分析項目（雛形プレースホルダ`（記入してください）`が残っていない）と残余リスク節（空でない）が記入済みであることを機械的に確認したうえで、workerが完了処理を行うための内部マーカーを書く。いずれか不成立なら**Issueをcloseせず**非ゼロで終了し、不足している条件を日本語で説明する。

## `postmortem link`/`complete`とworker.shの終端処理

`postmortem link`・`postmortem complete`は、他のIssue処理と同様にworkerの専用worktree内から（通常はexecターンでproviderが呼ぶ形で）実行される。どちらもGitHub上の状態遷移だけを行いcommitやPRを作らないため、worker.shの通常の完了判定（対応branchのmerge済みPRを要求する）とは前提が異なる。これを区別するため、両コマンドはこのターンの意図を示す内部マーカー（`$STATE_ROOT/postmortem/turn-ISSUE`）を書き、worker.shの終端処理（providerループ直後）がそれを読む。

- `link`turn: Issueは`agent:blocked`のまま留まり、`failed`へは落ちない。
- `complete`turn: branchがdefault branchより先に進んでいなければ（このturnに実コミットが無ければ）、workerがIssueを`agent:completed`にしてcloseし、専用worktree/local branchを削除する。**branchが先に進んでいた場合（実際にcommitが生じていた場合）は、この経路を使わず通常のmerge済みPR前提の完了経路にフォールバックする**（未mergeの作業を黙って捨てない）。

providerは通常どおり最終応答を`AGENTIC_LOOP_RESULT=completed`で終えればよい。判断はworker.shが上記マーカーの有無で行う。

## 設定（`.agentic-loop.toml`の`[postmortem]`）

```toml
[postmortem]
auto_detect = "on"              # on/off。既定on（コードのfallbackはoff）
max_auto_created_per_day = 5    # 1〜20の整数、既定5
```

- `auto_detect`: 自動起動2経路（反復失敗の`agent:parked`到達、全provider poolの`all-pools-paused`遷移）の有効・無効。`off`でも明示要求（`postmortem create`、および`postmortem`skillからの利用）は常に有効。
- `max_auto_created_per_day`: **自動起動のみ**に適用される1日（UTC）あたりの作成数上限。上限に達した自動起動候補は作成をログに記録するだけで見送り、supervisorのpollは止めない。明示要求はこの上限で拒否されない（ただし作成数は同じ日次counterへ加算されるため、明示要求が多い日はその後の自動起動が先に抑制される）。

## 再発防止の検証手順（action item完了後）

1. `bin/agentic-loop postmortem status ISSUE`で全action itemが「完了・検証済み」であることを確認する。
2. 原因と対策の対応、再現fixtureまたは同等の検証手段、回帰防止（テスト・lint・観測性）が揃っているか確認する。実施しない項目があれば理由と受容する残余リスクを本文の残余リスク節へ明記する。
3. `bin/agentic-loop postmortem complete ISSUE`を実行する。成功すれば次のturnでworkerがIssueをcloseし、専用worktree/branchを削除する。
4. 失敗した場合は理由（未完了のaction item／未記入のプレースホルダ／空の残余リスク節）を解消するか、不足しているaction item Issueを追加作成して`postmortem link`で紐付け直す。

## トラブルシュート

- **重複起票が疑われる**: `postmortem create`は`kind`+`subject`のfingerprintで**open**なIssueを検索し、一致すれば新規作成せず既存Issueへ再発commentを追記する。過去のポストモーテムが既にcloseされている場合は、同じ事象でも新しいIssueとして扱う（再発は別記録に値するため）。重複に見える場合は対象Issueの本文にある`fingerprint=`マーカーとkindを比較する。
- **上限に達した**: 自動起動はログへ「1日あたりの自動作成上限に達したため作成を見送りました」と記録するだけで、Issueは作られない。急ぎ記録が必要なら明示要求（`postmortem create`）を使う（上限で拒否されない）。
- **`postmortem complete`が拒否される**: stderrに日本語で理由（未完了のaction item番号、未記入のプレースホルダ、空の残余リスク節）が出力される。該当箇所を解消してから再実行する。
- **worker.shが`agent:blocked`を`failed`で上書きしてしまう場合**: `link`/`complete`ターンの内部マーカーが書き込まれていない可能性がある。`postmortem link`/`postmortem complete`のコマンドがそのターン内で正常終了（終了code 0）したか確認する。

## 費用

自動検出2経路それぞれ、`postmortem create`が行うdedup検索は`label:postmortem`かつ`state=open`のIssue一覧1回のREST呼び出しに限る。無制限な履歴取得・無限retryは行わない。`postmortem link`のREST呼び出し数はaction item数に線形。`postmortem complete`は既存の`dependency_satisfied`（依存Issueの状態確認、既存の依存関係機構が既に行っている呼び出し）と単純な文字列検査のみで、新規の外部呼び出しは追加しない。追加費用ゼロ（詳細は[ADR 0026](../decisions/0026-postmortem-closed-loop.md)）。
