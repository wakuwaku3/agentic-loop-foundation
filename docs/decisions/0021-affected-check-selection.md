# 0021: 変更影響に応じた段階的テスト選択を、gateとは別の local affected check として導入する

- 状態: 採用
- 日付: 2026-08-18

## 背景

[検証ハーネスポリシー](../policies/validation-harness.md)は local fast check / local full check / CI の3層を定義しているが、いずれも「変更されたfileから今回関係あるtestだけを選ぶ」層を持たない。共通入口`devbox run --pure check`の`test`は`tests/run-e2e.sh`が`queue`/`lifecycle`/`auxiliary`/`upgrade`の4群を並列実行するE2Eハーネス（`tests/test-agentic-loop.sh`、単一fileに群境界がある）であり、群を絞る外部interfaceが無いため、1行の文書変更でも4群すべてを待つことになる。Issue #59はこれを解消する要求である。

## 判断

### 新しい層は gate ではない

`local affected check`を検証ハーネスポリシーの層一覧へ追加するが、push gateとmerge gateは従来どおり`local full check`（`devbox run --pure check`）とCIのままであり、affected checkはこれらを一切代替・弱体化しない。`scripts/affected-check.sh`は実行のたびに「これはgateではない。gateは`devbox run --pure check`である」ことを`.agentic-loop/capabilities.toml`の`validation.full_check`から読んで明示する。CIワークフロー（`.github/workflows/ci.yml`）は変更しない。

### 判定規則は宣言的mapで表現し、コードに埋め込まない

`tests/impact-map.toml`（`schema_version = 1`）が「path prefix -> 検証単位」の唯一のsourceである。単位は既存のE2E群と1対1（`e2e:queue`/`e2e:lifecycle`/`e2e:auxiliary`/`e2e:upgrade`）とし、`environment`と`lint`は高速で決定的なため常に実行する（`always`）。一致は**pathセグメント境界を尊重した最長prefix一致**であり、単純な文字列prefixにはしない（`bin/agentic-loop-diagnose`が`bin/agentic-loop`に誤って一致しては安全側にならない、という逆の問題を避けるため）。

`rule`は一致した変更に検証単位を追加する。`widen`に一致した変更、およびどの`rule`/`widen`にも一致しない変更（unmatched）は、安全側として**常に全単位へ拡大**する。共有基盤（`bin/`配下の大半、`bin/lib/agentic-loop/common.sh`等）、build/runtime設定（`devbox.json`/`devbox.lock`/`Makefile`/`.agentic-loop.toml`/`.agentic-loop/capabilities.toml`）、test基盤自身（`tests/run-e2e.sh`/`tests/test-agentic-loop.sh`/`tests/impact-map.toml`）、CI定義（`.github`）はすべて`widen`として宣言し、判定不能時に安全側へ倒すという要件を構造的に満たす。

### `tests/test-agentic-loop.sh`は単一fileのまま、diff行推定は採らない

4群と共有helperが同一fileに同居する（群境界は`AGENTIC_LOOP_TEST_GROUP`のif文の行番号）。「変更行が第何群の区間に入るか」を推定する方式も検討したが、群境界の行番号はfile編集のたびに動くため、推定はfixture追加や並べ替えで**静かに**壊れる。このrepositoryの検証哲学（安全側に倒す、判定不能を隠さない）に反するため採らず、`tests/test-agentic-loop.sh`自体の変更は常に`widen`（全単位）とする。将来群ごとにfileを分割すれば、この制約は自然に解消する。

### 同等性と網羅性を`--audit`で機械的に守る

mapは人手で編集するため、群追加時の宣言漏れや、新規fileの分類漏れが静かな過小選択につながる。`scripts/affected-check.sh --audit`（`devbox run --pure affected-audit`、`make check`から`e2e:auxiliary`群のfixtureとして呼ばれる）は次を検証する。

1. `impact-map.toml`の`units`が`tests/run-e2e.sh`の`ALL_GROUPS`および`tests/test-agentic-loop.sh`の受理群集合と完全一致する。
2. すべての単位が少なくとも1つの`rule`から到達可能である（孤立群を禁止）。
3. すべての`rule`/`widen`のpathが実在する。
4. **追跡済みfileはすべて明示的に`rule`か`widen`へ一致する**。`unmatched`は新規・未分類fileのためだけの経路であり、既知fileがそこに落ちたままcommitされることを許さない。
5. 全変更集合（`git ls-files`全体）を入力すると、必ず`widen`に一度は一致し、選択が全単位に退化する（full check同等性）。

新規fileをmapに分類し忘れると4が失敗し、`make check`自体が失敗する。「静かな過小選択」より「明示的な失敗」を選ぶ、という判断である。分類の手間を減らすため、共有基盤やtest基盤はディレクトリ単位の`widen`で広く吸収し、個別fileの追加が新しいruleを要求するのは特定のE2E群にだけ効く場合に限る。

### 除外・skip用のinterfaceを持たない

`scripts/affected-check.sh`は`--exclude`/`--skip`のような引数を実装せず、flakyや過去の失敗を理由にした除外の入口を一切持たない。未知optionは終了code 2で拒否する。`scripts/lint.sh`はソース中にそれらしいtoken（除外系optionの実装、flaky・過去失敗を理由にした分岐）が無いことを検査し、将来のPRで抜け道が追加されることを防ぐ。

### 選択理由と実行結果は機械可読recordに残す

`--format json`は変更ごとの一致rule/units、unmatched一覧、拡大理由、選択・skip単位、単位ごとの結果と秒数、gateコマンドを1つのJSONへ出力する（既定で`tmp/affected-last.json`、`--no-record`で抑止）。診断可能性（要件「選択漏れを診断できる」）はこのrecordで満たす。

### repository capability manifestとの連携

`.agentic-loop/capabilities.toml`の`[validation]`へ`affected_check`・`impact_map`・`affected_check_seconds`を追加する。`schema_version`は据え置き（既存keyの意味を変えず追加のみ）。既存installはこれらを宣言しないまま動作し続け（未宣言=「affected checkを持たない」として扱われるだけ）、`capability_generate`は`Makefile`に`affected:` targetがあり`tests/impact-map.toml`が実在する場合にだけ検出・宣言する。`affected_check_seconds`が`full_check_seconds`以上になった場合は、既存の`drift:*`系warningと同じ経路（`bin/lib/agentic-loop/doctor.sh`は`drift:*`をwildcardで既に処理しているため、doctor側の追加コードは不要）で矛盾を警告する。

## 対象外

- CI workflowの変更、CIでの部分検証・selective CI（検証ハーネスポリシーがmerge gateの弱体化を禁止するため）。
- `.agentic-loop.toml`への新設定keyの追加とupgrade migration（本機能は挙動を切り替える設定を持たないため不要）。
- workerのplan/exec promptへのaffected入口の埋め込み（policy・運用文書での層定義に留め、prompt配線は別Issueの判断とする）。
- `tests/test-agentic-loop.sh`のE2E群再編・分割、test実行順序の最適化。

## 追加費用

`scripts/affected-check.sh`はローカルの`tests/impact-map.toml`とgit操作だけを読み、追加のGitHub API呼び出しやCI変更はゼロ。`--audit`もローカルの静的検査のみ。追加費用ゼロ。
