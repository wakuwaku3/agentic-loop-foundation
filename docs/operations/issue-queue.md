# Issueキュー運用

## セットアップ

`install.sh` は変更前に `git`、`gh`、Codex CLI、GitHubログイン、origin、リポジトリ参照、Projects API権限を検査する。既存ファイルとの競合もコピー前に検査する。検査後、7個の状態Label、4個の `priority:*` Label、6個の `category:*` Labelと `Agentic Loop - OWNER/REPOSITORY` Projectを冪等に用意し、既定ではSupervisorを起動する。Projectには同じ6選択肢の `Category` fieldを作成する。

GitHub tokenには対象リポジトリのIssue/PR操作権限と `project`、`read:project` scopeが必要である。不足時は `gh auth refresh -s project,read:project` など、利用中のGitHub認証方式に合う方法で追加する。Projectはuser/org所有のため、対象リポジトリとProjectの閲覧者が一致することを管理者が確認する。privateリポジトリの内容や秘密情報をProjectフィールドへ転記しない。

Project APIでlink、`Agent status` single-select、Issue item追加に加え、`Open Issues`、`Closed Issues`、`Open PRs`、`Closed PRs` のtable viewを設定する。各viewはitem種別とOpen/Closed状態でfilterされ、`install.sh` または `bin/agentic-loop setup` の再実行時に同名viewを再利用してfilterを修復し、既存のOpen/Closed PRをProjectへ追加する。workerが作成したPRも処理終了時に追加する。Project同期の障害はIssueキューの実行中にはキューを停止しない。

## 操作

```sh
bin/agentic-loop start
bin/agentic-loop status
bin/agentic-loop stop
```

利用者は要求をIssueとして登録し、6個の `category:*` のうち1つと `agent:queued` を付ける。取得順はcategory、同一category内のcritical、high、medium、low、優先度なし、作成日時、Issue番号の順とする。category順は `loop-continuity`、`confidentiality-incident`、`integrity-incident`、`availability-incident`、`feature`、`improvement` で固定する。複数のpriority LabelがあるIssueは最も高いものを使う。依存関係はIssue本文に明記する。回答は `agent:needs-input` のIssueへコメントする。

### 対話要求の受付

通常のbuild・変更要求を受けた対話中のAgentは、次の順序で経路を決める。

1. 読み取り専用の質問、診断、status確認、`start`・`stop`などの運用操作はIssue化しない。同期実行または直接実装を利用者が明示した場合も受付を省略する。
2. `.agentic-loop/config` と実行可能な `bin/agentic-loop` があり、`bin/agentic-loop status` の先頭行が `running` で、`gh`が対象repositoryのIssueを参照・更新できることを確認する。
3. open Issueのtitleとbodyを検索し、要求の目的と対象範囲が同じIssueがないか確認する。候補の本文とコメントを読み、同じ利用者結果を求めるなら再利用して新規作成しない。
4. 要求を `loop-continuity`、`confidentiality-incident`、`integrity-incident`、`availability-incident`、`feature`、`improvement` の順に評価し、該当する最上位の `category:*` を1個選ぶ。incidentはCIAへの実害で分類し、単なる重要度では選ばない。分類不能時は `category:improvement` を安全な既定値として使い、queued中に再トリアージする旨を記録する。
5. 重複Issueが `agent:running` ならURLと状態を報告して終了し、`agent:queued` なら選択カテゴリが1個だけになるよう確認して再利用する。それ以外は他の `agent:*` 状態Labelを外し、カテゴリ1個と `agent:queued` を付ける。重複がなければ要求・制約・完了条件を本文にしたIssueを作り、カテゴリと `agent:queued` を同時に付ける。
6. Issueを再取得し、open、カテゴリが1個、かつ `agent:queued` または `agent:running` であることを確認する。URL、カテゴリ、状態を報告し、直接実装せず終了する。

Supervisorがclaimした `agent:running` Issueのworkerは受付を再実行しない。元Issueとコメントを要求として、専用branch/worktree、全検証、secret guard、commit、push、PR、required checks、review対応、merge、default branch確認、branch/worktree cleanupまで進め、再帰的な代替Issueを作らない。

### カテゴリの修復とincident取扱い

Supervisorはclaim前、`setup`、およびProject同期の再処理時にqueued Issueを検査する。カテゴリなしには監査コメント付きで `category:improvement` を補い、複数カテゴリには上記順位の最上位だけを残す。どちらもqueued中にLabelを1個だけ残せば再分類でき、次のreconcileでProjectの `Category` と一致する。たとえばqueue停止やworker復旧障害は `loop-continuity`、認証情報の露出は `confidentiality-incident`、artifact改変は `integrity-incident`、利用機能の停止は `availability-incident`、新機能は `feature`、文書整理は `improvement` とする。loop自体が停止した可用性障害は `loop-continuity` を優先する。

incident Issueには、秘密の値、攻撃手順、不要な個人情報を本文・コメント・Label・Projectへ転記しない。LabelとProjectにはカテゴリ名だけを保存し、詳細証跡は承認された秘密保管境界で管理する。

### 安全なfallback

キューのファイル、GitHub権限、Supervisorの正常性、またはqueued/running状態を確認できない場合、失敗した確認項目を明示する。確認できないIssueを追加作成したり、queued Issueと並行して同じ変更を実装したりしない。

読み取り専用要求と運用操作はその場で続行する。通常の変更要求は、専用branch/worktreeを利用でき、追加費用・秘密・破壊的操作に関する判断が不要な場合に限り、キューが利用不能であることを明示してworker workflowを同期実行する。それ以外は復旧方法または必要な判断を正確に提示して停止する。明示された同期・直接実行も同じworker workflowと不変条件に従う。

