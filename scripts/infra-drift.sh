#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${TFVARS_FILE:=$root/infra/terraform.tfvars}"
[[ -r "$TFVARS_FILE" ]] || { printf 'Set TFVARS_FILE to an explicit, non-secret tfvars file.\n' >&2; exit 2; }
cd "$root/infra"
tofu init -input=false
tofu plan -refresh-only -input=false -var-file="$TFVARS_FILE"
