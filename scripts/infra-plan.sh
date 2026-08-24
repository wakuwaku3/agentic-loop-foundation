#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${TFVARS_FILE:=$root/infra/terraform.tfvars}"
[[ -r "$TFVARS_FILE" ]] || { printf 'Set TFVARS_FILE to an explicit, non-secret tfvars file.\n' >&2; exit 2; }
: "${TF_STATE_BUCKET:=}"
[[ -n "$TF_STATE_BUCKET" ]] || { printf 'Set TF_STATE_BUCKET to the Cloud Storage bucket that holds OpenTofu state.\n' >&2; exit 2; }
cd "$root/infra"
mkdir -p build
tofu init -input=false -backend-config="bucket=$TF_STATE_BUCKET"
tofu plan -input=false -out=build/infra.tfplan -var-file="$TFVARS_FILE"
# tofu show -json is the reproducible artifact a human approves: its sha256
# becomes target.plan_digest in the preflight record, and the apply step
# must refuse to run unless the saved plan it is about to apply hashes to
# that same approved digest.
tofu show -json build/infra.tfplan >build/infra.plan.json
sha256sum build/infra.plan.json
