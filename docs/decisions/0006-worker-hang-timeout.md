# 0006: 長時間ハングしたworkerを検出するper-workerタイムアウト

- 状態: 採用
- 日付: 2026-08-13

## 背景

Supervisorは lease heartbeat が生きている限りworkerを「生存」とみなす（[0003](0003-supervisor-resilience-and-api-budget.md)）。しかしheartbeatはworkerプロセスが生きている証明にすぎず、作業が進んでいる証明ではない。応答しないAPI待ちや無限リトライでworkerプロセスが生きたまま内部で長時間ハングした場合、heartbeatは継続してleaseは切れず、`recover_expired` でも回収されない。結果としてそのworkerがslotを無期限に占有し、`max_workers=1` ではキュー全体が停止しうる（#108）。実運用（#45監視中）でこの構造的な穴が認識されたが、今回はworkerが健全に進行していたため未発火であり、恒久的な安全網が無い状態だった。

## 判断

### Supervisor側のper-worker実行時間上限を主対策とする

判定と停止をハング当事者（workerを起動するbashプロセス）に委ねる案は、bash自身がハング（wedge）した場合に発火しない。Supervisor側に置けば、`setsid` で分離済みのworkerプロセスグループを既存のグレースフルシャットダウン（[0003](0003-supervisor-resilience-and-api-budget.md)）と同じ手法 `kill -TERM -$pid` で確実に落とせる。Supervisor自体が死んだ場合もsystemdが再起動し、起動時の回収処理（`enforce_worker_timeout`、`recover_expired` と同じ配置）で改めて判定される。

worker側にstage単位（plan/exec）のタイムアウトを別途持たせる案は今回採らない。二重のタイムアウトは責務と観測点を分散させ、どちらが実際に発火したかの切り分けを難しくする。Supervisor側の単一の安全網に統一する。

### 判定対象は自ホストのpidfileを持つworkerに限定する

`enforce_worker_timeout` はこのホストの `workers/<issue>.pid` が指すプロセスが生存している（`worker_pid_live`）Issueだけを対象にする。他ホストが担当するIssue（pidfileが無い）には一切触れない。複数端末で稼働する運用でも、各ホストが自分のworkerの実行時間だけを管理し、他ホストの正常なworkerを誤って停止しない。

### 超過時の処分は`agent:failed` + 既存の境界付き自動再試行

タイムアウトを検出したworkerはプロセスグループごと停止し、Issueを `agent:failed` にして監査コメントを1件残す。既存の `retry_failed` が `max_attempts`（既定3）と `retry_cooldown_seconds`（既定600秒）で自動的にqueuedへ戻し、上限に達した場合のみ「解決不能」としてcloseする。これにより、恒常的にハングするIssueが無限にclaim→ハング→requeueを繰り返してprovider予算を消費し続けることを防ぐ。[0004](0004-worker-resume-and-handoff.md)の再開機能により、途中までの成果物（plan・branch・PR）は次の試行でも無駄にならない。

`agent:queued` へ直接戻す案（Issue本文の期待動作の前段）ではなく`failed`経由を選んだのは、境界のある再試行と「解決不能」への収束を既存のリトライ機構に一本化し、タイムアウトのためだけに別の無限再試行経路を新設しないためである。

### 既定値は4時間、`0`で無効化

`queue.worker_timeout_seconds`（既定 `14400` 秒 = 4時間）を追加する。plan、execに加え `plan_max`（既定1）による再計画、PR checks待ちとreview対応まで含めた最悪ケースに十分な余裕を見た保守的な推定値である。実測値は各Issueの `agentic-loop:usage` コメントの `所要=Ns` で確認・調整できるため、運用ドキュメントに調整指針を記す。`0` は無効化（従来動作）で、既存導入先は設定変更なしで新しい既定値が有効になる。

`doctor` は値が負・非数値なら失敗、`0 < 値 < 1800秒`（`WORKER_TIMEOUT_MIN_SAFE_SECONDS`）なら「正常なworkerを誤停止する恐れがある」警告を出す（失敗にはしない。小さい値を意図的に許容する運用もあり得るため）。

## 却下した案

- **worker側のstage単位/全体タイムアウト**: 上記の通り、bash自身のハングに対して無力であり、Supervisor側の安全網と責務が重複する。
- **`recover_expired` のロジック変更（leaseの有効性判定自体を変える）**: leaseは「プロセス生存」の正本として[0003](0003-supervisor-resilience-and-api-budget.md)で確立済みであり、その意味を変えると他の判定（`issue_genuinely_running` 等）に影響が及ぶ。別の判定軸（実行時間）を追加する方が安全である。
- **タイムアウト超過時に直接`agent:queued`へ戻す**: 境界のない再試行経路を新設することになり、恒常的なハングを無限リトライさせてしまう。

## 帰結

- lease heartbeatが有効なまま実行時間上限を超えたworkerは、プロセスグループごと停止され（孤児化したprovider CLIプロセスを残さない）、`agent:failed` + 監査コメントを経て既存の境界付き自動再試行に合流する。
- `max_workers=1` の運用でも、ハングした1件が後続のqueued Issueのclaimを妨げなくなる。
- 上限内で正常に完了するworkerは一切停止されない。
- `status`（text/JSON）に `timeout_at` と超過警告 `worker-timeout` が現れ、運用者はGitHub Issueを見ずに超過を把握できる。

## 対象外

- 「上限内だが無進捗」のハング検出（本変更は時間ベースの安全網であり、進捗そのものの判定は行わない）。将来、stage単位の無進捗検出を追加する余地は残す。
- worker側のstage単位タイムアウトの追加。
- lease設計・`recover_expired` のロジック変更。
