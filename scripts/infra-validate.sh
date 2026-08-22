#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root/infra"
tofu fmt -check -diff
tofu init -backend=false -input=false
tofu validate
