# コードベース自己診断

## 目的

自己診断は、要件と実装の双方向のずれ、ディレクトリ構成、開発用skillの追加余地、古く未使用のファイルを定期的に監査する。診断中にコードを変更せず、検証できる改善点だけを `diagnosis`、`category:improvement`、`agent:queued` Label付きの日本語GitHub Issueとして記録し、既存のSupervisorへ修正を委譲する。

## 実行

`install.sh` はリポジトリごとのuser-level systemd timerを冪等に設定する。初回は起動後30分、その後は7日間隔、最大6時間のランダム遅延で実行する。選択中のprovider（後述）が持つ既存サブスクリプション契約のAI CLIと既存のGitHub認証だけを使い、API keyや追加の有料サービスは使わない。

手動診断はリポジトリルートで実行する。

```sh
bin/agentic-loop-diagnose
```

定期実行の状態と履歴は次で確認する。

```sh
systemctl --user list-timers 'agentic-loop-diagnosis-*'
journalctl --user -u 'agentic-loop-diagnosis-*'
```

同一リポジトリの診断はlockで直列化される。診断に使うAIツールは `.agentic-loop.toml` の `[agent.diagnose]`（未設定なら `[agent]`、既定 `codex`）で選択し、`[[agent.diagnose.tiers]]` を宣言した場合は `bin/agentic-loop _pick-tier diagnose` がプール・モデルの優先順位（[ADR 0012](../decisions/0012-provider-pool-fallback.md)）に従って選択する。Codexはread-only sandboxで変更を機械的に禁止し、Claudeやopencodeはsandboxを持たないためプロンプトの指示に依存する。いずれもコードを調査し、open・closedの既存Issueを検索して重複を避ける。Issueには問題、根拠、期待状態、受け入れ条件、対象パスを記載し、作成時に `diagnosis`、`category:improvement`、`agent:queued` を同時に付け、直後に `bin/agentic-loop sync-issue ISSUE_NUMBER` でProject同期を開始する。一時障害は再試行queueへ保存され、診断やIssue queueを停止しない。診断プロセスはコードを変更せず、worker数、lease、専用worktree、検証、secret guard、PR、CI、review、mergeは[Issueキュー運用](issue-queue.md)のSupervisorとworkerが担う。

`install.sh` の共有ファイル配布には、更新後の診断skill、運用文書、実行promptを含める。導入先に同名ファイルの差分がある場合は安全のため上書きせず停止するため、差分を確認・統合してから再実行する。
