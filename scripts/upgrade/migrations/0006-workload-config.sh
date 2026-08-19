#!/usr/bin/env bash
# id: 0006-workload-config
# risk: safe
# reversible: yes
# approval: no
# summary: .agentic-loop.toml の [queue] セクションへ、有限資源とスケーラビリティの
#   workload budget gate（`docs/decisions/0025-resource-scalability-budget.md`、詳細が
#   `docs/operations/workload-budget.md`）の既定値 `workload = "warn"` を追加します。
# recovery: 追加された `workload = "warn"` 行を削除するか、値を `off` へ書き換えて
#   ください。
#
# Contract (see docs/operations/upgrade.md): invoked as
#   0006-workload-config.sh <target-repository-root> check|apply
# check: exit 0 if already applied (nothing pending), exit 1 if apply is
#        pending. apply: exit 0 on success (including a no-op re-run), exit 1
#        on failure. Idempotent so a failed upgrade can simply be re-applied.
set -euo pipefail

target=$1
mode=$2
config="$target/.agentic-loop.toml"

applied() { [[ -r $config ]] && grep -Eq '^[[:space:]]*workload[[:space:]]*=' "$config"; }

case $mode in
  check) applied && exit 0 || exit 1 ;;
  apply)
    applied && exit 0
    [[ -f $config ]] || { printf '0006-workload-config: %s not found\n' "$config" >&2; exit 1; }
    tmp=$(mktemp)
    if grep -Eq '^\[queue\]' "$config"; then
      awk '
        /^\[queue\]/ && !done {
          print
          print "# Finite-resource/scalability workload budget gate (see"
          print "# docs/decisions/0025-resource-scalability-budget.md, docs/operations/"
          print "# workload-budget.md). require gates a missing/invalid record into"
          print "# agent:needs-input before exec runs; warn posts an audit/advisory"
          print "# comment and continues either way; off disables evaluation entirely."
          print "workload = \"warn\""
          done = 1
          next
        }
        { print }
      ' "$config" > "$tmp"
    else
      cp "$config" "$tmp"
      {
        printf '\n[queue]\n'
        printf '# Finite-resource/scalability workload budget gate (see\n'
        printf '# docs/decisions/0025-resource-scalability-budget.md, docs/operations/\n'
        printf '# workload-budget.md). require gates a missing/invalid record into\n'
        printf '# agent:needs-input before exec runs; warn posts an audit/advisory\n'
        printf '# comment and continues either way; off disables evaluation entirely.\n'
        printf 'workload = "warn"\n'
      } >> "$tmp"
    fi
    mv "$tmp" "$config"
    ;;
  *) printf '0006-workload-config: unknown mode: %s\n' "$mode" >&2; exit 1 ;;
esac
