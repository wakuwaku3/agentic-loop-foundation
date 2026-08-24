# v2 task DAGとgate判定基準

更新日: 2026-08-24

## 1. 目的とスコープ

`.agents/v2/task-state/` はtask DAGの機械可読な正本であり、`internal/contracts`の
台帳testがその形式的整合性を検証する。しかしJSONそのものは「なぜこの依存になっているか」
「gateはどの根拠でpassedと判定するか」「実環境が使えないときどう表現するか」を語らない。
本文書はその意味付けを、会話やcontextに依存せず別のagentが再開できる形で記録する。

読む順序の指針: 正本（task-state JSON）が事実、本文書はその解釈。両者が食い違ったら
task-state側を信じ、本文書を更新する。

## 2. task一覧（DAG構成表）

`依存`列のtask_idは対応するtask-stateの`dependencies`と一致する。`副作用`列は
`none`（local閉域で完結し取り消し不要）、`cost`（実課金が発生し得る）、
`irreversible`（取り消せない外部操作を含む）、`credential-scope`（実credentialの
スコープ・分離を検証するため一時的に実credentialへ触れる）のいずれか。

| task_id | milestone | 実行主体 | 依存 | local/live | 副作用種別 |
| --- | --- | --- | --- | --- | --- |
| V2-008 | M0 | terra | V2-025 | local | none |
| V2-009 | M0 | terra | V2-025 | local | none |
| V2-006 | M0 gate | sol | V2-025, V2-008, V2-009, V2-040, V2-041 | local | none |
| V2-040 | M0 remediation | luna | V2-009, V2-041 | local | none |
| V2-041 | M0 remediation | luna | V2-008 | local | none |
| V2-042 | M0 remediation | luna | V2-041 | local | none |
| V2-043 | M1 remediation | luna | V2-011 | local | none |
| V2-044 | M3 remediation | terra | V2-016 | local | none |
| V2-045 | M3 remediation | terra | V2-016, V2-017 | local | none |
| V2-046 | M4 | luna | V2-017 | live (privileged container/VM) | none |
| V2-010 | M1 | luna | V2-006 | local | none |
| V2-011 | M1 gate | sol | V2-010 | local | none |
| V2-012 | M2設計 | terra | V2-006 | local | none |
| V2-013 | M2 | luna | V2-011, V2-012 | local | none |
| V2-014 | M2 | luna | V2-013 | live | cost |
| V2-015 | M2 gate | sol | V2-014 | local | none |
| V2-016 | M3 | luna | V2-011 | local | none |
| V2-017 | M3 | luna | V2-016, V2-015 | live | credential-scope |
| V2-018 | M3 gate | sol | V2-017, V2-045 | local | none |
| V2-019 | M4 | luna | V2-016 | local | none |
| V2-020 | M4 gate | sol | V2-019 | local | none |
| V2-021 | M5 | luna | V2-009, V2-011 | local | none |
| V2-022 | M5 | luna | V2-021, V2-017, V2-020, V2-015 | live | cost |
| V2-026 | M5 gate | sol | V2-022 | local | none |
| V2-027 | M6 | luna | V2-018 | local | none |
| V2-028 | M6 | luna | V2-027, V2-026 | live | credential-scope |
| V2-029 | M6 gate | sol | V2-028 | local | none |
| V2-030 | M7 | luna | V2-020 | local | none |
| V2-031 | M7 | luna | V2-030, V2-029 | live | cost |
| V2-032 | M7 gate | sol | V2-031 | local | none |
| V2-033 | M8設計 | terra | V2-026 | local | none |
| V2-034 | M8 | luna | V2-033 | local | none |
| V2-035 | M8 | luna | V2-034, V2-032 | live | cost |
| V2-036 | M8 gate | sol | V2-035 | local | none |
| V2-037 | M9 | luna | V2-036 | live (read-only) | credential-scope |
| V2-038 | M9 | luna | V2-037 | live | irreversible |
| V2-039 | M9 gate | sol | V2-038 | live | irreversible |

