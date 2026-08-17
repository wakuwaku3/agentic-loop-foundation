# 0015: queued Issueの優先度を本文markerの数値priority（0-100）で決める

- 状態: 採用
- 日付: 2026-08-17

## 背景

現行の取得順は category rank が第1キーで、`priority:critical/high/medium/low` ラベルは同一 category 内のタイブレークにすぎない（[0002](0002-github-issue-queue.md)）。このため `category:improvement` + `priority:critical` でも全 `category:feature` の後になり、マニュアルの優先度昇格が category に勝てない。

GitHub はリポジトリの label が最大100個のため、0-100 の数値 label（101個）は物理的に作成できない。Project field をソートの正本にすると、queue 処理が best-effort な Projects/GraphQL 層に依存し、GraphQL 枯渇時（`GRAPHQL_RESERVE`）や Project 障害時に順序が決まらなくなる。現行設計では queue 処理は Label/REST のみで完結し、GraphQL は best-effort の Projects 操作だけに限定する（docs/operations/issue-queue.md）。本変更でもこの原則を保つ。

## 判断

### 取得順を「数値 priority（降順）→ category → created_at → Issue番号」にする

`agent:queued` Issue の取得順を次のとおり変更する。

| 順位 | キー | 向き |
| --- | --- | --- |
| 1 | priority（本文marker値） | 降順（大きいほど先） |
| 2 | category rank | 昇順（`loop-continuity` < ... < `improvement`） |
| 3 | created_at | 昇順（古いほど先） |
| 4 | Issue番号 | 昇順 |

### priority の正本は Issue 本文の marker だけにする

- 値域は整数 0-100。大きいほど優先。
- 入力は本文中の HTML コメント `<!-- agentic-loop:priority N -->`（`N` は 0-100 の十進整数のみ。前後の空白は許容）。
- 複数の marker がある場合は**有効なものの最大値**を採用する。
- 範囲外・非数値・形式不正（`agentic-loop:priority 200`、`agentic-loop:priority abc`、`agentic-loop:priority 90x` など）はその marker を無視し、有効がなければ未設定（0）扱い。
- comment は読まない。scope と同様、Issue **body のみ**を正本にする。
- 番号を source of truth にしないため、Project field / 101個の数値 label は作らない。

並びの単一正本は `queue_priority_jq()`（jq 断片）と `body_priority_value()`（shell 側）で、同じ正規表現を共有し、`queue_rank_jq()` は `(priority_value), (category_rank)` の順で返す。sort は `sort -k1,1nr -k2,2n -k3,3 -k4,4n`。

### 設定・更新は認可済みの運用コマンド `bin/agentic-loop priority ISSUE N`

`dispose` / `resume` と同じ `authorized_operator`（write/maintain/admin）だけが設定できる。処理は REST のみで、本文の既存 marker 行だけを削除して末尾へ1行 upsert し、残りの本文は保持する。移行期の残存 `priority:*` label があれば剥がし、`<!-- agentic-loop:priority-set schema=1 ... -->` 監査コメントと日本語説明を記録する。

### setup は4つの `priority:*` を作らず、既存 label を数値へ移行する

setup は `priority:critical|high|medium|low` を作成しない。代わりに `migrate_priority_labels()` が冪等に実行する。

1. open Issue を REST で page 取得し、`priority:*` label を持つものについて最高 label を数値化する（critical=90 / high=75 / medium=50 / low=25）。
2. body に有効 marker がなければ数値を marker として追記し、既に有効 marker がある場合は **marker を正**とし label だけ剥がす。
3. 当該 Issue から `priority:*` を除去し、最後にリポジトリ label 定義を削除する。

open 移行 + リポジトリ label 削除を完了条件とする。closed Issue に残った label は害が小さく、label 定義の削除で新規付与は止まる。

### status と metrics も数値ベースへ揃える

`status` の次の claim 候補プレビューは `claim_next` と同じ comparator を使い、JSON の `queue.candidates[]` は `priority_rank`（0-4）をやめて観測可能な **`priority`（0-100）** と既存 `category_rank` を出す。metrics の `by_priority` は**文字列キーの数値**（例 `"0"`, `"50"`, `"90"`）で、窓内作成 Issue の件数を、出現したキーだけ昇順で出力する。

## 帰結

- マニュアル指定の priority が category 順より勝てるようになり、昇格させたい要求を本文 marker 1行と認可済みコマンドで確実に先頭へ出せる。
- queue 処理の GraphQL 依存は現状ゼロのまま維持される（REST/Label のみ。Projects は best-effort の可視化層）。
- 既存の `priority:*` label は移行後にリポジトリから消え、setup 再実行で二度と現れない。
- 旧 `priority_rank` フィールドを読む外部利用者は JSON を読み替える必要がある（後方互換より claim との一致を優先する。docs に明記）。

## 対象外

- #166 の自動トリアージ再実装（`priority:*` 4 label 前提のため本変更で前提が変わる。認可済みの `dispose` または再スコープで判断する）。
- Project 上の Priority field をソート正本にすること（queue の GraphQL 依存を増やさない）。
- 0-100 の数値 label の作成（GitHub の label 上限で物理的に不可）。