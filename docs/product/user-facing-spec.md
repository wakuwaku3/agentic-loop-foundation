# Agentic Loop ユーザー向けプロダクト仕様

更新日: 2026-08-22

## 1. 目的

この文書は、Agentic Loopを利用する人間が操作・観測できる振る舞いを定義する。
control plane、database、GitHub、runner実装などの内部方式は定義しない。

大原則は [Agentic Loop 大原則](../principles.md)、プロダクトの背景と仮説は
[プロダクト定義](definition.md)を正本とする。

## 2. 利用者

初期版の利用者は、専用のAgentic Loop環境をself-hostし、1つ以上のrepositoryとrunner machineを
所有・管理する1人の人間である。multi-tenant SaaSの管理者や第三者利用者は想定しない。

利用者は実装工程ではなく、次に責任を持つ。

- 解決したい課題、観測した事実、feedbackを伝える
- repositoryの大原則とApplication固有の要求を定める
- 破壊的、不可逆、費用・権限上限を越える判断を承認または拒否する
- StableのApplicationを実際に利用し、次のfeedbackをRequirementとして返す
- 必要なときにループを開始、停止、再開、取消する

通常の計画、コード、PR、test、Preview評価、Stable昇格を逐次reviewする必要はない。

## 3. 利用者が扱う対象

### Installation

利用者専用のAgentic Loop環境。単一のRequirement Backlog、複数のRepositoryとRunner、共有AI資源を
束ねて管理する。

### Repository

改善対象Application、コード、ルール、権限、作業空間、変更履歴の境界。Requirement Queueの
所有者ではなく、RequirementまたはIncrementから関連付けられる解決対象である。

### Requirement

利用者が伝えた課題、観測した問題、またはfeedback。Howが含まれても確定した実装方法とはみなさず、
ループが背景の課題を深掘りしてアプローチを再検討する。PR単位も意味しない。

### Requirement Backlog

利用者の全Requirementを保持するInstallation単位の一つの待ち行列。Repositoryごとに分断しない。
ループは価値、緊急性、risk、cost、依存、実行可能性を継続的に評価し、優先順位を決め直す。

### Increment

Requirementを安全に前進させるため、ループが自律的に分解した統合可能な変更単位。
1 Requirementは0個以上のIncrementを持ち、Incrementは必要に応じて追加、再分解、置換される。

### Application Release

RepositoryのApplicationを利用可能にした版。PreviewとStableのchannelを持つ。

### Runner

利用者管理machine上でRequirementを処理する実行主体。1台で複数Repositoryを扱える。

### AI Provider

Runnerが問題の分析、計画、変更、検証に利用する交換可能なAI実行手段。初期対応providerは
Codex、Claude、opencodeとする。

## 4. 中核機能

### 4.1 Repositoryを登録する

利用者は改善対象RepositoryをInstallationへ登録できる。

登録後、少なくとも次を確認できる。

- Repositoryの識別情報
- ApplicationのPreview／Stable状態
- 適用中の大原則とRepository固有ルール
- Requirement Backlogの状態
- 利用可能なRunnerとAI資源
- ループが実行可能か、実行不能ならその理由

登録によって、既存Application、credential、外部resourceを無断で変更しない。

### 4.2 課題を登録する

利用者はInstallationへ自然言語で課題またはfeedbackを登録できる。少なくとも次を任意に伝えられる。

- 解決したい課題、困っていること、観測した事実
- 補足情報
- なぜ重要か、いつまでに必要かという事業・利用上の文脈
- Howの提案または仮説
- 変更してはならない制約

Howは課題を理解する手掛かりとして保存するが、その実装を約束しない。ループは課題を深掘りし、
必要なら別の解決策、変更不要という判断、または複数Repositoryにまたがるアプローチを選べる。

利用者は数値priorityやqueue順を決めない。ループが価値、緊急性、risk、cost、依存、feedbackから
優先順位を判断し、新しい情報や障害によって継続的に見直す。

登録が成功した場合、一意なRequirement IDと永続化された内容を表示する。
専用UIがcanonical stateへの登録完了を確認した時点を成功条件とする。

### 4.3 Backlogを見る