V2-010は枯渇したV2-007の後継である。retryではなくscope再計画として新規taskに
分離した（理由は6章）。V2-000〜V2-005は完了済みのbootstrap系列、V2-023/V2-024/V2-025は
決着済みのrepair chainであり、本表の対象外（両者とも別文書・別testの管轄）。

## 3. critical pathと並列可能な組

critical path（このtask列が全て順にcompleteしない限りM9 gateへ到達しない）:

```
V2-008/V2-009 → V2-006 → V2-010 → V2-011 → V2-013 → V2-014 → V2-015 →
V2-016 → V2-017 → V2-018 → V2-019 → V2-020 → V2-021 → V2-022 → V2-026 →
V2-027 → V2-028 → V2-029 → V2-030 → V2-031 → V2-032 → V2-033 → V2-034 →
V2-035 → V2-036 → V2-037 → V2-038 → V2-039
```

並列に着手できる組（依存が揃った時点でscheduler上の順序を問わない）:

- V2-012はV2-010と並列（両方ともV2-006だけに依存）
- V2-016、V2-019、V2-021はV2-011後（V2-021はV2-009にも依存）に並列
- V2-027はV2-018 gate後、V2-030はV2-020 gate後にそれぞれ並列

## 4. gate共通判定規則 G1〜G5

すべてのgate task（V2-006, V2-011, V2-015, V2-018, V2-020, V2-026, V2-029,
V2-032, V2-036, V2-039）は次を満たしてはじめてcompleteにできる。

- **G1**: 当該milestoneの全実装・設計taskがcomplete。1件でも未completeならgateは
  blockedのまま。
- **G2**: acceptanceに列挙したevidence entryが`evidence/index.json`に存在し、
  `result == "passed"`、`evidence_hash`がファイル本体のsha256と一致する。
- **G3**: 実環境必須の完了条件は、実target（GCP project ID、実CLI/Providerの
  version、machine識別子）を記録したlive evidenceでのみ充足する。local/emulator/fake
  evidenceでの代替をgateは明示的に拒否する。component名でlocalとliveを区別する
  （例: `infra-plan`はlocal、`gcp-live-apply`はlive）。
- **G4**: 依存にfailedがある場合はそのfailedタスクがsuperseded判定（6章）を
  満たしていること。未決着failedが1件でもあればgateはblockedのまま。
- **G5**: 同一candidateで結果が変動した（flakyにretryで上書きした）evidenceを
  含まない。

gate taskのcomplete transitionの`reason`に「`gate M<N> passed`」という文字列と、
判定根拠にしたevidence idの列を記録する。これがgate passedのcanonicalな記録であり、
専用fieldは追加しない。`reason`は自由文字列だが、この記録がなければ「なぜpassed
だったか」を後から再構成できないため、gate task実装者は必ずこの形式に従う。

## 5. milestone別の必須evidence一覧

各milestoneの実装・設計taskがどのcomponent evidenceを生成すべきかを示す。
`live`列が`必須`のものはG3によりlocal代替が拒否される。

| milestone | 対応task | 想定component | live必須か |
| --- | --- | --- | --- |
| M0 | V2-008, V2-009, V2-006 | 必須evidence: `ev-v2-025-contracts`, `ev-v2-008-candidate-aggregate`, `ev-v2-009-release-contract` | 不要（local閉域） |
| M1 | V2-010, V2-011 | domain（Safety Invariant 5件＋依存ゼロguard。validation.md §2のうちpriority comparison／飢餓防止はV2-030（M7）、retention eligibilityはV2-021（M5）とV2-034（M8）で成熟させる） | 不要 |
| M2設計 | V2-012 | infra（設計docのみ） | 不要 |
| M2 | V2-013 | infra-plan（emulator+tofu validate） | 不要 |
| M2live | V2-014 | gcp-live-apply（apply/verify/rollback/scale-to-zero/budget guard） | 必須 |
| M3 | V2-016 | runner（fake Providerでの縦断） | 不要 |
| M3live | V2-017 | provider-live-codex（Codexでの縦断・credential隔離） | 必須 |
| M4 | V2-019 | reconciler（制御・障害注入の収束） | 不要 |
| M5 | V2-021 | release（candidate/promotion/rollback/docs drift） | 不要 |
| M5live | V2-022 | release-live-dogfood（本Repositoryでの実運用） | 必須 |
| M6 | V2-027 | provider（Claude/opencode adapter、fixture契約） | 不要 |
| M6live | V2-028 | provider-live-multi（Codex/Claude/opencodeの実CLI検証） | 必須 |
| M7 | V2-030 | scheduler（単一machineでの多Runner/多Repository） | 不要 |
| M7live | V2-031 | scheduler-live-multi-machine（2台以上・2 Repository以上） | 必須 |
| M8設計 | V2-033 | update（設計docのみ） | 不要 |
| M8 | V2-034 | update（署名検証・Bootstrapper・migration・GC） | 不要 |
| M8live | V2-035 | update-live-self-deploy（新Loopの自己deploy・障害復旧・rollback） | 必須 |
| M9 | V2-037 | legacy-import-dry-run（read-only export・秘密scan） | 実データread-onlyでlive必須 |
| M9 | V2-038 | legacy-import-live-cutover（停止・drain・import・rollback rehearsal） | 必須 |
| M9 gate | V2-039 | cutover-record | 必須（cutover決定そのものがlive事実） |

