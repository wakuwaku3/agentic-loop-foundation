# Domain model・状態遷移・実行契約

更新日: 2026-08-22

## 1. Model方針

- domain stateの正本を一つにする
- Git、Provider、deployment、VCSなどの外部状態は正本ではなくObservationとして取り込む
- 現在状態はtyped recordとして保持し、監査用Eventもappend-onlyで残す
- Eventから毎回全状態を復元する完全event sourcingは採用しない
- Requirement、Increment、Execution、Release、Controlを独立したlifecycleにする
- 状態変更はCommandをtransactionで検証・適用し、対応EventとOutboxを同じtransactionで記録する
- 時間、乱数、外部I/Oをdomain判断へ直接持ち込まず、Observationまたは明示入力にする

## 2. Aggregateとentity

### Installation

single-tenant環境のroot。Backlog、Repository、Runner、Provider Account、全体Control Policy、Resource
Budgetを所有する。

主な属性:

- `installation_id`
- `owner_id`
- `control_revision`
- `budget_policy_id`
- `created_at`

### Repository

Applicationと変更境界。Backlogは所有しない。

主な属性:

- `repository_id`
- source locatorとdefault branch
- Stable／PreviewのApplication Release
- Stable／PreviewのLoop Version routing
- Repository Contract version
- Release Contract version
- control state

### Requirement

人間が伝えた課題・観測・feedbackと、ループが深掘りしたProblem Frameを追跡するaggregate。

主な属性:

- `requirement_id`
- 原文（Howを含みうる）
- `problem_frame`: 誰が、何に困り、何が望ましく、どう観測するか
- 関連Repository集合
- 現在状態
- Priority Assessment
- Question集合
- Increment集合
- 完了を支えるStable Release集合
- optimistic concurrency用`version`

原文は履歴として保持し、Problem Frameと混ぜない。Howは`proposed_approach`として保存できるが、
Constraintへ自動昇格させない。

### Priority Assessment

Requirementを次に処理する価値を説明するversion付き判断記録。

- 利用者価値と影響範囲
- 緊急性と期限の根拠
- security、availability、data、costなどのrisk
- 依存解除効果
- 学習価値と不確実性削減
- 推定resource cost
- 待機時間と飢餓risk
- 現在実行可能か
- 判断理由と再評価条件

単一の固定scoreを正本にしない。schedulerは候補比較結果をDecisionとして残し、新しいObservationで
再評価する。

### Increment

Requirementを前進させる、独立して統合・検証・rollback可能な変更単位。

- `increment_id`
- 親Requirement
- 目的と完了条件
- 対象Repository集合
- dependency
- Resource Claim
- Work Packet
- ArtifactとEvidence
- 現在状態
- Retry Budget

1 Requirementに複数Incrementを許す。Incrementが成功してもRequirementは自動完了しない。

### Resource Claim

Incrementが変更または排他的操作を行う対象の宣言。

- Repository path／structural namespace
- database schema／migration lane
- deploy target／Application channel
- external resource
- secret scope
- Provider／Runner capacity class

読み取り調査は原則共有可能とし、変更開始前にClaimを具体化する。実diffやplanでClaimが広がる場合は、
競合を再評価してから続行する。

### Work Packet

RunnerまたはProvider間でhandoffできるprovider-neutralなcheckpoint。

- Problem FrameとConstraint
- 対象Repository／Increment
- 現在のapproachと棄却したapproach、その理由
- Resource Claim
- workspace／commit／artifactへの参照
- 実行済み検証とEvidence
- 未解決事項と次の安全なcommand
- control revisionとfencing token

生conversation、raw prompt、credential、無制限logは含めない。Work Packetは事実と判断を分ける。

### Execution

1つのIncrementに対する、1 Runnerの有限な実行session。

- `execution_id`
- Increment、Runner、Loop Version、Provider Run集合
- Leaseとfencing token
- command deadline／progress deadline
- checkpoint
-終了理由

RequirementはRunnerへ固定しない。1 Incrementには同時に1つの有効Executionだけを許す。

### Lease

有限時間の排他的実行権。

- `lease_id`
- owner Runner／Execution
- scope
- `fencing_token`（scopeごとに単調増加）
- issued／expires／renewed timestamps
- state

Leaseの期限延長はprocess生存ではなくRunner Agentのackを示すだけで、進行signalとは分ける。

### Runner

利用者machine上のRunner Agentと、その実行能力。

- platform、capacity、sandbox capability
- 到達可能Repository
- 利用可能Provider CLIとversion
- health、last seen、control revision ack
- active Execution

### Provider Account／Provider Run

Codex、Claude、opencodeの認証・quota境界と、個々のAI実行記録。

