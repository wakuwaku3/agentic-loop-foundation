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
| V2-046 | M4 | luna | V2-016 | local（rootless namespace） | none |
| V2-047 | M3 remediation | luna | V2-016 | local | none |
| V2-048 | M3 remediation | luna | V2-047 | local | none |
| V2-049 | M5 remediation | luna | V2-011 | local | none |
| V2-050 | M3 remediation | luna | V2-047 | local | none |
| V2-051 | M2 | luna | V2-013 | live（preview-local） | none |
| V2-052 | M2 remediation | luna | V2-013 | local | none |
| V2-053 | 文書remediation | luna | V2-006 | local | none |
| V2-054 | D1 gate | sol | V2-014 | live（preview-gcp） | none |
| V2-055 | M3 remediation | sol | V2-050 | local | none |
| V2-056 | M3 remediation | sol | V2-047 | local | none |
| V2-057 | M4 remediation | sol | V2-019 | local | none |
| V2-058 | M4 remediation | luna | V2-019 | local | none |
| V2-059 | M4 remediation | terra | V2-019 | local | none |
| V2-060 | M2 remediation | sol | V2-051, V2-052 | live（preview-local） | none |
| V2-061 | 文書remediation | luna | V2-053 | local | none |
| V2-010 | M1 | luna | V2-006 | local | none |
| V2-011 | M1 gate | sol | V2-010 | local | none |
| V2-012 | M2設計 | terra | V2-006 | local | none |
| V2-013 | M2 | luna | V2-011, V2-012 | local | none |
| V2-014 | D1（初回deploy） | luna | V2-013, V2-015 | live（preview-gcp） | cost |
| V2-015 | M2 gate | sol | V2-013, V2-051, V2-053 | local | none |
| V2-016 | M3 | luna | V2-011 | local | none |
| V2-017 | M3 | luna | V2-016, V2-047, V2-048, V2-050 | live（preview-local） | credential-scope |
| V2-018 | M3 gate | sol | V2-017, V2-045, V2-047, V2-048, V2-050 | local | none |
| V2-019 | M4 | luna | V2-016 | local | none |
| V2-020 | M4 gate | sol | V2-019, V2-046 | local | none |
| V2-021 | M5 | luna | V2-009, V2-011 | local | none |
| V2-022 | M5 | luna | V2-021, V2-017, V2-020, V2-015, V2-049 | live | cost |
| V2-026 | M5 gate | sol | V2-022 | local | none |
| V2-027 | M6 | luna | V2-018 | local | none |
| V2-028 | M6 | luna | V2-027, V2-026 | live | credential-scope |
| V2-029 | M6 gate | sol | V2-028 | local | none |
| V2-030 | M7 | luna | V2-020 | local | none |
| V2-031 | M7 | luna | V2-030, V2-029 | live（container×2） | none |
| V2-032 | M7 gate | sol | V2-031 | local | none |
| V2-033 | M8設計 | terra | V2-026 | local | none |
| V2-034 | M8 | luna | V2-033 | local | none |
| V2-035 | M8 | luna | V2-034, V2-032 | live | cost |
| V2-036 | M8 gate | sol | V2-035 | local | none |
| V2-037 | M9 | luna | V2-036 | live (read-only) | credential-scope |
| V2-038 | M9 | luna | V2-037, V2-054 | live | irreversible |
| V2-039 | M9 gate | sol | V2-038 | live | irreversible |

V2-010は枯渇したV2-007の後継である。retryではなくscope再計画として新規taskに
分離した（理由は6章）。V2-000〜V2-005は完了済みのbootstrap系列、V2-023/V2-024/V2-025は
決着済みのrepair chainであり、本表の対象外（両者とも別文書・別testの管轄）。

## 3. critical pathと並列可能な組

critical path（このtask列が全て順にcompleteしない限りM9 gateへ到達しない）:

```
V2-008/V2-009 → V2-006 → V2-010 → V2-011 → V2-016 → V2-047 → V2-017 →
V2-018 → V2-019 → V2-020 → V2-027 → V2-028 → V2-029 → V2-030 → V2-031 →
V2-032 → V2-033 → V2-034 → V2-035 → V2-036 → V2-037 → V2-038 → V2-039
```

