# 0030: `gh --body "@path"` による要求の silent 消失を検出・予防する

- 状態: 採用
- 日付: 2026-08-21

## 背景

`gh issue create --body "@path/to/file.md"` のように `--body`/`-b` に `@path` を渡しても、`--body-file` と違ってfileは読まれない。リテラル文字列 `@path/to/file.md` がそのままIssue本文・commentになり、意図した要求が silent に失われる（Issue #272）。PR #285（Issue #278）で `scripts/lint-cli-contracts.sh` に警告規則が追加済みだったが、これはrepository source中の`gh`呼び出し行だけを対象とする静的検査であり、(a) 実際に発生済みの壊れたIssue本文を検出しない、(b) Bash経由で対話的に打たれた`gh`呼び出しには効かない、という2つの穴が残っていた。

## 判断

### 判定を単一の保守的な述語に集約する

「取り違えられたファイル参照」を`body_unexpanded_file_reference`（`bin/lib/agentic-loop/common.sh`）という1つのbash述語に固定する: 前後空白を除いた本文全体が改行を含まない1行で、`@`から始まり、`@`以降に空白を含まず、パス様（`/`を含むか`.拡張子`で終わる）であることをすべて満たす場合のみ真とする。複数行本文（`@path`を含む行があっても）、code block内の`@path`（fenceがあれば必ず複数行）、`@wakuwaku3 ご確認ください`のような空白を含む`@mention`、`@name`単体は、いずれも偽になる。誤検出で正当な要求を止めないことを、検出漏れを許容してでも優先する。

`.claude/hooks/require-gh-body-file.sh`（Bash呼び出しレベルのフック）は同じ述語をシェル関数として複製する。sourcing境界が異なる（hookは`common.sh`をsourceしない独立プロセス）ため共有できず、`tests/test-agentic-loop.sh`が両実装を同じfixture表で突き合わせて drift を検出する。`scripts/lint-cli-contracts.sh`のsource-level規則も同じ述語に合わせて精緻化する（`--body="@x"`・`-b "@x"`の検出漏れと`--body "通常の本文"`の誤検出を修正）。

### 3層で扱う: claim前diversion／status／doctor

1. **claim前diversion（予防）**: `claim_next`（`supervisor.sh`）は既に候補の本文をbase64から復元済みなので、`dependency_status`と同じ位置で判定し、該当すれば`mark_body_unexpanded`（`common.sh`、`mark_dependency_blocked`と同型）でclaimせず`agent:needs-input`へ移す。local state file（`$STATE_ROOT/body-unexpanded/issue-<N>`）で冪等化し、監査コメントは1回だけ投稿する。close はしない（[0016](0016-failure-park-not-close.md)と同じ理由: 要求そのものが無効になったわけではなく、人が正しい本文を再投稿すれば復旧できる）。これにより空の要求でexecを消費しない（#263と同種の浪費を防ぐ）。
2. **status（観測）**: `status_snapshot_fetch`のjq projectionに本文のbase64列を追加する。open Issue一覧は既に`--paginate`で取得済みのため追加のGitHub呼び出しは発生しない。queued Issueに対して判定し、該当すれば`issue-body-unexpanded`異常として「要対応」に表示する。本文そのものは出力しない（既存の「token・worker log本文・Issue本文・コメント・providerのresult fileは一切読まない・表示しない」方針に合わせ、番号・固定文・経過のみを出す）。
3. **doctor（診断）**: `status_snapshot_fetch`を再利用してopen Issue本文を検査する（追加呼び出し0回）ことに加え、repository全体の直近コメント最大100件（`--paginate`なしのbest-effort、`status_stale_fetch`と同じ方針）を1回だけ読み、同じ述語でスキャンする。該当すれば`doctor_add warning`。

3層とも、既存の open-Issue 一覧取得を再利用するか、高々1回の直近コメント取得を追加するだけで、実行のたびに増える追加費用はない。

### `status`のADR 0005（追加読み取りをしない）との関係

[0005](0005-status-observability.md)は「anomalyはlocal stateだけで判定し、statusは追加のGitHub読み取りをしない」ことを明文化した。本変更は既取得のsnapshot（open Issue一覧）に列を1つ足すだけで、`status`が発行するGitHub呼び出しの回数を変えないため、この制約に違反しない。0005自体は書き換えず、本ADRで「snapshotが既に持つ列を使う判定はlocal state判定と同格である」という拡張として記録する。

### Bash用PreToolUseフックはfail-open

`.claude/hooks/require-gh-body-file.sh`は`Bash`を対象にした新規のPreToolUseフックである。Bashはこのloopで最も使われるtoolのため、payload解析・yq不在・JSON破損などで判定不能になった場合は**必ず`exit 0`（許可）へfallbackする**。fail-closedにすると、想定外のcommand文字列1つでloop全体が停止しうる。`confirm-main-worktree-edit.sh`（Edit/Write/NotebookEdit対象）がfail-closedである理由（見逃すと本番Edit操作そのものを取り逃す）とは非対称であり、対象toolの使用頻度とfail-closedの影響範囲が異なるための意図的な判断である。`ask`は使わない（`bypassPermissions`に上書きされ実質的にgateにならないため、[0009](0009-foundation-upgrade.md)以来の既存hookと同じ理由）。`--body-file`を含む呼び出しは常に許可する。

## 却下した案

- **本文全体をGitHub APIから毎回取得してstatusで判定する**: 0005の「追加読み取りをしない」制約に反する。既取得snapshotへの列追加のみに留めた。
- **doctorのコメント検査を全件走査にする**: repository全体のコメントは累積して増え続けるため、`stop_condition`のない全件取得は[有限資源とスケーラビリティのポリシー](../policies/resource-scalability.md)に反する。`status_stale_fetch`と同じ「直近1ページのbest-effort」に限定した。
- **claim前diversionで該当Issueをcloseする**: 要求は失われているが、人が正しい本文を再投稿すれば復旧可能であり、[0016](0016-failure-park-not-close.md)の「失敗はparkし、closeしない」方針に従う。
- **Bashフックをfail-closedにする**: Bash呼び出し全体を止めるリスクがfail-closedの利益（`gh --body "@path"`の事前ブロック）を上回る。既にclaim前diversion・status・doctorの3層があるため、hookは「早期に気づければ助かる」best-effort追加防御として位置づける。

## 帰結

- `--body`/`-b`に`@path`様の値を渡した`gh issue`/`gh pr`のBash呼び出しは、hookにより実行前にdenyされる（fail-open、`--body-file`を案内）。
- 既に本文が壊れているqueued Issueは、claimされずに`agent:needs-input`へ移り、`status`/`doctor`の両方で観測できる。
- 判定述語は1箇所（`common.sh`）と1複製（hook）に限定され、fixture表による突き合わせでdriftを検出する。
- `status`が発行するGitHub呼び出し回数、`doctor`のopen-Issue一覧取得回数はいずれも本変更前と同一であり、追加費用は`doctor`実行あたり直近コメント取得1回のみ。
