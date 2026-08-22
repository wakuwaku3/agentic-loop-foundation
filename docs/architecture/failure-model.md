# Failure・safety model

更新日: 2026-08-22

## 1. 原則

- failureをRequirementの失敗と同一視しない
- process exit、timeout、quota、契約違反、課題未達を別classにする
- retry前に現在状態を観測し、同じ副作用を盲目的に再送しない
- retry回数だけでなく、同じfailure fingerprintと同じapproachの反復を制限する
- 自動回復不能でもRequirementを消さず、必要な判断とEvidenceを残す
- 安全性が不明なら可用性より停止を優先する

## 2. Failure taxonomy

| Class | 例 | 自動処理 | Requirementへの影響 |
| --- | --- | --- | --- |
| `invalid-input` | schema不正、存在しないRepository | 修正可能ならframingへ戻す | `framing`／`needs-input` |
| `policy-denied` | cost、権限、大原則違反 | 実行せずDecisionを記録 | `needs-input`または別approach |
| `capacity-unavailable` | Runnerなし、quota枯渇 | cooldown後に再評価 | `waiting` |
| `provider-transport` | CLI crash、通信断、JSON破損 | Adapter単位のbounded retry | `active`／`waiting` |
| `provider-model` | overloaded、model不存在 | 同poolの互換model候補を評価 | `active`／`waiting` |
| `provider-quota` | usage limit、429 | Accountをcooldownし他候補を評価 | `waiting` |
| `execution-lost` | Runner停止、Lease expiry | fencing後にcheckpointからhandoff | `recovering` |
| `progress-stalled` | process生存、進行signalなし | checkpoint、TERM/KILL、新Execution | `recovering` |
| `verification-failed` | test／capability失敗 | 原因分析し修正Increment | `active` |
| `external-ambiguous` | write timeoutで成否不明 | read-after-writeでreconcile | `recovering`／`needs-input` |
| `integration-conflict` | base更新、schema競合 | 新Observationでreplan | `active` |
| `preview-regression` | 実稼働capability失敗 | Preview停止、Stable routingへ復帰 | `active`／`recovering` |
| `promotion-partial` | deploy成功、docs routing失敗 | Outbox reconciliationまたはrollback | `recovering` |
| `secret-suspected` | scanner検知、log漏洩疑い | stop、redact、credential失効 | `needs-input`／`recovering` |
| `budget-exceeded` | hard usage上限 | 新規作用を停止 | `waiting`／`needs-input` |
| `contract-incompatible` | StableがPreview stateを読めない | Preview停止、migration rollback | `recovering` |
| `unknown` | 分類不能 | 反復せずEvidenceを保存 | `needs-input` |

## 3. Retry Budget

Retry Budgetは次のdimensionで管理する。

- Operationごとのtransient retry回数と総時間
- ExecutionごとのProvider Run回数とcost
- Incrementごとの同一failure fingerprint回数
- Requirementごとの同一approach失敗回数
- Provider Accountごとのcooldownと日次usage
- Installation全体の時間、compute、network、AI budget

Budgetを使い切った場合、counterをresetして同じ処理を繰り返さない。Observationが変化した、approachが
変わった、人間が上限変更を承認した場合だけ新Budgetを発行する。

## 4. Circuit breaker

Provider、Source Control、Deploy Targetごとにclosed／open／half-openを持つ。

- failure率または明示quota signalでopenにする
- open中は新規Operationを送らない
- reset timeまたはbounded probeでhalf-openにする
- 1つのprobeだけを許可し、成功でclosed、失敗でcooldownを増やす
- circuit状態はRequirement stateへ埋め込まずResource Observationとして保持する

永続的な認証失敗や権限不足をtransient retryしない。

## 5. Stop／partition safety

### Control PlaneとRunnerが接続中

1. Control Intentをdurable writeする
2. 新規claimとcredential発行を停止する
3. Runnerがrevisionをackし、childをcheckpoint／terminateする
4. Result／外部Operationをreconcileする
5. active Leaseがなくなったことを検証する

### Runnerがnetwork partition中

