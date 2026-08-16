# 0005: statusをlocal state優先の運用観測入口にする

- 状態: 採用
- 日付: 2026-08-13

## 背景

`bin/agentic-loop status` は当初、Supervisorの生死と、running Issueの番号・title・phase・scope、競合待ち・依存待ちの一覧だけを表示していた（[0004](0004-worker-resume-and-handoff.md)でphase表示を追加済み）。これでは次が確認できない。

- running workerの開始時刻・経過時間・最終heartbeat・lease期限・worktreeの実在・関連PRの状態。
- queuedの総数と、次にclaimされる候補（category・priorityを考慮した順序）。
- needs-input・failed・in-review・blocked・staleの件数と対象。
- staleなsupervisor pid、期限切れlease、残存worktree/branch、Project同期の失敗といった運用上の異常。

`status` は対話Agentの受付手順（`docs/operations/issue-queue.md`「対話要求の受付」手順2）から要求ごとに呼ばれる可能性があり、また利用者が任意の頻度で手動実行する。[0003](0003-supervisor-resilience-and-api-budget.md)のREST(core)予算ガバナーが守っているAPI消費規律と矛盾する実装は避ける必要がある。

## 判断

### 追加観測はlocal state優先、GitHub呼び出しは1回の実行あたり最大2回に固定する

`status`が新たに表示する項目のほとんどは、既存のworkerが既に観測してlocalへ書いている値の再利用で得られる。追加のGitHub呼び出しを増やさない。

- **running Issueの詳細**（開始時刻・経過時間・最終heartbeat・lease期限・worktree・PR情報）は、`workers/<issue>.started`（`worker()`起動直後に書く）、`workers/<issue>.lease`（`id\texpires\theartbeat`のTSVへ拡張。旧形式（idのみ）も読める後方互換）、`workers/<issue>.resume`（`resume_handoff_write()`が`resume_probe()`の観測結果をそのままTSVで書く。既存の`agentic-loop:handoff`コメントと同じ実体で、定義上secretを含みえない）から読む。GitHub呼び出しは0回。
- **queuedの件数・次のclaim候補・needs-input/failed/in-review/blocked/staleの件数**は、1回の`status_snapshot_fetch()`（open Issue全件を`agent:*`ラベルで分類し、`queue_rank_jq`（`claim_next`と共有するcategory/priority rank式）も同時に計算する）と、1回の`status_stale_fetch()`（closedな`agent:stale`を最大100件、`--paginate`なし）で得る。1回の実行での REST(core)読み取りは最大2回。GraphQL・Projects APIは呼ばない。
- **異常検知**（staleなsupervisor pid/lock、期限切れlease、residualなworktree/branch、破損local state、Project同期の再試行待ち、claim一時停止）は、Supervisor自身が既に読み書きしているmarker file（`supervisor.pid`/`.lock`、`budget-paused`、`core-budget-paused`、`agent-exhausted`、`stop.requested`、`project-pending`）とGit local操作（`git worktree list`、`git for-each-ref`）だけで判定する。`gh api rate_limit` は呼ばない。

queued候補の順序が`claim_next`と食い違うと運用者を誤誘導するため、category/priority rankのjq式を`queue_rank_jq()`として1箇所に抽出し、`claim_next`と`status`の双方から呼ぶ（sort/awkパイプラインは各呼び出し側に残すが、rank式そのものはdriftしない）。

### 依存関係・queued候補はbest-effortの再現であり、claim判定そのものではない

`agent:blocked`のIssueは`agent:queued`ラベルから外れているため、`status`のqueued候補一覧には最初から現れない。したがって候補の「claimされない理由」は`scope-conflict`（`conflict/issue-<n>`）、`retry-cooldown`（`attempts/issue-<n>`のcooldown）、`claim-paused`（budget/exhaustion/stop marker）の3種のlocal state判定に限定し、`dependency_status()`のGitHub再検証は行わない。これは[0003](0003-supervisor-resilience-and-api-budget.md)のAPI予算方針を優先した明示的なトレードオフであり、`docs/operations/issue-queue.md`に文書化する。

`agent:stale`はclosedなため`status`の分類はGitHubの一覧APIを別途1回叩く必要があり、`--paginate`せず最大100件に切る。全件集計が必要になった場合は別Issueとする。

### 常に終了code 0、書き込みは常にゼロ

`status`は運用者が任意の頻度で叩く観測入口であり、合否判定は`doctor`の責務として分離済みである（[0002](0002-github-issue-queue.md)）。異常はテキスト・JSONどちらでも「警告」として列挙するだけで、終了codeは引数不正（2）以外は常に0にする。GitHub取得に失敗した場合も`status`自身は失敗させず、`github_available:false`を示してlocalの情報だけで応答する（degraded運用）。

`status`はGit作業ツリー・GitHub状態のいずれも変更しない。異常検知に使う`clear_stale_lock()`相当の判定は、mutationを行わない読み取り専用の`status_supervisor_lock_stale()`として別に実装し、本来のmutateする`clear_stale_lock()`とは共有しない。

### machine-readable出力は既存のdoctorのJSON流儀を継承する

`bin/agentic-loop status --format json`は`schema_version:1`の単一JSONを出す。文字列のエスケープは`doctor --format json`が使っていた`doctor_json_escape()`を`json_escape()`へ改名して共有し、依存を増やさない（`jq`はこのリポジトリの依存に無い。`yq`はテスト側の検証にのみ用いる）。