M2のlive枝（V2-013 → V2-014 → V2-015）はcritical pathから外し、M5 liveの
V2-022へ合流する並行枝として扱う。roadmap M3の完了条件はGCP resourceを1つも
名指ししておらず、GCPはM2の完了条件である。実Providerの縦切りをGCPの外部調達に
人質として取ると、roadmap §7のrisk reduction order（「1 Provider／1 Repositoryの
縦切りを通す」が「Preview／Stableと文書を完成させる」より前）を事実上逆転させる。
したがってV2-014とV2-015がexternal-unavailableの間もM3とM4は進行する。Preview
実環境が本当に必要になるV2-022（M5 live dogfood）でV2-015依存が正しく効き続ける。

並列に着手できる組（依存が揃った時点でscheduler上の順序を問わない）:

- V2-012はV2-010と並列（両方ともV2-006だけに依存）
- V2-016、V2-019、V2-021はV2-011後（V2-021はV2-009にも依存）に並列
- V2-047はV2-016後にV2-019／V2-021と並列（M3 liveの前提整備）
- V2-048はV2-047後、V2-017のowner承認待ちと並行して進められる
- V2-027はV2-018 gate後、V2-030はV2-020 gate後にそれぞれ並列

## 4. gate共通判定規則 G1〜G5

すべてのgate task（V2-006, V2-011, V2-015, V2-018, V2-020, V2-026, V2-029,
V2-032, V2-036, V2-039）は次を満たしてはじめてcompleteにできる。

- **G1**: 当該milestoneの全実装・設計taskがcomplete。1件でも未completeならgateは
  blockedのまま。
- **G2**: acceptanceに列挙したevidence entryが`evidence/index.json`に存在し、
  `result == "passed"`、`evidence_hash`がファイル本体のsha256と一致する。
- **G3（実環境の等級）**: 実環境必須の完了条件は、その条件が名指しする実targetを
  実際に使ったevidenceでのみ充足する。
  1. 条件が**外部resource**（GCP project、IAP、実Firestore、deploy経路、実Provider CLI、
     複数の独立実行実体、GitHub）を名指しする場合、そのresourceの識別子（project ID、
     CLI version、machine／container識別子）を記録したlive evidenceだけが充足し、
     local／emulator／fakeでの代替を拒否する。
  2. 条件が**Loop自身の観測可能な挙動**を対象とする場合、環境class `preview-local`
     （owner実機・実プロセス・実CLI・Firestore emulator）のevidenceが**その条件の
     実環境evidence**であり、「代替」とは呼ばない。この場合もevidenceは環境class、
     machine識別子、emulator名とversion、関与した実外部systemの識別子を含むこと。
  3. unit／fake／stub／契約testのevidenceは、いずれの等級でも実環境条件を充足しない。

  環境classはcomponent名で区別する（`infra-plan`はlocal、
  `control-plane-local-live`と`provider-live-claude`はpreview-local、
  `gcp-live-apply`はpreview-gcp）。
- **G7（skipはpassでない）**: gateはskipされたtestをpassとして数えない。live系と
  namespace系のevidenceは、testが実際に実行された事実（skip 0件のverdict、実行環境
  識別子: kernel version、unshareの成功、CLI versionなど）を含むこと。実行環境が
  その検証を実行できない場合はevidenceを発行せず、blockedと
  `external-unavailable:`で表現する。
- **G4**: 依存にfailedがある場合はそのfailedタスクがsuperseded判定（6章）を
  満たしていること。未決着failedが1件でもあればgateはblockedのまま。
- **G5**: 同一candidateで結果が変動した（flakyにretryで上書きした）evidenceを
  含まない。

gate taskのcomplete transitionの`reason`に「`gate M<N> passed`」という文字列と、
判定根拠にしたevidence idの列を記録する。これがgate passedのcanonicalな記録であり、
専用fieldは追加しない。`reason`は自由文字列だが、この記録がなければ「なぜpassed
だったか」を後から再構成できないため、gate task実装者は必ずこの形式に従う。

