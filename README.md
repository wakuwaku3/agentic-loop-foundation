# agentic-loop-foundation

自然言語の要求を起点に、Agentが調査・変更・検証・修正・PRのマージまでを反復するための、言語非依存の最小プロジェクト基盤です。

## 開発と検証

Devboxを導入した `x86_64-linux` または `aarch64-linux` で、次の共通入口を使用します。`devbox.lock`によりBash、Make、設定ファイル（`.agentic-loop.toml`）の読み取りに使う `yq`（yq-go）その他の検証ツールが固定され、CIも同じ環境とコマンドを使用します。

```sh
devbox run --pure check
```

## インストール

Devboxを導入した上で、導入したいディレクトリへ移動し、次の1コマンドを実行します。ホストへ`yq`を別途インストールする必要はありません。インストーラーは`yq`がPATHにない場合、取得した`devbox.json`と`devbox.lock`を使って固定環境内でpreflight、setup、Supervisor起動までを続行します。

```sh
curl -fsSL https://raw.githubusercontent.com/wakuwaku3/agentic-loop-foundation/main/install.sh | bash
```

対象はoriginを持つGitリポジトリである必要があります。空のGitリポジトリにはDevboxによる固定開発環境を含む完全な基盤を作成し、既存プロジェクトには既存のコード化済みツールチェーンを維持したままAgent原則、要求入力Skill、IssueキューCLI、Git hooksによる機密情報ガードを追加します。既存プロジェクトにコード化済み環境がない場合は、導入後の変更を完了する前に追加する必要があります。`git`、`gh`、設定 `agent.provider`（環境変数 `AGENT_PROVIDER` と git管理外 `.agentic-loop.local.toml` による上書きを含む）から解決したAI CLI（`codex`、`claude`、または `opencode`、既定は `codex`）、GitHub認証とProjects権限を変更前に検査し、既存ファイルやhooks設定との競合時は上書きせず停止します。provider=opencodeの場合はCodex CLIが存在しなくてもインストールは成立します。

インストールは対象リポジトリ専用のLabelsとGitHub Projectを冪等に設定し、Supervisorをuser-level systemd serviceとして起動します。Supervisorは予期しない終了後に自動再起動し、Issueキューのcore操作にはREST APIを使うため、GraphQL quota枯渇中もProjects同期だけを抑制して処理を継続します。また、リポジトリごとのuser-level systemd timerを有効化し、15分間隔（最大2分のランダム遅延あり）でローカルの`main` worktreeを`origin/main`へ追従させます。更新はcleanかつlocal `main`が`origin/main`のancestorである場合のfast-forwardだけに限定され、ローカル変更、先行、分岐があれば何も変更せず失敗します。登録状態は`systemctl --user list-timers 'agentic-loop-main-sync-*'`、実行履歴は`journalctl --user -u 'agentic-loop-main-sync-*'`で確認できます。

GitHub Issueが要求と状態履歴の正本で、Projectは障害がキューを止めない可視化層です。中央キューや外部DB、OpenAI API keyは使いません。

