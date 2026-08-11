# Issueキュー運用

## セットアップ

`install.sh` は変更前に `git`、`gh`、Codex CLI、GitHubログイン、origin、リポジトリ参照、Projects API権限を検査する。既存ファイルとの競合もコピー前に検査する。検査後、7個の状態Label、`priority:critical`、`priority:high`、`priority:medium`、`priority:low` と `Agentic Loop - OWNER/REPOSITORY` Projectを冪等に用意し、既定ではSupervisorを起動する。

GitHub tokenには対象リポジトリのIssue/PR操作権限と `project`、`read:project` scopeが必要である。不足時は `gh auth refresh -s project,read:project` など、利用中のGitHub認証方式に合う方法で追加する。Projectはuser/org所有のため、対象リポジトリとProjectの閲覧者が一致することを管理者が確認する。privateリポジトリの内容や秘密情報をProjectフィールドへ転記しない。

Project APIでlink、`Agent status` single-select、Issue item追加に加え、`Open Issues`、`Closed Issues`、`Open PRs`、`Closed PRs` のtable viewを設定する。各viewはitem種別とOpen/Closed状態でfilterされ、`install.sh` または `bin/agentic-loop setup` の再実行時に同名viewを再利用してfilterを修復し、既存のOpen/Closed PRをProjectへ追加する。workerが作成したPRも処理終了時に追加する。Project同期の障害はIssueキューの実行中にはキューを停止しない。

## 操作

```sh
bin/agentic-loop start
bin/agentic-loop status
bin/agentic-loop stop
```

利用者は要求をIssueとして登録し、`agent:queued` を付ける。取得順はcritical、high、medium、low、優先度なしで、同じ優先度では作成日時が古いIssueを先にする。複数のpriority LabelがあるIssueは最も高いものを使う。依存関係はIssue本文に明記する。回答は `agent:needs-input` のIssueへコメントする。

### 対話要求の受付

通常のbuild・変更要求を受けた対話中のAgentは、次の順序で経路を決める。

1. 読み取り専用の質問、診断、status確認、`start`・`stop`などの運用操作はIssue化しない。同期実行または直接実装を利用者が明示した場合も受付を省略する。
2. `.agentic-loop/config` と実行可能な `bin/agentic-loop` があり、`bin/agentic-loop status` の先頭行が `running` で、`gh`が対象repositoryのIssueを参照・更新できることを確認する。
3. open Issueのtitleとbodyを検索し、要求の目的と対象範囲が同じIssueがないか確認する。候補の本文とコメントを読み、同じ利用者結果を求めるなら再利用して新規作成しない。
4. 重複Issueが `agent:running` ならURLと状態を報告して終了し、`agent:queued` ならそのまま再利用する。それ以外は他の `agent:*` 状態Labelを外して `agent:queued` を付ける。重複がなければ要求・制約・完了条件を本文にしたIssueを作り、`agent:queued` を付ける。
5. Issueを再取得し、openかつ `agent:queued` または `agent:running` であることを確認する。URLと状態を報告し、直接実装せず終了する。

Supervisorがclaimした `agent:running` Issueのworkerは受付を再実行しない。元Issueとコメントを要求として、専用branch/worktree、全検証、secret guard、commit、push、PR、required checks、review対応、merge、default branch確認、branch/worktree cleanupまで進め、再帰的な代替Issueを作らない。

### 安全なfallback

キューのファイル、GitHub権限、Supervisorの正常性、またはqueued/running状態を確認できない場合、失敗した確認項目を明示する。確認できないIssueを追加作成したり、queued Issueと並行して同じ変更を実装したりしない。

読み取り専用要求と運用操作はその場で続行する。通常の変更要求は、専用branch/worktreeを利用でき、追加費用・秘密・破壊的操作に関する判断が不要な場合に限り、キューが利用不能であることを明示してworker workflowを同期実行する。それ以外は復旧方法または必要な判断を正確に提示して停止する。明示された同期・直接実行も同じworker workflowと不変条件に従う。

`.agentic-loop/config` で `POLL_SECONDS`、`MAX_WORKERS`、`LEASE_SECONDS`、`STOP_TIMEOUT`、`STALE_DAYS` を変更できる。既定の並列数2をむやみに増やさない。増加はCodex契約上の制限、Git競合、端末資源を確認してから行う。stopは新規claimを止め、workerをdrainする。`STOP_TIMEOUT=0` は完了まで待つ。

Supervisorはclaimの直前に、`agent:queued` のまま `STALE_DAYS` 日以上更新されていないIssueを `agent:stale` に遷移し、監査コメントを残してcloseする。再開時はIssueをreopenし、要求を確認・更新して `agent:queued` を付ける。`STALE_DAYS=0` は自動closeを無効にする。queued以外のrunning、needs-input、failed、in-reviewや通常の未キューIssueは対象外である。

## 状態と復旧

- queued: 未取得
- running: leaseを持つworkerが処理中
- needs-input: 不可逆・費用・重大な安全判断または解消不能な権限不足への回答待ち
- in-review: PR確認中（workerが進捗として使用可能）
- completed: 検証済みPRがmerge済み
- failed: mergeを証明できず終了。原因確認後にqueuedを付けて再試行する
- stale: queuedのまま設定日数更新されず、監査コメント付きで自動closeされた

Supervisorは起動時にrunning Issueの最新leaseコメントを読み、期限切れをqueuedへ戻す。Issue worktreeは対象リポジトリと同じ親ディレクトリの `<repository>-worktrees/issue-<number>` に分離する。workerは `workspace-write` を維持し、Gitが解決した対象リポジトリのcommon metadataディレクトリと、保護対象だが要求実装に必要な専用worktree内の `.agents` だけをCodex CLIの `--add-dir` で書き込み可能にする。common directoryとworktree固有Git directoryの親子関係を検証できない場合、root、home、worktree rootのような広い範囲の場合、または `.agents` がsymlinkやworktree外のpathに解決される場合はworkerを起動しない。workerの標準出力・標準エラーはGit管理外の `.git/agentic-loop/logs` に保存し、Issueへ転載しない。ログに秘密が疑われる場合は削除し、資格情報を失効する。Project同期は再実行可能であり、`bin/agentic-loop setup` で修復する。

Supervisorが停止している場合はstatus、`.git/agentic-loop/supervisor.log`、`gh auth status` を確認する。同じリポジトリを複数端末から処理しない。default branch更新後の競合やrequired checks失敗はworkerが最新branchに対して修正・再検証する。
