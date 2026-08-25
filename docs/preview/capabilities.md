# Preview capability一覧

更新日: 2026-08-24

このcapability一覧は、`contracts/release-contract/foundation.json` が宣言する
baseline capabilityへのanchorだけを提供する。各capabilityの利用者操作、観測結果、
実証方法、外部依存の正本は `contracts/release-contract/foundation-capabilities.json`
であり、この文書ではその内容を複製しない。

現時点でv2のPreview Releaseはdeployされていないため、以下の全capabilityは
Preview実環境で未実施であり、証跡なしである。

<a id="cap-repository-registration"></a>
## Repositoryを登録する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-repository-registration` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-requirement-intake"></a>
## 課題を登録する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-requirement-intake` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-backlog-visibility"></a>
## Backlogを見る

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-backlog-visibility` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-autonomous-resolution"></a>
## 問題解決を自律実行する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-autonomous-resolution` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-human-input-request"></a>
## 人間の入力を求める

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-human-input-request` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-preview-operation"></a>
## Previewを運用する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-preview-operation` 宣言である。Preview Releaseが未deployのため証跡なし。

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
`cap-stable-promotion` 宣言である。Preview Releaseが未deployのため証跡なし。

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
`cap-loop-control` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-loop-self-update"></a>
## ループ自身を更新する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-loop-self-update` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-shared-resource-allocation"></a>
## Backlogへ共有資源を配分する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-shared-resource-allocation` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-provider-operation"></a>
## AI Providerを利用・切替する

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-provider-operation` 宣言である。Preview Releaseが未deployのため証跡なし。

<a id="cap-user-documentation"></a>
## 利用者文書を読む

正本は `contracts/release-contract/foundation-capabilities.json` の
`cap-user-documentation` 宣言である。Preview Releaseが未deployのため証跡なし。