### 進行(stall)の観測はlocal progress markerと`status --watch`/`tail`で提供する

lease heartbeatは「プロセス生存」しか証明せず、API待ち・無限リトライで**stack（進行停止）**しているworkerは`status`上healthyに見え続ける（[0006](0006-worker-hang-timeout.md)は時間上限ベースの安全網であり、進捗そのものは判定しない）。この穴は、単一ホストのローカルCLIだけを対象に次の3点で埋める。

- **progress marker**: workerが`$STATE_ROOT/workers/<issue>.progress`に`<epoch>\t<stage>\t<seq>`の1行TSVを書く（Git管理外）。`stage`はenumのみ（`claim`/`resume`/`worktree`/`plan`/`exec`/`replan`/`checks`/`review`/`merge`/`cleanup`/`done`/`failed`/`needs-input`/`queued`/`blocked`/`stopped`）で、providerの自由文を永続化しない（secret境界）。`seq`は単調増加で、同一stageの短周期touchでも`tail`が変化を検知できる。workerはstage境界と、自ホストが制御を持つ区間（probe、scope refine、PR後処理、cleanup）でtouchする。**heartbeatループはprogressを更新しない**（生存と進行を分離し、stall workerが自分をhealthyに見せられないようにする）。
- **`status`のhealth帯と`--watch`**: Running Issues行とJSON `workers[]`に`stage`・`progress_at`・`progress_age_seconds`・`health`（`healthy`/`stalled`/`timeout`/`unknown`）を追加する。3帯はlocalのみで判定し、`timeout`は既存の`worker_timeout_seconds`超過（[0006](0006-worker-hang-timeout.md)）を優先する。stallは観測警告（`worker-stalled` anomaly）に留め、自動停止しない。`status --watch [N]`（既定2秒）は端末をtick更新し、色分けはTTYかつ`NO_COLOR`未設定のときだけANSIを出す。watch中のGitHub snapshot/staleは**メモリ上のTTL cache**（既定60秒）を再利用するため、refreshあたりのREST(core)読み取りは従来と同じ最大2回のままで、TTL内のtickはlocal state（workers/*、worker log mtime、supervisor pid）だけを読む。`AGENTIC_LOOP_WATCH_ITERATIONS`はテスト用にループ回数を制限する逃げ道である。
- **`tail`**: `$STATE_ROOT/events.log`（append-only、`epoch\tissue|supervisor\tcode\tstage-or-`、codeは`progress`/`claim`/`recover`/`timeout`/`stop`/`start`のenum）を時刻整形して流す読み取り専用コマンド（REST 0）。`--issue N`でフィルタ、`--follow`で追尾する。worker log本文・Issue本文は読まない・出さない。

`status`/`--watch`/`tail`はすべてGitHub・Git作業ツリーへ書き込まない。progress markerとevents.logはGit-common stateへのlocal書き込みであり、既存の`.started`/`.lease`等と同じ扱いとする。`clear_worker_local`は`.progress`も削除する（events.logはappend-onlyの監査跡として残す）。stall判定の副シグナルとしてworker logの**mtime**を採用する（本文は読まない）。provider待ちの長考中はstage境界のprogressが止まるため、mtimeが新しければhealthyとみなす。

## 影響

- `bin/agentic-loop`: `cmd_status`の全面書き換え、`json_escape`の共有化、`queue_rank_jq`の抽出、worker local stateの拡張（`.started`／`.lease`拡張／`.resume`）と`clear_worker_local`の追随、`status --watch`とTTL cache、`cmd_tail`、workerのprogress計装、Supervisorのevents.log遷移。
- `tests/test-agentic-loop.sh`: fake ghへのstatus snapshot・stale queryルーティング追加と、idle・複数worker・queued順序・needs-input・lease期限切れ・壊れたlocal state・残存worktreeのシナリオ追加、watch/tail/stall/REST予算/色なし/不明ホストのシナリオ追加。
- `docs/operations/issue-queue.md`: `status`の出力仕様、JSON schemaの主なキー、`--watch`/`tail`の使い方、`doctor`/GitHub Project Viewとの責務分担表を追記。

既存の`start`/`stop`/`status`の互換性（1行目の`running`/`stopped`文言、`Running Issues:`/`競合待ちIssue:`/`依存待ちIssue:`見出しと既存の表示項目）は変更しない。JSONは`schema_version`を1のままキーを追加する（後方互換）。

## 対象外

- running workerの強制停止・再開（別Issue）。
- **web/TUI dashboardや本格的な継続監視UI**。`status --watch`は単一ホストの端末更新とメモリTTL cacheに限定し、別マシン集約・時系列DB・リモート表示はしない。
- 履歴の永続化やメトリクス出力（`tail`のevents.logはaudit用途のappend-onlyであり、時系列メトリクス集計は[0007](0007-loop-metrics.md)の責務）。
- 他端末が担当するleaseをGitHubから読み直すremote参照モード（`worker-missing`は「不明」と表示するに留める。他ホストのprogressはGitHub共有しない）。
- stall検出に基づく自動停止・自動再queue（[0006](0006-worker-hang-timeout.md)の時間上限のみ。stallは観測警告に留める）。
- AIによるscope・優先度の推定。
