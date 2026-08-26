# 宣言と実装の乖離台帳（分母の固定）

測定日: 2026-08-26
測定tree: worktree `v2-gap` / branch `v2-gap` / HEAD `7860857`
測定者: sol

## 1. この文書の目的と読み方

本repositoryの宣言（product定義とarchitecture文書）は実装より先へ進んでおり、その差が
何件あるかは誰も知らない。本文書はその**分母を一度だけ固定する**ための台帳である。
以後この種の欠陥が見つかったときに新しいtaskを起こすのではなく、本台帳の行を
一つ消す形で扱えるようにするのが唯一の目的である。

読む順序の指針: 分類はすべてcodeの実測であり、文書から文書への推論は一切含まない。
行ごとに「宣言のfile:line」「codeのfile:line（または不在を示したgrep argv）」を持ち、
code evidenceのない行は台帳に載せていない。§4の到達境界表が全表の共通根拠であり、
個別行はそこを参照する。

分類:

| 分類 | 意味 |
| --- | --- |
| `IMPLEMENTED` | 振る舞いが存在し、非testのcallerが到達できる |
| `UNREACHABLE` | 機構はcodeにあるが、testの外から到達する経路が一つもない |
| `ABSENT` | 機構がまったく存在しない |
| `DEFERRED-BY-DECLARATION` | 宣言自身が外部systemを名指しており、その等級の管轄。Cloud Run（初回deploy gate D1）、またはこの機械で未認証のProvider（codex／opencode。2026-08-26実測: `codex login status` は "Not logged in"、`opencode auth list` は "0 credentials"）。**到達できなかったという観測からは導かない** |
| `WIDER-THAN-CODE` | 文書がcodeの拒否する範囲を許可している、またはcodeより広い集合を記述している |

宣言が箇条書きや表で複数項目をまとめている場合、その1行を **複合行**（分類欄 `混在`）
として1件に数え、構成項目ごとの分類をevidence欄に名指した。複合行は本台帳のいちばん
粗い刻みであり、消せるのは名指した構成項目すべてが解消したときだけである。

`DEFERRED-BY-DECLARATION` は宣言自身が名指す外部systemからのみ導いた。到達不能の理由が
本repositoryの内側にあるものは、外部systemが絡んでいても `UNREACHABLE` または `ABSENT`
とした（例: IAP assertion検証はCloud Runを要さずcodeで書けるため `ABSENT`）。

### 訂正履歴

この台帳は分母なので、台帳自身の誤りは訂正して残す。**新しいtaskは起こさない。**

| 日付 | 行 | 訂正 |
|---|---|---|
| 2026-08-27 | L92b（および依存する L179 / L106-118） | `domain.PriorityAssessment` を「型が存在しない」＝ABSENTとしていたのは誤り。型は `internal/domain/model.go:66` にあり採点器も `internal/scheduler/priority.go:128` にある。実際の不在は `BuildAllocationSnapshot` が全rowにAssessmentを入れないことで、分類はUNREACHABLEが正しい。V2-095の設計が実測して指摘した |

## 2. 網羅範囲

対象文書の行数を再測定した（`wc -l`、合計 3080行）。

| 文書 | 行数 | 走査 |
| --- | --- | --- |
| `docs/product/definition.md` | 343 | 完全 |
| `docs/product/user-facing-spec.md` | 343 | 完全 |
| `docs/architecture/overview.md` | 286 | 完全 |
| `docs/architecture/domain-model.md` | 390 | 完全 |
| `docs/architecture/failure-model.md` | 167 | 完全 |
| `docs/architecture/firestore-store.md` | 63 | 完全 |
| `docs/architecture/orchestration.md` | 77 | 完全 |
| `docs/architecture/release-contract.md` | 106 | 完全 |
| `docs/architecture/technology.md` | 337 | 完全 |
| `docs/architecture/validation.md` | 291 | 完全 |
| `docs/architecture/documentation.md` | 181 | 完全 |
| `docs/architecture/roadmap.md` | 345 | 完全 |
| `docs/architecture/legacy-inventory.md` | 151 | **部分**（§10の知識8件と、`retain`／`redesign`のうち機構を名指す行を採録。`remove`／`defer`の判定行は「移植しない」という不在の宣言であり振る舞いの主張ではないため、代表4件のみ採録） |

未走査の文書は無い。粒度の下げ方（1行=1主張の刻み）は§17に明記した。

## 3. 分類ごとの件数

検査した振る舞い主張の総数: **501**（うち複合行126）

| 分類 | 件数 |
| --- | --- |
| `IMPLEMENTED` | 188 |
| `UNREACHABLE` | 97 |
| `ABSENT` | 49 |
| `DEFERRED-BY-DECLARATION` | 11 |
| `WIDER-THAN-CODE` | 30 |
| 複合行（`混在`） | 126 |

文書別:

| 文書 | IMPL | UNREACH | ABSENT | DEFER | WIDER | 混在 | 計 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `docs/product/definition.md` | 34 | 31 | 4 | 1 | 5 | 4 | 79 |
| `docs/product/user-facing-spec.md` | 15 | 14 | 2 | 0 | 6 | 9 | 46 |
| `docs/architecture/overview.md` | 16 | 6 | 1 | 1 | 1 | 12 | 37 |
| `docs/architecture/domain-model.md` | 24 | 10 | 4 | 0 | 9 | 9 | 56 |
| `docs/architecture/failure-model.md` | 11 | 7 | 0 | 1 | 1 | 5 | 25 |
| `docs/architecture/firestore-store.md` | 18 | 3 | 0 | 0 | 0 | 0 | 21 |
| `docs/architecture/orchestration.md` | 4 | 1 | 4 | 0 | 0 | 2 | 11 |
| `docs/architecture/release-contract.md` | 9 | 4 | 1 | 2 | 0 | 4 | 20 |
| `docs/architecture/technology.md` | 17 | 2 | 9 | 4 | 4 | 24 | 60 |
| `docs/architecture/validation.md` | 18 | 4 | 5 | 1 | 1 | 13 | 42 |
| `docs/architecture/documentation.md` | 4 | 2 | 4 | 0 | 0 | 6 | 16 |
| `docs/architecture/roadmap.md` | 5 | 2 | 1 | 1 | 0 | 14 | 23 |
| `docs/architecture/legacy-inventory.md`（部分） | 13 | 11 | 14 | 0 | 3 | 24 | 65 |
| **合計** | **188** | **97** | **49** | **11** | **30** | **126** | **501** |

`UNREACHABLE` と複合行の中の到達不能項目の大半は§4の3つの断線
（runner binaryの不在、Increment生成経路の不在、release observer未装着）に収束する。
`docs/architecture/firestore-store.md` だけが複合行ゼロかつ `ABSENT` ゼロであり、
宣言と実装が最も近い文書である。

## 4. 測定の土台: 到達境界

以下は全表の共通根拠である。各行は個別に実測した。

| 事実 | evidence |
| --- | --- |
| `cmd/runner` はstubである。`--fake` 以外では起動を拒否し、`internal/` から何もimportせず、ctx待ちで終わる | `cmd/runner/main.go:28-31`（"no external control-plane wiring is enabled"）、`cmd/runner/main.go:1-11`（import群にinternal packageが無い） |
| `internal/runner` の非test entry pointは3つだけ。enrollment、namespace probe、log redaction | `grep -rn 'runner\.' cmd/*/*.go \| grep -v _test.go` → `cmd/control-plane/main.go:156`（`runner.NewService`）と `cmd/bootstrap/main.go:133`（`runner.NamespaceConfinement`）の2件のみ。加えて `internal/api/operator_record.go:227`（`runner.RedactLog`） |
| Orchestrator、ProcessSupervisor、SecretBroker、Workspace、Journal、ControlLoop、ControlAgent、CostLedger、ForgePublisher はいずれも非testで構築されない | `grep -rn --include='*.go' 'Orchestrator{\|NewSecretBroker\|NewWorkspace\|OpenJournal\|ControlLoop{\|ControlAgent{\|NewCostLedger\|NewForgePublisher' internal cmd \| grep -v _test.go` → 宣言行のみ（`internal/runner/orchestrator.go:17`、`secret_broker.go:149`、`workspace.go:11`、`journal.go:38`、`control_loop.go:15`、`control_agent.go:25`、`forgepublish.go:234`） |
| Incrementを生成する唯一のcodeは `Service.Plan`。その非test callerは `Orchestrator` 1箇所のみ | `internal/application/service.go:993-994`（`domain.Increment{... Status: IncrementProposed}` + `SaveIncrement`）／唯一の非test caller `internal/runner/orchestrator.go:59` |
| したがって稼働中のControl Planeには Increment が一つも存在し得ない。`/v1/runner/claims:acquire`、`:renew`、`/v1/executions/...`、`/v1/executions/result` はすべて到達可能なrouteだが成功し得ない | 上記に加え `internal/api/api.go:384,392,427,432` |
| 非testのcodeが発行するRequirement commandは3つだけ（start-framing、needs-input、ready）。到達可能statusは `{captured, framing, needs-input, ready}` | `internal/application/framing.go:191`、`internal/application/human_input.go:519`、`internal/application/human_input.go:648`。他6 command（start／wait／recover／evaluate／pause／cancel）と `CompleteRequirementFromRelease` は非test発行が0で、`internal/application/framing.go:27-30` が同じ測定を記録している |
| 非testのcodeが発行するIncrement commandは `IncrementRecover` のみ（reconciler 2箇所）。prepare／lease／execute はService内で発行されるが、その入口が到達不能。verify／integrate／preview-validate／accept／release は非test発行が0 | `grep -rn --include='*.go' 'IncrementVerify\|IncrementIntegrate\|IncrementAccept\|IncrementRelease\|IncrementPreview\b' internal cmd \| grep -v _test.go \| grep -v internal/domain/model.go` → `internal/application/readmodels.go:322`（読み取りのみ）のみ |
| OutboxDispatcherは非testで構築されない。外部副作用の配送経路が稼働binaryに無い | `internal/application/outbox.go:178`、`:199`（`NewOutboxDispatcher`／`NewDispatcher` の唯一の呼び出しは`:200`の内部委譲） |
| release observerは非testで装着されない。`GET /v1/release/state` は稼働binaryでは常に `ErrReleaseObserverNotConfigured` を返す | `internal/application/release_surface.go:51`（`AttachReleaseObserver`）、`:121`（`NewSourceReleaseObserver`）に非test callerが0。`cmd/control-plane/main.go:172-207` はobserverを装着しない。503分岐は `internal/api/api.go:900` |
| `internal/store/memory` は稼働binaryのimport閉包に入らない（test専用） | `devbox run --pure -- sh -c 'go list -deps ./cmd/... \| grep agentic-loop'` の出力に `internal/store/memory` が現れない |
| Control Planeが実際に配線するbackground処理は2つだけ（lease expiry、control verification）。orphan sweep、outbox dispatch、retention GC、release promotionは配線されない | `cmd/control-plane/main.go:189-190`（`reconciler.Reconciler`、`reconciler.VerificationReconciler`）と `:199-207`（`InternalReconcile`） |
| Provider adapterはprocessを起動しない。`Run` は3 adapterすべて `NoExec` | `internal/provider/adapters.go:139-144`、`internal/provider/adapters.go:318` |
| Provider observationを書くcodeが無いため `/v1/providers` の各Providerは常に未観測 | `grep -rn --include='*.go' 'SaveProviderObservation' internal \| grep -v _test.go` → `internal/application/ports.go` のinterface宣言と2つのstore実装（`internal/store/memory/store.go:1176`、`internal/store/firestore/store.go:1766`）のみ。application層からの呼び出しが0 |

到達可能なowner／runner surface（この台帳で `IMPLEMENTED` の根拠になる集合）:

`GET /healthz`、`POST /internal/reconcile`、`GET /owner` と `/owner/assets/*`、
`POST|GET /v1/requirements`、`GET /v1/requirements/{id}`、
`POST /v1/requirements/{id}:start-framing`／`:request-input`／`:answer-input`、
`POST|GET /v1/repositories`、`GET /v1/repositories/{id}`、`POST .../{id}:retire`／`:observe`、
`POST|GET /v1/controls`、`GET /v1/queue/summary`、`GET /v1/runners`、`GET /v1/providers`、
`GET /v1/export`、`GET /v1/release/state`、
`POST /v1/runner/enrollment`／`enrollment/challenge`／`enrollment/complete`、
`POST /v1/runner/claims:acquire`／`permits:check`／`heartbeat`／`checkpoints`、
`POST /v1/leases/{id}:renew`、`POST /v1/executions/{id}:start`、`POST /v1/executions/result`
（`internal/api/api.go:91-487`）。CLIは `cmd/bootstrap` の `install`／`switch`／`run`
（`cmd/bootstrap/main.go:46-56`）と `cmd/ci-plan`、`cmd/legacy-import`。

## 5. `docs/product/definition.md`

