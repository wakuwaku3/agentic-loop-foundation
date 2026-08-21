# 0031: stage別・設定可能なstall閾値と、claude execのstream-json化による進行signal

- 状態: 採用
- 日付: 2026-08-21

## 背景

`status`のstall判定（[0005](0005-observability-and-notification.md)）は、進行markerが `STATUS_STALL_SECONDS`（固定300秒、stage共通）を超えて更新されないrunning Issueを`stalled`とし、`worker-stalled`要対応anomalyに出す。しかし実測（Issue #273のコメント）では、claude execの1呼び出しだけで410秒・1298秒・2720秒かかるケースがあり、いずれも300秒を超える。これらは`worker_timeout_seconds`（既定14400秒）の1/48程度で誤って要対応として通知され続け、真に停止したworkerとの区別がつかない。

進行signalそのものにも構造的な限界がある。`status_progress_health`は`workers/<issue>.progress`（stage境界でのみ更新）と`worker_log_mtime`（stage境界のログのみ）の2つしか見ていない。claude execは`--print --output-format json`で単一JSONを最後にまとめて出す非streaming呼び出しのため、exec中は`${result_file}.raw.$$`が実行時間中ずっと0バイトのままで、exec内部に進行signalが原理的に存在しない。

## 判断

### stall閾値をstage別・設定可能にする

`STATUS_STALL_SECONDS`という単一の固定値を廃止し、`STALL_SECONDS`（既定300秒）と`PROVIDER_STALL_SECONDS`（既定3600秒）の2つに分ける。`status_stall_threshold`はstageが`plan`/`exec`/`replan`（1回のprovider呼び出しに専念するstage）であれば`PROVIDER_STALL_SECONDS`を、それ以外（`claim`/`worktree`/`checks`/`merge`等の短時間stage）であれば`STALL_SECONDS`を返す。3600秒は実測最大2720秒を上回り、かつ`worker_timeout_seconds`（既定14400秒）の1/4で、真のstallを自動停止の3時間前に検出できる。両方とも`.agentic-loop.toml`の`[queue]`から`stall_seconds`/`provider_stall_seconds`として設定可能にし、`0`で当該bandのstall判定を無効化する（既存の`worker_timeout_seconds`等と同じ「0で無効」規約）。

`status --format json`の`workers[]`に`stall_threshold_seconds`（実際に適用された閾値）を追加し、`worker-stalled`anomalyの本文にstageと適用閾値を含める。分類は変更しない（下記「分類は緩和しない」を参照）。

### claude execをstream-json化し、進行signalを作る

`claude --print --output-format json`を`--print --output-format stream-json --verbose`に変える（実CLIで直接確認: `--verbose`なしのstream-jsonは`Error: When using --print, --output-format=stream-json requires --verbose`で即exit 1になる）。stream-jsonはJSONLで、終端の`{"type":"result",...}`eventが現行の`--output-format json`envelopeと同一形状であるため、`.result`抽出・`is_error`/`api_error_status`判定・usage抽出は既存ロジックのまま「終端event 1行」に対して適用できる。

`agent_claude_stream_terminal_event`が生rawから最後の`"type":"result"`行だけを抽出し、それ以降の解析はすべてこの1行（`.final.$$`）から行う。**rawストリーム全体をresult_fileへコピーする経路は作らない**: streamには`{"type":"rate_limit_event",...}`のような非終端eventが混ざり、これをそのまま`result_file`へ入れると`agent_result_is_pool_exhausted`の`rate.?limit`正規表現に必ず一致し、成功したstageが単にrate limitについて論じているだけでもpool枯渇に誤分類する（Issue #158/#226と同種の回帰）。非JSON行（実測: MCP由来のログ混入）が混ざっても、終端eventの抽出は「`.type == "result"`の最終行」を拾うだけなので壊れない。終端eventが存在しない場合（killed mid-run等）は`result_file`を空のまま残し、既存の`[[ ! -s $result_file && -s $stderr_file ]]`という stderr fallbackへ委ねる（stream化前と同じ後方互換の失敗経路）。

`status`の副signalを`worker_log_mtime`から`worker_stage_output_mtime`へ拡張する。plan/execのresult file（`issue-N-plan.txt`/`issue-N-result.txt`）とその`.raw.*`/`.final.*`を含めた最新mtimeを見る。中身は一切読まず、mtimeだけを見るため、secret境界（[0005](0005-observability-and-notification.md)が定めた「providerの出力内容は決して漏らさない」原則）は変わらない。

`claude_probe_usage_percent`（Issue #251）と`agent_provider_probe_once`は`--output-format json`のまま据え置く。probeは単発で、streaming化する動機（長時間exec中の進行signal）が無い。