利用者はInstallationの一つのBacklogとしてRequirementを一覧し、関連Repositoryで絞り込める。

各Requirementについて、少なくとも次を確認できる。

- 要求内容
- 現在の利用者向け状態
- ループが判断した優先度と、その根拠になった価値、緊急性、risk、cost、依存
- 現在関連付けられているRepository
- 実行中、停滞中、自動回復中、人間対応待ちの区別
- 現在進めているIncrementと、元のRequirementに対する進捗
- Preview／Stableのどこまで反映されたか
- 次にシステムまたは人間が行うこと

内部eventや生logを読まなくても、対応が必要か判断できるようにする。

### 4.4 問題解決を自律実行する

実行可能なRequirementをRunnerがclaimし、ルールと有限資源の範囲で次を反復する。

1. 問題と現在のApplicationを調査する
2. 人間が提示したHowも含めて課題を深掘りし、優先順位とアプローチを再評価する
3. Requirementを必要なIncrementへ分解または再計画する
4. 専用作業空間で変更する
5. リスクに適した方法で検証する
6. Incrementを統合する
7. Previewで実環境の振る舞いを観測する
8. 問題があれば修正または切り戻す
9. Requirementの充足を評価する
10. 昇格条件を満たせばStableへ反映する

1回のRunner占有、AI session、commit、PRで完結する必要はない。中断、handoff、再開後も元の
RequirementとIncrementの関係を失わない。

### 4.5 人間の入力を求める

ループは、破壊的・不可逆な判断、費用・権限上限の変更、要求の本質的な曖昧さなど、権限内で
安全に選べない場合だけ人間へ質問する。

質問には次を含める。

- なぜ自律判断できないか
- 何を決める必要があるか
- 選択肢とそれぞれの影響
- 回答まで何が停止し、何が継続できるか

回答後は新しいRequirementを作らず、元のRequirementを継続する。

### 4.6 Previewを運用する

利用者はRepository単位でPreview利用対象を選べる。複数RepositoryにまたがるIncrementは、対象が
同じchannelと互換contractで処理可能な場合だけ実行し、利用者には対象範囲を明示する。

Previewについて次を確認できる。

- 稼働versionと対象範囲
- Stableとの差分
- 処理したRequirementとIncrement
- test、smoke、dogfooding、実測比較の結果
- 未解決の障害と自動回復状況
- Stableへ昇格可能か、未充足なら何が必要か

Previewの障害時は新規claimを止め、必要ならStableへroutingを戻し、進行中Requirementを再開する。

### 4.7 Stableへ昇格する

昇格条件を満たしたPreviewのApplicationまたはループversionは、人間reviewを待たずStableへ
自動昇格できる。

昇格完了には少なくとも次を必要とする。

- Stable候補が提供する全ユーザー向け機能が、Previewの実稼働環境で動作した
- 外部systemに依存する各機能が、その対象となる実物との接続で動作した
- Providerへの直接変更は影響するProviderをすべて実確認し、それ以外は利用可能なProviderのいずれか1つ以上で動作した
- 対象versionがStableとして利用されている
- Requirementと大原則の検証結果が記録されている
- 切り戻し先が利用可能である
- 進行中Requirementを継続できる
- 利用者が昇格内容と根拠を確認できる
- 同じversionのStable向け利用者文書が公開されている

fake、stub、契約test、別Providerでの成功は、実物確認までのfeedbackを速めるために使えるが、
実稼働確認の代替にはしない。必要な実物を費用・権限・利用上限の範囲で実行できない場合、
その候補はStableへ昇格せずPreviewに留める。

Requirementは、その要求を満たすApplicationがStableへ昇格した時点で完了する。

### 4.8 ループを制御する

利用者は対象範囲を指定して次を実行できる。

- Requirement受付の停止・再開
- 新規claimの停止・再開
- 安全な境界でのgraceful stop
- 実行中processを終了させるimmediate stop
- 全Repository・Runner・versionへのemergency stop
- Requirementのpause、resume、cancel

制御要求は「受付済み」と「完了」を区別する。完了時は対象Runner、process、lease、新規副作用の
可否を表示する。到達不能Runnerがある場合は、それも隠さず、そのRunnerの古い権限で結果を
確定できないことを示す。