本文書は自ら「現行実装の挙動を保証する文書ではない」（L6）と宣言しているが、
§7以降には「実測は…で確認しており」のように現在形でcodeを名指す主張が含まれるため、
振る舞い主張として全件採録した。

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L39 | 登録すれば監視・指示なしで解決まで進む | UNREACHABLE | 到達可能Requirement statusは4種のみ（§4）。Increment生成は `internal/application/service.go:993` のみで、その非test callerは `internal/runner/orchestrator.go:59`（非test構築なし） |
| L40 | 待機中・実行中・停止中・完了済みを把握できる | WIDER-THAN-CODE | 読み口は `internal/api/api.go:253`（`/v1/queue/summary`）で存在するが、`active`／`paused`／`completed` に至るcommandを非testのcodeが一つも発行しない（§4）。4区分のうち到達可能なのは待機のみ |
| L41 | 複数マシン・複数workerで独立問題を並列解決 | UNREACHABLE | `cmd/runner/main.go:28-31` がstub。lease／fencingは `internal/domain/lease.go` に存在 |
| L42 | ループ自身の改善中も既知安定版で能力維持 | UNREACHABLE | channel routingの実装 `internal/release/release.go:120`（`NewRouter`）に非test callerが0 |
| L43a | 開始・停止・再開を利用者が制御できる | IMPLEMENTED | `internal/api/api.go:382` → `internal/application/service.go:1372`（`Service.Control`）。7 modeすべてを受理 |
| L43b | 更新を利用者が制御できる | IMPLEMENTED | `cmd/bootstrap/main.go:46-56`（`install`／`switch`／`run`）→ `internal/update/update.go:127`、`internal/update/switch.go`、`internal/update/launch.go` |
| L44a | control planeとデータを自分専用環境へself-hostできる | IMPLEMENTED | `cmd/control-plane/main.go:107-222`。emulator経路は `:144-151` |
| L44b | 他利用者の負荷や費用の影響を受けない配備（Cloud Run上） | DEFERRED-BY-DECLARATION | 宣言L322がCloud Run + Firestoreを名指す。`infra/main.tf:66` にservice定義はあるがapplyはD1の管轄 |
| L48 | 要求・制約・変更・検証結果・成果の対応が残る | UNREACHABLE | Requirement→Increment→Artifact→Evidence→Releaseのlinkのうち、Increment以降を作る経路が無い（§4）。`domain.CapabilityEvidence` は `internal/domain/release.go:21` に存在するが非test生成なし |
| L49 | 個々のagentやAIツールに依存せずworkerを交換・追加できる | UNREACHABLE | `internal/provider/pool.go:175`（`NewPool`）、`internal/provider/handoff.go:717`（`DecideHandoff`）に非test callerが0 |
| L50 | 失敗を隠さず、再試行可能な状態として保持できる | UNREACHABLE | `internal/domain/release.go:193-217`（`RetryBudget`／`Consume`）。`grep -rn --include='*.go' '\.Consume(' internal cmd \| grep -v _test.go` → 0件 |
| L51 | ループの運用品質を観測し、ループ自身を改善できる | ABSENT | metrics surfaceが存在しない。`grep -rn --include='*.go' 'lead_time\|LeadTime\|metrics' internal cmd \| grep -v _test.go` → 0件 |
| L62-63 a | RequirementをRepositoryへ関連付ける | IMPLEMENTED | `internal/domain/repository.go`（`RequirementRepositoryLink`）、読み口 `internal/api/repositories.go:109,118` |
| L62-63 b | IncrementをRepositoryへ関連付ける | UNREACHABLE | Incrementが生成され得ない（§4） |
| L67-69 | Queueは単一backlogでGitHub Issueではない | IMPLEMENTED | `internal/api/api.go:176`（`GET /v1/requirements`）、`internal/store/firestore/store.go:200`（`installations/<id>/<collection>`）。GitHub参照は永続層に無い |
| L71-73 a | schedulerが有限資源の配分候補と理由を算出する | IMPLEMENTED | `internal/application/allocation.go:625`（`scheduler.Decide`）← `Service.QueueSummary`（`allocation.go:558`）← `internal/api/api.go:253` |
| L71-73 b | 算出した配分をworkerへ渡す（適用する） | UNREACHABLE | `scheduler.Apply` の非test callerが0。`internal/application/allocation.go:37` が「この packageはApplyを呼ばない」と記録 |
| L82 | workerがキューから排他的に仕事を取得する | UNREACHABLE | routeは `internal/api/api.go:384`、実装は `internal/application/service.go:1132`。しかし `ready` のIncrementが存在し得ない（§4） |
| L92a | control planeがleaseを管理する | IMPLEMENTED | `internal/application/service.go:229`（`Renew`）、`internal/reconciler/reconciler.go:41`（expiry回収、`cmd/control-plane/main.go:202` で配線） |
| L92b | control planeが優先順位を管理する | UNREACHABLE | **訂正済み（2026-08-27、V2-095の設計が指摘）。** 初版は「`domain.PriorityAssessment` 型が存在しない」と書いたが**偽**である。同じgrepを再実行すると非test 13行で、`internal/domain/model.go:66` に `type PriorityAssessment struct`、`internal/scheduler/priority.go:128` に `multiFactorScore(a domain.PriorityAssessment, ...)`、`internal/scheduler/scheduler.go:70` に `Assessment *domain.PriorityAssessment` がある。実際の不在はもっと狭い——`BuildAllocationSnapshot`（`internal/application/allocation.go:457-467`）が全rowにPriorityもAssessmentも入れないので、`UsedAssessment` が全候補でfalseになる。型と採点器は在り、それを供給する経路だけが無い。よって分類はABSENTではなくUNREACHABLEである |
| L92c | control planeが依存を管理する | UNREACHABLE | `internal/scheduler/scheduler.go:60`（`Dependencies`）と `:204-207`（`dependenciesMet`）は存在するが、唯一のsnapshot生成 `internal/application/allocation.go:459-467` はこのfieldを一切設定しない |
| L92d | control planeが実行履歴を管理する | IMPLEMENTED | `internal/application/service.go:1773`（`record`）、`internal/store/firestore/store.go:583`（event create-only） |
| L97-99 | Execution Planeがrepository取得・作業空間・AI tool実行・変更・検証・成果物作成を担う | UNREACHABLE | `internal/runner/sourcecontrol.go`、`workspace.go:11`、`provider.go`、`forgepublish.go:234` のいずれも非test entryが無い（§4） |
| L104-105 | 改善完了はPreview実証とStable昇格の時点 | UNREACHABLE | `internal/release/pipeline.go:29,38`（`NewPipeline`／`Promote`）に非test callerが0 |
| L109 | 課題をRequirementとして登録できる | IMPLEMENTED | `internal/api/api.go:374` → `internal/application/service.go:748`（`Capture`） |
| L110 | キューへ永続化され、現在状態と待ち順が見える | IMPLEMENTED | `internal/application/readmodels.go:199`（`ListRequirementsPage`）、`internal/application/allocation.go:558`（待ち理由） |
| L111 | 実行可能Requirementをworkerが排他的にclaimする | UNREACHABLE | L82と同一（§4） |
| L112 | workerがルールを読み、専用作業空間でループを回す | UNREACHABLE | §4（runner不在） |
| L113 | workerが進捗・判断・検証結果をcontrol planeへ報告する | IMPLEMENTED | routeとhandlerは存在（`internal/api/api.go:394,396`→`internal/application/service.go:474,604`）。呼ぶ主体は不在だが、非testのcaller（HTTP client）に対して開いている |
| L114-115 | 判断不要なら増分を積み上げPreview実証しStable昇格まで自律進行 | UNREACHABLE | §4（Increment不在、pipeline不在） |
| L116 | 判断が必要なら問いと停止理由を提示し、回答後に同じRequirementを再開する | IMPLEMENTED | `internal/api/api.go:442,460` → `internal/application/human_input.go:460,572`。新Requirementを作らないことは `human_input.go:563` |
| L117 | 完了後にRequirement・変更履歴・検証・Preview実績・Stable昇格の対応を確認できる | UNREACHABLE | §4（completed到達不能、release observer未装着） |
| L119-120 | 大きなRequirementを実行可能な増分へ分解する | UNREACHABLE | `internal/application/service.go:887`（`Service.Plan`）の非test callerは `internal/runner/orchestrator.go:59` のみ |
| L126 | 調査・変更・検証・修正を人間の逐次指示なしに反復する | UNREACHABLE | §4 |
| L127 | 一時的外部障害やworker停止から安全に再開できる | IMPLEMENTED | `internal/reconciler/reconciler.go:41`（expired lease→`MarkExecutionLost`→`IncrementRecover`、`:130`）。`cmd/control-plane/main.go:202` で配線 |
| L128 | 同じ副作用を誤って重複実行しない | IMPLEMENTED | `internal/application/service.go:147`（`mutate` のidempotency）、`internal/store/firestore/store.go:583`（create-only） |
| L129 | 判断権限を越える場合は理由と選択肢を示して停止する | IMPLEMENTED | `internal/application/human_input.go:460`（reason／options／stopped scope／continuing scope） |
| L130 | 完了を自己申告だけで決めず要求と不変条件を検証する | UNREACHABLE | `internal/domain/model.go:526`（`CompleteRequirementFromRelease` はStableReleaseProof必須）だが非test callerが0 |
| L131 | 回復不能な失敗を無限再試行せず観測可能な状態で保持する | UNREACHABLE | `RetryBudget`（L50と同一） |
| L132 | 利用者の停止指示へ確実に従い、停止完了を検証可能にする | IMPLEMENTED | `internal/domain/control.go:266`（`permitAllowed`）、`internal/reconciler/control_verifier.go:87`（verification遷移）。ただし対象processの停止観測は§4のとおり不在 |
| L133 | ループ自身の更新をPreviewで試しStableへ昇格または切り戻す | IMPLEMENTED | `internal/update/switch.go`（`SwitchForward`／rollback）、`cmd/bootstrap/main.go:97-121` |
| L137-138 | control plane、scheduler、runnerが同じ制御状態を正本として扱う | WIDER-THAN-CODE | control planeは従う（`internal/domain/control.go:266`）が、runnerは存在せず、schedulerは制御を読むが適用しない（`internal/application/allocation.go:37`） |
| L142 | pause intake: 新規Requirement受付／queue投入を止める | IMPLEMENTED | `internal/domain/control.go:270`（`ControlPauseIntake` は `PermitIntake` を拒否） |
| L143 | pause claim: 新規claimを止める | IMPLEMENTED | `internal/domain/control.go:272` |
| L144 | graceful stop: 新規副作用を開始せずcheckpointを保存して停止 | IMPLEMENTED | `internal/domain/control.go:274`（`PermitCheckpoint` のみ許可） |
| L145a | immediate stop: leaseを失効させる | IMPLEMENTED | `internal/domain/control.go:276`（全kind拒否）、`internal/reconciler/reconciler.go:41` |
| L145b | immediate stop: 実行中processを終了する | UNREACHABLE | `internal/runner/supervisor.go:73`（`ProcessSupervisor`、TERM→KILL）に非test entryが無い（§4） |
| L145c | immediate stop: 外部副作用が残っていないかを確認する | UNREACHABLE | `internal/application/outbox.go:328`（`Reconcile`）を持つdispatcherが非test構築されない |
| L146 | resume: 保存したcheckpointと観測済み外部状態から安全に再開する | UNREACHABLE | `Service.Checkpoint`（`internal/application/service.go:604`）は到達可能だがcheckpointを消費する再開経路が無い。`internal/runner/journal.go:38` は非test entryなし |
| L147 | cancel requirement: 指定Requirementの以後の処理を止める | WIDER-THAN-CODE | `ControlCancel` intentは記録できる（`internal/application/service.go:1372`）が、Requirementを `cancelled` へ動かす `RequirementCancel` を非testのcodeが発行しない（§4）。文書は状態遷移まで約束している |
| L148 | emergency stop: 全repository・全worker・全versionの新規実行を一括停止 | IMPLEMENTED | `internal/domain/control.go:276` + `internal/domain/control.go:112`（severity合成、`ScopeInstallation` が最上位） |
| L150-154 | allow以外のどのmodeでもauthoritative effectは一切構成されない（effectの唯一の構成経路がallowを要求する） | IMPLEMENTED | `internal/domain/control.go:297`（`EffectFromPermit` が唯一のeffect constructor）、`internal/domain/control.go:266` |
| L155-159 | 7 mode×8 kind＝56 cellで実測し、許可cellはallow=8／pause-claim=2／pause-intake=1／graceful-stop=1／他0の合計12 | IMPLEMENTED | `internal/application/stop_matrix_test.go:19`（`wantStopMatrixCells = 56`）、`:52-62`（mode別許可数）、`:64`（`wantStopModeAllowedTotal = 12`）。表の8 kindはPermitKind 9種の部分集合（`internal/domain/control.go:188-196`）であり、`PermitCredential`／`PermitProcess` はmatrixの列に含まれない |
| L160 | この閉包はM1 gateがinvariant 2として証明した | UNREACHABLE | 証明はtest内に閉じる。matrixは `internal/store/memory` 上で走り（`internal/application/stop_matrix_test.go:10`）、その storeは稼働binaryのimport閉包に無い（§4）。outbox 4 kindのcellはdispatcher経由で、dispatcherは非test構築されない |
| L162 | このcell数を `internal/application` の `TestStopModeByKindMatrix` が検証している | IMPLEMENTED | `internal/application/stop_matrix_test.go` に同名testが存在し、L155-159の数値をconstantとして持つ |
| L164-166 a | 停止完了はack・process終了・lease解放・新規副作用の不在まで観測する | WIDER-THAN-CODE | ack（`internal/application/service.go:543`）とlease（`internal/reconciler/reconciler.go:41`）と副作用拒否（`internal/domain/control.go:266`）は在る。process終了の観測を書くcodeが無い（`domain.ProcessObservation` は `internal/domain/control.go:67` に型だけ存在し、非test生成が0） |
| L164-166 b | 到達不能runnerも期限切れleaseとfencing tokenで古い権限の副作用確定を防ぐ | IMPLEMENTED | `internal/domain/id.go:26` 近傍のfencing規則、`internal/reconciler/verification.go:11`（`VerificationBlockedUnreachable`） |
| L168-169 | 通常の変更・Preview評価・Stable昇格は人間レビューなしで自律実行する | UNREACHABLE | `internal/release/pipeline.go:38` に非test callerが0 |
| L173-174 | 複数マシン／1マシン複数workerからworkerを追加できる | UNREACHABLE | §4（runner不在）。enrollmentは `internal/runner/protocol.go:65` で到達可能 |
| L175 | 1つのRequirementを同時に複数workerが変更しない | UNREACHABLE | `internal/domain/lease.go` のfencingは存在するが、Increment不在で発行され得ない（§4） |
| L176 | 独立したRequirementは並列に進められる | UNREACHABLE | 同上 |
| L177 | 競合を事前予測できなくても検出・停止・再計画・統合できる | ABSENT | 再計画（replan）と統合（integration）を行うcodeが無い。`grep -rn --include='*.go' 'Replan\|replan' internal cmd \| grep -v _test.go` → 0件 |
| L178 | worker数を増やしても中央APIや外部I/Oが無制限に増えない | IMPLEMENTED | `internal/quota/quota.go:46`（`DefaultBudget`）、`internal/store/firestore/store.go:29-32`（`MaxWrites=400`／`MaxQueryRows=1000`）、`internal/reconciler/reconciler.go:16`（`MaxBatch=100`） |
| L179 | 単一backlog内でAI provider枠とrunner slotを価値・緊急性・risk・cost・依存で配分する | WIDER-THAN-CODE | `internal/scheduler/decision.go` は価値相当のscoreと飢餓時間を扱うが、riskとcostのinputが無く（`domain.PriorityAssessment` は存在するがsnapshotに供給されない。L92bの訂正を参照）、依存fieldは常に空（L92c） |
| L180 | 1 repositoryの大量要求・故障・再試行が他を占有し続けない | IMPLEMENTED | `internal/scheduler/priority.go`／`internal/scheduler/starvation_test.go` の飢餓規則。`internal/application/allocation.go:625` から到達 |
| L184-197 | 観測可能性11項目（どの要求／どの段階／進行か停滞か／なぜ待つ／人間入力が必要／失敗の回復性／何が変わりどう検証されたか／queue全体の処理能力・待ち時間・失敗傾向／停止の伝播範囲／repository別queueと共有資源） | 混在 | 到達可能: 要求一覧・段階（`internal/api/api.go:176,202`）、待機理由（`internal/application/allocation.go:558`）、人間入力（`internal/application/human_input.go`）、停止伝播（`internal/api/api.go:189`）、repository別（`internal/api/repositories.go:109`）、共有資源（`/v1/queue/summary`）。不在: 進行／停滞の区別（`progress deadline` を評価するcodeが無い。`grep -rn --include='*.go' 'ProgressDeadline' internal \| grep -v _test.go` → 0件）、失敗の自動回復性表示、変更内容と検証結果、処理能力・lead time・手戻り傾向（L51と同じくmetrics不在） |
| L206-211 | Stable: Previewの障害に影響されず起動でき、破壊的schema変更を受けず、rollback後も進行中Requirementを再開でき、Preview破損時に同じRequirementを引き継いで復旧できる | UNREACHABLE | `internal/update/launch.go:54`（channel別起動）と `internal/update/switch.go:99`（stableの前進条件）は `cmd/bootstrap` から到達するが、Requirement引き継ぎの主体（runner）が不在（§4） |
| L214-219 | Preview: 明示許可した大きな単位でdogfood、独立worker poolまたは明示routing、schema／lease protocol互換、実測比較、問題時は新規claim停止でStableへ戻す | UNREACHABLE | routing表 `internal/release/release.go:120`（`NewRouter`）に非test callerが0 |
| L221-228 | StableとPreviewが共有する5契約（state schema互換、lease／fencingの意味、stop／resume／cancel protocol、checkpointと外部副作用の識別、event／outboxの冪等性） | IMPLEMENTED | `internal/update/schema.go:98,156,209`（expand／migrate／contract）、`internal/domain/control.go`、`internal/store/firestore/store.go:583`。ただしexpand／migrate／contractは非test callerが0（§10で個別計上） |
| L232-239 | 昇格根拠6件（固定test成功、実サービスsmoke、dogfooding件数／期間、Stable比の完了率・retry率・停止率・lead time、rollbackと再開実績、未解決重大不具合なし） | UNREACHABLE | `internal/release/promotion_report.go:305`（8条件のreport）は存在し `GET /v1/release/state` から読めるが、observerが装着されないため稼働binaryでは常に503（§4）。Stable比の実測指標はcodeに無い |
| L241-242 | feature flagだけではStableを保護しない。binary／runtime独立、schema互換、routing、即時停止、rollbackを合わせて設計する | IMPLEMENTED | `internal/update/update.go:127`（署名付きbundle検証）、`internal/update/switch.go`、`internal/update/anchor.go`。`cmd/bootstrap` から到達 |
| L249-255 | 自己更新7項目（Stable独立起動、in-place変更でなく追加＋routing切替、rollback window中の旧module保持、参照が無くなってから削除、expand→migrate→contract、updater失敗時の復旧、自己更新もPreviewでdogfood） | 混在 | 到達可能: Stable独立起動と署名検証付きlaunch（`internal/update/launch.go`、`cmd/bootstrap/main.go:123-147`）、追加＋channel切替（`internal/update/switch.go`）、anchor経由の復旧（`cmd/bootstrap/main.go:1-9`）。到達不能: `internal/update/retention.go:206`（`Collect`）と `internal/update/schema.go:98,156,209` に非test callerが0（`grep -rn --include='*.go' 'update.Collect\|\.Expand(\|\.Migrate(' internal cmd \| grep -v _test.go` → 0件） |
| L262-274 | 初期スコープ11項目 | 混在 | 到達可能: 課題登録と単一backlog（`internal/api/api.go:374`）、状態と停止理由の表示（`:176,189`）、graceful／immediate／emergency stop（`internal/domain/control.go:266`）、共有資源配分の算出（`internal/application/allocation.go:625`）、新module追加とPreview切替・rollback（`cmd/bootstrap`）。到達不能: 排他claimとlease回復（§4）、専用作業空間での調査・変更・検証（§4）、増分の統合（`grep -rn --include='*.go' 'IncrementIntegrate' internal cmd \| grep -v _test.go` → 0件）、完了／再試行／人間入力待ちの明示遷移のうち完了と再試行、Stable／Previewのworker routing、Requirementの増分分解と達成追跡 |
| L281 | GitHub IssueをRequirementの受付・backlog・正本として使わない | IMPLEMENTED | 永続層にGitHub参照が無い。`grep -rn --include='*.go' 'api.github.com\|IssueNumber' internal/store internal/application \| grep -v _test.go` → 0件 |
| L288 | SaaSとして複数利用者を1 control planeへ同居させない | IMPLEMENTED | `cmd/control-plane/main.go:108`（単一 `INSTALLATION_ID`）、`internal/store/firestore/store.go:200` |
| L290 | サーバーをまたぐ重複調停は行わない | IMPLEMENTED | fencingのscopeは単一Installation（`internal/store/firestore/store.go:200`） |
| L291 | Control Planeはowner 1人が使い、認証は本人確認のために維持する | IMPLEMENTED | `cmd/control-plane/main.go:45-58`（`OWNER_EMAILS` allowlist）、`internal/api/auth.go:102-125` |
| L295-307 | 成功指標13件 | ABSENT | 指標を算出・保存するcodeが一つも無い。`grep -rn --include='*.go' 'LeadTime\|lead_time\|Throughput\|throughput' internal cmd \| grep -v _test.go` → 0件 |
| L311-328 | 確定したプロダクト判断18件 | 混在 | 到達可能: 完了はStable昇格（型として存在、`internal/domain/model.go:526`）、queueは単一（`internal/api/api.go:176`）、self-host（`cmd/control-plane`）、canonical state共有（`internal/store/firestore/store.go:200`）、専用UI（`internal/web/owner.html`）、GitHub Issue連携なし（L281）。DEFERRED: Cloud Run + Firestoreでの配備（L44b）、初期対応providerにCodexとopencodeを含めること（`internal/provider/adapters.go:14,16` に adapterはあるが実行はNoExec、`:139-144`。当該2 providerはこの機械で未認証）。到達不能: Preview routingのRepository単位（`internal/release/release.go:120`）、Stable候補の全機能Preview確認（`internal/release/pipeline.go:38`）、Provider依存機能の昇格条件、利用者文書のversion管理昇格（`internal/release/docs.go` の検査は `BuildPromotionReport` 経由でのみ、observer未装着） |

## 6. `docs/product/user-facing-spec.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L15-16 | 利用者はself-host環境と1つ以上のrepository／runner machineを所有する単一の人間 | IMPLEMENTED | `cmd/control-plane/main.go:45-58`、`internal/api/auth.go:125` |
| L30-33 | Installationが単一Backlog・複数Repository・複数Runner・共有AI資源を束ねる | WIDER-THAN-CODE | `domain.Installation` aggregateが存在しない（`grep -rn 'type Installation' internal --include='*.go'` → 0件）。束ねているのはFirestoreのpath prefix（`internal/store/firestore/store.go:405`）だけで、Provider Accountは型として無い |
| L40-43 | RequirementはHowを含みうるが確定した実装方法ではなく、PR単位でもない | IMPLEMENTED | `internal/application/service.go:748` は原文をそのまま保存し、`proposed_approach` 相当の昇格を行わない |
| L45-48 | Backlogは単一で、価値・緊急性・risk・cost・依存・実行可能性を継続評価し優先順位を決め直す | WIDER-THAN-CODE | 単一Backlogは `internal/api/api.go:176`。継続評価のうちscoreと飢餓は `internal/scheduler/decision.go`、risk／cost／依存のinputは存在しない（§5 L179、L92c） |
| L50-53 | 1 Requirementは0個以上のIncrementを持ち、Incrementは追加・再分解・置換される | UNREACHABLE | `internal/application/service.go:993` の非test callerが `internal/runner/orchestrator.go:59` のみ。再分解・置換を行うcodeは無い |
| L55-57 | Application ReleaseはPreviewとStableのchannelを持つ | UNREACHABLE | `internal/release/release.go:120`（`NewRouter`）に非test callerが0 |
| L59-61 | Runnerは利用者管理machineでRequirementを処理し、1台で複数Repositoryを扱える | UNREACHABLE | `cmd/runner/main.go:28-31` |
| L63-66 | AI Providerは交換可能なAI実行手段。初期対応はCodex、Claude、opencode | 混在 | 3 adapterのargv／parse／normalizeは `internal/provider/adapters.go:122-137` に存在（IMPLEMENTED相当だが非test callerは無い）。実行は `NoExec`（`:139-144`）でABSENT。codex／opencodeの実行確認はDEFERRED-BY-DECLARATION |
| L72-81 | Repository登録後に識別情報／Preview・Stable状態／適用ルール／Backlog状態／利用可能Runnerとの資源／実行可否と理由を確認できる | 混在 | 到達可能: 識別情報・実行可否と理由（`internal/api/repositories.go:109,118`、`internal/application/repository.go:470,513`）、Backlog状態（`internal/web/owner.html:9`）。到達不能／不在: Preview・Stable状態（release observer未装着）、適用中の大原則とRepository固有ルール（`grep -rn --include='*.go' 'RepositoryContract' internal \| grep -v _test.go` → 0件でABSENT）、利用可能Runnerとの資源対応 |
| L83 | 登録によって既存Application・credential・外部resourceを無断で変更しない | IMPLEMENTED | `internal/application/repository.go:132` はstoreへの書き込みのみ。`internal/application/repository.go:288`（observe）はRunnerが提出したObservationを保存するだけ |
| L87-94 | 自然言語で課題・補足・文脈・How・制約を任意に伝えられる | WIDER-THAN-CODE | `internal/application/service.go:748` が受けるのは `text` 一つ。`contracts/openapi/openapi-v1.yaml` のCaptureRequestにも構造化fieldが無い |
| L95-96 | Howは保存するが実装を約束せず、別解決策・変更不要・複数Repository跨ぎを選べる | UNREACHABLE | 解決策選択を行うcodeが無い（Plan／Prepareが到達不能、§4） |
| L98-99 | 利用者は数値priorityやqueue順を決めない | IMPLEMENTED | `POST /v1/requirements` にpriority fieldが無い（`internal/application/service.go:748` のCaptureRequest） |
| L101-102 | 成功時に一意なRequirement IDと永続化内容を表示し、canonical state登録完了を成功条件とする | IMPLEMENTED | `internal/application/service.go:748` の同一transaction内書き込み、`internal/web/owner.js:1`（capture handler） |
| L106-118 | Backlog一覧とRepository絞り込み、および各Requirementの8項目 | 混在 | 到達可能: 要求内容・状態（`internal/application/readmodels.go:199,243`）、関連Repository（`internal/web/owner.html:9`）、人間対応待ちの区別（`readmodels.go:160-162`）。到達不能／不在: 優先度とその根拠（`domain.PriorityAssessment` は存在するがsnapshotに供給されない。L92bの訂正を参照）、実行中／停滞中／自動回復中の区別（progress deadline不在、§5 L184-197）、進めているIncrementと進捗（Increment不在）、Preview／Stableへの反映（observer未装着）、次に行うこと（`next_action` fieldは `contracts/openapi/openapi-v1.yaml:483` に在るが到達可能statusが4種のため実質1値） |
| L119 | 内部eventや生logを読まずに対応要否を判断できる | IMPLEMENTED | `internal/api/api.go:857` → `internal/application/readmodels.go:243`。raw logを返さない |
| L123-134 | Runnerがclaimして10段階（調査／深掘り／分解／変更／検証／統合／Preview観測／修正／充足評価／Stable反映）を反復する | UNREACHABLE | 10段階すべての実行主体が `internal/runner` にあり非test entryが無い（§4）。分解は `service.go:887`、統合は `IncrementIntegrate` 非発行、Stable反映は `internal/release/pipeline.go:38` |
| L136-137 | 1回のRunner占有・AI session・commit・PRで完結せず、中断・handoff・再開後も関係を失わない | UNREACHABLE | `internal/provider/handoff.go:113,151`（`PrepareHandoff`／`ChainHandoff`）に非test callerが0 |
| L141-142 | 権限内で安全に選べない場合だけ人間へ質問する | IMPLEMENTED | `internal/application/human_input.go:460`。route roleはRunner／Schedulerのみ（`internal/api/api.go:452-455`） |
| L144-149 | 質問に4項目（判断できない理由／決めること／選択肢と影響／停止と継続の範囲）を含める | IMPLEMENTED | `internal/application/human_input.go:460` のrequest構造、`internal/web/owner.html:32`（stopped／continuing表示） |
| L151 | 回答後は新Requirementを作らず元のRequirementを継続する | IMPLEMENTED | `internal/application/human_input.go:563,648`（`RequirementReadyCommand` を同一aggregateへ適用） |
| L155-156 | Repository単位でPreview対象を選び、跨ぎIncrementは同一channel・互換contractのときだけ実行して範囲を明示する | UNREACHABLE | `internal/release/release.go:120` |
| L158-165 | Previewについて6項目を確認できる | UNREACHABLE | `internal/application/release_surface.go:181` は装着済みobserverを要求し、非test装着が0（§4） |
| L167 | Preview障害時は新規claimを止め、Stableへroutingを戻し、進行中Requirementを再開する | UNREACHABLE | 同上 + `internal/runner` 不在 |
| L171-172 | 昇格条件を満たしたPreviewは人間reviewを待たず自動昇格できる | UNREACHABLE | `internal/release/pipeline.go:38` |
| L174-184 | 昇格完了の9条件 | UNREACHABLE | `internal/release/promotion_report.go:74`（`AllConditionIDs`）と `:305` に8条件のreportが在るが、読み口が503（§4） |
| L186-188 | fake／stub／契約test／別Providerでの成功は実稼働確認の代替にしない。実行不能ならPreviewに留める | IMPLEMENTED | `internal/release/promotion_report.go:305` が環境classを入力に取り、`internal/application/release_surface.go:104-105` が明示宣言を必須にする（未宣言はconstructorが拒否、`:137-139`） |
| L190 | RequirementはApplicationがStableへ昇格した時点で完了する | UNREACHABLE | `internal/domain/model.go:526` に非test callerが0 |
| L194-201 | 対象範囲を指定して6種の制御を実行できる | 混在 | Requirement受付停止・claim停止・graceful・immediate・emergency: IMPLEMENTED（`internal/domain/control.go:266`、`internal/api/api.go:382`）。Requirementのpause／resume／cancel: WIDER-THAN-CODE（intentは記録できるがRequirement statusは動かない、§5 L147） |
| L203-205 | 「受付済み」と「完了」を区別し、完了時に対象Runner・process・lease・新規副作用の可否を表示する。到達不能Runnerも隠さない | WIDER-THAN-CODE | requested／acknowledged／effective／verifiedは `internal/domain/control.go:82-86` と `internal/reconciler/verification.go:11` に在り到達可能。ただしprocessの表示元 `domain.ProcessObservation`（`internal/domain/control.go:67`）を書くcodeが無い |
| L209-215 | ループ自身のmodule追加・Preview切替・dogfood・Stable昇格と5項目 | 混在 | 到達可能: Stable独立起動、rollback可能期間中の旧版保持（`internal/update/retention.go:92`、`internal/update/switch.go`）、updater失敗時の独立復旧（`cmd/bootstrap/main.go:1-9`）。到達不能: 旧版削除の実行（`internal/update/retention.go:206`、非test callerが0）、Previewへの切替をLoopが自律的に行うこと（`cmd/bootstrap` は人が叩くCLI） |
| L219-220 | BacklogはInstallationに一つ、Runner slotとAI providerの枠を全体の共有資源として扱う | IMPLEMENTED | `internal/application/allocation.go:447,558`（installation単位の限度とactive数） |
| L222-228 | 5項目を確認・設定できる | 混在 | 到達可能: Installation全体の同時実行上限（`internal/application/allocation.go:520`、control revisionで設定、`internal/web/owner.html:29`）、現在の割当・待機理由・枯渇（`allocation.go:558`）。到達不能／不在: Requirementごとの優先順位と理由（ABSENT、§5 L92b）、Repositoryごとの実行可否・権限・同時実行上限（Repository単位limitのfieldが無い。`grep -rn --include='*.go' 'RepositoryLimit\|PerRepositoryLimit' internal \| grep -v _test.go` → 0件）、AI providerごとの上限（`internal/application/provider_registry.go:736` はinstallation ceilingを流用） |
| L230-231 | 一つのRepositoryの大量要求・障害・再試行で他が無期限に処理されなくなることを許さない | IMPLEMENTED | `internal/scheduler/priority.go` の飢餓規則、`internal/application/allocation.go:625` |
| L235-243 | providerごとに5項目を確認できる | 混在 | 到達可能: 宣言されたprovider一覧と枯渇・停止状態の器（`internal/application/provider_registry.go:736`、`internal/api/api.go:294`）、対応versionの宣言（`internal/provider/compatibility.go:187,197`）。実測不能: 接続済みか／Runner上で利用可能か／認証の健全性／現在の割当（observationを書くcodeが無い、§4）。`internal/web/owner.html:15` 自身が「Nothing on this page starts a Provider CLI or probes anything」と記述 |
| L244-246 | ループがproviderを選び、互換ならhandoffし、満たせないなら黙って縮退せず待機理由を表示する | UNREACHABLE | 判定表 `internal/provider/handoff.go:597,717`（`HandoffDecisionTable`／`DecideHandoff`）に非test callerが0。表示側 `internal/application/provider_registry.go:1526` は別実装 |
| L248-249 | providerは交換可能adapterであり、provider固有の出力形式・error・usage制約をdomain stateへ漏らさない | IMPLEMENTED | `internal/provider/provider.go:133-139`（`FailureClass` への正規化）、`internal/provider/adapters.go:130-137` |
| L253-259 | channelとversionに対応した利用者文書を読める（5項目） | ABSENT | 文書をchannel／versionでroutingするHTTP経路が無い。`grep -rn --include='*.go' 'docs/stable\|docs/preview' internal/api internal/web \| grep -v _test.go` → 0件。`internal/release/docs.go` の検査群は昇格report内でのみ使われる |
| L261-262 | 実装と文書の差異はreleaseを止める不具合として扱う | UNREACHABLE | `internal/release/promotion_report.go:385`（`VerifyLinksResolve` を条件に組み込む）は `BuildPromotionReport` 内にあり、その読み口が503（§4） |
| L268-277 | 利用者向け8状態（queued／active／waiting／needs-input／paused／recovering／completed／cancelled） | WIDER-THAN-CODE | domain側の対応statusは `internal/domain/model.go:94-104` に11種在るが、非testのcodeが到達させられるのは `{captured, framing, needs-input, ready}`（§4）。8状態のうち到達可能なのはqueued相当とneeds-inputの2つ |
| L279-280 | `failed` を安易な終端にせず、回復不能failureは `needs-input` などで保持する | IMPLEMENTED | `internal/domain/model.go:455-521` にRequirementの `failed` status／commandが存在しない |
| L286-293 | 6つの独立lifecycle（Requirement／Increment／Application Release／Worker Execution／Loop Version／Control）を1つのlabelへ押し込めない | IMPLEMENTED | 型として分離している: `internal/domain/model.go`（Requirement／Increment／Execution）、`internal/domain/release.go:40`、`internal/domain/control.go:37`、`internal/application/runner_version.go:291`（Loop Version） |
| L295-296 | Workerが停止してもRequirementはcancelledにならず、queued／paused／recoveringのいずれかになりうる | WIDER-THAN-CODE | `internal/reconciler/reconciler.go:130` はIncrementを `ready` へ戻すのみで、Requirement statusを動かさない。`RequirementRecover` を非testのcodeが発行しない（§4） |
| L300-309 | 通知6種と、通常pollやheartbeatを逐次通知しないこと | ABSENT | 通知adapterが存在しない。`grep -rn --include='*.go' 'Notif\|notify' internal cmd \| grep -v _test.go` → 0件 |
| L313-327 | 初期版の完了条件13項目 | 混在 | 到達可能: (1) Repository登録、(2) 専用UIからのRequirement登録。到達不能: (3)〜(9)、(12)（複数Runner処理、Increment分割、Preview→Stable自動昇格、Preview破壊とStable復帰、3種stopの完了確認、self-update、複数Repository資源配分、全機能Preview確認）。(10) 秘密漏洩・不許可費用・Requirement消失・重複副作用の不在は、`internal/runner/secret_guard.go:12,32` と `internal/quota/quota.go:81` と `internal/application/service.go:147` により部分的にIMPLEMENTED。(11) codex／opencodeの接続状態と正常系実行はDEFERRED-BY-DECLARATION。(13) Preview文書公開はABSENT（L253-259） |
| L331-343 | 確定した仕様判断13項目 | 混在 | 到達可能: 専用UIとGitHub Issue adapterなし（`internal/web/owner.html`、§5 L281）、offline Runnerの制限（設計として `internal/domain/control.go:266` が新規claimを拒否）。到達不能: Preview routingのRepository単位、跨ぎIncrement、Release Contractのversion管理昇格、scheduler の継続評価と理由説明（§5 L179）、Increment単位ownerとhandoff（`internal/provider/handoff.go:717`）、provider-neutral Work Packet（`internal/provider/provider.go:46` に型は在るが非test生成が0）、expand／共存／migrate／contract（`internal/update/schema.go:98,156,209`、非test callerが0）、rollback windowの3条件（`internal/update/retention.go:92`、非test callerが0）。ABSENT: 調査完了条件と運用修復完了条件（対応する状態がcodeに無い） |

