# 0029: GitHub上でrunningでないlocal workerをgrace付きで自動回収する

- 状態: 採用
- 日付: 2026-08-19

## 背景

`recover_expired`（[0003](0003-supervisor-resilience-and-api-budget.md)）は `agent:running` のIssueだけを対象に、workerの終了・lease期限切れを回収する。`enforce_worker_timeout`（[0006](0006-worker-hang-timeout.md)）はこのホストのpidfileが指すworkerを対象に、`worker_timeout_seconds`（既定4時間）を超えて実行中のものを停止する。

この2つの間に空白がある。worker-orphan、すなわち「このホストで生存しているlocal worker（pidfile）が、GitHub上では `agent:running` ではない」状態（実例: Issue #132、`codex exec` が生存しつつGitHub上は `agent:queued`）は、`recover_expired` の対象外（GitHub上runningでないため）であり、`enforce_worker_timeout` の対象にもならない（`worker_timeout_seconds` に達するまで動き続ける）。結果として、ラベルがrunningから外れたのにlocal providerプロセスが生き残ったorphanは、最大4時間そのまま動作し続け、retry-cooldown明けにSupervisorが同一Issueを再claimすれば二重workerが起動しうる。`status`はこれを`worker-orphan`警告として観測のみ行っていた。

## 判断

### 実行時間ではなくGitHub Labelの不一致を判定基準にする

`enforce_worker_timeout` と同じ「このホストのpidfileが生存している」対象範囲を再利用しつつ、判定基準を実行時間ではなく「GitHub上 `agent:running` でない」ことに変える。これにより、`worker_timeout_seconds` を待たずに、ラベルの矛盾を検出した時点を起点とした短い猶予だけでorphanを回収できる。

### grace: 初回検出では殺さず、経過時間の永続化で連続確認する

正常終了するworkerも、ラベルをcompleted/queued/failedへ更新してからprocess groupが抜けるまでの短い窓では一瞬orphanに見える。この窓で殺すとworktree cleanup等の後処理を破壊しうる。そこで初回にorphanと判定したpollでは `workers/<issue>.orphan-since` へ検出epochを書くだけで何もせず、以降のpollで同じIssueが引き続きorphanと判定され、かつ経過秒数が `queue.worker_orphan_grace_seconds`（既定120秒、`0`で無効化）に達して初めて停止する。Issueがrunningへ戻る、またはpidfileのプロセスが消えれば、このmarkerは次のpollで消去され判定はリセットされる。カウンタをpoll回数ではなく経過秒数で永続化するのは、`poll_seconds` の設定値に関わらず猶予の意味が変わらないようにするためである。

### 停止手順は`enforce_worker_timeout`と同じprocess-group TERM→KILLを再利用する

`setsid` で分離済みのworker process groupに対して `kill -TERM -$pid`、5秒待って生存していれば `kill -KILL -$pid` する。provider CLIの子プロセスを孤児化させない。停止後は `lease_release` / `clear_worker_local` / `scope_cache_clear` / `clear_conflict_wait` でlocal stateを掃除する。

### Issueのラベルは変更しない

orphanと判定される時点で、GitHub上のラベルは既に `agent:running` ではない何らかの状態（queued/failed/completed/paused等）である。このラベル自体は別の経路（`recover_expired`、`retry_failed`、通常のworker終了処理等）が既に正しく設定済みという前提に立ち、本変更はlocal processとlocal stateの掃除だけを行う。ラベルを動かす既存の安全網（`recover_expired` の枯渇判定・attempts上限判定等）と責務が重複・競合しないようにするためである。監査のため `agentic-loop:worker-orphan-reaped` コメントを1件残す。

### running集合の取得に失敗した場合は何もしない（fail safe）