### 4.9 ループ自身を更新する

ループは自分のmoduleまたはversionを追加し、Previewへ切り替え、dogfoodingし、Stableへ昇格できる。

- 既知のStable版はPreviewと独立して起動できる
- 問題発生時はStableへ戻し、同じcanonical stateから処理を続ける
- rollback可能期間中は旧版を保持する
- 旧版を利用する進行中処理がなく、復旧条件を満たした後だけ削除する
- updaterの失敗時も、独立した管理経路から復旧できる

### 4.10 Backlogへ共有資源を配分する

Requirement BacklogはInstallationに一つ保持する。RequirementやIncrementを必要なRepositoryへ
関連付け、Runner slotとAI providerの利用枠をInstallation全体の共有資源として扱う。

利用者は少なくとも次を確認・設定できる。

- Requirementごとの優先順位とその判断理由
- Repositoryごとの実行可否、権限、同時実行上限
- Installation全体とRepositoryごとの同時実行上限
- AI providerごとの利用可能状態と上限
- 現在の割当、待機理由、枯渇状態

一つのRepositoryの大量要求、障害、再試行によって、他Repositoryが無期限に処理されなくなることを
許さない。

### 4.11 AI Providerを利用・切替する

利用者はCodex、Claude、opencodeを利用可能なAI Providerとして接続できる。providerごとに少なくとも
次を確認できる。

- 接続済みか、Runner上で利用可能か
- 対応versionと認証の健全性（秘密の値は表示しない）
- 同時実行上限、利用上限、枯渇または一時停止状態
- Stable／PreviewのどのLoop Versionが対応しているか
- 現在どのRequirementへ割り当てられているか

ループは課題、stage、利用可能性、資源上限に応じてproviderを選ぶ。provider障害や枯渇時は、
互換なproviderへ安全にhandoffできる場合だけ切り替え、文脈、Requirement、検証結果を失わない。
切替によって品質や権限条件を満たせない場合は、黙って縮退せず待機理由を表示する。

providerは交換可能なadapterであり、provider固有の出力形式、error、usage制約をRequirementや
Applicationのdomain stateへ漏らさない。

### 4.12 利用者文書を読む

利用者は、実際に利用しているchannelとversionに対応したユーザー向け文書を読める。

- Stable文書は、現在のStableで利用できる機能と操作を完全に説明する
- Preview文書は、現在のPreview全体の利用方法に加え、Stableとの差分、既知の問題、切り戻し方法を説明する
- 機能追加、変更、廃止は、同じPreview versionの文書へ反映されるまでPreview完成とみなさない
- PreviewからStableへの昇格時は、実装、設定schema、migration、利用者文書を同じreleaseとして昇格する
- 古いStableをrollback用に保持する間は、そのversionに対応する文書も参照可能にする

ユーザー向け仕様は、内部module、function、test fixtureの説明ではなく、利用者が実行できる操作、
観測できる結果、制約、復旧方法を記述する。実装と文書の差異はreleaseを止める不具合として扱う。

## 5. 利用者向け状態

内部実装のstageをすべて公開状態にしない。Requirementの主要状態は次とする。

| 状態 | 利用者にとっての意味 |
| --- | --- |
| `queued` | 永続化済みで、実行可能になるのを待っている |
| `active` | ループが調査、変更、検証、Preview評価のいずれかを進めている |
| `waiting` | 外部資源、依存、時間条件などを待ち、自動的に再評価される |
| `needs-input` | 人間の判断がなければ安全に進められない |
| `paused` | 人間の指示で新しい処理を停止している |
| `recovering` | failureまたはRunner消失後、安全な再開を行っている |
| `completed` | 要求を満たすApplicationがStableへ昇格した |
| `cancelled` | 人間が以後の処理を取り消した |

`failed`を安易な終端状態にしない。自動回復不能なfailureは、理由と必要な判断を伴う
`needs-input`または人間が明示的に処理する別の非完了状態として保持する。

## 6. 独立したライフサイクル

次の状態を1つのlabelやstatusへ押し込めない。

