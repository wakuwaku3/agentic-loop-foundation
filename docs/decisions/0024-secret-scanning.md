# 0024: 実績あるsecret検出ツール(gitleaks)を固定versionで導入し、既存guardとの責務を二層に整理する

- 状態: 採用
- 日付: 2026-08-19

## 背景

`.agentic-loop/guard-secrets.sh`は自前の正規表現（AWS/GitHub/OpenAI/Slack token、PEM鍵ヘッダのみ）とsensitive path判定だけを持ち、rule/allowlistの管理機構や履歴走査が無い。Issue #61は、実績あるsecret検出ツールを固定versionで導入し、既存guardとの責務を整理して検査漏れと重複をなくすことを要求する。

## 判断

### 二層構造にし、入口は1つのまま保つ

`guard-secrets.sh`を唯一のsecret検査入口として維持し、内部を次の二層にする。

- **baseline層**: 既存の`SECRET_PATTERN`/`SENSITIVE_PATH`。依存なしで常に動く最低保証であり、gitleaksが解決できない環境でも決して省略しない。
- **scanner層**: gitleaks 8.30.1（`devbox.json`に固定version追加）。`.agentic-loop/gitleaks.toml`の`[extend] useDefault = true`で既定ruleを継承する。

両層はORで結合し、どちらかが検出すればblockする。baselineは「重複だから省く」対象ではなく、fail-closedの最終防衛線として位置づける。

### gitleaksの解決順とfail-closed境界

`.githooks/*`はdevbox shellの外で動くため、次の順で解決する。

1. `AGENTIC_LOOP_GITLEAKS`（絶対path。無効化する値は受け付けず、指す先が実行可能でなければ「未解決」として扱う）
2. `PATH`上の`gitleaks`
3. このrepository自身の一時Devbox virtenv（`<repo root>/.devbox/nix/profile/default/bin/gitleaks`）
4. install先の永続runtime virtenv（`<git common dir>/agentic-loop/runtime/.devbox/nix/profile/default/bin/gitleaks`）。これは対象のdevbox.jsonではなくFoundation自身のdevbox.jsonから生成されるため、既存install先が自前のdevbox.jsonにgitleaksを持たなくても供給される。

解決できない場合の扱いは環境で分ける。

- **固定環境内（`DEV_ENVIRONMENT=agentic-loop-foundation-v2`）または`AGENTIC_LOOP_SECRET_SCAN=required`**: fail-closed。「scannerが無いので通す」を選ばない。`devbox run --pure check`・CI・workerの完全検証は常にこの経路に入るため、merge gateは必ずgitleaksを含めて評価される。
- **それ以外（devbox未導入のhostのhookなど）**: baseline層のみで検査を継続し、stderrへ明示的なdegraded警告を出す（無言のskipにはしない）。

### 検査範囲（modeの定義）

| mode | 範囲 | 実装 | gate |
| --- | --- | --- | --- |
| `--staged` | staged diff | `gitleaks git --staged` + baseline | pre-commit |
| `--push` | pushされるcommit範囲 | ref更新ごとに`gitleaks git --log-opts=...`（新規branchは`--no-walk=unsorted <shas>`） + baseline | pre-push |
| `--all` | 追跡fileの現在内容 | 追跡fileのsynthetic diffを`gitleaks stdin`へ + baseline | 共通検証入口（`devbox run --pure check`）・CI・worker完全検証 |
| `--history` | 全Git履歴 | `gitleaks git`（履歴全体） | 明示実行のみ。完全検証には含めない |
| `--text FILE` | 単一textの内容（8KiB上限の機械生成record） | baseline層のみ | trace/flaky/preflight recordのhot path |
| `--audit` | `.agentic-loop/gitleaks.toml`自体の統治検証 | yqによるTOML検査 | 共通検証入口・CI |

