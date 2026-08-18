# local affected check（[ADR 0021](../decisions/0021-affected-check-selection.md)）

`scripts/affected-check.sh`は、変更されたfileから実際に関係するE2E群だけを選んで実行し、編集中のfeedbackを短縮する。**push gateでもmerge gateでもない**。gateは常に[検証ハーネスポリシー](../policies/validation-harness.md)が定める`devbox run --pure check`（local full checkおよびCI）である。

## 使い方

```sh
devbox run --pure affected              # 選択した単位を実行する（既定: tmp/affected-last.jsonへ記録）
devbox run --pure affected-audit        # tests/impact-map.toml自体の整合性を検査する（testは実行しない）
```

```sh
scripts/affected-check.sh [--changed-from REF] [--files FILE|-] [--print-plan]
                           [--format text|json] [--record PATH|--no-record] [--audit]
```

- 既定の変更集合: `merge-base(origin/main または main, HEAD)..HEAD`に加え、index・working tree・untrackedの変更。baseを決定できない場合（upstream不明・shallow clone等）は安全側として全単位へ拡大する（理由: `git-base-unknown`）。
- `--changed-from REF`: baseを明示する。
- `--files FILE|-`: git状態を使わず、改行区切りのpath一覧を直接与える（fixture検証・CI以外の外部harnessからの呼び出し用）。
- `--print-plan`: 選択計画だけを出力し、何も実行しない。
- `--record PATH` / `--no-record`: 実行結果recordの出力先。既定は`tmp/affected-last.json`（`.gitignore`済み）。
- `--audit`: 選択は行わず、`tests/impact-map.toml`自体の整合性（後述）だけを検査する。
- **除外用のoptionは存在しない**。flakyや過去の失敗を理由にした除外を許さないため、`--exclude`/`--skip`相当のinterfaceを実装しておらず、未知optionは終了code 2で拒否する。

## `tests/impact-map.toml`の書き方

```toml
schema_version = 1
units = ["e2e:queue", "e2e:lifecycle", "e2e:auxiliary", "e2e:upgrade"]
always = ["environment", "lint"]

[[rule]]
path = "bin/lib/agentic-loop/scope.sh"
units = ["e2e:queue"]
reason = "..."

[[widen]]
path = "bin/lib/agentic-loop/common.sh"
reason = "shared-dependency"
```

一致は**pathセグメント境界を尊重した最長prefix一致**（`path`自身、または`path + "/"`で始まるものだけが一致し、`bin/agentic-loop-diagnose`は`bin/agentic-loop`に一致しない）。`rule`は一致した変更units群に単位を追加する。`widen`に一致した変更、およびどの`rule`/`widen`にも一致しない変更（`unmatched`）は、常に全単位へ拡大する。`always`に列挙した単位（`environment`・`lint`）は選択結果に関わらず常に実行する。

## 拡大（widen）のtrigger一覧

以下のいずれかに該当する変更は、必ず全E2E群へ拡大する。

- 判定不能（baseを決定できない、`impact-map.toml`が解釈できない）
- 共有依存（`bin/`配下の大半、`bin/lib/agentic-loop/common.sh`等）
- build/runtime設定（`devbox.json`/`devbox.lock`/`Makefile`/`.agentic-loop.toml`/`.agentic-loop/capabilities.toml`）
- test基盤自身（`tests/run-e2e.sh`/`tests/test-agentic-loop.sh`/`tests/impact-map.toml`）
- CI定義（`.github`）
- 未一致（`unmatched`）: `tests/impact-map.toml`にまだ分類されていない変更

## 実行結果recordのschema

```json
{"schema":1,"base":"<sha>","gate":{"full_check":"devbox run --pure check"},
 "changed":[{"path":"bin/lib/agentic-loop/scope.sh","rule":"bin/lib/agentic-loop/scope.sh","units":["e2e:queue"]}],
 "unmatched":[],"widened":false,"widen_reasons":[],
 "selected_units":["e2e:queue"],
 "skipped_units":[{"unit":"e2e:lifecycle","reason":"no-matching-change"}],
 "results":[{"unit":"lint","result":"passed","seconds":8},{"unit":"e2e:queue","result":"passed","seconds":131}],
 "total_seconds":143}
```

`gate.full_check`は`.agentic-loop/capabilities.toml`の`validation.full_check`から読む（これはgateの参照であり、affected check自体をgateにする宣言ではない）。

