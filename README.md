# agentic-loop-foundation

自然言語の要求を起点に、Agentが調査・変更・検証・修正・PRのマージまでを反復するための、言語非依存の最小プロジェクト基盤です。

## できること

GitHub Issueへ要求を書くと、Agentic Loopが調査・実装・検証・PR作成から、CIとレビュー対応、mainへのmergeまでを自律的に反復します。要求処理に使うAIコーディングツールはCodex、Claude Code、opencodeから選べ、特定のツールに固定されません。GitHub Issueが要求と状態履歴の正本で、中央キューや外部DBは使いません。

## 導入

前提として、Devboxを導入した `x86_64-linux` または `aarch64-linux`、originを持つGitリポジトリ、GitHub認証（`gh`）、いずれかのAI CLI（Codex／Claude Code／opencode）が必要です。導入したいディレクトリで次の1コマンドを実行します。

```sh
curl -fsSL https://raw.githubusercontent.com/wakuwaku3/agentic-loop-foundation/main/install.sh | bash
```

空のGitリポジトリには固定開発環境を含む完全な基盤を作成し、既存プロジェクトには既存のツールチェーンを維持したまま要求入力・Issueキュー・secretガードを追加します。導入後は、要求を処理する常駐Supervisor、GitHub上の状態管理（Labels/Project）、mainブランチの定期自動同期、コードベースの週次自己診断が有効になります。詳細は [Issueキュー運用](docs/operations/issue-queue.md)を参照してください。

## 要求を出す

`$submit-requirement`（Codex）または `/submit-requirement`（Claude）に続けて、達成したいことを自然言語で入力してください。

> `$submit-requirement 商品を検索できるWebアプリを作って`

## 日常の操作

```sh
bin/agentic-loop status
bin/agentic-loop status --watch
bin/agentic-loop tail
bin/agentic-loop doctor
```

`status` は今の実行状況とキューの見通しを1つの入口にまとめます。`status --watch` は継続表示、`tail` はイベント履歴の表示です。全CLIの一覧と使い方は [Issueキュー運用](docs/operations/issue-queue.md)を参照してください。

## 困ったときは

まず `bin/agentic-loop doctor` で環境の健全性を診断します。1件のIssueだけ止めたい場合は `bin/agentic-loop pause`、打ち切りたい場合は `bin/agentic-loop abort` を使います。Foundation自体の更新は `bin/agentic-loop upgrade` で確認してから適用します。詳細な復旧手順は [Issueキュー運用](docs/operations/issue-queue.md)と[導入済みFoundationのupgrade](docs/operations/upgrade.md)を参照してください。

## 文書の案内

- 開発（人間が担う責務）: [docs/development.md](docs/development.md)
- 運用（導入・CLI・トラブルシュート）: [docs/operations/](docs/operations/)
- 規範（不変条件・文書ポリシーを含む）: [docs/policies/](docs/policies/)
- AI向け指示: [AGENTS.md](AGENTS.md)
