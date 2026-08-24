#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${TFVARS_FILE:=$root/infra/terraform.tfvars}"
[[ -r "$TFVARS_FILE" ]] || { printf 'Set TFVARS_FILE to an explicit, non-secret tfvars file.\n' >&2; exit 2; }
: "${TF_STATE_BUCKET:=}"
[[ -n "$TF_STATE_BUCKET" ]] || { printf 'Set TF_STATE_BUCKET to the Cloud Storage bucket that holds OpenTofu state.\n' >&2; exit 2; }
cd "$root/infra"
tofu init -input=false -backend-config="bucket=$TF_STATE_BUCKET"
# -detailed-exitcode turns drift into a failing exit code (2) instead of a
# plan that is merely printed and ignored: 0 means no drift, 2 means drift,
# any other non-zero is a real error. -refresh-only never proposes a write.
tofu plan -refresh-only -input=false -detailed-exitcode -var-file="$TFVARS_FILE"