## 7. `docs/architecture/overview.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L7-10 | modular monolith（Control Plane 1 deployable）＋remote Runner | WIDER-THAN-CODE | Control Planeは1 deployable（`cmd/control-plane/main.go:212`）。remote Runnerは存在しない（`cmd/runner/main.go:28-31`） |
| L46 | RunnerからControl Planeへのoutbound接続だけを必須とし、利用者machineへinbound portを開けない | IMPLEMENTED | Runner向けrouteはすべてControl Plane側のHTTP endpoint（`internal/api/api.go:384-396`）。Runnerへ接続するcodeが無い |
| L52-56 | Web UI／APIが課題登録・Backlog・Requirement詳細・Question回答・Control Intent発行・停止verification表示・Repository／Runner／Provider Account／Budget管理・Preview／Stable Releaseと利用者文書の表示・typed commandの委譲を行う | 混在 | 到達可能: 課題登録・Backlog・詳細・Question回答・Control発行・verification表示・Repository管理（`internal/web/owner.html:4-9,32`、`internal/api/api.go:176-487`）、Budget（`internal/web/owner.html:29`）。ABSENT: Provider Account管理（型が無い、§6 L30-33）、Runner管理（読みのみ、`internal/application/runner_version.go:291`）、利用者文書の表示（§6 L253-259） |
| L58 | UIとAPIは同じreleaseで配布し、UI固有stateをcanonical domain stateにしない | IMPLEMENTED | `internal/web/web.go:7`（`go:embed`）、`internal/api/api.go:961` |
| L70-77 | Application Servicesが1 request／1 tickをtransaction境界とし、認可・schema validation・idempotency・aggregate load・optimistic concurrency・current state／Event／Outboxのatomic write・DTO生成を行い、transaction中に外部I/Oを実行しない | IMPLEMENTED | `internal/application/service.go:128`（`transact`）、`:147`（`mutate`／idempotency）、`internal/store/firestore/store.go:583`（callback成功後にflush）、`docs`宣言どおりcallback内でHTTPを呼ぶcodeが無い |
| L81-89 | Domain Coreが7項目（framing／priority、increment planningとdependency、lease／fencing／capacity、control effective policy、retry／failure、promotion gate、retention eligibility）を純粋関数で持ち、DB／clock／HTTP／Provider CLIに依存しない | 混在 | 純粋性はIMPLEMENTED（`internal/domain/` にimportされる外部packageが無い）。個別: framing（`internal/domain/model.go:455`）とlease／fencing（`internal/domain/lease.go`）とcontrol policy（`internal/domain/control.go:158`）とpromotion gate（`internal/domain/release.go:123,159`）は存在。priorityはABSENT（§5 L92b）、dependencyはUNREACHABLE（§5 L92c）、retryはUNREACHABLE（§5 L50）、retention eligibilityはUNREACHABLE（`internal/release/pipeline.go:83`／`internal/update/retention.go:92`、いずれも非test callerが0） |
| L93-97 | Schedulerが候補抽出・filter・比較・Decision保存・transaction内Lease発行を行う | 混在 | 抽出・filter・比較・理由は `internal/scheduler/scheduler.go:115`／`internal/scheduler/decision.go` に存在し `internal/application/allocation.go:625` から到達。Decisionの永続化はABSENT（`grep -rn --include='*.go' 'SaveDecision' internal \| grep -v _test.go` → 0件）。Lease発行は `scheduler.Apply` 非呼び出しでUNREACHABLE（§5 L71-73b） |
| L99-100 | AI判断を直接queue順へ適用せず、構造化Decisionをdomain validationへ通す。hard priorityとBudgetをAI判断より先に適用する | IMPLEMENTED | schedulerにAI呼び出しが無く（`internal/scheduler/` にnetwork importが無い）、`internal/application/allocation.go:625` の前に `internal/quota/quota.go:81` の予約が走る（`internal/store/firestore/store.go:457`） |
| L104-112 | Reconcilerがexpired Lease／lost Runner／orphan Execution、pending Outbox／曖昧な外部operation、Preview／Stable deploy、Control Intent ack／verification、旧Release・workspace・module・documentのGCを収束させ、同一tickで冪等かつ件数・時間を制限する | 混在 | IMPLEMENTED: expired Lease（`internal/reconciler/reconciler.go:41`）、Control verification（`internal/reconciler/control_verifier.go:87`）、件数上限（`internal/reconciler/reconciler.go:16`、`MaxBatch=100`）、時間上限（`cmd/control-plane/main.go:200`、5秒）。UNREACHABLE: orphan Execution（`internal/reconciler/orphan.go:28`、非test構築が0）、pending／ambiguous Outbox（`internal/application/outbox.go:328`）、旧module／document GC（`internal/update/retention.go:206`）。ABSENT: Preview／Stable deploy（deploy adapterが無い。`grep -rn --include='*.go' 'DeployTarget\|Deployer' internal \| grep -v _test.go` → 0件） |
| L116-122 | Release ManagerがRelease Contract読み取り・candidate固定・Preview deployとcapability exercise計画・Evidence freshness／Provider coverage／文書整合の評価・channel pointer切替・rollback／再開／文書routingの調停を行う | UNREACHABLE | `internal/release/bundle.go`（`AssembleCandidate`）と `internal/release/promotion_report.go:305` は存在し `internal/application/release_surface.go:134-145` から呼ばれるが、そのobserverが非test装着0（§4）。channel pointer切替は `internal/release/pipeline.go:38`、非test callerが0 |
| L123 | Release Manager自身もControl Intentとfencingに従う | IMPLEMENTED | `internal/release/pipeline.go:38` の引数が `domain.EffectiveControlResult` と `domain.PermitDecision`（機構としては存在。到達性は上行） |
| L127-134 | Adapter portsとして Source Control／Artifact Store／Application Deploy／AI Provider Observation／Notification／Clock・ID・Signer を持ち、payloadをdomain entityにせずtyped Observationへ正規化する | 混在 | 存在: Source Control（`internal/runner/sourcecontrol.go:200`、UNREACHABLE）、Clock／ID／Signer（`internal/application/ports.go`、`internal/update/anchor.go`、IMPLEMENTED）。ABSENT: Artifact Store、Application Deploy、Notification。AI Provider Observationは書き手が無い（§4） |
| L130 | Control Planeには実Provider実行を置かない | IMPLEMENTED | `internal/provider/adapters.go:139-144`（`NoExec`）、`cmd/control-plane` はprovider packageをimportしない |
| L140-144 | Runner Daemonがoutbound heartbeat／claim、control revision受信・ack、Execution slot管理、Stable／Preview routingに従ったlocal module起動、Result受理確認までのcheckpoint保持を行う | UNREACHABLE | `internal/runner/control_agent.go:25`、`internal/runner/control_loop.go:15`、`internal/runner/lease.go` に非test entryが無い（§4） |
| L146-152 | Workspace Managerがcheckout／Increment専用workspace作成、path・symlink・git common dir・ownership検証、Artifact snapshotとcleanup eligibility管理、workspace外writeのsandbox防止を行う | UNREACHABLE | `internal/runner/workspace.go:11`（`NewWorkspace`）非test callerが0。sandboxは `internal/runner/confinement.go:114` に在り `cmd/bootstrap/main.go:133` から到達するが、workspace管理側は不在 |
| L153 | Git worktreeは最初のSource Control Adapterとして利用できるがdomain contractにしない | IMPLEMENTED | `internal/runner/git.go:138`（`GitSourceControl`）はrunner側のみ。`internal/domain` にgit参照が無い |
| L156-161 | Process Supervisorが独立process groupでchildを起動、stdout／stderrを容量制限logへ、heartbeatとprogress eventを分離、graceful checkpoint／TERM／KILL、daemon再起動時のreconcileを行う | UNREACHABLE | `internal/runner/supervisor.go:73`、`internal/runner/log.go:38`（`NewBoundedLog`）に非test entryが無い |
| L165-170 | Secret BrokerがOS keyring等からcredentialを取得し、Execution／Repository／Provider／期間に限定して注入、Work Packet・canonical state・Artifactへ出さず、output・commit・outbound payloadをsecret guardへ通し、stop／expiryで払い出しを止める | 混在 | UNREACHABLE: `internal/runner/secret_broker.go:149`（`NewSecretBroker`）非test callerが0。IMPLEMENTED（部分）: secret guard自体は `internal/runner/secret_guard.go:12,32,44` に在り、`:44` は `internal/api/operator_record.go:227` から到達する |
| L171 | Control PlaneはProvider CLIのcredentialを保持しない | IMPLEMENTED | `cmd/control-plane/main.go:107-135` が読む環境変数にprovider credentialが無く、`internal/store/firestore` にcredential fieldが無い |
| L175-185 | Provider Adapterが7項目（capability／version probe、Work Packet変換、process起動contract、structured result／usage抽出、error正規化、cancel／timeout、live Preview exercise）をCodex／Claude／opencodeごとに実装し、他moduleはprovider固有JSON／exit code／messageを解釈しない | 混在 | IMPLEMENTED: Work Packet変換とargv（`internal/provider/adapters.go:122-128`）、result／usage抽出（`:130-132`）、error正規化（`:133-137`）、version interval宣言（`internal/provider/compatibility.go:187`）、非解釈境界（`internal/provider/provider.go:133-139`）。ABSENT: process起動contract（`NoExec`、`internal/provider/adapters.go:318`）、cancel／timeout挙動、capability probe。live Preview exerciseはcodex／opencodeについてDEFERRED-BY-DECLARATION |
| L189-192 | Validation Executorが宣言済みfast／full／smoke／capability exerciseを、shell文字列でなく検証済みargv／working directory／environmentで起動し、resultをEvidenceへ正規化し、outputの容量と秘密を制御する | UNREACHABLE | argv境界とguardは `internal/runner/secret_guard.go:12` と `internal/runner/sourcecontrol.go:258`（git subcommand allowlist）に在るが、実行主体（`internal/runner/orchestrator.go:286`）に非test entryが無い。Evidence正規化を行うcodeがrunner側に無い |
| L196-199 | Integration Adapterがcommit／branch／PR／merge／artifact publishを実行し、PRは任意で人間reviewを待たず、expected revisionとoperation idで重複検出し、expired fencing tokenのArtifactをStable releaseへ含めない | UNREACHABLE | `internal/runner/forgepublish.go:234`（`NewForgePublisher`）に非test callerが0。`internal/application/publication.go:152`（`PublishChange`）も非test callerが0 |
| L205-212 | Module versioningの7項目 | 混在 | IMPLEMENTED: manifestのversion固定と署名・digest検証（`internal/update/update.go:127`）、rollback window中の旧binary保持（`internal/update/retention.go:92`）、channel routing（`internal/update/switch.go`）。UNREACHABLE: 参照が無くなってからの削除実行（`internal/update/retention.go:206`）、Repository単位のLoop Release routing（`internal/release/release.go:120`）。ABSENT: 新旧implementationを同じsource treeで並存させるbuild-time registry（`grep -rn --include='*.go' 'ModuleRegistry\|moduleRegistry' internal \| grep -v _test.go` → 0件） |
| L214 | Goのruntime plugin、任意code download、言語内dynamic linkingを使わない | IMPLEMENTED | `grep -rn '"plugin"' --include='*.go' internal cmd` → 0件 |
| L219-228 | State ownership 8行 | 混在 | IMPLEMENTED: Requirement／Lease／Control Intentのcanonical store（`internal/store/firestore/store.go:200`）、external systemをObservationとして読む（`internal/application/repository.go:288`）。ABSENT／UNREACHABLE: Work Packet metadataのcanonical保存、Provider credentialのRunner local store、workspace／child processのreconcile、user documentation bundle、raw diagnostic logのbounded storage（いずれも§4のrunner不在） |
| L232 | 初期版はHTTPS JSON APIで、常時双方向socketを必須にしない | IMPLEMENTED | `internal/api/api.go`（`net/http` のみ）。WebSocket importが無い |
| L234-242 | Runner flow 7段（session認証とcapability更新、long poll／bounded pollでclaim、Lease／Work Packet／Control revision取得、progress／heartbeat／checkpointを別endpointへ、外部operation前のpermit再確認、Result／Observationのidempotency key付き送信、accept／rejectによるlocal cleanup） | 混在 | endpointはすべて存在（`internal/api/api.go:126-146,384-396,427-436`）。UNREACHABLE: capability更新（`grep -rn --include='*.go' 'RunnerCapability' internal \| grep -v _test.go` → 0件でABSENT）、Work Packet取得（`internal/provider/provider.go:46` の型は非test生成が0）、local cleanup判断（runner不在） |
| L244-245 | 各mutationは `request_id` を必須とし、retryで二重適用しない。API versionをURLで明示し、Stable／Preview Runnerの共存期間は後方互換を維持する | IMPLEMENTED | `internal/application/service.go:147`（`mutate`）と `requireRequest`（同ファイル）、path prefix `/v1/`（`internal/api/api.go:176-487`） |
| L249-254 | Availability 6項目 | 混在 | IMPLEMENTED: Control Plane停止中もcanonical storeに残る（Firestore）、UI停止時のemergency control用API（`internal/api/api.go:382` はUIと独立）。UNREACHABLE: Runnerの新規claim停止と接続回復待ち、他RunnerによるLease引き継ぎ（§4）、Outbox保持とbounded retry（`internal/application/outbox.go:178`）。ABSENT: Provider停止時にProvider Accountをwaitingにする（Provider Account型が無い） |
| L256-257 | Control Planeの高可用clusterを初期要件にせず、managed platformの再起動とdurable storeで復旧する | DEFERRED-BY-DECLARATION | managed platformはCloud Runを名指す（`docs/architecture/technology.md:11`）。`infra/main.tf:66-82`（`min_instance_count=0`）はplan段階まで |
| L261-268 | Security boundaries 5層（browser identity、Control Plane SA、Runner identity、local Provider credential、Repository／Deploy credential） | 混在 | IMPLEMENTED: browser identity（`internal/api/auth.go:102-125`）、Runner identity（`internal/runner/protocol.go:110,138`、Ed25519 challenge）、Control Plane SA（`infra/main.tf:24,60`）。UNREACHABLE: local Provider credentialの分離（`internal/runner/secret_broker.go:149`）。ABSENT: Deploy credentialの境界（deploy adapterが無い） |
| L270 | browser content、Requirement、Provider outputをuntrusted inputとしてescape／validateする | IMPLEMENTED | `internal/web/owner.js` は `textContent` で描画（`:1` の `renderRepositories`）、`internal/api/api.go` はJSON decodeとschema検査を通す |
| L271 | Runnerが返す成功を単独で信用せず、Evidenceとexternal Observationを検証する | IMPLEMENTED | `internal/application/service.go:1660`（`domain.AcceptExecutionResult` にlease／fence／revisionを要求）、`internal/domain/model.go:526`（完了はStableReleaseProof必須） |
| L272 | Artifactをdigestでcontent-addressし、release bundleへ含める前に署名／provenanceを確認する | IMPLEMENTED | `internal/release/bundle.go:318,363,384`（`VerifySource`／`VerifyCandidateDigests`／`VerifyCandidateAgainstContract`）、`internal/update/update.go:127`（Ed25519） |
| L273 | Control PlaneからRunnerへ任意shellを送らず、versioned command schemaを送る | IMPLEMENTED | Runner向けresponseにcommand文字列fieldが無い（`contracts/openapi/openapi-v1.yaml:668-669` のClaimResponse） |
| L274 | Repository Contractのcommand変更もPreview Release Contractで実証する | ABSENT | Repository Contract型が存在しない（§6 L72-81） |
| L277-286 | 意図的に採用しないもの9件 | IMPLEMENTED | GitHub Issue state store（§5 L281）、microservice分割（単一binary）、完全event sourcing（`internal/store/firestore/store.go` はcurrent state＋event併記）、distributed filesystem・inbound remote shell・raw conversation handoff（該当codeが存在しない）、1 Requirement 1 PR（PR生成codeが到達不能）、無署名plugin（`overview.md` L214行）、test成功だけのpromotion（`internal/release/promotion_report.go:74` の8条件） |

