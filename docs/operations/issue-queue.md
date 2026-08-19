# Issueキュー運用

## セットアップ

`install.sh` は変更前に `git`、`gh`、設定 `agent.provider`（環境変数 `AGENT_PROVIDER` と git管理外 `.agentic-loop.local.toml` による上書きを含む）から解決したAI CLI（`codex`／`claude`／`opencode`、既定は `codex`）、GitHubログイン、origin、リポジトリ参照、Projects API権限を検査する。provider=opencodeならCodex CLIが存在しなくてもinstallは成立する。既存ファイルとの競合もコピー前に検査する。検査後、14個の状態Label、7個の `category:*` Labelと `Agentic Loop - OWNER/REPOSITORY` Projectを冪等に用意し、既定ではSupervisorを起動する。`priority:*` Labelは作成せず、既存の open Issue と label 定義があれば本文markerへ移行して削除する（[ADR 0015](../decisions/0015-numeric-priority-marker.md)）。Projectには同じ7選択肢の `Category` fieldを作成する。再installは保存済みのProject identityを再利用し、高コストなProject drift走査とqueued Issueの同期修復を行わない。明示的な `bin/agentic-loop setup` はProjectのrepository link・field・viewを収束させるが、既存Issue/PRの一括backfillは行わない。Issue受付とworkerが扱ったPRは、必要になった時点で個別にProjectへ登録する。

GitHub tokenには対象リポジトリのIssue/PR操作権限と `project`、`read:project` scopeが必要である。不足時は `gh auth refresh -s project,read:project` など、利用中のGitHub認証方式に合う方法で追加する。Projectはuser/org所有のため、対象リポジトリとProjectの閲覧者が一致することを管理者が確認する。privateリポジトリの内容や秘密情報をProjectフィールドへ転記しない。

Project APIでlink、`Agent status` single-select、Issue item追加に加え、次のtable viewをdesired stateとして設定する。`install.sh` または `bin/agentic-loop setup` の再実行はview一覧を1回だけ取得して同名viewを再利用し、filterにdriftがあるものだけを修復して、既存のOpen PRをProjectへ追加する。workerが作成したPRも処理終了時に追加する。

| View | 自動適用するfilter | 目的と表示順 |
| --- | --- | --- |
| `Triage` | `is:issue is:open no:category` | Category未設定のopen Issueを検出する。Agent status・Priorityの欠落とLabel/field不整合は後述の手動検査も行う |
| `Queue` | `is:issue is:open label:"agent:queued"` | Priority（数値・降順）、Category、Created at、Issue番号の順で確認する。各値の順位はSupervisorのclaim順と同じにする。Priorityは本文marker（`bin/agentic-loop priority`）で管理し、Project fieldはソートの正本にしない |
| `Active` | `is:issue is:open label:"agent:running","agent:in-review"` | Agent statusでgroupし、Updated at降順。Issue、関連Pull requestを表示する |
| `Needs input` | `is:issue is:open label:"agent:needs-input"` | Updated at昇順。Title、Agent status、Updated atを表示し、Issue本文・commentを回答先とする |
| `Recovery` | `is:issue label:"agent:failed","agent:stale","agent:parked","flaky"` | Agent statusでgroupし、Updated at昇順。期限切れleaseはSupervisorがqueuedへ復旧するまでrunningとしてIssue commentで検査する。`agent:parked` はリトライ予算を使い切ったopenの人間トリアージ待ちで、自動closeされない（[ADR 0016](../decisions/0016-failure-park-not-close.md)）。`flaky` labelはflaky test修復Issue（`bin/agentic-loop flaky report`が作成、[ADR 0022](../decisions/0022-flaky-test-detection-and-quarantine.md)） |
| `Stopping` | `is:issue is:open label:"agent:stopping"` | 認可済み終了操作によるdrain中。worker成果物を削除しない |
| `Paused` | `is:issue is:open label:"agent:paused"` | 認可済み運用者が`pause`で実行を一時停止したIssue（[ADR 0019](../decisions/0019-issue-level-execution-control.md)）。`Blocked by`に操作者・時刻・再開方法を表示する |
| `Disposed` | `is:issue label:"agent:cancelled","agent:superseded","agent:duplicate","agent:merged" updated:@today-30d` | 理由と統合先を監査commentから追跡する |
| `Recently completed` | `is:issue label:"agent:completed" updated:@today-30d` | Updated at降順。対応Pull requestと完了証跡を追跡する |
| `Open PRs` / `Closed PRs` | `is:pr is:open` / `is:pr is:closed` | Updated at降順。Title、Status、Updated at、Repositoryを表示する |
| `All open issues` / `All closed issues` | `is:issue is:open` / `is:issue is:closed` | Updated at降順の運用一覧。Closed側はsetup以前の全履歴ではなく、Project導入後に登録されたitemを表示する |

Issue状態の正本は `agent:*` Labelであり、Project fieldは表示用の複製である。このため状態別ViewのfilterもLabelを使用し、回答後にLabelが遷移すると `Needs input` から自動的に外れる。private repositoryの本文やcomment、秘密情報をProject custom fieldへ複製しない。

GitHub Projects APIはviewのname、layout、filterを作成・更新できる一方、CLI/APIのversionや所有者種別によってvisible field、sort、groupの更新を利用できない場合がある。現在の自動適用範囲はtable layoutとfilterまでとし、上表の「目的と表示順」をそれ以外のdesired stateとする。Project画面で列、sort、groupを上表どおり設定し、`bin/agentic-loop setup` 後に各Viewのfilterと対象集合を目視検証する。`Triage` はCategory欠落を自動抽出し、Agent status・Priorityの欠落やLabel/field不整合を `All open issues` で比較する。`Queue` は `bin/agentic-loop` のpriority値（数値・降順）、category rank、Created at、Issue番号の比較、`Needs input` は `agent:needs-input` のopen Issue一覧との比較、`Recovery` の期限切れleaseは最新の `agentic-loop:lease` commentの `expires` と現在時刻の比較でdriftを検出する。

Project同期やview修復はbest-effortであり、失敗をstderrへ記録してIssueキュー自体は停止しない。権限または一時障害の復旧後にsetupを再実行する。filter更新は冪等で、再作成も既存名を再利用するためrollbackは直前のfilterへ同じsetup関数で戻せる。view削除は自動化せず、不要Viewを破棄する場合は対象と依存を確認して明示的に承認する。

### 固定runtime PATH

`bin/agentic-loop` の各commandは、install時に検証・記録した固定runtime PATHを自動的に復元するため、`devbox run`、`devbox shell`、direnv、ホストへの`yq`導入を意識せず、そのまま実行できる。記録先はGit管理外のrepository local state（`.git/agentic-loop/runtime.path`）である。installは`.git/agentic-loop/runtime`配下に、このFoundation自身の`devbox.json`/`devbox.lock`で固定した永続Devbox仮想環境を所有し、その`bin`ディレクトリを先頭に記録する。これはtarget自身のgit-common-dir配下でinstall/uninstallを跨いで残り続けるため、nixのGC rootとして継続的に保護され、install時の一時的なbootstrap環境（削除され得る）とは独立である（[ADR 0011](../decisions/0011-installed-runtime-profile.md)）。何らかの理由でこの永続環境自体が失われた場合も、次回コマンド実行時に一度だけ自動的に再構築を試みる（自己修復）。それでも復旧できない場合のみ、復旧手順を含むエラーで終了する。`start` が生成するSupervisorのsystemd serviceにも同じ固定ツールのPATHが設定される。

## 操作

```sh
bin/agentic-loop start
bin/agentic-loop status
bin/agentic-loop status --watch
bin/agentic-loop tail
bin/agentic-loop stop
bin/agentic-loop doctor
bin/agentic-loop metrics
bin/agentic-loop flaky
bin/agentic-loop flaky report
bin/agentic-loop priority ISSUE N
bin/agentic-loop pause ISSUE [--reason TEXT]
bin/agentic-loop resume ISSUE
bin/agentic-loop abort ISSUE [--reason TEXT]
bin/agentic-loop upgrade
```

CLIの公開入口は変更後も `bin/agentic-loop` のままである（[ADR 0013](../decisions/0013-agentic-loop-modules.md)）。実装は機能単位のモジュール `bin/lib/agentic-loop/*.sh` に分かれており、入口は定数と `source` 配線と `main` だけを持つ。実装変更時は、触るモジュールのpathを `agentic-loop:scope` marker（後述）の `paths=` に書いてモジュール単位のscope宣言を行うことで、`bin/agentic-loop` 単体への集中による直列化を避ける。

### status: いま何が動き、何を待ち、次に何が来るか

`bin/agentic-loop status`（[ADR 0005](../decisions/0005-status-observability.md)）は、Supervisorの生死だけでなく、running Issueの詳細、queuedの件数と次のclaim候補、needs-input/failed/in-review/blocked/staleの件数とURL、運用上の異常を1つの入口にまとめた運用snapshotである。常に読み取り専用（GitHubへの書き込み・Git作業ツリーの変更を一切行わない）で、GitHub REST(core)呼び出しは1回の実行あたり最大2回（open Issue全件のsnapshotと、closedな`agent:stale`の一覧）に抑え、GraphQL・Projects APIは呼ばない。引数不正時のみ終了code 2で、それ以外は異常があっても常に終了code 0（合否判定は`doctor`の責務）。

