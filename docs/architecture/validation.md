# 検証戦略

更新日: 2026-08-22

## 1. 品質保証model

```text
pure domain verification
  + store／API／Runner contract verification
  + adapter contract fixtures
  + bounded end-to-end journeys
  + Preview実環境の全capability exercise
  + rollback／stop／recovery exercise
        = Stable promotion evidence
```

自動testは早期feedbackと決定的なinvariant検証を担当する。外部systemの実契約、運用中のfailure、
利用者が観測するApplicationの成立はPreview実証が担当する。どちらか一方で代替しない。

## 2. Test pyramid

### Domain state tests

最も多く、最も速い層。database、filesystem、network、real clockを使わない。

- Requirement／Increment／Execution／Release／Controlの全許可遷移
- 禁止遷移とinvariant
- priority comparisonと飢餓防止
- effective Control Policy合成
- Retry Budget／circuit breaker
- Lease expiry／fencing
- promotion gate
- retention eligibility

table-driven testに加え、property-based／model-based testでcommand sequenceを生成し、次を常時確認する。

- expired tokenで状態が前進しない
- completed RequirementはStable Releaseを参照する
- stopped scopeで副作用permitが発行されない
- capability evidence欠損時にpromotableにならない
- credential fieldがdomain schemaに存在しない

### Application／store integration tests

Firestore emulatorを使い、transactionとqueryを実装と同じclient libraryで検証する。

- 同一Incrementへの同時claimは一つだけ成功する
- transaction callback再実行でEvent／Outboxが重複しない
- optimistic version conflict
- Control revision競合
- scheduler候補query後の再検証
- Outboxのat-least-once deliveryとidempotent completion
- schema expand／coexist／migrate／contract

emulatorの成功を実Firestoreの代替にはせず、Previewでcontentionと権限を確認する。

### API contract tests

- owner／Runner認証とscope
- idempotency key
- Stable／Preview Runner API互換
- request／response JSON Schema
- error taxonomy
- paginationとbounded response
- XSS／CSRF／content-type／size limit
- stop requested／effective／verifiedの表示

OpenAPIを契約の正本にし、server、Runner client、test fixture、reference docsを同じschemaから生成する。

### Runner component tests

- workspace path／symlink／permission validation
- process group TERM／KILL
- daemon restartとlocal SQLite reconciliation
- bounded log／redaction
- Secret Brokerが許可process以外へcredentialを渡さない
- sandboxからworkspace外へwriteできない
- atomic binary update／rollback
- offline時の新規claim／Result拒否

Linux namespace、git、process signalはcontainerまたはVM上のintegration testで実物を使う。

### Provider adapter contract tests

Codex、Claude、opencodeごとに、実CLIから採取した秘密を含まない最小contract fixtureを保持する。

- success
- explicit model error
- quota／usage exhaustion
- non-zero exit
- zero exit error envelope
- empty／malformed structured output
- timeout／cancel
- usageが取得可能／不可能
- CLI version incompatibility

fixture更新には、実CLI version、観測日時、変更理由、対応Preview exerciseを必要とする。

### Journey tests

巨大な一枚のshell E2Eを作らず、独立した少数journeyへ分ける。

1. 課題登録→framing→Increment→Preview→Stable→completed
2. Requirement分解と複数Increment
3. 2 Runner同時claim
4. Runner消失→fencing→別Runner再開
5. graceful／immediate／emergency stop
6. Provider quota→別Provider handoff／waiting
7. Preview regression→Stable rollback
8. Loop self-update→Preview failure→Stable復旧
9. 複数RepositoryをまたぐIncrement
10. secret疑い→stop→rotation→再実証

各journeyは専用fixtureとtimeoutを持ち、他journeyの実行順や残存processへ依存しない。

## 3. Preview capability exercise

Release Contractの各capabilityを実際のPreview revision、Firestore、Runner、Repository、Provider、deploy
targetで実行する。

- Stable候補の全capabilityを毎release実行する
- Provider依存変更は対象Provider、共通adapter変更は3 Providerすべてを実行する
- read-onlyで確認できない機能は、専用sandbox Repository／Applicationで可逆に実行する
- 課金、通知、外部公開、data変更は明示したtest resourceとhard budget内だけで行う
- Evidenceにはversion、target、operation ID、結果、時刻、duration、usageを残す
- raw prompt、credential、無制限outputは残さない

全機能実証の費用と時間を抑えるため、capabilityをAPI call単位へ細分化しすぎない。利用者に意味のある
journey単位にし、共通setupを再利用する。

## 4. Documentation verification

- Stable／Preview docsの全internal linkとversionを検査する
- UIの操作名、API schema、provider一覧、状態一覧をsource schemaと照合する
- code blockの安全なcommandを実行可能な範囲で検証する
- Preview docsにStableとの差分、既知問題、rollbackが存在することを検査する
- capabilityごとに利用者文書への参照があることをRelease Contractで検査する
- Stable docsがPreview文書への参照なしで現在機能を説明できることをreview agentが確認する

AIによる文書reviewは補助であり、schema照合とlink検査を代替しない。

## 5. Performance／resource verification