## 8. `docs/architecture/domain-model.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L7-13 | Model方針7項目（正本を一つ、外部状態はObservation、current state＋append-only Event、完全event sourcing不採用、5 lifecycleの独立、Command→transaction検証→Event／Outbox同時記録、時間・乱数・外部I/Oをdomain判断へ持ち込まない） | IMPLEMENTED | `internal/store/firestore/store.go:200,583`、`internal/application/service.go:1773`、`internal/domain/model.go:456-462`（actorとatを明示引数で要求） |
| L17-27 | Installation aggregateが `installation_id`／`owner_id`／`control_revision`／`budget_policy_id`／`created_at` を持つ | ABSENT | `grep -rn 'type Installation' internal --include='*.go'` → 0件。`internal/store/firestore/client.go:59` がinstallation documentを1件作るだけで、列挙された属性のうち `owner_id`／`budget_policy_id`／`created_at` はどこにも無い |
| L36-47 | Repository属性（opaque `repository_id`、値objectとしてのsource locator、`(forge, owner, name)` の正規化一意性、userinfo破棄、`version`、Stable／Preview Application Release、Stable／Preview Loop Version routing、Repository Contract version、Release Contract version、control state） | 混在 | IMPLEMENTED: opaque id、locator値object、正規化一意性、userinfo破棄、`version`（`internal/domain/repository.go` 全体、`internal/application/repository.go:132`）。ABSENT: Repository ContractのversionとRelease Contractのversion field、Stable／Preview Application ReleaseとLoop Version routing（`grep -rn --include='*.go' 'PreviewRelease\|LoopVersionRouting' internal \| grep -v _test.go` → 0件） |
| L55-64 | Requirement属性（原文、`problem_frame`、関連Repository集合、状態、Priority Assessment、Question集合、Increment集合、完了を支えるStable Release集合、`version`） | 混在 | IMPLEMENTED: 原文（`internal/store/firestore/store.go:271` co-locate）、状態、`version`、Increment id集合（`contracts/openapi/openapi-v1.yaml:468`）。ABSENT: `problem_frame`（`grep -rn 'ProblemFrame\|problem_frame' internal --include='*.go' \| grep -v _test.go` → 0件）、Priority Assessment（§5 L92b）、Question集合（`type Question` が無い。needs-inputは別side table、`internal/application/human_input.go`）、完了を支えるStable Release集合。関連Repositoryは1件linkのみで集合ではない（`internal/domain/repository.go` の `RequirementRepositoryLink`） |
| L66-67 | 原文は履歴として保持しProblem Frameと混ぜない。Howは `proposed_approach` として保存できるがConstraintへ自動昇格させない | WIDER-THAN-CODE | 原文保持はIMPLEMENTED（`internal/store/firestore/store.go:271`）。`proposed_approach` fieldが存在しないため、保存できるという許可はcodeより広い |
| L69-84 | Priority Assessmentの10要素と、固定scoreを正本にせずDecisionを残して再評価すること | ABSENT | 型が無い（§5 L92b）。Decision永続化も無い（§7 L93-97） |
| L86-101 | Increment属性10項目と「1 Requirementに複数Increment」「Increment成功でRequirementは自動完了しない」 | 混在 | IMPLEMENTED（型として）: id、親Requirement、状態、`version`、Preview candidate／evidence digest、Retry Budget field（`internal/domain/model.go:264,266`）。自動完了しないことは `internal/domain/model.go:526` が別commandであることで成立。ABSENT: 目的と完了条件、対象Repository集合、dependency、Resource Claim、Work Packet、ArtifactとEvidence（`internal/domain/model.go` のIncrement structにこれらのfieldが無い） |
| L103-115 | Resource Claimの6分類と「読み取り調査は共有可能」「変更開始前にClaimを具体化」「Claimが広がれば競合を再評価」 | ABSENT | `grep -rn 'ResourceClaim\|resource_claim' internal --include='*.go' \| grep -v _test.go` → 0件。`internal/scheduler/scheduler.go` の `ResourceRequest`（read／write mode）は別概念で、`internal/application/allocation.go:466` が1 requirementに1 write requestを機械的に付けるだけ |
| L117-130 | Work Packetの8内容と「生conversation・raw prompt・credential・無制限logを含めない」「事実と判断を分ける」 | UNREACHABLE | `internal/provider/provider.go:46`（`WorkPacket`）は存在し `internal/provider/adapters.go:26` でserializeされるが、非testの生成箇所が0（`grep -rn --include='*.go' 'WorkPacket{' internal cmd \| grep -v _test.go` → 0件）。canonical storeにWork Packet collectionが無い |
| L132-143 | Execution属性7項目と「RequirementをRunnerへ固定しない」「1 Incrementに同時1有効Execution」 | 混在 | IMPLEMENTED（型として）: `execution_id`、Increment、Runner、Lease／fencing、終了理由（`internal/domain/model.go` のExecution struct、`internal/domain/lease.go`）。ABSENT: Loop Version、Provider Run集合、command deadline／progress deadline、checkpoint field（`grep -rn --include='*.go' 'CommandDeadline\|ProgressDeadline' internal \| grep -v _test.go` → 0件） |
| L145-156 | Lease属性6項目と「期限延長はackを示すだけで進行signalとは分ける」 | IMPLEMENTED | `internal/domain/lease.go`、`internal/application/service.go:229`（`Renew`）と `:474`（`Heartbeat`）が別command／別route（`internal/api/api.go:427,394`） |
| L158-166 | Runner属性（platform、capacity、sandbox capability、到達可能Repository、利用可能Provider CLIとversion、health、last seen、control revision ack、active Execution） | 混在 | IMPLEMENTED: control revision ack と last seen（`internal/application/service.go:571`、`domain.RunnerObservation`、`internal/domain/control.go:56-63`）、報告されたversion（`internal/application/runner_version.go:291`）。ABSENT: platform、capacity、sandbox capability、到達可能Repository、利用可能Provider CLI |
| L168-185 | Provider Account／Provider Runの属性群と「provider固有errorをAdapter内でNormalized Failureへ変換する」 | 混在 | IMPLEMENTED: 正規化（`internal/provider/provider.go:133-139`）。ABSENT: Provider Account型、Provider Run型（`grep -rn 'type ProviderAccount\|type ProviderRun' internal --include='*.go'` → 0件）。`internal/application/provider_registry.go` の `ProviderObservationLog`／`ProviderAssignment` は別形で、書き手が無い（§4） |
| L187-193 | Artifact／Observation／Evidenceの3区分と「Evidenceは秘密やraw conversationを含まず再検証方法を持つ」 | 混在 | IMPLEMENTED: `domain.CapabilityEvidence`（`internal/domain/release.go:21`）と `contracts/schemas/evidence.json`。ABSENT: domain の `Artifact` 型（`internal/provider/provider.go:33` のものはprovider package内でruntime非生成）、domain の `Observation` 型 |
| L195-207 | Release Candidate／Releaseの7要素と「全gate後に同じbundleをStableへ昇格する」 | UNREACHABLE | `internal/domain/release.go:40`（`ReleaseCandidate`）と `internal/release/bundle.go` は存在。昇格実行 `internal/release/pipeline.go:38` に非test callerが0 |
| L209-219 | Control Intentの5属性、7 mode、単調増加revision、ack／verification、「より禁止的なIntentを優先」「古いrevisionの許可で新しい停止を上書きしない」 | IMPLEMENTED | `internal/domain/control.go:13-19,37-45,112-129,158` |
| L221-230 | Questionの4要素と「曖昧さだけを理由に安易に作らない」 | WIDER-THAN-CODE | `type Question` が無く、`internal/application/human_input.go:460` のside tableが理由／選択肢／停止scope／answerを持つ。domain aggregateの一部としてではない |
| L232-235 | EventとOutbox Itemを同じdatabase transactionでcurrent stateと共に書く | IMPLEMENTED | `internal/application/service.go:1707`（`effectOutbox`）と `:1773`（`record`）が同一 `UnitOfWork` 内、`internal/store/firestore/store.go:583` でまとめてflush |
| L239-256 | 関係図（Installation→1 Backlog→Requirement 1..*Increment→*Execution→1 Lease→1 Runner／*Provider Run／*Artifact等、Repository、Runner、Provider Account、Control Intent、Release Candidate→*Capability Evidence等） | WIDER-THAN-CODE | Installation aggregate、Provider Run、Provider Account、Artifact／Observationがcodeに存在しない（上記各行）。実在するedgeはRequirement→Increment→Execution→Lease→Runnerと Repository、Control Intentのみ |
| L261-273 | Requirement lifecycle表: `captured`→`framing`／`cancelled` | IMPLEMENTED | `internal/domain/model.go:475-479`（framing）、`:513-518`（cancel） |
| L264 | `framing`→`ready`／`needs-input`／`cancelled` | IMPLEMENTED | `internal/domain/model.go:480-484`、`:494-499`、`:513-518` |
| L265 | `ready`→`active`／`waiting`／`paused`／`cancelled` | IMPLEMENTED | `internal/domain/model.go:485-489`、`:489-494`、`:508-513`、`:513-518` |
| L266 | `active`→`evaluating`／`waiting`／`needs-input`／`paused`／`recovering`／`cancelled` | IMPLEMENTED | `internal/domain/model.go:499-518` の各case |
| L267 | `waiting`→`ready`／`active`／`paused`／`cancelled` | IMPLEMENTED | `internal/domain/model.go:480-489`、`:508-518` |
| L268a | `needs-input`→`ready` | IMPLEMENTED | `internal/domain/model.go:480-484`（`RequirementReadyCommand` が `RequirementNeedsInput` を許可） |
| L268b | `needs-input`→`framing` | WIDER-THAN-CODE | `internal/domain/model.go:475-479`: `RequirementStartFraming` は `RequirementCaptured` からのみ。`needs-input` からframingへ戻すcommandが無い |
| L268c | `needs-input`→`active` | WIDER-THAN-CODE | `internal/domain/model.go:485-489`: `RequirementStart` が許可するのは `ready`／`recovering`／`waiting` のみ |
| L269 | `paused`→直前の安全な非終端状態 | WIDER-THAN-CODE | `internal/domain/model.go:466-521` を全case走査すると、`current.Status == RequirementPaused` を許可するcaseは `RequirementCancel`（`:513-518`）だけである。pausedから復帰する遷移が一つも無い |
| L270a | `recovering`→`active`／`paused` | IMPLEMENTED | `internal/domain/model.go:485-489`、`:508-513` |
| L270b | `recovering`→`waiting` | IMPLEMENTED | `internal/domain/model.go:489-494` |
| L270c | `recovering`→`needs-input` | WIDER-THAN-CODE | `internal/domain/model.go:494-499`: `RequirementNeedInput` が許可するのは `framing`／`active`／`evaluating` のみ |
| L271a | `evaluating`→`completed` | IMPLEMENTED | `internal/domain/model.go:526-530`（`CompleteRequirementFromRelease` は `evaluating` を要求） |
| L271b | `evaluating`→`needs-input` | IMPLEMENTED | `internal/domain/model.go:494-499` |
| L271c | `evaluating`→`active` | WIDER-THAN-CODE | `internal/domain/model.go:485-489`: `RequirementStart` は `evaluating` を許可しない |
| L272-273 | `completed`／`cancelled` は終端 | IMPLEMENTED | `internal/domain/model.go:513-518`（cancelはcompleted／cancelledを拒否）。他のcommandはいずれも許可status列にcompleted／cancelledを含まない |
| L261-273 全体 | この表が示す遷移を実際に発行できること | UNREACHABLE | 非testのcodeが発行するのは3 commandのみ（§4）。表の残り8 commandは到達不能 |
| L275-276 | `failed` をRequirementの終端にせず、failureはExecution／Provider Run／Evidenceに記録する | 混在 | Requirementに `failed` が無いのはIMPLEMENTED（`internal/domain/model.go:94-104`）。Provider Runへの記録はABSENT（型が無い） |
| L280-293 | Increment lifecycle表12状態・全遷移 | IMPLEMENTED | `internal/domain/model.go:572-660`（`DecideIncrement`）の各caseが表と一致する。`preview-validating`→`execute`→`executing` を含む（`internal/domain/model.go:602-606` の `IncrementExecute` が `IncrementPreviewValidating` を許可する） |
| L280-293 全体 | この表の遷移を実際に発行できること | UNREACHABLE | 非test発行は `IncrementRecover` のみ（§4） |
| L295 | Incrementの一時停止は専用statusを設けず、Control Intentと親Requirementのpausedの組み合わせで表現する。再提案は新Incrementで表現する | WIDER-THAN-CODE | `IncrementStatus` にpausedが無いのはIMPLEMENTED。しかし親Requirementを `paused` にできない（§5 L147、L269）ため、宣言された表現手段の片方が成立しない |
| L297 | Preview失敗時は同じArtifactを上書きせず修正Incrementまたは新Artifactを作る | ABSENT | Artifact型もPreview失敗を扱うcodeも無い |
| L301-307 | Execution・Lease lifecycle図（offered→leased→starting→running→checkpointing→succeeded、およびfailed／terminated／lost／expired／revoked） | IMPLEMENTED | `internal/domain/model.go` のExecutionStatus定義と `internal/reconciler/reconciler.go:33-39`（terminal集合）。`internal/domain/model.go:158-166` に9値すべてが存在する（`offered`／`leased`／`starting`／`running`／`checkpointing`／`succeeded`／`failed`／`terminated`／`lost`）。図の `expired`／`revoked` はLease側の状態（`internal/domain/lease.go`） |
| L309 | schedulerがtransaction内でIncrement状態・Control Intent・Resource Claim・capacityを再検証してLeaseを発行する | UNREACHABLE | 発行は `internal/application/service.go:1132`（`Claim`）が行い、`scheduler.Apply` は呼ばれない（§5 L71-73b）。Resource Claimの再検証はABSENT（L103-115） |
| L310 | RunnerはLease取得後、最新Control IntentとRepository Contractを取得してからprocessを開始する | UNREACHABLE | runner不在（§4）。Repository ContractはABSENT |
| L311 | heartbeatと進行eventを分ける | IMPLEMENTED | `internal/api/api.go:394`（heartbeat）と `:396`（checkpoints）が別route |
| L312 | Lease更新には現在のfencing tokenとcontrol revisionを要求する | IMPLEMENTED | `internal/application/service.go:229`（`RenewRequest` にfencing tokenとcontrol revision） |
| L313 | expired／revoked LeaseのExecutionはResultをcommitできない | IMPLEMENTED | `internal/application/service.go:1660`（`domain.AcceptExecutionResult` にlease有効性を要求） |
| L314 | retryは新Executionと新fencing tokenを作り、古いExecutionを復活させない | IMPLEMENTED | `internal/reconciler/reconciler.go:130`（Incrementを `ready` へ戻す。Executionは terminal のまま、`:33-39`） |
| L318-327 | Release lifecycle図と「`promotable` には全capability・対象実Provider・文書・rollback／resume evidenceが必要」「Stable昇格はversionを変えずchannel pointerをtransactionalに切り替える」「非atomicな外部deployはOutboxとreconciliationで収束させ途中状態を表示する」 | UNREACHABLE | `internal/release/promotion_report.go:74,305` と `internal/release/pipeline.go:38`。読み口が503、実行経路が非test callerゼロ（§4）。外部deploy adapterはABSENT |
| L333-340 | 各操作直前にInstallationから対象entityまでのIntentを合成し、severity順序で解決する。7 boundaryでeffective policyとrevisionを再確認する | IMPLEMENTED（部分） | `internal/domain/control.go:112-129,158`（合成とseverity）、`internal/domain/control.go:239-249`（revision厳密一致）。7 boundaryのうち到達可能なのはclaim／intake／checkpoint／accept-result（`internal/api/api.go:374,384,392,396`）。process開始・credential取得・integration・Preview deploy・Stable promotionの4+はUNREACHABLE |
| L344-349 | Stop completion 4段（requested／acknowledged／effective／verified）の意味 | IMPLEMENTED | `internal/domain/control.go:82-86`（`ControlProgress`）、`internal/application/service.go:543`（ack）、`internal/reconciler/control_verifier.go:87`（verified）。ただしverifiedの根拠に含まれる「到達Runnerのprocessが止まり」の観測はABSENT（§5 L164-166a） |
| L351-352 | 到達不能Runnerはackなしでもeffectiveにでき、物理processの消滅を観測できない間はその事実を表示するが権限失効後のResultは確定できない | IMPLEMENTED | `internal/reconciler/verification.go:11`（`VerificationBlockedUnreachable`／`VerificationBlockedAmbiguous`）、`internal/application/service.go:1660` |
| L356-359 | Immediate stop 4項目（TERM→KILL、送信済みrequestを取り消せたと仮定せずreconcile、old fencing tokenのintegration／deploy／promotionを拒否、workspaceとcheckpointを削除せず秘密を除去して保持） | 混在 | IMPLEMENTED: old tokenの拒否（`internal/domain/control.go:297`、`internal/application/service.go:1660`）。UNREACHABLE: TERM→KILL（`internal/runner/supervisor.go:73`）、reconcile（`internal/application/outbox.go:328`）、workspace保持（`internal/runner/workspace.go:11`） |
| L363-373 | 外部副作用契約6段と「idempotency keyやCASを持たない外部systemは専用Integration Adapterで重複検出し、曖昧なtimeout後は必ずread-after-writeで観測し、判定不能なら `needs-input` へ進む」 | UNREACHABLE | 6段のうち第1段（Operation IntentとOutbox Item作成）は `internal/application/service.go:1707` で到達可能。第2〜6段は `internal/application/outbox.go:365,454,458,540` にあり、dispatcherが非test構築0（§4）。`needs-input` 隔離も同じ（`internal/application/outbox.go:540`） |
| L377-382 | Handoff契約6項目 | UNREACHABLE | `internal/provider/handoff.go:113,151,181,717`。いずれも非test callerが0 |
| L386-390 | Retention 5項目 | UNREACHABLE | `internal/update/retention.go:92,206,273`、`internal/release/pipeline.go:83`。いずれも非test callerが0 |

## 9. `docs/architecture/failure-model.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L7-12 | 原則6項目（failureとRequirement失敗の非同一視、classの分離、retry前の状態観測、fingerprintとapproachの反復制限、回復不能でもRequirementを消さない、安全性不明なら停止優先） | 混在 | IMPLEMENTED: class分離（`internal/domain/release.go:174-189`）、Requirementに `failed` が無い（`internal/domain/model.go:94-104`）、安全側停止（`internal/domain/control.go:266`、`internal/store/firestore/store.go:219` の fail closed）。UNREACHABLE: fingerprint／approach反復制限（`internal/domain/release.go:204,217`、`Consume` 非test callerが0） |
| L16-34 | Failure taxonomy 18 class、各classの自動処理とRequirementへの影響 | 混在 | 18 classのうち17がconstantとして存在（`internal/domain/release.go:174-189` の16件＋`internal/provider/provider.go:133-139`）。`unknown` classと「自動処理」「Requirementへの影響」列を実行するcodeは存在しない: `grep -rn --include='*.go' 'FailureClass' internal \| grep -v _test.go` の結果に、classからRequirement statusへ写す分岐が1件も無い。よってclass定義はIMPLEMENTED、taxonomyの右2列はABSENT |
| L38-44 | Retry Budgetの6 dimension | WIDER-THAN-CODE | `internal/domain/release.go:193-202`（`RetryBudget`）が持つのは `MaxAttempts`／`Attempts`／`MaxSameFingerprint`／`SameFingerprint`／`LastFingerprint` 相当のみ。Executionごとcost、Provider Accountごとcooldownと日次usage、Installation全体の時間・compute・network・AI budgetのdimensionが無い |
| L47-48 | Budget消尽時にcounterをresetして同じ処理を繰り返さない。新Budget発行はObservation変化・approach変更・人間承認のときだけ | UNREACHABLE | `internal/domain/release.go:217`（`Consume`）に非test callerが0 |
| L52-58 | Circuit breakerをProvider／Source Control／Deploy Targetごとにclosed／open／half-openで持ち、5規則に従う。circuit状態をRequirement stateへ埋め込まずResource Observationとして保持する | UNREACHABLE | `internal/provider/breaker.go:214`（`NewBreaker`）、`:321`（`ApplyObservation`）、`:444`（`Probe`）に非test callerが0。Source Control／Deploy Target用のbreakerはABSENT（provider名は3つに固定、`internal/provider/pool.go:108`） |
| L60 | 永続的な認証失敗や権限不足をtransient retryしない | IMPLEMENTED（機構として） | `internal/provider/breaker.go:130`（`ActionForFailureClass`）が class ごとにactionを分ける。到達性は上行 |
| L66-70 | 接続中の停止5段（durable write、新規claimとcredential発行の停止、Runnerのackとchild checkpoint／terminate、Result／外部Operationのreconcile、active Lease消滅の検証） | 混在 | IMPLEMENTED: durable write（`internal/application/service.go:1502`）、claim停止（`internal/domain/control.go:272,274,276`）、ack（`internal/application/service.go:543`）、Lease検証（`internal/reconciler/control_verifier.go:87`）。UNREACHABLE: credential発行停止（`internal/runner/secret_broker.go:149`）、child terminate（`internal/runner/supervisor.go:73`）、外部Operation reconcile（`internal/application/outbox.go:328`） |
| L74-80 | partition中の6項目 | 混在 | IMPLEMENTED: Lease expiry（`internal/reconciler/reconciler.go:41`）、fencing前進と旧Result拒否（`internal/application/service.go:1660`）、長寿命credentialを持つlocal processのintegration／promotionをControl Plane側で拒否（`internal/domain/control.go:266`）。ABSENT: local Provider processが残る可能性の表示（`domain.ProcessObservation` の書き手が無い）、external credentialのexpiry、再接続時のreconcile後にのみ新規claimを許すこと |
| L81 | 「processが絶対に存在しない」と「authoritativeな結果を確定できない」を区別する | IMPLEMENTED | `internal/reconciler/verification.go:11` の4状態（`VerificationBlockedUnreachable` と `VerificationBlockedAmbiguous` を別値にしている） |
| L85-93 | Ambiguous side effectの5段順序と「非冪等な外部APIを通信errorだけの理由で再試行しない」 | UNREACHABLE | `internal/application/outbox.go:365`（`beforeEffect`）、`:540`（`resolveObservation`）。dispatcherが非test構築0（§4） |
| L97-107 | Preview failure 7項目 | UNREACHABLE | `internal/release/promotion_report.go:225`（`Promotable`）、`internal/release/pipeline.go:38`、`internal/release/release.go:120`。いずれも非test callerが0 |
| L111-121 | Promotion failureをsagaとして扱う7項目と「利用者に `promoting`／`rolling-back` を表示し部分成功をStable完了と報告しない」 | UNREACHABLE | 同上。`internal/release/promotion_report.go:47`（`ConditionState`）は tri-state を持つが読み口が503（§4） |
| L125-135 | Secret incident 7段と「秘密の値そのものをIncident Evidenceへ保存しない」 | 混在 | IMPLEMENTED: redaction（`internal/runner/secret_guard.go:44`、`internal/api/operator_record.go:227`）、argv／env guard（`internal/runner/secret_guard.go:12,32`）、evidence schemaにvalue fieldが無い（`contracts/schemas/evidence.json`）。ABSENT: 停止・失効・rotation・露出範囲確認・再実証を行うcode（`grep -rn --include='*.go' 'Rotate\|revokeCredential' internal \| grep -v _test.go` → 0件） |
| L139-152 | Failure injection 12 scenario をPreview昇格前に意図的に試す | UNREACHABLE | scenarioの多くはtestとして存在する（例 `internal/runner/crash_test.go:437`、`internal/reconciler/failure_matrix_test.go`、`internal/reconciler/fencing_journey_test.go`）が、「Preview昇格前に実行する」gateがcodeに無く、`internal/release/promotion_report.go:74` の条件集合にfailure injectionの条件が無い |
| L154 | fakeによる故障注入に加え、破壊や費用を伴わない範囲でPreview実環境の実物境界を確認する | DEFERRED-BY-DECLARATION | `docs/architecture/release-contract.md:228-231` が `preview-gcp` の4点をD1へ、`preview-local` を実機と宣言する。実機側の実行主体（runner）は§4のとおり不在 |
| L158 | 同じIncrementへ同時に2つの有効fencing tokenは存在しない | IMPLEMENTED | `internal/domain/lease.go`（scopeごと単調増加）、`internal/application/service.go:1132` のtransaction内再検証 |
| L159 | expired tokenのResultはRequirement／Release／Stableを前進させない | IMPLEMENTED | `internal/application/service.go:1660` |
| L160 | stop effective後に新しいauthoritative副作用を開始しない | IMPLEMENTED | `internal/domain/control.go:297`（`EffectFromPermit` が唯一の構成経路） |
| L161 | Requirement完了は対応Stable Releaseとfresh Evidenceを必ず参照する | IMPLEMENTED（機構として） | `internal/domain/model.go:526-534`（`StableReleaseProof` 必須）。発行経路は非test callerゼロ（§4） |
| L162 | Release Contract未確認capabilityが一つでもあればStableへ昇格しない | IMPLEMENTED（機構として） | `internal/release/promotion_report.go:225`（`Promotable`）、`internal/domain/release.go:159`（`PromoteReleaseWithPermit`） |
| L163 | Provider依存変更は対象実Provider Evidenceなしに昇格しない | IMPLEMENTED（機構として） | `internal/release/promotion_report.go:305` の条件集合と `contracts/release-contract/foundation-capabilities.json:8`（`representative_provider`） |
| L164 | credentialはcanonical state、Work Packet、Release Artifactへ入らない | IMPLEMENTED | `internal/store/firestore/store.go` にcredential fieldが無い、`internal/provider/adapters.go:58` 近傍のargv secret検査、`internal/runner/secret_guard.go:12` |
| L165 | hard Budget超過後に新規resource消費を開始しない | IMPLEMENTED | `internal/quota/quota.go:81`（`Reserve`）、`internal/store/firestore/store.go:457`（transactionごとの予約） |
| L166 | Preview failureでStable runtime／docs／rollback targetを削除しない | IMPLEMENTED（機構として） | `internal/update/retention.go:92`（`RetentionEligible` がrollback windowと参照有無を要求） |
| L167 | external timeoutを成功または未実行と推測しない | UNREACHABLE | `internal/application/outbox.go:540`（`resolveObservation`）。dispatcherが非test構築0 |