`bin/agentic-loop status --watch [N]`は、`$STATE_ROOT/events.log`（`tail`と同じappend-onlyのイベント列）を`tail -f`相当で連続表示する（[ADR 0005](../decisions/0005-status-observability.md)）。端末（TTY）では全Issueのイベントを追尾し、workerのstage/progress遷移が追加されるたびに流れる。pipe/リダイレクトでは既存イベントを直近最大`TAIL_MAX_LINES`件だけ1回出力して即終了する（followしない）。旧仕様の再描画間隔Nは後方互換のため正の整数として受け付けるが無視される。`--format json`と`--watch`の併用はできない（usage→exit 2）。SIGINT/SIGTERM（Ctrl-C等）で終了code 0で止まる。REST(core)読み取りは0回で、GitHub・作業ツリーへ書き込まない。TTL cacheを使った再描画は`status`単体の実装であり、`--watch`は使わない。

`bin/agentic-loop tail [--issue N] [--follow]`は、`$STATE_ROOT/events.log`（append-only。`epoch<TAB>issue番号|supervisor<TAB>code<TAB>stage-or-`、codeは`progress`/`claim`/`recover`/`timeout`/`stop`/`start`のenum）を時刻整形して流す読み取り専用コマンドである。REST(core)読み取りは0回で、GitHub・作業ツリーへ書き込まない。`--issue N`で特定Issueだけに絞り込み、`--follow`で追尾（ログのinode回転にも追従）。`status --watch`は全Issueの`tail --follow`のショートカットであり、`tail`は`--issue`絞り込みや非followの履歴表示も担う。worker log本文・Issue本文・コメントは一切読まない・出さない。

### 認可済みの終了・統合

`dispose ISSUE --reason cancelled|superseded|duplicate|merged [--target ISSUE]` は唯一の終了入口である。実行者はGitHub認証済みで対象repositoryのwrite/maintain/admin権限を持つ必要がある。`cancelled` は要求撤回、`superseded` は後続Issueへの置換、`duplicate` は同一成果の重複、`merged` は異なる要求の統合を表す。後三者はopenで未終了の同一repository Issueを `--target` として必要とし、自己参照を拒否する。

running/in-reviewはまず`agent:stopping`へ遷移し、所有hostのworker process groupをTERM、`stop_timeout`後にのみKILLする。dirty worktree、未push commit、local/remote branchは保持する。終端化は`state_reason=not_planned`でcloseし、理由・実行者・統合先・時刻のmarkerと日本語説明を両Issueに残す。統合先は元Issue本文・コメント・依存関係を要求として調査する。終了済みIssueはSupervisorがclaim/retry/recovery経路からqueuedへ戻さない。再開は同じ認可を必要とする `resume ISSUE` だけで、履歴を保持してopen + `agent:queued` に戻す。merge済みPRを持つcompleted Issueはdisposeせず、revertまたは後続Issueを作成する。`resume ISSUE` は同じ認可で `agent:parked`（closeされていないopenの人間トリアージ待ち。[ADR 0016](../decisions/0016-failure-park-not-close.md)）や `agent:paused`（下記）も直接再開できる。

### 実行の一時停止・再開・打ち切り（[ADR 0019](../decisions/0019-issue-level-execution-control.md)）

`pause`/`abort`はdisposeとは別の**実行制御層**であり、Issueをcloseしない。実行者は`dispose`/`resume`と同じ認可（GitHub認証済みで対象repositoryのwrite/maintain/admin権限）を必要とし、Issueコメント・PR本文・provider出力を解析して起動する経路は存在しない（CLI入口だけが命令であり、コメント中の偶然の文言は状態に影響しない）。

`pause ISSUE [--reason TEXT]` はSupervisor全体を止めずに1件のIssueだけ実行を止める。`queued`/`needs-input`/`blocked`/`failed`からは即座に`agent:paused`へ、`running`/`in-review`からはworkerを安全にdrainしてから`agent:paused`へ移る。`agent:parked`は既に非claimのため拒否（`abort`と役割が重複する）、終端4種・closed・二重pauseはそれぞれ拒否／冪等成功で応答する。

| pause元 | pause時の動作 | resume復帰先 |
| --- | --- | --- |
| `queued` | 即座に`paused` | `queued` |
| `running` | drainしてから`paused` | `queued`（`running`へは直接戻さず、claim経由のみ） |
| `in-review` | 同上 | `queued` |
| `needs-input` | 即座に`paused` | `needs-input`（回答待ちを飛ばさない） |
| `blocked` | 即座に`paused` | `blocked` |
| `failed` | 即座に`paused`（自動retryの停止に有効） | `queued` |

workerのdrainは協調停止が基本である。workerは自身のstage境界（planループ先頭、replan直前）でだけ停止要求marker（`workers/<issue>.stop-requested`）を確認し、真なら静かに（Label・コメントを変更せず）終了する。それ以外は`pause_grace_seconds`（既定120秒）だけ待ってからTERM、5秒後にKILLする。native sub-issue作成や完了確定シーケンスなど不可分な区間は`workers/<issue>.critical`markerで保護し、drainはこのmarkerが立っている間`pause_grace_seconds`を上限に待機してから初めてTERMを送る。checkpoint（`agentic-loop:handoff`コメントの更新）は既存の「中断からの再開」節にある再開観測（`resume_probe`）を再実行するだけで、新しい仕組みは追加していない。

`resume ISSUE`は`agent:paused`のIssueについて、lease（このhostのlocal workerまたは他hostの有効なlease）・専用worktree/branch・PR・checksを再検証してから、上表の復帰先へ戻す。他hostの有効なlease・生存中のlocal worker・`unsafe-foreign`/`needs-decision`（「中断からの再開」節を参照）のいずれかがあれば拒否し、Label・Git状態を一切変更しない。

`abort ISSUE [--reason TEXT]`は進行中の実行を打ち切り、新しい終端状態を作らず既存の`agent:parked`（open・非claim・人間トリアージ待ち、[ADR 0016](../decisions/0016-failure-park-not-close.md)）へ移す。許可元は`queued|running|in-review|needs-input|blocked|failed|paused`で、`running`/`in-review`は`pause`と同じ協調drainを経る。`agent:parked`は冪等成功、終端4種・closedは拒否する。worktree・branch・PR・commitは削除しない。要求そのものの終了には引き続き`dispose`を使う。

`agent:paused`はGitHub Labelとして保持されるためSupervisor再起動後も維持され、`claim_next`・`retry_failed`・`recover_expired`・`triage_stale_queued`・`reconcile_queued_categories`・`requeue_answered`・`requeue_dependency_ready`のいずれからも構造的に対象外である（`agent:parked`と同じ設計）。他hostが所有するworkerへの`pause`/`abort`は、所有hostが次pollの`drain_paused_workers`で自律的にdrainする（追加のGitHub API呼び出しは発生しない）。`status`はpaused件数・URLに加え、local pause記録がある場合は操作者・時刻・pause前状態を併記し（Issue本文・コメント・worker logは読まない）、Project `Agent status`には`Paused`、`Blocked by`には一時停止理由と再開方法を表示する。

- **Supervisor**: 稼働状態、pid、`max_workers`（既存の1行目の文言は不変）。
- **Running Issues**: Issue番号・title・`(phase: ...)`・`(scope: ...)`に加え、`(started: 開始epoch, elapsed: 経過秒)`・`(timeout_at: 上限到達epoch[、超過なら「超過」]。`worker_timeout_seconds=0`では非表示)`・`(heartbeat: 最終heartbeat epoch)`・`(lease_expires: 期限epoch[、期限切れなら「期限切れ」])`・`(worktree: path[、dirty/diverged、または「なし」])`・`(pr: #番号 state=... checks=...)`を、追加のGitHub呼び出しなしでlocal state（`workers/<issue>.started`・`.lease`・`.resume`、scope cache）から表示する。加えて、workerがlocalへ書くprogress marker（`workers/<issue>.progress`、`epoch\tstage\tseq`。stageはenumのみ）から `(stage: plan|exec|...)`・`(progress: Ns ago)`・`(health: healthy|stalled|timeout)`を表示する。`health`は`timeout`（`worker_timeout_seconds`超過）> `stalled`（最後の進行から既定300秒超過）> `healthy`の順で、local stateのない他host所有Issueは`unknown`（「不明」）。色分けはTTYかつ`NO_COLOR`未設定のときだけANSI、pipe/JSONは無色。progress markerはworkerがstage境界と自ホスト制御区間で書く（heartbeatは更新しない）。worker logのmtimeは本文を読まずに副シグナルとして採用し、provider待ちの長考をstalledと誤判定しない。
- **キュー / 次のclaim候補**: queued総数とclaim可能数、および`claim_next`と同じ順序（priority値（数値・降順）→category rank→created_at→Issue番号）の上位候補を、claimされない理由code（`scope-conflict`／`retry-cooldown`／`claim-paused`）付きで表示する。依存関係の再検証はしない（`agent:blocked`のIssueはqueuedに現れないため対象外であり、これはコスト方針上のbest-effortな割り切りである）。また、各phaseで次に選ばれる `pool` / `provider` / `model`（`agent_pick_tier` のlocal計算。usage実測はせず、pool markerと設定からの推論に限定）と、プール別のclaim pause理由を表示する。markerの`resume_epoch`が読める場合は`pool=<pool> 枯渇（回復予定=YYYY-MM-DD HH:MM:SS, 根拠=…）`の形で回復予定時刻と根拠（`provider提示のreset時刻`／`使用率再probeの実測`／`実測不能のため指数backoff`）まで示し、読めない場合のみ`pool=<pool> 枯渇（回復待ち）`に留める。全プール利用不可時は`全プール利用不可`と表示する（Issue #158）。
- **状態サマリ**: `needs-input`／`failed`／`parked`／`in-review`／`blocked`／`paused`／`stale`の件数と、`https://github.com/OWNER/REPO/issues/N`形式のURL一覧（`stale`は直近100件までで打ち切りがある場合は明示する）。`paused`はlocal pause記録があるIssueに限り操作者・時刻・pause前状態も表示する。
- **警告**: staleなsupervisor pid/lock、期限切れlease、実行時間上限を超過したlocal worker（`worker-timeout`。次回pollで停止し自動的に再試行キューへ戻る）、stall（`worker-stalled`。最後の進行から300秒以上経過しているが上限は未超過。観測のみで自動停止はしない）、local stateのないrunning Issue（`worker-missing`、多端末運用では正常）、GitHub上でrunningでないlocal worker（`worker-orphan`）、対応するrunning Issueのない残存worktree/branch、破損したlocal state file、Project同期の再試行待ち、claim一時停止中、`agent:paused`なのにこのhostのlocal workerがまだ生存している（`paused-worker-live`。次回pollの`drain_paused_workers`で停止する）、をすべてlocal stateの読み取りだけで検出する。

