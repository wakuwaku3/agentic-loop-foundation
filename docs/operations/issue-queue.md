# Issueキュー運用

## セットアップ

`install.sh` は変更前に `git`、`gh`、Codex CLI、GitHubログイン、origin、リポジトリ参照、Projects API権限を検査する。既存ファイルとの競合もコピー前に検査する。検査後、6個の状態Labelと `Agentic Loop - OWNER/REPOSITORY` Projectを冪等に用意し、既定ではSupervisorを起動する。

GitHub tokenには対象リポジトリのIssue/PR操作権限と `project`、`read:project` scopeが必要である。不足時は `gh auth refresh -s project,read:project` など、利用中のGitHub認証方式に合う方法で追加する。Projectはuser/org所有のため、対象リポジトリとProjectの閲覧者が一致することを管理者が確認する。privateリポジトリの内容や秘密情報をProjectフィールドへ転記しない。

Project APIが許す範囲でlink、`Agent status` single-select、Issue item追加を設定する。ビューとProject workflowのAPIが利用できない場合、Inbox、Queued、Running、Needs input、In review、Done、Failedで手動にてビューを作成できる。この障害はIssueキューを停止しない。

## 操作

```sh
bin/agentic-loop start
bin/agentic-loop status
bin/agentic-loop stop
```

利用者は要求をIssueとして登録し、`agent:queued` を付ける。登録・並べ替え・回答専用CLIはない。順序はGitHub Issue一覧の順序に従い、依存関係はIssue本文に明記する。回答は `agent:needs-input` のIssueへコメントする。

`.agentic-loop/config` で `POLL_SECONDS`、`MAX_WORKERS`、`LEASE_SECONDS`、`STOP_TIMEOUT` を変更できる。既定の並列数2をむやみに増やさない。増加はCodex契約上の制限、Git競合、端末資源を確認してから行う。stopは新規claimを止め、workerをdrainする。`STOP_TIMEOUT=0` は完了まで待つ。

## 状態と復旧

- queued: 未取得
- running: leaseを持つworkerが処理中
- needs-input: 不可逆・費用・重大な安全判断または解消不能な権限不足への回答待ち
- in-review: PR確認中（workerが進捗として使用可能）
- completed: 検証済みPRがmerge済み
- failed: mergeを証明できず終了。原因確認後にqueuedを付けて再試行する

Supervisorは起動時にrunning Issueの最新leaseコメントを読み、期限切れをqueuedへ戻す。Issue worktreeは対象リポジトリと同じ親ディレクトリの `<repository>-worktrees/issue-<number>` に分離する。workerは `workspace-write` を維持し、Gitが解決した対象リポジトリのcommon metadataディレクトリだけをCodex CLIの `--add-dir` で書き込み可能にする。workerの標準出力・標準エラーはGit管理外の `.git/agentic-loop/logs` に保存し、Issueへ転載しない。ログに秘密が疑われる場合は削除し、資格情報を失効する。Project同期は再実行可能であり、`bin/agentic-loop setup` で修復する。

Supervisorが停止している場合はstatus、`.git/agentic-loop/supervisor.log`、`gh auth status` を確認する。同じリポジトリを複数端末から処理しない。default branch更新後の競合やrequired checks失敗はworkerが最新branchに対して修正・再検証する。