## `--audit`が検査する内容

testは実行せず、`tests/impact-map.toml`自体の整合性だけを検査する。

1. 宣言単位（`units`）が`tests/run-e2e.sh`の`ALL_GROUPS`および`tests/test-agentic-loop.sh`の受理群集合と完全一致する。
2. すべての単位が少なくとも1つの`rule`から到達可能である（孤立群を禁止）。
3. すべての`rule`/`widen`のpathが実在する。
4. 追跡済みfileはすべて明示的に`rule`か`widen`へ一致する（`unmatched`は新規・未分類fileのためだけの経路）。
5. 全変更集合（`git ls-files`全体）を入力すると、必ず`widen`に一致し選択が全単位に退化する（full check同等性）。

いずれかに違反すると`devbox run --pure affected-audit`は終了code 1になり、`make check`もE2Eの`auxiliary`群のfixtureを通じて失敗する。新規fileをmapに分類し忘れると4が失敗するため、静かな過小選択ではなく明示的な失敗になる。

## 実測時間と削減効果の限界

`tests/run-e2e.sh`の4群（`queue`/`lifecycle`/`auxiliary`/`upgrade`）は**並列実行**であるため、E2E全体のwall-clockは最も遅い群にほぼ支配される。したがってaffected checkの削減効果は次の条件に集中する。

- E2Eをまったく選ばない場合（docs/policy専用の変更など）: `environment`+`lint`だけで完了する。
- 除外できた群が最も遅い群だった場合。

逆に、除外できた群が最遅群でなければ、wall-clockの短縮はほとんど無い。この限界を隠さず、以下は2026-08-18時点、commit `e3a1cc7`（本Issue着手直前のmain）相当のcheckoutで、同一機で連続計測した実測値である（後続の大きな構成変更後は再計測すること）。

| 条件 | コマンド | 実測時間 |
| --- | --- | --- |
| full check（基準） | `devbox run --pure check` | 約145秒（`environment`+`lint`+4群並列、最遅群が支配） |
| docs専用変更 | `devbox run --pure affected`（`docs/policies/testing.md`のみ変更） | 約12秒（`environment`+`lint`のみ、E2Eゼロ） |
| 単一群選択（`e2e:queue`のみ） | `devbox run --pure affected`（`bin/lib/agentic-loop/scope.sh`のみ変更） | 約143秒（`environment`+`lint`+`queue`群、`queue`単独は約131秒） |
| 拡大時（full相当） | `devbox run --pure affected`（`bin/lib/agentic-loop/common.sh`など共有依存を変更） | `devbox run --pure check`とほぼ同じ（4群すべてを並列実行するため） |

## 既存repositoryでの採用手順

1. `agentic-loop upgrade`でADR・運用文書を取り込む（この文書とADR 0021は`scripts/lib/foundation-files.sh`のSHARED_FILESとして配布される）。
2. `scripts/affected-check.sh`と`tests/impact-map.toml`はINIT_FILES（一度きりの配布）のため、`agentic-loop upgrade`では上書きされない。既存repositoryは、このFoundationの2つのfileを手動で取り込み、自身のE2E群構成に合わせて`tests/impact-map.toml`を書き換える。
3. `Makefile`へ`affected`/`affected-audit` targetを追加し、`devbox.json`の`shell.scripts`へ同名entryを追加する。
4. `devbox run --pure affected-audit`を実行し、mapの整合性を確認する。
5. `.agentic-loop/capabilities.toml`へ`validation.affected_check`/`validation.impact_map`/`validation.affected_check_seconds`を追加する（`capability_generate`が`Makefile`の`affected:` targetと`tests/impact-map.toml`の存在から自動検出する）。

## gateとの関係

affected checkはpush・merge前に必須ではない。push gateとmerge gateは従来どおり`devbox run --pure check`のローカル成功、およびpublic repositoryではCIの必須check成功である（[検証ハーネスポリシー](../policies/validation-harness.md)）。affected checkの成功・失敗はこれらのgateの代替にならない。

## 費用

追加のGitHub API呼び出しはゼロ。すべてローカルのgit操作・`tests/impact-map.toml`の読み取り・既存の`scripts/check-environment.sh`/`scripts/lint.sh`/`tests/run-e2e.sh`の呼び出しのみ。追加費用ゼロ。