`.agentic-loop/config` で `POLL_SECONDS`、`MAX_WORKERS`、`LEASE_SECONDS`、`STOP_TIMEOUT`、`STALE_DAYS`、`GRAPHQL_RESERVE`、`RATE_LIMIT_CACHE_SECONDS`、`API_RETRY_ATTEMPTS`、`API_RETRY_BASE_SECONDS` を変更できる。既定の並列数は4とし、これを超えてむやみに増やさない。増加はCodex契約上の制限、Git競合、端末資源を確認してから行う。stopは新規claimを止め、workerをdrainする。`STOP_TIMEOUT=0` は完了まで待つ。

SupervisorとworkerはGit common stateにGraphQLの残量・reset時刻を短時間cacheして共有する。Issue一覧、Label、comment、heartbeat、PRのmerge確認などloopのcore操作はREST APIを使い、GraphQLはbest-effortのProjects操作だけに限定する。残量が `GRAPHQL_RESERVE` 以下ならProjects item・field同期だけを抑制し、Issue Labelを正本とするqueue処理は継続する。reset後は次のProjects操作が最新残量を再取得し、冪等な同期を再開する。既定値は500であり、0にすると残量によるProjects保護を無効化する。

GraphQL枯渇時もREST APIのquotaは別に確認できる。`gh api rate_limit --jq '.resources | {graphql,core}'` で現在値を確認する。GraphQLのreset前にSupervisorを繰り返し再起動したり、`gh issue list --limit 1000` や `gh project item-list --limit 1000` を手動で反復したりしない。

REST APIはrate limit、secondary rate limit、HTTP 429/5xx、timeout、connection resetなど明示的な一時障害だけを指数backoffで既定3回まで再試行する。認証・権限・入力不正など恒久的な4xxや、冪等性を確認できない操作を無制限に再試行しない。retry回数と待機は日本語でlocal logへ記録し、秘密やresponse本文はIssueへ転載しない。上限到達後は既存のlease、worktree、branchを保持し、Supervisor再起動時のlease復旧で再調査できる。

Supervisorはclaimの直前に、`agent:queued` のまま `STALE_DAYS` 日以上更新されていないIssueを `agent:stale` に遷移し、監査コメントを残してcloseする。再開時はIssueをreopenし、要求を確認・更新して `agent:queued` を付ける。`STALE_DAYS=0` は自動closeを無効にする。queued以外のrunning、needs-input、failed、in-reviewや通常の未キューIssueは対象外である。

## 状態と復旧

- queued: 未取得
- running: leaseを持つworkerが処理中
- needs-input: 不可逆・費用・重大な安全判断または解消不能な権限不足への回答待ち
- in-review: PR確認中（workerが進捗として使用可能）
- completed: workerの完了自己申告に加え、対応branchのmerge済みPRをGitHub APIで確認済み
- failed: mergeを証明できず終了。原因確認後にqueuedを付けて再試行する
- stale: queuedのまま設定日数更新されず、監査コメント付きで自動closeされた

Supervisorは起動時にrunning Issueの最新leaseコメントを読み、期限切れをqueuedへ戻す。Issue worktreeは対象リポジトリと同じ親ディレクトリの `<repository>-worktrees/issue-<number>` に分離する。workerは `workspace-write` を維持し、Gitが解決した対象リポジトリのcommon metadataディレクトリと、保護対象だが要求実装に必要な専用worktree内の `.agents` だけをCodex CLIの `--add-dir` で書き込み可能にする。common directoryとworktree固有Git directoryの親子関係を検証できない場合、root、home、worktree rootのような広い範囲の場合、または `.agents` がsymlinkやworktree外のpathに解決される場合はworkerを起動しない。workerの標準出力・標準エラーはGit管理外の `.git/agentic-loop/logs` に保存し、Issueへ転載しない。ログに秘密が疑われる場合は削除し、資格情報を失効する。Project同期は再実行可能であり、`bin/agentic-loop setup` で修復する。

workerが `AGENTIC_LOOP_RESULT=completed` を返しても、それだけでは完了にしない。SupervisorはIssue専用branchをheadとするmerge済みPRをGitHub APIで確認し、PRの `headRefOid` がlocal branch先端と一致することを検証する。さらに、専用worktreeが期待pathでそのbranchを使用し、未commit変更がないことを確認してからworktreeを通常削除し、確認済みOIDとのcompare-and-deleteでlocal branchを削除する。merge未確認、別worktreeで使用中、未commit変更、想定外ref、または削除競合がある場合は `failed` とし、残っているworktreeとbranch dataを保持して安全な再調査を可能にする。

remote branchは復旧可能性を残し、GitHubのPR merge時branch削除設定と責務を分離するため、Supervisorからは削除しない。remote branchの保持または削除はrepositoryのコード化されたGitHub設定に従い、このcleanupの成功条件には含めない。

Supervisorはrepositoryごとのuser-level systemd serviceとして登録され、予期しない終了では5秒後に自動再起動する。`stop`による正常終了では再起動しない。service名は `agentic-loop-supervisor-<repository path>.service` で、`systemctl --user status` と `journalctl --user -u` で確認できる。起動時には生存しないPIDとlockを削除してからlease復旧を行う。Supervisorが停止している場合はstatus、`.git/agentic-loop/supervisor.log`、systemd unit、`gh auth status` を確認する。同じリポジトリを複数端末から処理しない。default branch更新後の競合やrequired checks失敗はworkerが最新branchに対して修正・再検証する。
