# 論理architecture

更新日: 2026-08-22

## 1. Architecture style

初期版は、分散microservice群ではなく次の二つを中心にした**modular monolith + remote Runner**とする。

1. Control Plane: UI、API、domain transaction、scheduler、reconcilerを一つのdeployableへまとめる
2. Runner Agent: 利用者machine上でworkspace、Provider CLI、検証commandを実行する

内部moduleは明確に分けるが、独立scaleや障害隔離が実測で必要になるまでnetwork serviceへ分割しない。
これにより、外部同期を減らし、transaction境界を明確にし、self-hostの構成と費用を小さくする。

## 2. Context

```text
                         ┌────────────────────────────┐
                         │ User browser               │
                         │ Backlog / Status / Control │
                         │ Stable / Preview Docs      │
                         └─────────────┬──────────────┘
                                       │ HTTPS
┌──────────────────────────────────────▼─────────────────────────────────────┐
│ User-owned Control Plane                                                  │
│                                                                           │
│  Web/API ─ Application Services ─ Domain Core ─ Persistence/Outbox        │
│                 │                    │                 │                    │
│                 ├─ Scheduler         ├─ Policy        └─ Reconciler        │
│                 └─ Release Manager   └─ State Machines                    │
└───────────┬────────────────────────────┬─────────────────────────┬─────────┘
            │ outbound claim/result API  │ external adapters       │ events
            │                            │                         │
      ┌─────▼───────────┐        ┌───────▼──────────┐       ┌──────▼───────┐
      │ Runner Agent A  │        │ Source/Deploy    │       │ Canonical    │
      │ local machine   │        │ systems          │       │ Store        │
      ├─────────────────┤        └──────────────────┘       └──────────────┘
      │ Workspace mgr   │
      │ Process supervisor
      │ Secret broker   │         Runner B, C ... use the same outbound API
      │ Provider adapter│
      │ Validation exec │
      └─────────────────┘
```

RunnerからControl Planeへのoutbound接続だけを必須とし、利用者machineへのinbound portを開けない。

## 3. Control Plane modules

### Web UI / API

- 課題登録、Backlog、Requirement詳細、Question回答
- Control Intent発行と停止verification表示
- Repository、Runner、Provider Account、Budget管理
- Preview／Stable Releaseと利用者文書の表示
- typed commandをApplication Serviceへ渡し、databaseを直接操作しない

UIとAPIは同じreleaseで配布する。UI固有stateをcanonical domain stateにしない。

### Identity / Enrollment

- 1人のowner identityを認証する
- Runner enrollmentを一度だけの短命tokenで開始する
- Runner public keyとcapabilityを登録する
- enrollment後はchallenge応答から短命session tokenを発行する
- owner sessionとRunner sessionの権限を分ける

### Application Services

1 requestまたは1 background tickのtransaction boundary。

- Commandの認可、schema validation、idempotency
- Aggregateのload、Domain Core呼出し、optimistic concurrency
- current state、Event、Outboxのatomic write
- response DTO生成

外部I/Oをtransaction中に実行しない。

### Domain Core

- Requirement framing／priority decision
- Increment planningとdependency
- Lease／fencing／capacity
- Control effective policy
- Retry／failure transition
- Release promotion gate
- retention eligibility

純粋なstate transitionとして実装し、database、clock、HTTP、Provider CLIへ依存しない。

### Scheduler

- Backlogから実行可能なRequirement／Incrementを抽出する
- Control、dependency、Resource Claim、Budget、Runner Capabilityをfilterする
- 価値、緊急性、risk、cost、学習価値、飢餓時間を比較する
- 選択理由をDecisionとして保存する
- Runnerのclaim requestに対しtransactionでLeaseを発行する

AIにpriority判断を依頼する場合も、AIの回答を直接queue順へ適用せず、構造化Decisionをdomain validation
へ通す。安全上のhard priorityとBudgetはAI判断より先に適用する。

### Reconciler

desired stateとObservationの差を収束させる。

- expired Lease、lost Runner、orphan Execution
- pending Outbox／曖昧な外部operation
- Preview／Stable deploy
- Control Intent acknowledgment／verification
-旧Release、workspace、module、documentのGC eligibility

同じtickを何度実行しても同じ結果へ収束し、1 tickの件数と時間を制限する。

### Release Manager

- Repository Release Contractを読む
- Release Candidate bundleを固定する
- Preview deployと全capability exerciseを計画する
- Evidenceのfreshness、Provider coverage、文書整合を評価する
- channel pointerをStableへ切り替える
- rollback、再開、文書routingを調停する

Release Manager自身もControl Intentとfencingに従う。

### Adapter ports

- Source Control
- Artifact Store
- Application Deploy
- AI Provider Observation（Control Planeには実Provider実行を置かない）
- Notification
- Clock／ID／Signer

Adapterのpayloadをdomain entityとして保存せず、typed Observationへ正規化する。

## 4. Runner Agent modules

### Runner Daemon

- Control Planeへoutboundでheartbeat／claimする
- Control revisionを受信・ackする
- 1つ以上のExecution slotを管理する
- local module／Loop VersionをStableまたはPreview routingに従って起動する
- process終了後もResultが受理されたことを確認するまでcheckpointを保持する

### Workspace Manager

- Repository checkoutとIncrement専用workspaceを作る
- path、symlink、git common dir、ownershipを検証する
- Artifact snapshotとcleanup eligibilityを管理する
- workspace外writeをsandbox policyで防ぐ

Git worktreeは最初のSource Control Adapterとして利用できるがdomain contractにはしない。

### Process Supervisor

