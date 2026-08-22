# 技術選定

更新日: 2026-08-22

## 1. 結論

初期実装は次を採用する。

| 領域 | 選定 |
| --- | --- |
| Control Plane runtime | Google Cloud Run、request-based billing、min instances 0 |
| Canonical store | Cloud Firestore Standard、default database |
| 定期reconciliation | Cloud Scheduler 1 job → Control Planeのbounded tick |
| Owner／Runner入口認証 | Cloud Run direct Identity-Aware Proxy（IAP） |
| Control Plane secret | Google Secret Manager。Provider credentialは保存しない |
| Container／release image | Google Artifact Registry |
| Control Plane／Runner言語 | Go、同一module内の複数command |
| UI | Go server-rendered HTML＋embedded CSS／vanilla ES modules |
| Runner配布 | Cloud Run release imageに同梱した署名済みLinux binary |
| Runner local state | SQLite（cache／process journalのみ。canonicalではない） |
| Runner sandbox | Linux namespace sandbox＋Increment専用clone／workspace |
| Infrastructure as Code | OpenTofu＋Google provider、version／provider lock固定 |
| Repository environment | Repository Contractが宣言する既存の固定環境。Foundation自身はDevboxを継続 |

CloudはGoogle stackに統一する。AI Provider CLIとsource／deploy targetを除き、別vendorのmanaged serviceを
追加しない。

## 2. 候補比較

| 候補 | 複数machine | scale-to-zero | transaction | self-host容易性 | 無料枠適合 | 判定 |
| --- | --- | --- | --- | --- | --- | --- |
| Cloud Run＋Firestore | 良い | 良い | document transaction | IaCでuser projectへdeploy | 良い | 採用 |
| local server＋SQLite | NAT／TLSが必要 | 常時machine依存 | 良い | appは簡単、network運用が重い | machine次第 | 不採用 |
| GCE＋SQLite/PostgreSQL | 良い | しない | 良い | OS、patch、backupが必要 | region等に制約 | 不採用 |
| Cloud Run＋Cloud SQL | 良い | appのみ | relational transaction | managed | database常時費用 | 不採用 |

