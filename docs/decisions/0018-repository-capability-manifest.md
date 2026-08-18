# 0018: repository能力を`.agentic-loop/capabilities.toml`のmachine-readable manifestとして宣言する

- 状態: 採用
- 日付: 2026-08-18

## 背景

worker、doctor、CI、自己診断のいずれも、完全検証コマンド、短時間検証、secret guardの呼び出し方、対応platform、重要directory/ownership、変更禁止領域、外部環境、release/deploy有無、利用可能skill、想定実行時間を、`Makefile`・`devbox.json`・`.github/workflows/ci.yml`・`docs/policies/*.md`の散文・`.agentic-loop.toml`の`[foundation].verify_command`という複数の場所から毎回読み直し、推測して組み立てていた。宣言が一箇所にまとまっていないため、drift（例: policy文書とCIの実際のコマンドの不一致）が誰にも観測されず、workerのpromptも「全検証を実行してください」としか言えず、具体的なコマンド名を持てなかった。

既存の`.agentic-loop/manifest.json`はinstall/upgradeが書く**配布版数の記録**（`docs/operations/upgrade.md`）であり、能力宣言ではない。両者を混同させないため、本ADRの成果物は別ファイル・別名にする。

## 決定

### 別ファイル`.agentic-loop/capabilities.toml`を、正本の索引として定義する

TOML形式（`.agentic-loop.toml`と同じ語彙・parser）で、次のセクションだけを宣言する: `validation`（full_check/fast_check/secret_guard/secret_guard_modes/full_check_seconds）、`environment`（definition/platforms/marker）、`release`（deploy/binary_release/distribution）、`skills`（available）、`[[ownership]]`、`[[protected]]`、`[[external_environment]]`、そしてtop-levelの`undetermined`配列。

この manifest は**正本を再定義しない**。「正本ファイルへのpathと、機械的に検証できる少数の事実」だけを持ち、policy本文を再掲しない。`validation.full_check`は`docs/policies/development-environment.md`・`.github/workflows/ci.yml`・`.agentic-loop.toml`の`[foundation].verify_command`と一致すべき値を1箇所に集約するが、これらの文書自体を置き換えない。

### `undetermined`を schema の一級市民にする

「未宣言（キー欠落）」と「検出できないと分かっている」を区別する唯一の手段として、top-levelの`undetermined`配列に、確定できなかったdotted keyを列挙する。worker promptと`doctor`はこの配列を「推測で確定してはならない項目」として明示的に伝える。空配列は「何も未確定ではない」ことの明示的な宣言であり、キー自体の欠落（省略）とは異なる。

### versioning: 未知の`schema_version`はfail closed

`schema_version`は必須の整数。`bin/lib/agentic-loop/capability.sh`の`CAPABILITY_SCHEMA_SUPPORTED`（現在1）より大きい値、または非整数・欠落は、`capabilities`・`doctor`・`scripts/lint.sh`のいずれでも**失敗**として扱い、既定値へ暗黙fallbackしない。将来schema 2を導入する場合は、1を読めるcompat層と本ADRへのmigration手順の追記を必須とする。

### 安全性検証: pathとcommandの許可規則

manifestは信頼できない可能性がある入力（既存repositoryからの検出結果や、将来の手編集）なので、`bin/lib/agentic-loop/capability.sh`は次を**failure**として拒否する:

- **path系**の宣言（`validation.fast_check`/`secret_guard`、`environment.definition[]`、`ownership[].path`、`protected[].path`）: 英数字・`.`・`_`・`/`・`-`のみを許可する正規表現に一致し、絶対pathでなく、`..`セグメントを含まず、`realpath -m`で解決した結果がrepository root配下に留まること（symlink脱出を含む）。
- **command系**の宣言（`validation.full_check`、`external_environment[].apply`）: シェルmetacharacter（`; | & $ \` ( ) < > * ?`および改行）を一切含まず、空白区切りの先頭tokenが`devbox`・`make`・`bin/agentic-loop`、またはrepository相対の実行可能fileであること。

