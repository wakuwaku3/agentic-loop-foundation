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

インストールは対象リポジトリ専用のLabelsとGitHub Projectを冪等に設定し、Supervisorをuser-level systemd serviceとして起動します。Supervisorは予期しない終了後に自動再起動し、Issueキューのcore操作にはREST APIを使うため、GraphQL quota枯渇中もProjects同期だけを抑制して処理を継続します。また、リポジトリごとのuser-level systemd timerを有効化し、15分間隔（最大2分のランダム遅延あり）でローカルの`main` worktreeを`origin/main`へ追従させます。更新はcleanかつlocal `main`が`origin/main`のancestorである場合のfast-forwardだけに限定され、ローカル変更、先行、分岐があれば何も変更せず失敗します。登録状態は`systemctl --user list-timers 'agentic-loop-main-sync-*'`、実行履歴は`journalctl --user -u 'agentic-loop-main-sync-*'`で確認できます。

GitHub Issueが要求と状態履歴の正本で、Projectは障害がキューを止めない可視化層です。中央キューや外部DB、OpenAI API keyは使いません。

インストールはコードベース自己診断のuser-level systemd timerも設定します。週次診断は要件と実装のずれ、構成、skill候補、不要ファイルを調べ、コードを変更せず `diagnosis` と `agent:queued` Label付きの日本語Issueを作成してSupervisorへ修正を委譲します。手動実行は `bin/agentic-loop-diagnose`、詳細は [コードベース自己診断](docs/operations/codebase-diagnosis.md) を参照してください。

## AIツールの選択

要求処理のworkerが使うAIコーディングツールは `.agentic-loop.toml` の `[agent]` で選択します（`codex`、`claude`、または `opencode`、既定は `codex`）。同じリポジトリをローカル環境に応じて切り替えられ、特定のツールへ固定しません（[AIツール非依存ポリシー](docs/policies/ai-tool-neutrality.md)）。

各Issueは2段で処理します。まず調査と計画だけを行う高品質な**plan段**（Codexは read-only sandbox）、続いて計画に従って実装・検証・PR・mergeまで行う低コストな**exec段**です。高コストな推論を計画に集中させ、実作業は安価に回します。exec段が完了条件を満たせない場合は、`[agent.retry].plan_max`（既定1回）まで flagship で計画を見直して再実行します。

**局面ごとに provider・model・reasoning effort を指定できます。** 例えばplanはCodexのフラグシップを高effortで、execはopencodeで、のように混在させられます。段が provider を省略すると `[agent].provider` を継承します。`reasoning_effort` はCodexのみ（既定 plan=`high` / exec=`low`）、opencodeのmodelは `provider/model` 形式です。

```toml
[agent]
provider = "codex"            # 既定provider

[agent.plan]
provider = "codex"
model = "gpt-5-codex"
reasoning_effort = "high"

[agent.exec]
provider = "opencode"
model = "anthropic/claude-sonnet-4"
```

個人環境の上書きは git 管理外の `.agentic-loop.local.toml` に同じキーを書けば、キー単位で優先されます（例: 手元ではexecもcodexにする）。

Claudeとopencodeは（Codexと異なり）OS levelのsandboxを持たないため、書き込みの隔離は専用worktreeと秘密情報guard hookに依存します。opencodeは `opencode run --auto --format json --dir <worktree>` で作業ディレクトリに限定して実行し、step-finishイベントからtoken/コストを記録します。設定を変えたら `bin/agentic-loop start`（またはSupervisor再起動）で反映します。

## 要求の入力

`$submit-requirement`（Codex）または `/submit-requirement`（Claude）に続けて、達成したいことを自然言語で入力してください。

インストールされる `.codex/config.toml` により、Codex は承認確認なしで動作し、ファイルの書き込み先はワークスペース内に制限されます。既存の `.codex/config.toml` がある場合は上書きせず、インストールを停止します。

> `$submit-requirement 商品を検索できるWebアプリを作って`

継続処理する要求は対象リポジトリのIssueに `agent:queued` Labelを付けます。制御用CLIは次の4つです。

```sh
bin/agentic-loop start
bin/agentic-loop stop
bin/agentic-loop status
bin/agentic-loop doctor
```

`doctor` はGitHub認証とrepository権限、origin/default branch、選択中のAI CLI（Codex/Claude）、Devbox、hooks、Supervisor、systemd service/timer、GitHub Project、設定、残存worktree/branch/logを読み取り専用で検査します。各結果を成功・警告・失敗に分類し、影響と復旧方法を日本語で表示します。必須条件の失敗がある場合だけ終了code 1、警告だけなら0です。自動監視では `bin/agentic-loop doctor --format json` を使用できます。診断はtoken本体を表示せず、修復は `setup`、`start`、install再実行などの明示的な別操作で行います。

既定では30秒poll、最大4件を並列実行します。運用値は root 直下の `.agentic-loop.toml`（TOML、`yq`で読み取り）の `[queue]` セクションで設定し、個人環境の上書きは git 管理外の `.agentic-loop.local.toml` にキー単位で書けます。設定、状態遷移、lease復旧、Projectの制約、トラブルシュートは [Issueキュー運用](docs/operations/issue-queue.md)、設計上の判断は [0001](docs/decisions/0001-minimal-foundation.md) と [0002](docs/decisions/0002-github-issue-queue.md) に記録しています。
