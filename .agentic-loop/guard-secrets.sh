#!/usr/bin/env bash
set -euo pipefail

# Secret guard (Issue #61, ADR 0024: docs/decisions/0024-secret-scanning.md;
# operations: docs/operations/secret-scanning.md). Two layers, both fail the
# change (never fabricate a pass):
#   - baseline: a small dependency-free regex (SECRET_PATTERN/SENSITIVE_PATH)
#     that always runs, in every environment, including --text's hot path
#     (trace.sh/flaky.sh/preflight.sh call it repeatedly on short machine-
#     generated records and must never spawn a process for that).
#   - scanner: gitleaks (fixed version pinned in devbox.json), which covers
#     provider token/key shapes the baseline does not encode. It runs
#     whenever resolvable; see resolve_gitleaks for the 4-step order. Inside
#     the pinned devbox environment, or when AGENTIC_LOOP_SECRET_SCAN=required
#     is set (CI, worker full verification), an unresolvable scanner fails
#     the change instead of silently passing. Elsewhere (a host Git hook run
#     outside devbox) the baseline still runs and a warning is printed.
readonly SECRET_PATTERN='(AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-(proj-)?[A-Za-z0-9_-]{20,}|BEGIN [A-Z ]*PRIVATE KEY|xox[baprs]-[A-Za-z0-9-]{10,})'
readonly SENSITIVE_PATH='(^|/)(\.env($|\.)|id_(rsa|dsa|ecdsa|ed25519)$|credentials?\.(json|ya?ml)$|.*\.(pem|p12|pfx|key)$)'
readonly GITLEAKS_CONFIG='.agentic-loop/gitleaks.toml'
readonly GITLEAKS_ALLOWLIST_MAX=8
readonly GITLEAKS_LEAK_EXIT=92

fail() {
  printf 'Secret guard blocked the change: %s\n' "$1" >&2
  printf 'Remove the secret from Git, then rotate/revoke the credential if it was real.\n' >&2
  printf 'Do not paste the secret value into an Issue, PR, commit message, or log.\n' >&2
  exit 1
}

audit_fail() {
  printf 'gitleaks config audit blocked: %s\n' "$1" >&2
  printf 'Fix %s (see docs/operations/secret-scanning.md) and re-run --audit.\n' "$GITLEAKS_CONFIG" >&2
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
    fail 'credential-like content detected (baseline pattern)'
  fi
}

scan_text() {
  local file=$1
  if grep -E "$SECRET_PATTERN" "$file" >/dev/null 2>&1; then
    fail 'credential-like content detected (baseline pattern)'
  fi
}

# --- gitleaks resolution (ADR 0024): env override, PATH, this repository's
# own transient Devbox virtenv, then the install-owned persistent runtime
# virtenv (scripts/install-target.sh's provision_runtime_virtenv), in that
# order. Prints the resolved absolute path and returns 0, or returns 1.
guard_repo_root() { git rev-parse --show-toplevel 2>/dev/null; }
guard_git_common_dir() { git rev-parse --path-format=absolute --git-common-dir 2>/dev/null; }

resolve_gitleaks() {
  local repo_root common_dir candidate
  if [[ -n ${AGENTIC_LOOP_GITLEAKS:-} ]]; then
    [[ -x $AGENTIC_LOOP_GITLEAKS ]] && { printf '%s' "$AGENTIC_LOOP_GITLEAKS"; return 0; }
    return 1
  fi
  if command -v gitleaks >/dev/null 2>&1; then command -v gitleaks; return 0; fi
  repo_root=$(guard_repo_root) || repo_root=''
  candidate="$repo_root/.devbox/nix/profile/default/bin/gitleaks"
  if [[ -n $repo_root && -x $candidate ]]; then printf '%s' "$candidate"; return 0; fi
  common_dir=$(guard_git_common_dir) || common_dir=''
  candidate="$common_dir/agentic-loop/runtime/.devbox/nix/profile/default/bin/gitleaks"
  if [[ -n $common_dir && -x $candidate ]]; then printf '%s' "$candidate"; return 0; fi
  return 1
}

secret_scan_required() {
  [[ ${DEV_ENVIRONMENT:-} == agentic-loop-foundation-v2 || ${AGENTIC_LOOP_SECRET_SCAN:-} == required ]]
}

warn_degraded() {
  printf 'Secret guard degraded: gitleaks could not be resolved, so only the baseline pattern check ran for %s.\n' "$1" >&2
  printf 'Run "devbox install" (or install.sh) so gitleaks becomes available, or set AGENTIC_LOOP_GITLEAKS to its path.\n' >&2
}

