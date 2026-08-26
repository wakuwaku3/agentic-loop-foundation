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
実測の結果、証跡idを持つcapabilityは1件だけです。

## 既知の問題

実測で判明した問題を、実装が無いもの（escalation）と不足しているもの（shortfall）に
分けて記載します。いずれもこの実測で名前が付いたもので、以後は個別Requirementで
扱います。

- E22-1（解消済みの記録）: Repository登録route、Repository集約の登録command、
  GitとforgeのclientはすべてHEADに存在します。V2-022発行時点の「tree全体に
  Git/GitHub clientが無い」という測定は偽です。cap-repository-registrationが
  未実証な理由は実装の不在ではなく、本taskの副作用範囲がforgeとremoteを
  除外していることです。
- E22-2（部分的に解消済みの記録）: needs-inputの質問を記録するcommandとrouteと
  詳細fieldは存在します。残る決定的な欠落は、実surfaceで作られたRequirementが
  needs-inputへ遷移できるstatusに到達できないことです。`captured`を離れる
  commandは`start-framing`だけであり、これを発行するapplication commandが
  存在しないため、質問の表示も回答による再開も観測できません。
- E22-3（部分的に解消済みの記録）: `internal/release`はapplication層から
  importされ、read-onlyの`GET /v1/release/state`が存在します。残る欠落は配線で、
  `cmd/control-plane`がReleaseObserverを組み立てないため稼働processは503を返し、
  自分が提供しているversionを報告できません。
- E22-4（解消済みの記録）: Provider registryのread route `GET /v1/providers`は
  存在し、3 Providerの接続状態・上限source・割当先を報告します。
- E22-5（解消済みの記録）: schedulerはapplication層から配線され、
  `POST /v1/controls`は配分上限を受け取り、queueSummaryは配分・待機理由・枯渇を
  報告します。
- E22-6: 出荷される`cmd/runner`は`--fake`なしでは起動を拒否し、外部control plane
  への配線が無いと表示します。したがってRunner側protocolの実測は、実HTTPで
  protocolを話すtest processが行っており、Runner daemonについては何も主張できません。
- E22-7: `GET /v1/requirements`は`page_size`と`cursor`しか解釈しないため、
  Backlogを関連Repositoryで絞り込めません。
- E22-8: control read modelは対象Runner・process・lease・新規副作用可否を
  projectionとして持たず、owner consoleもそれを描画しません。
- E22-9: preview-local dogfoodのtest fileのimportは`api` componentが宣言する
  依存edgeを超えています。したがってselective CIは`internal/runner`・
  `internal/update`・`internal/release`の変更でこの実測を選択しません。この実測は
  commit毎のgateではなく、繰り返せるPreview workflowです。
- E22-10: owner consoleは文書routeを提供しません。`preview-local`ではownerが
  repositoryのworking treeを直接読むことになります。
- E22-11（新規、実測）: Firestoreに対してBacklogを2ページ目以降へ進められません。
  一覧が返すcursorをそのまま渡すとrouteは400を返します。原因はpage実装が
  document id順の走査に対してcollection pathを含む値をcursorとして渡していることで、
  clientがその値をdocument idとして扱い、collection pathを二重に組み立てます。

実測で確認した不足（capabilityの成功条件そのものではないもの）:

- Backlog（cap-backlog-visibility）: 宣言された利用者操作のうち「関連Repositoryで
  絞り込む」が未実装です（E22-7）。
- 利用者文書（cap-user-documentation）: 宣言する唯一の外部systemであるowner console
  から文書を参照できません（E22-10）。加えて、capability文書が`/v1/release/state`を
  owner可読と記しているのに稼働processは503を返すという、文書と実挙動の差異が
  存在します。
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

実測の結果、証跡idを持つcapabilityは cap-requirement-intake の1件だけです。
残る11件は証跡なしで、内訳は次のとおりです。

- 初回deploy gate（D1）に属するもの: cap-preview-operation、cap-stable-promotion、
  cap-loop-control、cap-loop-self-update。いずれも宣言する外部systemに
  Google Cloud Runを含むため、どれだけlocalの挙動を観測してもこのgradeでは
  充足しません。
- 後続milestoneに属するもの: cap-autonomous-resolution、cap-provider-operation、
  cap-shared-resource-allocation（Provider側）。いずれも3 Providerを宣言し、
  この機械ではclaudeだけが認証済みです。cap-shared-resource-allocationの
  複数Repository側も後続milestoneです。
- 実装または配線が無いもの: cap-human-input-request（needs-inputへ遷移できる
  statusに到達する経路が無い）、cap-backlog-visibility（E22-11）、
  cap-user-documentation（E22-10と文書・実挙動の差異）。
- 本taskの副作用範囲外のもの: cap-repository-registration。GitHubとGitを宣言し、
  この実測はforgeにもremoteにも接続していません。

このほかに、GCP Preview deployment、実Codex/opencode接続、複数の利用者管理
machineとRepositoryによる並列実行が引き続き不足しています。

capability baselineの正本は
[contracts/release-contract/foundation.json](../../contracts/release-contract/foundation.json)
であり、各capabilityの宣言とanchorは
[Preview capability一覧](capabilities.md) を参照してください。
