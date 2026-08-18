# 文書ポリシー

## 目的

このポリシーは、このリポジトリが持つすべての文書（README、利用者・開発者・運用者向けドキュメント、AI向け指示、policy、ADR、machine-readableな設定）に適用する。文書ごとの対象読者と責務境界を定義し、同じ事実を複数の文書へ重複させないことで、内部実装の変更が無関係な文書を陳腐化させない構造を維持する。

## 開発主体の前提

このリポジトリの開発主体は原則としてAgentic Loop（AI）である。人間はIssueによる要求の入力、破壊的・不可逆・重大costな判断への承認、PRのレビュー、例外時の手動確認・復旧だけを担う。したがって人間向けの開発文書には、AIへの実装指示・検証規則・不変条件を書かない。それらの正本は常に [AGENTS.md](../../AGENTS.md)、`docs/policies/` 配下の各policy、または `.agentic-loop.toml` や `.agentic-loop/capabilities.toml` のようなmachine-readableな設定であり、人間向け文書はそれらへリンクするだけにする。

## 読者と責務境界

| 文書 | 第一読者 | 責務（正本とするもの） |
| --- | --- | --- |
| `README.md` | 基本利用者 | 用途、導入、要求の入力、日常操作、問題時の参照先。これ以外の詳細はここへ書かず、案内リンクだけを置く |
| `docs/development.md` | 人間の開発者 | Agentic Loop（AI）が実装する前提で、人間しか担えない責務（要求の伝え方、意思決定・承認、レビュー、例外時の手動確認・復旧）だけを説明する |
| `docs/operations/` | 運用者 | 導入・運用・CLI詳細・トラブルシュートの正本 |
| `docs/policies/` | 規範の適用者（人間・AI共通） | 不変条件の正本 |
| `docs/decisions/` | 設計を追う人 | 設計判断（ADR）の正本。過去の決定を記録として保持し、書き換えない |
| `AGENTS.md` | AI（worker・Supervisor） | 原則、要求ルーティング、不変条件への参照 |
| machine-readableな設定（`.agentic-loop.toml`、`.agentic-loop/capabilities.toml`、`tests/impact-map.toml` 等） | ツール | 機械が読む規則の正本。散文の文書へ複製しない |

## README.mdの責務境界

README.mdは基本利用者だけを想定し、次を置かない。

- 開発・検証コマンド（`devbox run --pure`、`make check`、CIの詳細）
- 内部実装の詳細（`bin/lib/agentic-loop/*.sh` の構成、設定の全項目、systemd unit名）
- ADR（`docs/decisions/`）への参照
- Agentic Loop（AI）だけが必要とする作業手順

これらが必要な情報は `docs/development.md`、`docs/operations/`、`docs/policies/` のいずれかへ置き、README.mdからは案内リンクだけを張る。

## 重複防止と更新責務

同じ事実は1箇所にだけ書き、他の文書からは正本へのリンクで参照する。機能や設定を追加・変更するときの更新責務は次のとおりである。

1. その事実の第一読者に対応する文書（上表）を正本として1件だけ更新する。
2. README.mdや他の文書がその事実に触れる必要がある場合は、正本へのリンクを1行追加するだけにし、内容を複製しない。
3. README.mdはCLIの追加やAI向け実装の変更では更新しない（README.mdの責務境界に該当する情報を持たないため）。

## 機械検証

`scripts/lint.sh` が、README.mdの責務境界（開発・内部実装への言及がないこと）と必須要素（読者に必要な導線があること）、本ポリシーの必須要件、`docs/development.md` の責務範囲、および対象文書の配布（`scripts/lib/foundation-files.sh`）を検査する。`tests/test-agentic-loop.sh` のauxiliary群・upgrade群が、install/upgrade後の配布結果を検証する。
