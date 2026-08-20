# flaky testの検知・隔離・修復（[ADR 0022](../decisions/0022-flaky-test-detection-and-quarantine.md)）

`tests/run-e2e.sh`は、attempt1（既存どおり`queue`/`lifecycle`/`auxiliary`/`upgrade`4群並列のco-run文脈）が失敗した群だけを、最大2回、単独（isolated文脈）で追加試行する。これは検知・診断目的のretryであり、**`devbox run --pure check`とCIのmerge gateは一切弱まらない**。`flaky`と判定されても、`tests/flaky-registry.toml`に一致する隔離宣言が無い限り常に非ゼロで終了する。

## 試行契約とverdict

1. attempt1: 全群並列。成功なら`verdict=passed`（retry無し）。
2. attempt2: attempt1が失敗した群だけを単独再実行。失敗なら`verdict=failing`（決定的失敗、fingerprint不明なら`failing-unknown`）で確定し、attempt3は行わない。
3. attempt3: attempt2が成功した場合だけ、確認のため単独でもう一度再実行する。

| 観測 | verdict | hint | 既定終了code | quarantine一致時 |
| --- | --- | --- | --- | --- |
| attempt1成功 | `passed` | - | 0 | (該当なし) |
| attempt2も失敗 | `failing`/`failing-unknown` | - | ≠0 | 常に≠0（隔離不可） |
| attempt2成功・attempt3成功 | `flaky` | `isolation-sensitive` | ≠0 | 0 |
| attempt2成功・attempt3失敗 | `flaky` | `intermittent` | ≠0 | 0 |

失敗fingerprintは`tests/test-agentic-loop.sh`の`fail()`が出す`FAIL: <message>`の最初の1行をsha256した先頭12桁hexである。隔離は`unit`（`e2e:queue`等）と`fingerprint`の対にのみ効き、同じ群の別のassertionが壊れれば別のfingerprintとして扱われ、隔離は効かない。

## `tests/flaky-registry.toml`の書き方

```toml
schema_version = 1

[[entry]]
unit = "e2e:auxiliary"
fingerprint = "0123456789ab"
message = "簡潔な説明（200字以内、改行・backtick禁止）"
issue = 123
owner = "github-login"
first_seen = "2026-08-18"
until = "2026-08-25"
```

- `until`は今日より未来でなければならない（期限切れentryが1件あるだけで`devbox run --pure check`全体が失敗する）。
- `until - first_seen`は14日以内、かつ`until`は今日から14日以内（無期限隔離の禁止）。
- `issue`（正整数）と`owner`（非空）は必須（責任の所在の必須化）。
- 有効なentryは同時に最大3件まで（隔離が静かに膨らむことの禁止）。
- `message`に秘密様の文字列を含めない（`.agentic-loop/guard-secrets.sh --text`で検査される）。
- **除外・skip用のoptionは存在しない**。テストは常に実行され、隔離が変えるのは`verdict=flaky`の終了codeだけである。

## CLI

```sh
scripts/flaky.sh classify --unit UNIT --attempt CONTEXT:EXIT:LOGFILE [--attempt ...] [--registry PATH]
scripts/flaky.sh audit [--registry PATH]

bin/agentic-loop flaky [--format json]
bin/agentic-loop flaky report [--record PATH]
```

- `scripts/flaky.sh classify`は`tests/run-e2e.sh`が各群の試行結果からverdictを決定するために呼ぶ（人が直接使う必要はない）。標準出力へ`{"schema":1,"unit":...,"fingerprint":...,"verdict":...,"hints":[...],"quarantined":...,"attempts":[...]}`を出す。隔離が適用された場合は対象・Issue番号・期限・責任者をstdout/stderrへ明示する。
- `scripts/flaky.sh audit`は`tests/flaky-registry.toml`自体の整合性だけを検査する（testは実行しない）。`scripts/lint.sh`から実行される。
- `bin/agentic-loop flaky [--format json]`は読み取り専用。registryの検証結果（期限切れ・責任者欠落等は終了code 1、残期間3日以内は警告）を表示する。
- `bin/agentic-loop flaky report [--record PATH]`は、直近の実行record（既定`tmp/flaky-last.json`）から`flaky`/`flaky-unknown`のunitについて修復Issueを作成または再利用する。本文marker`<!-- agentic-loop:flaky unit=... fingerprint=... -->`で重複を防止し、同一fingerprintのopen Issueがあれば再利用（再発をcommentで追記）、closedしか無ければ新規作成して旧Issue番号を本文で参照する。作成したIssueには`flaky`・`category:improvement`・`agent:queued`を同時付与し、通常のSupervisor/workerの処理対象になる。`make check`からは自動的に呼ばれない（テスト実行経路からGitHubへの書き込みを分離するため、運用者または自己診断が明示的に呼ぶ）。

## 再現情報の記録

`tests/run-e2e.sh`はローカルの`tmp/flaky-last.json`（gitignore済み）へ、commit・固定環境marker・`devbox.lock`のsha256・タイムゾーン・locale・`nproc`・`uname -sr`・run単位のseed（`AGENTIC_LOOP_TEST_SEED`としてexportされ、乱数依存のfixtureが参照する規約）・単位ごとの試行（文脈・並列単位集合・実行順・所要秒・exit・失敗行）・verdict・hint・fingerprint・隔離適用有無を記録する。生ログは`tmp/`にのみ残し、git追跡fileとGitHubへはこの許可された構造化fieldだけを転記する。

## 連携

- **CI**: 共通入口は変えない。隔離再実行とrunner遅延を許容しつつ上限を維持するため`.github/workflows/ci.yml`の`timeout-minutes`は30である。
- **Projects Recovery View**: `Recovery`のfilterに`flaky` labelが含まれ、未修復の修復Issueが回復導線に現れる。
- **自己診断**: `diagnose-codebase` skillは`bin/agentic-loop flaky --format json`を追加の証跡sourceとして使う。
- **Issueキュー**: 修復Issueは通常の`agent:queued`としてSupervisorが処理する。`doctor`はregistryの期限切れを失敗、残期間3日以内を警告として表示する。

## gateとの関係

flaky検知・隔離はpush・merge gateを弱めない。push gateとmerge gateは従来どおり`devbox run --pure check`のローカル成功、およびpublic repositoryではCIの必須check成功である（[検証ハーネスポリシー](../policies/validation-harness.md)）。`verdict=flaky`は隔離宣言と一致する場合だけ終了codeが0になり、決定的失敗（`failing`/`failing-unknown`）は隔離できず常にgateを止める。

## 費用

`scripts/flaky.sh`と`tests/run-e2e.sh`はローカルの`tests/flaky-registry.toml`とgit操作、および失敗した単位だけの追加再実行を行う。追加のGitHub API呼び出しはゼロ。`flaky report`は明示的に呼ばれたときだけ既存のIssue作成経路を使う。追加費用ゼロ。
