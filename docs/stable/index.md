# Stable

Stable release: none

Stable利用者向け文書の入口です。2026-08-22時点でv2のStable Releaseは
存在せず、`main`のv1が既知の復旧点です。v2 branchやlocal buildをStableと
して運用しないでください。

最初のStable ReleaseはPreviewで全user-facing capabilityを実行し、対象となる
実Provider、停止、rollback、resume、文書digestのEvidenceが揃った場合だけ
自動昇格します。昇格と同じtransaction/sagaでこの文書入口も切り替えます。

現時点でv2のStable capabilityは一つも存在せず、capability baselineは
[contracts/release-contract/foundation.json](../../contracts/release-contract/foundation.json)
である。

`internal/update`のchannel pointerが指す「直前に検証済みのversion」は、
Release ContractのStable Releaseではない。前者はowner実機のLoop channelを
1つ前のinstall済みversionへ戻す操作であり、Release Contractの昇格gateも
capability証跡も一切通らない。2026-08-26のpreview-local dogfoodで実測した
rollbackとRequirementの再開は前者であり、後者は依然として存在しない。