## 10. `docs/architecture/firestore-store.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L249-252 | `internal/store/firestore` が `application.UnitOfWork` を `RunTransaction` に結び付け、正規状態をメモリへ複製せず、callback内で読んだdocumentを根拠にoptimistic version／revisionを検証する | IMPLEMENTED | `internal/store/firestore/store.go:262-345`、`:583` |
| L256-257 | 書き込みは `UnitOfWork` 内にstagedし、callback成功後にdeterministicなpath順でflushする | IMPLEMENTED | `internal/store/firestore/store.go:575-600` |
| L258-259 | callback errorやFirestore競合retryで staged event／outbox／idempotency／aggregate をまとめて破棄する | IMPLEMENTED | `internal/store/firestore/store.go:575-600`（flushはcallback成功後のみ） |
| L260-261 | authority timeはcallback前に一度だけ取得し `AuthorityContext` から渡す。retryごとにclockやID generatorを呼ばない | IMPLEMENTED | `internal/store/firestore/store.go` の `AuthorityContext` 経路、`internal/application/service.go:128` |
| L261 | 1 callbackの書き込み上限をFirestoreより小さい400 documentとする | IMPLEMENTED | `internal/store/firestore/store.go:29`（`const MaxWrites = 400`） |
| L264-269 | 各documentが `record_schema=v1`／`kind`／JSON `payload` のenvelopeを持ち、未知schema・kind不一致・壊れたpayloadを `ErrInvalidSchema` としてfail closedする | IMPLEMENTED | `internal/store/firestore/store.go:208-222,231,264` |
| L268-269 | identifierをUTF-8かつ512 bytes以下のURL-safe base64単一path componentへ変換する | IMPLEMENTED | `internal/store/firestore/store.go:32`（`const MaxPathKeyBytes = 512`）、`:200` |
| L269-270 | Event、Outbox、Idempotencyをcreate-onlyとしID衝突を上書きで隠さない | IMPLEMENTED | `internal/store/firestore/store.go:583`（`u.tx.Create`） |
| L271-273 | Requirement本文をRequirement documentの `text` field と同じpayload wrapperへco-locateし、`SaveRequirement` と `SaveRequirementText` がstaged documentをmergeするためN+1 readや別collectionの整合性問題を生まない | IMPLEMENTED | `internal/store/firestore/store.go:271` 近傍のtext co-location |
| L275-277 | `installations/<encoded-installation>/<collection>/<encoded-id>` をtenant境界とし、browserからFirestoreを直接読ませない | IMPLEMENTED | `internal/store/firestore/store.go:200,405`。`internal/web/owner.js` はControl Plane APIのみ叩く |
| L276-277 | `firebase.json` と `scripts/firestore-emulator.sh` はローカル検証専用で、実GCP接続やcredentialをテストから行わない | IMPLEMENTED | `firebase.json` が存在し、`internal/store/firestore/client.go` の production constructor がemulator hostを拒否する（`cmd/control-plane/main.go:138-146` がその事実を記録） |
| L281 | M2の汎用queryも1000 documentsでfail closedする | IMPLEMENTED | `internal/store/firestore/store.go:30`（`const MaxQueryRows = 1000`）、`:756` |
| L281-285 | 運用hot pathは公開projectionとindexへ移行済み。期限切れleaseはstatus/expiry、active leaseはstatus、control検証候補はverification、実行とoutbox照合はlease IDで直接queryし、各境界は100件（安全上限確認だけlimit+1）に制限する。全件scanへfallbackしない | IMPLEMENTED | `internal/store/firestore/store.go:982`（expired lease）、`:1018`（active lease、`lease_status`）、`:1066`（`control_verification`）、`:1041,1087`（`index_lease_id`）。`infra/indexes.tf:1,20` と `firestore.indexes.json` にprojectionがある |
| L285 | 未コミットstaged valueを含むaggregate内検索は同じpredicateを通しread-your-writesを維持する | IMPLEMENTED | `internal/store/firestore/store.go:1499` 近傍の同一predicate適用 |
| L285 | index追加は `firestore.indexes.json`、emulator、read quota reservationを同じ変更で更新する | IMPLEMENTED | `firestore.indexes.json` と `infra/indexes.tf` が併存し、`internal/quota/quota.go:50-56` が bounded query の read 予約を宣言する |
| L287-288 | emulator integration testがrollback、aggregate/event/outbox/idempotencyのatomicity、codec corruptionを検証し、`FIRESTORE_EMULATOR_HOST` がない環境ではskipしてproduction endpointへ接続しない | IMPLEMENTED | `internal/store/firestore/store_test.go`、`internal/store/firestore/envelope_test.go`、`internal/store/firestore/quota_integration_test.go` |
| L292-293 | Outboxが配送中状態に加え `ambiguous`／`reconciling`／`confirmed`／`not-observed`／`superseded`／`needs-input` を持つ | IMPLEMENTED | `internal/application/outbox.go` の状態定義（`OutboxObservation` 系、`:540`） |
| L294-297 | Dispatcherがtransactionで一件ずつclaimし短いdelivery leaseを記録してからtransaction外で `EffectSink` を呼ぶ。effect直前に別transactionでoutbox所有権・最新control revision・active lease・fencing tokenを読み直すため、停止後や古いRunnerのintentは送信されない。transaction callback内で外部I/Oを行わない | UNREACHABLE | `internal/application/outbox.go:332`（`claim`）、`:365`（`beforeEffect`）、`:454`（`deliver`）。dispatcherの非test構築が0（`internal/application/outbox.go:178,199`） |
| L299-301 | `control-changed` だけはlease/fenceを持たない制御伝播用outboxとして許可し、対象scopeの最新revisionとの一致を検証する。それ以外は `ControlTarget`／`PermitKind`／Increment／Lease／fencing tokenが揃わない限りmalformedとして `dead` に収束する | UNREACHABLE | `internal/application/outbox.go:365-452` |
| L303-307 | sinkの失敗をbounded exponential backoffとjitterで `waiting` へ戻し、上限到達時だけ `dead` とする。配送のidempotency keyをOperation IDで固定する。ack前にdispatcherが落ちた場合は盲目的に再送せず同じOperation IDをobserverで照合し、confirmedなら確定、未観測なら再送、観測不能ならneeds-inputとして隔離する | UNREACHABLE | `internal/application/outbox.go:92-148`（`RetryPolicy`／`delay`）、`:479`（`fail`）、`:540`（`resolveObservation`） |
| L306-307 | `firestore.indexes.json` のoutbox projectionがcandidate queryのstatus／next-attempt順序を保つ | IMPLEMENTED | `firestore.indexes.json` と `infra/indexes.tf:1`（`outbox_delivery`） |

## 11. `docs/architecture/orchestration.md`

本文書はv2構築作業そのもののorchestrationを定めるため、主張の多くはagent運用規約である。
codeで真偽を判定できる行だけを採録した。

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L170-171 | Codexのcustom subagentはbootstrap実装手段として使うが、task stateの正本にしない | IMPLEMENTED | 正本は `.agents/v2/task-state/` のJSONで、`internal/contracts/validator.go:32`（`canonical-task-state`）が形式を検証する。`.codex/agents/` にstateは無い |
| L175 | 永続化されたtask stateとwork packetを役割間の契約にする | 混在 | task stateはIMPLEMENTED（上行）。work packetはABSENT: `.agents/v2/work-packets/` が存在しない（`ls .agents/v2` → `DEFERRED.md HANDOFF.md README.md evidence historical packets provider-preflight task-state`）。`contracts/schemas/work-order.json` と `design-packet.json` は存在し `internal/contracts/validator.go` から検証される |
| L198-206 | Lunaに渡す前にpacketが6項目を満たすことをschema validationする | IMPLEMENTED | `contracts/schemas/work-order.json` と `contracts/schemas/design-packet.json`、検証入口 `internal/contracts/validator.go:32` 近傍と `internal/contracts/packets_test.go` |
| L207 | 不足packetをLunaに補完させない | ABSENT | packet不足を機械的に拒否するgateがcodeに無い（`grep -rn --include='*.go' 'work-order' internal \| grep -v _test.go` → 0件。schemaはfixture testからのみ参照される） |
| L211 | write-heavyなtaskは同一component ownerを同時に一つに制限する | ABSENT | component owner排他を強制するcodeが無い。`ci/components.json` は所有関係を宣言するが、同時writerの制限は行わない（`internal/ci/manifest_dependencies.go:405-460` は被覆・辺・検証入口の3検査のみ） |
| L215-223 | `.agents/v2/` 配下に `backlog/`／`tasks/`／`work-packets/`／`decisions/`／`evidence/`／`releases/` を置く | ABSENT | 実在するのは `evidence/`／`task-state/`／`packets/`／`provider-preflight/`／`historical/`。宣言された6 directoryのうち存在するのは `evidence/` のみ（`ls .agents/v2`） |
| L225 | 各状態遷移がactor、入力hash、output hash、次のowner、retry budgetを記録し、prompt全文や秘密情報を保存しない | IMPLEMENTED | `contracts/schemas/task-state.json` と `contracts/schemas/task-state-transition.json`／`task-state-retry.json`。`internal/contracts/validator.go:32` が検証する |
| L225 | 進行中agentのleaseが失効すれば同じpacketから別agentが再開する | ABSENT | agent leaseを表す機構がcodeに無い（`grep -rn --include='*.go' 'AgentLease' internal \| grep -v _test.go` → 0件） |
| L237 | M2でControl Planeのtask stateが成立した後は、同じpacket契約を製品schedulerから起動する | UNREACHABLE | 製品scheduler側にpacketを読む経路が無い（`internal/scheduler/` に `.agents` 参照が無い）。`internal/application/allocation.go:625` はRequirementのみを入力に取る |
| L240 | Lunaの出力をcoordinatorがWork Orderのscope、diff、affected validation Evidenceに一致することを機械検査してから統合する | 混在 | affected validationはIMPLEMENTED（`internal/ci/planner.go`、`cmd/ci-plan`、`Makefile:14` の `affected`）。Work Order scopeとdiffの一致検査はABSENT |
| L244 | 各agentの終了前に結果をcanonical task stateとEvidence indexへcheckpointする | IMPLEMENTED | `contracts/schemas/evidence-index.json` と `.agents/v2/evidence/`、`internal/contracts/validator.go` の台帳test |

## 12. `docs/architecture/release-contract.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L193-195 | 各Repositoryがversion管理されたRelease Contractを持ち、Stable昇格に必要なcapabilityと実証方法の唯一の一覧とする | 混在 | Foundation自身の分はIMPLEMENTED（`contracts/release-contract/foundation.json`、`foundation-capabilities.json`、schema `contracts/schemas/release-contract.json`）。Repositoryごとに持つ機構はABSENT（`domain.Repository` にRelease Contract version fieldが無い、§8 L36-47） |
| L197-204 | 各capabilityが7項目（利用者操作、観測すべき結果、Preview実環境での実行方法、必要な外部system／Provider／credential scope、成功・失敗・rollbackの観測条件、証跡の保存先と有効期間、対応するStable／Preview文書）を宣言する | IMPLEMENTED | `contracts/release-contract/foundation-capabilities.json:9` 以降の各capability objectが `user_action`／`observable_result`／`preview_exercise`／`external_dependencies`／`outcome_conditions`／`evidence_retention`／`documentation` を持つ |
| L206 | 内部functionやtest caseの一覧をcapability一覧の代わりにしない | IMPLEMENTED | 同ファイルのcapabilityはすべて利用者操作語彙で書かれ、test名を含まない |
| L210-213 | Stable候補はRelease Contractの全capabilityを候補versionが動くPreview実環境で確認し、変更されたものだけでなく全capabilityを対象にする | UNREACHABLE | `internal/release/promotion_report.go:305` が全capabilityの条件を組み立てるが、読み口 `GET /v1/release/state` はobserver未装着で503（§4）。exerciseを実行するcodeは存在しない（`grep -rn --include='*.go' 'CapabilityExercise\|RunExercise' internal \| grep -v _test.go` → 0件） |
| L214 | fake、stub、契約testは早期feedbackに使うが実稼働確認を置き換えない | IMPLEMENTED | `internal/application/release_surface.go:104-105,137-139`（環境classの明示宣言を必須にし、未宣言をconstructorが拒否） |
| L215 | 外部system依存機能は対象systemへ実際に接続する | UNREACHABLE | 接続主体（runner）が不在（§4）。`internal/runner/forge_live_test.go`／`git_live_test.go`／`provider_live_test.go` はtestのみ |
| L215 | Provider非依存機能はRelease Contractの代表Providerによる実行を確認する。代表providerはcapability declaration setの `representative_provider` で宣言する | IMPLEMENTED（宣言として） | `contracts/release-contract/foundation-capabilities.json:8`（`"representative_provider": "claude"`）、schema `contracts/schemas/capability-declaration-set.json:40` |
| L216-218 | Provider依存機能は影響する各Providerを実際に利用する。Codex依存はCodex、Claude依存はClaude、opencode依存はopencodeで確認する。共通adapter／handoff契約の変更は3 Providerすべてで確認する | DEFERRED-BY-DECLARATION | 宣言自身がcodex／opencodeを名指す。この機械では両者が未認証（測定 2026-08-26）。claude分の実証記録は `.agents/v2/provider-preflight/V2-017-provider-live-claude.json` ほか5件に存在する |
| L219 | 実行不能なcapabilityが一つでもあれば理由にかかわらずStableへ昇格しない | IMPLEMENTED（機構として） | `internal/release/promotion_report.go:225`（`Promotable`）。到達性は L210-213 |
| L221-222 | 費用やquotaを理由に確認を省略せず、上限内で実証できない候補はPreviewに留める | ABSENT | 費用上限とcapability実証回数を結びつけるcodeが無い（`internal/quota/quota.go` はFirestore read/writeのみを数える） |
| L226-231 | Preview実環境を二等級（`preview-local`／`preview-gcp`）で宣言し、4点（IAP認証境界、scale-to-zero、実Firestoreの権限と競合、deploy経路）は`preview-local`では実証できずD1で `preview-gcp` により実証する | DEFERRED-BY-DECLARATION | 宣言自身がCloud Run／IAP／実Firestoreを名指す。`infra/main.tf:66-82,139-158`（IAPとscale-to-zero設定）はplan段階まで。環境class自体は `internal/application/release_surface.go:104-105` がcodeに存在する |
| L231 | D1未通過の間、Stableは提供環境をowner実機上のself-hostとして宣言し、GCP上での運用をcapabilityとして主張しない | IMPLEMENTED | `contracts/release-contract/foundation-capabilities.json` のcapabilityにGCP運用を主張するものが無く、`cmd/control-plane/main.go:138-146` がpreview-local経路を明示する |
| L232 | capability evidenceは環境class、machine識別子、emulator名とversion、関与した実外部systemの識別子を必ず記録する | IMPLEMENTED | `contracts/schemas/evidence.json` と `internal/contracts/capability_evidence_test.go`、`internal/release/promotion_report.go:305`（`EnvironmentClass` を入力に取る） |
| L236-245 | Stable自動昇格の8条件 | UNREACHABLE | `internal/release/promotion_report.go:74`（`AllConditionIDs`）が8条件を持ち `:305` が評価するが、読み口が503で、`internal/release/pipeline.go:38` に非test callerが0（§4） |
| L247 | 昇格後にRequirementを完了へ移し、対応するStable文書を既定表示にする | UNREACHABLE | `internal/domain/model.go:526` に非test callerが0。文書routingはABSENT（§6 L253-259） |
| L251-254 | Stable文書は現在のStableを初めて使う利用者が理解できる完全な文書とし、過去の変更履歴だけで現在仕様を説明しない | IMPLEMENTED（検査として） | `internal/release/docs.go:197`（`VerifyRequiredSections`）、`:222`（`VerifyNoStableToPreviewLinks`）。到達性は `BuildPromotionReport` 経由のみ（`internal/release/promotion_report.go:385`） |
| L256-264 | Preview文書は単独で利用できる完全な文書とし、Preview version／差分／既知問題／Stableへ戻す方法／不足実証を明示する | IMPLEMENTED（検査として） | `internal/release/docs.go:167`（`VerifyPreviewReleaseMarker`）、`:181`（`VerifyStableReleaseMarker`）、`:129`（`VerifyCapabilityAnchorBijection`） |
| L268-272 | 文書のrelease 5規則（同じPreview versionの文書も変更、driftをPreview gateで検出、昇格時に統合、rollback対象versionの文書保持、同じ事実の手作業複製を避ける） | 混在 | drift検出はIMPLEMENTED（`internal/release/docs.go:93,129,222` と `internal/release/docs_test.go`）。昇格時統合とrollback版文書保持はABSENT（文書bundleを扱うcodeが無い） |
| L276-280 | Version routing 5項目（Preview routingの単位はRepository、Repositoryは一度に一つのLoop channel、canonical state共有と全eventへのversion記録、跨ぎIncrementの条件、同じrelease versionの付与） | 混在 | UNREACHABLE: routing（`internal/release/release.go:120`）。ABSENT: 全eventへの実行version記録（`internal/application/service.go:1773` の `record` にversion fieldが無い） |
| L284-287 | 秘密の扱い4項目（credentialをfixture／証跡／文書／canonical stateへ保存しない、証跡にはprovider発行の不透明な実行識別子までとしpromptやraw応答を既定保存しない、最小scopeのcredentialを実行時に渡す、leak検知時は昇格を止めて失効・交換し再実証する） | 混在 | IMPLEMENTED: 証跡schemaにprompt／応答fieldが無い（`contracts/schemas/evidence.json`、`contracts/schemas/provider-preflight.json`）、canonical stateにcredentialが無い（§9 L164）。UNREACHABLE: 最小scope注入（`internal/runner/secret_broker.go:149`）。ABSENT: leak検知時の失効・交換・再実証（§9 L125-135） |

