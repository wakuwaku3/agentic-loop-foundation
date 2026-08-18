# repository能力manifest（[ADR 0018](../decisions/0018-repository-capability-manifest.md)）

`.agentic-loop/capabilities.toml`は、このrepositoryの開発・検証・配布能力を、worker・`doctor`・CI・自己診断が毎回推測せずに読める形で宣言するmanifestである。`bin/lib/agentic-loop/capability.sh`が唯一のparser/validatorであり、`bin/agentic-loop capabilities`・`doctor`・worker prompt・`scripts/lint.sh`はすべてこれを経由する。

`.agentic-loop/manifest.json`（install/upgradeが書く配布版数の機械記録）とは別物であり、混同しないこと。capabilities.tomlは値そのものに意味がある宣言、manifest.jsonは「何が導入されているか」の記録である。

## schema

```toml
schema_version = 1
undetermined = []          # 確定できなかったdotted keyの一覧。捏造しない。

[validation]
full_check = "devbox run --pure check"   # repository共通の完全検証入口
fast_check = ".githooks/pre-commit"      # commit前などの短時間・決定的な検証
secret_guard = ".agentic-loop/guard-secrets.sh"
secret_guard_modes = ["--staged", "--push", "--all", "--text", "--history", "--audit"]
full_check_seconds = 300                 # 実測に基づく想定所要秒数。0=未計測
# local affected check（gateではない。ADR 0021）。任意宣言。
affected_check = "devbox run --pure affected"
impact_map = "tests/impact-map.toml"
affected_check_seconds = 150

[environment]
definition = ["devbox.json", "devbox.lock"]  # 環境定義の正本
platforms = ["x86_64-linux", "aarch64-linux"]
marker = "DEV_ENVIRONMENT=agentic-loop-foundation-v2"  # 環境内実行の機械的な印

[release]
deploy = false
binary_release = false
distribution = "main-merge"   # 配布方法。deploy/binary_releaseが無いことも明示する

[skills]
available = ["submit-requirement", "diagnose-codebase"]

[[ownership]]
path = "bin"
purpose = "Issueキュー CLI とその実装モジュール"

[[protected]]
path = "devbox.lock"
reason = "意図しないlock更新を禁止する"
change_requires = "approval"   # approval | tooling

[[external_environment]]
name = "github"
desired_state = "docs/policies/external-environment.md"
apply = "bin/agentic-loop setup"
```

top-levelの`undetermined`は、TOMLの仕様上**必ず最初の`[section]`より前**に書くこと（`[[ownership]]`などのtable arrayの後に書くと、その直前のtable要素の一部として誤って解釈される）。

## 必須/任意fieldと既定値

| field | 必須 | 既定 |
| --- | --- | --- |
| `schema_version` | 必須 | なし（欠落・非整数・未対応値は失敗） |
| `undetermined` | 省略可 | 空配列相当（何も未確定でない） |
| `validation.*` | すべて省略可 | 未宣言のまま（consumerは「未宣言」として扱う） |
| `environment.*` | すべて省略可 | 同上 |
| `release.*` | すべて省略可 | 同上 |
| `skills.available` | 省略可 | 空配列相当 |
| `[[ownership]]` / `[[protected]]` / `[[external_environment]]` | 省略可 | 0件 |

任意fieldを省略した場合、その項目は「宣言されていない」として伝わる。「不明であることを明示したい」場合は値を書かず`undetermined`にdotted keyを追加する。両者は区別される。

## versioning

`bin/lib/agentic-loop/capability.sh`の`CAPABILITY_SCHEMA_SUPPORTED`（現在`1`）より大きい`schema_version`、または欠落・非整数は、`capabilities`・`doctor`・`scripts/lint.sh`のいずれでも**失敗**として扱う。既定値へは暗黙fallbackしない（fail closed）。schema 2を導入する場合は、1を読めるcompat層を`capability.sh`に追加し、本ドキュメントへ移行手順を追記してから`CAPABILITY_SCHEMA_SUPPORTED`を上げる。

## 安全性検証

- **path系**の値（`validation.fast_check`/`secret_guard`、`environment.definition[]`、`ownership[].path`、`protected[].path`）は、英数字・`.`・`_`・`/`・`-`のみで構成され、絶対pathでなく、`..`セグメントを含まず、解決後のpathがrepository配下に留まること（symlink脱出を含めて拒否）。
- **command系**の値（`validation.full_check`、`external_environment[].apply`）は、シェルmetacharacter（`; | & $ \` ( ) < > * ?`、改行）を含まず、先頭tokenが`devbox`・`make`・`bin/agentic-loop`、またはrepository相対の実行可能fileであること。
- いずれの値も、Foundationのどのコード（worker prompt生成、`doctor`、`capabilities`）からも**実行されない**。値は提示専用であり、`eval`にも`bash -c`にも渡らない。

