# ADR 0014: 変更競合の判定を構造的衝突に限定し、通常編集を並列実行する

## 決定

変更scopeの競合判定を「pathの重なり＝常に直列化」から「**構造的衝突（rename/move・directory再編）だけを直列化**」へ緩和する。通常の同一file編集は並列実行し、既存のrebase・再検証経路でmerge時に収束させる。

直列化（hard conflict）するのは次のいずれかが真のときだけである。

- いずれかのscopeが `*`（repository全体、`[queue].exclusive_paths` による昇格を含む）
- 双方がscope未宣言（`unknown_scope=isolated` の `unknown` 同士）
- `env:` が完全一致（rebase不能な外部環境）
- いずれかが `structural`（markerの `structural=1` またはworkerのGit実測）で、かつpathが `/` 境界で重なる

これ以外のpath重なりはsoft overlapとして並列許容し、`scope-conflict` markerも記録しない。実行中のscope cacheは引き続きunionでのみ拡大し、`structural` sentinelを含む。

## 理由

実測（metrics: `scope_conflict=1191` vs `attempts=743`）のとおり、同一pathのprefix重なりでclaimを抑止する旧判定は過度に保守的で、PR review待ち（平均約5,000秒）の間もrunning scopeが残り、重複pathのIssueを長時間stallさせてworker稼働率（約18.7%）を落としていた。rename/move・directory再編のような**mergeでは自動吸収できない構造的変更**だけが直列化に値し、通常の同一file編集は既存のneeds-rebase／rebase・再検証経路で収束できる。

## 安全性

- **fail-closed維持**: `*`／`unknown`／`env:` の直列化は従来どおり。`structural` の検知漏れは「フラグなし扱い（並列）」になるが、diff実測のrename検出が補い、検知の不確実さより構造的変更を過検知して直列化に振れる方が安全側である。
- **AIによるscope・structural推定は行わない**（コストポリシー順守）。structuralはplanの明示宣言（`structural=1`）とworkerのGit実測（`git diff --name-status -M` のR検出、同一directoryの多数D/A）のみで判断する。
- **後方互換**: 旧marker（`paths=` のみ）はsoft overlap扱い（並列）。`paths=*`、`exclusive_paths`、`unknown_scope` の意味は不変。marker省略は従来どおり `unknown_scope` の安全側既定にフォールバックする。
- 実衝突は既存経路で吸収される。通常編集が実際に衝突した場合、workerはdefault branch更新後にrebase・再検証し、失敗時はbounded retryで再開する。

## 帰結

- `scope_conflict` カウントはhard conflictのみになり、件数は構造的衝突の発生頻度を反映する（定義は `docs/operations/loop-metrics.md` に明記）。
- planがrename/move・directory再編を含む場合は、scope markerへ `structural=1` を付ける運用を `docs/operations/issue-queue.md` とplan promptに追記する。
- 同一file編集の並行PRが増えるため、rebase・conflict解決の負荷は従来より増える（許容したトレードオフ）。
- 外部環境の追加はない。GitHub Issue/PR/Projectの既存投影のみ。