## 13. `docs/architecture/technology.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L11 | Control Plane runtimeはCloud Run、request-based billing、min instances 0 | DEFERRED-BY-DECLARATION | 宣言自身がCloud Runを名指す。`infra/main.tf:66,78-82`（`min_instance_count = 0`）はplan段階まで（applyはD1） |
| L12 | Canonical storeはCloud Firestore Standard、default database | IMPLEMENTED | `internal/store/firestore/client.go`、`infra/main.tf:46` |
| L13 | 定期reconciliationはCloud Scheduler 1 job → Control Planeのbounded tick | 混在 | bounded tickはIMPLEMENTED（`internal/api/api.go:99`、`cmd/control-plane/main.go:199-207`、5秒／100件）。Cloud Scheduler jobはDEFERRED-BY-DECLARATION（`infra/main.tf:160-174`、applyはD1） |
| L14 | Owner／Runner入口認証はCloud Run direct IAP | ABSENT | `internal/api/auth.go:102` が読むのは `X-Goog-Authenticated-User-Email` header文字列のみ。IAP assertionを検証するcodeが無い（下行） |
| L185-186 | Control Planeも `X-Goog-IAP-JWT-Assertion` のsignature、issuer、audience、subjectを検証する | ABSENT | `grep -rn 'IAP-JWT-Assertion\|jwt\|JWT' internal cmd --include='*.go' \| grep -v _test.go` → 0件。`grep -n 'jwt\|jose' go.mod go.sum` → 0件。Cloud Runを必要としないcode側の検証が存在しない（D1が担うのは境界のlive実証であり、検証器の実装ではない） |
| L15 | Control Plane secretはGoogle Secret Manager。Provider credentialは保存しない | 混在 | Provider credentialを保存しないのはIMPLEMENTED（§7 L171）。Secret Managerの利用はABSENT: `grep -rn 'secret_manager' infra/*.tf` → 0件、`grep -rn 'secretmanager' --include='*.go' internal cmd` → 0件 |
| L16 | Container／release imageはGoogle Artifact Registry | ABSENT | `grep -rn 'artifact_registry' infra/*.tf` → 0件。`Containerfile` は存在するがregistry宣言が無い |
| L17 | Control Plane／Runner言語はGo、同一module内の複数command | IMPLEMENTED | `go.mod`、`cmd/control-plane`／`cmd/runner`／`cmd/bootstrap`／`cmd/ci-plan`／`cmd/legacy-import` |
| L18 | UIはGo server-rendered HTML＋embedded CSS／vanilla ES modules | WIDER-THAN-CODE | `internal/web/web.go:7` はHTML／CSS／JSをembedするが、`OwnerHTML()` は静的fileをそのまま返す（`:10`）だけでtemplate renderingではない。`html/template` の使用が無い（`grep -rn 'html/template' internal --include='*.go'` → 0件） |
| L19 | Runner配布はCloud Run release imageに同梱した署名済みLinux binary | 混在 | 署名検証はIMPLEMENTED（`internal/update/update.go:127`）。imageへの同梱はABSENT（`Containerfile` にrunner binaryを置くstageが無く、Artifact Registryも無い） |
| L20 | Runner local stateはfsync付きJSONL journal（durable append、fsync後ack、idempotent replay、partial-tail耐性、corruption拒否、bounded size、canonicalではない） | UNREACHABLE | `internal/runner/journal.go:38`（`OpenJournal`）に非test callerが0。保証自体は `internal/runner/journal_test.go:10,36` と `internal/runner/crash_test.go:437` で実測されている |
| L21 | Runner sandboxはLinux namespace sandbox＋Increment専用clone／workspace | 混在 | namespace sandboxはIMPLEMENTED（`internal/runner/confinement.go:114`、`cmd/bootstrap/main.go:133,138` から到達）。Increment専用clone／workspaceはUNREACHABLE（`internal/runner/workspace.go:11`） |
| L22 | Infrastructure as CodeはOpenTofu＋Google provider、version／provider lock固定 | IMPLEMENTED | `infra/versions.tf`、`scripts/infra-validate.sh`（`Makefile:120`） |
| L23 | Repository environmentはRepository Contractが宣言する既存の固定環境。Foundation自身はDevboxを継続 | 混在 | DevboxはIMPLEMENTED（`devbox.json`、`devbox.lock`）。Repository ContractはABSENT（§6 L72-81） |
| L30-37 | 候補比較表と「Cloud SQLは常設無料instanceを前提にできないため不採用」 | IMPLEMENTED | 採用結果がcodeに反映されている: Firestoreのみ（`internal/store/firestore`）、SQL driverのdependencyが無い（`go.mod`） |
| L45-49 | Cloud Run採用理由5項目（scale-to-zero、request-based billing、immutable revision／traffic split／rollback、IAMで最小権限接続、同じIaCで利用者projectへdeploy） | DEFERRED-BY-DECLARATION | 宣言自身がCloud Runを名指す。`infra/main.tf:66-138`（service、`:122` traffic）はplan段階まで |
| L50-51 | canonical state、session、Outbox、artifactをlocal filesystemへ保存しない | IMPLEMENTED | `internal/store/firestore/store.go` のみが永続化を担い、`internal/api` にfile書き込みが無い |
| L55 | request-based billing | DEFERRED-BY-DECLARATION | Cloud Run設定（`infra/main.tf:66-82`） |
| L56 | min instances `0` | IMPLEMENTED（宣言として） | `infra/main.tf:81`（`min_instance_count = 0`） |
| L57 | service-level max instances 初期値 `2` | WIDER-THAN-CODE | `infra/main.tf:82` は `max_instance_count = 1`。文書は実際より広い上限を宣言している |
| L58 | concurrency 初期値 `20` | WIDER-THAN-CODE | `infra/main.tf:78` は `max_instance_request_concurrency = 1` |
| L59 | request timeoutはUI/APIを短く、reconcile tickもbounded batchで終了する | IMPLEMENTED | `cmd/control-plane/main.go:200`（5秒）、`internal/reconciler/reconciler.go:16`（100件） |
| L60 | regionはFirestore、Artifact Registryと同一region | 混在 | Firestore／Cloud Runのregion変数はある（`infra/variables.tf`）。Artifact RegistryはABSENT（L16） |
| L61 | direct IAPを有効化しownerだけへ `roles/iap.httpsResourceAccessor` を付与する | DEFERRED-BY-DECLARATION | `infra/main.tf:71`（`iap_enabled = true`）、`:149-158`。applyはD1 |
| L62 | preview revisionへtag URLを付けStable trafficと分ける | ABSENT | `infra/main.tf:122` のtraffic blockにtag指定が無い（`grep -rn 'tag' infra/main.tf` の結果にrevision tagが現れない） |
| L64-70 | max instancesをcost／contention抑制のhard guardとして設定し、preview tagにrevision-level min instancesは設定しない | IMPLEMENTED | `infra/main.tf:81-82`（revision-level minimumの指定が無い） |
| L74-79 | Stable／Preview: 一つのserviceに両revisionを置き、通常URLとtag URLで分け、両revisionが同じFirestore stateをschema互換で読み、Repository routingとLoop Release manifestがRunner binary／moduleを決め、Preview実証後にtraffic targetを切り替え、rollbackは旧revisionへ戻す | 混在 | schema互換読みはIMPLEMENTED（`internal/store/firestore/store.go:208-222` のenvelope）。tag URLはABSENT（L62）。Repository routingはUNREACHABLE（`internal/release/release.go:120`）。traffic切替とrollbackはDEFERRED-BY-DECLARATION（Cloud Run） |
| L87-92 | Firestore採用理由5項目（idle費用なし、複数documentのatomic transaction、競合時の再実行と部分write無し、strongly consistent read、無料枠の十分性） | IMPLEMENTED | `internal/store/firestore/store.go:575-600`（callback成功後flush） |
| L99-102 | 無料枠の数値（1 GiB、50,000 reads、20,000 writes、20,000 deletes、月10 GiB egress）と、TTL delete／PITR／backup／restore／cloneが無料に含まれないこと | IMPLEMENTED（宣言との整合） | `internal/quota/quota.go:46`（`DefaultBudget{25000,10000,10000}`）がその50%として設定されている |
| L104-106 | 無料枠をSLAとみなさず、application側に日次hard budgetを設ける。budget alertは強制停止ではないためread／write counterの見積りとhard stopを使う | IMPLEMENTED | `internal/quota/quota.go:81`（`Reserve` がover budgetでerror）、`internal/store/firestore/store.go:457`、`internal/api/api.go:1196-1203`（`quota.ErrOverBudget` → 429） |
| L110-127 | Data layout 15 collection | WIDER-THAN-CODE | 実在するcollectionは `internal/store/firestore/store.go` が書くものに限られる。`providerAccounts`／`providerRuns`／`releaseCandidates`／`releases`／`artifacts`／`capabilityEvidence`／`questions` に対応する書き込みが無い（型自体がABSENT、§8） |
| L130-131 | 巨大aggregateを1 documentへ埋め込まない。Requirement current summaryは1 documentに置き、Event／Execution／Evidenceを別collectionへ分ける | IMPLEMENTED | `internal/store/firestore/store.go:200`（collection分離）、`:271`（summary co-location） |
| L135-138 | Transaction boundaries 4種（Increment claim、Control Intent、Result accept、Release promotion decision） | 混在 | IMPLEMENTED: Increment claim（`internal/application/service.go:1132`）、Control Intent（`:1372,1502`）、Result accept（`:1577`）。UNREACHABLE: Release promotion decision（`internal/release/pipeline.go:38`） |
| L140 | transaction内で外部I/O、ID生成、log送信をしない | IMPLEMENTED | `internal/store/firestore/store.go` のAuthorityContext（callback前に1度だけ取得）、`internal/application/service.go:128` |
| L144-148 | Hotspot対策5項目 | 混在 | IMPLEMENTED: Runner／Lease／Executionごとのdocument分散（`internal/store/firestore/store.go:200`）、random／time-random複合ID（`cmd/control-plane/main.go:30-36`）、tickごとの固定上限（`internal/reconciler/reconciler.go:16`）、候補query後の再読（`internal/application/service.go:1132`）。IMPLEMENTED（不作為として）: Installation counterのheartbeatごと更新をしない（`internal/application/service.go:474` にcounter更新が無い） |
| L155-157 | managed backup／PITRを使わず、Control Planeが日次の整合したlogical exportをownerが選んだRunnerへstreamし、Runnerが `age` で暗号化してlocal bounded storageへ保存する。exportはsecret値とraw logを含まない | 混在 | IMPLEMENTED: logical exportのAPI（`internal/api/api.go:311`、`internal/application/readmodels.go:449`）、secret値を含まないこと（export recordにcredential fieldが無い）。ABSENT: 日次stream、`age` 暗号化、Runner local保存（`grep -rn --include='*.go' 'age -\|ageEncrypt\|filippo.io/age' internal cmd \| grep -v _test.go` → 0件） |
| L157 | restore rehearsalをPreview capabilityに含める | ABSENT | `contracts/release-contract/foundation-capabilities.json` にrestore rehearsalのcapabilityが無い（`grep -n 'restore' contracts/release-contract/*.json` → 0件） |
| L159 | 有料managed backupを有効化する場合は費用上限変更として人間承認を必要とする | ABSENT | 費用上限変更に承認を要求するcodeが無い |
| L163-172 | Cloud Scheduler 1 jobが `/internal/reconcile` をOIDC認証付きで呼び、5種（expired Lease、pending／ambiguous Outbox、Control verification、circuit half-open probe eligibility、retention／GC候補）をbounded batchで処理する。通常のclaimはRunner requestが直接起こし、Scheduler tickがqueueをpollしてworkerを起動しない | 混在 | IMPLEMENTED: endpointと認証境界（`internal/api/api.go:99`、`cmd/control-plane/main.go:191`（`ReconcileIdentity`））、expired Lease、Control verification（`:202,205`）、tickがworkerを起動しないこと。UNREACHABLE: pending／ambiguous Outbox（`internal/application/outbox.go:328`）、circuit half-open probe（`internal/provider/breaker.go:444`）、retention／GC（`internal/update/retention.go:206`） |
| L176-177 | Cloud Tasks、Pub/Sub、Workflowを初期版で導入しない | IMPLEMENTED | `go.mod` に該当client libraryが無い |
| L183-186 | Owner認証4項目（IAP直接有効化、owner identityだけへaccess付与、assertion検証、Security Rulesをclient denyにする） | 混在 | IMPLEMENTED: owner allowlist（`cmd/control-plane/main.go:45-58`）。ABSENT: assertion検証（L185-186）、Firestore Security Rules（`firebase.json` はemulator portのみを宣言し、`firestore.rules` fileが存在しない） |
| L192-200 | Runner認証9段（IAP用custom OAuth clientとdesktop clientをIaC／bootstrapで作成しallowlistへ登録、ownerが各Runnerでdesktop OAuth flowを一度実行しrefresh tokenをOS keyringへ、RunnerがIAP audienceの短命ID tokenを取得、一回限りの短命enrollment tokenをownerがUIで発行、RunnerがEd25519 key pairを生成しprivate keyをkeyringへ、Control Planeはpublic keyだけを保存、challenge signature確認後に短命session tokenを発行、Runner削除／emergency stopでkeyをrevokedにする） | 混在 | IMPLEMENTED: enrollment token発行と一回限り（`internal/runner/protocol.go:71`、`ErrReplay`）、Ed25519 challenge／signature（`:87,110`）、public keyのみ保存、短命session token（`:110,138`、`TokenTTL`）。ABSENT: OAuth clientのIaC作成（`grep -rn 'oauth' infra/*.tf` → 0件）、OS keyringへの保存、IAP audience ID token取得、emergency stopでのenrollment key revoke（`grep -n 'Revoke' internal/runner/protocol.go` → 0件。`internal/runner/secret_broker.go:171` の `Revoke` はsecret grant用で、しかも非test到達不能） |
| L205-206 | IAP identityを入口認証、Ed25519 keyを個々のRunner認証として二層で使う。OAuth refresh tokenもRunner外へ送らない | 混在 | 二層のうちEd25519層はIMPLEMENTED（`internal/api/auth.go` のRunner session header分離、`internal/runner/protocol.go:138`）。IAP層はABSENT（L185-186） |
| L209-213 | Secret Managerにsession signing key、export signing keyなど少数のcontrol secretだけを置く | ABSENT | Secret Manager利用が無い（L15）。session signing keyもcodeに無い。`grep -rn --include='*.go' 'SigningKey' internal cmd \| grep -v _test.go` の3件はいずれも `internal/update/update.go:83,147,149` のmodule manifest署名key idであり、Control Planeのsession署名鍵ではない |
| L215-216 | secret accessをrequestごとに行わずinstance memoryへ短時間cacheする。log、Firestore、Artifactへ値を出さない | 混在 | 値を出さないことはIMPLEMENTED（§9 L164）。cacheはABSENT（Secret Managerが無いため） |
| L219-226 | Artifact Registry／binary updateの6項目 | 混在 | IMPLEMENTED: 同一source revisionからのreproducible build（`Makefile`）、release manifestへのSHA-256とEd25519 signature（`internal/update/update.go:127`）、独立Bootstrapperによる検証とversioned directory＋atomic symlink切替（`cmd/bootstrap/main.go:58-95`、`internal/update/update.go:296`）、Bootstrapperを自動更新対象外にすること（`cmd/bootstrap/main.go:1-9`）。ABSENT: Artifact Registry保存、`amd64`／`arm64` の両build target、Runner binary／module manifest／docsのcontainer image同梱（`Containerfile:7` がbuildするのは `./cmd/control-plane` だけ） |
| L227-233 | Artifact Registryのretentionを絞り、Runner downloadをControl Plane経由にしてlocal machineへ `gcloud` を必須にしない。download egressをBudgetへ計上する | ABSENT | Runner binary配布endpointが無い（`internal/api/api.go` にdownload routeが無い）。egress計上も無い |
| L236-242 | Goを選ぶ理由5項目 | IMPLEMENTED | `go.mod`、単一module内の5 command、`internal/domain` の型共有 |
| L246-262 | 構成図が宣言するdirectory（`internal/provider/{codex,claude,opencode}`、`internal/adapter/{git,deploy,notify}`、`web/`） | ABSENT | `internal/provider/codex`／`claude`／`opencode`、`internal/adapter`、`web/` のいずれも存在しない（`ls internal`、`ls`）。3 adapterは `internal/provider/adapters.go:14-16` に1 fileで同居し、UIは `internal/web/` にある |
| L264 | Go toolchain、module dependency、provider CLI、OpenTofu providerをlock fileで固定する | 混在 | IMPLEMENTED: `go.sum`、`devbox.lock`、`infra/versions.tf`。ABSENT: provider CLIのversion固定（`internal/provider/compatibility.go:172` は区間宣言であってlockではない） |
| L268-274 | UI: SPA frameworkを使わず `html/template` でserver-renderし、必要な部分だけvanilla ES modulesで更新する。build toolchain削減、docs embed、URLごとの直接open、JS失敗時のreadとemergency stop導線維持 | 混在 | IMPLEMENTED: SPA frameworkなし、vanilla JS（`internal/web/owner.js`）、build toolchainなし。ABSENT: `html/template` によるserver-render（L18）、Stable／Preview docsのembed（`internal/web/web.go:7` はowner 3 fileのみ）、URLごとに直接開けること（route は `/owner` 一枚、`internal/api/api.go:159`）、JS失敗時のread導線（すべての表示が `fetch` 依存、`internal/web/owner.js:1`） |
| L276-277 | 進捗更新はETag付きbounded pollingとし、browserからFirestore realtime listenerを使わない | 混在 | realtime listenerを使わないのはIMPLEMENTED。ETagはABSENT（`grep -rn 'ETag\|If-None-Match' internal/api internal/web \| grep -v _test.go` → 0件） |
| L280-291 | Runner local implementation 9項目 | 混在 | IMPLEMENTED: Linuxのみ対応（`internal/runner/confinement.go:114` がLinux前提）、namespace sandbox（`cmd/bootstrap` から到達）。UNREACHABLE: journal（L20）、Increment専用clone（`internal/runner/git.go:138`）、process group＋cgroup制御（`internal/runner/supervisor.go:73`）。ABSENT: local bare mirror cache、cgroup resource limit（`grep -rn --include='*.go' 'cgroup' internal \| grep -v _test.go` → 0件） |
| L282-286 | Runner local durable storeの6保証と「この節が固定するのは保証であってengineではない」 | UNREACHABLE | 保証はtestで実測済み（`internal/runner/journal_test.go:10,36`、`internal/runner/crash_test.go:437`）。稼働経路が無い（L20） |
| L293-294 | 最初からmacOS／Windows Runnerを約束しない | IMPLEMENTED | Linux以外のsandbox adapterが無い |
| L298-309 | OpenTofuが宣言する10項目 | 混在 | IMPLEMENTED: API enablement（`infra/main.tf:17`）、Firestore databaseとindex（`:46`、`infra/indexes.tf`）、Cloud Run service（`:66`）、Cloud Scheduler job（`:160`）、service accountsと最小IAM（`:24,31,60`）、direct IAPとowner IAM（`:71,149`）。ABSENT: Stable／Preview revision設定（L62）、Artifact Registry、Secret Manager secret metadata、programmatic OAuth client／allowlist、log retention／budget alert／hard budget application設定（`grep -rn 'artifact_registry\|secret_manager\|billing_budget\|logging_project\|oauth' infra/*.tf` → 0件） |
| L310-311 | 適用不能なOAuth consent／client操作に検証commandと明示的one-time bootstrap stepを用意し、desired stateとdrift検出を文書化する。`apply`、`plan`、`verify`、`destroy` を共通commandから実行する | 混在 | IMPLEMENTED: `Makefile:114,117,120`（`infra-policy`／`infra-lint`／`infra-validate`）と `scripts/infra-plan.sh`。ABSENT: `apply`／`destroy` の共通command、OAuth用one-time step |
| L315-324 | Cost guard 9項目 | 混在 | IMPLEMENTED: Cloud Run min 0（`infra/main.tf:81`）、Firestore daily budgetを無料quotaの50%以下（`internal/quota/quota.go:46`）、Scheduler 1 job（`infra/main.tf:160`）、Cloud Loggingへraw logを送らない（log送信codeが無い）。WIDER-THAN-CODE: max 2（実際は1、L57）。ABSENT: Secret Manager active versions／access cache、Artifact Registry 0.4 GiB soft limit、Provider実証回数の有限化、予測budget超過時の `needs-input` 停止（`grep -rn --include='*.go' 'ForecastBudget\|predictedBudget' internal \| grep -v _test.go` → 0件） |
| L326-327 | 無料quotaをIaCに埋めて安全を推測せず、deploy時のcost preflightで公式条件とbilling設定を再確認する。budget alertはhard stopではない | 混在 | cost preflightの機構はIMPLEMENTED（`internal/contracts/deployment_preflight.go:108`、`contracts/schemas/deployment-preflight.json`）。実行はDEFERRED-BY-DECLARATION（deploy時＝D1） |
| L331-337 | Rejected technologies 7件 | IMPLEMENTED | いずれもcodeに存在しない: PostgreSQL／Cloud SQL driver、SQLite、Kubernetes manifest、Cloud Tasks／Pub/Sub／Workflow client、Node SPA（`package.json` が無い）、dynamic Go plugin（§7 L214）、browser直Firestore（`internal/web/owner.js` はAPIのみ） |

## 14. `docs/architecture/validation.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L7-18 | 品質保証modelの6層合算がStable promotion evidenceになる。自動testとPreview実証はどちらか一方で代替しない | UNREACHABLE | `internal/release/promotion_report.go:305` が層ごとの条件を持つが、`GET /v1/release/state` はobserver未装着で503（§4）。`make candidate`（`Makefile:24`）は決定的test層のevidenceのみを検査する |
| L24 | Domain state testはdatabase、filesystem、network、real clockを使わない | IMPLEMENTED | `internal/domain/` にそれらのimportが無く、`internal/domain/model.go:456-462` が時刻をactorから受け取る |
| L26-33 | Requirement／Increment／Execution／Release／Controlの全許可遷移、禁止遷移とinvariant、priority comparisonと飢餓防止、effective Control Policy合成、Retry Budget／circuit breaker、Lease expiry／fencing、promotion gate、retention eligibilityをtestする | IMPLEMENTED | `internal/domain/core_test.go`、`internal/domain/invariant_model_test.go`、`internal/domain/release_test.go`、`internal/scheduler/priority_test.go`、`internal/scheduler/starvation_test.go`、`internal/provider/breaker_test.go`、`internal/release/pipeline_test.go` |
| L35-41 | property-based／model-based testでcommand sequenceを生成し、5 invariant（expired tokenで前進しない、completed RequirementはStable Releaseを参照、stopped scopeで副作用permitが発行されない、capability evidence欠損でpromotableにならない、credential fieldがdomain schemaに存在しない）を常時確認する | IMPLEMENTED | `internal/domain/invariant_model_test.go`、`internal/application/stop_matrix_test.go:19`、`internal/release/contract_baseline_test.go` |
| L45-53 | Firestore emulatorでtransactionとqueryを実装と同じclient libraryで検証し、7項目（同時claim、callback再実行での重複なし、optimistic version conflict、Control revision競合、候補query後の再検証、Outboxのat-least-onceとidempotent completion、schema expand／coexist／migrate／contract）を確認する | 混在 | IMPLEMENTED: 前5項目（`internal/store/firestore/store_test.go`、`internal/store/firestore/envelope_test.go`）。UNREACHABLE: Outbox delivery（`internal/application/outbox_test.go` はmemory storeのみで、dispatcher自体が非test構築0）。UNREACHABLE: schema expand／coexist／migrate／contract（`internal/update/schema.go:98,156,209` に非test callerが0） |
| L55 | emulatorの成功を実Firestoreの代替にせず、実Firestoreのcontentionと権限はD1で確認する | DEFERRED-BY-DECLARATION | 宣言自身が実Firestore／D1を名指す。`internal/store/firestore/store_test.go` は `FIRESTORE_EMULATOR_HOST` なしでskipする |
| L59-66 | API contract test 8項目（owner／Runner認証とscope、idempotency key、Stable／Preview Runner API互換、request／response JSON Schema、error taxonomy、paginationとbounded response、XSS／CSRF／content-type／size limit、stop requested／effective／verifiedの表示） | 混在 | IMPLEMENTED: 認証とscope（`internal/api/auth_test.go`）、idempotency（`internal/application/service_test.go`）、schema（`internal/contracts/contracts_test.go`）、pagination（`internal/application/readmodels.go:199`、`contracts/openapi/openapi-v1.yaml:483` の `page_size` 上限100）、content-type／origin（`internal/api/api_test.go`）、stop段階表示（`internal/api/api.go:189`）。ABSENT: Stable／Preview Runner API互換test（`grep -rn --include='*_test.go' 'PreviewRunnerAPI\|api compatibility' internal/api` → 0件） |
| L68 | OpenAPIを契約の正本にし、server、Runner client、test fixture、reference docsを同じschemaから生成する | WIDER-THAN-CODE | `contracts/openapi/openapi-v1.yaml` は存在し `internal/contracts` が形式を検査するが、生成物は一つも無い（`generated/` directoryが存在せず、`grep -rn 'go:generate' internal cmd --include='*.go'` → 0件）。serverとclientは手書きである |
| L72-82 | Runner component test 8項目 | 混在 | IMPLEMENTED（testとして実在）: workspace path／symlink／permission（`internal/runner/working_directory_test.go`、`internal/runner/confinement_test.go`）、process group TERM／KILL（`internal/runner/stop_test.go`、`internal/runner/crash_test.go:437`）、journal reconciliation（`internal/runner/journal_test.go:10,36`）、bounded log／redaction（`internal/runner/log_test.go`）、Secret Broker境界（`internal/runner/secret_broker_test.go`）、sandbox境界（`internal/runner/confinement_test.go`）、atomic binary update／rollback（`internal/update/switch_test.go`）。ABSENT: offline時の新規claim／Result拒否test（`grep -rn --include='*_test.go' 'offline' internal/runner` → 0件） |
| L74-77 | daemon restartとlocal journal reconciliationを `TestJournalRecoveryAndIdempotentAppend`／`TestJournalRejectsCorruptCompleteLine`／`TestJournalSurvivesRealSIGKILLOfProcessGroup` が検証する | IMPLEMENTED | `internal/runner/journal_test.go:10`、`internal/runner/journal_test.go:36`、`internal/runner/crash_test.go:437` に同名testが実在する |
| L84 | Linux namespace、git、process signalは実kernel上のintegration testで実物を使う。workspace封じ込めはrootless user+mount namespaceで非特権のまま証明し、namespaceなしのpositive controlを残す。kernelが許可しない場合はcontainer／VMを代替とするがskipをpassとして数えない。gateはtestが実行されたverdictと実行環境識別子を要求する | 混在 | IMPLEMENTED: rootless namespaceの実装と対照（`internal/runner/confinement.go:114`、`internal/runner/confinement_test.go`）。ABSENT: skipをpassとして数えないことを強制するgate、実行環境識別子の要求（`grep -rn --include='*.go' 'KernelVersion\|uname' internal \| grep -v _test.go` → 0件） |
| L88-98 | Provider adapter contract testを3 providerそれぞれ9 caseで持つ | 混在 | IMPLEMENTED: fixture parseの9 case相当（`internal/provider/fixtures_test.go`、`internal/provider/adapters_test.go`、`internal/provider/provider_test.go`）。ただしcodex／opencodeのfixtureが実CLI由来であることはこの機械では確認できない（両者未認証、DEFERRED-BY-DECLARATION） |
| L100 | fixture更新には実CLI version、観測日時、変更理由、対応Preview exerciseを必要とする | ABSENT | fixture更新にmetadataを要求する機構が無い（`contracts/fixtures/` にversion／観測日時fieldが無い） |
| L104-116 | Journey test 10本を独立させる | 混在 | IMPLEMENTED（実在するもの）: (4) Runner消失→fencing→別Runner再開（`internal/reconciler/fencing_journey_test.go`）、(5) stop（`internal/reconciler/stop_journey_test.go`、`internal/runner/stop_test.go`）、(7)相当のrollback（`internal/scheduler/rollback_test.go`、`internal/release/journey_test.go`）。ABSENT: (1) 課題登録→completed、(2) Requirement分解と複数Increment、(3) 2 Runner同時claim、(6) Provider quota→handoff、(8) self-update→Preview failure→Stable復旧、(9) 複数Repository跨ぎ、(10) secret疑い→rotation→再実証。`test/journeys/` directoryが存在しない（`ls test` → No such file） |
| L117 | 各journeyは専用fixtureとtimeoutを持ち、他journeyの実行順や残存processへ依存しない | IMPLEMENTED | 実在するjourney testはいずれも `t.TempDir()` と自前fixtureを使う（`internal/reconciler/fencing_journey_test.go`） |
| L121-128 | Preview capability exerciseを実際のPreview revision、Firestore、Runner、Repository、Providerで実行し、6規則に従う | UNREACHABLE | exerciseを実行するcodeが存在しない（§12 L210-213）。環境等級の宣言だけが `internal/application/release_surface.go:104-105` にある |
| L130-131 | capabilityをAPI call単位へ細分化しすぎず、利用者に意味のあるjourney単位にして共通setupを再利用する | IMPLEMENTED | `contracts/release-contract/foundation-capabilities.json` のcapabilityが利用者操作単位である |
| L135-140 | Documentation verification 6項目 | 混在 | IMPLEMENTED（機構として）: internal linkとversion（`internal/release/docs.go:93,167,181`）、capabilityごとの文書参照（`:129`）、Stable docsのPreview参照検出（`:222`）、code blockのallowlist（`:262`）、必須章の存在（`:197`）。到達性は `BuildPromotionReport` 経由のみで、その読み口は503（§4）。ABSENT: UI操作名・API schema・provider一覧・状態一覧のsource schema照合、review agentによる確認 |
| L142 | AIによる文書reviewは補助でありschema照合とlink検査を代替しない | IMPLEMENTED | AI reviewを昇格条件に含めるcodeが無い（`internal/release/promotion_report.go:74` の条件集合にAI reviewが無い） |
| L146-155 | 設計上限8値（Backlog 10,000、active Increment 100、enrolled Runner 20、concurrent Execution 20、Repository 20、Event retention 100,000、Work Packet 256 KiB、reconcile tick 100 item／30秒） | 混在 | IMPLEMENTED: concurrent Execution 20（`internal/web/owner.html:29` の1–20、`internal/application/allocation.go:520`）、reconcile tick 100 item（`internal/reconciler/reconciler.go:16`）、Work Packet size（`internal/provider/provider.go:19`（`MaxPacketBytes = 1 << 20`））。ABSENT: Backlog 10,000／active Increment 100／enrolled Runner 20／Repository 20／Event retention 100,000 の上限をcodeが持たない（`grep -rn --include='*.go' '10_000\|100_000' internal \| grep -v _test.go` → 0件）。30秒上限は5秒で代替されている（`cmd/control-plane/main.go:200`） |
| L157-163 | 検証する増加率5項目（claimはindexed candidate queryに比例、statusは全raw log／Eventを読まない、heartbeat writeはRunner／Execution数に線形、provider health probeはRequirement数で増えない、documentation exerciseはcapability数に線形） | 混在 | IMPLEMENTED: indexed candidate query（`internal/store/firestore/store.go:982,1018,1066`）、statusがraw logを読まないこと（`internal/application/readmodels.go:243`）、heartbeat write（`internal/application/service.go:474,571`）。UNREACHABLE: provider health probe（`internal/provider/breaker.go:444`）。ABSENT: documentation exerciseの計測 |
| L165 | Firestore read／write数、Cloud Run CPU時間、Provider usageをtest resultへ含めBudget regressionをgateする | ABSENT | evidence recordにこれらの計測fieldが無い（`contracts/schemas/evidence.json`）。`grep -rn --include='*.go' 'BudgetRegression' internal cmd` → 0件 |
| L169-179 | Security verification 11項目 | 混在 | IMPLEMENTED: static secret scanと履歴scan（`Makefile:103`、`gitleaks git`）、redaction test（`internal/api/operator_record_test.go`、`internal/runner/log_test.go`）、negative authorization test（`internal/api/auth_test.go`）、path traversal／symlink escape（`internal/runner/confinement_test.go`、`internal/provider/adapters.go:31`）、command schema injection（`internal/runner/secret_guard.go:12`）、Artifact digest／signature改竄（`internal/update/update_test.go`、`internal/release/bundle_test.go`）、dependency vulnerabilityとlicense scan相当のpin検査（`Makefile:106`）、IaC least privilege drift（`scripts/infra-policy.sh`、`Makefile:114`）。ABSENT: revoked Runner keyとexpired session（enrollment keyのrevoke経路が無い、§13 L192-200）、IAP assertion mismatch（§13 L185-186）、Firestore browser direct access denial（Security Rulesが無い、§13 L183-186） |
| L181 | 実credentialの値をtest reportへ表示しない。live testには専用の最小権限accountを使う | IMPLEMENTED | `internal/api/live_dogfood_test.go`／`live_local_test.go`、`internal/runner/*_live_test.go` がcredential値をlogしない構造。`internal/runner/secret_guard.go:44` |
| L183 | secret scanのallowlist追加・変更時は、同じpatternのsecretをallowlist対象外pathに置いても検出されることをpositive controlとして実測し、before/afterをevidenceに残す | ABSENT | positive controlの実測を強制する機構が無い（`.gitleaks.toml` を検査するtestが無い。`grep -rn 'gitleaks' internal --include='*_test.go'` → 0件） |
| L185 | `.agents/v2/` 配下のgeneric-api-key allowlistは、task-stateの `input_hash`／`output_hash`、evidenceの `evidence_key` が要求する64桁hex参照digestと、`ev-`／`wo-`／`dp-`／`pb-`／`ts-` で始まる参照識別子の二形だけを対象にする | IMPLEMENTED | `.gitleaks.toml` のallowlistとschema要求（`contracts/schemas/task-state.json`、`contracts/schemas/evidence.json`） |
| L187 | `gitleaks git` はcheckout中のbranchのHEADだけでなくrepositoryの全refを走査する | IMPLEMENTED | `Makefile:103`（`gitleaks git --no-banner --redact`、path指定なし） |
| L191-196 | Feedback time targets 4段（format／lint／domain 30秒、affected component 2分、candidate aggregate 2分、Preview deploy＋全capability） | 混在 | IMPLEMENTED（gateの存在として）: `Makefile:87,90,93`（format／lint／test）、`Makefile:14`（affected）、`Makefile:24`（candidate）。ABSENT: 時間目標を測定・強制するcodeが無い |
| L198-199 | candidate aggregate gateはtestを一括再実行せず、現在のcomponent hashに対応するEvidenceを検査する。real Providerと実deployは別のdurableなPreview Release workflowとして完遂する | 混在 | IMPLEMENTED: `Makefile:24`（`candidate`）→ `cmd/ci-plan`、`internal/ci/key_closure.go:113`。ABSENT: Preview Release workflow（§12 L210-213） |
| L203-207 | Evidence freshness 5項目 | 混在 | IMPLEMENTED: candidate digestへの結び付けとbinary／config／schema／docs変更時の無効化（`internal/ci/key_closure.go:113`、`internal/ci/manifest_dependencies.go`）。UNREACHABLE: 外部Provider CLI version変化時の再実行（`internal/provider/compatibility.go:233` は判定するが再実行を起こす経路が無い）。ABSENT: Release Contractによる外部契約ごとのfreshness条件 |
| L211-215 | Flaky policy 5項目 | ABSENT | flaky検知・quarantine・retry方針を実装するcodeが無い（`grep -rn --include='*.go' 'Flaky\|quarantine' internal cmd \| grep -v _test.go` → 0件） |
| L221-236 | Definition of Doneを Increment Done 4項目と Requirement Done 7項目に分ける | UNREACHABLE | Increment Doneの判定は `internal/domain/model.go:572-660` の状態遷移に対応するが非test発行が無い（§4）。Requirement Doneは `internal/domain/model.go:526` で非test callerが0 |
| L240-248 | 境界と差分に基づく選択的CIの5段と「未分類file、循環依存、存在しないtest targetをmanifest検証の失敗とする」 | IMPLEMENTED | `internal/ci/manifest_dependencies.go:405`（`VerifyDependencyCoverage`）、`:436`（`VerifyNoUnjustifiedEdges`）、`internal/ci/manifest_dependencies.go:513`（`VerifyCheckTargetInsideClosure`）、`internal/ci/planner.go`、`Makefile:11,14,17`、`ci/components.json` |
| L252-253 | component ごとのevidence keyを作り、同じkeyの成功evidenceを再利用する。候補commitのgateはaggregate attestationで全componentの新鮮さを確認する。evidence keyは `make check` が緑になった最終treeで読む | IMPLEMENTED | `internal/ci/key_closure.go:113`、`Makefile:24,30,33` |
| L255-261 | evidence key算法5項目（version prefix `agentic-loop/evidence-key/v2`、依存面の推移閉包をhash、無条件file 7 path、versioned＋length-prefixed framing、`verification_dependencies` を影響閉包の選択に使わない、閉包を `ci/key-closure.json` へpublishしgolden testが byte一致を検証） | IMPLEMENTED | `internal/ci/key_closure.go:113`、`ci/key-closure.json`、`internal/ci/key_closure_test.go`、`internal/ci/manifest_dependencies.go` |
| L265 | `cmd/ci-plan` が `--task-id`／`--correlation-id` を受け取り、空なら既定値へfallbackし、`task_id` は `^V2-[0-9]{3}$` を満たさない限りrecordを1件も書かずに失敗する | IMPLEMENTED | `cmd/ci-plan/main.go:18-19`（既定値 `V2-000`／`local-component-evidence`）、`:22`（`^V2-[0-9]{3}$`）、`:38-39,45` |
| L269-271 | `make evidence-all`／`make candidate`／`make evidence-keys` の3 target | IMPLEMENTED | `Makefile:30,24,33`、`cmd/ci-plan/main.go:27-28`（`--all`／`--keys`） |
| L273 | `build/evidence` は `.gitignore` 済みのephemeralで、commitしない | IMPLEMENTED | `.gitignore:13`（`/build/`）、`Makefile:30` の出力先 |
| L275-281 | 2-commit protocol 5段 | ABSENT | protocolを強制する機構が無い（`grep -rn 'base_commit' internal --include='*.go' \| grep -v _test.go` → 0件。attestation生成のcodeが存在しない） |
| L285 | 選択的CIは開発feedback用のgateであり、Previewでの全capability exercise・実Provider接続・Stable昇格判定は差分にかかわらず実行する | UNREACHABLE | 後者3つを実行するcodeが無い（§12 L210-213、§4） |
| L289 | 代表的な差分fixtureに対し、期待する影響閉包と実際のmatrixが一致することをtestする | IMPLEMENTED | `internal/ci/planner_test.go`、`ci/fixtures/` |
| L291 | manifest自身に3検査を課し、いずれにも「壊せば落ちる」positive controlを1本以上付ける | IMPLEMENTED | `internal/ci/manifest_dependencies.go:405,436,513`、`internal/ci/manifest_dependencies_test.go` |