- childを独立process groupで起動する
- stdout／stderrを容量制限付きlocal diagnostic logへ送る
- heartbeatとprogress eventを分ける
- graceful checkpoint、TERM、KILLを実施する
- daemon再起動時にlocal processとcanonical Executionをreconcileする

### Secret Broker

- OS keyringまたはlocal secret storeからcredentialを取得する
- Execution、Repository、Provider、期間に限定してchild processへ注入する
- Work Packet、canonical state、Artifactへcredentialを出さない
- output、commit、outbound payloadをsecret guardへ通す
- stop／expiryで新規credential払い出しを止める

Control PlaneはProvider CLIのcredentialを保持しない。

### Provider Adapters

Codex、Claude、opencodeごとに次を実装する。

- capability／version probe
- Work Packetからprovider入力への変換
- process起動contract
- structured result／usage抽出
- error、quota、model failure、transport failureの正規化
- cancel／timeout挙動
- live Preview exercise

Adapter以外のmoduleはprovider固有JSON、exit code、message textを解釈しない。

### Validation Executor

- Repository Contractで宣言されたfast／full／smoke／capability exerciseを実行する
- commandをshell文字列として無制限に解釈せず、検証済みargv／working directory／environmentで起動する
- resultをEvidenceへ正規化する
- outputの容量と秘密を制御する

### Integration Adapter

- commit、branch、PR、merge、artifact publishなどRepositoryごとの統合方式を実行する
- PRは任意であり、人間reviewを待たない
- expected revisionとoperation idで重複を検出する
- expired fencing tokenのArtifactをStable releaseへ含めない

## 5. Module versioning

「既存moduleを直接変更」ではなく「追加・切替・削除」を実現する。ただし初期版から動的plugin基盤は
導入しない。

- module interfaceとserialized contractにversionを付ける
- 新旧implementationを同じsource treeで並存可能にする
- Loop Releaseごとに使用module versionのmanifestを固定する
- Repository単位でStable／Preview Loop Releaseをroutingする
- Runnerは署名・digest検証済みのimmutable binary bundleを取得する
- rollback window中は旧binaryと旧schema readerを保持する
- 参照中Execution、Requirement、Releaseがなくなってから旧moduleを削除する

Goのruntime plugin、任意code download、言語内dynamic linkingは使わない。新moduleはbuild-time registryまたは
別process protocolとして追加し、契約をtestできる形にする。

## 6. State ownership

| State | Owner | 備考 |
| --- | --- | --- |
| Requirement／Increment／Priority | Control Plane canonical store | UIやVCSへ複製しない |
| Lease／Control Intent／Release | Control Plane canonical store | transactional authority |
| Work Packet metadata | Control Plane canonical store | large Artifactはobject store参照 |
| workspace／child process | Runner Agent | canonical Executionへreconcileする |
| Provider credential | Runner local Secret Broker | Control Planeへ送らない |
| source commit／deploy状態 | external system | Observationとして読む |
| user documentation bundle | Release Artifact | channelごとにroutingする |
| raw diagnostic log | Runner local bounded storage | 必要な要約だけcanonical Eventへ送る |

## 7. Communication protocol

初期版はHTTPS JSON APIとする。常時双方向socketを必須にしない。

Runner flow:

1. session認証とcapability更新
2. long pollまたはbounded pollでclaim request
3. Lease、Work Packet、Control revisionを取得
4. progress／heartbeat／checkpointを別endpointへ送る
5. 外部operation前にpermitを再確認する
6. ResultとObservationをidempotency key付きで送る
7. Control Planeのaccept／rejectを受けてlocal cleanupを判断する

各mutationは`request_id`を必須とし、同じrequestのretryで二重適用しない。API versionはURLまたはmedia
typeで明示し、Stable／Preview Runnerの共存期間は後方互換を維持する。

## 8. Availability model

- Control Plane停止中もRequirementとcheckpointはcanonical storeに残る
- Runnerは新規claimとauthoritative副作用を行わず、接続回復を待つ
- Runner停止中も他RunnerがLease expiry後に引き継げる
- Provider停止は該当Provider Accountをwaitingにし、互換Providerまたは回復を待つ
- Source／Deploy system停止はOutboxを保持してbounded retry／reconciliationする
- UI停止は実行中Leaseを即座に壊さないが、emergency control用のCLI/APIを同じControl Planeに残す

Control Planeの高可用clusterは初期要件にしない。managed platformによる再起動とdurable storeで復旧し、
single-user無料枠と単純性を優先する。

## 9. Security boundaries

```text
Browser identity ── owner command authority
Control Plane SA ── canonical store / artifact metadata / signing
Runner identity ── claim / progress / result for enrolled Runner only
Local Provider credential ── one provider account on one Runner
Repository credential ── declared Repository operations only
Deploy credential ── declared Application target and channel only
```

- browser content、Requirement、Provider outputはすべてuntrusted inputとしてescape／validateする
- Runnerが返す成功を単独で信用せず、Evidenceとexternal Observationを検証する
- Artifactはdigestでcontent-addressし、release bundleへ含める前に署名／provenanceを確認する
- Control PlaneからRunnerへ任意shellを送らず、versioned command schemaを送る
- Repository Contractのcommand変更もPreview Release Contractで実証する

## 10. 意図的に採用しないもの

- GitHub Issue／Label／Projectをstate storeにする
- 最初からmicroserviceへ分割する
- 完全event sourcing
- distributed filesystemでworkspaceを共有する
- Runnerへのinbound remote shell
- AI Providerの生conversationをhandoff protocolにする
- 1 Requirement 1 PR
- dynamic language pluginを無署名で読み込む
- test成功だけによるStable promotion
