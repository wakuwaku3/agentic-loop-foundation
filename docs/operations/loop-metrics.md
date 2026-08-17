# Loopの運用品質指標（[ADR 0007](../decisions/0007-loop-metrics.md)）

`bin/agentic-loop metrics` は、既存のGitHub Issue/PR/comment履歴だけから、待ち時間・処理時間・失敗・手戻り・競合の傾向を再現可能に集計する読み取り専用コマンドである。`status`（いまの状態）・`doctor`（合否）と並ぶ3つ目の運用入口として、「履歴の傾向」を担当する。

```sh
bin/agentic-loop metrics [--days N] [--as-of EPOCH] [--format json]
```

- `--days`（既定30）: 集計する窓の長さ（日数）。
- `--as-of`（既定は実行時点のepoch秒）: 窓の終端。同じ値を指定すれば、同じGitHub状態に対して`generated_at`（集計を実行した実時刻の記録）を除き常に同一の出力になる（read-onlyかつイベント処理・分布計算のいずれも入力順に依存しないため）。baseline取得や回帰確認をこの値で固定して再実行できる。
- `--format json`: `schema_version: 1` の単一JSONを1行で出す。既定はテキストの日本語summary。
- 引数不正時のみ終了code 2。それ以外は常に0（合否判定は`doctor`の責務。異常はテキスト・JSONいずれも情報としてのみ示す）。
- GitHub・Git作業ツリーへの書き込みは一切行わない。

## 収集境界

REST(core)読み取りは常に3本に固定する。

| 収集 | endpoint | 用途 |
| --- | --- | --- |
| A | `GET issues?state=all&since=<窓開始>` | Issue番号、作成時刻、close時刻、state、`category:*`/`agent:*` label、本文markerの数値priority（0-100、未設定0） |
| B | `GET issues/comments?since=<窓開始>&sort=created&direction=asc` | `<!-- agentic-loop:...-->` marker文字列とその作成時刻（`.issue_url`からIssue番号を得て、収集Aの集合に無い番号＝PRの一般コメントは除外する） |
| C | `GET pulls?state=all` | `agent/issue-<N>`ブランチのPR番号、作成時刻、merge時刻 |

Actions（CI run）、GraphQL、Projects APIは読まない。理由は[ADR 0007](../decisions/0007-loop-metrics.md)を参照。実行前に`core_budget_allows "$((CORE_RESERVE + 3))"`を確認し、reserve未達なら1回もAPIを呼ばずに`github_available:false`を返す（heartbeatや復旧など必須操作の枠を優先する）。収集ごとに独立して失敗しうる。1本が失敗しても取得できた分だけで報告を続け、`warnings`に記録する。

### privacy境界

- 収集AはIssue本文のうち数値`agentic-loop:priority` marker値（0-100）だけを取り出し、収集Bはコメント本文のうち`<!-- agentic-loop:...-->`の中身だけを取り出す。Issue本文の他の部分、コメントの日本語本文、worker log、providerへの生のpromptは要求も保存もしない。
- markerの`worker=`fieldは常に破棄する。出力にworker識別子は一切現れず、worker単位の内訳・ランキングを出す経路も実装しない。
- Issue titleは収集A・B・Cのいずれでも取得しない。出力に現れるのはIssue番号・PR番号・集計値・label名・enum値・数値priorityだけである。

## 指標schema

「試行(attempt)」= 1本のleaseコメント（claim時刻で作成され、以後は同一コメントをPATCHで更新するため一意に数えられる）から、次の終端marker（`completed`/`failed`/`declined`/`needs-input`/`exhausted`/`worker-timeout`/`recovered`/`recover-exhausted`）までの区間。

| 指標 | 起点 | 終点 | 窓帰属 | 欠損時 |
| --- | --- | --- | --- | --- |
| `queue_wait` | Issue作成時刻、または直前の再queue marker（`retry`/`recovered`/`answer-detected`/`exhausted`/`shutdown`） | 次のlease時刻 | 終点（lease時刻） | 未claim（QSTARTが窓終端でも未消費）は`open_queue_wait`へ別集計 |
| `attempt_duration` | lease時刻 | 終端marker | 終点 | 終端が観測できない試行は`open_attempts`に計数し、分布から除外 |
| `needs_input_wait` | `needs-input` | `answer-detected` | 終点 | 未回答は`open_needs_input_wait`へ別集計 |
| `conflict_wait` | `scope-conflict` | `scope-resolved` | 終点 | 未解消は`open_conflict_wait`へ別集計 |
| `dependency_wait` | `dependency-blocked` | `dependency-ready` | 終点 | 未解消は`open_dependency_wait`へ別集計 |
| `lead_time` | Issue作成時刻 | 最後の`completed`marker時刻 | 終点（close時刻と同じ窓判定に従う） | `completed`markerが無い場合（labelだけで転帰を判定した場合を含む）は計測対象外 |
| `plan_seconds` / `exec_seconds` | — | — | markerの観測時刻（窓判定なし。件数が少ないため単純集計） | `usage`markerの`seconds=`が無ければ欠損として除外 |
| `pr_review_wait` | PR作成時刻 | merge時刻 | 終点（merge時刻） | 未mergeは`counters.unmerged_pr`で件数のみ別集計 |

