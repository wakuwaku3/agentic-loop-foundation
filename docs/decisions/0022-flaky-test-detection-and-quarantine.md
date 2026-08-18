# 0022: flaky testの証拠収集・限定的隔離・修復追跡を、gateを弱めない形で導入する

- 状態: 採用
- 日付: 2026-08-18

## 背景

[検証ハーネスポリシー](../policies/validation-harness.md)は「flakyや過去の失敗を理由に特定のtestを黙って除外する経路を持たせてはならない」と定めるが、再実行で成功したtestを単純な成功として扱う経路は元々存在せず、共通入口`devbox run --pure check`の`test`（`tests/run-e2e.sh`）は`queue`/`lifecycle`/`auxiliary`/`upgrade`の4群を1回だけ実行するため、間欠的に失敗するtestはCIの「たまたまの失敗」として消え、証拠も原因追跡も残らない。Issue #60はこれを解消する要求である。

## 判断

### 判定単位は既存の検証単位に一致させる

flakyの判定単位は、[ADR 0021](0021-affected-check-selection.md)が定義した既存の検証単位（`e2e:queue`/`e2e:lifecycle`/`e2e:auxiliary`/`e2e:upgrade`）と完全に一致させ、test case ID化や群分割は行わない。ADR 0021は「`tests/test-agentic-loop.sh`の群境界（`AGENTIC_LOOP_TEST_GROUP`のif文の行番号）の推定は、file編集のたびに静かに壊れるため採らない」と判断済みであり、本Issueもこの判断を継承する。将来群ごとにfileを分割すれば、判定単位は自然に細粒度化する。

### retryは検知・診断目的に限定し、`passed`への昇格を禁止する

`tests/run-e2e.sh`は、attempt1（既存どおり全群並列のco-run文脈）が失敗した群だけを対象に、最大2回の追加試行を**単独**（他群を並列実行しない孤立文脈）で行う。

1. attempt1: 全群並列（co-run文脈）。成功なら`verdict=passed`で確定し、retryは発生しない。
2. attempt2: attempt1が失敗した群だけを単独再実行（isolated文脈）。失敗なら`verdict=failing`（決定的失敗）で確定し、attempt3は行わない。
3. attempt3: attempt2が成功した場合だけ、確認のためもう一度単独再実行する。

retryは`verdict`の分類と証拠収集にしか使わず、**最初の失敗ログ（attempt1のFAIL行）と全試行結果を必ず保持する**。`flaky`と判定された場合も、`tests/flaky-registry.toml`に一致する隔離宣言が無い限り常に非ゼロで終了する。すなわちmerge gate（`devbox run --pure check`のローカル成功、public repositoryのCI成功）は一切弱まらない。

### 失敗fingerprintは`FAIL:`行のhashであり、群まるごとのミュートを防ぐ

`tests/test-agentic-loop.sh`の`fail()`が出す唯一の失敗形式`FAIL: <message>`の最初の1行をsha256した先頭12桁hexを`fingerprint`とする。隔離は`unit`と`fingerprint`の対にのみ効き、同じ群の別のassertionが壊れても隔離は効かない。`FAIL:`行が無い異常終了（`set -e`によるcrash等）は`fingerprint=unknown`として常に隔離不可にする。

### verdictと終了code

| 観測 | verdict | 既定終了code | quarantine一致時 |
| --- | --- | --- | --- |
| attempt1成功 | `passed` | 0 | (該当なし。既存entryがあれば除去候補として報告) |
| attempt2も失敗 | `failing`（fingerprint不明なら`failing-unknown`） | ≠0 | **常に≠0（隔離不可）** |
| attempt2成功・attempt3成功 | `flaky`（hint: `isolation-sensitive`） | ≠0 | 0 |
| attempt2成功・attempt3失敗 | `flaky`（hint: `intermittent`） | ≠0 | 0 |
| fingerprint不明のまま`flaky`相当 | `flaky-unknown` | ≠0 | **常に≠0（隔離不可）** |

根本原因（並列性か順序依存か真の間欠か）は推定して断定せず、観測した文脈（`co-run`/`isolated`）とhintだけを記録する。

### quarantineはtest coverageを落とさず、期限と責任を機械的に強制する