- **G6（evidenceの鮮度）**: gateが根拠に列挙した各componentについて、判定commitで
  `devbox run --pure -- make evidence-keys`が出力するcomponent keyと一致する
  `evidence_key`を持つ`result == "passed"`のentryが`evidence/index.json`に少なくとも
  1つ存在すること。鮮度を満たすentryは根拠のsemantic recordと同一である必要はなく、
  後続の再発行（例: V2-045の全component再発行）でよい。根拠に列挙されていない
  componentのkey staleはgateを妨げない。milestone完了条件のwitness testが根拠
  componentのcheck targetに含まれない場合（例: M4完了条件1のwitnessはrunnerのtestだが
  M4の根拠componentはreconcilerである）、gateは判定時に当該`make component-<id>`を
  再実行し、verdictをcomplete transitionのreasonへ記録する。この再実行はevidence
  entryを追加しない。

  evidence-keys の出力に無い**合成component**（例: `control-plane-local-live` のように
  複数componentの実証を1つのlive exerciseとして束ねたもの）の`evidence_key`は、その
  evidenceが宣言した算出方法（構成component keyの連結のsha256）を判定commitのkeyで
  再計算して一致することを鮮度とする。算出方法をevidenceに書いていない合成keyは
  鮮度を判定できないため、G6を満たさない。

  **改定（2026-08-25、M3 gate判定の失敗を受けて）**: 上のper-component規則は、
  `make check`が判定commitで再実行する検証に対しては正しいが、**live evidenceに対しては
  原理的に満たせない**。V2-045が`evidenceKey`を依存先Rootsの推移閉包へ変えた結果、
  runnerの閉包は11 componentに広がった。したがってリポジトリのほぼどこを変えても
  live recordのkeyが動き、live evidenceは「観測の後に何も着地しない」場合しか鮮度を
  満たせない。実際にM3 gateはこれで失敗した——coordinatorがV2-063のlive測定の後に
  runner roots内のdoc commentを1つ直しただけでkeyが動いた。規則がこの形のままだと、
  gateごとに無限のlive再実行か、その場しのぎの読み替えのどちらかを強制する。
  どちらもvalidation.md §11の趣旨に反する。

  そこでG6を**evidenceの種類で分ける**。

  - **G6a（`make check`が再実行する検証）**: そのevidenceのchecksが判定commitの
    `make check`で再実行されるなら、鮮度は`make check`が緑であることで満たされる。
    主張は毎commit再証明されているので、記録された`evidence_key`は出自の記録であって
    鮮度の門ではない。従来のper-component key一致は、根拠componentの再発行として
    引き続き実施してよいが、それ自体をgateの可否条件にはしない。
  - **G6b（`make check`が再実行しない検証＝live・一回限りの観測・deploy）**: 鮮度は
    次の2つを**実測**して満たす。(1) evidenceが観測commitを記録していること。
    (2) 観測commitから判定commitまでのdiffと、その exercise が実際にcompileし実行する
    file集合との**交差が空であること**。file集合は`go list -deps -test <exercising
    package>`とharnessとCLIのpathから機械的に得られる。交差が空でなければ**再観測を
    要する**。交差の測定結果は判定のevidenceに転記する。

  G6bは「commentだけだから良い」という論証を許すものではない。許すのは**測定**である。
  交差が空でないなら理由に関係なく再観測になり、交差が空ならkeyがどれだけ動いていても
  鮮度は満たされる。これはkey一致より、意味のある場合には厳しく（exercise pathへの
  変更は必ず再実行を強制する）、無関係な変更に対しては空虚でない。

  この改定は**規則の緩和ではなく置換**である。緩和として使われないための拘束を置く:
  交差の測定を省いたlive evidenceはG6bを満たさない（測定していない鮮度は主張できない）。
  また**live taskの完結規約**として、live exerciseのkeyを測った後にそのexerciseが
  compileするfileを同一taskで編集してはならない。V2-063はこれを守っていたが、
  coordinatorが統合時に破った。

  G2は「記録の完全性」（存在・passed・hash一致）を見るのに対し、G6は「記録と現在の
  treeの結合」を見る。M1 gateの判定でこの検査を実務として行っていたが成文化されて
  いなかったため、per-recordではなくper-componentとして明文化する。per-recordにすると
  V2-045の全component再発行が着地した時点で過去の全gate根拠が機械的にstaleになり、
  規則が自壊する。

