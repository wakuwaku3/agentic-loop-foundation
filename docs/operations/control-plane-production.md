# Production control-plane authentication

Cloud Run must use direct IAP authentication. The application consumes the
IAP assertion from `X-Goog-Authenticated-User-Email` and accepts only the
`accounts.google.com:<email>` form for an email in `OWNER_EMAILS`.

Runner calls use `X-Agentic-Runner-Session`; `Authorization` is not a runner
credential because IAP owns that transport header. The session is verified by
the runner enrollment service and is never written to logs.

`INSTALLATION_ID`, `GCP_PROJECT_ID`, `OWNER_EMAILS`, and `OWNER_ORIGINS` are mandatory. Startup
fails closed when any is missing (only `--version` bypasses configuration),
and the process closes Firestore and gracefully shuts down HTTP on SIGTERM.

The Cloud Run/IAP IAM binding and ingress policy are deployment invariants:
do not expose the service publicly, do not add unauthenticated invoker access,
and do not replace direct IAP with a caller-controlled spoofable header. The
IAP service-agent `roles/run.invoker` binding must be managed by IaC.

## preview-local (V2-051): owner session/token boundary and the Firestore emulator

Roadmap M2 defines owner authentication on the owner's own machine as a
session/token boundary, with the IAP boundary above deferred to D1. Two
environment variables, both unset by default and both refused when
`APP_ENV=production`, opt this binary into that local grade without changing
anything above:

- `AGENTIC_LOOP_LOCAL_OWNER_TOKENS` (`token=email[,token=email...]`), every
  email must already be in `OWNER_EMAILS`, selects
  `api.LocalOwnerBearerAuthenticator` instead of `api.CombinedAuthenticator`:
  the owner authenticates with `Authorization: Bearer <token>` instead of an
  IAP assertion header. Runner sessions are unaffected either way.
- `AGENTIC_LOOP_ALLOW_FIRESTORE_EMULATOR=1` is required whenever
  `FIRESTORE_EMULATOR_HOST` is set, or startup fails closed; it selects
  `firestore.NewEmulatorClient` instead of `firestore.NewClient` (the
  production constructor already refuses an emulator host on its own, so
  this is defense in depth, not the only guard).

Neither variable creates, reads, or mutates any Google Cloud resource. See
`internal/api/live_local_test.go` (`TestControlPlanePreviewLocalLive`, gated
on `AGENTIC_LOOP_LIVE_LOCAL=1`, never run by `make check`) for the exact
composition this exercises end to end as a real process against a real
Firestore emulator.
## Recording who requested a Requirement intake or Control Intent

Every Requirement intake (`POST /v1/requirements`) and Control Intent
(`POST /v1/controls`) response carries `requested_by: {actor_type, subject}`.
`actor_type` is `owner` when the authenticated caller is the human owner and
`loop` when the Loop decided on its own; `subject` is an identity reference
only (never a credential, and never written to logs or evidence). The
production authentication path already provides everything this needs: the
owner's IAP subject above is exactly the subject recorded for an owner
request, so no additional wiring is required here for that side. A
Loop-originated request (`actor_type: loop`) is reached only through an
internal Go caller, never through IAP or the runner session header; today no
in-repo caller exercises that path yet, so it is provisioned but unused.
