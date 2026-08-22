# 実装・移行roadmap

更新日: 2026-08-22

## 1. 方針

現行Bashへ新architectureを段階的に埋め込まない。`main`から作る専用v2 branchのtracked treeを
bootstrap commitで白紙化し、新しいsystemだけを構築する。v1は既知Stableとして`main`で維持し、
v2が全gateを満たした最後に一括で置き換える。

- 現行GitHub Issue stateと新canonical stateを双方向同期しない
- 現行moduleを新domainへ直接移植しない
- 知識はcontract、fixture、failure scenarioとして移す
- 新Loopが自分自身をdogfoodできる最小vertical sliceを先に通す
- milestoneは作業順序でありPR境界ではない。各milestoneのcheckpointとEvidenceをv2 branchへ残す
- v2内で利用可能になったcapabilityは、その時点からRelease Contractへ追加してPreview実証する

## 2. v2 Repository layout

v2 branchには新実装と、その作業を再開するための正本だけを置く。v1実装を`legacy/`として複製しない。

```text
cmd/control-plane/
cmd/runner/
cmd/bootstrap/
internal/domain/
internal/application/
internal/store/firestore/
internal/api/
internal/scheduler/
internal/release/
internal/runner/
internal/provider/
contracts/
web/
infra/
docs/stable/
docs/preview/
docs/architecture/
test/journeys/
.agents/v2/                  # task ledger、work packet、evidence index
.codex/agents/               # bootstrap期間のproject-scoped agent role
```

旧実装から持ち込むのは、再確認したcontract、fixture、failure scenario、必要なpolicyだけとする。
v1 sourceのarchiveはGit historyとcutover tagを正本とし、v2 treeへ残さない。

## 3. Milestone

### M0: Contract baseline

成果:

- 大原則、ユーザー仕様、domain model、architectureをtracked文書として採用
- Release Contract schema
- OpenAPIとEvent／Work Packet schema
- capability一覧とStable／Preview docs骨格
- Go module、format、lint、domain testの共通入口
- component／contract dependency manifestと選択的CI入口
- Problem Brief、Design Packet、Work Order、Evidenceのschema
- Sol／Terra／Lunaのproject-scoped custom agent定義

完了条件:

- 現行機能棚卸しの全項目にretain／redesign／remove／deferがある
- schema compatibility testが動く
- 代表差分fixtureから期待するCI影響閉包を算出できる
- checkpointだけから別agentがtaskを再開できる
- まだ外部resourceを作らない

### M1: Pure domain core

成果:

- Requirement、Increment、Priority Assessment
- Execution、Lease、fencing
- Control Intent
- Release promotion gate
- Retry Budget、failure taxonomy

完了条件:

- model-based testでSafety Invariantを検証
- database／GitHub／Provider dependencyなし
- stop effective後の副作用permitが全command sequenceで0になる

### M2: Self-host Control Plane

成果:

- OpenTofuによるuser GCP projectへのCloud Run／Firestore／IAP／Scheduler配備
- 専用UIで課題登録、Backlog、Requirement、Control表示
- Firestore current state＋Event＋Outbox
- owner認証、budget hard guard、logical export

完了条件:

- clean projectへapply／verify／rollbackできる
- idle時scale-to-zeroする
- Firestore無料quotaの50% hard budgetを越えない
- GitHub Issueなしで課題を永続化できる

### M3: One Runner・Codex vertical slice

成果:

- Runner enrollment、heartbeat、claim、Lease
- workspace、process supervisor、Secret Broker
- Codex Adapter
- 1 Repository／1 Incrementの変更、検証、integration
- provider-neutral Work Packet

完了条件:

- 課題登録からIncrement Artifactまで通る
- Runner crash後、別Executionで再開できる
- Codex実物のPreview capabilityが成功する
- credentialがControl Plane／Work Packet／logへ入らない

### M4: Control and recovery first

成果:

- pause intake／claim
- graceful／immediate／emergency stop
- Control revision／fencing
- lost／orphan reconciliation
- ambiguous external operation処理