`--all`で`gitleaks dir`を直接使わないのは、それが`.gitignore`を無視し、追跡外file（ローカルの`.env`等）まで拾って誤って開発を止めるため（実測で確認済み）。既存の`--staged`/`--push`/`--all`が共有していたsynthetic diff生成をそのまま`gitleaks stdin`へ渡し、tracked-files-onlyの意味論を保つ。

`--history`を完全検証に含めない理由は、履歴走査の所要時間がrepositoryの寿命に比例して増加し、`full_check_seconds`（`queue.worker_timeout_seconds`の根拠）を将来的に破壊しうるため。持ち込まれる履歴は`--push`が毎回検査するため、gateとしての取りこぼしは無い。

`--text`をbaseline層のみに留める理由は、入力が8KiB上限の機械生成record（trace/flaky/preflightのhot path）であり、「どの環境でも必ず動く」ことがfail-closed設計の前提になっているため。ここへプロセス起動を挟むと実行時間とfail-closed意味論の両方を壊す。

### rule・allowlist・baselineの統治

- rule: `.agentic-loop/gitleaks.toml`に`[extend] useDefault = true`のみを置き、baselineと重複する自前ruleは定義しない（baselineは常時併走するため、重複させても検出力は上がらず維持コストだけが増える）。
- allowlist: `[[allowlists]]`（top-level array）または`[rules.allowlist]`（rule単位、singular table）に限定する。`--audit`が次を機械検証する: (1) 全entryに`description`があること、(2) `regexes`/`paths`が`.*`等の広すぎるpatternでないこと、(3) `extend.useDefault`が`true`のままであること、(4) entry数が合計8以下であること、(5) `.gitleaksignore`が存在しないこと（fingerprint列挙はconfigの外側で除外できる別経路になるため、統治されたallowlistへ一本化する）。
- baseline（gitleaksの`--baseline-path`機能）: このrepositoryでは採用しない。既知の漏えいを恒久的に不可視化する機能であり、`--history`がcleanであることを確認して導入したため不要と判断した。

### 検出時の出力

gitleaks呼び出しに`--redact --no-banner`を付け、`-v`（verbose）は使わない（実測により、`-v`無しではgitleaksが標準出力・標準エラーへ発見詳細を出さないことを確認済み）。JSON report（`--redact`適用済み）からrule ID・file・行番号だけを抽出して要約し、gitleaksの生の標準出力・標準エラーはそのまま表示しない。失敗メッセージには修復手順（Gitからの除去、実在した場合のcredential失効・再発行、Issue/PR/logへの転記禁止）を含める。

### exit codeの扱い

gitleaksは既定で「leak検出」と「設定・実行エラー」の両方をexit code 1で返すため、`--exit-code`で検出専用のexit codeを明示的に割り当て、scanner自体のエラー（exit 1）を「clean」と誤認しないようにする（実測で両者が区別できることを確認済み）。

### install先への配布

`.agentic-loop/gitleaks.toml`はSHARED_FILES（`guard-secrets.sh`と同様、upgradeで更新される）。`devbox.json`/`devbox.lock`はINIT_FILES（対象所有）のままとし、書き換えない。新規repository（init）はFoundationのdevbox.json/devbox.lockごと配布されるためgitleaksは最初から入る。既存install先は、永続runtime virtenv経由でgitleaksが供給される（解決順の4番目）ため、対象のdevbox.jsonを推測で書き換えるmigrationは追加しない。`doctor`にsecret scannerの解決可否・versionの診断を追加する。

## 対象外

- gitleaksの`--baseline-path`機能の採用（上記の理由により不要と判断）。
- 既存install先の`devbox.json`/`devbox.lock`への自動追記migration（対象所有のfileを推測で書き換えないため）。
- `.github/workflows/ci.yml`の変更（共通入口`devbox run --pure check`は変わらないため不要）。

## 追加費用

`gitleaks`はOSSでdevboxパッケージとして無償かつ固定versionで取得できる。追加のAPI呼び出しや有料serviceは無い。
