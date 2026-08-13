#!/usr/bin/env bash
# id: 0001-foundation-config-section
# risk: safe
# reversible: yes
# approval: no
# summary: .agentic-loop.toml に [foundation] セクション（配布元repository・pin対象revision・完全検証コマンド）を追加します。
# recovery: 追加された [foundation] セクションを削除するか、`agentic-loop upgrade --rollback` を実行してください。
#
# Contract (see docs/operations/upgrade.md): invoked as
#   0001-foundation-config-section.sh <target-repository-root> check|apply
# check: exit 0 if already applied (nothing pending), exit 1 if apply is
#        pending. apply: exit 0 on success (including a no-op re-run), exit 1
#        on failure. Idempotent so a failed upgrade can simply be re-applied.
set -euo pipefail

target=$1
mode=$2
config="$target/.agentic-loop.toml"

applied() { [[ -r $config ]] && grep -Eq '^\[foundation\]' "$config"; }

case $mode in
  check) applied && exit 0 || exit 1 ;;
  apply)
    applied && exit 0
    [[ -f $config ]] || { printf '0001-foundation-config-section: %s not found\n' "$config" >&2; exit 1; }
    cat >> "$config" <<'EOF'

[foundation]
# Foundationの配布元repository。`agentic-loop upgrade` はここを基準に新しいrevisionを取得する。
repository = "wakuwaku3/agentic-loop-foundation"
# 適用済み・pin対象のrevision（40桁SHA）。空のままだとupgradeは未検証のrevisionへ
# 暗黙追従せず、明示的な --revision または --source を要求する。
revision = ""
# upgrade適用後に実行する完全検証コマンド。失敗時は自動rollbackせず、
# `agentic-loop upgrade --rollback` の案内を提示したまま適用状態を保持する。
verify_command = "devbox run --pure check"
EOF
    ;;
  *) printf '0001-foundation-config-section: unknown mode: %s\n' "$mode" >&2; exit 1 ;;
esac