## 5. milestone別の必須evidence一覧

各milestoneの実装・設計taskがどのcomponent evidenceを生成すべきかを示す。
`live`列が`必須`のものはG3によりlocal代替が拒否される。

| milestone | 対応task | 想定component | live必須か |
| --- | --- | --- | --- |
| M0 | V2-008, V2-009, V2-006 | 必須evidence: `ev-v2-025-contracts`, `ev-v2-008-candidate-aggregate`, `ev-v2-009-release-contract` | 不要（local閉域） |
| M1 | V2-010, V2-011 | domain（Safety Invariant 5件＋依存ゼロguard。validation.md §2のうちpriority comparison／飢餓防止はV2-030（M7）、retention eligibilityはV2-021（M5）とV2-034（M8）で成熟させる） | 不要 |
| M2設計 | V2-012 | infra（設計docのみ） | 不要 |
| M2 | V2-013, V2-051, V2-052 | infra-plan（emulator+tofu validate）と control-plane-local-live（実機の実プロセス＋emulator＋localhost） | preview-local必須 |
| D1（初回deploy gate） | V2-014, V2-054 | gcp-live-apply（IAP認証境界・scale-to-zero・実Firestoreの権限と競合・deploy経路） | preview-gcp必須 |
| M3 | V2-016 | runner（fake Providerでの縦断） | 不要 |
| M3live | V2-017 | provider-live-claude（代表Provider claudeでの縦断・credential隔離・暴走検知threshold 16 invocation／累計$10.00） | preview-local必須 |

M3 gate（V2-018）が受理する実物のerror証明はtransport failure（到達不能base URLで
誘発するもの）だけである。FailureModelとFailureQuotaの実物誘発は3 Provider全部に
ついてV2-028（M6 live）の管轄であり、V2-018はtransport failureをerror matrixの
claude分として計上しない。roadmap M3の完了条件はerror taxonomyの網羅を要求して
いない（要求しているのはM6である）。

代表Provider宣言の置き場所はcapability declaration set側で確定した（Sol裁定）。
foundation.jsonへ移すと同じ事実が二箇所に生まれ、release-contract.json schemaと
release.goのstructとbaseline fixtureと契約testの4点改修に対して新しい保証がゼロに
なる。将来foundation.jsonが同fieldを持った場合の一致はk4 assertionが既に守る。
| M4 | V2-019 | reconciler（制御・障害注入の収束） | 不要 |
| M5 | V2-021 | release（candidate/promotion/rollback/docs drift） | 不要 |
| M5live | V2-022 | release-live-dogfood（本Repositoryでの実運用） | 必須 |
| M6 | V2-027 | provider（Claude/opencode adapter、fixture契約） | 不要 |
| M6live | V2-028 | provider-live-multi（codex／claude／opencodeの実CLI検証） | 必須 |
| M7 | V2-030 | scheduler（単一machineでの多Runner/多Repository） | 不要 |
| M7live | V2-031 | scheduler-live-multi-runner（同一host上のcontainer 2つ＋注入clockずれ・2 Repository以上。実機2台の初回起動はsetup手順） | 必須 |
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

## 9. 既知の外部制約（2026-08-25時点で実測済み）

このsession（v2-task/dag-registration worktree）で実際に確認した事実。実行時には
再観測し、変化していればこの記述を盲信しない。

- **`claude` CLIは存在し使える**。`~/.local/bin/claude`、version 2.1.241、認証済み。
  `internal/provider`の`ClaudeAdapter.Build`が組むargv（`claude --print
  --output-format json --no-session-persistence` ＋ Work Packetをstdin）は実CLIと
  wire互換であることを実測した。CLIはJSONに`total_cost_usd`・`usage`・
  `duration_api_ms`・`session_id`を返すため、release-contract.md §7が要求する
  「Provider、version、capability、時刻、結果、消費量」をそのまま記録できる。
  **費用は実在し、1 invocationあたり約$0.08〜0.11が下限**である（CLI自身の
  system promptのcache作成が毎回乗るため、入力2 tokenの最小probeでも$0.077〜0.105）。
  → V2-017はexternal-unavailableではない。着手前にV2-047が用意するprovider
  preflight（16 invocation／累計$10.00、fail-closed）へownerの承認を記録する。