この検証はFoundation自身がmanifestの値を**実行しない**という前提の上での多層防御である。worker prompt・`doctor`・`capabilities --format json`はmanifestの値を提示するだけで、`eval`や`bash -c`には一切渡さない。

### install時の初期化は推測しない

`scripts/install-target.sh`は、空repositoryへのinit時にはFoundation自身の`.agentic-loop/capabilities.toml`をそのまま配布する（そのrepositoryはFoundationの`devbox.json`/`Makefile`/CIをまるごと受け取るため、宣言は推測ではなく事実になる）。既存repositoryへのinstallでは、`bin/lib/agentic-loop/capability.sh`の`capability_generate`が小さな検出テーブル（`devbox.json`の`shell.scripts.check`、`Makefile`の`check:` target、`.githooks/pre-commit`と`.agentic-loop/guard-secrets.sh`の実行可能性、`devbox.json`/`devbox.lock`の存在、`.claude/skills/*/SKILL.md`の実体）だけを見て値を書き、検出できなかった項目は値を捏造せず`undetermined`へ列挙する。既存導入先へは`scripts/upgrade/migrations/0003-capability-manifest.sh`が同じ検出ロジックを冪等に適用する。

生成された`.agentic-loop/capabilities.toml`は`class=init`として`.agentic-loop/manifest.json`に記録し、target所有として`agentic-loop upgrade`が二度と上書き・削除しない。

### `.agentic-loop/manifest.json`との混同を避ける

`.agentic-loop/manifest.json`は「今installされているFoundationの版数と、どのfileがshared/init管理か」の機械記録であり、値の意味論は空である。`.agentic-loop/capabilities.toml`は「このrepositoryが実際に何ができるか」の宣言であり、値そのものに意味がある。`doctor`はこの2つを別のcheck名（「Foundation manifest」と「能力manifest」）で報告し、混同を防ぐ。

### 消費者は同一parser/validatorを共有する

`bin/lib/agentic-loop/capability.sh`は、`bin/agentic-loop`から`source`されるだけでなく、`scripts/install-target.sh`・`scripts/upgrade/migrations/0003-capability-manifest.sh`・`scripts/lint.sh`からも直接`source`できるよう、明示的な`repo_root`引数だけに依存し、`bin/agentic-loop`のグローバル状態（`REPO_ROOT`等）を前提にしない。これにより、worker prompt生成・`doctor`・`capabilities --format json`・CIのlintが、同じ不正なmanifestに対して必ず同じ判定を返す。

### workerへの反映は縮退可能な提示だけ

`worker.sh`の`issue_prompt`/`plan_prompt`は、検証済みmanifestが存在するときだけ、全検証コマンド・短時間検証・secret guard・`undetermined`を数行のブロックとして差し込む。manifestが存在しない、または検証に失敗する場合は何も差し込まず、従来どおりpolicy文書参照にfallbackする。worker自体はmanifestの値を実行しない（実行するのはあくまでworker上で動くAI CLIの判断）。

## 対象外

- manifestの値を`bin/agentic-loop`が自動実行すること（本ADRの範囲では一切実行しない。値は提示専用）。
- `Makefile`・`devbox.json`・CI workflow・policy文書自体の書き換えや、それらを`capabilities.toml`単独の正本へ置き換えること。
- `scope`markerの`env=`語彙の検証ロジックをこのmanifestへ統合すること（`bin/lib/agentic-loop/scope.sh`の既存動作は変更しない）。

## 追加費用

追加のGitHub API呼び出しはゼロ（すべてローカルのTOML/ファイルシステム検査）。CI時間の増加はlintでの検証で数百ms程度。manifestには秘密情報を書けない構造（値はpathとcommand語彙のみ）で、`--all`/`--text`のsecret guardの走査対象にも含まれる。
