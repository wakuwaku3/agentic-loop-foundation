#!/usr/bin/env bash
set -euo pipefail
list_changed() {
  {
    git diff --name-only "${1:-HEAD^}" HEAD 2>/dev/null || true
    git diff --name-only
    git diff --cached --name-only
    git ls-files --others --exclude-standard
  } | sed '/^$/d' | sort -u
}

if [[ "${1:-}" == "--list" ]]; then
  list_changed "${2:-HEAD^}"
  exit 0
fi

changed="$(list_changed "${1:-HEAD^}")"
go run ./cmd/ci-plan --execute --changed "$changed"