- Leaseをrenewできずexpiryする
- fencing tokenを進め、旧ExecutionのResultを拒否する
- Runner localのProvider processが物理的に残る可能性を表示する
- external credentialが短命化できる場合はexpiryさせる
- 長寿命credentialを持つlocal processについてはintegration／promotionをControl Plane側で拒否する
- 再接続時に古いprocess、workspace、Operationをreconcileしてから新規claimを許可する

「processが絶対に存在しない」と「authoritativeな結果を確定できない」を区別する。

## 6. Ambiguous side effect

timeoutはfailureではなく結果不明である。次の順序を守る。

1. 同じOperation IDの結果をlocal recordから探す
2. 外部systemをreadし、期待したrevision／content／idempotency keyを探す
3. 成功を確認できればResultを収束させる
4. 未実行を確認できれば同じOperation IDで再実行する
5. 成功・未実行を区別できなければ再送せず`needs-input`または補償Operationへ進む

非冪等な外部APIを「通信errorだから」という理由だけで再試行しない。

## 7. Preview failure

Preview capabilityが失敗した場合:

- Release Candidateを`promotable`にしない
- Previewへの新規Requirement routingを停止する
- Stable ApplicationとStable Loopの健全性を独立に確認する
- 進行中Incrementをcheckpointし、互換ならStable Loopへhandoffする
- failureを再現するEvidenceを秘密なしで保持する
- 修正は候補Artifactの上書きでなく新しいRelease Candidateとして作る
- rollback exerciseが失敗した場合は重大incidentとして扱う

Preview failure自体は想定内であり、Stableの問題解決能力を失った場合にincidentとなる。

## 8. Promotion failure

Promotionは複数外部resourceを完全atomicには更新できないため、sagaとして扱う。

- Release Candidate bundleをimmutableに固定する
- Stable channelの期待versionを確認する
- deploy、schema、configuration、docs routingを順序付きOperationとして実行する
- 各OperationをObservationで確認する
- 全Operation成功後にcanonical Stable Releaseを確定する
- 途中失敗時は安全なforward recoveryを優先し、不可能なら旧Stableへ補償rollbackする
- rollbackも同じEvidenceとverificationを必要とする

利用者には`promoting`／`rolling-back`を表示し、部分成功をStable完了と報告しない。

## 9. Secret incident

疑いの段階で次を行う。

1. 関連Execution、outbound Operation、Release promotionを停止する
2. raw valueを新しいlogや通知へ複製しない
3. secret identifier、露出境界、時刻だけを記録する
4. credentialを失効・rotationする
5. canonical state、Artifact、workspace、VCS履歴、Provider入力の露出範囲を確認する
6. 必要な削除・履歴修復を人間承認下で行う
7. 全capabilityをPreviewで再実証してから再昇格する

秘密の値そのものをIncident Evidenceへ保存しない。

## 10. Failure injection

Preview昇格前に少なくとも次を意図的に試す。

- Control Plane request直前／直後のRunner停止
- Lease renewal停止と古いResult送信
- Provider CLIの非0終了、0終了error envelope、不正JSON、空result
- Provider quota枯渇と回復probe
- Source Control writeのtimeout後成功／未実行
- validation process hang
- Preview deploy途中失敗
- docs routingだけの失敗
- schema migration途中で旧Stableを再起動
- emergency stop中の新規claim／promotion試行
- secret-like fixtureのcommit、log、Provider outbound混入
- 2 Repository間のcapacity飢餓

fakeによる故障注入に加え、破壊や費用を伴わない範囲でPreview実環境の実物境界を確認する。

## 11. Safety invariants

- 同じIncrementへ同時に2つの有効fencing tokenは存在しない
- expired tokenのResultはRequirement、Release、Stableを前進させない
- stop effective後に新しいauthoritative副作用を開始しない
- Requirement完了は対応Stable Releaseとfresh Evidenceを必ず参照する
- Release Contract未確認capabilityが一つでもあればStableへ昇格しない
- Provider依存変更は対象実Provider Evidenceなしに昇格しない
- credentialはcanonical state、Work Packet、Release Artifactへ入らない
- hard Budget超過後に新規resource消費を開始しない
- Preview failureでStable runtime／docs／rollback targetを削除しない
- external timeoutを成功または未実行と推測しない
