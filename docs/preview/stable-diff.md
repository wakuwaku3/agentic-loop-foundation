# Stableとの差分

## 差分

現在Stable Releaseはまだ存在しません。したがってv2候補の全機能が差分で、
`main`のv1を置換する根拠にはなりません。最初のStable昇格時に、この文書の
確定内容をStable文書へ移し、ここには次のPreview差分だけを残します。

## 既知の問題

Preview Releaseはまだ実環境へdeployされていないため、全capabilityが
Preview実環境で未実施であり証跡なしです。既知の問題は個別capabilityごとの
実exercise結果が得られてから記載します。

## Stableへ戻す方法

Stable Releaseが存在しないため、戻す対象は`main`のv1です。
`contracts/release-contract/foundation.json`の`rollback.procedure`に記載の
手順（当該commitのgit revertとdevbox run --pure checkの再実行）に従ってください。

## 昇格に不足している実証

現在不足している実証は、GCP Preview deployment、Foundation Repositoryでの
dogfooding、実Codex/Claude/opencode接続、停止/rollback/resume、および複数の
利用者管理machineとRepositoryによる並列実行です。

capability baselineの正本は
[contracts/release-contract/foundation.json](../../contracts/release-contract/foundation.json)
であり、各capabilityの宣言とanchorは
[Preview capability一覧](capabilities.md) を参照してください。
