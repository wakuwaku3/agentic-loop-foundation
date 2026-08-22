# v2 owner console (M2-A05)

The owner console is a same-origin, server-rendered HTML surface at `/owner/`.
It assumes an IAP-authenticated principal and does not accept an actor identity
from the browser. Mutating requests remain behind the existing owner
authentication and origin checks (deployment adds the CSRF token at the IAP
boundary).

The Stable surface is the JSON API under `/v1`: bounded requirement pages use
`page_size <= 100` and opaque `cursor` values. `/v1/requirements/{id}` exposes
the original text, status, increment/execution summaries, and a current next
action. `GET /v1/controls` reports requested, acknowledged, effective, and
verified evidence explicitly; an absent proof is never inferred as verified.

Preview and Stable release views show only digest references and lifecycle
metadata. Raw provider responses, credentials, and outbox payloads are not
part of the read models or `/v1/export` (JSON/NDJSON) output. Export is owner
authenticated and bounded to 100 records per request; every record carries a
schema version and SHA-256 digest reference.

Ownership: `internal/application` owns read-model semantics and bounds,
`internal/store/memory` and `internal/store/firestore` own parity adapters,
`internal/api` owns HTTP authentication/error contracts, and `internal/web`
owns the accessible HTML skeleton. Deploy-time IAP/CSRF wiring is outside this
repository component.
