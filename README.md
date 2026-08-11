# agentic-loop-foundation

自然言語の要求を起点に、Agentが調査・変更・検証・修正を繰り返すための、言語非依存の最小プロジェクト基盤です。アプリケーションの技術選定は、実際の目的が与えられた時点で行います。

## 始める

必要なのは Git、Bash、標準的な Unix コマンドです。

```sh
# このリポジトリ自体を次のプロジェクトとして使う
git clone <repository-url> my-project
cd my-project
make check

# 既存ディレクトリへ原則を導入する
./bin/agentic-loop install /path/to/existing-project

# 空のディレクトリへ新規プロジェクトを作る
./bin/agentic-loop init /path/to/new-project
```

`install` は既存プロジェクトへ `AGENTS.md` だけを追加し、既存のツールチェーンへ干渉しません。既存の `AGENTS.md` があれば何も変更せず終了するため、内容を確認して手動で統合してください。`init` で作成したプロジェクトでは、対象ディレクトリで `make check` を実行してください。

## 開発コマンド

```sh
make format  # テキストファイルの末尾空白などを正規化
make lint    # 構成とシェル構文を静的検証
make test    # CLIを隔離した一時領域で検証
make check   # lintとtestを実行
```

新しい技術を採用したら、その技術固有の format / lint / test / build をこれらの入口へ接続します。環境変数の例は `.env.example` に安全なダミー値だけを記載し、実値はコミットしません。

## 要求の入力

Agentへ「このプロジェクトで達成したい目的、制約、完了条件」を自然言語で伝えてください。

設計上の判断は [docs/decisions/0001-minimal-foundation.md](docs/decisions/0001-minimal-foundation.md) に記録しています。