M0行は他の行と異なり、component名ではなく`.agents/v2/evidence/index.json`上のevidence
idを直接列挙する。`contracts`はcomponent一覧に実在するが、V2-008が生成する
aggregate attestationのcomponent値は`candidate-aggregate`であり、これは
`ci/components.json`のcomponent一覧に存在しない（23個の実componentの評価結果を
束ねる合成attestationのためのcomponent値であり、単体のcomponentではない）。
component名で書くとindexの実際のcomponent値と食い違うため、M0 gate（V2-006）が
根拠として直接読むevidence idをそのまま書く。

`ev-v2-009-release-contract`はV2-040が発行済みである。この evidence は id を検証対象
（V2-009の成果物）に、`task_id`を実行主体（生産者V2-040）に置く。すでにcompleteした
taskへ新しいevidenceを帰属させると、V2-024をfailedにした
active-evidence-task-identity-mismatchを再発させるためである。以後のevidenceも
`task_id`は生産者に置く。

M1成果の「Priority Assessment」は判断記録としての存在で充足する。比較はdomain-model.mdの
Priority Assessment節の設計どおりschedulerがDecisionとして残す責務であり、domain packageに
比較関数を置かない。`internal/scheduler`にはscalar `Priority`によるscore付け、経過時間による
aging（飢餓防止）、failure storm隔離、bounded candidate snapshotが実装とtestを伴って既に存在する。
これを`PriorityAssessment`の多因子評価へ接続することはV2-030（M7）のscopeである。

validation.md §2はmilestone gateのchecklistではなくtest pyramidの到達目標であり、各項目は
対応するproduction振る舞いが成立したmilestoneで成熟する。存在しない振る舞いをgateで要求すると
検証の代わりに宣言を書くことになるため、M1では要求しない。

V2-045はM3 gateの直前に実行する。ci/components.jsonとinternal/ciの変更は全component
のevidence keyを無効化するため、gateがどのみち全component分のevidence-allを払う地点に
相乗りさせれば再evidence化の追加費用がゼロになる。逆に先に実行すると、進行中の
V2-016／V2-017のevidenceを着地前に無効化する。

## 6. 失敗タスクの処置とsuperseded規則

既存のfailed 3件（V2-007, V2-023, V2-024）は書き換えない。retry予算は再発行しない。
これは`internal/contracts/contracts_test.go`の`TestCanonicalTaskStateMigration`が
ファイル全体のsha256 digestで固定しており、1byteでも変えればtestがfailする。

superseded の定義（台帳test層2の項目8/10と同一）:

> failedタスクは、いずれかのcompleteタスクの`repair_of`または`input_refs`から
> そのtask_idが参照されていれば superseded とみなす。

適用状況（2026-08-24時点）:

- **V2-023 / V2-024**: V2-025（complete、repair chainの終端）により決着済み。
  V2-025の`repair_of`は`["V2-024"]`であり、V2-024自身がV2-023/V2-024を`repair_of`に
  持つ。この2件を直接`dependencies`に持つqueued/runningタスクは現時点で存在せず、
  台帳test層2の項目8はこの2件について発火しない（発火時も上記の連鎖で解決している）。
