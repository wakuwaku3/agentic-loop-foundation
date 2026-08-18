# 0019: Issue単位の実行制御（pause / resume / abort）

- 状態: 採用
- 日付: 2026-08-18

## 背景

これまでの停止系入口は2つしかなかった。`stop`（Supervisor全体をdrainして終了する運用操作）と、[ADR 0010](0010-authorized-issue-disposition.md)の`dispose`（認可済みの**要求終端**。close を伴う）である。「特定Issueの作業だけを一時的に止めて、後でそのまま続ける」手段が存在せず、運用者は`agent:*` Labelを手で書き換えるしかなかったが、それは`claim_next`・`retry_failed`・`recover_expired`と競合し、走行中のworkerも止まらない。

## 判断: 実行制御層と要求終端層を分離する

| 層 | 入口 | 対象 | closeするか |
| --- | --- | --- | --- |
| Supervisor全体 | `start` / `stop` | 全Issue | しない |
| **実行制御（本ADR）** | **`pause` / `resume` / `abort`** | Issue 1件 | **しない** |
| 要求終端（ADR 0010） | `dispose` / `resume` | Issue 1件 | する（`not_planned`） |

- **`pause ISSUE [--reason TEXT]`**: 新しい非claim・openの状態`agent:paused`へ移す。要求は生きたまま、実行だけを止める。
- **`resume ISSUE`**: 既存`resume`（disposed closed / parked）に`paused`を追加。lease・worktree・branch・PR・checksを再検証してから、pause前の状態に応じた安全な状態へ戻す。
- **`abort ISSUE [--reason TEXT]`**: 進行中の実行を打ち切り、既存の`agent:parked`（open・非claim・人間トリアージ待ち、[ADR 0016](0016-failure-park-not-close.md)）へ移す。**新しい終端状態は作らない。** 要求そのものの終端は引き続き`dispose`だけが行う。「abortはキャンセル・置換Issueの方針に従う」という要件は、ADR 0010の認可・監査・成果物保持の規律にabortも従う（parkedへ寄せ、closeはdisposeへ委譲する）と解釈した。abortのコメントには`resume`と`dispose`の次操作を明示する。

checkpointは新規発明していない。[ADR 0004](0004-worker-resume-and-handoff.md)の再開機構（`resume_probe`によるGit/GitHub観測からのphase導出＋`agentic-loop:handoff`コメント）が既にworkerの突然死からの安全な再開を担保している。pause/abortはworkerを安全に止めた直後にこの観測を再実行してhandoffを更新する（`control_checkpoint`）ことを「checkpointを残す」の実体とした。

## 状態遷移

`pause`の許可元と、`resume`の復帰先（pause時に記録した`from=`から決定）:

| pause元 | pause時の動作 | resume復帰先 |
| --- | --- | --- |
| `queued` | 即座に`paused`（workerなし） | `queued` |
| `running` | `stopping`を経由してworkerをdrain → `paused` | `queued`（`running`へは直接戻さない。claim経由のみ） |
| `in-review` | 同上 | `queued` |
| `needs-input` | 即座に`paused` | `needs-input`（回答待ちを飛ばさない） |
| `blocked` | 即座に`paused` | `blocked`（依存回復が再評価してqueuedへ） |
| `failed` | 即座に`paused`（自動retryの停止に有効） | `queued` |
| `parked` | 拒否（既に非claim。abortと重複するため） | — |
| `paused` | 冪等成功（メッセージのみ、状態・コメント変更なし） | — |
| 終端4種 / closed | 拒否（exit 1、`dispose`/`resume`を案内） | — |
| pause記録が読めない | — | `queued`（安全な既定。コメントに明記） |

`abort`の許可元は`queued|running|in-review|needs-input|blocked|failed|paused`。`parked`は冪等成功、終端4種・closedは拒否。

## 実装

