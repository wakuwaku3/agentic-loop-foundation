# Stableとの差分

## 差分

現在Stable Releaseはまだ存在しません。したがってv2候補の全機能が差分で、
`main`のv1を置換する根拠にはなりません。最初のStable昇格時に、この文書の
確定内容をStable文書へ移し、ここには次のPreview差分だけを残します。

2026-08-26に、このFoundation Repository自身をPreview対象として環境class
`preview-local`（owner実機・実process・実CLI・Firestore emulator）で12 capability
を1件ずつ実測しました（V2-022）。手順は
`docs/operations/release-live-dogfood.md` にあります。
この実測はGCPへのdeployを一切含まず、forgeにもremoteにも接続していません。
実測の結果、証跡idを持つcapabilityは1件だけでした。

2026-08-27に同じ環境classで再実測しました（V2-095）。今回はgh CLIとgit CLIの
実物へ接続し、対象Repositoryをread-onlyで読んだ有界Observationを提出しています。
証跡idを持つcapabilityは5件です。残る7件は宣言そのものからこの環境classで
実証できないと判定できるもので、4件は宣言する外部systemにGoogle Cloud Runを
含むため初回deploy gate（D1）に、3件はcodexとopencodeの両方を宣言し、この機械
ではclaudeだけが認証済みであるため後続milestoneに属します。この4/3/5の分割は
散文の主張ではなく、宣言fileから2つの独立した述語で判定するtestです。

## 既知の問題

実測で判明した問題を、実装が無いもの（escalation）と不足しているもの（shortfall）に
分けて記載します。いずれもこの実測で名前が付いたもので、以後は個別Requirementで
扱います。

- E22-1（解消済みの記録）: Repository登録route、Repository集約の登録command、
  GitとforgeのclientはすべてHEADに存在します。V2-022発行時点の「tree全体に
  Git/GitHub clientが無い」という測定は偽です。cap-repository-registrationが
  未実証な理由は実装の不在ではなく、本taskの副作用範囲がforgeとremoteを
  除外していることです。
- E22-2（解消済みの記録）: needs-inputの質問を記録するcommandとrouteと
  詳細fieldは存在します。V2-022が残る欠落として記録した「実surfaceで作られた
  Requirementがneeds-inputへ遷移できるstatusに到達できない」はすでに偽です。
  `captured`を離れるcommandは今も`start-framing`だけですが、それを発行する
  application commandとownerのrouteが存在するため、captured→framing→
  needs-input→readyを実base URLに対して歩けます。V2-095で実測しました。
- E22-3（解消済みの記録）: `internal/release`はapplication層から
  importされ、read-onlyの`GET /v1/release/state`が存在し、release source rootを
  明示的に与えられたprocessはこの読みに200で答え、自分が組み立てたversionを
  報告します。rootを与えられていないprocessは今も503を返します。既定rootは
  存在しません。既定rootを使えば、そのprocessが組み立てられていないversionを
  名乗ることになるからです。V2-095で、出荷構成（capability証跡を記録しない
  processなのでcandidate identityを与えない構成）ではこの配線が起動を拒否して
  いたことも実測し、observeできることとpromoteできることを分けて解消しました。
  未証跡candidateがpromotableになることはありません。
- E22-4（解消済みの記録）: Provider registryのread route `GET /v1/providers`は
  存在し、3 Providerの接続状態・上限source・割当先を報告します。
- E22-5（解消済みの記録）: schedulerはapplication層から配線され、
  `POST /v1/controls`は配分上限を受け取り、queueSummaryは配分・待機理由・枯渇を
  報告します。
- E22-6: 出荷される`cmd/runner`は`--fake`なしでは起動を拒否し、外部control plane
  への配線が無いと表示します。したがってRunner側protocolの実測は、実HTTPで
  protocolを話すtest processが行っており、Runner daemonについては何も主張できません。
- E22-7（解消済みの記録）: `GET /v1/requirements`は任意の`repository_id`を
  解釈します。絞り込みは書き込み一度きりのRequirement-Repository結び付きを通り、
  `page_size`と`cursor`と合成でき、未知のrepository idは空listを返します。
  V2-095で実測しました。
