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

候補をPreview Releaseにするには、GCP plan approval、pinned image deployment、
実Runner enrollment、Release Contract全capabilityの実行が必要です。Provider
依存機能は対象Providerの実 CLI 確認がない限りStableへ昇格しません。

2026-08-28に、このrepository自身を `preview-local` で再実測しました。実
Control Plane、別Runner process、Firestore emulator、Codex 0.149.1、OpenCode
1.18.18、GitHub read、Git clone を使用し、fixture/fakeを実物確認の代替にして
いません。この候補はProviderへの直接変更を含まないため、利用可能なProviderの
実確認としてこの2つで条件を満たします。実測はGCP deploymentを含みません。
Stable Releaseは存在せず、昇格も行っていません。

capability baselineの正本は
[contracts/release-contract/foundation.json](../../contracts/release-contract/foundation.json)
であり、各capabilityの宣言とanchorは
[Preview capability一覧](capabilities.md) を参照してください。