- **`codex` CLIと`opencode` CLIは導入済みで、非対話実行モードを持つ**（2026-08-25に
  coordinatorが実測）。`npm install -g @openai/codex@0.149.1` と
  `npm install -g opencode-ai@1.18.22` で
  `~/.nvm/versions/node/v24.18.0/bin/{codex,opencode}` に入り、
  `codex --version` → `codex-cli 0.149.1`、`opencode --version` → `1.18.22` が返る。
  非対話経路はそれぞれ `codex exec [PROMPT]`（stdinからprompt可）と
  `opencode run --format json`であり、Work Packetをstdinで渡してJSONを受ける
  `internal/provider`のadapter形と噛み合う。
  **残る壁は認証だけである**。`codex login status` → `Not logged in`、
  `opencode auth list` → `0 credentials`、`OPENAI_API_KEY`／`ANTHROPIC_API_KEY`は未設定。
  codexは`codex login`（browser OAuth）か`--with-api-key`／`--with-access-token`
  （stdinから秘密を読む）、opencodeは`opencode auth login`（対話選択＋OAuth）を要する。
  いずれもownerのsubscription identityそのものであり、**agentが代行できる作業ではない**
  （手作業の押し付けではなく、identityの境界である）。
  → V2-027（fixture相手のadapter完成）は認証不要で着手可能。V2-028（実3 provider）は
  着手時に`external-unavailable: codex and opencode provider credentials; needs: owner
  subscription login`へ遷移させる。`devbox.json`には入れない（Provider CLIは
  `devbox run --pure check`の実行pathに乗らず、lock変更は23 componentのevidence keyを
  全滅させるだけである）。再現性はenrollment時のCLI path／version観測、
  `docs/operations/`のprovider runbook、evidenceへのversion記録で担保する。
  **capability宣言のprovidersからcodex／opencodeを削って昇格を通してはならない**
  （契約の実質的弱体化になる）。
- 開発機に`gcloud`が無く、`devbox.json`にも含まれない。`~/.config/gcloud`には
  旧credential（`credentials.db`／`access_tokens.db`／`legacy_credentials`）と
  account・projectを持つ`config_default`が残っているが、**ADCもbinaryも無いため
  観測経路にならない**。既存の既定projectは空である保証がなく、
  「clean projectへapply」というM2完了条件を崩すため**M2 liveの既定候補にしない**。
  → V2-014は`external-unavailable`のまま。観測とapplyはGitHub Actions＋WIF経由とする。
- **2台目の実機が無い**（`java`も不在）。ただしM7の完了条件は「独立した2つの
  Runner実行実体」へ改定済みであり、V2-046がrootless user+mount namespaceの
  利用可能性を実測している。→ V2-031はcontainerではなくnamespaceで満たす経路を
  第一候補とし、それが不能と実測できた場合にのみ`external-unavailable`とする。

これらはブロック要因の事前記録であり、担当taskが着手時に自ら再確認して
`blocked`＋`block_reason: external-unavailable:...`へ遷移させる。本文書は
先回りしてtask-stateを書き換えない。

## 8.1 完了条件の読み方——安全性の主張と到達性の要求を混ぜない（M5 gateの失敗を受けて）

milestoneの完了条件には2種類ある。**安全性の主張**（「Xでないものは起きない」）と
**到達性の要求**（「Yが起きる」）である。**混ぜて読むと、自分のmilestoneでは原理的に
満たせない条件を作ってしまう。**

M5の完了条件「**全機能Evidenceを持つcandidateだけStableになる**」は安全性である。
「だけ」が全体を支配しており、要求しているのは「全機能Evidenceの無いcandidateは
Stableにならない」ことであって「candidateがStableになる」ことではない。
release-contract.md §3の「実行不能なcapabilityが一つでもあれば理由にかかわらず
Stableへ昇格しない」も同型で、**拒否することで満たされる規則**である。

最初のM5 gate判定はこれを到達性として読み、初回Stable昇格が起きていないことを
不成立の理由に数えた。その読みが誤りである根拠は3つある。

1. **判定者自身がguardを両面で証明していた。** 実treeは空evidenceのcapability idを
   名指しして拒否し、拒否集合が適格集合の補集合と正確に一致し、合成treeでは昇格して
   1 byte反転で拒否される。安全性の主張として要求されているものはこれで尽きている