- E22-8: control read modelは対象Runner・process・lease・新規副作用可否を
  projectionとして持たず、owner consoleもそれを描画しません。
- E22-9: preview-local dogfoodのtest fileのimportは`api` componentが宣言する
  依存edgeを超えています。したがってselective CIは`internal/runner`・
  `internal/update`・`internal/release`の変更でこの実測を選択しません。この実測は
  commit毎のgateではなく、繰り返せるPreview workflowです。
- E22-10（解消済みの記録）: owner consoleは文書routeを2本提供します。1本は
  稼働channelと組み立てられたversionと参照できる文書の一覧を答え、もう1本は
  その文書自身を答えます。提供する集合は組み立てたrelease bundleの
  documentation role member集合そのもので、集合への所属判定で引きます。集合に
  無いpathはfileを1つも開く前に拒否されます。V2-095で実測しました。
- E22-11（解消済みの記録）: Firestoreに対してBacklogを最後のページまで進められます。
  一覧が返すcursorをそのまま渡す走査で、その実測が作成したRequirementを
  1件ずつ、重複も欠落もなく網羅できます。V2-022が記録した原因（page実装が
  document id順の走査に対してcollection pathを含む値をcursorとして渡し、client
  がcollection pathを二重に組み立てる）はV2-079で除かれています。

実測で確認した不足（capabilityの成功条件そのものではないもの）:

- Backlog（cap-backlog-visibility）: 宣言された確認項目8つすべてを一覧と
  queue summaryが合わせて持ち、関連Repositoryでの絞り込みも実装されています。
  残る不足は優先度の根拠の中身です。多要因の優先度評価は型も採点器も存在します
  が、配分snapshotがそれを供給しないため、rankingへ入っておらず、応答は
  `used_assessment` をfalseとして報告します。応答が根拠を偽らないという意味では
  これは不足の報告であり、隠された欠落ではありません。
- 利用者文書（cap-user-documentation）: 宣言する唯一の外部systemであるowner console
  から、稼働channelとversionに対応する文書を参照できます。残る限界は、この
  環境classがCloud Run上のdeploy済みprocessではないことだけです。
- 自律実行（cap-autonomous-resolution）: enrollmentからresultまでの実protocolは
  実HTTPで駆動でき実claude invocationも通りますが、Incrementを作るrouteが`/v1`に
  無いため、Runnerだけではclaimに到達できません。変更・検証・統合は行っていません。
- ループ制御（cap-loop-control）: 制御の受付から検証までの遷移とclaim停止・解除、
  実child process groupの終了は観測できますが、宣言が要求する対象Runner・process・
  lease・新規副作用可否の表示がありません（E22-8）。

## Stableへ戻す方法

Stable Releaseが存在しないため、戻す対象は`main`のv1です。
`contracts/release-contract/foundation.json`の`rollback.procedure`に記載の
手順（当該commitのgit revertとdevbox run --pure checkの再実行）に従ってください。

preview-local dogfoodで実測したLoop channelのrollbackは、これとは別のlayerです。
`internal/update`のchannel pointerを直前に検証済みのversionへ戻す操作であり、
Release ContractのStable Releaseへ戻す操作ではありません。Stable Releaseは
存在せず、昇格も行っていません。

## 昇格に不足している実証

2026-08-28の再検証で、このマシンから確認可能な境界は次のとおり実物で通過しました。

- `make check`: 全package、contract、secret scan、Firestore emulator、workflow pin。
- Codex 0.149.1 / OpenCode 1.18.18: 実CLI、session ID、usage、ledger settlement。
- GitHub / Git: 認証済みreadとbounded shallow clone。
- preview-local: 実Control Plane、Firestore emulator、別Runner process、実Codexで
  claimからprovider resultまで通過し、quota hard guardも実測。

Stable昇格に残る外部条件はGCP Preview deploymentでの全capability journeyです。
Providerへの直接変更を含まない今回の候補は、実Codexと実OpenCodeの確認により
Provider条件を満たしています。GCP確認を通すまではcontract statusを`stable`へ
書き換えません。

capability baselineの正本は
[contracts/release-contract/foundation.json](../../contracts/release-contract/foundation.json)
であり、各capabilityの宣言とanchorは
[Preview capability一覧](capabilities.md) を参照してください。
