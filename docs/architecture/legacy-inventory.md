# 現行機能の棚卸しと再設計での扱い

更新日: 2026-08-22

## 1. 判定

| 判定 | 意味 |
| --- | --- |
| `retain` | プロダクト価値を保ち、domain modelから再実装する |
| `redesign` | 必要な能力だが、現行の状態・境界・操作方式は引き継がない |
| `defer` | 初期vertical sliceには入れず、中核成立後に追加する |
| `remove` | 現行architectureを補正するだけなので移植しない |

コードを移植するかではなく、利用者に必要な能力を判定する。現行のBash function、Label、marker、
file形式、CLI commandとの互換は原則として要求しない。

## 2. 要求受付・Backlog

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| GitHub Issueを要求の正本にする | `remove` | 専用UIとcanonical storeへ置換する |
| `submit-requirement`による自然言語受付 | `redesign` | UIから課題・観測・feedbackを登録する。Howは仮説として扱う |
| `category:*` label | `redesign` | 固定category順ではなく、価値・緊急性・risk・cost・依存を継続評価する |
| 数値priority marker／`priority` command | `remove` | 利用者はqueue順を指定せず、ループが優先順位と理由を管理する |
| Issue作成時刻によるFIFO | `redesign` | 飢餓時間は判断材料の一つにするが、FIFOを主規則にしない |
| 親子Issueによる要求分解 | `redesign` | RequirementからIncrementへのdomain関係として保持する |
| sub-issue／Blocked by依存 | `redesign` | Requirement／Increment dependencyとしてcanonical storeへ保持する |
| stale Issueの自動close | `remove` | 課題は経過時間だけで消さず、再評価、統合、取消を明示する |
| duplicate／superseded／merged disposition | `retain` | Backlog内のRequirement関係として扱い、人間またはループが根拠を記録する |
| `@body-file`誤用検出 | `remove` | GitHub本文搬送がなくなるため不要 |
| Issue commentからの回答検出 | `remove` | UI上のQuestion／Answerをcanonical stateへ直接保存する |

## 3. Scheduling・並列実行

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| repositoryごとのSupervisor | `remove` | Installation全体のschedulerと、独立したRunner Agentへ分離する |
| hostごとの`max_workers` | `redesign` | Installation、Runner、Repository、Provider Poolの階層的capacityにする |
| GitHub commentによるclaim競合調停 | `remove` | database transactionによるLease取得へ置換する |
| lease heartbeat／期限切れ回収 | `retain` | fencing token付きLeaseとして中核domainへ置く |
| scope markerによる競合予測 | `redesign` | IncrementのResource Claimと実diff観測へ置換する |
| unknown scopeの全体直列化 | `redesign` | 調査段階は並列可、変更開始前にResource Claimを具体化する |
| structural／environment conflict | `retain` | pathだけでなくschema、deploy target、secret、外部resourceをclaim対象にする |
| repository間の公平性 | `redesign` | 単一Backlogで価値と飢餓時間を評価し、resource classごとに配分する |
| provider pool fallback | `retain` | Provider AdapterとProvider Pool capacityへ分離する |
| pool exhaustion marker | `redesign` | Provider Accountのcapacity observationとcooldownとして保存する |
| API rate limit reserve | `redesign` | 各adapterのresource budgetへ一般化する |

## 4. Worker・問題解決

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| plan／execの固定2段階 | `remove` | Analyze、Implement、Verify、Releaseをcommandとして反復し、固定AI call数を持たない |
| flagship plan／cheap exec model | `redesign` | Work type、risk、残予算からProvider／modelをschedulerが選ぶ |
| bounded replan | `retain` | Retry Budgetと異なるapproachの履歴として扱う |
| provider最終行marker | `remove` | Provider Adapterが構造化されたRun Resultへ正規化する |
| provider固有error判定 | `retain` | Adapter内のcontractと実Provider Preview検証に閉じ込める |
| Worktree分離 | `retain` | Workspace Managerの責務として維持する |
| 1 Issue 1 branch／PR | `remove` | Increment単位のworkspaceとintegration strategyへ置換する |
| providerへ全工程を1 turnで委任 | `remove` | Orchestratorが短いdurable commandへ分け、待機中はRunnerを占有しない |
| worktree／branch／PR観測から再開 | `retain` | Work Packet、Checkpoint、Artifact Observationとして一般化する |
| dirty worktree引継ぎ | `redesign` | checkpoint可能なworkspace snapshotとして明示する |
| handoff comment | `remove` | provider-neutral Work Packetをcanonical storeへ保持する |
| 同一Requirementの複数Increment | `retain` | 1対多の中核modelとし、Requirement達成まで継続する |

## 5. Control・停止・回復

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| `start`／`stop` | `redesign` | Installation、Repository、Requirement、Runner、channelを対象にするControl Intentへ置換する |
| stop時のworker drain | `retain` | requested／acknowledged／effective／verifiedの段階を持たせる |
| `pause`／`abort`／`resume` | `redesign` | pause、cancel、resumeをRequirement lifecycleとExecution lifecycleで分離する |
| process group TERM→KILL | `retain` | Runner Process Supervisorのlocal責務として維持する |
| worker timeout | `redesign` | command deadline、progress deadline、Lease expiryを別々に扱う |
| orphan worker検出 | `retain` | Lease fencingとlocal process reconciliationで構造化する |
| Supervisor自動再起動 | `redesign` | Control Planeはmanaged service、Runner AgentはOS serviceとして個別に回復する |
| parked Issue | `redesign` | 自動回復不能でもRequirementを終端にせず`needs-input`へ遷移する |
| stop file／PID file | `remove` | local実装detailとして新Runner Agentが必要なら内部で使用するが契約にしない |