## 15. `docs/architecture/documentation.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L5-11 | 原則5項目（Stable文書だけで理解できる、Preview文書だけで利用でき差分も分かる、現在仕様と設計理由と運用手順と生成referenceを混ぜない、同じ事実を複製しない、文書を実装と同じRelease Artifactと同じpromotion gateで扱う） | 混在 | IMPLEMENTED（検査として）: `internal/release/docs.go:197,222`、`internal/release/promotion_report.go:385`。到達性は503（§4）。ABSENT: 生成referenceの分離（`generated/` が存在しない） |
| L15-58 | 読者別の正本tree（`docs/stable/` 7 file、`docs/preview/`、`docs/product/`、`docs/architecture/`＋`decisions/`、`docs/runbooks/` 4 file、`contracts/`＋`events/`＋`work-packet/`、`generated/reference/`） | 混在 | 存在: `docs/stable`、`docs/preview`、`docs/product`、`docs/architecture`、`docs/architecture/decisions`、`contracts/openapi/openapi-v1.yaml`、`contracts/release-contract/`。ABSENT: `docs/runbooks/`、`docs/architecture/security.md`、`contracts/events/`、`contracts/work-packet/`、`generated/reference/`（`ls` で不在を確認） |
| L62-65 | AGENTS.mdに大原則と短いroutingだけを置き、毎turn読む内容を限定し、停止protocol／API／test commandを複製せず、大原則の変更に人間承認が必要と記す | IMPLEMENTED | `AGENTS.md` が存在し、`Makefile:99`（`docs` target）が旧文書名への参照をrgで禁止する |
| L72-76 | Stable文書: default URLで現在Stable版を表示、現在形の完全仕様、UI各画面からのcontext link、rollback対象の過去Stable文書をversion URLで保持 | ABSENT | 文書をURLで配信する経路が無い（§6 L253-259）。version URL保持も無い |
| L80-83 | Preview文書: Preview tag URLでcandidate版を表示、Stable文書を前提にしない完全な操作説明、`stable-diff.md` に追加／変更／廃止／migration／既知問題／rollbackをまとめる、未実証capabilityと昇格blockerを自動表示 | 混在 | IMPLEMENTED（検査として）: Preview markerと必須章（`internal/release/docs.go:167,197`）。ABSENT: tag URL配信、未実証capabilityの自動表示（`internal/release/promotion_report.go:305` は算出するが表示経路が503） |
| L85-86 | StableとPreviewで同じ章を手作業copyせず、sourceを一つ持ちbuild時にchannel bannerと差分情報を組み合わせる | ABSENT | build時に文書を合成するcodeが無い（`grep -rn 'channel banner\|ChannelBanner' internal --include='*.go'` → 0件） |
| L90-98 | Release Contractの各capabilityがStable文書anchor／Preview文書anchor／UI route／exercise implementation／Evidence schemaを参照し、anchorがない・存在しない・version不一致はpromotionを止める | 混在 | IMPLEMENTED: anchor bijection検査（`internal/release/docs.go:129`）、`contracts/release-contract/foundation-capabilities.json` の `documentation`／`owner_surfaces`／`evidence_schema`。ABSENT: Stable文書anchor（capability objectに `stable` keyが無い）、exercise implementationへの参照 |
| L102-108 | schemaから生成するもの6種（API endpoint／field reference、Requirement／Increment／Release状態一覧、Event type一覧、Provider capability matrix、configuration key reference、CLI command reference） | ABSENT | 生成物が一つも無い（§14 L68） |
| L111-118 | 人間／Loopが説明を書くもの6種 | IMPLEMENTED | `docs/stable/`／`docs/preview/` に該当文書が存在する |
| L120 | generated referenceを説明文へcopyせずlinkする | ABSENT | generated referenceが存在しない |
| L124-133 | Change contract 6段を同じIncrementで行い、文書だけを後続Requirementへ先送りしない | UNREACHABLE | Incrementが存在し得ない（§4）。drift検査自体は `internal/release/docs.go:93` にある |
| L138-146 | ADRはContext／Decision／Alternatives／Consequences／Revisit triggerを持ち、置換時は削除・書換えせずsuperseded linkを付ける | IMPLEMENTED | `docs/architecture/decisions/` が存在。構造検査は `internal/release/docs.go:197`（`VerifyRequiredSections`） |
| L151-159 | Verification 9項目（Markdown lint／link／anchor／spelling、docs内versionとRelease manifestの一致、generated referenceのclean regeneration、UI label／routeと文書の照合、state／Provider一覧とschemaの照合、code blockの安全なsmoke、Stable docsからPreview-only機能への誤参照検出、Preview diffのbreaking changeとrollback検査、初見利用者personaによるAI review） | 混在 | IMPLEMENTED（機構として）: link／anchor（`internal/release/docs.go:93,81`）、version marker一致（`:167,181`）、code block allowlist（`:262`）、Stable→Preview誤参照検出（`:222`）、必須章（`:197`）。ABSENT: Markdown lintとspelling、generated reference regeneration、UI label／routeとの照合、state／Provider一覧とschemaの照合、AI review。`make docs`（`Makefile:99-101`）が実行するのは旧文書名への参照禁止 2 rgだけである |
| L161 | AI review promptと結果は再現可能にするが決定的checkの代替にしない | IMPLEMENTED | AI reviewを条件に含めるcodeが無い（§14 L142） |
| L165-169 | Loopが通常の文書maintenanceを行い、人間承認が必要なのは大原則の意味変更だけ。文書品質はRelease Contract capabilityを文書だけから実行できるかで評価する | UNREACHABLE | 文書maintenanceを行うLoop経路が無い（§4）。評価はcapability exercise依存（§12 L210-213） |
| L173-181 | v2開発中の設計・作業文書5種をv2 branch自身にversion管理し、task固有の一時メモとStable／Preview文書を混在させない | 混在 | IMPLEMENTED: product／architecture／decision文書、canonical task state（`.agents/v2/task-state/`）、validation evidenceのindex（`.agents/v2/evidence/`）。ABSENT: component ownershipとcontract dependency manifestは `ci/components.json` として存在するが、provider-neutral work packetとrelease candidate manifestとcutover／rollback runbookが無い（`ls .agents/v2`、`docs/runbooks` 不在） |

## 16. `docs/architecture/roadmap.md`

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L7-9 | `main` から作るv2 branchのtracked treeをbootstrap commitで白紙化し、新systemだけを構築する。v1は `main` で維持する | IMPLEMENTED | 現treeにv1 Bash実装が無い（`ls` の結果にshell実装群が無い）、`git log --oneline -1` が `7860857` |
| L11-16 | 方針6項目（GitHub Issue stateと双方向同期しない、現行moduleを直接移植しない、知識をcontract／fixture／failure scenarioとして移す、自己dogfood可能な最小vertical sliceを先に通す、milestoneはPR境界でない、v2内で利用可能になったcapabilityをRelease Contractへ追加する） | 混在 | IMPLEMENTED: 双方向同期なし（§5 L281）、直接移植なし、contract／fixtureとしての移送（`contracts/`）。UNREACHABLE: 自己dogfood可能なvertical slice（§4のとおりrunnerが無く、Increment生成経路も無い） |
| L22-43 | v2 Repository layoutが宣言するdirectory | 混在 | 存在: `cmd/control-plane`／`cmd/runner`／`cmd/bootstrap`、`internal/domain`／`application`／`store/firestore`／`api`／`scheduler`／`release`／`runner`／`provider`、`contracts/`、`infra/`、`docs/stable`／`docs/preview`／`docs/architecture`、`.agents/v2/`、`.codex/agents/`。ABSENT: `web/`（実体は `internal/web/`）、`test/journeys/`（`ls test` → 不在） |
| L45-46 | 旧実装から持ち込むのは再確認したcontract／fixture／failure scenario／必要policyだけ。v1 sourceのarchiveをv2 treeへ残さない | IMPLEMENTED | `legacy/` directoryが無い。`internal/legacyimport/import.go` は一度きりのimport toolでありv1 sourceではない |
| L52-69 | M0成果8項目と完了条件5項目 | 混在 | IMPLEMENTED: 文書採用、Release Contract schema（`contracts/schemas/release-contract.json`）、OpenAPIとEvent／Work Packet schema（`contracts/openapi/openapi-v1.yaml`、`contracts/schemas/domain-event.json`、`work-packet.json`）、capability一覧（`contracts/release-contract/foundation-capabilities.json`）、Go module／format／lint／domain testの共通入口（`Makefile:9,87,90,93`）、component／contract manifestと選択的CI（`ci/components.json`、`cmd/ci-plan`）、Problem Brief／Design Packet／Work Order／Evidence schema（`contracts/schemas/`）、agent定義（`.codex/agents/`）、schema compatibility test（`internal/contracts/contracts_test.go`）、影響閉包の算出（`internal/ci/planner_test.go`）、外部resourceを作らないこと。ABSENT: Stable／Preview docs骨格のうち `docs/runbooks` 系（§15 L15-58）、現行機能棚卸しの全項目にretain／redesign／remove／deferがあることを機械検査するもの |
| L75-86 | M1成果5項目と完了条件3項目 | 混在 | IMPLEMENTED: Requirement／Increment（`internal/domain/model.go`）、Execution／Lease／fencing（`internal/domain/lease.go`）、Control Intent（`internal/domain/control.go`）、Release promotion gate（`internal/domain/release.go:123,159`）、failure taxonomy（`internal/domain/release.go:174-189`）、model-based test（`internal/domain/invariant_model_test.go`）、DB／GitHub／Provider依存なし、stop effective後の副作用permitが0（`internal/application/stop_matrix_test.go:52-64`）。ABSENT: Priority Assessment（§5 L92b）。UNREACHABLE: Retry Budget（§5 L50） |
| L91-102 | M2成果5項目と完了条件4項目 | 混在 | IMPLEMENTED: 専用UIでの課題登録・Backlog・Requirement・Control表示（`internal/web/owner.html:4-9`）、Firestore current state＋Event＋Outbox（`internal/store/firestore/store.go`。ただしOutboxは書けるが配送されない、§10）、owner認証のlocal session/token境界（`internal/api/auth.go:131-136`）、budget hard guard（`internal/quota/quota.go:81`）、logical export（`internal/api/api.go:311`）、OpenTofuのoffline validateとplan digest生成経路（`scripts/infra-plan.sh`、`Makefile:120`）。DEFERRED-BY-DECLARATION: 実planからのdigest生成とD1の4点（宣言L101-102がそう述べる） |
| L106 | D1: 4点＋承認対象plan digestの実生成を `preview-gcp` のlive evidenceで実証する | DEFERRED-BY-DECLARATION | 宣言自身がCloud Run／実Firestore／IAP／deploy経路を名指す。`infra/main.tf:13`（`data "google_project"`）が実GCP接続を要する |
| L110-123 | M3成果5項目と完了条件4項目 | UNREACHABLE | Runner enrollmentは到達可能（`internal/runner/protocol.go:65`）だが、heartbeat／claim／Leaseを駆動する主体、workspace、process supervisor、Secret Broker、代表Provider Adapterの実行、1 Increment変更・検証・integration、provider-neutral Work Packetのいずれも非test到達不能（§4）。「課題登録からIncrement Artifactまで通る」を成立させるcodeが無い |
| L127-142 | M4成果5項目と完了条件4項目 | 混在 | IMPLEMENTED: pause intake／claim、graceful／immediate／emergency stop、Control revision／fencing（`internal/domain/control.go:266,297`）、stop中にclaim／integration／promotionが成功しないこと（`internal/application/stop_matrix_test.go:52-64`）、partition Runnerの古いResult拒否（`internal/application/service.go:1660`）。UNREACHABLE: lost／orphan reconciliation（`internal/reconciler/orphan.go:28`）、ambiguous external operation処理（`internal/application/outbox.go:540`）、接続中Runnerのprocess終了検証（`internal/runner/supervisor.go:73`） |
| L147-162 | M5成果6項目と完了条件4項目、および「本milestoneのPreviewは `preview-local` で満たす」 | UNREACHABLE | Release Candidate bundle（`internal/release/bundle.go`）とRelease Contract（`contracts/release-contract/`）とpromotion report（`internal/release/promotion_report.go:305`）は存在するが、Preview deploy／capability exercise／Stable promotion／rollback／文書routing／completed判定のいずれも非test到達不能（§4、§12） |
| L167-178 | M6成果4項目と完了条件4項目 | 混在 | 3 adapterのargv／parse／normalize（`internal/provider/adapters.go:122-137`）とcompatibility宣言（`internal/provider/compatibility.go:172,187`）はIMPLEMENTED。Provider Account／pool／quota／circuit breaker、provider-neutral handoffはUNREACHABLE（`internal/provider/pool.go:175`、`breaker.go:214`、`handoff.go:717`）。3 Provider live確認はDEFERRED-BY-DECLARATION（codex／opencodeが未認証） |
| L182-195 | M7成果5項目と完了条件4項目 | 混在 | IMPLEMENTED: 1 Backlogから複数Repositoryへの関連付け（`internal/domain/repository.go`、`internal/api/repositories.go:109`）、複数Runner capacityとpriority scheduling（`internal/application/allocation.go:625`）、2台目setup手順書（`docs/operations/runner-local.md`）。ABSENT: Resource Claim（§8 L103-115）、cross-repository Increment（`grep -rn --include='*.go' 'CrossRepository' internal \| grep -v _test.go` → 0件） |
| L199-213 | M8成果6項目と完了条件4項目 | 混在 | IMPLEMENTED: signed Runner bundle（`internal/update/update.go:127`）、independent Bootstrapper（`cmd/bootstrap`）、module version manifest（`internal/update/update.go:83`）。UNREACHABLE: Repository単位のStable／Preview Loop routing（`internal/release/release.go:120`）、schema expand／coexist／migrate／contract（`internal/update/schema.go:98,156,209`）、old module／binary GC（`internal/update/retention.go:206`）。ABSENT: 新Loopが自分自身の新versionをPreviewへdeployする経路 |
| L217-231 | M9成果5項目と完了条件5項目 | 混在 | IMPLEMENTED: one-time import tool（`cmd/legacy-import/main.go:19-27`、`internal/legacyimport/import.go:64`）、dry-runとcontent digest（`internal/legacyimport/import.go` のManifest）。ABSENT: legacy stop／drain verification、cutover manifest、legacy read-only archive、GitHub Issue queue code削除候補一覧、rollback rehearsal（該当codeが無い） |
| L235-247 | Legacy data migration 9段と「comment marker、Label履歴、Project fieldを新domain Eventとして完全再現しない」 | 混在 | IMPLEMENTED: open Issueのread-only export入力（`internal/legacyimport/import.go:64` が `Export` を受ける）、正規化とduplicate／completed／cancelledの分類、dry-run表示、上限（`cmd/legacy-import/main.go:27` の `maxIssues`／`maxText`）。ABSENT: legacy側の新規claim停止、drain完了の検証、secret scanによる隔離一覧、transactionでの新Backlogへのimportと `legacy_source` 参照付与（`grep -rn --include='*.go' 'legacy_source\|LegacySource' internal \| grep -v _test.go` → 0件） |
| L252-273 | Cutoverとrollback（Cutover前4項目、Cutover5項目、Rollback: 旧legacyを無条件に再起動せずM8で保持した直前のnew Stableへ戻す） | ABSENT | cutoverを実行・記録するcodeが無い（`grep -rn --include='*.go' 'cutover\|Cutover' internal cmd \| grep -v _test.go` → 0件） |
| L277-286 | v2稼働後のproduct workflow 8段 | 混在 | IMPLEMENTED: 新専用UIへの課題登録（`internal/web/owner.html:4`）、affected deterministic test（`Makefile:14`）、全componentのaggregate Evidence gate（`Makefile:24`）。UNREACHABLE: LoopによるProblem Frame／priority／Increment管理、module追加とPreview routing、Previewへのdeploy、capability exercise、自動Stable promotion、旧module削除（§4、§12） |
| L290-297 | Risk reduction order 7段 | 混在 | 1（state machineとfencing）と2（stopとrecovery）はIMPLEMENTED（`internal/domain/`、`internal/application/stop_matrix_test.go`）。3以降（1 Provider／1 Repositoryの縦切り、Preview／Stableと文書、横展開、self-update、legacy切離し）はUNREACHABLEまたはABSENT（上記各milestone行） |
| L302-311 | 初期defer 10項目と「需要を確認するまでdomainとAPIへ予約fieldを増やさない」 | IMPLEMENTED | 10項目いずれのcodeも存在しない。`contracts/openapi/openapi-v1.yaml` に予約fieldが無い |
| L317-320 | 非目標2件（サーバー跨ぎ重複調停、複数人でのタスク管理） | IMPLEMENTED | §5 L290、L291と同じevidence |
| L324-332 | v2 white-slate branch戦略5項目と「branch作成と大量削除時はread-only checkで対象を確定し明示的preflight承認を得る」 | 混在 | IMPLEMENTED: 共通祖先を維持する通常branch（`git log` で `main` からの派生）。ABSENT: preflight承認を強制する機構 |
| L336-345 | 会話に依存しないcheckpointとして各taskが6項目を残し、分析／詳細設計／実装／検証の各境界でcheckpoint commitを作る | IMPLEMENTED | `contracts/schemas/task-state.json` が該当fieldを要求し、`internal/contracts/validator.go:32` が検証する。`.agents/v2/task-state/` が正本 |

