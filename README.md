# agentic-loop-foundation

自然言語の要求を起点に、Agentが調査・変更・検証・修正・PRのマージまでを反復するための、言語非依存の最小プロジェクト基盤です。

## 開発と検証

Devboxを導入した `x86_64-linux` または `aarch64-linux` で、次の共通入口を使用します。`devbox.lock`によりBash、Makeその他の検証ツールが固定され、CIも同じ環境とコマンドを使用します。

```sh
devbox run --pure check
```

## インストール

導入したいディレクトリへ移動し、次の1コマンドを実行します。

```sh
curl -fsSL https://raw.githubusercontent.com/wakuwaku3/agentic-loop-foundation/main/install.sh | bash
```

対象はoriginを持つGitリポジトリである必要があります。空のGitリポジトリにはDevboxによる固定開発環境を含む完全な基盤を作成し、既存プロジェクトには既存のコード化済みツールチェーンを維持したままAgent原則、要求入力Skill、IssueキューCLI、Git hooksによる機密情報ガードを追加します。既存プロジェクトにコード化済み環境がない場合は、導入後の変更を完了する前に追加する必要があります。`git`、`gh`、Codex CLI、GitHub認証とProjects権限を変更前に検査し、既存ファイルやhooks設定との競合時は上書きせず停止します。

インストールは対象リポジトリ専用のLabelsとGitHub Projectを冪等に設定し、Supervisorをバックグラウンド起動します。また、リポジトリごとのuser-level systemd timerを有効化し、15分間隔（最大2分のランダム遅延あり）でローカルの`main` worktreeを`origin/main`へ追従させます。更新はcleanかつlocal `main`が`origin/main`のancestorである場合のfast-forwardだけに限定され、ローカル変更、先行、分岐があれば何も変更せず失敗します。登録状態は`systemctl --user list-timers 'agentic-loop-main-sync-*'`、実行履歴は`journalctl --user -u 'agentic-loop-main-sync-*'`で確認できます。

GitHub Issueが要求と状態履歴の正本で、Projectは障害がキューを止めない可視化層です。中央キューや外部DB、OpenAI API keyは使いません。

インストールはコードベース自己診断のuser-level systemd timerも設定します。週次診断は要件と実装のずれ、構成、skill候補、不要ファイルを調べ、コードを変更せず `diagnosis` と `agent:queued` Label付きの日本語Issueを作成してSupervisorへ修正を委譲します。手動実行は `bin/agentic-loop-diagnose`、詳細は [コードベース自己診断](docs/operations/codebase-diagnosis.md) を参照してください。

## 要求の入力

Codexで `$submit-requirement` に続けて、達成したいことを自然言語で入力してください。

インストールされる `.codex/config.toml` により、Codex は承認確認なしで動作し、ファイルの書き込み先はワークスペース内に制限されます。既存の `.codex/config.toml` がある場合は上書きせず、インストールを停止します。

> `$submit-requirement 商品を検索できるWebアプリを作って`

継続処理する要求は対象リポジトリのIssueに `agent:queued` Labelを付けます。制御用CLIは次の3つだけです。

```sh
bin/agentic-loop start
bin/agentic-loop stop
bin/agentic-loop status
```

既定では30秒poll、最大2件を並列実行します。設定、状態遷移、lease復旧、Projectの制約、トラブルシュートは [Issueキュー運用](docs/operations/issue-queue.md)、設計上の判断は [0001](docs/decisions/0001-minimal-foundation.md) と [0002](docs/decisions/0002-github-issue-queue.md) に記録しています。