完了条件:

- 接続中Runnerのprocess終了を検証できる
- partition Runnerの古いResultを拒否できる
- stop中にclaim、integration、promotionが成功しない
- 意図的failure injectionの全scenarioが収束する

停止は後付けにせず、Provider追加やself-updateより前に完成させる。

### M5: Preview／Stable release and docs

成果:

- Release Candidate bundle
- Repository Release Contract
- Preview deploy／全capability exercise
- Stable promotion／rollback
- Preview／Stable利用者文書routing
- Requirement completed判定

完了条件:

- このFoundation RepositoryをPreview対象にしてdogfoodする
- Previewを意図的に壊し、旧StableでRequirementを再開する
- docsだけのdriftでもpromotionが止まる
- 全機能Evidenceを持つcandidateだけStableになる

### M6: Claude／opencode and shared provider resources

成果:

- Claude Adapter、opencode Adapter
- Provider Account／pool／quota／circuit breaker
- provider-neutral handoff
- 3 Provider live Release Contract

完了条件:

- 各Providerのsuccess／error／quota／cancelを実物で確認する
- 共通adapter変更を3 Providerすべてで実証する
- Provider変更後もIncrement、Artifact、Evidenceを失わない
- opencodeも共通sandbox境界を越えない

### M7: Multiple Runner／Repository

成果:

- 1 Backlogから複数Repositoryへ関連付け
- Resource Claim、競合、dependency
- 複数Runner capacityとpriority scheduling
- cross-repository Increment

完了条件:

- 2 machine以上、2 Repository以上で並列実行する
- 同一Incrementの二重ownerが生じない
- 1 Repositoryのfailure stormで他の重要Requirementが飢餓しない
- cross-repository rollbackが収束する

### M8: Loop self-update

成果:

- signed Runner bundle
- independent Bootstrapper
- module version manifest
- Repository単位Stable／Preview Loop routing
- schema expand／coexist／migrate／contract
- old module／binary GC

完了条件:

- 新Loopが自分自身の新versionをPreviewへdeployする
- Preview Control Plane／Runnerを壊し、Stable launcherから復旧する
- shared stateを失わずrollbackする
- rollback window中の旧binary／docsを削除しない

### M9: Legacy cutover

成果:

- 現行open Issueのone-time import tool
- legacy stop／drain verification
- cutover manifest
- legacy read-only archive
- GitHub Issue queue codeの削除候補一覧

完了条件:

- import対象と除外対象を人間が確認できるdry-run
- 同一課題を二つのLoopがclaimしない
- active legacy workerが0であることをprocess／GitHub／workspaceから確認する
- import後のRequirement件数とcontent digestが一致する
- rollback rehearsalを完了する

## 4. Legacy data migration

GitHub Issueとの継続同期は作らない。一度だけ次を行う。

1. legacy側の新規claimを停止する
2. local process、lease comment、running label、worktreeを照合してdrain完了を検証する
3. open Issueをread-only exportする
4. title、body、comment内の利用者回答、dependency、disposition、関連PRをmigration inputへ正規化する
5. secret scanし、疑わしいcontentは自動importせず隔離一覧へ出す
6. duplicate／completed／cancelledを分類し、未完了課題だけRequirement候補にする
7. dry-runでProblem Frame候補、関連Repository、content digestを表示する
8. transactionで新Backlogへimportし、`legacy_source`参照を付ける
9. GitHub側へ書き戻さず、archiveとしてread-onlyにする

comment marker、Label履歴、Project fieldを新domain Eventとして完全再現しない。課題、利用者回答、現存Artifact、
必要なEvidenceだけを移す。

## 5. Cutoverとrollback

### Cutover前

- legacy mainを既知Stable revisionへpinする
- emergency修正以外のlegacy機能追加を停止する
- new PreviewのRelease Contractを全実証する
- legacyとnewの両方から同じsource／deploy targetへwriteできるcredentialを同時に配らない

