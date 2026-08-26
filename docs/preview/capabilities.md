# Preview capability一覧

更新日: 2026-08-26

このcapability一覧は、`contracts/release-contract/foundation.json` が宣言する
baseline capabilityへのanchorだけを提供する。各capabilityの利用者操作、観測結果、
実証方法、外部依存の正本は `contracts/release-contract/foundation-capabilities.json`
であり、この文書ではその内容を複製しない。

2026-08-26に、このFoundation Repository自身をPreview対象として
環境class `preview-local`（owner実機・実process・実CLI・Firestore emulator）で
12 capabilityを1件ずつ実測した（V2-022）。手順は
`docs/operations/release-live-dogfood.md` にある。
証跡idを持つのは、宣言する成功条件を全て観測でき、かつ宣言する外部依存の全systemへ
実接続できた2件だけである。残る10件は証跡なしであり、その理由（初回deploy gate D1、
後続milestone、または未実装のescalation）を各項に記す。GCP Preview deploymentは
まだ存在しない。

<a id="cap-repository-registration"></a>
## Repositoryを登録する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-repository-registration` 宣言である。登録route・一覧・詳細の表示はpreview-local dogfood（V2-022）で実測したが、宣言する外部systemのGitHubとGitへは接続していない（本taskの副作用範囲がforgeとremoteを除外する）。ループ実行可否はRunnerが提出するforge Observationからしか決まらないため未観測であり、証跡なし（gate規則G3-1）。

<a id="cap-requirement-intake"></a>
## 課題を登録する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-requirement-intake` 宣言である。preview-local dogfood（V2-022）で宣言する成功条件を全て観測し、宣言する外部system（owner UIとFirestore）へ実接続した。証跡idは `contracts/release-contract/foundation.json` にある。

<a id="cap-backlog-visibility"></a>
## Backlogを見る

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-backlog-visibility` 宣言である。preview-local dogfood（V2-022）で宣言する成功条件を全て観測し、宣言する外部system（owner UIとFirestore）へ実接続した。証跡idは `contracts/release-contract/foundation.json` にある。宣言された利用者操作のうち「関連Repositoryで絞り込む」だけは未実装であり、`GET /v1/requirements` は `page_size` と `cursor` しか解釈しない（実測済みの不足、escalation E22-7）。

<a id="cap-autonomous-resolution"></a>
## 問題解決を自律実行する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-autonomous-resolution` 宣言である。preview-local dogfood（V2-022）でenrollmentからclaim・start・renew・heartbeat・checkpoint・resultまでの実protocolを実HTTPで駆動し、実claude invocationを1回行ったが、宣言する成功条件のうち変更・検証・統合は行っていない（GitHubとGitへ接続していない）。宣言する3 Providerのうちclaudeだけを使ったため、証跡なし（残りはM6/V2-028）。

