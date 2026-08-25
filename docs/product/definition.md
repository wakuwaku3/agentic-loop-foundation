# Agentic Loop プロダクト定義

更新日: 2026-08-22

この文書はv2で何を作るか、その価値とscopeを定めるproduct requirementsの正本である。
現行実装の挙動を保証する文書ではない。内部方式と移行手順は各architecture文書を正本とする。
変更不能な最小原則は [Agentic Loop 大原則](../principles.md) に分離する。

## 1. 原点

作りたかったものは、個別のアプリケーション機能やPRを作るツールではなく、
**問題が解決され、アプリケーションのStable版へ反映されるまで、自律的に改善を
繰り返すループを構築・運用する仕組み**である。

利用者は実装工程を逐次指示するのではなく、解決してほしい課題とfeedbackに注力する。
Howが入力に含まれても解決策の仮説として扱い、背景にある課題を深掘りしてアプローチを再検討する。
ループはリポジトリに定義された最小限のルールと不変条件に従い、調査、変更、検証、
修正を反復して、要求が満たされた状態まで進める。

## 2. プロダクト仮説

リポジトリに少数の明確なルールと検証可能な完了条件を置けば、複数の自律workerが
要求キューから安全に仕事を取得し、互いに競合せず、問題解決を継続できる。

これにより人間は、実装の監督ではなく次の責務へ集中できる。

- 解決すべき課題、観測した事実、feedbackを伝える
- 不変条件と許容できないリスクを定める
- 機械には判断できない意思決定を行う
- ループの成果と問題解決能力を評価する

課題の分析、解決策の選択、要求間の優先順位付け、増分への分解、実装、検証、リリースは、
通常のプロダクト開発チームと同様にループの責務とする。

## 3. 提供価値

### 利用者にとっての価値

- 要望を登録すれば、通常は工程を監視・指示し続けなくても解決まで進む
- 何が待機中、実行中、停止中、完了済みかを把握できる
- 複数マシン・複数workerによって、独立した問題を並列に解決できる
- ループ自身の改善中も、既知の安定版による問題解決能力を維持できる
- ループの開始、停止、再開、更新を利用者が確実に制御できる
- control planeとデータを自分専用の環境へself-hostでき、他利用者の負荷や費用の影響を受けない

### リポジトリにとっての価値

- 要求、制約、変更、検証結果、成果の対応が残る
- 個々のagentやAIツールへ依存せず、同じルールでworkerを交換・追加できる
- 失敗を隠さず、再試行可能な状態として保持できる
- ループの運用品質を観測し、ループ自身を改善できる

## 4. 中核概念

### Requirement（要求）

利用者が解決してほしい課題、観測した問題、またはfeedback。利用者がHowを提示しても、
それを確定した実装要求とはみなさず、元の課題を深掘りして適切なアプローチを選び直す。

### Repository（問題空間）

Application、コード、ルール、不変条件、変更履歴の境界。1つのRequirementまたはIncrementは、
必要に応じて1つ以上のRepositoryに関連付けられる。

### Queue（待ち行列）

利用者のRequirementを一つのbacklogとして失わず、ループが判断した価値、緊急性、依存、risk、
cost、実行可能性に従ってworkerへ渡す仕組み。
キューはGitHub Issueそのものを意味しない。

Queueは利用者のInstallationに一つ置く。Repositoryはqueueの分割単位ではなく、Requirementを
解決する対象または変更境界である。AI providerの同時実行数、利用量、runner slotは、全Requirement
とRepositoryで共有される有限資源としてschedulerが配分する。

### Loop（問題解決ループ）

要求を理解し、調査、変更、検証、評価、修正を、完了または人間の判断が必要になるまで
反復する制御過程。

### Worker（実行主体）

キューから排他的に仕事を取得し、専用の作業空間でループを実行する交換可能な実行主体。
複数マシン上で複数workerを並列実行できる。

### Rule / Invariant（ルール／不変条件）

ループが守る最小限の制約。実装手順を逐一規定するものではなく、禁止事項、完了条件、
検証入口、承認が必要な境界を定める。

### Control Plane（制御面）

要求、状態遷移、lease、優先順位、依存、実行履歴を管理する。workerの作業そのものは行わない。
利用者が自分専用のinstanceとしてself-hostでき、他利用者とのmulti-tenant運用を前提としない。

### Execution Plane（実行面）

