#!/usr/bin/env bash
# shellcheck disable=SC2155
# flaky test検知・限定的隔離（Issue #60, ADR 0022）。
#
# testは常に実行する。このscriptが変えるのはverdict=flakyの終了codeだけで、
# 選択・skip・除外用のinterfaceは一切持たない。classifyはtests/run-e2e.shが
# 各群の試行結果からverdictを決定するために呼ぶ。auditはtests/flaky-registry.
# tomlの整合性だけを検査する（testは実行しない）。除外用のoptionは存在しない
# （docs/policies/validation-harness.mdおよびdocs/decisions/0022参照）。
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
readonly PROJECT_ROOT="$PWD"
# shellcheck source=bin/lib/agentic-loop/flaky.sh
source "$PROJECT_ROOT/bin/lib/agentic-loop/flaky.sh"

registry_path="$PROJECT_ROOT/tests/flaky-registry.toml"
secret_guard="$PROJECT_ROOT/.agentic-loop/guard-secrets.sh"

usage() {
  cat >&2 <<'EOF'
使い方:
  scripts/flaky.sh classify --unit UNIT --attempt CONTEXT:EXIT:LOGFILE [--attempt ...] [--registry PATH]
  scripts/flaky.sh audit [--registry PATH]

除外・skip用のoptionは存在しない（実行が不安定であることを理由にした除外を許さないため）。
EOF
}

