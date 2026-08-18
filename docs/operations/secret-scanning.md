# secret検査（[ADR 0024](../decisions/0024-secret-scanning.md)）

`.agentic-loop/guard-secrets.sh`が唯一のsecret検査入口である。内部はbaseline層（依存なしの正規表現、常に実行）とscanner層（gitleaks、解決できるときに実行）の二層で、どちらかが検出すればblockする。

## mode

| mode | 範囲 | gate |
| --- | --- | --- |
| `--staged` | staged diff | `.githooks/pre-commit` |
| `--push` | pushされるcommit範囲 | `.githooks/pre-push` |
| `--all` | 追跡fileの現在内容 | `devbox run --pure check`（共通検証入口）・CI・workerの完全検証 |
| `--history` | 全Git履歴 | 完全検証には含めない。導入時・監査時に明示実行する |
| `--text FILE` | 単一textの内容（baseline層のみ） | trace/flaky/preflight recordの検証 |
| `--audit` | `.agentic-loop/gitleaks.toml`自体の統治検証 | `devbox run --pure check`・CI |

## gitleaksの解決順

1. `AGENTIC_LOOP_GITLEAKS`（絶対path。troubleshooting・test用）
2. `PATH`上の`gitleaks`
3. このrepository自身のDevbox virtenv
4. install先の永続runtime virtenv（`bin/agentic-loop`が使うものと同じ）

固定環境内（`DEV_ENVIRONMENT=agentic-loop-foundation-v2`）または`AGENTIC_LOOP_SECRET_SCAN=required`を設定した場合、gitleaksが解決できないとcommit/push/checkが停止する（fail-closed）。それ以外の環境（devbox未導入のhostのGit hookなど）では、baseline層のみで検査を継続し、degradedである旨をstderrへ警告する。`doctor`もgitleaksの解決可否とversionを診断する。

## allowlistの運用

誤検知の除外は`.agentic-loop/gitleaks.toml`の`[[allowlists]]`（全体）または`[rules.allowlist]`（個別rule）だけで行う。`.gitleaksignore`は使わない（fingerprintの列挙は理由・審査の外側で除外できる別経路になるため、このrepositoryでは常に拒否する）。

```toml
[[allowlists]]
description = "なぜこの一致が誤検知なのか（必須）"
regexes = ["具体的で最小限のpattern"]
```

`guard-secrets.sh --audit`が次を機械的に強制する。

- 全entryに`description`があること。
- `regexes`/`paths`が`.*`等の広すぎるpatternでないこと。
- `extend.useDefault`が`true`のままであること（既定ruleの無効化を禁止）。
- entry数が合計8件以下であること。
- `.gitleaksignore`が存在しないこと。

## versionの更新

`devbox.json`の`gitleaks@<version>`を明示的に上げ、`devbox install`で`devbox.lock`を再生成する。上げた後は次を確認する。

1. `devbox run --pure -- gitleaks version`が期待versionを返す。
2. 既定ruleの変更で新規の検出・誤検知が生じていないか、`--all`と`--history`を一度実行して確認する。
3. `devbox run --pure check`が成功する。

## 漏えいを発見したとき

1. 該当credentialを**最優先で失効・再発行**する（Gitからの除去より先に行う）。
2. 値そのものをIssue・PR・commit message・logへ転記しない（`guard-secrets.sh`はredactされた要約しか出さない）。
3. 対象commitから値を除去する。既にpushされている場合は履歴の書き換えが必要になることがあるため、対象repositoryの運用者と相談する。
4. 必要であれば`.agentic-loop/guard-secrets.sh --history`で他に残っていないか確認する。

## `--history`の実行

全Git履歴を走査するため所要時間がrepositoryの規模に応じて増加する。完全検証には含めないため、導入直後の一度と、以後は運用者が必要と判断した監査契機（credential漏えいの疑いがあるときなど）に手動で実行する。

```sh
devbox run --pure -- .agentic-loop/guard-secrets.sh --history
```

## gateとの関係

secret検査はpush・merge gateを弱めない。push gateとmerge gateは従来どおり`devbox run --pure check`のローカル成功、およびpublic repositoryではCIの必須check成功である（[検証ハーネスポリシー](../policies/validation-harness.md)）。

## 費用

gitleaksはOSSでdevboxパッケージとして無償かつ固定versionで取得できる。追加のAPI呼び出しや有料serviceは無い。