Provider Account:

- provider、pool、利用可能Runner
- health、quota Observation、cooldown
- concurrency／usage Budget

Provider Run:

- provider、CLI／model version、work type
- start／finish、normalized outcome
- token／cost／duration（取得可能な範囲）
- input Work Packet versionとoutput Artifact参照

provider固有errorとpayloadはAdapter内でNormalized Failureへ変換する。

### Artifact／Observation／Evidence

- `Artifact`: workspace snapshot、commit、build、document、migrationなどの生成物へのimmutable参照
- `Observation`: 外部systemから取得した時点付き事実。現在も有効とは限らない
- `Evidence`: capabilityの成功・失敗を判断するために採用したObservationと検証結果

Evidenceは秘密やraw provider conversationを含まず、再検証方法を持つ。

### Release Candidate／Release

ApplicationまたはLoopのversioned bundle。

- implementation Artifact
- schema／migration
- configuration schema
- Release Contract
- Stable／Preview利用者文書
- capabilityごとのEvidence
- rollback target

Release Candidateが全gateを満たした後、同じbundleをStable Releaseへ昇格する。

### Control Intent

人間が発行した開始、pause、stop、cancel、resumeのauthoritative command。

- scope: Installation／Repository／Requirement／Increment／Runner／channel
- mode: allow／pause-intake／pause-claim／graceful-stop／immediate-stop／emergency-stop／cancel
- monotonically increasing revision
- requested by／at、reason
- acknowledgmentとverification

複数scopeが重なる場合、より禁止的なIntentを優先する。古いrevisionの許可で新しい停止を上書きしない。

### Question

人間の判断を必要とする問い。

- 判断不能な理由
- 選択肢、影響、推奨
- 回答まで止めるscope
- answerと解決時刻

曖昧さだけを理由に安易に作らず、安全に可逆な仮説を試せる場合はループが進める。

### Event／Outbox Item

Eventはdomain state変更の監査記録、Outbox Itemは外部副作用のdurable commandである。同じdatabase
transactionで現在状態と共に書き、外部作用の途中失敗で状態だけ先に進むことを防ぐ。

## 3. 関係

```text
Installation
├─ one Requirement Backlog
│  └─ Requirement 1 ──* Increment
│                       ├─ * Execution ──1 Lease ──1 Runner
│                       ├─ * Provider Run ──1 Provider Account
│                       └─ * Artifact / Observation / Evidence
├─ * Repository ──* Requirement / Increment
│  ├─ 1 Stable Application Release
│  └─ 0..1 Preview Application Release
├─ * Runner
├─ * Provider Account
└─ * Control Intent

Release Candidate
├─ * Capability Evidence
├─ implementation / schema / configuration
└─ Stable / Preview documentation
```

## 4. Requirement lifecycle

| 状態 | 意味 | 主な遷移先 |
| --- | --- | --- |
| `captured` | 原文を失わず登録済み | `framing`, `cancelled` |
| `framing` | 課題、価値、制約、関連Repositoryを分析中 | `ready`, `needs-input`, `cancelled` |
| `ready` | 優先順位評価済みで実行可能 | `active`, `waiting`, `paused`, `cancelled` |
| `active` | 1つ以上のIncrementが進行中 | `evaluating`, `waiting`, `needs-input`, `paused`, `recovering`, `cancelled` |
| `waiting` | dependency、capacity、time条件を自動待機 | `ready`, `active`, `paused`, `cancelled` |
| `needs-input` | 人間の判断が必要 | `framing`, `ready`, `active`, `cancelled` |
| `paused` | 人間が処理を停止 | 直前の安全な非終端状態、`cancelled` |
| `recovering` | lost ExecutionやPreview failureから再開中 | `active`, `waiting`, `needs-input`, `paused` |
| `evaluating` | Stableで課題が解決したか評価中 | `completed`, `active`, `needs-input` |
| `completed` | 対応Stable ReleaseとEvidenceで課題解決を確認 | 終端。新feedbackは新Requirement |
| `cancelled` | 人間が以後の解決を不要と判断 | 終端。再開は新しい明示Intentでのみ可能 |

`failed`をRequirementの終端状態にしない。failureはExecution、Provider Run、Evidenceに記録し、
Requirementはrecovering、waiting、needs-inputのいずれかになる。

## 5. Increment lifecycle

```text
proposed → ready → leased → executing → verifying → integrated
    ↑         │        │         │           │          │
    └─ revise ┘        ├─ lost → ready       ├─ revise ├─ preview-validating
                       ├─ paused              └─ failed │
                       └─ cancelled                     ↓
                                         accepted ← preview-validating
                                             │
                                          released
```