リポジトリ取得、作業空間作成、AI tool実行、変更、検証、成果物作成を担う。
runnerは利用者が管理する複数のマシンで動作し、将来は1台のrunnerが複数リポジトリの仕事を
扱える。runnerの有限なAI実行枠はリポジトリ間で共有する。

### Application（成果物）

ループが継続的に改善する対象。個々のPR、commit、調査報告はApplicationを改善するための
中間成果であり、プロダクトの最終成果物ではない。システム上の改善完了は、対象変更が
Previewで実証され、Stableへ昇格した時点とする。

## 5. 最小ユーザージャーニー

1. 利用者が解決したい課題またはfeedbackをRequirementとして登録する。
2. Requirementがキューへ永続化され、現在状態と待ち順が見える。
3. 利用可能なworkerが、実行可能なRequirementを排他的にclaimする。
4. workerがリポジトリのルールを読み、専用作業空間で問題解決ループを回す。
5. workerは進捗、判断、検証結果をcontrol planeへ報告する。
6. 人間の判断が不要なら、変更を小さく積み上げ、Previewで実証し、Stableへ昇格するまで
   自律的に進む。
7. 判断が必要なら、必要な問いと停止理由を提示し、回答後に同じRequirementを再開する。
8. 完了後、Requirement、変更履歴、検証、Preview実績、Stable昇格の対応を利用者が確認できる。

RequirementとPRは1対1である必要がない。ループは大きなRequirementを実行可能な増分へ分解し、
各増分を安全に統合しながら、元のRequirementの達成まで継続する。

## 6. 自律性の定義

自律的とは「何があっても処理を継続する」ことではない。次を満たすことを指す。

- 通常の調査・変更・検証・修正を、人間の逐次指示なしに反復する
- 一時的な外部障害やworker停止から、安全に再開できる
- 同じ副作用を誤って重複実行しない
- 判断権限を越える場合は、理由と選択肢を示して停止する
- 完了を自己申告だけで決めず、要求と不変条件を検証する
- 回復不能な失敗を無限再試行せず、観測可能な状態で保持する
- 利用者の停止指示へ確実に従い、停止完了を検証可能にする
- ループ自身の更新をPreviewで試し、Stableへ安全に昇格または切り戻す

## 7. 人間による統制

自律性は利用者の統制より優先されない。control plane、scheduler、runnerは、利用者が発行した
制御状態を共通の正本として扱う。

最低限、次の制御を提供する。

- **pause intake**: 新しいRequirementの受付またはqueue投入を止める。allow以外のmodeであるため、実行中の仕事に残る操作はcheckpointの保存だけになる
- **pause claim**: 新しい仕事のclaimを止める。実行中の仕事が進める「定義済みの境界」はcheckpointであり、結果の受理や外部副作用の確定はできない
- **graceful stop**: 新しい副作用を開始せず、checkpointを保存して安全に停止する
- **immediate stop**: 実行中processを終了し、leaseを失効させ、外部副作用が残っていないかを確認する
- **resume**: 保存したcheckpointと観測済みの外部状態から安全に再開する
- **cancel requirement**: 指定したRequirementの以後の処理を止める
- **emergency stop**: 全repository、全worker、全versionの新規実行を一括停止する

pauseはmode名が示す入口だけを閉じる制御ではない。allow以外のどのmodeでも、
durableな副作用を伴うauthoritative effectは一切構成されない
（effectの唯一の構成経路が、current effective modeがallowであることを要求する）。
したがってpause intake／pause claimの下でも、claim、実行結果の受理、
outboxのintegration／promotion／preview-deploy／external-effectはすべて拒否される。
実測は7 mode×8 kind＝56 cellのmatrixで確認しており、許可されるcellはallow=8、
pause-claim=2（Requirement受付とcheckpoint）、
pause-intake=1（checkpointのみ）、
graceful-stop=1（checkpointのみ）、
immediate stop／emergency stop／cancel=0、合計12である。
この閉包はM1 gateがinvariant 2として証明した設計であり、
pauseを「新規受付だけを止める弱い制御」と読むことはできない。
このcell数はinternal/applicationのTestStopModeByKindMatrixが検証している。

停止commandの受付だけを成功とみなさない。対象workerのack、process終了、lease解放または失効、
新規副作用の不在まで観測して初めて停止完了とする。到達不能なrunnerがある場合も、期限切れleaseと
fencing tokenにより、そのrunnerが後から古い権限で副作用を確定できないようにする。

破壊的・不可逆または許容不能な費用を伴う判断以外は、人間レビューを要求しない。通常の変更、
Preview評価、Stable昇格は自律的に実行する。

