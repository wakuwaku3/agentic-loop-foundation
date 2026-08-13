# 0004: 中断したworkerの再開と観測ベースの引き継ぎ

- 状態: 採用
- 日付: 2026-08-13

## 背景

lease切れ、Supervisor再起動、端末再起動、worker異常終了は運用上避けられない（[0003](0003-supervisor-resilience-and-api-budget.md)）。従来の `worker()` は再開時に専用worktreeの有無しか検査せず、次の問題があった。

- worktreeが存在すれば、それが別Issueのbranchに登録されていても無条件に再利用していた。別Issueの成果物の中でproviderを起動しうる構造的なbugである。
- 既存のlocal/remote commit、PR、CI、merge状態を検査せず、常にplan/execを最初から実行していた。merge済みPRがあるIssueを再claimすると、providerを再起動して二重PR・二重mergeを起こす経路が存在した。
- 中断・再開の経緯はlocalの生log（`.git/agentic-loop/logs`）にしか残らず、workerが交代すると経緯を追えなかった。

## 判断

### phaseはGit/GitHub観測から導出し、自己申告を信頼しない

`resume_probe()` が `worker()` の先頭でGit（`show-ref`、`rev-parse`、`worktree list`、`rev-list --left-right`）とGitHub REST（`pulls?state=all&head=...`、必要な場合のみ `commits/<sha>/check-runs`）だけから次のphaseを導出する。worker自身や前任workerの発言は一切参照しない。

| phase | 導出条件 | 再開時の動作 |
| --- | --- | --- |
| `fresh` | branchが存在しない | 新規worktree/branchを作成してplan/execを実行 |
| `worktree-ready` | branchのheadがdefault branchのtipと同一 | 既存worktree/branchを再利用してplan/execを実行（追加API呼び出し0） |
| `committed-unpushed` | local commitがあり、remoteと一致しない | plan/execを継続 |
| `pushed-no-pr` | remote tipとheadが一致し、PRがない | plan/execを継続（PR作成から） |
| `pr-open` | 対応するopen PRがある | plan/execを継続。既存branch/PRを再利用し新規PRを作らない指示を注入する |
| `pr-merged` | merge済みPRのhead commitがbranchのheadと一致 | providerを起動せず、cleanupと完了報告だけを行う |
| `needs-decision` | localとremoteが分岐、またはmerge済みPRのhead commitがbranchのheadと不一致 | providerを起動せず `agent:needs-input` にし、復旧手順を提示する |
| `unsafe-foreign` | 専用worktree pathが別branchに登録済み、またはbranchが別worktreeで使用中 | providerを起動せず `agent:failed` にし、既存成果物は一切変更しない |

dirty worktree（未commit変更）は単独では異常としない。同一Issueの自分の未commit変更として扱い、そのままplan/execへ引き継ぐ。分岐（`needs-decision`）と組み合わさる場合のみ人手判断が必要になるが、分岐自体が既に人手判断を要するため、dirtyかどうかで判定を変えない。

`pr-merged`・`needs-decision`・`unsafe-foreign` はいずれもproviderを起動せずに確定するため、二重PR・二重merge・不正なcleanupが構造的に発生しない。`unsafe-foreign` は当ADRが修正する既存bug（別Issueのworktreeを無条件に再利用しうる）の直接の対策であり、worktreeが実在の別branchに登録されている場合だけ発火し、worktree未登録（既存の `resolve_worker_git_common_dir`／`resolve_worker_agents_dir` が別に検知する破損状態）には介入しない。

### 引き継ぎ成果物はproviderの自己申告ではなく観測結果そのものにする

worker交代時に生logを読み直さず再開できるようにする追加要件（Issue #45コメント）に対し、providerが書く自由記述のnotesファイルをlogから抽出してIssueへ転記する設計は、秘密混入の経路を新設する上に「古い自己申告を信頼する」問題を再生産するため採らなかった。代わりに、`resume_probe()` が導出したphase・branch・head・remote・PR番号・checks・dirty・divergedをそのまま1つの `agentic-loop:handoff` コメント（[0003](0003-supervisor-resilience-and-api-budget.md)のlease同様、Issueごと1件をPATCHで更新）として記録する。この成果物は定義上Git/GitHubの観測結果のみから構成されるため、secretや実行log本文を含みえず、専用のsecret guardを追加で通す必要がない。plan/exec prompt冒頭にも同じ内容を「再開コンテキスト」として注入し、workerが既存branch/PRを再利用するよう明示する。

### 所有権の再確認

`worker_confirm_running_label()` が、Git状態に触れる前にGitHub上のLabelが依然 `agent:running` であることを確認する。claimからworker起動までの間にIssueが別状態へ遷移していた場合（stale/duplicateな `_worker` 起動など）、Label・comment・Git状態のいずれも変更せず静かに終了する。既存のclaim側pidfile検査・lease機構と合わせた多層防御であり、複数マシン間の完全な分散lockは目的としない（[0003](0003-supervisor-resilience-and-api-budget.md)のleaseと同じ既存の設計思想を維持する）。

## 却下した案

- **providerが書くnotesファイルをsecret guard経由でIssueへ転記する**: 引き継ぎ成果物に自由記述を許すと、secret混入・古い自己申告の混在という2つの問題を再導入する。観測結果だけで完了条件のtriageは十分であり、詳細な調査記録は既存のexec prompt・Issueコメント・PR本文で担保できる。
- **ローカルDBやファイルへ再開履歴を正本として保存する**: [0002](0002-github-issue-queue.md)のGitHub Issue正本原則に反し、マルチマシンの将来を閉ざす。
- **分岐・merge不一致を自動でreset/force-pushして解決する**: 履歴を失う可能性がある不可逆操作であり、安全な既定動作は常に人手判断を残すことである。

## 帰結

- 中断→再開1回あたりの追加GitHub REST呼び出しは、branch未作成またはdefault branchと同一の場合0、それ以外で最大2（PR一覧、openPRがある場合のみcheck-runs）。
- merge済みIssueの再claimはproviderを起動せず即座にcleanup・completeへ収束するため、二重PR・二重mergeが起きない。
- 安全に再開できないIssue（分岐、merge不一致、別Issueの成果物）はworktree・branch・remote branchのいずれも削除せず、`agentic-loop:handoff` と理由コメントだけをIssueに残す。
- `bin/agentic-loop status` はrunning Issueごとにこのphaseを追加API呼び出しなしで表示し、`doctor` の残存状態チェックは残存worktree/branchがagent:running Issueに対応するかどうかで成功・警告を分ける。
