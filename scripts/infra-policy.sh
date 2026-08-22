#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
infra="$root/infra"
fail() { printf 'infra policy: %s\n' "$1" >&2; exit 1; }

[[ -d "$infra" ]] || fail 'infra/ is missing'
rg -n 'max_instance_count\s*=\s*(?!1\b)[0-9]+' "$infra" --pcre2 >/dev/null && fail 'Cloud Run max_instance_count must be exactly 1'
rg -n 'min_instance_count\s*=\s*(?!0\b)[0-9]+' "$infra" --pcre2 >/dev/null && fail 'Cloud Run min_instance_count must be 0'
rg -n 'max_instance_count\s*=\s*1\b' "$infra" >/dev/null || fail 'Cloud Run max_instance_count=1 is missing'
rg -n 'min_instance_count\s*=\s*0\b' "$infra" >/dev/null || fail 'Cloud Run min_instance_count=0 is missing'
rg -n 'max_instance_request_concurrency\s*=\s*1\b' "$infra" >/dev/null || fail 'Cloud Run concurrency must be exactly 1'
rg -n 'cpu\s*=\s*"0\.08"' "$infra" >/dev/null || fail 'Cloud Run CPU must be 0.08'
rg -n 'memory\s*=\s*"512Mi"' "$infra" >/dev/null || fail 'Cloud Run memory must be 512Mi'
rg -n 'cpu_idle\s*=\s*true' "$infra" >/dev/null || fail 'request-based billing cpu_idle=true is missing'
[[ "$(rg '^resource "google_firestore_database"' "$infra"/*.tf | wc -l | tr -d ' ')" == "1" ]] || fail 'exactly one Firestore database resource is required'
rg -n '^\s*name\s*=\s*"\(default\)"' "$infra" >/dev/null || fail 'default Firestore database is missing'
rg -n 'iap_enabled\s*=\s*true' "$infra" >/dev/null || fail 'Cloud Run IAP must be enabled'
rg -n 'roles/run\.unauthenticated|member\s*=.*(allUsers|allAuthenticatedUsers)|members\s*=.*(allUsers|allAuthenticatedUsers)' "$infra" --pcre2 >/dev/null && fail 'public or unauthenticated IAM is forbidden'
rg -n 'data "google_project" "current"' "$infra" >/dev/null || fail 'project number lookup for IAP service agent is missing'
rg -n 'resource "google_cloud_run_v2_service_iam_member" "iap_service_agent_invoker"' "$infra" >/dev/null || fail 'IAP service agent invoker binding is missing'
rg -n 'gcp-sa-iap\.iam\.gserviceaccount\.com' "$infra" >/dev/null || fail 'Cloud Run invoker must be the IAP service agent'
rg -n 'resource "google_cloud_run_v2_service_iam_member" "iap_invoker"' "$infra" >/dev/null && fail 'IAP owners must not receive run.invoker directly'
rg -n 'image_digest|sha256:' "$infra" >/dev/null || fail 'Cloud Run image must be digest-pinned'
rg -n 'prevent_destroy\s*=\s*true|deletion_protection\s*=\s*true' "$infra" >/dev/null || fail 'destroy protection is missing'
rg -n 'Shards\s*=\s*32|Reads:\s*40_000|Writes:\s*16_000|Deletes:\s*16_000' "$root/internal/quota/quota.go" >/dev/null || fail 'bounded 80% Firestore reservation is missing'
rg -n 'ReadTransactionUsage|MutationUsage|MaxBoundedQueryReads|MaxReadBoundaryReads' "$root/internal/quota/quota.go" >/dev/null || fail 'conservative boundary I/O reservations are missing'

if command -v tofu >/dev/null 2>&1; then
  (cd "$infra" && tofu fmt -check -diff)
else
  printf 'infra policy: tofu not installed; structural checks passed\n'
fi