token、worker log本文、Issue本文・コメント、providerのresult fileは一切読まない・表示しない。

```sh
bin/agentic-loop status --format json
```

`--format json`は`schema_version: 1`の単一JSONを1行で出す。主なキーは`supervisor`、`pools`（宣言されている全pool。各要素は`pool`/`exhausted`/`resume_at`（epoch秒、markerが読めなければ`null`）/`basis`（`reset`／`probe`／`backoff`、非exhaustedまたは未読なら`null`）。Issue #158）、`workers`（running Issueごとの詳細。既存の`started_at`/`elapsed_seconds`/`timeout_at`/`timeout_exceeded`/`heartbeat_at`/`lease_expires_at`/`lease_expired`/`worktree`/`worktree_exists`/`dirty`/`diverged`/`branch`/`pr`/`pr_url`/`pr_state`/`checks`/`local_state`/`phase`に加え、後方互換で`stage`・`progress_at`・`progress_age_seconds`・`health`）、`queue`（`queued`・`claimable`・`candidates`。候補は本文marker由来の数値`priority`（0-100）と`category_rank`を持つ）、`waits`（scope/dependency待ち）、`states`（needs-input/failed/in-review/blocked/staleの件数とIssue一覧）、`anomalies`（`level`/`code`/`subject`/`detail`）、`github_available`（GitHub取得に失敗した場合は`false`になり、それ以外のフィールドはlocalの情報のみを反映する）。

token、worker log本文、Issue本文・コメント、providerのresult fileは`status`/`--watch`/`tail`のいずれも読まない・表示しない。progress markerとevents.logはenumのみを保存するため、providerの自由文はここに混入しえない。

#### `status` / `doctor` / `metrics` / `upgrade` / Projects Viewの責務分担

| 入口 | 目的 | 実行頻度・コスト | 合否判定 |
| --- | --- | --- | --- |
| `status` | いま何が動き、何を待ち、次に何が来るかの運用snapshot（`--watch [N]`はevents.logを`tail -f`相当で連続表示。TTYのみ追尾、pipeでは1回出力、`tail`も同じevents.logを時刻付きで表示） | 対話Agentの受付手順からも毎回呼べる（REST(core)読み取り最大2回、GraphQL/Projects 0回、書き込み0回。`--watch`/`tail`は0回） | 常に終了code 0（異常はwarning/infoとして列挙するのみ） |
| `doctor` | 導入・復旧のための環境健全性診断（認証・権限・CLI・Devbox・hooks・systemd・Project設定・設定値・残存状態・Foundation manifest/revision pin/中断したupgrade） | 導入時・障害時に実行 | 必須項目の失敗で終了code 1 |
| `metrics` | 過去の傾向（待ち時間・失敗率・手戻り・稼働率）の再現可能な集計（[運用ドキュメント](loop-metrics.md)） | 利用者が任意の頻度で実行（REST(core)読み取り最大3回、GraphQL/Projects 0回、書き込み0回） | 常に終了code 0（合否判定は`doctor`の責務） |
| `upgrade` | 導入済みFoundationの安全な更新（[運用ドキュメント](upgrade.md)、[ADR 0009](../decisions/0009-foundation-upgrade.md)） | 運用者が明示実行（Supervisor停止中のみ`--apply`可）。既定はdry-runでGitHub書き込み0回 | 承認未済は終了code 3、適用・検証失敗は1、引数不正は2 |
| GitHub Project View | 人向けのIssue/PR一覧の可視化層（best-effort、障害はキューを止めない） | GitHub UI上で確認 | 判定には使わない |

### 事前診断

`bin/agentic-loop doctor` は、`status` の稼働状況表示より広い導入・復旧向けの読み取り専用診断である。GitHub認証とrepository権限、origin/default branch、plan段・exec段・diagnoseが使用する各AI CLI（`codex`／`claude`／`opencode`、tiersを含む全providerをそれぞれ `AI CLI (<provider>)` として個別に検査）、tiersスキーマ（未知provider・空models・不正な `max_usage_percent`）、opencode go usage APIの認証keyの有無（値は表示しない。なければusage実測が使えずcooldownフォールバックになるwarning）、プール枯渇marker、Devbox、hooks、Supervisor、systemd user service/timer、Project設定、設定値、残存worktree/branch/logを検査し、成功・警告・失敗、影響、復旧方法を日本語で出力する。Projectとtimerは任意の可視化・自動運用機能なので利用不能時は警告とし、GitHub Issueキュー、固定検証環境、hooks、Supervisorなど処理に必須の条件は失敗とする。

通常形式と `--format json` はどちらも状態を変更せず、token本体や認証commandの詳細を表示しない。JSONの `schema_version` は1で、`summary` と `checks` を返す。必須診断に失敗がなければ終了code 0、1件以上あれば1、引数不正は2である。警告だけでは自動監視を失敗させない。修復は診断から自動実行せず、表示された `setup`、`start`、install再実行などを利用者が別途明示して実行する。

```sh
bin/agentic-loop doctor --format json
```

### capabilities: repository能力manifestの読み取り

`bin/agentic-loop capabilities`（[運用ドキュメント](capability-manifest.md)、[ADR 0018](../decisions/0018-repository-capability-manifest.md)）は、`.agentic-loop/capabilities.toml`（存在すれば）を検証済みの形で読み取り専用に表示する読み取り専用コマンドである。完全検証・短時間検証・secret guard・対応platform・重要directory/ownership・変更禁止領域・外部環境・release/deploy有無・利用可能skill・想定実行時間を、worker・`doctor`・CI・自己診断が共有する単一のparser/validator（`bin/lib/agentic-loop/capability.sh`）経由で提示する。manifestが存在しない場合は任意設定として警告のみで終了code 0、存在するが安全性検証（pathのworkspace外参照や`..`、commandのシェルmetacharacter）や`schema_version`（未対応値は暗黙fallbackせず失敗）に違反する場合は終了code 1になる。

```sh
bin/agentic-loop capabilities --format json
```

### upgrade: 導入済みFoundationの安全な更新

`bin/agentic-loop upgrade`（[運用ドキュメント](upgrade.md)、[ADR 0009](../decisions/0009-foundation-upgrade.md)）は、既定では書き込みを一切行わないdry-runで、追加・更新・利用者編集との競合・削除候補・設定migrationを日本語で表示する。`--apply`で実際に適用し、破壊的・不可逆・追加費用・権限変更を伴う項目は`--approve`なしでは適用しない。適用前後で`doctor`と完全検証を実行し、失敗時は適用状態を保持したまま`--rollback`または再実行を案内する。Supervisorが稼働中の`--apply`と、明示的なrevision指定を欠く実行はいずれも拒否する（`main`への暗黙追従はしない）。

利用者は要求をIssueとして登録し、7個の `category:*` のうち1つと `agent:queued` を付ける。取得順は本文markerの数値priority（0-100・降順、[ADR 0015](../decisions/0015-numeric-priority-marker.md)）、category、作成日時、Issue番号の順とする。priorityが未設定（markerなし）のIssueは0扱いで最下位となり、以降はcategory→作成日時→Issue番号の従来順で並ぶ。category順は `loop-continuity`、`confidentiality-incident`、`integrity-incident`、`availability-incident`、`bug`、`feature`、`improvement` で固定する。`bug` は既存動作の不具合修正を表し、実害を伴うincident（confidentiality/integrity/availability）とは区別する。複数のpriority markerがあるIssueは最も大きい有効値を使う。priorityの設定・更新は `bin/agentic-loop priority ISSUE N`（0-100）で行う。依存関係は後述の「Issue間の依存関係」に従ってGitHub標準機能またはIssue本文に明記する。変更が及ぶpathやexternal環境が分かる場合は、後述の「変更競合の予防」に従って本文へ `agentic-loop:scope` markerを1行記載する。不明な場合は記載を省略してよく、安全な既定動作（`unknown_scope`）にフォールバックする。回答は `agent:needs-input` のIssueへコメントする。