json_escape() {
  local value=$1
  value=${value//\\/\\\\}; value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  printf '%s' "$value"
}

[[ $# -ge 1 ]] || { usage; exit 2; }
sub=$1
shift

case $sub in
  classify)
    unit=''
    declare -a attempt_specs=()
    while [[ $# -gt 0 ]]; do
      case $1 in
        --unit)
          [[ $# -ge 2 ]] || { usage; exit 2; }
          unit=$2; shift 2 ;;
        --attempt)
          [[ $# -ge 2 ]] || { usage; exit 2; }
          attempt_specs+=("$2"); shift 2 ;;
        --registry)
          [[ $# -ge 2 ]] || { usage; exit 2; }
          registry_path=$2; shift 2 ;;
        -h | --help)
          usage; exit 0 ;;
        *)
          printf 'flaky.sh: unknown argument: %s\n' "$1" >&2
          usage; exit 2 ;;
      esac
    done
    [[ -n $unit ]] || { usage; exit 2; }
    (( ${#attempt_specs[@]} >= 1 )) || { usage; exit 2; }

    # A missing or unparsable registry is never a classify-time error: it
    # just means no quarantine entry can match (audit is the only place a
    # broken registry is reported as a failure).
    flaky_registry_load "$registry_path" || true

    declare -a ctxs=() exits=() logs=()
    for spec in "${attempt_specs[@]}"; do
      ctx=${spec%%:*}
      rest=${spec#*:}
      ex=${rest%%:*}
      log=${rest#*:}
      case $ctx in co-run | isolated) ;; *) printf 'flaky.sh: invalid --attempt context: %s\n' "$ctx" >&2; exit 2 ;; esac
      [[ $ex =~ ^[0-9]+$ ]] || { printf 'flaky.sh: invalid --attempt exit code: %s\n' "$ex" >&2; exit 2; }
      ctxs+=("$ctx"); exits+=("$ex"); logs+=("$log")
    done

    n=${#ctxs[@]}
    fingerprint=''
    hint=''
    if [[ ${exits[0]} == 0 ]]; then
      verdict=passed
    elif (( n == 1 )); then
      fingerprint=$(flaky_fingerprint "${logs[0]}")
      if [[ $fingerprint == unknown ]]; then verdict=failing-unknown; else verdict=failing; fi
    else
      fingerprint=$(flaky_fingerprint "${logs[0]}")
      if [[ ${exits[1]} != 0 ]]; then
        if [[ $fingerprint == unknown ]]; then verdict=failing-unknown; else verdict=failing; fi
      else
        hint=intermittent
        if (( n >= 3 )) && [[ ${exits[2]} == 0 ]]; then hint=isolation-sensitive; fi
        if [[ $fingerprint == unknown ]]; then verdict=flaky-unknown; else verdict=flaky; fi
      fi
    fi

    quarantined=0
    q_issue='' q_owner='' q_until=''
    removal_candidate=0
    case $verdict in
      passed)
        flaky_registry_has_unit "$unit" && removal_candidate=1 ;;
      flaky)
        if IFS="$FLAKY_FS" read -r q_issue q_owner q_until < <(flaky_registry_match "$unit" "$fingerprint"); then
          quarantined=1
        fi ;;
    esac

    case $verdict in
      passed) exit_code=0 ;;
      flaky) exit_code=$(( quarantined ? 0 : 1 )) ;;
      *) exit_code=1 ;;
    esac

    attempts_json=''
    sep=''
    for i in "${!ctxs[@]}"; do
      idx=$((i + 1))
      fl=''
      [[ ${exits[$i]} != 0 ]] && fl=$(flaky_fail_line "${logs[$i]}")
      attempts_json+="${sep}{\"index\":$idx,\"context\":\"${ctxs[$i]}\",\"exit\":${exits[$i]},\"fail_line\":\"$(json_escape "$fl")\"}"
      sep=','
    done

    printf '{"schema":1,"unit":"%s","fingerprint":"%s","verdict":"%s","hints":[%s],"quarantined":%s' \
      "$(json_escape "$unit")" "$(json_escape "$fingerprint")" "$verdict" \
      "$([[ -n $hint ]] && printf '"%s"' "$hint")" \
      "$([[ $quarantined == 1 ]] && printf true || printf false)"
    if (( quarantined == 1 )); then
      printf ',"quarantine":{"issue":%s,"owner":"%s","until":"%s"}' "$q_issue" "$(json_escape "$q_owner")" "$q_until"
    fi
    printf ',"removal_candidate":%s,"attempts":[%s]}\n' "$([[ $removal_candidate == 1 ]] && printf true || printf false)" "$attempts_json"

    if (( quarantined == 1 )); then
      printf 'flaky隔離を適用しました: unit=%s fingerprint=%s issue=#%s owner=%s until=%s\n' "$unit" "$fingerprint" "$q_issue" "$q_owner" "$q_until"
      printf 'flaky隔離を適用しました: unit=%s fingerprint=%s issue=#%s owner=%s until=%s\n' "$unit" "$fingerprint" "$q_issue" "$q_owner" "$q_until" >&2
    fi
    if (( removal_candidate == 1 )); then
      printf '%s は隔離entryが登録されていますが今回は成功しました。修復済みであればentryを削除してください。\n' "$unit"
    fi
    exit "$exit_code"
    ;;
  audit)
    while [[ $# -gt 0 ]]; do
      case $1 in
        --registry)
          [[ $# -ge 2 ]] || { usage; exit 2; }
          registry_path=$2; shift 2 ;;
        -h | --help)
          usage; exit 0 ;;
        *)
          printf 'flaky.sh: unknown argument: %s\n' "$1" >&2
          usage; exit 2 ;;
      esac
    done
    rc=0
    flaky_registry_validate "$registry_path" "$secret_guard" || rc=$?
    for i in "${!FLAKY_LEVELS[@]}"; do
      printf '[%s] %s\n' "${FLAKY_LEVELS[$i]}" "${FLAKY_MESSAGES[$i]}" >&2
    done
    if (( rc == 0 )); then printf 'flaky-audit: passed\n'; fi
    exit "$rc"
    ;;
  -h | --help)
    usage; exit 0 ;;
  *)
    printf 'flaky.sh: unknown subcommand: %s\n' "$sub" >&2
    usage; exit 2 ;;
esac