# Summarize a redacted gitleaks JSON report (rule id, file, line only; the
# report was produced with --redact so Match/Secret are already "REDACTED").
gitleaks_summarize() {
  yq -p json -o tsv '.[] | [.RuleID, .File, .StartLine] | @tsv' "$1" 2>/dev/null |
    awk -F'\t' '{file=($2==""?"(tracked file; run --staged locally to see its name)":$2); printf "  - %s: %s:%s\n", $1, file, $3}' |
    head -n 5
}

# Runs `gitleaks $subcommand "$@" <fixed safety flags>`. Sets GITLEAKS_REPORT
# to a temp file the caller must rm. Returns 0 (clean), 2 (leak found; see
# GITLEAKS_REPORT), or 1 (scanner/config error -- never treated as clean).
gitleaks_invoke() {
  local gitleaks_bin=$1 repo_root=$2 subcommand=$3 rc=0 report errlog
  local -a config_flag=()
  shift 3
  # A missing repository-specific config (e.g. a fixture that only copies
  # this script, or a repository that has not yet received .agentic-loop/
  # gitleaks.toml) falls back to gitleaks' own embedded default rules rather
  # than a hard scanner-error; there is no repository governance to enforce
  # when there is no repository config to govern.
  [[ -f "$repo_root/$GITLEAKS_CONFIG" ]] && config_flag=(-c "$repo_root/$GITLEAKS_CONFIG")
  report=$(mktemp); errlog=$(mktemp)
  "$gitleaks_bin" "$subcommand" --no-banner --redact --exit-code "$GITLEAKS_LEAK_EXIT" \
    "${config_flag[@]}" -i "$repo_root" -f json -r "$report" "$@" >/dev/null 2>"$errlog" || rc=$?
  GITLEAKS_REPORT=$report
  case $rc in
    0) rm -f "$report" "$errlog"; return 0 ;;
    "$GITLEAKS_LEAK_EXIT") rm -f "$errlog"; return 2 ;;
    *) rm -f "$report" "$errlog"; return 1 ;;
  esac
}

# Fails with a summary (never the raw secret: the report was --redact'd) when
# gitleaks_invoke returned 2; fails with a generic scanner-error message
# (never gitleaks' raw stderr, which could echo file contents on decode
# errors) when it returned 1; a caller-supplied label appears in either.
gitleaks_handle() {
  local rc=$1 label=$2
  case $rc in
    0) return 0 ;;
    2)
      printf 'Secret guard blocked the change: gitleaks detected a likely secret in %s.\n' "$label" >&2
      gitleaks_summarize "$GITLEAKS_REPORT" >&2
      rm -f "$GITLEAKS_REPORT"
      printf 'Remove the secret from Git, then rotate/revoke the credential if it was real.\n' >&2
      printf 'Do not paste the secret value into an Issue, PR, commit message, or log.\n' >&2
      exit 1
      ;;
    *)
      printf 'Secret guard could not complete the gitleaks scan of %s (scanner or config error).\n' "$label" >&2
      printf 'Run "gitleaks dir -c %s ." locally to see the underlying error.\n' "$GITLEAKS_CONFIG" >&2
      exit 1
      ;;
  esac
}

# Runs the gitleaks layer for one (subcommand, args...) invocation, honoring
# the fail-closed/degrade split. label is used only in messages.
gitleaks_layer() {
  local label=$1 gitleaks_bin repo_root rc=0
  shift
  repo_root=$(guard_repo_root) || fail 'not inside a Git repository'
  if gitleaks_bin=$(resolve_gitleaks); then
    gitleaks_invoke "$gitleaks_bin" "$repo_root" "$@" || rc=$?
    gitleaks_handle "$rc" "$label"
  elif secret_scan_required; then
    local reason='AGENTIC_LOOP_SECRET_SCAN=required'
    [[ ${DEV_ENVIRONMENT:-} == agentic-loop-foundation-v2 ]] && reason='the pinned development environment'
    printf 'Secret guard blocked the change: gitleaks is required (%s) but could not be resolved.\n' "$reason" >&2
    printf 'Run "devbox install" (or install.sh) so gitleaks becomes available, or set AGENTIC_LOOP_GITLEAKS to its path.\n' >&2
    exit 1
  else
    warn_degraded "$label"
  fi
}

