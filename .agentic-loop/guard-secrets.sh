#!/usr/bin/env bash
set -euo pipefail

readonly SECRET_PATTERN='(AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-(proj-)?[A-Za-z0-9_-]{20,}|BEGIN [A-Z ]*PRIVATE KEY|xox[baprs]-[A-Za-z0-9-]{10,})'
readonly SENSITIVE_PATH='(^|/)(\.env($|\.)|id_(rsa|dsa|ecdsa|ed25519)$|credentials?\.(json|ya?ml)$|.*\.(pem|p12|pfx|key)$)'

fail() {
  printf 'Secret guard blocked the change: %s\n' "$1" >&2
  printf 'Remove the secret from Git and rotate it if it was real.\n' >&2
  exit 1
}

scan_names() {
  local name
  while IFS= read -r name; do
    [[ $name =~ $SENSITIVE_PATH ]] && fail "sensitive path: $name"
  done
  return 0
}

scan_patch() {
  local patch=$1
  if grep -E '^\+[^+]' "$patch" | grep -E "$SECRET_PATTERN" >/dev/null; then
    fail 'credential-like content detected'
  fi
}

scan_text() {
  local file=$1
  if grep -E "$SECRET_PATTERN" "$file" >/dev/null 2>&1; then
    fail 'credential-like content detected'
  fi
}

main() {
  local patch
  if [[ ${1:-} == --text ]]; then
    [[ -n ${2:-} && -f $2 ]] || { printf 'Usage: %s --text FILE\n' "${0##*/}" >&2; exit 2; }
    scan_text "$2"
    return 0
  fi
  patch=$(mktemp)
  trap "rm -f '$patch'" EXIT
  case ${1:---all} in
    --staged)
      git diff --cached --name-only --diff-filter=ACMR | scan_names
      git diff --cached --no-ext-diff --unified=0 --diff-filter=ACMR > "$patch"
      ;;
    --push)
      local local_ref local_sha remote_ref remote_sha
      while read -r local_ref local_sha remote_ref remote_sha; do
        [[ $local_sha == 0000000000000000000000000000000000000000 ]] && continue
        if [[ $remote_sha == 0000000000000000000000000000000000000000 ]]; then
          local commit
          while IFS= read -r commit; do
            git diff-tree --root --no-commit-id --name-only -r "$commit" | scan_names
            git show --format= --no-ext-diff --unified=0 "$commit" >> "$patch"
          done < <(git rev-list "$local_sha" --not --remotes)
        else
          git diff --name-only --diff-filter=ACMR "$remote_sha..$local_sha" | scan_names
          git diff --no-ext-diff --unified=0 "$remote_sha..$local_sha" >> "$patch"
        fi
      done
      ;;
    --all)
      git ls-files | scan_names
      git diff --no-index -- /dev/null /dev/null > "$patch" || true
      while IFS= read -r file; do
        [[ -f $file ]] || continue
        git diff --no-index --unified=0 /dev/null "$file" >> "$patch" || true
      done < <(git ls-files)
      ;;
    *) printf 'Usage: %s [--staged|--push|--all|--text FILE]\n' "${0##*/}" >&2; exit 2 ;;
  esac
  scan_patch "$patch"
}

main "$@"