## 17. `docs/architecture/legacy-inventory.md`（部分走査）

`remove`／`defer` 判定は「移植しない」という不在の宣言であり、振る舞いの主張ではないため
代表4件のみを採録した。`retain`／`redesign` は新systemにその能力が存在するという主張なので
採録した（同じevidenceに帰着する行はまとめた）。

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| L366 | GitHub Issueを正本にせず専用UIとcanonical storeへ置換する（`remove`） | IMPLEMENTED | §5 L281、`internal/web/owner.html:4` |
| L367 | UIから課題・観測・feedbackを登録しHowを仮説として扱う（`redesign`） | IMPLEMENTED | `internal/application/service.go:748`、`internal/web/owner.js:1` |
| L368-371 | 固定category順でなく価値・緊急性・risk・cost・依存を継続評価する／飢餓時間を判断材料の一つにする（`redesign`） | WIDER-THAN-CODE | `internal/scheduler/decision.go` と `internal/scheduler/priority.go` は飢餓時間とscoreを扱うが、risk／cost／依存のinputが存在しない（§5 L179、L92c） |
| L369 | 利用者はqueue順を指定せず、ループが優先順位と理由を管理する（`remove`） | 混在 | 利用者がqueue順を指定できないのはIMPLEMENTED（§6 L98-99）。ループが理由を管理することはABSENT（`domain.PriorityAssessment` が無い、§5 L92b） |
| L371-372 | RequirementからIncrementへのdomain関係／Requirement・Increment dependencyをcanonical storeへ保持する（`redesign`） | 混在 | Requirement→Increment関係の型はIMPLEMENTED（`internal/domain/model.go` のIncrement.RequirementID）。dependencyはUNREACHABLE（§5 L92c） |
| L374 | duplicate／superseded／merged dispositionをBacklog内のRequirement関係として扱う（`retain`） | ABSENT | Requirement間関係を表す型が無い（`grep -rn --include='*.go' 'Duplicate\|Superseded' internal/domain \| grep -v _test.go` → 0件） |
| L376 | UI上のQuestion／Answerをcanonical stateへ直接保存する（`remove`） | IMPLEMENTED | `internal/application/human_input.go:460,572`、`internal/web/owner.html:32` |
| L382 | repositoryごとのSupervisorをInstallation全体のschedulerと独立Runner Agentへ分離する（`remove`） | 混在 | Installation全体のschedulerはIMPLEMENTED（`internal/application/allocation.go:625`）。独立Runner AgentはUNREACHABLE（§4） |
| L383 | hostごとの `max_workers` をInstallation／Runner／Repository／Provider Poolの階層的capacityにする（`redesign`） | WIDER-THAN-CODE | Installation levelのみ実在（`internal/application/allocation.go:520`）。Runner／Repository／Provider Pool levelのlimit fieldが無い（§6 L222-228） |
| L384 | claim競合調停をdatabase transactionによるLease取得へ置換する（`remove`） | UNREACHABLE | `internal/application/service.go:1132` は存在するが `ready` Incrementが存在し得ない（§4） |
| L385 | lease heartbeat／期限切れ回収をfencing token付きLeaseとして中核domainへ置く（`retain`） | IMPLEMENTED | `internal/domain/lease.go`、`internal/application/service.go:229,474`、`internal/reconciler/reconciler.go:41` |
| L386-387 | scope markerによる競合予測をIncrementのResource Claimと実diff観測へ置換する／調査段階は並列可、変更開始前にClaimを具体化する（`redesign`） | ABSENT | Resource Claimが存在しない（§8 L103-115） |
| L388 | pathだけでなくschema、deploy target、secret、外部resourceをclaim対象にする（`retain`） | ABSENT | 同上 |
| L389 | 単一Backlogで価値と飢餓時間を評価しresource classごとに配分する（`redesign`） | 混在 | 飢餓時間はIMPLEMENTED（`internal/scheduler/priority.go`）。resource classごとの配分はABSENT（`scheduler.ResourceRequest` はname＋modeのみ、`internal/application/allocation.go:466`） |
| L390-392 | provider pool fallbackをProvider AdapterとProvider Pool capacityへ分離する／pool exhaustionをcapacity observationとcooldownとして保存する／API rate limit reserveを各adapterのresource budgetへ一般化する | UNREACHABLE | `internal/provider/pool.go:175`、`internal/provider/breaker.go:214,321`。いずれも非test callerが0 |
| L398 | plan／execの固定2段階を廃し、Analyze／Implement／Verify／Releaseをcommandとして反復し固定AI call数を持たない（`remove`） | ABSENT | commandループを回すcodeが無い（`internal/runner/control_loop.go:15` は非test到達不能で、かつAnalyze／Implement／Verify／Releaseのcommand集合を持たない） |
| L399 | Work type、risk、残予算からProvider／modelをschedulerが選ぶ（`redesign`） | UNREACHABLE | `internal/provider/handoff.go:717`（`DecideHandoff`）に非test callerが0 |
| L400 | bounded replanをRetry Budgetと異なるapproachの履歴として扱う（`retain`） | UNREACHABLE | `internal/domain/release.go:204,217`（`Allow`／`Consume`）。`Consume` に非test callerが0 |
| L402 | provider固有error判定をAdapter内のcontractと実Provider Preview検証に閉じ込める（`retain`） | 混在 | Adapter内正規化はIMPLEMENTED（`internal/provider/provider.go:133-139`）。実Provider Preview検証はcodex／opencodeについてDEFERRED-BY-DECLARATION |
| L403 | Worktree分離をWorkspace Managerの責務として維持する（`retain`） | UNREACHABLE | `internal/runner/workspace.go:11`、`internal/runner/git.go:138` |
| L406 | worktree／branch／PR観測からの再開をWork Packet、Checkpoint、Artifact Observationとして一般化する（`retain`） | UNREACHABLE | `internal/provider/provider.go:46`（非test生成0）、`internal/application/service.go:604`（checkpointは保存できるが消費経路が無い） |
| L407 | dirty worktree引継ぎをcheckpoint可能なworkspace snapshotとして明示する（`redesign`） | ABSENT | workspace snapshotの型が無い |
| L409 | 同一Requirementの複数Incrementを1対多の中核modelとする（`retain`） | UNREACHABLE | 型はIMPLEMENTED（`internal/domain/model.go`）だが2つ目以降を作る経路が無い（§4） |
| L415 | `start`／`stop` をInstallation／Repository／Requirement／Runner／channelを対象にするControl Intentへ置換する（`redesign`） | IMPLEMENTED | `internal/domain/control.go:24-31`（6 scope kind）、`internal/api/api.go:382` |
| L416 | stop時のworker drainにrequested／acknowledged／effective／verifiedの段階を持たせる（`retain`） | IMPLEMENTED | `internal/domain/control.go:82-86`、`internal/reconciler/control_verifier.go:87` |
| L417 | pause、cancel、resumeをRequirement lifecycleとExecution lifecycleで分離する（`redesign`） | WIDER-THAN-CODE | Execution側はIMPLEMENTED（`internal/domain/model.go:158-166`）。Requirement側の pause／resume／cancel は遷移を発行できない（§5 L147、§8 L269） |
| L418 | process group TERM→KILLをRunner Process Supervisorのlocal責務として維持する（`retain`） | UNREACHABLE | `internal/runner/supervisor.go:73` |
| L419 | worker timeoutをcommand deadline、progress deadline、Lease expiryとして別々に扱う（`redesign`） | 混在 | Lease expiryはIMPLEMENTED（`internal/reconciler/reconciler.go:41`）。command deadlineとprogress deadlineはABSENT（§8 L132-143） |
| L420 | orphan worker検出をLease fencingとlocal process reconciliationで構造化する（`retain`） | UNREACHABLE | `internal/reconciler/orphan.go:28`（非test構築0） |
| L421 | Control Planeはmanaged service、Runner AgentはOS serviceとして個別に回復する（`redesign`） | 混在 | Control Plane側はDEFERRED-BY-DECLARATION（Cloud Run）。Runner AgentのOS service定義はABSENT（`grep -rn 'systemd\|\.service' scripts infra \| head` に該当unitが無い） |
| L422 | 自動回復不能でもRequirementを終端にせず `needs-input` へ遷移する（`redesign`） | IMPLEMENTED | `internal/application/human_input.go:519`（`RequirementNeedInput`）、`internal/domain/model.go:94-104`（`failed` が無い） |
| L429 | 共通full checkをRelease Contractの決定的検証の一部にする（`retain`） | IMPLEMENTED | `Makefile:9`（`check`）、`internal/release/promotion_report.go:305` の決定的test条件 |
| L430 | affected checkをIncrement中の高速feedbackに限定し昇格gateには使わない（`retain`） | IMPLEMENTED | `Makefile:14,27`（`affected`／`candidate-affected`）と `Makefile:24`（`candidate` は別target） |
| L431 | 巨大fake E2Eをdomain state test、adapter contract、少数journey、Preview実証へ分割する（`redesign`） | 混在 | 前3つはIMPLEMENTED（§14 L26-33、L88-98、L104-116）。Preview実証はUNREACHABLE（§12 L210-213） |
| L432 | flaky検知／quarantineを決定的検証の信頼性管理として維持する（`retain`） | ABSENT | §14 L211-215 |
| L433 | host smokeをPreview実証へ統合し対象外部systemごとに実施する（`retain`） | 混在 | `Makefile:109`（`smoke`）は3 binaryの `--version` のみ。外部systemごとの実施はUNREACHABLE（§12 L215） |
| L435 | traceability recordをRequirement→Increment→Artifact→Evidence→Releaseのdomain linkにする（`retain`） | UNREACHABLE | §5 L48 |
| L436 | preflight risk gateをChange ProposalとCapability／Resource定義から評価する（`retain`） | 混在 | deployment preflightとprovider preflightの台帳検査はIMPLEMENTED（`internal/contracts/deployment_preflight.go:108`、`internal/contracts/provider_preflight.go:92`）。Change ProposalはABSENT |
| L437 | workload manifest／static scanをCost／Resource Budgetのchange-time評価へ統合する（`retain`） | ABSENT | change-timeのcost評価codeが無い（§13 L315-324） |
| L439 | install／upgrade検証をLoop VersionのPreview／Stable releaseとBootstrapperへ分ける（`redesign`） | 混在 | BootstrapperはIMPLEMENTED（`cmd/bootstrap`）。Loop VersionのPreview／Stable releaseはUNREACHABLE（`internal/release/release.go:120`） |
| L440 | rollbackでApplication、Loop、schema、docsを同一Release単位で戻す（`retain`） | 混在 | Loop binaryのrollbackはIMPLEMENTED（`internal/update/switch.go`、`cmd/bootstrap/main.go:97-121`）。Application／schema／docsを同一単位で戻す経路はUNREACHABLE／ABSENT（`internal/update/schema.go:209` に非test callerが0、docs bundleが無い） |
| L446 | `status` を専用UIのInstallation／Backlog／Runner／Release viewへ置換する（`retain`） | 混在 | Installation／Backlog／RunnerはIMPLEMENTED（`internal/web/owner.html:5,9,12`）。Release viewは503（§4） |
| L447 | `tail`／events.log をstructured EventとRunner local diagnostic logへ分ける（`redesign`） | 混在 | structured EventはIMPLEMENTED（`internal/application/service.go:1773`）。Runner local diagnostic logはUNREACHABLE（`internal/runner/log.go:38`） |
| L448 | `doctor` をControl Plane、Runner、Provider、Repository、Release Contractのhealth診断へ置換する（`retain`） | 混在 | Repositoryの実行可否はIMPLEMENTED（`internal/application/repository.go:288,513`）。Control Planeは `/healthz`（`internal/api/api.go:91`）。Runner／Provider／Release Contractのhealthはそれぞれ自己申告のみ・未観測・503（§6 L235-243、§4） |
| L449 | `metrics` を単一Backlogのlead time、自律完了率、Preview失敗、rollback、資源配分の測定へ置換する（`retain`） | ABSENT | §5 L295-307。資源配分の現況表示のみIMPLEMENTED（`internal/application/allocation.go:558`） |
| L452 | marker整合性診断をtyped stateとschema validationへ置換する（`remove`） | IMPLEMENTED | `internal/store/firestore/store.go:208-222`、`internal/contracts/validator.go` |
| L453 | progress stage／stall表示をCommand progressとlast evidence timeとして扱う（`retain`） | ABSENT | progress deadlineとlast evidence timeのfieldが無い（§5 L184-197） |
| L454 | usage commentをProvider Runのusage／duration recordとしてcanonical stateへ保存する（`redesign`） | ABSENT | Provider Run型が無い（§8 L168-185） |
| L455 | postmortemをIncidentとAction Itemとして同じBacklogへ登録し再発防止のStable昇格まで追跡する（`retain`） | ABSENT | Incident／Action Itemの型が無い |
| L461 | Control Plane、Runner Agent、Repository Contractを別artifactにする（`remove`） | 混在 | Control PlaneとRunnerは別commandでIMPLEMENTED。Repository ContractはABSENT（§6 L72-81） |
| L462 | install scriptをself-host Control Plane provisionerとRunner enrollmentへ分ける（`redesign`） | 混在 | provisionerは `infra/` としてIMPLEMENTED（applyはDEFERRED）。Runner enrollmentはIMPLEMENTED（`internal/runner/protocol.go:65`） |
| L463 | repository-local TOMLをRepository ContractとInstallation／Runner設定へ分離する（`redesign`） | ABSENT | Repository Contractが無い。Installation設定は環境変数のみ（`cmd/control-plane/main.go:107-135`） |
| L464 | capability manifestをversion付きRepository Contractの一部として拡張する（`retain`） | 混在 | capability declaration setはIMPLEMENTED（`contracts/release-contract/foundation-capabilities.json`）。Repository Contractの一部としては存在しない |
| L465 | environment definition／DevboxをRepositoryが選ぶ再現可能Environment Contractとして扱う（`retain`） | 混在 | Foundation自身のDevboxはIMPLEMENTED（`devbox.json`、`devbox.lock`）。Repositoryごとに選ばせる機構はABSENT |
| L466 | provider CLI selectionをRunner CapabilityとProvider AccountとしてUIで登録する（`retain`） | ABSENT | Runner Capability型もProvider Account型も無い（§8 L158-185）。`internal/web/owner.html:15` は読み取り専用 |
| L467 | upgrade migration scriptsをversioned module、expand/migrate/contract、Releaseへ置換する（`redesign`） | UNREACHABLE | `internal/update/schema.go:98,156,209` に非test callerが0 |
| L469 | Stable／Preview利用者文書を実装と同じversioned release artifactとして新設する（`retain`） | 混在 | `docs/stable/`／`docs/preview/` は存在し marker検査もある（`internal/release/docs.go:167,181`）。release artifactとしてのbundle化と配信はABSENT（§15 L72-83） |
| L475 | secret guard hookをWorkspace、commit、artifact、outbound payloadの複数境界へ適用する（`retain`） | 混在 | argv／env／log境界はIMPLEMENTED（`internal/runner/secret_guard.go:12,32,44`）。commit／artifact／outbound payload境界はUNREACHABLE（`internal/runner/forgepublish.go:302,520`） |
| L476 | gitleaksをRepository scanの一層として維持する（`retain`） | IMPLEMENTED | `Makefile:103` |
| L477 | canonical stateにもraw secret-bearing contentを既定保存しない（`retain`） | IMPLEMENTED | §9 L164 |
| L478 | Runner Secret Brokerがruntimeだけ注入しControl Planeは秘密を保持しない（`redesign`） | 混在 | Control Planeが保持しないのはIMPLEMENTED（§7 L171）。Secret BrokerはUNREACHABLE（`internal/runner/secret_broker.go:149`） |
| L479 | Provider Adapterとは別に全Providerへ共通のWorkspace Sandbox契約を要求する（`redesign`） | 混在 | 共通sandboxはIMPLEMENTED（`internal/runner/confinement.go:114`、`cmd/bootstrap/main.go:133,138` から到達）。Provider実行にそれを課す経路はUNREACHABLE（`internal/runner/provider.go:500-506`） |
| L480 | GitHub scope検査をExternal Accountごとの最小権限とpreflightへ一般化する（`redesign`） | ABSENT | External Account型が無い（`grep -rn --include='*.go' 'ExternalAccount' internal \| grep -v _test.go` → 0件） |
| L486-493 | 棚卸しの結論8件（期限付きLeaseとfencing、process生存と進行の分離、観測からの再開、failure classの分離、Retry／poll／log／API／worker／costの有限性、fakeだけでは契約を保証できない、完了・停止・rollbackの外部観測、4 lifecycleの分離） | 混在 | IMPLEMENTED: Lease＋fencing（`internal/domain/lease.go`）、heartbeatとprogressの分離（`internal/api/api.go:394,396`）、failure classの分離（`internal/domain/release.go:174-189`）、有限性（`internal/quota/quota.go:81`、`internal/reconciler/reconciler.go:16`、`internal/store/firestore/store.go:29-32`、`internal/runner/log.go:38`）、lifecycle分離（§6 L286-293）。UNREACHABLE: 観測からの再開（Work Packet／Artifact Observationの生成経路が無い）。ABSENT: 完了・停止・rollbackの外部観測（`domain.ProcessObservation` の書き手が無い、deploy観測が無い） |
| L495-496 | GitHub同期、Label状態機械、Project投影、comment marker protocol、1 Issue 1 PR、巨大な単一E2Eを移植しない（`remove`） | IMPLEMENTED | 該当codeが一つも存在しない（`grep -rn --include='*.go' 'Label\|ProjectV2\|comment marker' internal/store internal/application \| grep -v _test.go` → 0件） |

## 18. 対象13文書の外にある1件（明示依頼により採録）

`contracts/openapi/openapi-v1.yaml` は§2の対象文書ではないが、既測定の
`WIDER-THAN-CODE` 事例として検証のうえ採録する。

| 宣言 | 内容 | 分類 | code evidence |
| --- | --- | --- | --- |
| `contracts/openapi/openapi-v1.yaml:7-8` | "Increment decomposition/planning is intentionally not a public owner operation; it remains a loop scheduler responsibility."（Increment分解／計画はowner操作ではなくloop schedulerの責務である） | WIDER-THAN-CODE | `internal/application/service.go:888` は `callerActor(ctx, RoleOwner, RoleScheduler)`、`internal/application/service.go:1023` も `callerActor(ctx, RoleOwner, RoleScheduler)`。すなわち `Service.Plan` と `Service.Prepare` はどちらも `RoleOwner` を受理する。宣言は「ownerの操作ではない」と述べているが、codeはowner callerを拒否しない。なお両methodにHTTP routeは無いため（§4）、この差は現時点でtransportからは踏めない |

§8のRequirement遷移表の測定（既測定事例のもう一方）は、`docs/architecture/domain-model.md:268`
の `needs-input → active` と同 `:271` の `evaluating → active` を、それぞれ§8の
L268cとL271cの行として `WIDER-THAN-CODE` で記録した。`internal/domain/model.go:485-489`
の `RequirementStart` が許可するのは `ready`／`recovering`／`waiting` の3 statusだけである。
同じ走査で、文書だけが許す遷移がさらに3件見つかった（L268b `needs-input → framing`、
L269 `paused → 直前の安全な非終端状態`、L270c `recovering → needs-input`）。

## 19. この台帳が扱わないもの（境界）

以下は意図的に測定対象から外した。境界を知らずに本台帳を「全部」と読むと誤る。

1. **§2の13文書以外の宣言**。`docs/principles.md`、`AGENTS.md`、`docs/stable/**`、
   `docs/preview/**`、`docs/operations/**`、`docs/policies/**`、`contracts/**`
   （§18の1件を除く）、`README.md`、`.agents/v2/**` のREADME／HANDOFF／DEFERRED、
   `ci/components.json` の宣言は走査していない。特に `docs/stable/**` と
   `docs/preview/**` は利用者向け宣言の本体であり、そこにある主張の乖離は
   本台帳の分母に**含まれていない**。
2. **純粋な散文・根拠・背景**。採用理由、trade-off、比較表の判定理由、外部URLの引用は
   振る舞い主張ではないため数えていない。
3. **`remove`／`defer` 判定行**（`docs/architecture/legacy-inventory.md` §2〜§9）。
   「移植しない」という不在の宣言は代表4件のみ採録した。残り約40行は未採録である。
4. **非機能の量的宣言の実測**。「30秒以内」「2分以内」などの目標値は、gateの存在の有無だけを
   測り、実測時間は測っていない。同様に、無料枠の数値（Firestore／Cloud Scheduler／
   Secret Manager／Artifact Registry）が今日も正しいかは検証していない。
5. **testの中身の正しさ**。「そのtestが存在し、宣言された名前・定数・値を持つ」ところまでを
   測り、testが主張を本当に証明しているかは評価していない。`IMPLEMENTED` は
   「非testのcallerが到達できる」ことの測定であって、正しさの測定ではない。
6. **live実行**。Cloud Run／実Firestore／IAPへは接続していない。codex／opencodeへも
   接続していない（`codex login status` = "Not logged in"、`opencode auth list` =
   "0 credentials"、いずれも2026-08-26実測）。claudeについても実行していない。
   `DEFERRED-BY-DECLARATION` の11件は「宣言がそう述べている」ことの測定であり、
   その外部systemで実際に動くかどうかの測定ではない。
7. **到達可能性の粒度**。`IMPLEMENTED` は「非testの経路が存在する」ことであり、
   「稼働中に実際に呼ばれる」ことではない。逆に `UNREACHABLE` の判定は
   「非testのcallerが1件も無い」ことの測定であり、reflectionやinterface経由の
   間接呼び出しがあれば取りこぼしうる。使用したidiomは
   `grep -rn '<symbol>' --include='*.go' . | grep -v _test.go` と
   `go list -deps ./cmd/...` の2つだけである。
8. **複合行の内部件数**。126の複合行は構成項目を名指しているが、項目単位の件数は
   合計に反映していない。項目単位で数え直せば総数は1000件超になる。
9. **`.agents/v2/task-state/` との対応付け**。本台帳は既存taskへの紐付けを一切行っていない。
   taskを提案することも、task idを書くこともしていない。
10. **v1（`main` branch）の宣言と実装**。本台帳はv2 treeのみを測っている。