分布はいずれも n（件数）/ mean（平均）/ p50 / p90 / max の5値で、値はすべて秒。p50・p90はnearest-rank（floor）法で、`sort -n`した後に算出するためイベント処理順に依存しない。n=0のときは`mean`以下がJSONで`null`になる。

### 転帰（disposition）

`completed` / `unresolved` / `stale` / `declined` / `open` / `other` の6種。closedなIssueはそのclose時刻が窓に入っている場合だけ数え、`declined`markerが1件でもあれば（labelが更新されない既知の非対称性があるため）labelより優先して`declined`と判定する。それ以外はlabel（`agent:completed`→`completed`、`agent:failed`のままclose→`unresolved`、`agent:stale`→`stale`）で判定し、いずれにも当たらない場合は`other`とし、`warnings`に`label_marker_mismatch`として明示する。openなIssueは時間帰属なしで常に`open`に数える。

### category / priority別集計

`by_category` / `by_priority` は、**Issue作成時刻**が窓に入っているものだけを分母にする（転帰の集計とは異なる窓の切り方であることに注意する）。「今期どんな種類の要求が積まれたか」を見るための集計である。`by_priority` のキーは本文markerの数値priority（[ADR 0015](../decisions/0015-numeric-priority-marker.md)）で、出現した値だけを文字列キー（`"0"`、`"50"`、`"90"`等）として昇順に出力する。旧 `priority:*` labelは読まない（移行後は存在しない）。

### 件数系（counters）

`conflict_wait` と `scope_conflict` は、直列化が必要な**hard conflictのみ**を数える。理由tokenは `structural:path`（rename/move・directory再編でpath重複）／`*`（全体・`exclusive_paths`昇格）／`unknown`（`unknown_scope=isolated` 同士）／`env:NAME`（外部環境の完全一致）のいずれかで、`scope-conflict` markerの `token=` に記録される。通常の同一path編集（soft overlap）は並列実行され、`scope-conflict` markerもカウントも発生しない（merge時はrebase・再検証で収束する。緩和前はpath重複の全件がカウントされていたため、件数は構造的衝突のみに縮小する）。

`attempts`（=lease件数）、`retry`、`recovered`、`worker_timeout`、`exhausted`、`scope_conflict`、`dependency_block`、`needs_input_round`、`resume`（`handoff`の`phase!=fresh`）、`requeue`（retry+recovered+answer-detected+exhausted+shutdownの合計）、`replan`、`open_attempts`（終端未観測の試行数。現在進行中の試行も含む中立的な件数）、`unmerged_pr`。

### 失敗理由（failures）

`failed`markerの`reason=`（`merge-or-cleanup`/`foreign-artifact`/未指定時は`unspecified`）、`worker-timeout`、`exhausted`、`recover-exhausted`をキーごとに件数集計する。

### worker稼働率（utilization）

`busy_seconds`は窓内に収まる`attempt_duration`（開始が窓より前なら窓開始でクリップ）の総和、`ratio = busy_seconds / (max_workers × window_seconds)`。これは「Supervisorが持つworker slotのうち平均して何割が稼働していたか」という**設備の占有率**であり、worker単位の内訳を持たないため個人やworker単位の速度比較には使えない。

## 出力例（`--format json`の主なキー）

```json
{
  "schema_version": 1,
  "generated_at": 1700000000,
  "window": {"start": 1697408000, "end": 1700000000, "days": 30},
  "github_available": true,
  "dispositions": {"completed": 0, "unresolved": 0, "stale": 0, "declined": 0, "open": 0, "other": 0},
  "durations": {"queue_wait": {"n": 0, "mean": null, "p50": null, "p90": null, "max": null}, "...": "..."},
  "counters": {"attempts": 0, "retry": 0, "...": "..."},
  "failures": {},
  "utilization": {"max_workers": 4, "window_seconds": 2592000, "busy_seconds": 0, "ratio": 0},
  "by_category": {"loop-continuity": 0, "confidentiality-incident": 0, "integrity-incident": 0, "availability-incident": 0, "feature": 0, "improvement": 0},
  "by_priority": {"25": 2, "50": 1, "75": 3},
  "warnings": []
}
```

## 運用判断への対応