<a id="cap-human-input-request"></a>
## 人間の入力を求める

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-human-input-request` 宣言である。preview-local dogfood（V2-022）で実測したところ、質問を記録するcommandとrouteと詳細fieldは存在するが、実surfaceで作られたRequirementはneeds-inputへ遷移できるstatusに到達できない（`captured` を離れるcommandは `start-framing` だけで、これを発行するapplication commandが存在しない）。よって質問の表示も回答による再開も観測できず、証跡なし。

ownerが読めるようになったもの: needs-inputのRequirement詳細（`GET /v1/requirements/{requirement_id}`）に
質問が載る。載るのは、なぜ自律判断できないかを閉じた3値の理由class（破壊的・不可逆な判断／
費用・権限上限の変更／要求の本質的な曖昧さ）とその理由文、何を決める必要があるかの質問文、
選択肢とその選択肢ごとの影響（影響は必須であり、影響が空の選択肢は記録段階で拒否される）、
そして回答まで何が停止し何が継続できるかの2つの範囲listである。範囲listは閉じた語彙であり、
そのうち「このRequirementへの新規claim」と「保持中leaseの延長」の2値は実際の拒否で裏打ち
されている。owner consoleにも同じ内容を読む欄が付き、選択肢を選んで押したときにだけ回答を
送る。回答は`POST /v1/requirements/{requirement_id}:answer-input`（ownerのみ）で、選択肢idを
1つ指定する。回答すると新しいRequirementは作られず、同じRequirementがreadyへ戻る。
2回目の回答はdomainの遷移表自身が拒否する（readyはreadyコマンドの許可元ではない）。
質問の記録は`POST /v1/requirements/{requirement_id}:request-input`（runnerまたはscheduler。
ownerは拒否される）で、質問を記録しないRequirementは詳細でこのfieldごと欠落する。
status がneeds-inputでも記録が無ければ欠落したままであり、statusから質問文を合成することはしない。

この面が保証するもの／しないもの: 質問が開いている間、そのRequirementのIncrementへは
新規claimが発行されず、保持中のleaseも延長されない（`Claim`と`Renew`が拒否する）。
一方で、質問した瞬間に既存leaseが取り消されるわけではない。activeなleaseを早期にrevokeできる
domain遷移が存在しない（`ExpireLease`は満了前を拒否する）ため、既に保持されているleaseは
既存の`ExpiresAt`で失効し、以降は既存の期限切れlease経路が扱う。「待機中のRequirementは
claimを一切保持しない」とは言えないので、そうは書かない。

未配線の残余: 「質問すべきだと自動で判断する」経路は配線されていない。
`docs/architecture/failure-model.md`がneeds-inputへ送ると定めているpolicy-denied、
budget-exceeded、分類不能の失敗classからこのcommandを呼ぶ配線は存在せず、
この版が備えるのはcommand・route・詳細fieldという露出面だけである。したがって
cap-human-input-requestのuser journeyはこの版では実行されていない。

<a id="cap-preview-operation"></a>
## Previewを運用する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-preview-operation` 宣言である。宣言する外部systemにGoogle Cloud Runが含まれるため、このcapabilityの証跡は初回deploy gate（D1）に属する。preview-local dogfood（V2-022）では、稼働processが `GET /v1/release/state` に503 release_observer_not_configuredを返すことも実測した（`cmd/control-plane` がReleaseObserverを組み立てていない配線の残余）。証跡なし。

ownerが読めるようになったもの: owner consoleのRelease evidence欄と、ownerだけが読める
read-onlyのGET route `/v1/release/state` から、稼働processが組み立てられたPreview release
versionとその由来、release契約4節の8条件それぞれのmet／unmet／not-observable-hereと理由と
判定source、rollback先とrollback履歴、およびrollback先をまだ参照しているRequirementの
有界走査結果を読める。routeは「このprocess自身が記録したroute」であり、routeを記録して
いないprocessは推測値ではなく「no route recorded」と答える。

この面が観測しないもの: Cloud Runの稼働revision、deploy済みimage、deploy経路、IAP認証境界、
scale-to-zero、実Firestoreの権限と競合。これらは初回deploy gate（D1）の対象であり、応答の
`not_observed` に列挙して明示する。version判定にupdate channelのpointerは読まない。

未配線の残余: 障害時に新規claimを止めてStableへroutingを戻す自動応答は配線されていない。
この面はrollback先とrollback履歴を報告するだけで、routingを動かすrouteは存在しない
（promote／rollback／SetPreviewのrouteをこのversionは持たない）。

<a id="cap-stable-promotion"></a>
## Stableへ昇格する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-stable-promotion` 宣言である。宣言する外部systemにGoogle Cloud RunとGitHubが含まれるため、このcapabilityの証跡は初回deploy gate（D1）に属する。preview-local dogfood（V2-022）では昇格routeが存在しないことも実測した。証跡なし。

