#!/usr/bin/env bash
set -euo pipefail
# TFLint is intentionally not a floating external download in the pinned
# environment. The repository-local policy is the equivalent fail-closed
# static lint for this small, cost-sensitive module.
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/infra-policy.sh"