| ライフサイクル | 問い |
| --- | --- |
| Requirement | 利用者の問題は解決したか |
| Increment | 次の安全な変更単位は統合・検証されたか |
| Application Release | Preview／Stableのどのversionが稼働しているか |
| Worker Execution | どのRunnerが何を実行し、leaseは有効か |
| Loop Version | Stable／Previewのどのループ実装が処理しているか |
| Control | intake、claim、実行、副作用は許可されているか |

各ライフサイクルは関連付けるが、同じ状態として同期しない。例えばWorkerが停止してもRequirementは
cancelledではなく、再開可能なqueued、paused、recoveringのいずれかになりうる。

## 7. 通知

利用者への通知は、次のように行動が必要な変化を優先する。

- 明示的な承認または回答が必要になった
- stopが完了した、または一部Runnerへ到達できない
- Previewが壊れStableへ戻った
- Stable昇格が完了しRequirementが満たされた
- 費用、権限、容量の上限に達して処理を止めた
- 自動回復できず問題解決能力が低下した

通常のpoll、heartbeat、内部stage遷移を逐次通知しない。

## 8. 初期版の完了条件

初期版は、少なくとも次の一連の操作を実環境で成立させる。

1. 利用者専用InstallationへRepositoryを1つ以上登録できる。
2. 専用UIまたは選択した受付adapterからRequirementを登録できる。
3. 一つのBacklogから、利用者管理の複数Runnerが排他的にRequirementを処理できる。
4. 1 Requirementを複数Incrementへ分けてApplicationを改善できる。
5. Previewで実証し、自動的にStableへ昇格してRequirementを完了できる。
6. Previewを意図的に壊し、Stableへ戻して進行中Requirementを再開できる。
7. graceful、immediate、emergency stopの完了を利用者が確認できる。
8. ループ自身のPreview更新、Stable昇格、rollbackを実行できる。
9. 2つ以上のRepositoryにまたがるRequirementへ共有Runner／AI資源を優先順位に従って配分できる。
10. 上記の過程で秘密漏洩、許可されない費用、Requirement消失、重複した副作用確定がない。
11. Codex、Claude、opencodeの接続状態と資源上限を表示し、少なくとも正常系を実行できる。
12. Stable候補の全ユーザー向け機能をPreview実環境で確認し、Provider依存機能を対象Providerで実行できる。
13. Preview文書とStableとの差分を公開し、昇格時に同じversionのStable文書を公開できる。

## 9. 確定した仕様判断

- Requirementの正規受付とBacklog表示には専用UIを使い、GitHub Issue adapterは提供しない
- Preview routingはRepository単位とし、このRepositoryを処理するLoop VersionとApplication Releaseを大きく切り替える
- 複数RepositoryにまたがるIncrementは、対象Repositoryが同じchannelで処理可能なときだけ実行する
- Applicationごとの全ユーザー向け機能と実稼働確認方法を、Repository管理のversion付きRelease Contractへ定義する
- Stable昇格ではRelease Contractの全機能をPreview実環境で確認し、影響する外部systemとProviderを実際に使用する
- code差分を伴わない調査は、利用者が読める結果がStableへ公開され、元の課題へ回答できた時点で完了する
- 運用修復は、対象ApplicationのStable環境で望ましい状態が観測され、再発検出または復旧方法がStable文書へ反映された時点で完了する
- schedulerは固定scoreだけでなく、価値、緊急性、risk、cost、依存、学習価値、飢餓時間を継続評価し、理由を説明する
- Requirementは特定Runnerへ固定せず、各Incrementには同時に1つの有効なownerだけを置く。checkpoint後は別RunnerまたはProviderへhandoffできる
- Provider間handoffには、課題、制約、関連Repository、現在のIncrement、判断と根拠、変更artifact、検証結果、未解決事項だけを含むprovider-neutralなWork Packetを使い、生conversationとcredentialを渡さない
- state schemaはexpand、共存、migrate、contractの順で変更し、StableとPreviewが共有状態を読める期間を保証する
- rollback windowは時間だけでなく、旧版を参照するRequirementがないこと、Stableによる復旧実証、対応文書の保持を満たすまで閉じない
- offline Runnerは既存checkpointまでのlocal作業だけを許可し、新しいclaim、共有状態更新、外部副作用の確定、Stable昇格にはcontrol planeとの接続を必要とする
