#!/usr/bin/env bash
# id: 0007-postmortem-config
# risk: safe
# reversible: yes
# approval: no
# summary: .agentic-loop.toml へ、closed-loop postmortem learning
#   （`docs/decisions/0026-postmortem-closed-loop.md`、詳細が
#   `docs/policies/postmortem.md`）の既定 `[postmortem]` セクション
#   （auto_detect = "on", max_auto_created_per_day = 5）を追加します。
# recovery: 追加された `[postmortem]` セクションを削除するか、
#   `auto_detect = "off"` へ書き換えてください。
#
# Contract (see docs/operations/upgrade.md): invoked as
#   0007-postmortem-config.sh <target-repository-root> check|apply
# check: exit 0 if already applied (nothing pending), exit 1 if apply is
#        pending. apply: exit 0 on success (including a no-op re-run), exit 1
#        on failure. Idempotent so a failed upgrade can simply be re-applied.
set -euo pipefail

target=$1
mode=$2
config="$target/.agentic-loop.toml"

applied() { [[ -r $config ]] && grep -Eq '^\[postmortem\]' "$config"; }

case $mode in
  check) applied && exit 0 || exit 1 ;;
  apply)
    applied && exit 0
    [[ -f $config ]] || { printf '0007-postmortem-config: %s not found\n' "$config" >&2; exit 1; }
    {
      printf '\n[postmortem]\n'
      printf '# Closed-loop postmortem learning (see docs/policies/postmortem.md,\n'
      printf '# docs/decisions/0026-postmortem-closed-loop.md). "on" lets the two\n'
      printf '# structurally cheap detection points (repeated-failure via park_issue,\n'
      printf '# resource-exhaustion via exhaustion_note_pause) create a dedup-checked\n'
      printf '# postmortem Issue; "off" disables automatic creation entirely (explicit\n'
      printf '# `bin/agentic-loop postmortem create` still works).\n'
      printf 'auto_detect = "on"\n'
      printf '# Bound on total auto-created postmortem Issues per UTC day (1-20).\n'
      printf 'max_auto_created_per_day = 5\n'
    } >> "$config"
    ;;
  *) printf '0007-postmortem-config: unknown mode: %s\n' "$mode" >&2; exit 1 ;;
esac
