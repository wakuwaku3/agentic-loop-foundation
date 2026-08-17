# 0007: 既存のGitHub履歴からloopの運用品質指標を追加費用ゼロで集計する

- 状態: 採用
- 日付: 2026-08-13

## 背景

[0005](0005-status-observability.md)は「いまの状態」（`status`）と「合否」（`doctor`）を運用の入口として整備したが、「履歴の永続化やメトリクス出力」を明示的に対象外とした。運用者はqueue待ち時間・失敗率・needs-input滞留・worker稼働率のようなloop改善のボトルネックを、体感や個々のIssueコメントの目視以外で把握する手段を持たない（#47）。

Agentic Loopは既に、lease・usage・handoff・retry・recovered・worker-timeout・needs-input・answer-detected・scope-conflict/resolved・dependency-blocked/ready・replan・completed/failed/declined/exhausted/unresolved/recover-exhausted/stale/shutdownの各遷移を`<!-- agentic-loop:...-->`というHTML commentのmarkerとしてIssueコメントに記録している。新しい計測基盤は不要で、既存markerの読み出し・集計が本質的な作業である。

## 判断

### 収集境界はREST(core)読み取り3本（Issues・repo横断comments・Pull Requests）に固定し、Actions/CI・GraphQL・Projectsは対象外とする

`bin/agentic-loop metrics`は次の3クエリだけを読み取る。

- `GET issues?state=all&since=<窓開始>`: Issue番号・作成時刻・close時刻・stateと、`category:*`/`agent:*`ラベル、本文markerの数値priority（0-100、未設定0）。
- `GET issues/comments?since=<窓開始>&sort=created&direction=asc`: repository全体のIssueコメントを走査し、`<!-- agentic-loop:...-->` marker文字列を含む行だけを対象にする。Issue番号は`.issue_url`から取り、上記Issue集合に無い番号（Pull Requestの一般コメント）は内部結合で除外する。
- `GET pulls?state=all`: Pull Request番号・作成時刻・merge時刻と`head.ref`（`agent/issue-<N>`のみ対象）。

Actions（CI run）の収集は見送る。CI失敗率・CI所要時間は運用判断に有用だが、追加のREST endpointとAPI予算を要し、#47の完了条件（指標schema・fixture検証・baseline・費用/privacy/rate limit適合・`devbox run --pure check`）はいずれもCI集計を前提としない。将来必要になれば独立した収集として追加できるよう、`docs/operations/loop-metrics.md`に拡張点として明記する。

GraphQL・Projects APIは呼ばない。`metrics`は集計結果をどこにも書き込まない生成reportであり、Projectの数値集約・時系列表示には単一選択/テキストfieldしか使えず不向きである。ADR 0003が予約するGraphQL予算をmetricsの実行のたびに消費することも避けたい。「生成reportによる再現可能な代替」を選び、Project連携は不採用とする。

### 個人評価・worker速度競争を出力仕様で構造的に不可能にする

`worker=`fieldは常に破棄し、収集したいずれのmarkerからもworker識別子を取り出さない。Issue本文・コメント本文・worker log・生のpromptはそもそも要求されず、ローカルに到達しない（jqの段階で`<!-- agentic-loop:...-->`の中身だけを抜き出す）。Issue titleも取得しない。出力にはIssue番号と集計値・enumだけが現れ、worker単位の内訳やランキングを出す経路は実装しない。要求（#47）の「個人評価やworkerの速度競争に使わない」を、運用ルールではなく出力仕様として保証する。

### 指標は「試行(attempt)」単位の小さな状態機械で導出する

1つのIssueは複数回claimされうる（再試行・recovered・needs-input応答・scope/dependency解消のいずれも再queueを生む）。「試行」を1本のleaseコメント（claim時刻固定、以後はPATCHで同一コメントを更新するため一意に数えられる）から次の終端marker（completed/failed/declined/needs-input/exhausted/worker-timeout/recovered/recover-exhausted）までと定義し、Issueごとにこの区間を順に追跡する。

- `queue_wait`: Issue作成時刻、または直前の再queue marker（retry/recovered/answer-detected/exhausted/shutdown）の時刻から、次のlease時刻まで。
- `attempt_duration`: lease時刻から終端markerまで。
- `needs_input_wait` / `conflict_wait` / `dependency_wait`: 対応するopen/closeのmarker対から。
- `lead_time`: Issue作成時刻からその`completed`marker時刻まで（labelだけで転帰を判定した場合はmarkerが無いため計測対象外とし、欠損として扱う）。
- `pr_review_wait`: `agent/issue-<N>`ブランチのPR作成時刻からmerge時刻まで。

終端markerが窓の終わりまでに現れない区間（現在進行中、または観測できない中断）は、対応する`open_*`指標へ別集計し、通常の分布（n/mean/p50/p90/max）からは除外する。分布は`sort -n`した後にawkで算出するため、イベント処理の順序に依存せず再現可能である。

### 転帰(disposition)はclose時刻で窓に帰属し、labelを正としつつ`declined`markerだけ例外にする

closedなIssueは、そのclose時刻が窓に入っている場合だけ`completed`/`unresolved`(`agent:failed`のまま解決不能closeされたもの)/`stale`/`declined`/`other`のいずれかに数える。openなIssueは常に`open`として数える（時間帰属は不要）。