Cloud SQLにはtrialはあるが、継続利用の常設無料instanceを前提にできないため採用しない。
[Cloud SQL pricing](https://cloud.google.com/sql/pricing?hl=en)

## 3. Cloud Run

### 採用理由

- requestがなければscale-to-zeroできる
- request-based billingではrequest処理中を中心に課金される
- immutable revision、traffic split、rollbackを提供する
- IAM service accountでFirestore、Secret Manager、Artifact Registryへ最小権限接続できる
- 利用者GCP projectへ同じIaCをdeployでき、single-tenantになる

Cloud Runのcontainer filesystemは破棄されるため、canonical state、session、Outbox、artifactをlocal
filesystemへ保存しない。[Cloud Run overview](https://docs.cloud.google.com/run/docs/overview/what-is-cloud-run)

### 設定

- request-based billing
- min instances: `0`
- service-level max instances: 初期値`2`
- concurrency: 初期値`20`
- request timeout: UI/APIは短く、reconcile tickもbounded batchで終了する
- region: Firestore、Artifact Registryと同一region。利用者に近いGoogle regionを選択する
- direct IAPを有効化し、ownerだけへ`roles/iap.httpsResourceAccessor`を付与する
- preview revisionへtag URLを付け、Stable trafficとは分ける

max instancesは費用とFirestore contentionを抑えるhard guardとして設定する。Cloud Run公式もmaximum
instancesをcost／downstream protectionに使えるとしている。
[Cloud Run scaling controls](https://docs.cloud.google.com/run/docs/configuring)

preview tagを使ってもrevision-level min instancesは設定しない。tag付きrevisionにrevision-level minimumを
設定するとidleでも起動・課金されるためである。
[Cloud Run minimum instances](https://docs.cloud.google.com/run/docs/configuring/min-instances)

### Stable／Preview

- 一つのCloud Run serviceにStable revisionとPreview revisionを置く
- Stableは通常URL、Previewはtag URLで明示アクセスする
- 両revisionは同じFirestore stateをschema互換で読む
- Repository routingとLoop Release manifestが、どちらのRunner binary／moduleを使うか決める
- Preview実証後、Cloud Run traffic targetを同じrevisionへ切り替える
- rollbackは旧revisionへtrafficを戻し、旧Runner bundleを再選択する

Cloud Runはrevision間のtraffic splitとrollbackを標準提供する。
[Cloud Run revision traffic](https://docs.cloud.google.com/run/docs/overview/what-is-cloud-run)

## 4. Firestore

### 採用理由

- serverlessでidle時のdatabase instance費用がない
- 複数documentのatomic transactionを提供する
- transactionは競合時に再実行され、部分writeを行わない
- commit済みwriteに対するstrongly consistent readを提供する
- single-user規模のBacklog／Lease／Event／Outboxには無料枠が十分大きい

[Firestore transactions](https://firebase.google.com/docs/firestore/manage-data/transactions)、
[Firestore consistency and scale](https://firebase.google.com/docs/firestore/understand-reads-writes-scale)

### 無料枠

default database 1個について、1 GiB storage、1日50,000 reads、20,000 writes、20,000 deletes、月10 GiB
outbound data transferの無料枠がある。無料databaseはproject当たり1個で、TTL delete、PITR、backup、
restore、cloneは無料利用に含まれない。
[Firestore quotas](https://firebase.google.com/docs/firestore/quotas)

無料枠をSLAとみなさず、application側に日次hard budgetを設ける。Google Cloud budget alertは超過時の
強制停止ではないため、read／write counterの見積りとControl Planeのhard stopを使う。
[Firestore pricing](https://cloud.google.com/firestore/pricing?hl=en)

### Data layout

```text
installations/{installationId}
repositories/{repositoryId}
requirements/{requirementId}
  /events/{eventId}
  /questions/{questionId}
increments/{incrementId}
  /executions/{executionId}
leases/{scopeId}
runners/{runnerId}
providerAccounts/{providerAccountId}
providerRuns/{providerRunId}
releaseCandidates/{releaseId}
releases/{releaseId}
controlIntents/{intentId}
outbox/{operationId}
artifacts/{artifactId}          # metadata only
capabilityEvidence/{evidenceId}
```

巨大なaggregateを1 documentへ埋め込まない。Requirement current summaryは1 documentに置き、Event、
Execution、Evidenceを別collectionへ分ける。

### Transaction boundaries

- Increment claim: Increment＋Lease＋Runner capacity＋Event
- Control Intent: Control revision＋Intent＋対象summary＋Outbox
- Result accept: Execution＋Increment＋Lease token検証＋Event＋Outbox
- Release promotion decision: Release Candidate＋Evidence summary＋Release pointer＋Event＋Outbox

Firestore transaction callbackは再実行されうるため、transaction内で外部I/O、ID生成、log送信をしない。

### Hotspot対策

- Installation全体のcounterをheartbeatごとに更新しない
- Runner／Lease／Executionごとのdocumentへ分散する
- Event IDは時間単調な連番でなくrandom／time-random複合にする
- scheduler候補query後、選択対象だけtransactionで再読する
- tickごとの処理件数を固定上限にする

Firestore公式は単一documentへの高頻度更新がcontentionとlatencyを起こすと説明している。
[Firestore best practices](https://firebase.google.com/docs/firestore/best-practices?hl=en)

### Backup

Firestoreのmanaged backup／PITRは無料枠外なので初期既定では使わない。代わりにownerが選んだRunnerへ、
Control Planeが日次の整合したlogical exportをstreamし、Runnerが`age`で暗号化してlocal bounded storageへ
保存する。exportはsecret値とraw logを含まない。restore rehearsalをPreview capabilityに含める。

有料managed backupを有効化する場合は、費用上限変更として人間承認を必要とする。

## 5. Schedulingとbackground work

Cloud Runはrequestがなければscale-from-zeroしないため、1つのCloud Scheduler jobが毎分または設定間隔で
`/internal/reconcile`をOIDC認証付きで呼ぶ。jobは次だけをbounded batchで処理する。

- expired Lease
- pending／ambiguous Outbox
- Control verification
- circuit half-open probe eligibility
- retention／GC候補

通常のclaimはRunner requestが直接起こすため、Scheduler tickがqueueをpollしてworkerを起動しない。
Cloud Schedulerはbilling account当たり月3 jobsの無料枠があり、1 jobだけを使う。
[Cloud Scheduler pricing](https://cloud.google.com/scheduler/pricing)

Cloud Tasks、Pub/Sub、Workflowは初期版では導入しない。Outbox＋bounded reconcileで不足するthroughputが
実測された場合だけ追加する。

## 6. Authentication・secret

### Owner

- Cloud Run serviceへIAPを直接有効化する
- owner Google identityだけへIAP accessを付与する
- Control Planeも`X-Goog-IAP-JWT-Assertion`のsignature、issuer、audience、subjectを検証する
- Firestoreをbrowserから直接読ませず、Security Rulesはclient access denyにする

Cloud Run direct IAPはdefault `run.app` URLをload balancerなしで保護でき、追加load balancer費用を必要と
しない。[Configure IAP for Cloud Run](https://docs.cloud.google.com/run/docs/securing/identity-aware-proxy-cloud-run)

### Runner

- IAP用custom OAuth clientとdesktop clientをIaC／bootstrapで作り、programmatic access allowlistへ登録する
- ownerが各Runnerでdesktop OAuth flowを一度実行し、refresh tokenをOS keyringへ保存する
- RunnerはIAP audienceの短命ID tokenを取得してControl Planeへ接続する
- その上で、一回限り・短命のenrollment tokenをownerがUIで発行する
- RunnerがlocalでEd25519 key pairを生成し、private keyをOS keyringへ保存する
- Control Planeはpublic keyだけを保存する
- challenge signature確認後に短命Runner session tokenを発行する
- Runner削除／emergency stopでkeyをrevokedにする

IAPはdesktop user accountのprogrammatic accessとrefresh token flowを公式に提供する。
[IAP programmatic authentication](https://docs.cloud.google.com/iap/docs/authentication-howto?hl=en)

IAP identityはInstallationへの入口認証、Ed25519 keyは個々のRunner認証として二層で使う。OAuth refresh
tokenもProvider credentialと同様にRunner外へ送らない。

### Secret Manager

Control Planeのsession signing key、export signing keyなど少数のcontrol secretだけを置く。Provider、VCS、
deploy credentialはRunner local Secret Brokerに置く。Secret Managerは最初の6 active secret versionsと
月10,000 access operationsまで無料枠がある。
[Secret Manager pricing](https://cloud.google.com/security/products/secret-manager?hl=en)

secret accessをrequestごとに行わず、instance memoryへ短時間cacheする。log、Firestore、Artifactへ値を
出さない。

## 7. Artifact Registry・binary update

- Control Plane containerをArtifact Registryへ保存する
- 同じsource revisionからLinux `amd64`／`arm64` Runner binaryをreproducibly buildする
- Runner binary、module manifest、Stable／Preview docsをcontainer imageへ同梱する
- release manifestへSHA-256 digestとEd25519 signatureを付ける
- 独立した最小Bootstrapperがmanifestを検証し、versioned directoryへdownload後にatomic symlink切替する
- Bootstrapper自身は自動更新対象から外し、人間承認の別手順でのみ更新する

Artifact Registryはbilling account全体で0.5 GiB-monthまでstorage無料枠がある。Stable、Preview、直前rollback
候補にretentionを絞り、不要imageを削除する。
[Artifact Registry pricing](https://cloud.google.com/artifact-registry/pricing)

Runner downloadをControl Plane経由にすることで、local machineへ`gcloud`やArtifact Registry credentialを
必須にしない。download egressはBudgetへ計上する。

## 8. Go

Control PlaneとRunnerをGoで実装する。

- single static binaryを作りやすい
- process、signal、HTTP、concurrency、cross compileが標準libraryで扱える
- Cloud RunとGoogle Cloud client libraryのfirst-class supportがある
- Bashの暗黙globalとsource順依存を、package／interface／typeで排除できる
- Runnerとserverでdomain type、API schema、secret redactionを共有できる

構成:

```text
cmd/control-plane
cmd/runner
cmd/bootstrap
internal/domain
internal/application
internal/store/firestore
internal/api
internal/scheduler
internal/release
internal/runner
internal/provider/{codex,claude,opencode}
internal/adapter/{git,deploy,notify}
web/                         # embedded templates/assets/docs
contracts/                   # JSON Schema / compatibility fixtures
infra/                       # OpenTofu
```

Go toolchain、module dependency、provider CLI、OpenTofu providerはlock fileで固定する。

## 9. UI

React等のSPA frameworkは初期版では使わない。Goの`html/template`でserver-renderし、必要な部分だけ
vanilla ES modulesで更新する。

- build toolchainとdependencyを減らす
- Stable／Preview docsを同じbinaryへembedできる
- URLごとにBacklog、Requirement、Runner、Release、Controlを直接開ける
- JavaScriptが失敗してもreadとemergency stop導線を維持する

進捗更新は短いconditional pollingまたはSSEを選べるが、初期版はETag付きbounded pollingとする。
Firestore realtime listenerをbrowserから使わず、read budgetと認可をControl Planeで一元化する。

## 10. Runner local implementation

- Linux `x86_64`／`aarch64`を初期対応platformとする
- Runner local journal／cacheにSQLiteを使う
- canonical decisionはSQLiteだけから行わない
- Incrementごとに独立cloneを作り、shared git metadataへのwrite依存を避ける
- local bare mirrorをnetwork削減cacheに使えるが、workspaceは自己完結させる
- namespace sandboxでworkspace、toolchain、選択credential以外をread-onlyまたは不可視にする
- Provider CLIはsandbox内でnetworkを利用できるが、Repository／Deploy credentialは必要なcommandにだけ渡す
- child processはprocess groupとcgroup resource limitで制御する

最初からmacOS／Windows Runnerを約束しない。需要が確認された後、同じRunner contractの別sandbox adapterを
追加する。

## 11. Infrastructure as Code

OpenTofuで次を宣言する。

- API enablement
- Firestore databaseとindex
- Cloud Run service、Stable／Preview revision設定
- Artifact Registry
- Cloud Scheduler job
- service accountsと最小IAM
- Secret Manager secret metadata
- Cloud Run direct IAP、owner IAM、programmatic OAuth client／allowlist
- log retention／budget alert／hard budget application設定

適用不能なOAuth consent／client操作は、検証commandと明示的なone-time bootstrap stepを用意し、desired state
とdrift検出を文書化する。`apply`、`plan`、`verify`、`destroy`は共通commandから実行する。

## 12. Cost guard

初期既定:

- Cloud Run min 0、max 2
- Firestore daily app budgetを無料quotaの50%以下に設定
- Scheduler 1 job
- Secret Manager active versions 6以下、access cache
- Artifact Registry 0.4 GiB soft limit、Stable＋Preview＋rollback候補だけ保持
- Cloud Loggingへraw Runner／Provider logを送らない
- Provider実証回数をRelease Contractで有限化する
- 予測budget超過時はPreview昇格より前に`needs-input`へ停止する

無料quotaは変更されうるため、IaCに数値を埋めて安全を推測せず、deploy時のcost preflightで公式条件と
billing設定を再確認する。budget alertは通知でありhard stopではない。

## 13. Rejected technologies

- PostgreSQL／Cloud SQL: relational modelには適するが常設database費用が無料枠要件に反する
- SQLite on Cloud Run: disposable filesystemと複数instanceでcanonical storeにできない
- Kubernetes: single-user初期規模に対して運用・費用・境界が過剰
- Cloud Tasks／Pub/Sub／Workflow: 初期throughputに不要な分散状態と費用面を増やす
- Node SPA: UI規模に対してbuild dependencyとrelease artifactを増やす
- dynamic Go plugin: version互換、署名、rollbackが難しく、build-time module registryで目的を満たせる
- direct browser Firestore access: authorization、read budget、domain invariantをclientへ分散させる