- `accepted`: Increment固有の条件とPreview capability evidenceを満たした
- `released`: Incrementを含むApplication ReleaseがStableへ昇格した
- `abandoned`: approachが不要・不適切になった非成功終端。Requirementは継続できる
- `cancelled`: Requirementまたは人間のControl Intentにより停止した終端

Integration後にPreviewで失敗した場合、同じArtifactを上書きせず修正Incrementまたは新Artifactを作る。

## 6. Execution・Lease lifecycle

```text
offered → leased → starting → running → checkpointing → succeeded
             │         │          │             ├──────→ failed
             │         │          ├─────────────→ terminated
             │         └──────────→ lost
             └────────────────────→ expired / revoked
```

- schedulerはtransaction内でIncrementの状態、Control Intent、Resource Claim、capacityを再検証してLeaseを発行する
- RunnerはLease取得後、最新Control IntentとRepository Contractを取得してからprocessを開始する
- heartbeatと進行eventを分ける
- Lease更新には現在のfencing tokenとcontrol revisionを要求する
- expired／revoked LeaseのExecutionはResultをcommitできない
- retryは新Executionと新fencing tokenを作り、古いExecutionを復活させない

## 7. Release lifecycle

```text
assembling → preview-deployed → exercising → promotable → promoting → stable
                    │                │             │
                    ├─ failed ─→ rejected          ├─ failed → rollback
                    └─ superseded                  └────────→ stable(previous)
```

`promotable`にはRelease Contractの全capability、対象実Provider、文書、rollback／resume evidenceが必要。
Stable昇格はbundleのversionを変えずchannel pointerをtransactionalに切り替える。実外部deployがatomicで
ない場合はOutboxとreconciliationで収束させ、途中状態を利用者へ表示する。

## 8. Control contract

### Effective policy

各操作直前に、Installationから対象entityまでのControl Intentを合成する。

```text
emergency-stop > immediate-stop > cancelled > graceful-stop > paused > allowed
```

新規claim、process開始、credential取得、外部副作用、integration、Preview deploy、Stable promotionの
各boundaryでeffective policyとrevisionを再確認する。

### Stop completion

| 段階 | 意味 |
| --- | --- |
| `requested` | Control Intentをdurableに保存した |
| `acknowledged` | 対象Runnerがrevisionを受信した |
| `effective` | 新規claim／credential／authoritative副作用を拒否している |
| `verified` | active Leaseがなく、到達Runnerのprocessが止まり、in-flight外部作用をreconcileした |

到達不能Runnerはacknowledgedにならなくても、Lease expiryとfencingによりeffectiveにできる。
物理processの消滅を観測できない間は、その事実を表示するが、権限失効後のResultは確定できない。

### Immediate stop

- Runner Agentがchild process groupへTERM、猶予後KILLを送る
- すでに送信済みの外部requestは「取り消せた」と仮定せず、結果をObservationとしてreconcileする
- old fencing tokenによるintegration、deploy、promotionを拒否する
- workspaceとcheckpointは削除せず、秘密を除去して回復可能に保つ

## 9. 外部副作用契約

外部副作用には一意な`operation_id`、対象、期待version、fencing tokenを付ける。

1. domain transactionでOperation IntentとOutbox Itemを作る
2. adapterが対象外部systemの現在状態を観測する
3. 既に同じ結果なら成功として収束する
4. 未実行ならconditional／idempotent APIで適用する
5. 結果をObservationとして保存する
6. domain transactionで現在も有効なtokenか確認し、Operationを確定する

外部systemがidempotency keyやcompare-and-setを持たない場合、専用Integration Adapterで重複検出し、
曖昧なtimeout後は再実行前に必ずread-after-writeで観測する。安全に判定不能なら`needs-input`へ進む。

## 10. Handoff contract

- checkpointはcommand boundaryまたは明示的な安全点で作る
- 次RunnerはWork Packetを信用しきらず、Artifactと外部状態を再観測する
- provider変更時もProblem FrameとEvidenceは維持し、provider固有conversationは引き継がない
- handoff前後でResource ClaimとControl revisionを再評価する
- 同じIncrementへ有効なownerを二つ作らない
- handoff失敗はRequirement失敗ではなくExecution failureとして回復する

## 11. Retention

- canonical Requirement、Decision、Release、Control Eventは監査期間中保持する
- raw Runner logとProvider出力は短期・容量上限付き・秘密検査済みにする
- Work Packetは後続handoffと監査に必要な要約だけを保持する
- workspace、旧module、旧Release、旧文書は参照中entityとrollback windowがなくなってからGCする
- GCも削除対象を列挙・再検証し、Control Intentとretention policyに従う