## 8. 並列性と資源配分の定義

- 複数マシンからworkerを追加できる
- 1台のマシン上でも複数workerを実行できる
- 1つのRequirementを同時に複数workerが変更しない
- 独立したRequirementは並列に進められる
- 競合可能性を事前に完全予測できなくても、検出、停止、再計画、統合が可能である
- worker数を増やしたとき、中央APIや外部サービスへのI/Oが無制限に増えない
- 単一backlogの中で、AI provider枠とrunner slotを価値、緊急性、risk、cost、依存に基づいて配分する
- 1つのrepositoryの大量要求、故障、再試行が、他の重要な課題の処理能力を占有し続けない

並列workerは目的ではなく、要求の待ち時間と解決時間を短くするための手段である。

## 9. 観測可能性

利用者が最低限知りたいのは、内部の細かな実行ログではなく次の情報である。

- どの要求が存在するか
- 現在どの段階か
- 実行中なら、進行しているか停滞しているか
- 待機中なら、なぜ待っているか
- 人間の入力が必要か
- 失敗したなら、自動回復するのか対応が必要か
- 完了したなら、何が変わり、どう検証されたか
- キュー全体の処理能力、待ち時間、失敗・手戻りの傾向
- 停止指示がどこまで伝播し、どのworkerまたは副作用が残っているか
- repository別のqueue状態と、共有worker／AI資源の使用・割当状況

## 10. Stable / Preview 仮説

外部CLI、GitHub、Git、OS process、AI providerとの接続は、決定的な自動テストだけでは
本番の故障形状を完全に再現できない。したがって品質保証をテストだけへ集中させず、
実運用で安全に学べるリリース境界をプロダクトに含める。

### Stable

- 既知の互換性と回復性を持つ、通常運用の既定版
- Previewの障害に影響されず起動できる
- Previewから自動的に破壊的な状態schema変更を受けない
- rollback後も進行中Requirementを安全に再開できる
- Previewが壊れた場合に、同じRequirementと共有状態を引き継いで復旧できる

### Preview

- このリポジトリ自身など、明示的に許可した大きな単位でdogfoodingする候補版
- 独立したworker poolまたは明示的なroutingでのみ使われる
- Stableと同じcontrol planeを使う場合、読み書きするschemaとlease protocolに互換性を持つ
- 障害、回復、外部境界の実測をStableと比較できる
- 問題があれば、新規claimを停止してStableへ戻せる

StableとPreviewは同じcanonical stateを共有する。このため、両versionは少なくとも次の契約を
共有し、Previewだけが理解できる状態へ一方的に移行してはならない。

- 状態schemaの読み書き互換性
- leaseとfencing tokenの意味
- stop、resume、cancelの制御protocol
- checkpointと外部副作用の識別方法
- eventおよびoutboxの冪等性

### 昇格の考え方

PreviewからStableへの昇格は「テストが通った」だけでは決めない。少なくとも次を根拠にする。

- 固定された自動テストと契約テストの成功
- 実サービス境界のsmoke検証
- 一定件数または一定期間のdogfooding
- Stable比での完了率、retry率、停止率、lead time
- rollbackおよび進行中Requirementの再開実績
- 未解決の重大な不具合がないこと

feature flagは分離を実現する手段の一つであり、それだけではStableを保護しない。
binary/runtimeの独立、状態schema互換、routing、即時停止、rollback可能性を合わせて設計する。

## 11. ループ自身の更新

ループはApplicationと同様に自己更新できる。ただし、更新対象のループと、更新を停止・復旧する
制御経路を同時に失ってはならない。

- Stable runtimeはPreview runtimeと独立して起動できる
- 更新は既存moduleのin-place変更を基本とせず、新module/versionを追加してroutingを切り替える
- 切替後も旧module/versionをrollback window中は保持する
- 不要になった旧moduleは、参照中のRequirementがなくrollback条件を満たした後に削除する
- control plane schemaはexpand → migrate → contractの段階で更新し、StableとPreviewの共存期間を持つ
- updater自身が失敗しても、既知のStable launcherまたは管理経路から復旧できる
- 自己更新もPreviewでdogfoodingし、昇格条件を満たしてからStableへ切り替える

この追加・切替・削除モデルは変更影響を新moduleとrouting境界へ局所化し、テスト対象と切り戻し範囲を
限定する。ただし共通schema、共有resource、横断的な不変条件を変更する場合は局所変更とはみなさない。

## 12. 初期スコープ

最初の縦切りでは次だけを成立させる。