- `bin/lib/agentic-loop/control.sh`（新モジュール、[ADR 0013](0013-agentic-loop-modules.md)の方針どおり独立モジュールとした）が`cmd_pause`・`cmd_abort`・`control_resume_paused`を持つ。`dispose.sh`の`authorized_operator`・`issue_agent_state`を再利用し、認可・状態読み取りの経路を一本化した。
- **協調停止**: `worker_state.sh`に`worker_stop_requested`/`worker_request_stop`マーカーを追加した。`worker()`は自身のstage境界（planループの先頭、replan直前）でだけこれを確認し、真なら静かに（Label・コメントを変更せず）終了する。Labelの確定は`pause`/`abort`側が行うため、二重書き込みの競合はない。provider実行中は協調点を作れないため、`control_drain_local_worker`は`pause_grace_seconds`（既定120秒）だけ待ってからTERM、5秒後にKILLする。外部操作が中途で切れた場合の収束は既存のworker突然死と同じ回復経路（次回claim時の`resume_probe`）に委ねる。
- **不可分操作の保護**: `worker_critical_begin`/`worker_critical_end`マーカーを追加し、`decomposition_materialize`（native sub-issue/dependency作成）と、完了確定シーケンス（`trace_gate`〜`cleanup_completed_worker`〜`set_issue_state completed`〜close）を囲んだ。drainはこのマーカーが立っている間、TERM送出前に`pause_grace_seconds`を上限として待機する。
- **Supervisor再起動後の永続**: `agent:paused`はGitHub Label（正本）に保持されるため、Supervisor再起動後もそのままである。`refresh_supervisor_snapshot`（`project.sh`）に`paused`という専用bucketを追加したが、これを読むのは新設の`drain_paused_workers`（`supervisor.sh`）だけである。`claim_next`・`retry_failed`・`recover_expired`・`triage_stale_queued`・`reconcile_queued_categories`・`requeue_answered`・`requeue_dependency_ready`はいずれも別bucketしか読まないため、paused Issueは構造的に自動遷移の対象外になる（`agent:parked`と同じ設計）。`drain_paused_workers`は、pause発行後にclaim/worker起動が競合した場合や、pauseがこのhostのsnapshotに現れる前に発行された場合の取りこぼしを、次pollで無償に（追加のGitHub API呼び出しゼロで、既存snapshotだけを使って）回収する。
- **statusとProject**: `status`の「状態サマリ」に`paused`の件数・URLを追加し、local pause記録があるIssueには操作者・時刻・pause前状態を併記する（Issue本文・コメント・worker logは読まない）。Projectの`Agent status`に`Paused`を追加し、`Blocked by`フィールドにはconflict-waitより優先して一時停止情報を表示する（他hostのpauseはlocal記録がないため空欄になるbest-effort挙動）。
- **監査**: markerは`<!-- agentic-loop:pause schema=1 actor=... issue=N from=<state> at=<epoch> -->`と同形の`abort`、既存の`agentic-loop:resume`。すべて`<!-- agentic-loop:`で始まるため、`requeue_answered`の「人間の返信」判定に混入しない。

## コメントを命令として誤解しないための構造的保証

操作入口は**CLIのみ**。Issueコメント・PR本文・provider出力を解析して操作を起動する経路は一切追加しなかった。認可は`authorized_operator`（`gh api user` → `repos/OWNER/REPO/collaborators/LOGIN/permission`が`write|maintain|admin`）で、不足時はGitHub・Git・Labelを一切変更せず終了する。これにより、`/pause`や`agentic-loop:pause`に見える文言をIssueコメントへ投稿しても状態は変わらない。

## 対象外

- pause中のworktree/branch/PRの自動削除。
- pauseの自動期限切れ・スケジュール実行。
- コメント由来の操作。
- 他hostが所有するworker processの直接kill（`drain_paused_workers`はこのhostのpidfileが生存しているものだけをdrainする。他host所有のIssueへの`pause`は、所有hostが次pollで自ホストのsnapshotから同じ関数を使って自律的にdrainする）。

## 費用

追加の外部service・課金はない。Supervisorのpoll毎のREST呼び出し回数は変わらない（`drain_paused_workers`は既存snapshotのみを参照する）。`pause`/`abort`/`resume`は運用者の明示実行時のみ数回のREST呼び出しを行う。追加費用ゼロ。
