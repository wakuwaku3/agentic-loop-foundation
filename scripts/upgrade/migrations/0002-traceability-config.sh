#!/usr/bin/env bash
# id: 0002-traceability-config
# risk: safe
# reversible: yes
# approval: no
# summary: .agentic-loop.toml の [queue] セクションへ、要求・変更・検証のトレー
#   サビリティgateの既定mode `traceability = "warn"` を追加します(見出しが
#   `docs/decisions/0016-requirement-traceability.md`、詳細が
#   `docs/operations/traceability.md`)。
# recovery: 追加された `traceability = "warn"` 行を削除するか、値を `off` へ
#   書き換えてください。
#
# Contract (see docs/operations/upgrade.md): invoked as
#   0002-traceability-config.sh <target-repository-root> check|apply
# check: exit 0 if already applied (nothing pending), exit 1 if apply is
#        pending. apply: exit 0 on success (including a no-op re-run), exit 1
#        on failure. Idempotent so a failed upgrade can simply be re-applied.
set -euo pipefail

target=$1
mode=$2
config="$target/.agentic-loop.toml"

applied() { [[ -r $config ]] && grep -Eq '^[[:space:]]*traceability[[:space:]]*=' "$config"; }

case $mode in
  check) applied && exit 0 || exit 1 ;;
  apply)
    applied && exit 0
    [[ -f $config ]] || { printf '0002-traceability-config: %s not found\n' "$config" >&2; exit 1; }
    tmp=$(mktemp)
    if grep -Eq '^\[queue\]' "$config"; then
      awk '
        /^\[queue\]/ && !done {
          print
          print "# Requirement traceability gate (see docs/decisions/0016-requirement-traceability.md,"
          print "# docs/operations/traceability.md). require blocks completion on a missing/invalid"
          print "# record without closing the Issue; warn only posts an advisory comment; off disables"
          print "# evaluation entirely."
          print "traceability = \"warn\""
          done = 1
          next
        }
        { print }
      ' "$config" > "$tmp"
    else
      cp "$config" "$tmp"
      {
        printf '\n[queue]\n'
        printf '# Requirement traceability gate (see docs/decisions/0016-requirement-traceability.md,\n'
        printf '# docs/operations/traceability.md). require blocks completion on a missing/invalid\n'
        printf '# record without closing the Issue; warn only posts an advisory comment; off disables\n'
        printf '# evaluation entirely.\n'
        printf 'traceability = "warn"\n'
      } >> "$tmp"
    fi
    mv "$tmp" "$config"
    ;;
  *) printf '0002-traceability-config: unknown mode: %s\n' "$mode" >&2; exit 1 ;;
esac