### priority: 数値priorityの設定・更新

`bin/agentic-loop priority ISSUE N` は、Issue の priority を 0-100 の整数 `N` に設定する。実行者は `dispose` / `resume` と同じく GitHub 認証済みで対象repositoryの write/maintain/admin 権限が必要である。処理は次のとおりで、queue 処理の GraphQL 呼び出しを増やさない。

1. Issue を REST で取得し、本文の既存 `agentic-loop:priority` marker 行を削除して末尾に `<!-- agentic-loop:priority N -->` を1行 upsert する（本文の他の内容は保持）。
2. 移行期に残存する `priority:*` label があれば Issue から外す。
3. `<!-- agentic-loop:priority-set schema=1 actor=... issue=... value=N at=... -->` 監査コメントと日本語説明を記録する。

読み取り確認は `bin/agentic-loop status` の `queue.candidates[].priority`、または `bin/agentic-loop metrics --format json` の `by_priority` で行える。手動で marker を編集しても同じ値が読み取られる（複数 marker は最大値、範囲外・不正値は無視）。

### 対話要求の受付

通常のbuild・変更要求を受けた対話中のAgentは、次の順序で経路を決める。

1. 読み取り専用の質問、診断、status確認、`start`・`stop`などの運用操作はIssue化しない。同期実行または直接実装を利用者が明示した場合も受付を省略する。
2. `.agentic-loop.toml` と実行可能な `bin/agentic-loop` があり、`gh`が対象repositoryとIssueを参照・更新できることを確認する。`bin/agentic-loop status` でSupervisorの状態を観測するが、`running` は受付条件にしない。Supervisorはclaim開始条件であり、Issue永続化の条件ではない。
3. API呼び出し前に、要求経路ごとの重複確認範囲を決める。
   - 利用者が「Issueを作って」のように新規Issue作成を明示した場合は、open Issue一覧・候補本文・コメントの重複検索を呼ばず、新規作成する。ただし同じ指示に「既存Issueがあれば再利用」「重複確認して」も含まれる場合は検索する。
   - 通常の自然言語build・変更要求を自動的にキューへ送る場合は、`agent:*` 状態Labelを持つopen Issueだけをactive Issueとして確認する。titleとbodyを検索し、目的と対象範囲が近い候補だけcommentsを確認して、同じ利用者結果なら再利用する。
   - コード診断・定期監査は従来どおりopen・closed Issueを検索し、同じ所見を重複作成しない。
4. 要求を `loop-continuity`、`confidentiality-incident`、`integrity-incident`、`availability-incident`、`bug`、`feature`、`improvement` の順に評価し、該当する最上位の `category:*` を1個選ぶ。incidentはCIAへの実害で分類し、単なる重要度では選ばない。既存動作の不具合修正はincidentではなく `bug` とする。分類不能時は `category:improvement` を安全な既定値として使い、queued中に再トリアージする旨を記録する。
5. 重複Issueが `agent:running` ならURLと状態を報告して終了し、`agent:queued` なら選択カテゴリが1個だけになるよう確認して再利用する。それ以外は他の `agent:*` 状態Labelを外し、カテゴリ1個と `agent:queued` を付ける。重複がなければ要求・制約・完了条件を本文にしたIssueを作り、カテゴリと `agent:queued` を同時に付ける。
6. 新規作成、再キュー、または再分類の直後に `bin/agentic-loop sync-issue ISSUE_NUMBER` を実行する。Supervisorのclaimを待たずProjectへ追加し、一時障害時は再試行queueへ永続化する。Project障害はIssue受付を停止しない。
7. Issueを再取得し、open、カテゴリが1個、かつ `agent:queued` または `agent:running` であることを確認する。URL、カテゴリ、状態を報告し、直接実装せず終了する。手順2でSupervisorが停止中だった場合は、Issueを登録済みであること、Supervisorが停止中であること、処理開始にはSupervisorの起動が必要であることも報告する。

Supervisorがclaimした `agent:running` Issueのworkerは受付を再実行しない。元Issueとコメントを要求として、専用branch/worktree、全検証、secret guard、commit、push、PR、required checks、review対応、merge、default branch確認、branch/worktree cleanupまで進め、再帰的な代替Issueを作らない。

### カテゴリの修復とincident取扱い（[ADR 0028](../decisions/0028-content-based-category-triage-and-bug-category.md)）

Supervisorはclaim前、`setup`、およびProject同期の再処理時にqueued Issueを検査する。カテゴリが複数のIssueは上記順位の最上位だけを残す。カテゴリなしのIssueは、タイトル・本文・commentをキーワードで判別し、`loop-continuity`（例: supervisor/worker/queue/lease/claim/資源枯渇/スケーラビリティ/ポストモーテム/トレーサビリティなどloop自身の運用に関する語）・`bug`（例: バグ/不具合/誤動作/regression/crashなど既存動作の破綻を表す語）・`feature`（例: 新機能/を実装する、など新規capabilityを表す語）のいずれにも該当しなければ、安全な既定値 `category:improvement` を補う。この自動判定はconfidentiality/integrity/availability-incidentを対象にしない（実害の確認が必要なため）。人手で付与済みの単一カテゴリは上書きしない。どちらの補正もqueued中にLabelを1個だけ残せば再分類でき、次のreconcileでProjectの `Category` と一致する。たとえばqueue停止やworker復旧障害は `loop-continuity`、認証情報の露出は `confidentiality-incident`、artifact改変は `integrity-incident`、利用機能の停止は `availability-incident`、既存機能の不具合修正は `bug`、新機能は `feature`、文書整理は `improvement` とする。loop自体が停止した可用性障害は `loop-continuity` を優先する。

incident Issueには、秘密の値、攻撃手順、不要な個人情報を本文・コメント・Label・Projectへ転記しない。LabelとProjectにはカテゴリ名だけを保存し、詳細証跡は承認された秘密保管境界で管理する。

### 安全なfallback

Supervisor停止中でも、キューのファイル、対象repositoryとIssueのread/write、作成・更新後のopen・カテゴリ1個・queued状態を確認できれば受付は成功である。`sync-issue` がProject同期失敗を永続的な再試行queueへ保存できた場合も受付を成功として扱う。

キューのファイル、GitHub repositoryまたはIssueのread/write、作成・更新後のopen・カテゴリ1個・queued/running状態を確認できない場合は安全なfallbackとし、失敗した確認項目を明示して成功扱いしない。確認目的の追加Issueを作成せず、登録された可能性のあるIssueと並行して同じ変更を実装しない。Supervisor停止はこのfallbackと区別する。

読み取り専用要求と運用操作はその場で続行する。通常の変更要求は、専用branch/worktreeを利用でき、追加費用・秘密・破壊的操作に関する判断が不要な場合に限り、キューが利用不能であることを明示してworker workflowを同期実行する。それ以外は復旧方法または必要な判断を正確に提示して停止する。明示された同期・直接実行も同じworker workflowと不変条件に従う。

`.agentic-loop.toml` の `[queue]` で `poll_seconds`、`max_workers`、`lease_seconds`、`stop_timeout`、`stale_days`、`graphql_reserve`、`rate_limit_cache_seconds`、`api_retry_attempts`、`api_retry_base_seconds`、`max_attempts`、`retry_cooldown_seconds`、`worker_timeout_seconds`、`unknown_scope`、`exclusive_paths` を変更できる。個人環境向けの上書きは git 管理外の `.agentic-loop.local.toml` に同じキーを書けば、キー単位で優先される。設定はTOMLで、読み取りには `yq` を用いる。既定の並列数は4とし、これを超えてむやみに増やさない。worker失敗の多くはtoken枯渇やセッション中断などの一時的要因であるため、失敗したIssueは即座にagent:failedへ留め置かない。Supervisorの`retry_failed`が、開いている全てのagent:failed（過去分・追跡外を含む）を`[queue].max_attempts`（既定3、総試行回数）まで`[queue].retry_cooldown_seconds`（既定600秒）のクールダウンを挟んで自動的にagent:queuedへ戻して再試行し、上限に達してもcloseせず`agent:parked`（open・非claimの人間トリアージ待ち。[ADR 0016](../decisions/0016-failure-park-not-close.md)）へ移す。人手でのラベル付け替えは不要で、`resume`で再投入するかdisposeで終了するかは人間が判断する。workerが実施不要または実施不能と判断した場合は`AGENTIC_LOOP_RESULT=declined`を返すが、worker自身はcloseせず`agent:needs-input`へ載せ、認可済み運用者の判断（`dispose`等）を待つ。`[budget].weekly_reserve_percent` は緊急枠の確保用で、週次利用率が `100 - 値` を超える間はSupervisorが新規Issueのclaimを一時停止し、回復すると再開する。利用率はheadlessで取得できるCodexのセッションログ（最新の `token_count` の `secondary.used_percent`）から読むbest-effortで、取得できない場合やCodex以外のproviderのみの場合はfail open（claim継続）とする。0で無効化できる。増加はCodex契約上の制限、Git競合、端末資源を確認してから行う。stopは新規claimを止め、workerをdrainする。`STOP_TIMEOUT=0` は完了まで待つ。

### plan段・exec段の2段実行