ownerが読めるようになったもの: 昇格に何が足りないかを、同じread-only routeから条件単位で
読める。昇格の権威は `internal/domain` のReleaseCandidateであり、この面はその拒否理由を
12のclassに分けて返すだけで、判定を作り直さない。空のcapability集合、6つのcandidate識別
fieldそれぞれの欠落、capability targetの欠落、capability証跡の欠落、rollback証跡の欠落、
resume証跡の欠落は互いに区別できるclassとして返る。集約flagのpromotableは8条件すべてが
metのときだけtrueになるため、not-observable-hereが1つでもあればfalseのままである。

現在の実測: 契約が宣言するbaseline capabilityはすべて証跡なしであり、条件2はその全件で
unmetである。したがって現在の候補はpromotableではない。これは正しい答えであり、この面は
それを隠さない。

観測できない条件: 条件1（決定的な自動testと静的検証）はCIの `make check` が判定するもので
稼働processからは読めない。条件5（未解決の重大な障害、秘密漏洩、許可されない費用がない）は
in-processのsourceが持たない障害・秘密走査台帳を要する。両者はnot-observable-hereとして
理由付きで返し、metにもunmetにもしない。

未配線の残余: 8条件が満たされたときに自動で昇格を実行する経路は配線されていない。この面は
不足条件を報告するだけである。

<a id="cap-loop-control"></a>
## ループを制御する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-loop-control` 宣言である。preview-local dogfood（V2-022）でgraceful stopの発行と受付から検証までの遷移、claim停止と解除、実child process groupの終了までを実測したが、宣言する成功条件が要求する対象Runner・process・lease・新規副作用可否の表示は `ControlReadModel` に無い（escalation E22-8）。宣言する外部systemにGoogle Cloud Runが含まれるためD1にも属する。証跡なし。

<a id="cap-loop-self-update"></a>
## ループ自身を更新する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-loop-self-update` 宣言である。preview-local dogfood（V2-022）でLoop channel層の切替とrollback、切替前Requirementの同一canonical stateからの再開までを実測した。ただし宣言する外部systemにGoogle Cloud Run・Git・GitHubが含まれるため、このcapabilityの証跡は初回deploy gate（D1）に属する。証跡なし。

<a id="cap-shared-resource-allocation"></a>
## Backlogへ共有資源を配分する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-shared-resource-allocation` 宣言である。preview-local dogfood（V2-022）で `POST /v1/controls` の上限設定とqueueSummaryの配分・待機理由・枯渇の報告を実測した。宣言する3 Providerのうちclaudeだけが認証済みであり、複数Repositoryの同時処理も未実施のため、証跡なし（Provider側はM6/V2-028、複数RepositoryはM7/V2-030・V2-031）。

<a id="cap-provider-operation"></a>
## AI Providerを利用・切替する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-provider-operation` 宣言である。preview-local dogfood（V2-022）で `GET /v1/providers` が3 Providerの接続状態・上限source・割当先を報告することを実測した。宣言する3 Providerのうちclaudeだけが認証済みで互換providerが存在しないため、障害時のhandoffまたは待機理由の明示は観測できず、証跡なし（M6/V2-028）。

<a id="cap-user-documentation"></a>
## 利用者文書を読む

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-user-documentation` 宣言である。preview-local dogfood（V2-022）で `internal/release/docs.go` の決定的な文書routing検査（link・anchor解決、capability anchorの双方向全単射、固定形式のrelease marker、必須4節、Stableからkeyword Previewへのlink不在、code block許可list）が実doc setに対して成立することと、release文字列が契約・compile済み契約・`Release:` markerの3箇所で一致することを実測した。それでも証跡なしである。理由は2つあり、いずれも実測である。第一に、宣言する唯一の外部systemであるowner consoleが文書routeを提供しないため、稼働channel/versionに対応する文書を宣言surfaceから参照できない（escalation E22-10）。第二に、この文書自身が `/v1/release/state` をowner可読と記しているのに稼働processは503を返し、実挙動との差異が存在する。
