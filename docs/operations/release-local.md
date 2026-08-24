# Local release core

`internal/release` はprovider-neutralなM5 local closureである。`CompileContract`
はcanonical Release Contract surface（`id`、`kind`、`created_at`、
`correlation_id`、`release`、capability毎の`name`/`status`/`evidence_ids`、
`verification`、`rollback{procedure,target}`）を`DisallowUnknownFields`で
decodeし、drift／捏造fieldを含むcontractを拒否する。`status: "stable"`かつ
`evidence_ids`が空のcapability宣言も拒否する。`Bundle`はcandidateをcloneして
保持し、`Put`後の呼び出し元による書き換えを防ぐ。

## Bundleの7 roleとdigest framing

`bundle.go`は宣言済みmemberを7つのroleへ分類し、各roleをsource treeの実bytes
から組み立てる。呼び出し元がdigestを主張することはできない。

| role | 対象 |
| --- | --- |
| contract | `contracts/release-contract/**` |
| schema | `contracts/schemas/**` |
| api-contract | `contracts/openapi/**` |
| implementation-manifest | `ci/components.json`、`go.mod`、`go.sum`、`devbox.lock` |
| migration | `firestore.indexes.json` |
| configuration | `devbox.json`、`firebase.json` |
| documentation | `docs/preview/**.md`、`docs/stable/**.md` |

`Member`は`(role, repository-relative path, sha256(bytes))`。`BundleDigest`は
`role\x00path\x00hexdigest\n`のframingを`(role, path)`でsortして全memberに
適用したsha256であり、`DocsDigest`はdocumentation roleだけに絞った同じ
framingである。framingはfilesystem列挙順に依存せず、renameには反応する。
symlinked member、宣言済みだが実在しないmember、0 memberのrole、rootを
外れるpathは、いずれもassembly自体を拒否する。

M5-localにおける実装のidentityは、buildした binary のdigestではなく、この
source manifestである。実binary digestはV2-022（live image）と
V2-034（signed update bundle）の責務である。

## 実装がguardされない範囲（escalation E1・E4）

`ci/components.json`のrelease componentは`internal/release/**`、
`contracts/**`（`contracts` dependencyの`public_contracts`経由）、および
無条件4file（`ci/components.json`、`go.mod`、`go.sum`、`devbox.lock`）だけを
evidence keyのclosureに含む。したがってbundleのcontract／schema／
api-contract／implementation-manifest roleはguardされるが、migration／
configuration／documentation roleはguardされない。この境界は
`UnguardedMembers`という明示allowlistとしてcodeへ書かれ、
`TestReleaseEvidenceKeyClosureAndUnguardedAllowlist`がclosureとallowlistの
双方をci/components.jsonから再計算して検証する。

結果として、**docsだけのcommitはselective CIでrelease componentを選択
しない**。documentation drift gateはこのtaskのtestとして`make check`／
`make test`では強制されるが、docs-only commitに対するselective CI run
としては強制されない。これはE1として記録された未解決事項であり、fixには
`ci/components.json`の編集（禁止path）とmilestone境界での23-key
再evidence化が要る。E4として、migration／configuration role、および
将来の binary role は同じ理由でM5-localではsource-guardできない。

allowlist entryを削除してtestを通すことは誤りであり、closureが実際に
その member を覆うようになったときにだけ削除してよい。

## Promotionとrollback

Promotionは`PromotionGate`という純粋なgateであり、candidateが自身の
evidenceと内部整合しているかだけを見る。`Pipeline.Promote`はその上位で
(1) `VerifySource`によるsource tree再検証、(2) `VerifyCandidateDigests`に
よるcandidateのdigest fieldとsource-derived digestの一致確認、
(3) `VerifyCandidateAgainstContract`によるcapability集合の一致確認、
(4) `PromotionGate`の順に実行し、いずれかが失敗すれば`Router`は一切
変化しない。

`Router.Rollback`はmonotonicである。1回のrollbackで
`PreviousStableDigest`を空にし`RollbackRecord{repository, from, to,
reason, at}`を記録するため、2回目の連続rollbackは拒否される。withdrawn
digestへ戻るにはgateを通らないrollbackではなく、新しい`SetPreview`＋
`Promote`が必要である。

`domain.ReleaseAssembling`、`domain.ReleasePreviewDeployed`、
`domain.ReleaseRejected`、`domain.ReleaseRollback`はどの遷移関数からも
生成されない定数であり、domainには対応するrollback transitionも存在しない。
これはこのtaskが決定した仕様ではなく、`internal/domain`の非test fileが
prohibitedであることに起因する既知のdrift（escalation E3）として記録する。

`internal/release`はroute rollbackを`internal/update.Switch`のatomic
channel symlink rollbackとは意図的に結合しない。両者は別のlayerの別の
rollbackであり、`internal/release`から`internal/update`をimportすることは
（test fileでも）行わない。関係はこの文書に書くだけで、codeでは結ばない。

## Retention eligibility

`RetentionEligible`は注入されたclockとcontract由来のrollback windowを
入力とする純粋関数であり、次の独立した3つの拒否理由と1つの許可条件を
持つ。

1. 対象versionが現在のstable route targetである場合は拒否する
2. 対象versionが直前のstable route targetである場合は拒否する
3. rollback windowが開いている場合は拒否する
4. いずれかのRequirementの`StableSnapshot`が対象versionを参照している
   場合は拒否する
5. 上記のいずれにも該当しない場合にのみ許可する

削除（GC）の実行はM8のV2-034が担う。ここではeligibility判定だけを持つ。

## 文書とdigestの不動点禁止

doc setのどの文書も自身のdigest値を含まない。文書自身のdigestをその文書へ
書き込むと、どのassemblyも満たせない不動点になるためである。digest値は
in-memoryのcandidateと`.agents/v2/evidence/`のevidence recordだけに記録し、
`docs/preview/index.md`と`docs/stable/index.md`が持つのは
固定形式の`Release: <value>` / `Stable release: <value>`という機械可読な
release markerのみであり、値そのもの（digest）ではない。

## Journey・doc routing・実tree

localのjourney testは、missing capability、stale evidence、documentation
drift、immutable conflict、preview promotion、monotonic rollbackを
`internal/release/journey_test.go`のJourney 1（release segment）と
Journey 7（local segment）で検証する。Preview実環境を実際にdeploy／破壊
する半分はV2-022の責務であり、ここでは行わない。

`docs.go`はPreview/Stable文書routingのdeterministic検査（link解決、
anchor解決、capability anchor bijection、release marker、Preview必須
4節、Stable→Preview link禁止、fenced code blockのallowlist）を提供する。
AI documentレビューはこれらの代替にならない。

実tree（repository root）に対する`AssembleFromRoot`はrole構成の健全性を
証明するが、実contractの12 capabilityは全てevidence_idsが空であるため、
実tree candidateはpromotion不能であるという誠実な否定結果を返す。
Production storage／deploy adapterはこのpackageのpersistence portの外に
留まる。
