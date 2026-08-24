# Lease and outbox reconciliation

## `Reconciler.Tick`（lease期限切れpass）

`Reconciler.Tick`は1passにつき`MaxBatch = 100`件の`ExpiredActiveLease`候補を
cursor付きでscanする。100件を超える候補があれば非空cursorを返し、続くTickが
残りを回収する。150件のcandidateに対して1回目のTickが100件、2回目が残り50件
を回収し、いずれのcandidateも二重に回収されないことを構造的に（wall-clock閾
値ではなく`context.WithTimeout`の範囲内で完了することとして）assertしてい
る。

各候補は次の3通りに分類され、いずれも次の候補の処理を妨げない。

- **Recovered**: `Lease`を`LeaseExpired`にし、対応する`Execution`が非terminal
  なら`ExecutionLost`へfenceし、`Increment`を`ready`へ戻す。
- **Closed**（terminal-Executionの場合）: `Execution`が`ExecutionSucceeded`
  など既にterminalな状態に達している場合、`domain.MarkExecutionLost`は
  `ErrInvalidTransition`を返す（terminal状態からの遷移を正しく拒否するた
  め）。この場合`Lease`だけを`LeaseExpired`にして候補を閉じ、`Execution`には
  一切触れない。`Report.Closed`は`Report.Recovered`と区別されるcounterであ
  り、「回収した」と「既に終わっていたので閉じただけ」を混同しない。修正前
  は、この経路の`ErrInvalidTransition`がTick全体をabortさせ、同じ候補が毎
  passで再読込みされ続け、後続の正当な候補が永久に回収されない不具合があっ
  た（happy path: Execution成功後にLeaseを閉じるcodeが存在しなかった）。
- **Skipped**: `domain.ErrStaleVersion`／`domain.ErrStaleFence`のみが
  skippableであり、`quota.ErrOverBudget`はpass全体をabortして呼び出し元へ
  返す。hard Budget超過後に新規のmutationを一切開始しないという安全invariant
  を、部分的な回収で上書きしてはならない。

## Orphan sweep（`OrphanSweep`）

lease-keyedなTickは`lease_status == active`のLeaseしか見ないため、既に
`LeaseExpired`／`LeaseRevoked`になったLease、あるいはLease行が存在しない
Executionは対象外になる。`OrphanSweep.Tick`はこの隙間を埋める別passであり、
新しいrepository portやstore変更を一切追加せず、既存のportだけを合成する。

1. `RequirementsPage(afterID, limit=MaxBatch)`でRequirementを cursor-paged に
   scanする。
2. `IncrementsForRequirements(ids)`でそのIncrementを取得する。
3. `ExecutionsForIncrements(ids)`でExecutionを取得し、非terminalなものだけを
   候補にする。
4. 候補ごとに`Lease(id)`を読み直し、Leaseが存在しない・`LeaseExpired`・
   `LeaseRevoked`のいずれかであれば`domain.MarkExecutionLost`でterminalへ
   fenceし、`Increment`を`ready`へ戻す。Leaseが存在しない場合は、
   `Execution`自身のLeaseID／FencingToken／IDと一致するようlinkageのみの
   参照値を作り、`MarkExecutionLost`の整合性checkに用いる（存在しない
   Leaseについて何も偽の事実を主張しない）。

`Service.Claim`のreclaim枝（同一Incrementへの再claim時に旧Leaseを期限切れに
する経路）は、旧Executionを同一transactionでterminalにする改修を試みたが、
`internal/runner/crash_test.go`の`TestJourneyFourLocalCrashResumeAcrossExecutions`
が、旧Executionのversionが変わらないことを前提に`domain.ErrLeaseExpired`を
厳密に期待しており、`domain.MarkExecutionLost`が必ずversionを進めるため
両立できないことが分かった。`internal/runner/crash_test.go`はこのtaskの
prohibited pathであり、A1（既存testを変更しない）とA4（reclaim元での修正）
が衝突するため、reclaim側の修正は見送り、sweep側のみで収束させている。この
衝突はtech_leadへescalationした。

## Outbox delivery

Outbox delivery keeps provider errors out of canonical state. Timeout,
cancellation, and transport-reset failures become `ambiguous`. 次のpassでは、
外部systemを読む前に必ず**local recordを先に確認する**
（failure-model.md section 6 step 1）。具体的には`OutboxDispatcher`が
ambiguousな候補を`Observer.Observe`に渡す前に、同じidの`OutboxItem`行を
transaction内で再読込みし、既に`OutboxConfirmed`／`OutboxDelivered`／
`OutboxSuperseded`へ解決済みであればexternalな`Observe`を一切呼ばずにそれを
採用する。修正前はbatch snapshotの古い状態のまま無条件に`Observe`を呼んで
おり、既に確定済みの結果を再observeし得た。

local recordで未解決の場合のみexternal systemを読み、`confirmed`は
delivered（`DeliveredAt`設定）へ収束し、`not-observed`は同一のimmutableな
operation ID／idempotency keyで再送し、判別不能な観測は再送せず
`needs-input`へ進む。Policy denialやstale fenceのfailureはexternal effect
として再試行しない。