- **V2-007**: まだ superseded ではない。後継V2-010が complete し、その`output_refs`
  または`input_refs`にV2-007が現れた時点で superseded になる（V2-010の
  `input_refs`は現在`["V2-010","V2-006","V2-007"]`であり、V2-010がcompleteすれば
  この条件を満たす）。それまでは「未決着failed」として扱い、これはM1 gate
  （V2-011）をblockする正しい挙動である。V2-007を`dependencies`に持つ
  queued/runningタスクは現時点で存在しないため、台帳test層2の項目8は現状発火
  しないが、仮にV2-007へ直接依存するタスクが追加されればV2-010のcomplete前は
  superseded判定を満たさずtestがfailする（意図した設計）。

failedタスクを`dependencies`に持つcompleteタスクは存在してはならない（台帳test層2
項目10）。これは「失敗を踏み台にしてcompleteを詐称する」経路を塞ぐ。

## 7. 実環境が使えない場合の状態表現

task-stateの`status` enumには`waiting`や`needs-input`は無い。実環境（GCPプロジェクト、
Provider CLIなど）が使えずtaskを進められない場合は、既存enumの範囲内で次のように表現する。

- `status`を`blocked`にする。
- `block_reason`を`external-unavailable: <資源名>; needs: <人間に求める入力>`という
  形式にする（例: `external-unavailable: gcloud CLI and GCP project; needs: install
  gcloud and provision a project ID`）。
- `next_owner`を`sol`にする（人間の入力を仲介するのはSolの役割であり、terra/luna
  ではない）。
- **local evidenceによる代替登録は禁止**。fake/emulator/local実行の結果を
  「live相当」として記録しない（G3と同じ理由）。
- 解消時はSolが`blocked → queued`のtransitionを発行し、`reason`に解消内容
  （何が使えるようになったか）を記録する。

台帳test層2の項目7はこの形式を検証する: `block_reason`が`external-unavailable:`で
始まる場合は`next_owner == "sol"`であることのみを要求し、それ以外の
`block_reason`はカンマ区切りのtask_id列として依存タスクの存在と未完了を要求する。
両者は排他的に扱われる。

## 8. 台帳testの2層構造

`internal/contracts/contracts_test.go`の task-state 検証は2層に分離している。

- **層1（`TestCanonicalTaskStateMigration`）**: v1→v2移行という歴史的事実を固定する。
  移行時点で存在したID集合の下限（V2-000〜V2-009, V2-023〜V2-025の13件）が現在も
  含まれることだけを検証し、追加は許容する。終端failed 3件（V2-007, V2-023,
  V2-024）はファイル全体のsha256 digestで固定し、byte単位の不変を保証する。
  V2-025のtransitions列とrepair chain（V2-024/V2-025の`repair_of`）も維持する。
- **層2（`TestCanonicalTaskStateInvariants`）**: `.agents/v2/task-state/*.json`の
  全ファイルに対して、schema適合、filename/id/task_id整合、依存グラフの非循環
  （`dependencies`と`repair_of`の両方を辺とする）、transitions内部整合と
  top-level projectionの一致、retry budgetの算術、terminal状態での`next_owner`の
  null化、block_reasonの意味検証、release_eligibleの恒偽性、superseded規則、
  「failedへ依存するcompleteは存在しない」という10項目の構造的不変条件を検証する。
  `TestActiveEvidenceIndex`にはさらに、evidence index entryの`task_id`が実在する
  task-stateファイルを指すという11項目目の検証を追加している。

設計意図: 層1は「過去に何が起きたか」を凍結し、層2は「今のDAGの形が正しいか」を
検証する。DAGにtaskを追加してもID集合が変わるだけなら層1は失敗しない。追加された
taskが構造的不変条件を満たす限り層2も失敗しない。つまり**DAGを伸ばしても
test改修が不要**であり、これがV2-010〜V2-039の27件追加をtest変更なしで
受け入れられる理由である。逆に言えば、不変条件そのものを緩めたい変更（例:
`release_eligible`をtrueにできるようにする）は明示的にtestを変更しない限り
できない。これは意図した抵抗である。

