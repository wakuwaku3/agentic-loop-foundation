#!/usr/bin/env bash
# id: 0005-preflight-config
# risk: safe
# reversible: yes
# approval: no
# summary: .agentic-loop.toml の [queue] セクションへ、変更影響とリスクのpreflight
#   gate（`docs/decisions/0020-change-risk-preflight.md`、詳細が
#   `docs/operations/preflight.md`）の既定値 `preflight = "warn"` を追加します。
# recovery: 追加された `preflight = "warn"` 行を削除するか、値を `off` へ書き換えて
#   ください。
#
# Contract (see docs/operations/upgrade.md): invoked as
#   0005-preflight-config.sh <target-repository-root> check|apply
# check: exit 0 if already applied (nothing pending), exit 1 if apply is
#        pending. apply: exit 0 on success (including a no-op re-run), exit 1
#        on failure. Idempotent so a failed upgrade can simply be re-applied.
set -euo pipefail

target=$1
mode=$2
config="$target/.agentic-loop.toml"

applied() { [[ -r $config ]] && grep -Eq '^[[:space:]]*preflight[[:space:]]*=' "$config"; }

case $mode in
  check) applied && exit 0 || exit 1 ;;
  apply)
    applied && exit 0
    [[ -f $config ]] || { printf '0005-preflight-config: %s not found\n' "$config" >&2; exit 1; }
    tmp=$(mktemp)
    if grep -Eq '^\[queue\]' "$config"; then
      awk '
        /^\[queue\]/ && !done {
          print
          print "# Change-risk preflight gate (see docs/decisions/0020-change-risk-preflight.md,"
          print "# docs/operations/preflight.md). require gates every non-autonomous verdict"
          print "# (including a missing/invalid record) before exec runs; warn gates only"
          print "# genuinely risky verdicts and otherwise posts an audit/advisory comment; off"
          print "# disables evaluation entirely."
          print "preflight = \"warn\""
          done = 1
          next
        }
        { print }
      ' "$config" > "$tmp"
    else
      cp "$config" "$tmp"
      {
        printf '\n[queue]\n'
        printf '# Change-risk preflight gate (see docs/decisions/0020-change-risk-preflight.md,\n'
        printf '# docs/operations/preflight.md). require gates every non-autonomous verdict\n'
        printf '# (including a missing/invalid record) before exec runs; warn gates only\n'
        printf '# genuinely risky verdicts and otherwise posts an audit/advisory comment; off\n'
        printf '# disables evaluation entirely.\n'
        printf 'preflight = "warn"\n'
      } >> "$tmp"
    fi
    mv "$tmp" "$config"
    ;;
  *) printf '0005-preflight-config: unknown mode: %s\n' "$mode" >&2; exit 1 ;;
esac