違反はすべて`level: failure`として報告され、`capabilities`・`scripts/lint.sh`は終了code 1になる（`doctor`は既存の慣習どおり終了code 1）。

## `bin/agentic-loop capabilities`（読み取り専用CLI）

```sh
bin/agentic-loop capabilities [--format json]
```

manifestが存在しない場合は`installed: false`、警告1件（`not-installed`）で終了code 0（manifestは任意）。存在するが解釈・検証に失敗する場合は終了code 1。`--format json`は次の形状を返す。

```json
{"schema_version":1,"installed":true,"valid":true,"summary":{"failure":0,"warning":0},"findings":[],"data":{...}}
```

`data`は`capabilities.toml`をJSONへ正規化したものそのもの。`findings[]`は`level`（`warning`/`failure`）・`code`・`message`を持つ。

## installとupgradeでの初期化

- 空repositoryへのinit: このFoundation自身の`.agentic-loop/capabilities.toml`をそのまま配布する（そのrepositoryはFoundationの`devbox.json`/`Makefile`/CIをまるごと受け取るため、宣言は推測ではなく事実になる）。
- 既存repositoryへのinstall、または既存導入先の`agentic-loop upgrade`（`scripts/upgrade/migrations/0003-capability-manifest.sh`）: `capability_generate`が次の検出テーブルだけを見て値を書く。

| 検出条件 | 書き込む値 |
| --- | --- |
| `devbox.json`に`shell.scripts.check`がある | `validation.full_check = "devbox run --pure check"` |
| 上記が無く`Makefile`に`check:` targetがある | `validation.full_check = "make check"` |
| いずれも無い | `undetermined`へ`validation.full_check` |
| `.githooks/pre-commit`が実行可能 | `validation.fast_check`に記録 |
| `.agentic-loop/guard-secrets.sh`が実行可能 | `validation.secret_guard`に記録 |
| `devbox.json`/`devbox.lock`が存在 | `environment.definition`に記録 |
| `.claude/skills/*/SKILL.md`が実在 | `skills.available`にdirectory名を記録 |
| `Makefile`に`affected:` targetがあり`tests/impact-map.toml`が実在 | `validation.affected_check = "devbox run --pure affected"`、`validation.impact_map = "tests/impact-map.toml"` |
| 上記のいずれかが無い | `undetermined`へ`validation.affected_check`・`validation.impact_map` |
| その他（`environment.platforms`、`release.*`、`ownership`、`protected`、`external_environment`、`validation.full_check_seconds`、`validation.affected_check_seconds`） | 常に`undetermined`（推測しない） |

生成された`.agentic-loop/capabilities.toml`は`.agentic-loop/manifest.json`へ`class=init`（target所有）として記録され、`agentic-loop upgrade`は二度と上書き・削除しない。install/upgrade直後は未commitのため、`doctor`が「能力manifest: 未commit」を警告する。main worktreeの定期同期（`.agentic-loop/update-main.sh`）は、この未commit fileが1件だけ残っている状態を安全な例外として許容し、fast-forwardを止めない（fast-forwardは未追跡fileを書き換えないため安全）。

## `doctor`の追加check

`bin/agentic-loop doctor`は次を報告する。

- 「能力manifest」: manifestが存在しない（警告）／解釈できない・schema_versionが未対応（失敗）／安全性検証違反（失敗）／参照先pathが実在しない（警告）／`undetermined`が空でない（警告）／`.agentic-loop.toml`の`[foundation].verify_command`との不一致（警告、drift検出）／すべて健全（成功）。
- 「能力manifest: 未commit」: 生成直後で未commitの場合の警告。

## worker promptへの反映

検証済みmanifestが存在するときだけ、`issue_prompt`/`plan_prompt`が生成するprompt末尾に短いblockを差し込む（全検証コマンド、短時間検証、secret guard、`undetermined`）。manifestが存在しない、または検証に失敗する場合は何も差し込まず、従来どおりpolicy文書参照にfallbackする。

## 再測定

`full_check_seconds`は実測値であり、devbox/CIの構成を大きく変えた場合は`devbox run --pure check`の実測所要時間で更新する。`queue.worker_timeout_seconds`がこの値を下回っている場合、`doctor`が矛盾として警告する。`affected_check_seconds`も同様に実測値であり、`full_check_seconds`以上になった場合は矛盾として警告する（[local affected checkの運用ドキュメント](affected-checks.md)を参照）。

## 費用

追加のGitHub API呼び出しはゼロ（すべてローカルのTOML/ファイルシステム検査）。