| 観測 | 示唆される見直し |
| --- | --- |
| `queue_wait`が悪化 | `[queue].max_workers`／`poll_seconds`の見直し |
| `attempt_duration`のうち`exec`失敗率上昇（`failures`） | `agent.retry.plan_max`やproviderの見直し |
| `needs_input_wait`が長期化 | 要求受付時の完了条件・情報の充実 |
| `recovered`／`worker_timeout`が増加 | `[queue].lease_seconds`／`worker_timeout_seconds`の見直し |
| `scope_conflict`が増加 | 構造的衝突（rename/move・directory再編）の頻度、`[queue].exclusive_paths`、`unknown_scope`の見直し。soft overlap（通常の同一path編集）はこのカウントに含まれない |
| `pr_review_wait`が長期化 | required checksの所要時間・レビュー体制の見直し |
| `utilization.ratio`が高止まり | `max_workers`の増加、または要求の流入量そのものの見直し |

## 初期baseline

取得日: 2026-08-13、`--as-of 1786579200 --days 90`（`1786579200` = 2026-08-13T00:00:00Z。[wakuwaku3/agentic-loop-foundation](https://github.com/wakuwaku3/agentic-loop-foundation) に対して実行）。

```
対象期間: 2026-05-15 〜 2026-08-13（90日間、as-of epoch=1786579200)
転帰: completed=15 unresolved=2 stale=0 declined=0 open=25 other=3
待ち時間・所要時間:
  queue_wait: n=468 mean=29466s p50=51109s p90=53448s max=65481s
  open_queue_wait: n=0（欠損）
  attempt_duration: n=77 mean=2828s p50=76s p90=9619s max=9687s
  needs_input_wait: n=0（欠損）
  open_needs_input_wait: n=3 mean=143009s p50=143080s p90=143080s max=143569s
  conflict_wait: n=0（欠損）
  open_conflict_wait: n=0（欠損）
  dependency_wait: n=0（欠損）
  open_dependency_wait: n=0（欠損）
  lead_time: n=14 mean=2921s p50=1567s p90=5788s max=11713s
  plan_seconds: n=0（欠損）
  exec_seconds: n=0（欠損）
  pr_review_wait: n=17 mean=137s p50=94s p90=152s max=754s
件数: attempts=468 retry=0 recovered=34 worker_timeout=0 exhausted=0 scope_conflict=0 dependency_block=0 needs_input_round=8 resume=0 requeue=34 replan=1 open_attempts=23 unmerged_pr=1
失敗理由:
  unspecified: 27件
category別件数（作成が対象期間内のIssue）:
  loop-continuity: 3件
  confidentiality-incident: 0件
  integrity-incident: 0件
  availability-incident: 0件
  feature: 8件
  improvement: 15件
priority別件数（作成が対象期間内のIssue）:
  なし（窓内作成Issueにpriority markerがありません）
worker稼働率: 0.0070（max_workers=4, 対象期間=7776000秒, 稼働=217718秒。個人やworker単位の速度ではなく設備全体の占有率です）
警告:
  label_marker_mismatch: 窓内でcloseしたIssueのうち3件はLabelからdispositionを判定できませんでした（otherとして集計）。
```

同じ`--as-of`で再実行し、出力が完全に一致することを確認済み（再現可能性の確認。text形式は`generated_at`を含まないためbyte一致）。

`priority`別件数が「なし」なのは指標の欠陥ではない: このbaselineは本変更の統合前（2026-08-13）に旧実装で取得した実測記録であり、当時の窓内Issueに本文markerの`agentic-loop:priority`は1件も存在しないため。統合後は`bin/agentic-loop priority`や手動marker編集で運用すれば、`by_priority`は出現した数値キー（`"0"`〜`"100"`）だけで非ゼロになる。旧`priority:*`label時代のキー（critical/high/medium/low）は読まない。

## 既知の限界・v1で計測しないもの

- **Actions/CI runsは収集しない**: CI失敗率・CI所要時間は将来の拡張点。追加のREST endpointと予算消費を要するため、必要になった時点で独立した収集として追加する。
- **worker単位の内訳・ランキングは出力しない**: 個人評価や速度競争に使わせないという要求を、出力仕様として保証している。
- **レビュー往復回数・review comment数は計測しない**: PRごとの追加API呼び出しが必要。
- **Issueの再open回数は厳密に計測しない**: GitHubのevents APIが必要で、`completed`markerの複数回出現を代理指標にもしない（誤検知を避けるため）。
- **`agent:queued`付与時刻は近似**: events APIが必要なため、通常経路ではIssue作成時刻で近似する。
- **`since`はGitHubの`updated_at`基準で絞り込む**が、窓への帰属判定は必ずローカルで`created_at`／close時刻に対して再度行う。長期間まったく動きのない古いopen Issueは、直近の活動が窓の外にあると収集対象から外れる場合がある。
- **`pr_review_wait`は`agent/issue-<N>`ブランチのPRだけを対象にする**。手動で切ったPRや別命名のブランチは対象外。
