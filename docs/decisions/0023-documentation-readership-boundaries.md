# 0023: 文書の対象読者と責務境界を規定し、READMEを基本利用者向けに絞る

- 状態: 採用
- 日付: 2026-08-19

## 背景

README.mdは開発・検証コマンド（`devbox run --pure check`）、導入時の運用詳細（Labels/Project、systemd service/timer）、AIツール選択の内部設計（2段実行、`[agent.*]` TOML、tiers fallback）、各CLIの長文解説を、基本利用者向けの導入・利用情報と同一ファイルに混在させていた。この結果、内部実装（例: workerのphase遷移、設定の追加項目）を変更するたびにREADMEが陳腐化する構造になっており、かつ「READMEだけを読んだ基本利用者が用途・導入・要求入力・日常操作・トラブルシュート先を識別できる」という基本的な読みやすさを損なっていた（Issue #64）。

また、開発主体は原則としてAgentic Loop（AI）であり、人間が実際に担う責務（要求の伝え方、意思決定・承認、レビュー、例外時の手動確認・復旧）と、AIが従うべき実装指示・検証規則・不変条件（AGENTS.md、policy、machine-readable設定が正本）を区別する文書が存在しなかった。

## 決定

1. **文書ごとの第一読者と責務境界を [文書ポリシー](../policies/documentation.md) として定義する。** README.md（基本利用者）、`docs/development.md`（人間の開発者、新設）、`docs/operations/`（運用者）、`docs/policies/`（規範の適用者）、`docs/decisions/`（設計を追う人）、`AGENTS.md`（AI）、machine-readableな設定（ツール）の7種を区別し、同じ事実を複数箇所へ重複させず正本へリンクする原則を定める。
2. **README.mdを基本利用者向けに再構成する。** 「できること」「導入」「要求を出す」「日常の操作」「困ったときは」「文書の案内」の6見出しに固定し、開発・検証コマンド、内部実装の詳細、AIだけが必要な手順を置かない。
3. **README.mdから移設が必要な固有情報を既存の運用文書へ統合する。** main branchの定期同期timerは [docs/operations/upgrade.md](../operations/upgrade.md) へ、plan/exec 2段実行の意図と `[agent.retry].plan_max` は [docs/operations/issue-queue.md](../operations/issue-queue.md) の既存tiers節の前に、固定runtime PATHの自己修復は同issue-queue.mdのセットアップ節へ、それぞれ追記する。
4. **`docs/development.md` を新設し、人間の開発者だけが担う責務に限定する。** 要求の伝え方、意思決定・承認、レビュー、例外時の手動確認・復旧の4項目を説明し、規則本文は複製せず正本へリンクする。新規リポジトリへの一度きりのseed（`INIT_FILES`）として配布し、target所有とする。
5. **AGENTS.mdの不変条件へ文書ポリシーの参照を1行追加する。**
6. **CLI一覧の正本を `docs/operations/issue-queue.md` とし、本決定をもってADR 0007・ADR 0009に記された「README.mdのCLI一覧へ追記する」運用を置き換える。** 過去のADR本文は記録として保持し、書き換えない。

## 代替案

- **README.mdに全情報を残し、見出しで整理するだけにする案**: 見出しを整理しても、内部実装の変更（新CLI追加、tiers設定拡張等）のたびにREADMEを更新する構造は変わらず、陳腐化の根本原因を解消しない。採用しない。
- **`docs/development.md` を作らずAGENTS.mdへ人間向け情報も含める案**: AGENTS.mdはAIが読む文書であり、人間向けの説明（意思決定の仕方、レビューの仕方）を混在させると対象読者が不明確になる。採用しない。
- **文書ポリシーをADRだけで表現し、`docs/policies/` に新設しない案**: ADRは決定の記録であり、継続的に参照される規範（README責務境界の禁止事項、更新責務）を置く場所として不適切。既存の他policyと同じ形式で `docs/policies/documentation.md` を設けることで、AGENTS.mdの不変条件から一貫して参照できるようにした。

## 帰結

- README.mdは基本利用者だけを対象にした短い文書になり、内部実装の変更では更新不要になる。
- 開発情報とAI向け指示の境界が明文化され、将来の文書追加時にどこへ書くかが機械的に判定できる（`scripts/lint.sh` が責務境界とREADME必須要素を検査する）。
- 空リポジトリへの新規installでは `docs/development.md` がseedされ、既存Foundation管理文書の一部として `docs/policies/documentation.md` が配布・upgradeされる。

## 対象外

- 既存ADR本文の書き換え（記録の不変性を保つ）。
- CLIやSupervisor挙動の変更、`.agentic-loop.toml` のスキーマ変更。
- `docs/operations/*.md` の全面再編（今回はREADME固有情報の受け入れに必要な追記だけ）。

## 追加費用

文書構成の変更だけであり、追加のGitHub API呼び出しや外部serviceは発生しない。追加費用ゼロ。
