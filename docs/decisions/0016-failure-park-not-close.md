# 0016: リトライ予算の枯渇はcloseではなくparkとして扱う

- 状態: 採用
- 日付: 2026-08-18

## 背景

`retry_failed` は、担当workerが `[queue].max_attempts`（既定3）回試行しても検証済みmergeへ到達できなかった `agent:failed` Issueを、`agentic-loop:unresolved` 監査コメントとともに無条件で `state=closed` にしていた。供給経路は3つ:

- workerが `AGENTIC_LOOP_RESULT=failed` を返す
- lease期限切れ・突然死の繰り返しで `recover_expired` が `agent:failed` へ昇格させる
- 上記に合流した `retry_failed` がcloseする

この結果、実際に**正当で未実装のまま12件が誤ってclose**された（2026-08-13、17分間で9件が連続close）。GitHub上の `stateReason` は `COMPLETED` となり、`metrics` の転帰でも成功に埋没して失敗が不可視化されていた。

## 判断

**「workerがリトライ予算を使い切った」と「要求が解決不能である」を区別する。** 前者は今回のmodel/試行回数で完了できなかっただけで、要求そのものの正当性とは無関係である。

`max_attempts` に到達した `agent:failed` Issueは、`retry_failed` によって新しい非claim状態 **`agent:parked`** へ移す。park は次の性質を持つ:

- **open のまま**。closeしない。GitHubの `stateReason` を汚さない。
- **非claim**。`claim_next`・`retry_failed`・`recover_expired`・`triage_stale_queued`・`reconcile_queued_categories` のいずれからも対象外（`refresh_supervisor_snapshot` の分類では意図的に `other` に落ちる）。
- **キューを塞がない**。scope cache・conflict wait・attempts・local worker stateをすべて解放するため、他Issueのclaimを妨げない。
- **人間トリアージ待ち**。要求の精緻化・より小さな単位への分解・`bin/agentic-loop resume ISSUE` による再投入・不要なら認可済み `dispose` のいずれかを促す監査コメントを1件残す。

park化（`park_issue`）は `set_issue_state` のLabel付け替えが失敗した場合はcloseもparkもせず `agent:failed` のまま次pollに委ねる（fail-safe）。

## 自動closeのallowlist

自律ループが `state=closed` を呼び出せるのは次の4箇所だけとし、`scripts/lint.sh` がソース全体を静的に検査してこれを超えないことを保証する。

| 箇所 | state_reason | 意味 |
| --- | --- | --- |
| `worker.sh`（completed、resume経路含め2箇所） | 既定（completed相当） | Supervisorが検証済みmergeを確認した正当な完了 |
| `worker_state.sh` の `triage_stale_queued` | `not_planned` | `agent:queued` のまま `stale_days` 日間放置された要求のトリアージclose |
| `dispose.sh` の `cmd_dispose` | `not_planned` | 認可済み運用者による明示的な終了（cancelled/superseded/duplicate/merged） |

`retry_failed` と `recover_expired` の関数本体にこの呼び出しが現れないことも同じlintで検査する。単なるfailure上限は、この表のどれにも該当しないため、二度とcloseの理由になり得ない。

## `declined` はcloseではなく `agent:needs-input` である

Issueの完了条件には「declined→close」という記述もあり得るが、現行実装（`worker.sh`）はworkerが `AGENTIC_LOOP_RESULT=declined` を返した場合でも `agent:needs-input` へ載せるだけで、worker自身はcloseしない。これは [ADR 0010](0010-authorized-issue-disposition.md) の「workerの自己申告やbot commentを認可根拠にしない」という判断の帰結であり、本ADRの目的（失敗由来のcloseを根絶する）に対してはより厳格な上位互換である。認可済み運用者が内容を確認し、必要なら `bin/agentic-loop dispose ISSUE --reason cancelled` 等で終了する。worker promptとdocs/operations/issue-queue.mdの記述もこの実装に合わせて修正した。

## metrics/statusの転帰

`status` は `agent:parked` の件数とURLを他の状態と同じ形式で表示する。`metrics` は open Issueのうち `agent:parked` を持つものを `dispositions.parked` として独立集計し（`dispositions.open` に二重計上しない）、`counters.parked` にpark回数を積む。既存の `dispositions.unresolved`（過去に誤ってcloseされた `agent:failed` closeの集計用キー）はデータ形状の後方互換のため残すが、本ADR以降は新規に発生しない。

## 帰結

- 失敗由来の終端がGitHub上で `stateReason=COMPLETED` にならなくなる。
- リトライ予算を使い切った正当な要求はbacklogとして残り、人間が精緻化・分解・再投入・終了のいずれかを選べる。
- 追加のGitHub API呼び出しは発生しない（park化は既存のclose呼び出し1回をLabel PUT + comment 1回に置き換えるだけで、pollあたりの一覧取得回数は変わらない）。
- 追加費用ゼロ。外部サービス・課金は増えない。

## 対象外

- 過去に誤ってcloseされたIssueの自動復帰（人手での `resume` に委ねる）。
- `[queue.stale_days]` による `agent:stale` のclose自体の是非（本ADRは `state_reason=not_planned` の付与だけを変更し、park化の対象外とする。stale化はqueued放置に対するトリアージであり、失敗由来ではない）。
- 枯渇カスケード（token/rate limit枯渇時に多数のIssueが短時間でparkされうる現象）自体の緩和。これは別Issue（枯渇pauseの頑健化）の範囲であり、本ADRのparkはその上流で「大量close」という被害を防ぐ二層目の防御である。
