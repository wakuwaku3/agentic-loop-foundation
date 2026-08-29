#!/usr/bin/env bash
# id: 0008-foundation-local-state
# risk: safe
# reversible: yes
# approval: no
# summary: Git common dirのFoundation stateを有効にします。
# recovery: stateを削除すると旧manifest fallbackへ戻ります。
set -euo pipefail
target=$1; mode=$2
state_root="$(git -C "$target" rev-parse --path-format=absolute --git-common-dir)/agentic-loop"
state="$state_root/foundation-state.json"
legacy="$target/.agentic-loop/manifest.json"
case $mode in
  check) [[ -r $state || ! -r $legacy ]] && exit 0 || exit 1 ;;
  apply)
    [[ -r $state || ! -r $legacy ]] && exit 0
    mkdir -p "$state_root"
    printf '{"schema_version":1,"source":{"revision":""},"migration_level":8,"files":[],"history":[],"legacy_manifest":"%s"}\n' "$legacy" > "$state.tmp.$$"
    mv -f "$state.tmp.$$" "$state"
    ;;
  *) exit 1 ;;
esac
