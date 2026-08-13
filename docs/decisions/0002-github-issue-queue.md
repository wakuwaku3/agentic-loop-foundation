# 0002: GitHub Issueをリポジトリ単位の要求キューにする

- 状態: 採用
- 日付: 2026-08-11

## 背景

要求をローカル端末の寿命から切り離して永続化し、同一リポジトリの複数要求を安全に並列処理したい。一方、外部DB、ホスティング、OpenAI API従量課金は費用ポリシーに反する。Projectはuserまたはorganization所有で、Issueと完全に同じ権限境界にはならず、ビューや組み込みworkflowの設定APIにも制約がある。

## 判断

GitHub Issueと `agent:*` Labelを処理状態の正本にする。リポジトリごとに一つのSupervisorだけをローカルで起動し、既定4 workerをIssue専用branch/worktreeで動かす。worktreeは `.git` 配下を避けてリポジトリ隣接の専用ディレクトリに置く。workerは公式の非対話モード `codex exec` を `workspace-write` sandboxと承認待ちなしで使い、検証済みのGit common metadataと専用worktree内の `.agents` だけを `--add-dir` で追加する。OpenAI API keyとdanger-full-accessは使わない。

状態は `queued`、`running`、`needs-input`、`in-review`、`completed`、`failed`、`stale`、`blocked` とする。runningにはworker ID、heartbeat、期限付きleaseをIssueコメントとして記録する。起動時に期限切れleaseをqueuedへ戻す。needs-input後のIssue返信は再取得対象へ戻す。Issueごとの失敗は他workerやキューを停止しない。queued Issueはpriority Labelの優先度と作成日時で決定的に並べ、設定期間更新がないものはclaim前に監査コメントを残してstaleへ移し、closeする。自動closeは無効化でき、reopenと再queueで復旧できる。

claim前、queued Issueの依存関係（GitHub標準のissue dependenciesとIssue本文 `Blocked by:` 行の和集合）を検査する。依存Issueが「closedかつ検証済み完了」（`agent:completed`、または人手管理Issueなら`state_reason=completed`）でなければblockedへ遷移し、理由code付きの監査コメントを残す。close済みだが未検証、循環依存、欠損、別repository参照、構文不正、権限・API障害は、いずれも黙って着手せずfail closedとする。依存が解消すれば人手操作なしに自動でqueuedへ戻る。この判定はscope競合判定より前に働き、blocked Issueの存在は他の着手可能Issueのclaimを妨げない。

`completed` はworkerの出力だけを根拠にせず、Issue専用branchに対応するPRがGitHub上でmerge済みであることをSupervisorが独立に確認した場合だけ遷移する。未mergeまたは確認不能なら `failed` とし、Issueをcloseせずworktreeを保持する。

Projectはリポジトリ名を含む専用名で冪等に作成または再利用し、リポジトリへlinkする。受付、queue、実行中、入力待ち、復旧、最近の完了、Issue監査、PR監査を分ける10個のtable viewを名前で再利用し、filterを再同期する。setupは既存contentを一括登録せず、受付時のIssueとworker終了時のそのbranchのPRを必要時にProject itemへ追加する。実行中の可視化はbest-effortで再同期可能とし、Project障害でIssueの取得・状態遷移を止めない。Projectの所有者アクセスがリポジトリより広い場合があるため、機密情報はProjectへ複製せず、管理者がアクセス境界を確認する。

## 帰結

状態と履歴はGitHub上でリポジトリごとに分離され、中央キューや追加の有料基盤が不要になる。同一リポジトリの単一Supervisorによりclaim競合を避ける。複数端末で同時にSupervisorを起動することはサポートしないため、運用上も一台に限定する。Projectの自動追加workflowには依存せず、Agent statusフィールド、link、10個のview、Supervisorのitem-addまでを自動化し、Issueを正本として継続する。

## 親子分解

plan は必要な場合だけ schema `1` の `agentic-loop:decomposition` JSON manifest を出せる。Supervisorは GitHub 変更前に子key、個別受け入れ条件、scope、先行key、DAG、直接子2〜6件、深さ2、総子孫20の上限を検証する。原子的変更、共有変更を切り離せない要求、統合条件を定義できない要求は分解しない。

正本は GitHub native sub-issues と native `blocked_by` である。子は最初 agent state を持たず、親子関係・依存・Project登録を確認してから `agent:queued` を付ける。親は全子を dependency として待ち、closed だけでなく既存の「closed かつ `agent:completed`」証明を満たした後に再queueされ、通常の worker が統合検証・PR・merge を行う。部分作成やAPI障害は fail closed とし、marker と native 関係を照合して再試行時の二重作成を防ぐ。