大きな要求は、planが安全な分解条件を満たす場合だけGitHub native sub-issuesへ子を作成できます。子は独立したscope・受け入れ条件・依存を持ち、親は子がすべて検証済み完了になった後も統合検証と通常のPR/mergeを終えるまでopenのままです。詳細は[Issueキュー運用](docs/operations/issue-queue.md#大きな要求の親子分解)を参照してください。

インストールはコードベース自己診断のuser-level systemd timerも設定します。週次診断は要件と実装のずれ、構成、skill候補、不要ファイルを調べ、コードを変更せず `diagnosis`、`category:improvement`、`agent:queued` Label付きの日本語Issueを作成してSupervisorへ修正を委譲します。手動実行は `bin/agentic-loop-diagnose`、詳細は [コードベース自己診断](docs/operations/codebase-diagnosis.md) を参照してください。

## AIツールの選択

要求処理のworkerが使うAIコーディングツールは `.agentic-loop.toml` の `[agent]` で選択します（`codex`、`claude`、または `opencode`、既定は `codex`）。同じリポジトリをローカル環境に応じて切り替えられ、特定のツールへ固定しません（[AIツール非依存ポリシー](docs/policies/ai-tool-neutrality.md)）。

各Issueは2段で処理します。まず調査と計画だけを行う高品質な**plan段**（Codexは read-only sandbox）、続いて計画に従って実装・検証・PR・mergeまで行う低コストな**exec段**です。高コストな推論を計画に集中させ、実作業は安価に回します。exec段が完了条件を満たせない場合は、`[agent.retry].plan_max`（既定1回）まで flagship で計画を見直して再実行します。中断後の再開時は既存のworktree・branch・commit・PR・merge状態をGit/GitHub APIから観測してphaseを判定し、既存の成果物を再利用します（[運用ドキュメント](docs/operations/issue-queue.md#中断からの再開)、[ADR 0004](docs/decisions/0004-worker-resume-and-handoff.md)）。

**局面ごとに provider・model・reasoning effort を指定できます。** 例えばplanはCodexのフラグシップを高effortで、execはopencodeで、のように混在させられます。段が provider を省略すると `[agent].provider` を継承します。`reasoning_effort` はCodexのみ（既定 plan=`high` / exec=`low`）、opencodeのmodelは `provider/model` 形式です。

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

**プール・モデルの優先順位付きフォールバック**（[ADR 0012](docs/decisions/0012-provider-pool-fallback.md)）も使えます。`[[agent.<phase>.tiers]]` で「プール（=サブスク、quota境界）」と「プール内モデル（優先順位）」を宣言し、枠を使い切ったら次のプール・モデルへ移り、回復したら優先候補へ戻ります。

```toml
[agent.plan]
[[agent.plan.tiers]]
pool = "plus"
provider = "codex"
reasoning_effort = "high"
models = [{ model = "gpt-5-codex", max_usage_percent = 60 }]

[[agent.plan.tiers]]
pool = "gogo"
provider = "opencode"
reasoning_effort = "high"
models = [
  { model = "opencode-go/gpt-5.6-luna", max_usage_percent = 60 },
  { model = "opencode-go/kimi-k2.7-code", max_usage_percent = 85 },
  { model = "opencode-go/deepseek-v4-pro" },  # 閾値省略 = 最後まで使う
]
```

`tiers` 未設定のphaseは従来のscalar設定と等価です（後方互換）。枠枯渇の回復検知は、codexはセッションログ、opencode goは `GET https://opencode.ai/zen/go/v1/usage`（`~/.local/share/opencode/auth.json` の `opencode-go` keyで認証。key値はリポジトリ・ログ・Issue/PRへ転記されません）から実測し、読めない場合は固定cooldownにフォールバックします。全プールが利用不可のときだけclaimが一時停止され、`status` にプール別または「全プール利用不可」の理由と次に選ばれる候補が表示されます。

個人環境の上書きは git 管理外の `.agentic-loop.local.toml` に同じキーを書けば、キー単位で優先されます（例: 手元ではexecもcodexにする）。

Claudeとopencodeは（Codexと異なり）OS levelのsandboxを持たないため、書き込みの隔離は専用worktreeと秘密情報guard hookに依存します。opencodeは `opencode run --auto --format json --dir <worktree>` で作業ディレクトリに限定して実行し、step-finishイベントからtoken/コストを記録します。設定を変えたら `bin/agentic-loop start`（またはSupervisor再起動）で反映します。

## 要求の入力

`$submit-requirement`（Codex）または `/submit-requirement`（Claude）に続けて、達成したいことを自然言語で入力してください。

インストールされる `.codex/config.toml` により、Codex は承認確認なしで動作し、ファイルの書き込み先はワークスペース内に制限されます。既存の `.codex/config.toml` がある場合は上書きせず、インストールを停止します。

> `$submit-requirement 商品を検索できるWebアプリを作って`

継続処理する要求は対象リポジトリのIssueに `agent:queued` Labelを付けます。制御用CLIは次の6つです。

```sh
bin/agentic-loop start
bin/agentic-loop stop
bin/agentic-loop status
bin/agentic-loop status --watch
bin/agentic-loop tail
bin/agentic-loop doctor
bin/agentic-loop metrics
bin/agentic-loop trace
bin/agentic-loop upgrade
```

これらはinstall時に記録された固定runtime PATHを自動的に復元するため、`devbox run`、`devbox shell`、direnv、ホストへの`yq`導入を意識せず、そのまま実行できます。記録先はGit管理外のrepository local state（`.git/agentic-loop/runtime.path`）です。installは`.git/agentic-loop/runtime`配下に、このFoundation自身の`devbox.json`/`devbox.lock`で固定した永続Devbox仮想環境を所有し、その`bin`ディレクトリを先頭に記録します。これはtarget自身のgit-common-dir配下でinstall/uninstallを跨いで残り続けるため、nixのGC rootとして継続的に保護され、install時の一時的なbootstrap環境（削除され得る）とは独立です（[ADR 0011](docs/decisions/0011-installed-runtime-profile.md)）。何らかの理由でこの永続環境自体が失われた場合も、次回コマンド実行時に一度だけ自動的に再構築を試みます（自己修復）。それでも復旧できない場合のみ、復旧手順を含むエラーで終了します。`start` が生成するSupervisorのsystemd serviceにも同じ固定ツールのPATHが設定されます。

`status` はSupervisorの稼働状態に加え、running Issueごとの経過時間・最終heartbeat・lease期限・worktree・関連PR・進行stage（plan/exec等）・最終進行からの秒・healthy/stalled/timeoutのhealth帯、queuedの件数と次のclaim候補、needs-input/failed/in-review/blocked/staleの件数とURL、staleなsupervisor pidや期限切れleaseなどの運用上の異常を1つの入口にまとめます（[0005](docs/decisions/0005-status-observability.md)）。常に読み取り専用で、GitHub呼び出しは1回の実行あたり最大2回に抑え、異常があっても終了code 0のままです（合否判定は `doctor` の責務）。`status --watch [N]`は既定2秒tickで端末を更新し、TTL cache（既定60秒）によりwatch中のREST読み取りを抑えます。`tail [--issue N] [--follow]`はSupervisor/workerの遷移・progressイベントを時刻付きで流します（REST 0回）。自動化からは `bin/agentic-loop status --format json` を使用できます。詳細は [Issueキュー運用](docs/operations/issue-queue.md) を参照してください。

不要になったIssueはLabelを手で置換せず、認可済みの運用者が `bin/agentic-loop dispose 123 --reason cancelled`、または `--reason superseded|duplicate|merged --target 456` を実行します。write/maintain/admin権限をRESTで確認し、監査marker、Project状態、GitHubの `not_planned` closeを一貫して記録します。`resume 123` は同じ認可を確認して履歴を残したままqueuedへ戻します。詳細は [ADR 0010](docs/decisions/0010-authorized-issue-disposition.md) を参照してください。

`doctor` はGitHub認証とrepository権限、origin/default branch、plan段・exec段が使用する各AI CLI（`codex`／`claude`／`opencode`、それぞれ `AI CLI (<provider>)` として個別に検査）、Devbox、固定runtime（記録済みディレクトリの実在と永続Devbox profileの生存、nix-storeが利用可能な環境ではGC root保護の実証も追加で検査）、hooks、Supervisor、systemd service/timer、GitHub Project、設定、残存worktree/branch/logを読み取り専用で検査します。各結果を成功・警告・失敗に分類し、影響と復旧方法を日本語で表示します。必須条件の失敗がある場合だけ終了code 1、警告だけなら0です。自動監視では `bin/agentic-loop doctor --format json` を使用できます。診断はtoken本体を表示せず、修復は `setup`、`start`、install再実行などの明示的な別操作で行います。

`metrics` は既存のIssue/PR/comment履歴だけから、queue待ち時間・処理時間・失敗率・手戻り・worker稼働率などの傾向を再現可能に集計する読み取り専用コマンドです（[ADR 0007](docs/decisions/0007-loop-metrics.md)）。GitHub REST(core)読み取りは1回の実行あたり最大3回に固定し、Actions/CI・GraphQL・Projects APIは呼びません。追加の外部DB・有料monitoring・API課金は一切発生しません。Issue title・コメント本文・worker識別子は取得・出力せず、worker単位の内訳やランキングも出しません。`--as-of EPOCH`で窓の終端を固定すれば、同じ入力から常に同一の出力が得られます。詳細は [運用ドキュメント](docs/operations/loop-metrics.md) を参照してください。

`trace` は、PR本文の`agentic-loop:traceability` recordとGitHubの観測結果（check-runs、変更ファイル一覧）を照合し、Issueの受け入れ条件が実際に何で満たされたかを確認する読み取り専用コマンドです（[ADR 0016](docs/decisions/0016-requirement-traceability.md)）。`trace ISSUE`は対応PRの評価結果を、`trace --audit`はrecordが不整合なIssueをリポジトリ全体から列挙します。`[queue].traceability`（既定`warn`）が`off`以外のとき、workerの完了確定でも同じ評価を行い、`require`ではrecord不在・不整合時にIssueをcloseせず保持します。詳細は [運用ドキュメント](docs/operations/traceability.md) を参照してください。

`upgrade` は導入済みのFoundationを、利用者の変更を失わず安全に更新するコマンドです（[ADR 0009](docs/decisions/0009-foundation-upgrade.md)）。既定は書き込みを一切行わないdry-runで、追加・更新・競合・削除候補・設定migrationを日本語で表示します。`--apply`で実際に適用し、破壊的・不可逆・追加費用・権限変更を伴う項目は`--approve`なしでは適用しません。適用後は`doctor`と完全検証を実行し、失敗時は`--rollback`または再実行の案内を表示します。詳細は [運用ドキュメント](docs/operations/upgrade.md) を参照してください。

既定では30秒poll、最大4件を並列実行します。運用値は root 直下の `.agentic-loop.toml`（TOML、`yq`で読み取り）の `[queue]` セクションで設定し、個人環境の上書きは git 管理外の `.agentic-loop.local.toml` にキー単位で書けます。lease heartbeatが生きたままworkerが内部でハングした場合に備え、`[queue].worker_timeout_seconds`（既定4時間、`0`で無効化）を超えて実行中のworkerはプロセスグループごと停止され、失敗として自動的に境界付き再試行キューへ戻ります（[0006](docs/decisions/0006-worker-hang-timeout.md)）。設定、状態遷移、lease復旧、Projectの制約、トラブルシュートは [Issueキュー運用](docs/operations/issue-queue.md)、設計上の判断は [0001](docs/decisions/0001-minimal-foundation.md) と [0002](docs/decisions/0002-github-issue-queue.md) に記録しています。
