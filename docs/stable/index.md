# Stable

Stable利用者向け文書の入口です。2026-08-22時点でv2のStable Releaseは
存在せず、`main`のv1が既知の復旧点です。v2 branchやlocal buildをStableと
して運用しないでください。

最初のStable ReleaseはPreviewで全user-facing capabilityを実行し、対象となる
実Provider、停止、rollback、resume、文書digestのEvidenceが揃った場合だけ
自動昇格します。昇格と同じtransaction/sagaでこの文書入口も切り替えます。