## 9. 既知の外部制約（2026-08-24時点で実測済み）

このsession（v2-task/dag-registration worktree）で実際に確認した事実。実行時には
再観測し、変化していればこの記述を盲信しない。

- 開発機に`gcloud`が無く、`devbox.json`にも含まれない。GCPプロジェクトも未設定。
  → V2-014（M2 live）は現状の環境では`external-unavailable`見込み。
- `codex` CLIと`opencode` CLIが不在。
  → V2-017（M3 live, Codex）とV2-028（M6 live, 3 Provider）のうちopencode/Codex分は
  `external-unavailable`見込み。
- `claude` CLIは存在する。

これらはブロック要因の事前記録であり、担当taskが着手時に自ら再確認して
`blocked`＋`block_reason: external-unavailable:...`へ遷移させる。本文書は
先回りしてtask-stateを書き換えない。

## 10. checkpoint pushの手順と既知のdeploy workflow defect

### checkpointは1本ずつpushする

`scripts/verify-parent-ci.sh`は、diffが`.github/*`・`ci/*`・`scripts/*`・
`Makefile`・`devbox.json`・`devbox.lock`・`go.mod`・`go.sum`のいずれにも触れて
いない限り、親commit（`GITHUB_EVENT_BEFORE`、無ければ`HEAD^`）に対する
`v2 selective CI`のsuccessful runがGitHub上に存在することを要求し、無ければ失敗する。
そのため複数commitを連続でpushすると、後続commitの親（先にpushしたcommit）の
CIがまだ完了していない時点でCIが起動し、`parent <sha> has no successful v2
selective CI attestation`で失敗する。これはcode上の欠陥ではなく、pushの順序に
起因する見せかけの失敗（spurious failure）である。checkpointは1本ずつpushし、
親commitのCIが緑になったことを確認してから次のcheckpointをpushする。

### task worktreeはcommit後に再検証する

`gitleaks git`はcommit済みのgit履歴を走査し、working treeの未commit内容は
走査しない（本文書の運用上の帰結。詳細は
`docs/architecture/validation.md`の11章を参照）。したがって、commit前に
`make check`が緑であっても、それはcommitしようとしている内容がsecret scanを
通過したことの証明にはならない。task worktreeでの実装が終わったら、commitした
「後」にあらためて`devbox run --pure -- make check`（`secrets`を含む）を実行し、
commit済みの状態で緑であることを確認する。

### `.github/workflows/deploy.yml`の既知の欠陥

`.github/workflows/deploy.yml`は`jetify-com/devbox-install-action`を
`22c0f5500b14df4ea357ce673fbd4ced940ed6a1`というrevisionでpinしているが、この
revisionはupstream repositoryに存在しない。`.github/workflows/ci.yml`が使って
いる正しいpinは`22b0f5500b14df4ea357ce673fbd4ced940ed6a1`（2文字目が`b`）であり、
`deploy.yml`のみが`c`になっている。この結果、`deploy.yml`のjobは
`devbox-install-action`のresolveに失敗し、起動できない。この欠陥はこの文書が
記録するのみで、この文書の担当taskでは修正しない。修正はM2の設計・実装を担う
V2-012（設計）とV2-013（実装）の責務である。

## 11. 次の安全な1アクション

M0 gate（V2-006）はpassedで記録済みである。次はV2-010（M1、luna）とV2-012（M2設計、
terra）の並列着手であり、両者はV2-006だけに依存する。critical path上はV2-010が先で、
V2-010のcompleteが枯渇したV2-007をsupersededに転じさせてM1 gate（V2-011）のG4を解く。

gate対象taskのWork Orderには、次を必ずacceptanceとして書く。「testが通った」ではなく
「evidenceがindexに登録され台帳testが緑」を完了の定義とすること、evidenceの`task_id`を
実行主体に置くこと、evidenceをcommitした後に`make check`の緑を確認すること、そして
remediation taskであってもDesign PacketとWork Orderを`.agents/v2/packets/`へ残すこと
（V2-040はpacketなしで実行されており、この規約の例外として記録しておく）。
