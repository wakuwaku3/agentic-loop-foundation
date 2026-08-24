# Control verification

Control intents become effective immediately at issuance, while verification
remains pending until bounded per-runner observations prove that active leases
and processes have checkpointed or terminated. Mixed reachable/unreachable
runners never produce a false `verified` result. Deadline expiry produces
`blocked-unreachable`; ambiguous outbox operations produce
`blocked-ambiguous`. A later allow intent is required to release an effective
stop policy.

## RunnerからのackとProcess観測

Runnerは`Heartbeat`を通じてのみcontrol revisionをackし、process観測を報告す
る。`LifecycleResponse`は`latest_revision`と`latest_mode`（追加optional
field、`heartbeat`／`checkpoint`のresponseにのみ存在し`RenewResponse`には
存在しない）を返し、Runner側の`ControlAgent`が`LatestRevision`が既に適用済
みのrevisionを超えているときだけ`domain.ControlIntent`を組み立てて
`ControlLoop`へ適用する。`latest_mode`は`ControlAgent`が
`checkpoint`（graceful stop）と`terminate`（immediate／emergency
stop／cancel）のどちらを選ぶかを決める唯一の材料であり、`latest_mode`が
欠落または未知の値であれば、Runnerは安全側に倒してfail-closedな
`immediate-stop`として扱う（未適用のrevisionを楽観的に`allow`とは決して
みなさない）。

`ControlLoop.Apply`が実際にprocessをcheckpointまたはterminateした結果は、
次の`Heartbeat`で`ControlRevision`（適用したrevision）と
`Processes: [{ProcessID: executionID, State: terminated|checkpointed}]`と
して報告される。この報告はRunner側の`Journal`に書かれた
`control_observation`から読み直した値であり、`ControlAgent`が
「terminatedしたはず」と楽観的に決め打つのではない。

`ControlAgent`はexplicitな`Tick`呼び出しでのみ駆動され、background
goroutineやtimerを一切持たない。`ControlAgent`を配線しない場合（あるいは
一度も`Tick`を呼ばない場合）、`Heartbeat`の`AppliedRevision`は決して停止
revisionへ追いつかず、`Verification`は`pending`のまま進まない。これは
「Control Plane側のverificationがobservationからしか進まない」ことを示す
positiveなcontrolである。

## Verificationの収束条件

Verification reconciler（`VerificationReconciler.Tick`）は
`PendingControlProgresses`を読み、対象それぞれについてLeaseの現在status
（`LeaseActive`なら依然pending）、`RunnerObservation`
（`AppliedRevision`・`Reachable`・`Processes`）、`OutboxResolution`
（ambiguous／pendingな外部effectが残っていないか）を再読込みし、durableな
観測だけからverified／blocked-unreachable／blocked-ambiguous／pendingを
決定する。単なるControl Intentの存在からは`ControlVerified`に到達しない。

## Scheduler endpoint

`POST /internal/reconcile` is reserved for a dedicated Cloud Scheduler OIDC
service-account identity and rejects owner/runner session authentication. IaC
contains an optional Scheduler resource, disabled by default; it can be enabled
only after the account-level free-tier/cost preflight and custom IAP audience
have both been supplied. Until then, use an authenticated manual trigger for
maintenance without weakening the endpoint identity check.