2. **「全capability exercise」はroadmapの成果の欄にあり完了条件ではない。** 全capability
   成功を要求するのは§3で、それがgovernsするのはStable昇格である
3. **厳しい読みは構造的に充足不能である。** 3 capabilityはcodexとopencodeを要し
   M6 liveは**M5 gateの下流**にある。4 capabilityはCloud Runを要しD1待ちである。
   自分のmilestoneで原理的に満たせない条件は、条件ではなく誤読である。roadmap自身が
   「D1の4点はこのmilestoneの完了条件ではない」とcarve-outしていることもこれを支持する

**この読みは規則の緩和ではない。** 緩めた読みでも未証明のまま残るものを明示する:
**全機能Evidenceを持つcandidateが存在することは証明されていない。** それはM6 liveと
D1の管轄であり、M9の最終置換までに払われる。M5が証明したのは「存在しないものは
昇格しない」である。gateは以後この区別を明示的に述べること——安全性の条件については
「拒否が働くこと」を、到達性の条件については「起きたこと」を根拠にせよ。

**逆向きの拘束も置く。** 安全性として読める条件を到達性の証拠で通してはならない。
「昇格が起きたから安全性も満たされている」は成立しない（1回の昇格は拒否の網羅性を
何も語らない）。安全性は列挙か閉包で示すこと。

## 9.1 台帳artifactをcommitする前の手順と、禁止git操作の実体

**commitする前に、これからcommitするpathだけをgitleaksで走査すること。**
`gitleaks git` はcommit済み履歴しか読まないので、commit してから気付くと、
後続commitで直しても当該blobを持つcommitがrefから到達可能な限り`make check`は
永久に赤になる。

走査対象を絞ること。`devbox run --pure -- gitleaks dir . --config .gitleaks.toml`
は**使えない**。`make evidence-all` が吐く`build/evidence/**`（gitignore済み・
未追跡）を走査して26件を報告するため、追跡fileの状態が読めなくなる。台帳artifactを
書くときは `devbox run --pure -- gitleaks dir .agents --config .gitleaks.toml` の
ように、これから commit する subtree を名指しする。
この罠でこれまで6つのtaskが止まっている。捕まった形はいずれも secretではなく、
generic-api-keyのkeyword隣接だった: 64桁digestの直後の句読点、40桁のcommit sha、
そして`"I3_credential_isolation"`という subtest名の直後に来た`"I4_lease_continuity"`
という別のsubtest名。allowlistを識別子の形まで広げることはしない（それは実際の
token を通す穴になる）。**commit前にworking treeを走査するのが正しい手当てである。**

**禁止git操作の意図を明文化する。** これまでtaskへは
`git reset` / `git checkout --` / `git stash` / `--amend` を禁止と伝えてきたが、
**禁じている実体は「commitしていない作業内容を失う操作」**である。過去に
coordinatorがworktreeで`git reset --hard`を打って他agentの未commit編集を破壊した
ことが理由である。V2-075は上記のgitleaks罠に当たり、**作業treeの内容を1 byteも
失わない`git update-ref`**でbranch tipを観測commitへ戻して作り直し、そのことを
自ら報告した。これは禁止の意図に反しないので受理する。今後はcommand名の列挙では
なく次の形で伝える: 「commitしていない内容を失う操作をしてはならない。
history を作り直す必要が生じたら、失うものが無いことを示して報告せよ」。

## 9.2 発行済みWork Orderへの追記（dispatch時に必ず読む）

Work Orderは発行時点の測定に基づく。後続taskが着地するとその測定が偽になることが
あるので、**packetを書き換えるのではなくここに追記し、dispatchする者がpacketと
併せて渡す**。packetを書き換えると、実装者が読んだ指示書と記録された指示書が
食い違い、evidenceの`design_packet_ref`が指す内容が動いてしまう。

### wo-v2-068（scheduler配線）への追記2件

