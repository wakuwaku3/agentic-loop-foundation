# Preview

Preview候補の利用者向け文書の入口です。現在のv2 branchは実装候補であり、
実環境へdeploy済みのPreview Releaseではありません。

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