各Issueは2段で処理する。まず調査と計画だけを行う高品質な**plan段**（Codexは `--sandbox read-only` で実行し、書き込みを行わない）、続いて計画に従って実装・検証・PR・mergeまで行う低コストな**exec段**である。高コストな推論を計画に集中させ、実作業は安価に回す。exec段が完了条件を満たせない場合は、`[agent.retry].plan_max`（既定1回）まで、より高いreasoning effortでplanを見直して再実行する。中断後の再開時は既存のworktree・branch・commit・PR・merge状態をGit/GitHub APIから観測してphaseを判定し、既存の成果物を再利用する（[中断からの再開](#中断からの再開)、[ADR 0004](../decisions/0004-worker-resume-and-handoff.md)）。

**局面ごとにprovider・model・reasoning effortを指定できる。** 例えばplanはCodexのフラグシップを高effortで、execはopencodeで、のように混在させられる。段がproviderを省略すると `[agent].provider` を継承する。`reasoning_effort` はCodexのみ（既定 plan=`high` / exec=`low`）、opencodeのmodelは `provider/model` 形式である。

```toml
[agent]
provider = "codex"            # 既定provider

[agent.plan]
provider = "codex"
model = "gpt-5-codex"
reasoning_effort = "high"

[agent.exec]
provider = "opencode"
model = "anthropic/claude-sonnet-4"
```

### プール・モデルの優先順位と枠枯渇・回復（[ADR 0012](../decisions/0012-provider-pool-fallback.md)）

plan/exec/diagnoseの各phaseは、`[[agent.<phase>.tiers]]` で「プール（=サブスク、quota境界）」と「プール内モデル（優先順位）」の2階層を宣言できる。実行時（`agent_pick_tier <phase>`）は、枯渇していない最上位プールの、使用率閾値（`max_usage_percent`）を超えていない最上位モデルを選ぶ。

```toml
[agent.plan]
[[agent.plan.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "high"
models = [{ model = "gpt-5.6-sol", max_usage_percent = 60 }]

[[agent.plan.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "high"
models = [
  { model = "opencode-go/gpt-5.6-luna", max_usage_percent = 60 },
  { model = "opencode-go/kimi-k2.7-code", max_usage_percent = 85 },
  { model = "opencode-go/deepseek-v4-pro" },   # 閾値省略 = 最後まで使う
]
```

`tiers` 未設定のphaseは、従来のscalar `provider` / `model` / `reasoning_effort` を「暗黙pool=provider名、models 1件、max_usageなし」の1 tierとして解釈する（後方互換）。`[agent].provider` 継承も現行どおり。

枠の枯渇・回復はプール単位で記録する。workerがプール枯渇（quota / 429 / usage limit / `insufficient_quota` / credit balance）と分類した失敗では、該当プールのmarker（`.git/agentic-loop/pools/<pool>/exhausted`）に回復予定epochを書き、Issueを `agent:queued` へ戻す（attemptsはクリア）。次回claimで `agent_pick_tier` が次のプール・モデルを選ぶため、部分枯渇では他プールで継続する。**グローバルpause（`agent-exhausted`）は全候補プールが同時に利用不可のときだけ**発動し、statusにはプール別または「全プール利用不可」の理由が表示される。プール使用率の実測は、codexはセッションログ、opencode goは `GET https://opencode.ai/zen/go/v1/usage`（`~/.local/share/opencode/auth.json` の `opencode-go` keyで認証、key値はIssue/PR/logへ転記しない）から読み、読めない場合は `EXHAUSTION_PAUSE_SECONDS` の固定cooldownにフォールバックする。回復は「usage実測で回復」→「cooldown経過」の順でmarkerを削除して優先候補へ戻る。usage APIは `USAGE_CACHE_SECONDS`（300秒）で間引きされる。claudeのように使用率を実測する経路がないproviderのみのpoolでは、cooldown経過後の再試行が再び枯渇を検知するたびに連続回数を数え、既定の固定cooldownから指数的にpauseを延ばす（上限`EXHAUSTION_BACKOFF_MAX_SECONDS`=6時間）。実測が使えるpoolでの再exhaustedは実測そのものが根拠になるため、この指数backoffは適用しない。連続回数は、providerが具体的なreset時刻を示したとき・実測で回復を確認したとき・当該poolでのstageが実際に成功したときにリセットする（Issue #158、[ADR 0027](../decisions/0027-exhaustion-hardening.md)）。

モデル固有失敗（`overloaded`、model解決失敗など）はプール枯渇ではなく、**同一プール内の次モデル**へ同一stage内で即時切り替える。`max_usage_percent` はプール内モデル降格用で、`budget.weekly_reserve_percent`（緊急枠のclaim全体pause）とは役割が異なる。空result + 非0 exitは後方互換のためプール枯渇寄りとして扱う。

SupervisorとworkerはGit common stateにGraphQLの残量・reset時刻を短時間cacheして共有する。Issue一覧、Label、comment、heartbeat、PRのmerge確認などloopのcore操作はREST APIを使い、GraphQLはbest-effortのProjects操作だけに限定する。残量が `GRAPHQL_RESERVE` 以下ならProjects item・field同期だけを抑制し、Issue Labelを正本とするqueue処理は継続する。reset後は次のProjects操作が最新残量を再取得し、冪等な同期を再開する。既定値は500であり、0にすると残量によるProjects保護を無効化する。

Supervisorは1 poll内の状態別処理をOpen Issue snapshot最大2回（maintenance前と、状態遷移を反映するclaim直前）で共有する。Project ID・field/option ID・最大1,000件のitem対応はSupervisor process内で1回取得して再利用し、field更新ごとに全item・fieldを取り直さない。claimとlease復旧が読むcommentは現在のlease期限に関係する更新期間へ限定し、native dependencyの結果は120秒cacheする。

GraphQL枯渇時もREST APIのquotaは別に確認できる。`gh api rate_limit --jq '.resources | {graphql,core}'` で現在値を確認する。GraphQLのreset前にSupervisorを繰り返し再起動したり、`gh issue list --limit 1000` や `gh project item-list --limit 1000` を手動で反復したりしない。

Project同期はProject全itemを走査しない。対象Issue/PRのcontentから `projectItems(first:20)` を直接照会し、pageInfoに従って必要な場合だけcursorを進める。Project IDとfield/option IDだけをプロセス内metadata cacheに置き、item IDとfield値はreconcileごとに再読込する。desired valueと現在値が一致するfieldは更新せず、driftがあるfieldだけを更新する。一時障害の再試行は1 pollあたり10件に制限し、各Projects操作の前には通常reserveに加えて25 pointの操作余裕を要求する。

Projectはbest-effortな投影先であり、唯一の正本はGitHub IssueのLabelである。`Agent status` は唯一の `agent:*` Label（`queued`→Queued、`running`→Running、`needs-input`→Needs input、`in-review`→In review、`completed`→Done、その他の終端・保留状態も同名の選択肢）から、`Category` は唯一の `category:*` Label（`loop-continuity`→Loop continuity、各incident→対応するincident、`bug`→Bug、`feature`→Feature、`improvement`→Improvement）から導出する。`Blocked by` は最新のnative dependency/bodyの `Blocked by:` と現在のscope競合から導出する。Labelが欠落・複数、またはIssue REST取得に失敗した場合はProjectを書き換えない。

`project-pending` はIssue番号だけを保存する再試行ヒントであり、過去の状態・カテゴリ・noteを保存しない。Supervisor起動時にはopen/closedを含むIssue snapshotからヒントを再構築するため、ファイルの消失、host間の重複、強制終了は正しさに影響しない。各番号は、最新Labelの取得、対象itemの取得、必要なmutation、mutation後のfield再読込、Issue `updated_at` の再確認に成功した場合だけackする。途中終了、reserve不足、REST/GraphQL失敗はentryを残す。

GraphQLの`errors`、repository/content欠落、ページ取得失敗は「未所属」とは区別する失敗であり、item-addやitem-editを実行しない。正常な未所属もfield reconcilerは変更しない。membershipの追加とAuto-addは #114 の責務であり、Auto-addでitemが現れた次回reconcileでfieldを投影する。複数hostはsingle-writerロックを置かず、同じ最新Labelからdesired valueを導出して現在値が一致すればmutationしない冪等収束に統一する。書込みの前後でLabel更新を検査するため、競合した古いsnapshotはackされず次回同期で収束する。外部でProject fieldが変更された場合は、次の `sync-issue` またはSupervisor再起動・再試行で再読込して修復し、適用後に読み戻して確認する。

REST APIはrate limit、secondary rate limit、HTTP 429/5xx、timeout、connection resetなど明示的な一時障害だけを指数backoffで既定3回まで再試行する。認証・権限・入力不正など恒久的な4xxや、冪等性を確認できない操作を無制限に再試行しない。retry回数と待機は日本語でlocal logへ記録し、秘密やresponse本文はIssueへ転載しない。上限到達後は既存のlease、worktree、branchを保持し、Supervisor再起動時のlease復旧で再調査できる。

Supervisorはclaimの直前に、`agent:queued` のまま `STALE_DAYS` 日以上更新されていないIssueを `agent:stale` に遷移し、監査コメントを残してcloseする。再開時はIssueをreopenし、要求を確認・更新して `agent:queued` を付ける。`STALE_DAYS=0` は自動closeを無効にする。queued以外のrunning、needs-input、failed、in-reviewや通常の未キューIssueは対象外である。

## 変更競合の予防

密接に関連するIssueを複数workerが同時に処理してmerge conflictや手戻りを起こす可能性を、claim前に検出して**構造的衝突（rename/move・directory再編）だけを直列化**する。通常の同一file編集は並列実行し、merge時に既存のrebase・再検証経路で収束させる。Issue本文（または最新の `agentic-loop:scope` コメント）に次のmarkerを1行記載すると、影響が及ぶpathとexternal環境を宣言できる。

```
<!-- agentic-loop:scope paths=bin/agentic-loop,docs/operations/ env=github-project structural=1 -->
```

`paths=` は対象file・directoryのカンマ区切りで、末尾に `/` を付けるとそのdirectory配下すべてを含む。`env=` は外部環境やmigrationなど、pathで表現できない対象をカンマ区切りの名前で宣言する。repository全体に及ぶ場合は `paths=*` とする。計画がfileのrename/moveやdirectory再編を含む場合のみ `structural=1`（または `true`）を付ける。不正な文字を含むtokenや空のtokenは破棄され、有効なtokenが残らなければ後述の「scope不明」として扱われる。

claim前、queued Issueのscopeはbody（一覧取得時に既に取得済みで追加API呼び出しは発生しない）から解決する。running Issueの実効scopeはGit common state（`.git/agentic-loop/scope/issue-<番号>`）にcacheし、Supervisor起動時にrunning Issue分だけ再構築する。実行中workerはplan段完了直後にscope markerを検出してcacheへ反映し、exec段完了直後には実測の変更範囲（`git diff --name-only`）でcacheを補正する。補正時はrenameを検出できる `git diff --name-status -M` も見て、rename/move・同一directoryの多数の削除＋追加をstructuralとして検知し、そのpathと `structural` sentinelをcacheへ加える。cacheは既存宣言との和集合として更新され、決して縮小しない。scopeが変化したときだけ、audit用のIssueコメントを1回記録する。

競合判定は「直列化すべきhard conflict」と「並列許容のsoft overlap」を区別する。直列化するのは次のいずれかが真のときだけである。

| 種別 | 条件 | 理由token |
| --- | --- | --- |
| 全体 | いずれかが `paths=*`、または `exclusive_paths` に昇格 | `*` |
| 未宣言 | 双方がscope未宣言（`unknown_scope=isolated`） | `unknown` |
| 外部環境 | `env:` が完全一致（rebase不能な外部資源） | `env:NAME` |
| 構造的 | いずれかが `structural`（宣言または実測）で、かつpathが `/` 境界で重なる | `structural:path` |

これ以外の重なり、つまり双方が通常のpath編集で同一file・directoryへ触れる場合は**soft overlap**として並列実行する。実際のmerge conflictはworker既存の経路（default branch更新後のrebase・再検証、`agent:failed`→retry）で吸収される。この緩和のため、過去に同一path宣言を全直列化していた時代の `scope-conflict` 待ちは、構造的衝突と、`*`／`unknown`／`env:` の衝突だけに限定される。

Supervisorは各pollで取得済みのOpen Issue snapshotを正本としてlocal scope/conflict cacheを照合する。`agent:running`でなくなったIssueのscope cacheと、待機側が`agent:queued`でない、または競合相手が`agent:running`でないconflict cacheはlocal stale stateとして除去し、Projectの`Blocked by`投影も空へ収束させる。これにより、別hostがIssueをclaim・完了した場合も、停止中でないSupervisorが再起動を待たず次のpollで追随する。照合用のIssue単位API呼び出しは追加せず、別hostのworker、lease、worktree、branchには触れない。

hard conflictを検出したqueued Issueはclaimせず、priority・category・created_at・Issue番号による既存の取得順を変えずに次の非競合Issueへ進む。競合が解消すれば、待機していたIssueは本来の順位で自然にclaimされる（恒久的な順位降格や飢餓は発生しない）。soft overlapでは待機も `scope-conflict` コメントも出さない。競合判定はworker数上限のhard constraintと既存のqueue処理（budget guard、stale triage、retry）の内側で働くfilterであり、後述の「Issue間の依存関係」によるclaim前block判定はこのfilterの手前に位置づける。

scopeを宣言していないIssueの既定動作は `[queue].unknown_scope`（既定 `isolated`）で制御する。`isolated` は未宣言scope同士でのみ競合し、同時に走る未宣言scope workerを常に1件に制限する一方、宣言済みの独立scope Issueとは並列に走る。`exclusive` は未宣言scopeをrepository全体として扱い、`open` は未宣言scopeの競合判定を行わない（本機能の実質無効化）。`[queue].exclusive_paths`（既定は空）にcomma区切りのpathを設定すると、宣言scopeがそのpathと重なるIssueをrepository全体として扱う（共有基盤file・生成物・migrationなど）。両設定の不正値は起動時検証と `doctor` が失敗として報告する。

`bin/agentic-loop status` は running Issueの実効scopeと、hard conflictで待機しているIssue・相手Issue番号・理由token（`structural:...`／`*`／`unknown`／`env:...`）を表示する。GitHub Projectには `Blocked by` というTEXT fieldを冪等に用意し、相手Issue番号と理由tokenだけを書き込む（Issue本文や秘密情報は転記しない）。GraphQL残量が不足する場合はProjects同期のみ既存のretry queueへ退避し、Issue Labelを正本とするqueue処理は継続する。

本機能は実行中Issueの強制停止・再開（別Issueで対応）、AIによるscope・structural推定（コストポリシー順守のため行わない。structuralは明示宣言とGit実測のみ）を対象外とする。通常編集が実際には同一fileへ触れる可能性は残るが、既存のrebase・再検証経路（default branch更新後の競合はworkerが最新branchに対して修正・再検証する）で吸収する。

## Issue間の依存関係

前提Issueが未完了のまま依存先をclaimすることを防ぐ。実効依存の正本は次の2つの和集合（どちらか一方でも依存を主張すれば依存とみなす、fail closed）とする。

1. GitHub標準のissue dependencies（`blocked_by`）。GitHub UI上でそのまま確認・編集できる。
2. Issue本文の1行構文 `Blocked by: #12, #34`（`#`+同一repositoryの正整数を、カンマまたは空白区切りで並べる）。この行は1つのIssue本文に1行だけ許可する。`Blocks: #56` は逆向きの人間向け記述として書いてもよいが、claim判定には使わない。複数行、別repository参照（`owner/repo#1`やURLを含む）、その他の不正なtokenは構文不正として扱う。

native issue dependencies APIが利用できない環境（404）では、本文構文だけで同じ判定が成立する。

依存Issueが「完了」とみなされるのは、closedであることに加えて次のいずれかを満たす場合だけである。closenだけでは完了にしない。

- Supervisorが管理するIssue（`agent:*` Labelを持つ）: `agent:completed` を持つこと（Supervisorがmerge検証済みである証跡）。`agent:failed`・`agent:stale` など他の `agent:*` で閉じられたものは未完了として扱う。
- 人手管理のIssue（`agent:*` Labelを持たない）: `state_reason` が `completed` であること（`not_planned` は未完了）。

claim前、queued Issueは（scope競合判定より前に）依存を検査し、すべて完了していなければ `agent:blocked` へ遷移し、理由code（`incomplete`、`missing`、`cross-repo`、`syntax`、`cycle`、`permission`、`api`）付きの監査コメントを1回記録する。循環依存（AがBに、BがAに依存）は自動解決できないため専用の理由codeで報告し、人手での解消を要求する。API接続や権限の一時障害は即座にlabelを動かさず、claimだけを最初の1回目から抑止したうえで、連続して問題が続いた場合だけ `agent:blocked` へ遷移する（30秒pollごとのblipでlabelが往復しないようにするため）。`agent:blocked` は毎pollで依存の充足を再評価し、すべて解消すれば人手のLabel操作なしに自動的に `agent:queued` へ戻り、以後は通常の取得順で扱われる。`agent:blocked` はcategory reconcileとstale triageの対象外である。

GitHub Projectには `Agent status` に `Blocked` を追加し、`Blocked by` TEXT fieldへ相手Issue番号と理由の短い要約を書く（scope競合待ちと同じfieldを再利用し、両者は状態が異なるため書き込みが競合しない）。既存Projectは `bin/agentic-loop setup` の再実行でoptionを追記する。`bin/agentic-loop status` は依存待ちIssueと理由codeを表示する。

## 大きな要求の親子分解

通常は一つのIssueを一つのPRで完了する。独立してmerge・rollback・検証できる成果が2件以上あり、各成果が一workerで完結し、scopeが共有されず、統合受け入れ条件を明記できる場合だけ、planは次の fenced JSON manifest を出せる。

```agentic-loop:decomposition
{"schema":1,"mode":"children","integration_acceptance_criteria":"全体検証に成功する","children":[{"key":"api","title":"APIを更新する","purpose":"…","acceptance_criteria":"…","scope":"paths=api env=github-issue","depends_on":[]},{"key":"ui","title":"UIを更新する","purpose":"…","acceptance_criteria":"…","scope":"paths=ui env=github-issue","depends_on":["api"]}]}
```

keyは一意の小文字英数ハイフン、依存は先行keyだけを参照する。直接子は2〜6、深さは2、全子孫は20までである。不正scope、空の受け入れ条件、未知key、循環、上限超過はGitHubを書き換える前に停止する。Project APIは可視化のbest-effortであり、障害時もIssue Labelとnative relationによるqueueを停止しない。

子はnative sub-issue、native dependency、scope marker、親を指すchild markerを持つ。全関係を作成・確認してからqueueへ公開される。親は子をnative `blocked_by` として待つため、子が単にclosedではなく検証済み完了になるまで統合に進まない。子の失敗・cancel・置換・rollbackは親を完了させず、復旧または利用者判断を必要とする。

## 状態と復旧

- queued: 未取得
- running: leaseを持つworkerが処理中
- needs-input: 不可逆・費用・重大な安全判断または解消不能な権限不足への回答待ち
- in-review: PR確認中（workerが進捗として使用可能）
- completed: workerの完了自己申告に加え、対応branchのmerge済みPRをGitHub APIで確認済み
- failed: mergeを証明できず終了。原因確認後にqueuedを付けて再試行する
- stale: queuedのまま設定日数更新されず、監査コメント付きで自動closeされた
- blocked: 依存Issueが未完了のためclaimを保留中。依存が解消すると自動的にqueuedへ戻る
- paused: 認可済み運用者が`pause`で実行を一時停止（open・非claim）。closeされず、`resume`で一時停止前の状態へ戻る（[ADR 0019](../decisions/0019-issue-level-execution-control.md)）

Supervisorは起動時に加えて各pollでもrunning Issueの最新leaseコメントを読み、期限切れをqueuedへ戻す。これにより、workerがクラッシュしてリースが切れたIssueは長時間稼働中でも自動でキューへ復帰し、agent:runningのまま滞留しない。ただし、完了前に繰り返し停止する（lease期限切れ・急死で `AGENTIC_LOOP_RESULT=failed` すら返さない）Issueが無限に再キューされ続けないよう、claim都度記録される試行回数が `max_attempts` に達した回復対象は、queuedへ戻さず `agent:failed` へ移し、`retry_failed` が `agent:parked`（open・非claim。[ADR 0016](../decisions/0016-failure-park-not-close.md)）へ移す。closeはしない。回復時にいずれかのpoolが枯渇marker中であれば、provider枯渇による停止の可能性が高いと判断し、試行回数の判定に関わらず即座にattemptsをクリアしてqueuedへ戻す（環境要因を要求そのものの失敗として retry 予算から差し引かない。Issue #158、[ADR 0027](../decisions/0027-exhaustion-hardening.md)）。

### ハングしたworkerの検出と停止（[ADR 0006](../decisions/0006-worker-hang-timeout.md)）

lease heartbeatはworkerプロセスが生きている証明にすぎず、作業が進んでいる証明ではない。応答しないAPI待ちや無限リトライでworkerが生きたままハングすると、heartbeatは継続してleaseは切れず、上記の期限切れ回収では検出できない。Supervisorは各pollで（起動時とstopのdrain待ち中を含む）、`[queue].worker_timeout_seconds`（既定14400秒=4時間、`0`で無効化）を超えて実行中のworker（このホストの`workers/<issue>.pid`が生存しているものに限る。他host所有のIssueには触れない）をプロセスグループごと停止する。停止時にいずれかのpoolが枯渇marker中であれば、応答しないAPI待ちはprovider枯渇による停止の可能性が高いと判断し、attemptsをクリアして`agent:queued`へ直接戻す（試行回数を消費しない。Issue #158、[ADR 0027](../decisions/0027-exhaustion-hardening.md)）。枯渇markerがなければ従来どおりIssueを`agent:failed`にして監査コメント（`agentic-loop:worker-timeout`）を1件残し、既存の`retry_failed`（`max_attempts`とクールダウン）による境界付き自動再試行に合流するため、恒常的にハングするIssueが無限に再試行を繰り返すことはない。既定値は保守的な見積りであり、各Issueの`agentic-loop:usage`コメントの`所要=Ns`を参考に、実測に基づいて調整してよい。極端に小さい値（1800秒未満）は`doctor`が「正常なworkerを誤停止する恐れがある」警告を出す。Issue worktreeは対象リポジトリと同じ親ディレクトリの `<repository>-worktrees/issue-<number>` に分離する。workerは専用worktreeの外へ書き込まない。Gitが解決した対象リポジトリのcommon metadataディレクトリと、保護対象だが要求実装に必要な専用worktree内の `.agents` の解決・親子関係の検証に失敗した場合はprovider共通のゲートとしてworkerを起動しない（common directoryとworktree固有Git directoryの親子関係を検証できない場合、root、home、worktree rootのような広い範囲の場合、または `.agents` がsymlinkやworktree外のpathに解決される場合を含む）。追加の書き込み許可の与え方はproviderごとに異なる。CodexとClaudeはこの2ディレクトリを `--add-dir` で明示的に書き込み可能にし、Codexはさらに `--sandbox workspace-write` によるOS levelのsandboxで隔離する。opencodeはOS levelのsandboxを持たず、`--dir <worktree>` で作業ディレクトリを専用worktreeに限定するだけで追加dirは与えないため、隔離は専用worktreeと秘密情報guard hookに依存する残余リスクがある。workerの標準出力・標準エラーはGit管理外の `.git/agentic-loop/logs` に保存し、Issueへ転載しない。ログに秘密が疑われる場合は削除し、資格情報を失効する。各セッション終了時には、費用分析のためprovider、model、reasoning effort、token数、判明すればコスト、所要時間、exit codeをまとめた1行を対象IssueへJapaneseコメントとして記録する。provider固有の使用量が取得できない項目は省略し、記録の失敗はworkerを止めない。秘密情報はコメントに含めない。Project同期は再実行可能であり、`bin/agentic-loop setup` で修復する。

workerが `AGENTIC_LOOP_RESULT=completed` を返しても、それだけでは完了にしない。SupervisorはIssue専用branchをheadとするmerge済みPRをGitHub APIで確認し、PRの `headRefOid` がlocal branch先端と一致することを検証する。さらに、専用worktreeが期待pathでそのbranchを使用し、未commit変更がないことを確認してからworktreeを通常削除し、確認済みOIDとのcompare-and-deleteでlocal branchを削除する。merge未確認、別worktreeで使用中、未commit変更、想定外ref、または削除競合がある場合は `failed` とし、残っているworktreeとbranch dataを保持して安全な再調査を可能にする。

### exec終了プロトコルと外部待機

workerはCI、required checks、AI review、mergeなどの外部完了を同一turn内の前景処理で待つ。`gh pr checks --watch` 等は有限のtimeout単位で実行し、timeout後もpendingなら状態を再確認して繰り返す。background process、別agent、別sessionへの待機委譲、または「待機中です」だけの終了は許可しない。checks未確定、review feedback未対応、merge未実施、default branch検証未完了のいずれかでは最終応答を書かない。この待機は `[queue].worker_timeout_seconds` のworker全体上限の内側で行われ、上限を延長または無効化しない。

正当な終了時、providerは最後の非空行に `AGENTIC_LOOP_RESULT=completed`、`AGENTIC_LOOP_RESULT=failed`、`AGENTIC_LOOP_RESULT=needs-input`、`AGENTIC_LOOP_RESULT=declined` のいずれか一つだけを返す。provider processが正常終了しても有効なmarkerがない場合は、正常な失敗やCI待ちをreplan理由にせず、同一計画に「前景待機を完遂しmarkerを返す」補足を加えてexecを1回だけ即時再実行する。再実行もmarkerなしなら無限再試行・高コストなreplanへ進まず `failed` に遷移する。一方、providerの異常終了、明示的な `AGENTIC_LOOP_RESULT=failed`、token/rate-limit枯渇は既存のbounded replanまたはexhausted復旧処理をそのまま使う。GitHub設定の変更、新規外部service、追加課金は導入しない。

remote branchは復旧可能性を残し、GitHubのPR merge時branch削除設定と責務を分離するため、Supervisorからは削除しない。remote branchの保持または削除はrepositoryのコード化されたGitHub設定に従い、このcleanupの成功条件には含めない。

Supervisorはrepositoryごとのuser-level systemd serviceとして登録され、予期しない終了では5秒後に自動再起動する。`stop`による正常終了では再起動しない。service名は `agentic-loop-supervisor-<repository path>.service` で、`systemctl --user status` と `journalctl --user -u` で確認できる。起動時には生存しないPIDとlockを削除してからlease復旧を行う。Supervisorが停止している場合はstatus、`.git/agentic-loop/supervisor.log`、systemd unit、`gh auth status` を確認する。同じリポジトリを複数端末から処理できる。SupervisorはLabel変更前にGitHub上へ期限付きclaimを作り、同じIssueへ同時にclaimした候補をcomment idで一意に調停する。勝者のclaimはlease heartbeatと同じコメントで更新され、敗者はLabel、Git、workerを変更しない。各ホストの`max_workers`はローカル上限なので、repository全体の上限は各ホストの合計になり得る。費用・GitHub API・端末資源に合わせてホストごとの値を設定する。ホスト間でworktreeやPID fileを共有する必要はなく、共有しないこと。default branch更新後の競合やrequired checks失敗はworkerが最新branchに対して修正・再検証する。

## 中断からの再開

lease切れ、Supervisor再起動、端末再起動、worker異常終了の後にworkerが再起動した場合、`worker()` は既存のIssue専用worktree・branch・commit・PR・check・merge状態をGitとGitHub REST APIから観測し、そこから再開phaseを導出する（[ADR 0004](../decisions/0004-worker-resume-and-handoff.md)）。前任workerの発言やlog内容は信頼せず、観測結果だけを根拠にする。

| phase | 観測条件 | 動作 |
| --- | --- | --- |
| `fresh` | 専用branchが存在しない | 新規worktree/branchを作成しplan/execを実行 |
| `worktree-ready` | branchのheadがdefault branchのtipと同一 | 既存worktree/branchを再利用しplan/execを実行 |
| `committed-unpushed` | local commitがありremoteと不一致 | plan/execを継続 |
| `pushed-no-pr` | remote tipとheadが一致しPRがない | plan/execを継続 |
| `pr-open` | 対応するopen PRがある | 既存branch/PRを再利用する指示付きでplan/execを継続 |
| `needs-rebase` | open PRがdefault branchよりbehind、RESTでmerge不可、または競合状態 | `git merge origin/<default-branch>`でbaseを通常取り込み、競合解消、通常push、checks/review再確認、既存PRのmerge、default branch検証まで継続 |
| `pr-merged` | merge済みPRのhead commitがbranchのheadと一致 | providerを起動せず、cleanupと完了報告だけを行う |
| `needs-decision` | local/remoteが分岐、またはmerge済みPRのhead commitが不一致 | providerを起動せず `agent:needs-input` にし復旧手順を提示する |
| `unsafe-foreign` | 専用worktree pathが別Issueのbranchに登録済み、またはbranchが別worktreeで使用中 | providerを起動せず `agent:failed` にし、既存成果物は一切変更しない |

未commitの変更（dirty）は単独では異常とせず、そのままplan/execへ引き継ぐ。`pr-merged`・`needs-decision`・`unsafe-foreign` はいずれもproviderを起動せずに確定するため、二重PR・二重merge・不正なcleanupは構造的に発生しない。安全に再開できない（`needs-decision`・`unsafe-foreign`）場合、worktree・local branch・remote branchのいずれも削除しない。

観測結果はIssueごと1件の `agentic-loop:handoff` コメント（lease同様PATCHで更新）として記録し、phase・branch・head・remote・PR番号・checks・base・behind・mergeable・dirty・divergedを含む。この内容はGit/GitHubの観測結果だけから構成されるためsecretやlog本文を含みえず、専用のsecret guardを追加で通す必要がない。同じ内容はplan/exec promptの先頭にも「再開コンテキスト」として注入し、workerが既存branch/PRを再利用するよう明示する。`needs-rebase` はchecksが成功していてもbase競合が未解消であることをhandoffと`status`で示し、workerはrebase・reset・force-pushを使わず通常mergeで収束する。

Providerを起動する直前に `worker_confirm_running_label()` がGitHub上のLabelが依然 `agent:running` であることを再確認する。claimからworker起動までの間にIssueの状態が変わっていた場合（stale/duplicateな起動など）、Label・comment・Git状態のいずれも変更せず静かに終了する。

`bin/agentic-loop status` はrunning Issueごとにこのphaseを追加API呼び出しなしで表示する（worker実行中のlocal cacheから読む）。競合待ちは同じ呼び出しで取得した現在のqueued/running集合とlocal conflict cacheの両方が一致する場合だけ表示し、stale cacheを表示やclaim可能件数へ反映しない。`doctor` の残存状態チェックは、残存worktree/branch/logがGitHub上のagent:running Issueに対応していれば成功、対応しなければ警告として区別する。

### 失敗の分類

- `needs-input`: 人の判断が必要（branchの分岐、merge済みPRとのcommit不一致、権限不足など）。返信で自動的にqueuedへ戻る。
- `failed`: 再試行で回復しうる（一時的なAPI障害、破損metadata、別Issueの成果物との競合など）。`retry_failed` が自動的に再試行し、`max_attempts` 到達でも解決不能とみなさず `agent:parked`（open・非claimの人間トリアージ待ち）へ移す。closeしない（[ADR 0016](../decisions/0016-failure-park-not-close.md)）。
- `exhausted`（`agent:queued` へ戻る）: プール枯渇（quota / 429 / usage limit / `insufficient_quota` / credit balance、および後方互換のため空result + 非0 exit）。該当プールのmarkerを書き、全プールが利用不可の間だけSupervisorのclaimを一時停止する。部分枯渇では次claimで他プールが使われる（[ADR 0012](../decisions/0012-provider-pool-fallback.md)）。
- モデル固有失敗（`overloaded`、model解決失敗）: プール枯渇にしない。同一stage内で同プールの次モデルへ切り替える。
- `declined`（`agent:needs-input`。closeしない）: workerが実施不要または実施不能と判断した場合。worker自身はcloseせず、認可済み運用者の判断（`dispose`等）を待つ。

## 要求・変更・検証のトレーサビリティ

workerの完了確定（通常のmerge後、および再開時の`pr-merged`確認の両方）は、`[queue].traceability`が`off`以外のとき、PR本文の`agentic-loop:traceability` fenced JSON blockを評価する`trace_gate`を通る。record不在・検証失敗時、`require`はIssueをcloseせず`agent:failed`にしてworktree/branchを保持し、`warn`は完了を継続しつつ助言commentを1件残す。詳細なrecordの書き方・失敗理由コード・`bin/agentic-loop trace`の使い方は[docs/operations/traceability.md](traceability.md)、設計判断（recordの正本の選び方、識別子の方式、自己申告を信用しない理由、既定modeの根拠）は[ADR 0017](../decisions/0017-requirement-traceability.md)を参照。

mode切替は`.agentic-loop.toml`の`queue.traceability`を`off`/`warn`/`require`のいずれかへ書き換えるだけで、GitHub側の設定変更は不要である。新規導入は`warn`から始まり、記録が定着した運用では`require`へ切り替えることを推奨する。

## 変更影響とリスクのpreflight

planとexecの間には、着手前に変更影響とリスクを判定するpreflight gateがある。plan段は`security`/`confidentiality`/`integrity`/`availability`/`data_migration`/`external_environment`/`cost`/`compatibility`/`release_deploy`/`rollback`の10軸を`agentic-loop:preflight` fenced JSON blockとして自己申告し、`preflight_gate`はこれを単独では信用せず、`.agentic-loop/capabilities.toml`とIssueの変更scopeから導いた`signal`と照合する。破壊的・不可逆・重大costまたはsecurity上のリスク（`approval-required`）、判定不能（`undetermined`）、recordとsignalの矛盾（`signal-mismatch`）は、`[queue].preflight`（既定`warn`）が`off`でない限り既存の`agent:needs-input`へgateし、認可済み運用者が`bin/agentic-loop preflight ISSUE --approve --token TOKEN`で承認するまで処理を進めない。execが実装からPRのmergeまでを1 turnで完遂する構造上、実装中に承認範囲を超えるscope拡大を検出した場合は完了確定（cleanup・close）の直前で再評価し、Issueをcloseせずworktree・branch・PRを保持したまま停止する。record・signal・verdict・mode・承認手順・`category`/検証ハーネス/継続的デリバリー各ポリシーとの関係の詳細は[docs/operations/preflight.md](preflight.md)、設計判断は[ADR 0020](../decisions/0020-change-risk-preflight.md)を参照。

読み取り専用の確認は `bin/agentic-loop preflight ISSUE [--format json]` で行える（signalが`approval`なら終了code 1）。

## flakyテストの検知・限定的隔離

`tests/run-e2e.sh`は、attempt1（`queue`/`lifecycle`/`auxiliary`/`upgrade`4群並列のco-run文脈）が失敗した群だけを、最大2回、単独（isolated文脈）で追加試行し、`scripts/flaky.sh classify`が失敗fingerprint（`FAIL:`行のhash）と試行文脈からverdict（`passed`/`failing`/`flaky`/`flaky-unknown`）を決定する（[ADR 0022](../decisions/0022-flaky-test-detection-and-quarantine.md)）。retryは検知・診断目的に限定し、`tests/flaky-registry.toml`の明示的な隔離entry（期限最長14日、責任者・修復Issue必須）に一致しない限り、`verdict=flaky`は常に非ゼロで終了する。決定的失敗（`failing`）は隔離できない。`devbox run --pure check`とCIのmerge gateはこの機能によって一切弱まらない。詳細は[docs/operations/flaky-tests.md](flaky-tests.md)を参照。

読み取り専用の確認は `bin/agentic-loop flaky [--format json]` で行える（registryが期限切れ・不正なら終了code 1）。`bin/agentic-loop flaky report [--record PATH]` は直近の実行recordから修復Issueを作成・再利用する（`make check`からは自動的に呼ばれない）。

## ポストモーテム

反復失敗の`agent:parked`到達、全provider poolの`all-pools-paused`遷移からは、`postmortem` labelと`category:*`を持つ日本語ポストモーテムIssueが重複抑止付きで自動起票されうる（[ADR 0026](../decisions/0026-postmortem-closed-loop.md)、`[postmortem].auto_detect`）。利用者・workerは`bin/agentic-loop postmortem create`で他の事象も明示的に起票できる。action itemは通常のIssueキューへ入り、既存の依存関係機構がその完了・検証を追跡する。CLI詳細・設定・トラブルシュートは[docs/operations/postmortem.md](postmortem.md)、非難しない原則・起動基準・完了条件の正本は[ポストモーテムポリシー](../policies/postmortem.md)を参照。