`declined`経路には既知の非対称性がある: workerが実施不要と判断してIssueをcloseする際、`agentic-loop:declined`markerは記録するが`agent:*`labelは更新しない（実装上の抜けであり、本Issueの範囲では追わない）。label通りに判定すると、closedにも関わらずlabelが`agent:running`のまま残ったIssueを誤分類してしまう。そこで、closedなIssueに`declined`markerが1件でも見つかった場合はlabelより`declined`判定を優先し、それ以外はlabelを正とする。labelからも`declined`markerからも転帰を判定できないclosedなIssueは`other`に落とし、`warnings`に`label_marker_mismatch`として明示する（黒箱の欠損にしない）。

### `category`/`priority`別集計は作成時刻で窓に帰属する

closeの有無に関わらず、Issue作成時刻が窓に入っているものだけを`by_category`/`by_priority`の分母に数える。「今期どんな種類の要求が積まれたか」を見るための集計であり、転帰(closeベース)とは異なる窓の切り方を意図的に採用する。両者の非対称性は`docs/operations/loop-metrics.md`に明記する。`by_priority`のキーは本文markerの数値priority（[0015](0015-numeric-priority-marker.md)）で、出現した値だけを文字列キー（`"0"`, `"50"`, `"90"`等）として出力する。旧`priority:*`labelは読まない。

### worker稼働率は設備占有率として定義し、個人の速度指標にしない

`utilization.ratio`は、窓内に収まる`attempt_duration`（窓境界にクリップ済み）の総和を`max_workers × 窓の長さ`で割った値であり、「Supervisorが持つworker slotのうち平均して何割が稼働していたか」を表す設備稼働率である。worker単位の内訳を持たないため、個人やworker単位の速度比較には使えない。

### 既存の`usage`markerを1箇所だけ拡張し、plan/exec所要時間を機械可読にする

`agent_post_usage()`が付ける`<!-- agentic-loop:usage worker=... -->`markerには、これまでstage・所要時間・exit codeが入っていなかった（日本語本文側にしかなく、markerだけを読むmetricsの収集境界では読めない）。`stage=`/`provider=`/`seconds=`/`exit=`をmarker自身に追加する。いずれも列挙値または整数であり、秘密や自由記述を一切含まない。既存の日本語本文行（Token使用量の表示）は不変に保ち、既存のテスト・運用手順に影響しない。

### 常に終了code 0、GitHubへの書き込みは常にゼロ、REST(core)予算はheartbeat/復旧より低い優先度にする

`metrics`は[0005](0005-status-observability.md)の`status`と同じ運用姿勢を継承する。引数不正だけが終了code 2で、それ以外は常に0。GitHub・Git作業ツリーへの書き込みは一切行わない。実行前に`core_budget_allows "$((CORE_RESERVE + 3))"`を確認し、reserve未達なら1回もAPIを呼ばずに`github_available:false`を返す。個々の収集（Issues・comments・pulls）が失敗しても、取得できた分だけで報告を続け、失敗は`warnings`に記録する（全体を失敗させない）。

### `--as-of`で窓の終端を固定し、再現可能なbaselineを取れるようにする

`--days N`（既定30）と`--as-of EPOCH`（既定は現在時刻）で窓を決める。同じ`--as-of`を指定した2回の実行は同一のGitHub状態に対して`generated_at`（集計を実行した実時刻の記録）を除き同一の出力になる（read-onlyであり、イベント処理も分布計算も入力順に依存しないため）。これにより、baseline取得や回帰確認を再実行可能な形で行える。

## 却下した案

- **worker単位の内訳を出しつつ利用を運用ルールで禁止する**: 出力に存在する情報は将来必ず使われる。要求の「個人評価に使わない」を運用ルールに委ねず、出力仕様として不可能にする方を選んだ。
- **Actions/CI runsの収集を初期scopeに含める**: 追加のREST endpointと予算消費を要し、#47の完了条件はCI集計を前提としない。拡張点として文書化し、必要になれば別Issueで追加する。
- **GitHub Projectのfieldへ集計値を書き込む**: 数値集約・時系列表示に不向きで、GraphQL予算を消費し、Project連携という追加の書き込み系統を持つことになる。

## 影響

- `bin/agentic-loop`: `metrics`サブコマンド（`cmd_metrics`、収集・状態機械・分布計算・text/JSON出力）を追加。`agent_post_usage`のmarkerに`stage`/`provider`/`seconds`/`exit`を追加。
- `docs/operations/loop-metrics.md`: 指標schema（起点・終点・欠損・窓帰属）、収集境界、privacy境界、baseline、既知の限界、運用判断への対応表を新設。
- `docs/operations/issue-queue.md`: `status`/`doctor`/`metrics`の責務分担表を更新。
- `README.md`: CLI一覧に`metrics`を追記。
- `scripts/lint.sh`/`scripts/install-target.sh`: 新規docsの必須化と配布、および出力仕様上の不変条件（worker内訳を出さない、追加費用が発生しない）のgrep検証を追加。
- `tests/test-agentic-loop.sh`: fake ghへのmetrics用ルーティングと、正常完了・失敗再試行・needs-input・再queue/クラッシュ回復・欠損履歴・privacy・API予算・決定性のシナリオを追加。

## 対象外

- Actions/CI runsの収集（CI失敗率・CI所要時間）。将来の拡張点として残す。
- worker単位の内訳・ランキング（要求により出力仕様として不可能にする）。
- レビュー往復回数・review comment数の収集（PRごとの追加API呼び出しを要するため見送る）。
- Issueの再open回数の厳密な計測（GitHubのevents APIが必要。`completed`markerの複数回出現を代理指標にもしない: 誤検知を避けるため単純に計測しない）。
- `agent:queued`付与時刻の厳密な計測（events APIが必要。Issue作成時刻を通常経路の近似として使う）。
- watch/TUIのような継続監視UIや、履歴のGitHub外への永続化。
