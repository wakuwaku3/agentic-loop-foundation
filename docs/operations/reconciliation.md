# Lease and outbox reconciliation

The lease reconciler scans at most 100 expired active leases per cursor pass.
Each candidate is re-read in a transaction before expiring the lease, marking
the execution `lost`, and returning a recoverable increment to `ready`. The
current fencing token is retained, so stale results are rejected by the
application result gate. A durable event and bounded reconciliation outbox
record are emitted for each recovered lease.

Outbox delivery keeps provider errors out of canonical state. Timeout,
cancellation, and transport-reset failures become `ambiguous`; the next pass
uses the optional `EffectObserver` first. `confirmed` converges to delivered,
`not-observed` retries the same operation ID, and unknown observations become
`needs-input`. Policy or stale-fence failures are never retried as external
effects.
