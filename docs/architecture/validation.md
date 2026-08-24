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
- daemon restartとlocal journal reconciliation（journalのrecoveryとidempotent appendを検証する
  `TestJournalRecoveryAndIdempotentAppend`、corrupt lineの拒否を検証する
  `TestJournalRejectsCorruptCompleteLine`、process groupへの実SIGKILL後の耐久性を検証する
  `TestJournalSurvivesRealSIGKILLOfProcessGroup`）
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

secret scanのallowlistを追加・変更するときは、同じ形（同じpattern）のsecretをallowlist対象外のpathに置いても検出されることをpositive controlとして実測し、そのbefore/afterをevidenceに残す。allowlistの条件を並べただけでは既定がORになり、意図より広く抑制される。

`.agents/v2/` 配下のgeneric-api-key allowlistは、canonical schemaが要求する台帳の参照情報のうち二つの形だけを対象にする：task-state（`input_hash`／`output_hash`）とevidence（`evidence_key`）が要求する64桁hexの参照digest、および`ev-`/`wo-`/`dp-`/`pb-`/`ts-`で始まる参照識別子（evidence／work-order／design-packet／problem-brief／task-stateのid）。後者を許可する必要があるのは実測された理由による：generic-api-keyはkeyword（例えば`secret`）の直後に続くquoted valueを捕捉するところ、evidence id `ev-v2-041-secrets-gate`はkeyword部分文字列`secret`を含むため、`input_refs`配列で隣接する別の参照id（`ev-v2-025-contracts`）があたかもsecret値であるかのように誤って捕捉される。

`gitleaks git`はcheckout中のbranchのHEADだけでなくrepositoryの全refを走査するため、検証用のprobe commitを作ったbranchが残っている限りgateは赤のままになる。probeを含むbranchは統合時にsquashしてから削除し、task branchはsquash統合後に削除する。

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
component の evidence key は、`make check`（`format` を含む）が緑になった最終 tree で読む。formatting のみの後続 commit でも tracked file の内容が変わるため component の key は動き、先に読んだ key はその後の tree を attest しない。

### aggregate attestation と identity 注入

`cmd/ci-plan` は `--task-id` / `--correlation-id` を受け取り、evidence record にそのまま記録する。値が空または空白のみのときは既定値（`task_id=V2-000`, `correlation_id=local-component-evidence`）へ fallback し、`task_id` は `^V2-[0-9]{3}$` を満たさない限り record を1件も書かずに失敗する。identity は環境変数ではなく argv で注入する。`devbox run --pure` は継承した環境変数を落とすため、共通入口 `devbox run --pure -- make <target>` を経由しても確実に届く経路は make 変数（argv 展開）だけである。

全 component の evidence を一括生成／確認するために次の target を使う。

- `make evidence-all EVIDENCE_TASK_ID=<id> EVIDENCE_CORRELATION_ID=<corr>` — manifest の全 component を選択し（`--all`）、各 component の検証入口を実行して `build/evidence` に record を書く。
- `make candidate` — `build/evidence` にある record が現在の evidence key と一致し `passed` であることを全 component について確認する。
- `make evidence-keys` — 各 component の現在の evidence key を `--all --keys` で算出して JSON で出力する。検証の実行は行わない。

`build/evidence` は `.gitignore` 済みの ephemeral な developer feedback であり、commit しない。evidence key は tracked file の内容と working tree の状態から決まるため、生成前に `rm -rf build/evidence` して clean な staged tree で実行する。

候補 commit の閉包を記録として残す場合は aggregate attestation を作るが、単一 commit では自己整合しない（attestation は自分自身を含む tree の evidence key を記録するため、記録対象に記録物自身が含まれる不動点になり、原理的に一致させられない）。そのため 2-commit protocol を用いる。

1. commit A: コード・test・Makefile・docs の変更のみを含む commit を作る。
2. commit A の sha を `base_commit` として、`make evidence-all` と `make evidence-keys` の結果から aggregate attestation を作成する。
3. commit B: attestation とその index への登録のみを含む commit を作る（コード変更は含めない）。
4. commit B の分だけ `.agents/**` の内容が変わるため、`task-ledger` component の evidence だけが古くなる。`task-ledger` の evidence 1件だけを再発行する。
5. commit B を検証する際は `make candidate` だけでなく `make check` を実行して green（`make candidate` は `{"candidate":true}`）を確認する。`gitleaks git` は commit 済み履歴を走査する仕組みであり、未 commit の working tree の内容は走査対象に入らない。したがって attestation ファイルを commit する前に `make check`（`secrets` を含む）を実行しても、その attestation 自身に含まれる 64-hex の evidence key が secret scan を通過したことにはならない。commit B を作った後にあらためて `make check` を実行して初めて、その内容が commit 済み履歴として scan されたと言える。

### Full-system gate との責務分離

選択的 CI は開発 feedback を短縮する gate である。Preview での全 user capability exercise、対象 Provider の実接続確認、Stable 昇格判定は差分にかかわらず実行する。したがって、境界定義の誤りがそのまま Stable に到達しない。

### CI 自身の検証

代表的な差分 fixture に対し、期待する影響閉包と実際の matrix が一致することを test する。依存 DAG と file ownership manifest の変更も通常の validation 対象とし、影響範囲を狭める変更ほど強い根拠を要求する。
