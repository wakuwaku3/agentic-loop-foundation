# Runner local daemon

`cmd/runner` has no production control-plane or provider connection. It exits
unless the local mode is explicit:

```sh
go run ./cmd/runner --fake --runner-id runner-local
go run ./cmd/runner --fake --runner-id runner-local --data-root /absolute/path
```

Fake mode uses the in-memory application boundary and local durable journal
only. The temporary data root is removed on exit; a supplied root must be an
absolute directory with mode `0700`. The daemon handles `SIGINT` and `SIGTERM`
and then exits. It does not manufacture owner/scheduler credentials for a
production deployment.

The orchestrator obtains a process permit immediately before provider start
and a result permit immediately before acceptance. A stop or stale fence
therefore prevents the external action and leaves no accepted result. A
`result_pending` journal record is recovered by request ID, so retry skips the
provider and records one `result_accepted` event after the idempotent
application acceptance.
