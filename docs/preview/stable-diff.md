# Stableとの差分

現在Stable Releaseはまだ存在しません。したがってv2候補の全機能が差分で、
`main`のv1を置換する根拠にはなりません。最初のStable昇格時に、この文書の
確定内容をStable文書へ移し、ここには次のPreview差分だけを残します。

現在不足している実証は、GCP Preview deployment、Foundation Repositoryでの
dogfooding、実Codex/Claude/opencode接続、停止/rollback/resume、および複数の
利用者管理machineとRepositoryによる並列実行です。

capability baselineの正本は
[contracts/release-contract/foundation.json](../../contracts/release-contract/foundation.json)
であり、各capabilityの宣言とanchorは
[Preview capability一覧](capabilities.md) を参照してください。
