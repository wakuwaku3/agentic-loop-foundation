#!/usr/bin/env bash
# id: 0004-pause-control-config
# risk: safe
# reversible: yes
# approval: no
# summary: .agentic-loop.toml の [queue] セクションへ、Issue単位のpause/resume/abort
#   （`docs/decisions/0019-issue-level-execution-control.md`）が使う
#   `pause_grace_seconds = 120` の既定値を追加します。
# recovery: 追加された `pause_grace_seconds = 120` 行を削除するか、値を書き換えて
#   ください。
#
# Contract (see docs/operations/upgrade.md): invoked as
#   0004-pause-control-config.sh <target-repository-root> check|apply
# check: exit 0 if already applied (nothing pending), exit 1 if apply is
#        pending. apply: exit 0 on success (including a no-op re-run), exit 1
#        on failure. Idempotent so a failed upgrade can simply be re-applied.
set -euo pipefail

target=$1
mode=$2
config="$target/.agentic-loop.toml"

applied() { [[ -r $config ]] && grep -Eq '^[[:space:]]*pause_grace_seconds[[:space:]]*=' "$config"; }

case $mode in
  check) applied && exit 0 || exit 1 ;;
  apply)
    applied && exit 0
    [[ -f $config ]] || { printf '0004-pause-control-config: %s not found\n' "$config" >&2; exit 1; }
    tmp=$(mktemp)
    if grep -Eq '^\[queue\]' "$config"; then
      awk '
        /^\[queue\]/ && !done {
          print
          print "# `pause`/`abort` drain the owning host'"'"'s worker cooperatively (a stop-request"
          print "# file the worker checks at its own stage boundaries) for up to this many"
          print "# seconds before escalating to TERM, then 5s later to KILL, so an unsafe-to-"
          print "# interrupt section gets a bounded chance to finish on its own first."
          print "pause_grace_seconds = 120"
          done = 1
          next
        }
        { print }
      ' "$config" > "$tmp"
    else
      cp "$config" "$tmp"
      {
        printf '\n[queue]\n'
        printf '# `pause`/`abort` drain the owning host'"'"'s worker cooperatively (a stop-request\n'
        printf '# file the worker checks at its own stage boundaries) for up to this many\n'
        printf '# seconds before escalating to TERM, then 5s later to KILL, so an unsafe-to-\n'
        printf '# interrupt section gets a bounded chance to finish on its own first.\n'
        printf 'pause_grace_seconds = 120\n'
      } >> "$tmp"
    fi
    mv "$tmp" "$config"
    ;;
  *) printf '0004-pause-control-config: unknown mode: %s\n' "$mode" >&2; exit 1 ;;
esac