初期の設計上限を次とする。値は実測後にversion付きBudget Policyとして変更できる。

- Backlog: 10,000 Requirement
- active Increment: 100
- enrolled Runner: 20
- concurrent Execution: 20
- Repository: 20
- Event retention対象: 100,000 event
- 1 Work Packet: 256 KiB以下（large Artifactを埋め込まない）
- 1 reconcile tick: 100 item以下、30秒以内

検証する増加率:

- claimは全Event走査でなくindexed candidate queryに比例する
- statusは全raw log／Eventを読まない
- heartbeat writeはRunner／Execution数に線形で、Requirement数に比例しない
- provider health probeはRequirement数で増えない
- documentation exerciseはcapability数に線形で、test case内部数に比例しない

Firestore read／write数、Cloud Run CPU時間、Provider usageをtest resultへ含め、Budget regressionをgateする。

## 6. Security verification

- static secret scanと履歴scan
- generated fixtureを使うoutbound／log／Work Packet redaction test
- owner／Runner／Scheduler tokenのnegative authorization test
- revoked Runner keyとexpired session
- path traversal／symlink escape
- command schema injection
- Artifact digest／signature改竄
- IAP assertion issuer／audience／subject mismatch
- Firestore browser direct access denial
- dependency vulnerabilityとlicense scan
- IaC least privilege／public exposure drift

実credentialの値をtest reportへ表示しない。live testには専用の最小権限accountを使う。

## 7. Feedback time targets

| Gate | 目的 | 目標 |
| --- | --- | --- |
| format／lint／domain | 編集feedback | 30秒以内 |
| affected component | module変更feedback | 2分以内 |
| candidate aggregate | 現在の全component Evidence充足 | 2分以内 |
| Preview deploy＋全capability | Stable昇格 | workload依存、各capabilityに上限 |

candidate aggregate gateはtestを一括再実行せず、現在のcomponent hashに対応するEvidenceを検査する。
real Providerと実deployは省略せず、durableなPreview Release workflowとして別に完遂する。

## 8. Evidence freshness

- EvidenceはRelease Candidateのimmutable digestへ結び付ける
- binary、configuration、schema、docsのどれかが変われば影響Evidenceを無効にする
- Stable候補の全capabilityは同じcandidateに対して再実行する
- 外部Provider CLI versionがexercise後に変わった場合、昇格前に該当capabilityを再実行する
- clockだけで無条件失効させず、Release Contractが外部契約ごとのfreshness条件を定める

## 9. Flaky policy

- 同一candidate／環境で結果が変動したtestは成功へ昇格させない
- retryは診断用であり、最初のfailureを消さない
- quarantineは対象、期限、責任Requirement、代替coverageを必須とする
- Preview live exerciseのflakyはStable昇格を止める
- flaky修復もPreviewで再現不能と修復後安定の両方を確認する

## 10. Definition of Done

IncrementのDoneとRequirementのDoneを分ける。

Increment Done:

- Artifactがimmutableである
- Increment条件とaffected verificationを満たす
- Work PacketとEvidenceを更新した
- Preview候補へ統合可能である

Requirement Done:

- 全必要IncrementがRelease Candidateに含まれる
- Release Contract全capabilityがPreview実環境で成功した
- 対象実Providerを使用した
- stop／rollback／resumeを含む必要Evidenceがある
- Stable／Preview docsが一致する
- candidateがStableへ昇格し利用者が観測できる
- Problem Frameの課題が解決したことを評価した

## 11. 境界と差分に基づく選択的 CI

開発中の CI は repository 全体を毎回一括検証しない。component と contract の依存 DAG を正本として、変更差分から検証対象を機械的に算出する。

1. すべての tracked file を一つ以上の component に割り当てる。
2. component ごとに、公開 contract、直接依存、検証入口、生成物を宣言する。
3. 変更 file から起点 component を求める。
4. 公開 contract が変わった場合は、その consumer の推移閉包まで対象を広げる。
5. 対象 component の unit、component、contract test を共通入口から matrix 実行する。

未分類 file、循環依存、存在しない test target は manifest 検証の失敗とする。core domain、永続化 schema、security boundary、build environment、deployment/IaC の変更は影響範囲が広いため、原則として広い閉包になる。選択的 CI は検証省略の手段ではなく、明示された境界を品質保証の単位にする仕組みである。

### 検証 evidence の再利用

component ごとに、component source、公開 contract、依存 surface、test source／runner、lockfile／toolchain／Devbox 等の環境定義を hash 化した evidence key を作る。同じ key の成功 evidence は再利用できる。候補 commit の gate は、全 component について現在の key に対応する新鮮な evidence が存在することを aggregate attestation で確認する。

### Full-system gate との責務分離

選択的 CI は開発 feedback を短縮する gate である。Preview での全 user capability exercise、対象 Provider の実接続確認、Stable 昇格判定は差分にかかわらず実行する。したがって、境界定義の誤りがそのまま Stable に到達しない。

### CI 自身の検証

代表的な差分 fixture に対し、期待する影響閉包と実際の matrix が一致することを test する。依存 DAG と file ownership manifest の変更も通常の validation 対象とし、影響範囲を狭める変更ほど強い根拠を要求する。