**追記1: A14の前提はV2-073の着地で偽になる。** A14は「`domain.Requirement`が
capture timestampを持たないこと、よって全候補が一律zeroの`CreatedAt`でSnapshotに
入り、aging項が一律になってproductionの実効順序はID tie-breakであること」を
測定事実として記録せよと書いている。V2-073が`CapturedAt`を追加するので、着地後は
この前提が偽になる。**実装者は記録を写さず自分で測り、食い違ったら実測を採ること。**
記録すべき限界は「timestampが無い」ことではなく、**V2-073がescalateした
「productionにはDを縛るものが何も無い」**の方である（Dはfloodのcaptureがwaiterより
何秒古いかで、V2-030の飢餓boundが真なのは実測でD in [-6500, +3499]秒）。

**追記2: Snapshot builderへの配線はV2-068の仕事である。** V2-073は
「欠損時のmapping規則をここで宣言し、適用はV2-068」と定めた。規則は
**capture時刻を持たない候補をsnapshotの`Now`にcapturedされたものとしてage 0で
並べる**こと。zero instantを渡すと約2000年分の秒がscoreを圧倒して欠損値が絶対優先に
なるので、これはbugの回避ではなく規則である。`internal/scheduler`はproduction codeに
`Snapshot`を組む場所を持たないので、builderの所有者はV2-068である。

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

## 11. 現在地と残作業の切り分け（2026-08-25）

passed済みgate: **M0・M1・M2・M4**。M3はliveの実行が着地しており、gate（V2-018）は
V2-045待ちである。

残作業を「人の介在が要るか」で切ると次の3群になる。

**群A: 介在不要（agentだけで完遂できる）**

| task | 内容 | 直前依存 |
|---|---|---|
| V2-045 | component依存DAGの実態是正と`evidenceKey`の依存面被覆 | 済 |
| V2-018 | M3 gate判定 | V2-045 |
| V2-022 | M5 dogfood（`preview-local`で12 capabilityを実行） | 済 |
| V2-026 | M5 gate判定 | V2-022 |
| V2-027 | Claude／opencode adapter・account pool・quota・breakerをfixture相手に完成 | V2-018 |
| V2-030 | M7 local（multi-Runner／multi-Repository scheduling） | 済 |
| V2-033 | M8設計（署名bundle・Bootstrapper・schema 4段移行・GC・rollback窓） | V2-026 |
| V2-034 | M8実装（local closure） | V2-033 |
| V2-058 | orphanの発生源を封じる（reclaim transaction内でterminal化） | 済 |
| V2-059 | pause modeの製品定義を実測へ合わせる | 済 |
| V2-062 | 使用量台帳が守る範囲の境界を明記 | 済 |

**群B: ownerのidentityが要る（作業の押し付けではなく境界）**

- **V2-028**（M6 live）。codexとopencodeのCLIは§9のとおり導入済みで非対話実行できるが、
  未認証である。`codex login` と `opencode auth login` はownerのsubscription identityを
  使う操作であり、agentは代行できない。認証が済めばV2-028は介在不要に転じる。
- **V2-029**（M6 gate）はV2-028待ち。**V2-031／V2-032**（M7 live／gate）はV2-029待ち。
  **V2-035／V2-036**（M8 live／gate）はV2-032待ち。つまり1回の認証がM6〜M8のliveを解く。

**群C: GCP projectが要る**

- **V2-014**（M2 live apply）と**V2-054**（D1初回deploy gate）。ownerが後回しと決めている。
- **V2-038／V2-039**（M9）はV2-054に依存するため、GCPが入るまで到達しない。

したがって「人の介在が不要なv2作業の完遂」の到達点は、**群Aの11 taskを全て完了させ、
M3とM5のgateをpassedにし、M6のfixture半分（V2-027）とM7のlocal半分（V2-030）と
M8のlocal実装（V2-034）まで積む**ことである。群B・群Cは待ちであることを台帳の
`block_reason`で明示する。

gate対象taskのWork Orderには、次を必ずacceptanceとして書く。「testが通った」ではなく
「evidenceがindexに登録され台帳testが緑」を完了の定義とすること、evidenceの`task_id`を
実行主体に置くこと、evidenceをcommitした後に`make check`の緑を確認すること、そして
remediation taskであってもDesign PacketとWork Orderを`.agents/v2/packets/`へ残すこと
（V2-040はpacketなしで実行されており、この規約の例外として記録しておく）。