`tests/flaky-registry.toml`（`schema_version = 1`、出荷時0件）を隔離宣言の唯一のsourceにする。**testは常に実行する**。隔離が変えるのは`verdict=flaky`の終了codeだけで、選択・skip・除外の経路は一切追加しない。既存の検証ハーネスポリシーの禁止事項（flaky・過去の失敗を理由にした黙った除外の禁止）は、`scripts/affected-check.sh`に加えて`scripts/flaky.sh`・`tests/run-e2e.sh`にも及ぶ（両方が`--exclude`/`--skip`相当のinterfaceを持たないことを`scripts/lint.sh`が機械的に検査する）。

`scripts/flaky.sh audit`（`bin/lib/agentic-loop/flaky.sh`の`flaky_registry_validate`を再利用し、`scripts/lint.sh`から実行）が次を強制する。

1. schema/必須field（`unit`/`fingerprint`/`message`/`issue`/`owner`/`first_seen`/`until`）の完全性と型。
2. `until`が今日より未来であること（**期限切れentryが1件あるだけで`make check`全体が失敗する**）。
3. `until - first_seen`が14日以内、かつ`until`が今日から14日以内（無期限隔離の禁止）。
4. `issue`（正整数）と`owner`（非空）が必須であること（責任の所在の必須化）。
5. 有効なentryが同時に最大3件であること（隔離が静かに膨らむことの禁止）。
6. `message`が秘密様の文字列を含まないこと（`.agentic-loop/guard-secrets.sh --text`を再利用）。

隔離が適用された実行は、対象unit・fingerprint・Issue番号・期限・責任者を必ずstdout/stderrへ明示する。

### 再現情報は秘密を含まない構造化fieldだけを記録する

`tests/run-e2e.sh`はローカルの`tmp/flaky-last.json`（gitignore済み）へ、commit・固定環境marker・`devbox.lock`のhash・タイムゾーン・locale・`nproc`・`uname -sr`・run単位のseed・単位ごとの試行（文脈・並列単位・順序・所要時間・exit・失敗行）・verdict・hint・fingerprint・隔離適用有無を記録する。生ログは`tmp/`にのみ残し、git追跡fileとGitHubへは、この許可された構造化fieldだけを転記する。

### 修復Issue作成はテスト実行経路から分離する

`bin/agentic-loop flaky`は読み取り専用（registryの期限・直近recordのverdictを表示、期限切れ・schema不正で終了code 1）。`bin/agentic-loop flaky report`は明示的に呼ばれたときだけ、直近recordから`flaky`/`flaky-unknown`のunitについて修復Issueを作成または再利用する。重複防止は本文marker`<!-- agentic-loop:flaky unit=... fingerprint=... -->`の検索によって行い、同一fingerprintのopen Issueがあれば再利用（再発をcommentで追記）、closedしか無ければ新規作成して旧Issue番号を本文で参照する。`make check`からGitHubへの自動投稿は行わない（検証ハーネスポリシーの「CIのtestから外部影響を行わない」に従う）。

### 4つの連携

- **CI**: 共通入口は変えない。隔離再実行のぶん実行時間が伸びうるため`.github/workflows/ci.yml`の`timeout-minutes`を10から20へ引き上げる。timeoutは依然失敗なのでgateは弱まらない。
- **Projects Recovery View**: `setup.sh`のRecovery viewのfilterへ`flaky` labelを追加し、`flaky` labelを冪等に作成する。
- **自己診断**: `diagnose-codebase` skill（`.agents/`・`.claude/`双方）の監査対象へ、`bin/agentic-loop flaky --format json`が報告する期限切れ・未修復entryを追加する。
- **Issueキュー**: 修復Issueは通常の`agent:queued`としてSupervisorが処理する。`doctor`はregistryの期限切れを失敗、残期間3日以内を警告として可視化する。

## 対象外

- test case単位への群分割、群境界の行番号推定（ADR 0021の判断を継承）。
- `.agentic-loop.toml`への新設定keyの追加とupgrade migration（本機能は挙動を切り替える設定を持たないため不要）。
- `.agentic-loop/capabilities.toml`への宣言追加（既存の`validation.*`宣言群を変更せず、本機能は別経路（`bin/agentic-loop flaky`・`doctor`）で可視化するため）。
- CIでの部分検証・selective CI、affected checkとの統合（検証ハーネスポリシーがmerge gateの弱体化を禁止するため）。

## 追加費用

`scripts/flaky.sh`と`tests/run-e2e.sh`はローカルの`tests/flaky-registry.toml`とgit操作・追加のtest再実行だけを行い、追加のGitHub API呼び出しや有料test基盤はゼロ。`flaky report`はテスト実行経路の外から明示的に呼ばれたときだけGitHub Issue APIを使う（既存のIssue作成経路と同じ費用特性）。追加費用ゼロ。
