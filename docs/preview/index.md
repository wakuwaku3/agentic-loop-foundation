# Preview

Release: 0.1.0-preview.1

Preview候補の利用者向け文書の入口です。現在のv2 branchは実装候補であり、
実環境へdeploy済みのPreview Releaseではありません。上記`Release:`行は
`contracts/release-contract/foundation.json`のreleaseと機械的に一致確認される
固定形式のmarkerであり、この文書自体は自身のdigestを保持しません。

候補に含まれる機能:

- Installation単位の単一backlogとowner UI
- local Runner enrollment、session、workspace、checkpoint
- lease/fencing、lost execution recovery、曖昧な外部作用の隔離
- pause、graceful/immediate/emergency stopと停止結果の再検証
- Codex、Claude、opencodeのprovider-neutral adapter contract
- Repository別Preview/Stable routing gateとsigned Runner bundle

候補をPreview Releaseにするには、GCP preflight、pinned image deployment、
実Runner enrollment、Release Contract全capabilityの実行が必要です。Provider
依存機能は対象Providerの実物Evidenceがない限りStableへ昇格しません。

2026-08-26に、このrepository自身をPreview対象として環境class `preview-local`
（owner実機・実process・実CLI・Firestore emulator）で12 capabilityを1件ずつ
実測しました。手順は `docs/operations/release-live-dogfood.md`
にあります。実測の結果、証跡idを持つcapabilityは1件だけであり、残る11件の
理由は `docs/preview/stable-diff.md` に列挙しています。この実測はGCPへの
deployを含まず、forgeにもremoteにも接続していません。Stable Releaseは
存在せず、昇格も行っていません。

capability baselineの正本は
[contracts/release-contract/foundation.json](../../contracts/release-contract/foundation.json)
であり、各capabilityの宣言とanchorは
[Preview capability一覧](capabilities.md) を参照してください。
