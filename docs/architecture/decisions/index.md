# 再設計decision register

更新日: 2026-08-22

詳細文書を読まずに全体判断を確認するためのindex。仕様や理由の正本はリンク先とする。

| ID | Decision | 主な理由 | 正本 |
| --- | --- | --- | --- |
| D01 | プロダクトはPR生成器でなくApplicationを改善する自律product team | 利用者の関心は課題とStableの結果 | [Product](../../product/definition.md) |
| D02 | 人間入力のHowは仮説として再検討する | 課題と手段を分離する | [User spec](../../product/user-facing-spec.md) |
| D03 | BacklogはInstallationに一つ | 利用者視点の優先順位と共有資源配分を一元化 | [Domain](../domain-model.md) |
| D04 | Repositoryはcode／権限／workspace／release境界 | Queue境界と変更境界を分ける | [Domain](../domain-model.md) |
| D05 | 優先順位はLoopが継続判断し理由を残す | 通常のproduct team責務を自律化 | [User spec](../../product/user-facing-spec.md) |
| D06 | RequirementとIncrementを1対多にする | 1 Requirement 1 PRによる長期占有をなくす | [Domain](../domain-model.md) |
| D07 | Requirement、Execution、Release、Controlを別lifecycleにする | worker停止と課題取消などの誤同期を防ぐ | [Domain](../domain-model.md) |
| D08 | canonical current state＋Event＋Outboxを使う | 完全event sourcingを避けつつ外部作用を回復 | [Domain](../domain-model.md) |
| D09 | transaction Lease＋単調fencing tokenを使う | multi-machine claimと古いResultを排除 | [Domain](../domain-model.md) |
| D10 | stopはrequested／acknowledged／effective／verifiedを区別 | command受付だけの偽停止を防ぐ | [Domain](../domain-model.md) |
| D11 | Control PlaneとRunnerを分離する | cloud stateとlocal AI／workspace責務を隔離 | [Architecture](../overview.md) |
| D12 | Control Planeはmodular monolith | 初期の分散同期、費用、運用を減らす | [Architecture](../overview.md) |
| D13 | Runnerはoutbound接続だけを行う | 利用者machineへinbound portを開けない | [Architecture](../overview.md) |
| D14 | Work Packetはprovider-neutralな事実・判断・Evidenceだけ | 生conversation／credential依存をなくす | [Domain](../domain-model.md) |
| D15 | Codex、Claude、opencodeを初期対応する | 現行能力と利用者要件 | [User spec](../../product/user-facing-spec.md) |
| D16 | Provider固有契約はAdapterへ閉じ込める | error形式をdomain stateへ漏らさない | [Architecture](../overview.md) |
| D17 | Stable候補はPreviewで全capabilityを実物確認する | 外部toolをfakeだけで保証できない | [Release](../release-contract.md) |
| D18 | Provider依存変更は対象実Providerを使用する | 別Provider成功を代替にできない | [Release](../release-contract.md) |
| D19 | 実装・schema・config・docsを同じreleaseで昇格 | user-facing driftとrollback不整合を防ぐ | [Release](../release-contract.md) |
| D20 | Preview routingはRepository単位 | 大きくdogfoodしつつ他RepositoryをStableで維持 | [Release](../release-contract.md) |
| D21 | moduleはversion追加→切替→保持→削除 | rollbackとaffected verificationを局所化 | [Architecture](../overview.md) |
| D22 | dynamic Go pluginは使わない | binary compatibilityと署名riskを避ける | [Technology](../technology.md) |
| D23 | Control PlaneはCloud Run＋Firestore | single-tenant self-host、scale-to-zero、無料枠 | [Technology](../technology.md) |
| D24 | 定期処理はScheduler 1 job＋bounded reconcile | Cloud Run scale-to-zeroとdurable recoveryを両立 | [Technology](../technology.md) |
| D25 | owner／Runner入口はCloud Run direct IAP | public unauthenticated requestとload balancer費用を避ける | [Technology](../technology.md) |
| D26 | Provider credentialはRunner localにだけ置く | cloudへの秘密集中を避ける | [Architecture](../overview.md) |
| D27 | Control Plane／RunnerをGoで実装 | typed contract、static binary、process制御 | [Technology](../technology.md) |
| D28 | UIはserver-rendered HTML＋最小JS | dependencyとrelease surfaceを減らす | [Technology](../technology.md) |
| D29 | IaCはOpenTofu＋Google provider | user projectへ再現可能にself-host | [Technology](../technology.md) |
| D30 | GitHub Issue受付・同期を廃止し専用UIを使う | token、drift、複数状態表現を削除 | [Inventory](../legacy-inventory.md) |
| D31 | GitHub PRは任意のintegration手段 | 人間reviewとRequirement粒度から切り離す | [Inventory](../legacy-inventory.md) |
| D32 | legacyとはone-time importだけ行う | 二つのcanonical storeの長期同期を作らない | [Migration](../roadmap.md) |
| D33 | stop／recoveryをProvider拡張より先に実装 | 制御不能を再発させない | [Migration](../roadmap.md) |
| D34 | Stable／Preview利用者文書を常時maintainする | 利用者が現在仕様を読めるようにする | [Docs](../documentation.md) |
| D35 | 大原則だけはLoopが自己変更できない | 自律変更の最小境界を保つ | [Principles](../../principles.md) |
| D36 | CIはcomponent／contract差分の影響閉包だけを実行しhash evidenceを再利用 | feedback短縮と境界品質を両立 | [Validation](../validation.md) |
| D37 | Stable昇格前のPreview全capability実証は選択的CIと分離して常に行う | 外部統合と境界定義の漏れを検出 | [Validation](../validation.md) |
| D38 | v2はmain由来の通常branchを白紙化し、checkpointを直接積んで最後に一括merge | ゼロベース開発と追跡可能な置換を両立 | [Migration](../roadmap.md) |
| D39 | agent引き継ぎの正本はversion管理されたtask ledger／work packet／evidence | token枯渇やagent交代から再開可能にする | [Migration](../roadmap.md) |
| D40 | Solは重要判断、Terraは詳細設計、Lunaは限定実装を担い、永続packetでhandoffする | 難易度に応じてmodel費用を配分しcontext汚染を防ぐ | [Orchestration](../orchestration.md) |

## Revisit trigger

次の場合はdecisionを黙って変更せず、新ADRで再評価する。

- Firestoreのtransaction／query modelが実測throughputまたはinvariantを満たさない
- Cloud無料quota内でRelease Contract全実証を継続できない
- modular monolithのmodule間failureがStable／Preview隔離を破る
- Repository単位routingでcross-repository Requirementを実用的に処理できない
- IAP programmatic accessがRunner enrollment／長期運用に過剰な摩擦を生む
- Go／Linux sandboxでCodex、Claude、opencodeの必要機能を共通隔離できない
- single-user以外の利用者要件が明示的に発生する
- 選択的CIの誤った非選択がPreview障害を反復して生む
- v2長期branchのdriftが統合不能になる
