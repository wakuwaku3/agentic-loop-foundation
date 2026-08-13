# 0003: Supervisorの障害耐性とGitHub API消費の構造的削減

- 状態: 採用
- 日付: 2026-08-13

## 背景

運用で次の事象が観測された。

- Supervisorが一時的なGitHub API障害（secondary rate limit / HTTP 403、5xx）に遭遇すると、`set -e` により失敗が伝播してSupervisorプロセス全体がクラッシュした。
- Supervisorがkill・急死した際、`&` で起動したworkerが親を失って孤児化し（initへ里子）、対応Issueは `agent:running` のまま滞留した。stale lockも残った。
- lease heartbeatを都度「新規コメント」で記録する設計が、GitHubのcontent作成を多発させてsecondary rate limitを誘発し、さらにコメント肥大が `recover_expired` の全コメント読み取りコストを押し上げる悪循環を生んだ。

不変条件として、[0002](0002-github-issue-queue.md)の通りGitHub Issueと `agent:*` Labelを状態の正本とし、将来のマルチマシン並列を閉ざさないため、生存・回復の正本をローカルへ移さない。費用ポリシー上、GitHub APIの無償枠を超えず、枯渇による停止を許容する。

## 判断

障害耐性・API削減・安全弁の3層で構成する。いずれもGitHubを正本に保ち、ローカルは補助（高速化）に限定する。

### 障害耐性

- **グレースフルシャットダウン**: SupervisorはSIGTERM/SIGINTを捕捉し、claimを止め、進行中Issueを安全にキューへ戻し、workerをプロセスグループごと停止し、lock/pid/leaseを解放してから終了する。`systemctl stop`・`kill`・OS/WSLの正常シャットダウン（SIGTERMが届く）はクリーンに畳まれ、孤児やphantom runningを残さない。
- **再起動時リカバー**: 急死・電源断・WSLクラッシュのようにSIGTERMが届かない場合は、次にSupervisorが起動した時点で回復する。`agent:running` のうちleaseが期限切れのものをqueuedへ戻す（マシンをまたいだ回復の基本）。加えて、leaseがこのマシンのworkerに属し、そのworkerがローカルで死んでいる場合は期限切れを待たず即時にqueuedへ戻す（他マシンのleaseには触れないので並列安全）。この回復は起動時とpoll毎の双方で行う。
- **安いlease**: heartbeatは新規コメントを作らず、Issueごとに1つのleaseコメントを更新（PATCH）で打ち直す。Issueは肥大せず、`recover_expired` は1コメントの読み取りで済む。heartbeat間隔はマシン跨ぎ調整に十分な粒度まで延ばす。

### API削減

- **アイドル時backoff**（実装済み）: queuedが空でworkerも居ない間はpoll間隔を `poll_seconds` から `poll_max_seconds` まで指数的に延ばし、workerが動き出したら通常間隔へ戻す。アイドル時のpoll読み取り回数を大きく削減する。
- **ETag条件付き取得**（保留）: 取得に `If-None-Match` を付け304（rate limit非消費）を狙う案。`gh` CLIは条件付きリクエストとETag/304の受け渡しを素直に扱えず実装が脆くなるため見送る。安いlease（C）・APIバジェット・ガバナー（E）・アイドルbackoff（F）でAPI消費は十分に抑えられており、必要になれば専用HTTPクライアントで再検討する。

### 安全弁

- **APIバジェット・ガバナー**: REST/GraphQLの残量に加え、secondary rate limit対策としてcontent作成を律速するローカルtoken-bucketを一元管理する。残量が予備閾値を下回る間はclaimと非必須の書き込みを自動停止し、回復で再開する（tokenの[費用ガード](../operations/issue-queue.md)と同じ思想のGitHub API版）。403等が返す `Retry-After` を厳守し、叩き続けない。
- **poll処理の耐性化**: poll内のbest-effortなメンテナンス（recover・reconcile・triage・retry・lease・project同期）は単一の失敗でSupervisorを落とさず、そのpollをスキップして次pollで再試行する。

## 却下した案

- **lease・生存判定の完全ローカル化**: 単一マシンでは安いが、マシンをまたいだ調整点を失い将来のマルチマシン並列を閉ざす。安いleaseコメント（PATCH）で目的を達しつつGitHub正本を保つ方を採る。
- **ローカルDB（SQLite等）を正本にする**: 同様にマルチマシンを閉ざし、[0002](0002-github-issue-queue.md)の「GitHub Issueを正本」原則に反する。

## 帰結

- アイドル時はheartbeatが無く、pollもbackoffで疎になるためAPI消費が大きく下がる。稼働時も書き込みは状態遷移とIssueごと1つのlease更新（PATCH）に限られ、secondary rate limitの主因が消える。
- 万一枯渇へ近づいてもガバナーが予備を残して自動停止し、Retry-Afterを守り、耐性化により落ちない。
- Supervisorがkill・急死しても、正常時はグレースフルに、異常時は次回起動時にクリーンへ収束する。生存・回復はGitHub leaseを正本に保つため、マルチマシン並列の可能性は損なわれない。
