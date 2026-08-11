# agentic-loop-foundation

自然言語の要求を起点に、Agentが調査・変更・検証・修正・PRのマージまでを反復するための、言語非依存の最小プロジェクト基盤です。

## インストール

導入したいディレクトリへ移動し、次の1コマンドを実行します。

```sh
curl -fsSL https://raw.githubusercontent.com/wakuwaku3/agentic-loop-foundation/main/install.sh | bash
```

空のディレクトリには完全な基盤を作成し、既存プロジェクトには既存のツールチェーンを維持したままAgent原則、要求入力Skill、Git hooksによる機密情報ガードを追加します。既存ファイルやhooks設定との競合時は上書きせず停止します。

## 要求の入力

Codexで `$submit-requirement` に続けて、達成したいことを自然言語で入力してください。

> `$submit-requirement 商品を検索できるWebアプリを作って`

設計上の判断は [docs/decisions/0001-minimal-foundation.md](docs/decisions/0001-minimal-foundation.md) に記録しています。