### stage開始時に前回の`.raw.*`/`.final.*`残骸を削除する

`worker_timeout_seconds`超過やorphan reapでprocess groupごと停止されたstageは、`agent_run_stage`自身の末尾cleanupを経由しないため`${result_file}.raw.<pid>`が残り得る（Issue本文の実測にも`issue-222-result.txt.raw.51731`の残存が記録されている）。stream化で書き込み量が増える分、放置すれば増え続ける。`agent_run_stage`の冒頭で、同一`result_file`prefixの`.raw.*`/`.stderr.*`/`.final.*`を無条件に削除してから実行する。disk占有は「実行中のstage 1本ぶん」に固定される。

### `plan-failed`をPROGRESS_STAGESに追加する

`worker.sh`のplan失敗terminalパスは`progress_touch "$issue" plan-failed`を呼ぶが、`plan-failed`が`PROGRESS_STAGES`のenumに無かったため`progress_write`が`return 1`で静かに捨てていた（markerもeventも書かれない）。plan失敗時の最後のmarkerが古い`plan`のまま取り残され、stall判定の入力を悪化させていた。`PROGRESS_STAGES`へ追加する（`metrics.sh`はusage markerのstageだけを読み、events.logのstage enumには依存しないため影響なし）。

### 分類は緩和しない（`worker-stalled`は引き続き要対応）

`worker-stalled`は真の停止を早期発見するための要対応通知であり、[0027](0027-exhaustion-hardening.md)のpostmortem（#250: 長時間停止が要対応として浮上しなかった）が示す教訓のとおり、これを`recovering`へ降格させると再発リスクになる。誤警報の根本原因は「閾値が単一・固定で長時間exec呼び出しに合っていなかった」ことであり、stage別・設定可能な閾値と進行signalの拡張で構造的に解消される。分類自体を緩めることは選ばない。代わりに、要対応通知の本文へstage・適用された閾値・（`worker_timeout_seconds>0`なら）自動停止の見込みを載せ、運用者が閾値の妥当性を判断しやすくする。

## 却下した案

- **claudeのusage率（`rate_limit_info.utilization`）を`agent_provider_usage_percent`の実測源にする**: streamに含まれる`rate_limit_event.rate_limit_info`は将来の実測源になり得るが、[0027](0027-exhaustion-hardening.md)の枯渇判定ロジックを書き換える別の要求であり、本Issueのscope外とする。
- **heartbeatを進行の証拠に使う**: `worker_state.sh`の既存コメント（"heartbeat only proves the worker process is alive, not that it is making progress"）どおり、liveness/progressの独立性は`worker_timeout_seconds`（[0006](0006-worker-hang-timeout.md)）の設計根拠そのものであり、崩さない。
- **`worker_timeout_seconds`自体をstage別にする**: 自動停止は[0006](0006-worker-hang-timeout.md)の責務であり、本Issueは観測（`status`のstall判定）のみを対象にする。自動停止のstage別化は別の意思決定を要する。

## 帰結

- `plan`/`exec`/`replan`の進行が300秒〜`provider_stall_seconds`未満のとき、`status`は要対応`worker-stalled`を出さず`healthy`のまま表示する。
- `claim`/`worktree`等の短時間stageは引き続き既定300秒でstall検出する。
- `.agentic-loop.toml`の`stall_seconds`/`provider_stall_seconds`は`0`でそのband自体を無効化できる。
- claude execがstream-json化されたことで、実行中の`.raw.*`のmtimeが進行signalとして`status`から見えるようになる（ただし逐次flushの実際の挙動はCLI実装依存であり、確認できなくてもstage別閾値だけで受け入れ条件は満たされる設計にしてある）。
- `.raw.*`/`.final.*`の残骸はstage開始時に削除され、disk占有は実行中stage1本ぶんに固定される。
- plan失敗時に`plan-failed`のprogress markerとeventが実際に記録される。
- キー未記載の既存installは既定値（300秒/3600秒）で動作し、upgrade migrationは不要（[0029](0029-worker-orphan-reap.md)の`worker_orphan_grace_seconds`と同様、既定値付きkeyの追加のみのため）。

追補（Issue #280、[0032](0032-time-constant-invariants-and-calibration.md)）: この`stall_seconds`/`provider_stall_seconds`と`worker_timeout_seconds`等の関係は、当初この文書のdoctor警告3件だけで検査されていたが、既定値そのもの・他の時間定数（lease/heartbeat、pause_grace、poll backoff、枯渇backoff、orphan grace）・実測分布との整合は未検査だった。0032でlint hard gateと実測calibrationに拡張した。