- 利用者ごとの課題登録と永続的な単一backlog
- 状態と停止理由の表示
- 複数workerからの排他的claimとlease回復
- 専用作業空間での調査、変更、検証
- Applicationを改善する増分の統合（PRは利用可能な中間手段の一つ）
- 完了、再試行、人間入力待ちの明示的な状態遷移
- Stable / Previewの明示的なworker routingとStableへの復帰
- 検証可能なgraceful stop、immediate stop、emergency stop
- 単一backlog内の課題に対する共有worker／AI資源の配分
- Requirementの増分分解と、複数変更を通じた達成状態の追跡
- 新module/versionの追加、Preview切替、Stable昇格、rollback

複雑な依存関係、追加provider、Project表示、postmortem自動化などは、
中核の成立に本当に必要かを棚卸ししてから追加する。

## 13. 非目的

- GitHub IssueをRequirementの受付、backlog、canonical stateとして使うこと
- あらゆる開発手順を中央で細かく規定すること
- すべての外部障害を事前のfake E2Eだけで再現すること
- workerを常に起動しておくこと自体
- 内部イベントをすべて利用者へ表示すること
- 最初から現行機能を完全移植すること
- 要求とPRを1対1に固定すること
- SaaSとして複数の利用者を1つのcontrol planeへ同居させること
- すべての変更に人間レビューを要求すること
- サーバー（Installation）をまたぐ重複調停。lease／fencingが防ぐ二重ownerは一つのControl Planeの内側だけであり、リポジトリを共有する複数のサーバー間の調停は人の運用と外部システム（例: GitHub Project）が担う
- 複数人でのタスク管理。Control Planeはowner 1人が使う。認証（IAP＋owner単独）は本人確認のために維持する

## 14. 成功指標

- 人間の介入なしに完了したRequirementの割合
- Requirementの登録から完了までのlead time
- queue waitと実処理時間の分離
- Requirementあたりの試行回数と再計画回数
- 外部同期不整合による失敗件数
- Preview障害時にStableで処理能力を回復するまでの時間
- rollback後に失われた、または手動修復が必要になったRequirement数
- worker追加に対するthroughputと外部I/Oの増加率
- 変更から原因特定まで、修正から安全な昇格までの時間
- stop要求から全対象の停止を検証できるまでの時間
- RequirementあたりのPR数ではなく、Stableへ到達した有効な増分数と問題解決率
- repository間の公平性と、AI／runner共有枠の飢餓発生件数
- 自己更新失敗からStableへ復旧するまでの時間

## 15. 確定したプロダクト判断

- 最終成果物はPRではなく、ループによって改善されるApplicationである
- システム上の完了はPreviewからStableへの昇格とする
- 人間レビューは通常不要である
- リポジトリの大原則以外はループ自身が変更できる
- 大原則は、費用上限など現実的で変更してはならない最小限の制約にする
- queueは利用者のInstallationに一つ置き、Requirementを必要なRepositoryへ関連付ける
- Requirementの優先順位は、価値、緊急性、risk、cost、依存を踏まえてループが判断する
- runnerは利用者管理の複数マシンで動作する
- control planeは利用者自身が専用instanceとしてself-hostできる
- StableとPreviewはcanonical stateを共有する
- Previewは個別の小さなflagではなく、大きな対象単位で選択する
- Control Planeは利用者自身のGoogle Cloud projectへCloud Run + Firestoreで配備する
- Requirementの受付とbacklog表示には専用UIを提供し、GitHub Issue連携は提供しない
- 初期対応AI providerはCodex、Claude、opencodeとする
- Preview routingはRepository単位とする
- Stable候補は全ユーザー向け機能をPreview実環境で確認する
- Provider依存機能は影響する実Providerを使用するまでStableへ昇格しない
- PreviewとStableの利用者文書をversion管理し、実装と同じreleaseで昇格する

詳細な仕様判断は [ユーザー向けプロダクト仕様](user-facing-spec.md)、内部protocolは
[Domain model](../architecture/domain-model.md)と[論理architecture](../architecture/overview.md)、技術選定は
[技術選定](../architecture/technology.md)を正本とする。

## 16. 継続的な設計評価軸

今後、新旧の機能、CLI command、状態、文書、testを評価するときは次へ分類する。

1. 上記の中核価値に不可欠
2. 安全性・回復性のため不可欠
3. GitHub正本または現行アーキテクチャの補正としてだけ必要
4. 有用だが初期版には不要
5. 利用者向け仕様が不明
6. 廃止候補