cmd_audit() {
  local repo_root config json total desc entry item
  repo_root=$(guard_repo_root) || fail 'not inside a Git repository'
  config="$repo_root/$GITLEAKS_CONFIG"
  [[ -f $config ]] || audit_fail "missing $GITLEAKS_CONFIG"
  command -v yq >/dev/null 2>&1 || fail 'yq is required for --audit'
  [[ -e "$repo_root/.gitleaksignore" ]] && audit_fail '.gitleaksignore is not permitted; use the reviewed allowlist in '"$GITLEAKS_CONFIG"' instead'
  json=$(yq -p toml -o json '.' "$config" 2>/dev/null) || audit_fail "$GITLEAKS_CONFIG is not valid TOML"
  [[ $(yq -r '.extend.useDefault // "false"' <<< "$json" 2>/dev/null) == true ]] ||
    audit_fail 'extend.useDefault must stay true (default rules must not be disabled)'

  total=0
  while IFS= read -r entry; do
    [[ -n $entry ]] || continue
    total=$((total + 1))
    desc=$(yq -r '.description // ""' <<< "$entry" 2>/dev/null)
    [[ -n $desc ]] || audit_fail 'an allowlist entry is missing a required description'
    while IFS= read -r item; do
      [[ -n $item ]] || continue
      case $item in
        '.*' | '.+' | '^.*$' | '^.+$' | '(?s).*' | '.' | '**')
          audit_fail "allowlist entry \"$desc\" uses an overly broad pattern: $item" ;;
      esac
    done < <(yq -r '(.regexes // [])[], (.paths // [])[]' <<< "$entry" 2>/dev/null)
  done < <(yq -o json -I=0 '(.allowlists // []) + [.rules[]? | select(has("allowlist")) | .allowlist] | .[]' <<< "$json" 2>/dev/null)

  [[ $total -le $GITLEAKS_ALLOWLIST_MAX ]] ||
    audit_fail "too many allowlist entries ($total, max $GITLEAKS_ALLOWLIST_MAX): keep exclusions minimal and reviewed"

  printf 'gitleaks config audit passed (%d allowlist entries).\n' "$total"
}

cmd_history() {
  local repo_root gitleaks_bin rc=0
  repo_root=$(guard_repo_root) || fail 'not inside a Git repository'
  gitleaks_bin=$(resolve_gitleaks) || {
    printf 'Secret guard could not run --history: gitleaks is required for full-history scanning and could not be resolved.\n' >&2
    printf 'Run "devbox install" (or install.sh) so gitleaks becomes available, or set AGENTIC_LOOP_GITLEAKS to its path.\n' >&2
    exit 1
  }
  gitleaks_invoke "$gitleaks_bin" "$repo_root" git "$repo_root" || rc=$?
  gitleaks_handle "$rc" 'the full Git history'
  printf 'No secrets found in Git history.\n'
}

main() {
  local patch
  if [[ ${1:-} == --text ]]; then
    [[ -n ${2:-} && -f $2 ]] || { printf 'Usage: %s --text FILE\n' "${0##*/}" >&2; exit 2; }
    scan_text "$2"
    return 0
  fi
  if [[ ${1:-} == --audit ]]; then cmd_audit; return 0; fi
  if [[ ${1:-} == --history ]]; then cmd_history; return 0; fi
  patch=$(mktemp)
  trap "rm -f '$patch'" EXIT
  case ${1:---all} in
    --staged)
      git diff --cached --name-only --diff-filter=ACMR | scan_names
      git diff --cached --no-ext-diff --unified=0 --diff-filter=ACMR > "$patch"
      scan_patch "$patch"
      gitleaks_layer 'staged changes' git --staged "$(guard_repo_root)"
      ;;
    --push)
      local local_ref local_sha remote_ref remote_sha shas
      while read -r local_ref local_sha remote_ref remote_sha; do
        [[ $local_sha == 0000000000000000000000000000000000000000 ]] && continue
        if [[ $remote_sha == 0000000000000000000000000000000000000000 ]]; then
          local commit
          while IFS= read -r commit; do
            git diff-tree --root --no-commit-id --name-only -r "$commit" | scan_names
            git show --format= --no-ext-diff --unified=0 "$commit" >> "$patch"
          done < <(git rev-list "$local_sha" --not --remotes)
          shas=$(git rev-list "$local_sha" --not --remotes | tr '\n' ' ')
          shas=${shas% }
          if [[ -n $shas ]]; then
            gitleaks_layer "pushed commits ($local_ref)" git --log-opts="--no-walk=unsorted $shas" "$(guard_repo_root)"
          fi
        else
          git diff --name-only --diff-filter=ACMR "$remote_sha..$local_sha" | scan_names
          git diff --no-ext-diff --unified=0 "$remote_sha..$local_sha" >> "$patch"
          gitleaks_layer "pushed commits ($local_ref)" git --log-opts="$remote_sha..$local_sha" "$(guard_repo_root)"
        fi
      done
      scan_patch "$patch"
      ;;
    --all)
      git ls-files | scan_names
      git diff --no-index -- /dev/null /dev/null > "$patch" || true
      while IFS= read -r file; do
        [[ -f $file ]] || continue
        git diff --no-index --unified=0 /dev/null "$file" >> "$patch" || true
      done < <(git ls-files)
      scan_patch "$patch"
      gitleaks_layer 'tracked files' stdin < "$patch"
      ;;
    *) printf 'Usage: %s [--staged|--push|--all|--text FILE|--history|--audit]\n' "${0##*/}" >&2; exit 2 ;;
  esac
}

main "$@"