## 6. Verification・Release

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| 共通full check | `retain` | Release Contractの決定的検証の一部にする |
| affected check | `retain` | Increment中の高速feedbackに限定し、昇格gateには使わない |
| 巨大fake E2E | `redesign` | domain state test、adapter contract、少数journey、Preview実証へ分割する |
| flaky検知／quarantine | `retain` | 決定的検証の信頼性管理として維持する |
| host smoke | `retain` | Preview実証へ統合し、対象外部systemごとに実施する |
| PR checks／review／merge gate | `remove` | PRは任意のintegration adapter。人間reviewを完了条件にしない |
| traceability record | `retain` | Requirement→Increment→Artifact→Evidence→Releaseのdomain linkにする |
| preflight risk gate | `retain` | AI自己申告ではなく、Change ProposalとCapability／Resource定義から評価する |
| workload manifest／static scan | `retain` | Cost／Resource Budgetのchange-time評価へ統合する |
| main mergeをrelease扱い | `remove` | Preview実証後のStable promotionをreleaseとする |
| install／upgrade検証 | `redesign` | Loop VersionのPreview／Stable releaseとBootstrapperへ分ける |
| rollback | `retain` | Application、Loop、schema、docsを同一Release単位で戻す |

## 7. Observability・運用

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| `status` | `retain` | 専用UIのInstallation／Backlog／Runner／Release viewへ置換する |
| `tail`／events.log | `redesign` | structured EventとRunner local diagnostic logへ分ける |
| `doctor` | `retain` | Control Plane、Runner、Provider、Repository、Release Contractのhealthを診断する |
| `metrics` | `retain` | 単一Backlogのlead time、自律完了率、Preview失敗、rollback、資源配分を測る |
| GitHub Project view | `remove` | 専用UIへ置換する |
| Label／Project drift修復 | `remove` | canonical stateからGitHubへ投影しないため不要 |
| marker整合性診断 | `remove` | typed stateとschema validationへ置換する |
| progress stage／stall表示 | `retain` | Command progressとlast evidence timeとして扱う |
| usage comment | `redesign` | Provider Runのusage／duration recordとしてcanonical stateへ保存する |
| postmortem | `retain` | IncidentとAction Itemを同じBacklogへ登録し、再発防止のStable昇格まで追跡する |

## 8. Installation・Configuration・Documentation

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| repositoryへFoundation filesをcopy | `remove` | Control Plane、Runner Agent、Repository Contractを別artifactにする |
| install script | `redesign` | self-host Control Plane provisionerとRunner enrollmentに分ける |
| repository-local TOML | `redesign` | Repository ContractとInstallation／Runner設定を分離する |
| capability manifest | `retain` | version付きRepository Contractの一部として拡張する |
| environment definition／Devbox | `retain` | Repositoryが選ぶ再現可能Environment Contractとして扱う |
| provider CLI selection | `retain` | Runner CapabilityとProvider AccountをUIで登録する |
| upgrade migration scripts | `redesign` | versioned module、expand/migrate/contract、Releaseへ置換する |
| policy／ADR／operations docs | `redesign` | 大原則、ユーザー仕様、architecture、Release Contract、runbookへ責務分離する |
| Stable／Preview利用者文書 | `retain` | 実装と同じversioned release artifactとして新設する |

## 9. Security

| 現行能力 | 判定 | 再設計での扱い |
| --- | --- | --- |
| secret guard hook | `retain` | Workspace、commit、artifact、outbound payloadの複数境界へ適用する |
| gitleaks | `retain` | Repository scanの一層として維持する |
| prompt／logをIssueへ転載しない | `retain` | canonical stateにもraw secret-bearing contentを既定保存しない |
| provider credentialのhost依存 | `redesign` | Runner Secret Brokerがruntimeだけ注入し、Control Planeは秘密を保持しない |
| Codex sandbox／opencode残余risk | `redesign` | Provider Adapterとは別に、全Providerへ共通のWorkspace Sandbox契約を要求する |
| GitHub scope検査 | `redesign` | External Accountごとの最小権限とpreflightへ一般化する |

## 10. 棚卸しの結論

残すべき資産は、Bash実装やGitHub markerではなく、失敗から得た次の知識である。

- 排他的claimには期限付きLeaseとfencingが必要
- process生存と作業進行は別のsignalである
- 再開はagentの記憶でなく、workspaceと外部状態の観測から行う
- provider error、quota、model failure、課題失敗は別のfailure classである
- Retry、poll、log、API、worker、costはすべて有限でなければならない
- fakeだけでは外部toolの契約を保証できない
- 完了、停止、rollbackは外部から観測して検証する
- 状態、実行、release、controlのlifecycleを一つへ押し込めない

GitHub同期、Label状態機械、Project投影、comment marker protocol、1 Issue 1 PR、巨大な単一E2Eは、
この知識を実現する必須要素ではないため移植しない。
