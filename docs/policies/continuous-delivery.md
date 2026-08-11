# 継続的デリバリーポリシー

## 目的と適用条件

本番環境へのdeployまたは利用者へ配布するapplication binaryのreleaseが必要になった場合、手動提供ではなく、検証可能で再現可能な継続的デリバリー（CD）を必須とする。本番deployも配布binaryもないrepositoryには空のpipelineを要求しない。最初の対象機能と同じ変更でこのポリシーを満たすpipeline、定義、テストおよび運用文書を追加し、pipelineを通らない通常提供を禁止する。

本ポリシーは[外部環境コード化ポリシー](external-environment.md)、[開発環境ポリシー](development-environment.md)、[検証ハーネスポリシー](validation-harness.md)、[テストポリシー](testing.md)、[費用ポリシー](cost.md)と同時に満たす。破壊的・不可逆または重大な費用、availability、data lossを伴う判断は自動化せず、実行前に明示的な承認を得る。

## Triggerと成果物の昇格

- `main`へのmergeを唯一の通常release triggerとする。branch、pull request、local環境または任意の手動操作から本番成果物を発行しない。
- merge後のsource revisionを一意に固定し、そのcommitに対する共通検証入口の成功をgateとする。固定されたコード化済み環境で成果物を一度だけbuildし、content digestで識別する。
- 検証した同一artifactを変更せずにGitHub Releaseと各environmentへ昇格する。environmentごとの再build、未検証artifactへの差し替え、mutable tagだけによる参照を禁止する。
- 同じsource revisionまたはartifactに対する再実行は既存状態を検証して再利用し、二重release、二重deployおよびversionの再割当てを起こさない。並行実行もrepositoryとenvironmentごとに直列化する。
- 手動実行は障害復旧に限定する。通常経路と同じrevision、pipeline、artifact、gate、権限および監査証跡を使い、release内容を変更する迂回路にしない。

## InterfaceとSemVer

interfaceとは、利用者または連携先との互換性境界であり、公開API、CLI、設定schema、protocol、永続data形式、配布binaryの契約などを含む。各repositoryはrelease対象ごとに、interfaceの機械可読な定義、直前releaseとの決定的な比較方法、判定規則および代表的なfixtureをコード化し、自動テストする。

直前のimmutable releaseと`main`のinterface差分からSemVerを次のように自動決定する。

- 後方互換性を壊す変更はmajorとする。
- 後方互換なinterface追加はminorとする。
- interfaceを変更しない互換修正はpatchとする。

成果物が変わるのにversionが据え置かれる場合、差分を一意に判定できない場合、複数の規則が矛盾する場合、または直前releaseを信頼して特定できない場合はreleaseを停止する。推測や上書きで誤ったversionを発行しない。決定したversionには重複しないimmutable Git tagを割り当て、同名tagまたはreleaseが異なるcommitやartifactを指す場合は停止する。

## GitHub Releaseと監査証跡

pipelineはimmutable tagに対応するGitHub Releaseを作成し、少なくとも次を機械的に追跡可能にする。

- version、release notes、source revisionおよびinterface差分の判定結果
- artifactのcontent digestとchecksum、build環境の固定定義、build provenance
- 検証したcommit、実行した共通入口と結果、artifactの再現・checksum検証手順
- pipeline run、承認、昇格先environment、実行時刻、health check、停止またはrollbackの結果

監査記録とrelease notesへ秘密や短期credentialを含めない。再実行時はtag、GitHub Release、artifact、deployment記録の整合性を検査し、不一致を上書きせず停止する。

## 本番deploy、停止とrollback

本番deployがあるrepositoryは、environmentごとのdesired state、設定、artifact digest、最小権限、承認gate、適用手順、drift検出、health checkおよび復旧手順をrepositoryにコード化する。段階的反映が可能な基盤では小さい対象から昇格し、各段階のhealth check成功後だけ次へ進む。失敗、timeout、想定外driftまたは承認不足では直ちに昇格を停止し、成功として扱わない。

rollbackは、直前の検証済みartifactと対応するコード化済み設定へ戻す再現可能な操作とし、事前にfixtureまたは安全なlocal/fake環境でテストする。data schemaなどrollbackが破壊的または不可能な変更は、後方互換な段階的migration、forward recovery、backupとrestore検証、および必要な人の承認を変更前に設計する。自動rollbackがdata lossや追加障害を拡大し得る場合は自動実行せず、安全に停止して承認を求める。

## 秘密、権限、費用と例外

秘密をrepository、source、log、artifact、provenanceまたはreleaseへ保存しない。実行時は可能な限り短期credentialを発行し、repository、workflow、environmentごとに必要最小限の権限と保護を設定する。長期credentialが避けられない場合も外部secret storeで管理し、rotationと失効手順をコード化する。

GitHub Actionsその他の実行基盤は、repositoryのvisibilityと料金・請求条件を確認し、[費用ポリシー](cost.md)が許容する場合だけ利用する。CDを理由に追加課金、有料serviceまたは別途課金される認証情報を導入しない。許容される自動実行基盤がない場合は手動releaseへ迂回せず、成果物の提供を停止し、費用を伴わないコード化済み基盤の選定または明示的な判断を求める。

緊急修正も本ポリシーのversion判定、同一artifact、検証、秘密、監査および承認gateを省略しない。例外が必要な場合は、理由、影響、承認者、対象revision、有効期限、復旧方法と再検証結果を記録し、恒久的な迂回路にしない。hookや安全なsandboxのbypassは例外として認めない。
