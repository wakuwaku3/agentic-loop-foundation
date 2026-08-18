#!/usr/bin/env bash
# id: 0003-capability-manifest
# risk: safe
# reversible: yes
# approval: no
# summary: 既存の導入先へ .agentic-loop/capabilities.toml (repository capability
#   manifest) を、小さな検出テーブルから安全に初期化します(見出しが
#   docs/decisions/0018-repository-capability-manifest.md、詳細が
#   docs/operations/capability-manifest.md)。検出できなかった項目は値を
#   捏造せず undetermined へ列挙します。
# recovery: 生成された .agentic-loop/capabilities.toml を削除するか、内容を
#   手で編集してください。upgradeはこのfileを二度と上書き・削除しません
#   (class=init、target所有)。
#
# Contract (see docs/operations/upgrade.md): invoked as
#   0003-capability-manifest.sh <target-repository-root> check|apply
# check: exit 0 if already applied (nothing pending), exit 1 if apply is
#        pending. apply: exit 0 on success (including a no-op re-run), exit 1
#        on failure. Idempotent so a failed upgrade can simply be re-applied.
set -euo pipefail

target=$1
mode=$2
source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
# shellcheck source=../../../bin/lib/agentic-loop/capability.sh
source "$source_root/bin/lib/agentic-loop/capability.sh"

manifest="$target/.agentic-loop/capabilities.toml"

applied() { [[ -e $manifest ]]; }

case $mode in
  check) applied && exit 0 || exit 1 ;;
  apply)
    applied && exit 0
    capability_generate "$target"
    applied || { printf '0003-capability-manifest: failed to generate %s\n' "$manifest" >&2; exit 1; }
    ;;
  *) printf '0003-capability-manifest: unknown mode: %s\n' "$mode" >&2; exit 1 ;;
esac
