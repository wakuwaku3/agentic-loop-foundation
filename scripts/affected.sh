#!/usr/bin/env bash
set -euo pipefail
changed="$(git diff --name-only "${1:-HEAD^}" HEAD; git diff --name-only; git ls-files --others --exclude-standard)"
devbox run --pure -- go run ./cmd/ci-plan --execute --changed "$changed"
