# Control verification

Control intents become effective immediately at issuance, while verification
remains pending until bounded per-runner observations prove that active leases
and processes have checkpointed or terminated. Mixed reachable/unreachable
runners never produce a false `verified` result. Deadline expiry produces
`blocked-unreachable`; ambiguous outbox operations produce
`blocked-ambiguous`. A later allow intent is required to release an effective
stop policy.

`POST /internal/reconcile` is reserved for a dedicated Cloud Scheduler OIDC
service-account identity and rejects owner/runner session authentication. IaC
contains an optional Scheduler resource, disabled by default; it can be enabled
only after the account-level free-tier/cost preflight and custom IAP audience
have both been supplied. Until then, use an authenticated manual trigger for
maintenance without weakening the endpoint identity check.