判定対象のpidfileごとに「GitHub上runningか」を確認するのではなく、開いている `agent:running` Issue全体の集合を1回取得し、その集合にpidfileのIssue番号が含まれるかで判定する。この集合の取得に失敗した場合、空集合として扱うと生存中の正当なworkerまで誤ってorphan判定してしまう。取得に失敗したpollは何もせず次回に委ねる。Supervisorが既にそのpollで取得済みのsnapshot（`refresh_supervisor_snapshot`）があればそれを再利用し、無ければ（起動直後等）専用のGitHub呼び出しを1回行う。この取得は`running_issue_numbers`（`worker_state.sh`）としてpoll単位でmemo化し、同じ集合を必要とする`recover_expired`と共有する。`clear_supervisor_snapshot`がsnapshotと同時にmemoをリセットするため、poll毎に高々1回のGitHub呼び出しに留まる（従来は`recover_expired`と本機構がそれぞれ独自に取得しており、snapshot不在のpollで同じ一覧を2回取得していた）。

### 正常終了のcritical section中、およびpause/abortの協調停止drain中は殺さない

`worker.sh`の`worker_critical_begin`/`worker_critical_end`で囲まれた区間（completedへの状態更新からIssue closeまで）は、GitHub上のラベルが変わった後もこのホストのprocess自体はまだ動作している。ここでgrace超過を理由に殺すと、closeやpostmortemトリガ等の後処理が中断される。同様に、`control.sh`の`control_drain_local_worker`（pause/abort）は`worker_request_stop`でstop-requestマーカーを立ててから最大`PAUSE_GRACE_SECONDS`（既定120秒）を協調停止に費やす区間があり、ここで`control_checkpoint`（resume用の状態記録）が書かれる前に殺すと、resumeがpre-pause状態を復元できなくなる。`reap_orphan_workers`は停止直前に`worker_critical_active`または`worker_stop_requested`を確認し、いずれかが真であればそのpollは何もせず（`orphan-since`マーカーは消さない）、次のpollで再評価する。両区間とも有限（`worker_critical_end`／drainの終了、または最終的に`enforce_worker_timeout`）で終わるため、恒久的に停止されないことはない。

### stale `orphan-since` マーカーはgraceを再計上する

同じIssue番号が再claimされて新しいworkerが起動した直後に、前回worker（crash等で`clear_worker_local`が走らなかった）が残した`orphan-since`マーカーが残っていると、新workerがgraceを一切消費せずに即座にkill判定へ入ってしまう。`workers/<issue>.started`（現worker開始epoch）より古い`orphan-since`は無効とみなし、現在epochで書き直してgraceを再計上する。

## 却下した案

- **poll回数ベースのgrace（N回連続検出）**: `poll_seconds` の設定値によって実質的な猶予時間が変わってしまい、運用ごとに意味が変わる。経過秒数の永続化の方が設定間で一貫する。
- **orphan検出時にIssueのラベルも同時に補正する**: ラベルは既に他の安全網が正しく設定済みという前提が成り立たない状況（未知の不整合）まで本変更の責務に含めると、既存の状態遷移ロジックと競合しうる。local processとstateの掃除だけに限定する。
- **`worker_timeout_seconds` の判定に統合する**: 実行時間とLabel不一致は別の signal であり、[0006](0006-worker-hang-timeout.md)が明示的に「二重のタイムアウトは責務と観測点を分散させる」として単一の安全網に統一する方針を採っている。Label不一致は実行時間と独立した別の安全網として追加する方が、どちらが発火したかの切り分けが容易になる。

## 帰結

- GitHub上 `agent:running` でない状態が `worker_orphan_grace_seconds` を超えて持続したこのホストのworkerは、`worker_timeout_seconds`（既定4時間）を待たずにprocess groupごと安全に停止され、local stateが掃除される。
- 正常終了の一過性の不整合（ラベル更新後、process終了までの短い窓）はgraceの範囲内であれば誤って停止されない。
- 他ホストが担当中（GitHub上running）のIssueには一切触れない。
- `status`の`worker-orphan`警告は、次回pollでの自動停止までの残り猶予を表示するようになる。
- 正常終了のcritical section、pause/abortの協調停止drain、および再claim直後のstaleマーカーの3つは、いずれもgrace超過を理由に誤って停止されない。
- `queue.worker_orphan_grace_seconds`はキー未記載の既存installでも既定120秒でこの機構を有効化する。`worker_timeout_seconds`等の既存キーと同様、この既定値の追加のみでは`.agentic-loop.toml`のupgrade migrationを必要としない。
