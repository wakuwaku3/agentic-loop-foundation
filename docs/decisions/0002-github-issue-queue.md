# 0002: GitHub Issueをリポジトリ単位の要求キューにする

- 状態: 採用
- 日付: 2026-08-11

## 背景

要求をローカル端末の寿命から切り離して永続化し、同一リポジトリの複数要求を安全に並列処理したい。一方、外部DB、ホスティング、OpenAI API従量課金は費用ポリシーに反する。Projectはuserまたはorganization所有で、Issueと完全に同じ権限境界にはならず、ビューや組み込みworkflowの設定APIにも制約がある。

## 判断

GitHub Issueと `agent:*` Labelを処理状態の正本にする。リポジトリごとに一つのSupervisorだけをローカルで起動し、既定2 workerをIssue専用branch/worktreeで動かす。workerは公式の非対話モード `codex exec` を `workspace-write` sandboxと承認待ちなしで使う。OpenAI API keyとdanger-full-accessは使わない。

状態は `queued`、`running`、`needs-input`、`in-review`、`completed`、`failed` とする。runningにはworker ID、heartbeat、期限付きleaseをIssueコメントとして記録する。起動時に期限切れleaseをqueuedへ戻す。needs-input後のIssue返信は再取得対象へ戻す。Issueごとの失敗は他workerやキューを停止しない。

Projectはリポジトリ名を含む専用名で冪等に作成または再利用し、リポジトリへlinkする。可視化はbest-effortで再同期可能とし、Project障害でIssueの取得・状態遷移を止めない。Projectの所有者アクセスがリポジトリより広い場合があるため、機密情報はProjectへ複製せず、管理者がアクセス境界を確認する。

## 帰結

状態と履歴はGitHub上でリポジトリごとに分離され、中央キューや追加の有料基盤が不要になる。同一リポジトリの単一Supervisorによりclaim競合を避ける。複数端末で同時にSupervisorを起動することはサポートしないため、運用上も一台に限定する。Projectのビューと自動追加workflowをAPIで構成できない場合は、Agent statusフィールド、link、Supervisorのitem-addまでを自動化し、Issueを正本として継続する。