### Cutover

- legacy Controlをstopしverifiedにする
- cutover generationをcanonical stateへ記録する
- new StableだけへRepository integration／deploy credentialを発行する
- imported Requirementをschedulerへ公開する
- UIとStable docsを既定入口にする

### Rollback

new Control Planeをemergency stopし、全Leaseをfenceする。旧legacyを無条件に再起動しない。legacyは
new canonical stateを理解しないため、rollback先はM8で保持した**直前のnew Stable**である。

legacyへの最終rollbackが必要な期間は、新Backlogで新規Requirementを処理せず、cutover直後のread-only
rehearsalに限定する。二つのcanonical storeを運用上の選択肢として長期維持しない。

## 6. v2稼働後のproduct workflow

- 課題は新専用UIへ登録する
- LoopがProblem Frame、priority、Incrementを管理する
- Incrementごとに新module/versionを追加してPreview routingする
- affected deterministic testでfeedbackを得る
- 全componentのaggregate Evidence gate後、Previewへdeployする
- Release Contract全capabilityを実物でexerciseする
- 自動Stable promotionする
- 旧moduleはrollback window終了後に別Incrementで削除する

PRを使用してもよいが、中間integration mechanismに限定し、人間reviewや1 Requirement 1 PRを要求しない。

## 7. Risk reduction order

1. state machineとfencingを先に証明する
2. stopとrecoveryをProvider／機能拡張より先に作る
3. 1 Provider／1 Repositoryの縦切りを通す
4. Preview／Stableと文書を完成させる
5. Provider、Runner、Repositoryを横に増やす
6. self-updateを最後に閉じる
7. legacyを切り離す

UIの見栄え、複雑なpriority AI、高度なanalyticsは、この順序を遅らせない。

## 8. 初期defer

- multi-user／tenant
- hosted Runner
- macOS／Windows Runner
- mobile UI
- custom Provider plugin marketplace
- cross-Installation federation
- strict high availability
- managed Firestore backup／PITR
- arbitrary workflow designer
- external GitHub Issue intake

需要を確認するまでdomainとAPIへ予約fieldを増やさない。

## 9. v2 white-slate branch 戦略

v2 は既存実装を段階的に改造せず、専用の長期 branch と worktree でゼロから構築する。ただし orphan branch にはしない。`main` から通常 branch を作り、最初の bootstrap commit で tracked tree を意図的に白紙化して v2 の原則、設計、環境、最小 scaffold だけを置く。これにより最終的な `main` への置換 merge でも共通祖先を維持できる。

- v2 開発中の `main` は v1 の emergency fix に限定する。
- milestone や task ごとの PR は作らず、検証済み checkpoint commit を v2 branch に直接積む。
- 並列作業では各 task を一時 branch／worktree に隔離し、coordinator だけが検証後に v2 へ統合する。統合に PR は必須としない。
- v1 の emergency fix は v2 に必要かを個別に再評価し、旧構造をそのまま移植しない。
- M9 の全 gate を満たした時点で、一つの最終 merge により `main` を v2 へ置き換える。

branch 作成と大量削除の実行時には、対象 branch、worktree、復旧点を read-only check で確定し、明示的な preflight 承認を得る。

## 10. 会話に依存しない checkpoint

モデルの context や token 残量を進捗の保存先にしない。v2 branch 内の機械可読な作業台帳を正本とし、各 task は少なくとも次を残す。

- problem、期待 outcome、不変条件、非目標
- 依存 task と変更可能な境界
- 設計判断と未決事項
- acceptance criteria と実行すべき validation target
- 変更済み file、成功／失敗 evidence、現在の blocker
- 次の agent が安全に始められる一つの action

分析、詳細設計、実装、検証の各境界で checkpoint commit を作る。agent が token 枯渇、停止、障害で終了しても、次の agent は branch、task ledger、evidence、必要なら限定された dirty diff だけから再開できなければならない。raw conversation transcript は補助情報であり正本にしない。
