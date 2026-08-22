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
