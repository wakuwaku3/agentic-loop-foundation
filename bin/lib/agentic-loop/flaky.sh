# Module: flaky.sh. Sourced by bin/agentic-loop (see docs/decisions/
# 0013-agentic-loop-modules.md). Like capability.sh, the registry validation
# and lookup functions below (flaky_registry_* / flaky_fingerprint / flaky_
# fail_line) take explicit path arguments and depend on nothing but yq, date,
# and sha256sum, so scripts/flaky.sh and tests/test-agentic-loop.sh can
# `source` them standalone, outside bin/agentic-loop's global state. Only
# cmd_flaky/cmd_flaky_report and the *_render_* helpers rely on the wider
# bin/agentic-loop context (REPO_ROOT, repo_api, search_issues_api, repo_name,
# common.sh's json_escape).
# See docs/decisions/0022-flaky-test-detection-and-quarantine.md and
# docs/operations/flaky-tests.md.
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034,SC2153

readonly FLAKY_SCHEMA_VERSION=1
readonly FLAKY_MAX_ACTIVE_ENTRIES=3
readonly FLAKY_MAX_SPAN_DAYS=14
readonly FLAKY_MESSAGE_MAX_BYTES=200
# ASCII unit separator, not tab: bash's `read` folds consecutive IFS
# whitespace (space/tab/newline, even when IFS is set to only one of them)
# into a single delimiter, silently shifting fields when an interior column
# (e.g. an empty fingerprint or reason) is empty. 0x1F is never IFS
# whitespace, so an empty column always survives as its own field.
readonly FLAKY_FS=$'\x1f'

flaky_registry_file() { printf '%s/tests/flaky-registry.toml' "$1"; }


# --- registry parsing (mirrors capability_load) -----------------------------
# Return codes: 0 = loaded, 2 = file does not exist (registry is optional for
# a repository that has not adopted this feature yet), 1 = present but
# unreadable/unparsable, or yq is missing.
flaky_registry_load() {
  local path=$1
  FLAKY_REGISTRY_JSON=''
  [[ -e $path ]] || return 2
  [[ -r $path ]] || return 1
  command -v yq >/dev/null 2>&1 || return 1
  FLAKY_REGISTRY_JSON=$(yq -p toml -o json "$path" 2>/dev/null) || return 1
  [[ -n $FLAKY_REGISTRY_JSON ]] || return 1
  return 0
}


flaky_registry_query() { yq -p json -r "$1" <<< "$FLAKY_REGISTRY_JSON" 2>/dev/null; }


# unit<FS>fingerprint<FS>message<FS>issue<FS>owner<FS>first_seen<FS>until per
# declared entry, FS-joined (see FLAKY_FS) rather than @tsv so an empty
# interior column (most commonly fingerprint) is never lost.
flaky_registry_entries_tsv() {
  flaky_registry_query "$(printf '.entry[]? | [.unit, .fingerprint, (.message // ""), (.issue // ""), (.owner // ""), (.first_seen // ""), (.until // "")] | join("%s")' "$FLAKY_FS")"
}


flaky_today() { date -u +%Y-%m-%d; }

flaky_date_valid() { [[ $1 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] && date -u -d "$1" >/dev/null 2>&1; }

flaky_date_epoch() { date -u -d "$1" +%s 2>/dev/null; }


# --- failure fingerprinting --------------------------------------------------
# The first `FAIL: ...` line in a run-e2e.sh attempt log (tests/test-agentic-
# loop.sh's fail() is the only source of this format), or empty when absent
# (a harness crash under set -e, never fingerprintable).
flaky_fail_line() { grep -m1 '^FAIL: ' "$1" 2>/dev/null || true; }


# 12-hex fingerprint of the first FAIL: line, or the literal "unknown" when
# the log has none. Quarantine never matches "unknown" (see flaky_registry_
# match): a failure that cannot be fingerprinted can never be isolated.
flaky_fingerprint() {
  local line
  line=$(flaky_fail_line "$1")
  if [[ -z $line ]]; then
    printf 'unknown'
  else
    printf '%s' "$line" | sha256sum | cut -c1-12
  fi
}


# --- registry lookup (never trusts an expired or malformed entry) ----------
# flaky_registry_match UNIT FINGERPRINT -> "issue<FS>owner<FS>until" (see
# FLAKY_FS) for a non-expired exact (unit, fingerprint) match; returns 1
# otherwise. Requires
# flaky_registry_load to have already populated FLAKY_REGISTRY_JSON. A
# decisive/unfingerprintable failure (fingerprint=unknown) never matches.
flaky_registry_match() {
  local unit=$1 fingerprint=$2 today r_unit r_fp r_issue r_owner r_until
  [[ $fingerprint != unknown && $fingerprint =~ ^[0-9a-f]{12}$ ]] || return 1
  [[ -n ${FLAKY_REGISTRY_JSON:-} ]] || return 1
  today=$(flaky_today)
  while IFS="$FLAKY_FS" read -r r_unit r_fp _ r_issue r_owner _ r_until; do
    [[ $r_unit == "$unit" && $r_fp == "$fingerprint" ]] || continue
    flaky_date_valid "$r_until" || continue
    [[ $r_until > $today ]] || continue
    printf '%s%s%s%s%s\n' "$r_issue" "$FLAKY_FS" "$r_owner" "$FLAKY_FS" "$r_until"
    return 0
  done < <(flaky_registry_entries_tsv)
  return 1
}


# Whether any entry (any fingerprint, any expiry) already declares this unit
# -- used to flag a "repaired" removal candidate when a previously-flaky unit
# passes attempt1 cleanly again.
flaky_registry_has_unit() {
  local unit=$1
  [[ -n ${FLAKY_REGISTRY_JSON:-} ]] || return 1
  flaky_registry_entries_tsv | awk -F"$FLAKY_FS" -v u="$unit" '$1 == u { found=1 } END { exit !found }'
}


# --- registry validation (mirrors capability_validate/doctor_add) ----------
flaky_add() {
  local level=$1 code=$2 message=$3
  FLAKY_LEVELS+=("$level"); FLAKY_CODES+=("$code"); FLAKY_MESSAGES+=("$message")
  case $level in warning) FLAKY_WARNINGS=$((FLAKY_WARNINGS + 1)) ;; failure) FLAKY_FAILURES=$((FLAKY_FAILURES + 1)) ;; esac
}


# flaky_registry_validate PATH [SECRET_GUARD] -> populates FLAKY_LEVELS/CODES/
# MESSAGES/WARNINGS/FAILURES and returns 1 iff at least one entry is level=
# failure. A missing registry is not itself a failure (a repository that
# predates this feature has none yet); a present-but-broken one always is.
flaky_registry_validate() {
  local path=$1 secret_guard=${2:-} rc=0
  declare -ga FLAKY_LEVELS=() FLAKY_CODES=() FLAKY_MESSAGES=()
  declare -g FLAKY_WARNINGS=0 FLAKY_FAILURES=0
  flaky_registry_load "$path"; rc=$?
  if (( rc == 2 )); then
    flaky_add warning not-installed 'tests/flaky-registry.toml が存在しません（隔離entryが無いだけであり、testは常に実行されます）。'
    return 0
  elif (( rc != 0 )); then
    flaky_add failure unreadable 'tests/flaky-registry.toml を解釈できません（TOML構文が不正か、yqが利用できません）。'
    return 1
  fi

  local schema_version
  schema_version=$(flaky_registry_query '.schema_version // ""')
  [[ $schema_version == "$FLAKY_SCHEMA_VERSION" ]] || {
    flaky_add failure schema-version "schema_versionが不正です（宣言値: ${schema_version:-空}、対応値: $FLAKY_SCHEMA_VERSION）。"
    return 1
  }

  local today active=0
  today=$(flaky_today)
  local unit fingerprint message issue owner first_seen until_date label
  while IFS="$FLAKY_FS" read -r unit fingerprint message issue owner first_seen until_date; do
    [[ -n $unit || -n $fingerprint ]] || continue
    label="entry unit=${unit:-?} fingerprint=${fingerprint:-?}"
    if [[ -z $unit || -z $fingerprint || -z $message || -z $issue || -z $owner || -z $first_seen || -z $until_date ]]; then
      flaky_add failure incomplete-entry "$label は必須fieldが不足しています（unit/fingerprint/message/issue/owner/first_seen/untilすべて必須）。"
      continue
    fi
    [[ $fingerprint =~ ^[0-9a-f]{12}$ ]] || flaky_add failure invalid-fingerprint "$label のfingerprintが12桁16進ではありません。"
    [[ $issue =~ ^[1-9][0-9]*$ ]] || flaky_add failure invalid-issue "$label のissueが正の整数ではありません。"
    if [[ ${#message} -gt $FLAKY_MESSAGE_MAX_BYTES || $message == *$'\n'* || $message == *'`'* ]]; then
      flaky_add failure invalid-message "$label のmessageが${FLAKY_MESSAGE_MAX_BYTES}字超、改行、またはbacktickを含みます。"
    elif [[ -n $secret_guard && -x $secret_guard ]]; then
      local tmp_msg
      tmp_msg=$(mktemp)
      printf '%s' "$message" > "$tmp_msg"
      "$secret_guard" --text "$tmp_msg" >/dev/null 2>&1 || flaky_add failure secret-like-message "$label のmessageに秘密様の文字列が含まれています。"
      rm -f "$tmp_msg"
    fi
    if ! flaky_date_valid "$first_seen"; then
      flaky_add failure invalid-date "$label のfirst_seenがYYYY-MM-DD形式ではありません。"
    fi
    if ! flaky_date_valid "$until_date"; then
      flaky_add failure invalid-date "$label のuntilがYYYY-MM-DD形式ではありません。"
      continue
    fi
    if [[ $until_date > $today ]]; then
      local days_left=$(( ( $(flaky_date_epoch "$until_date") - $(flaky_date_epoch "$today") ) / 86400 ))
      active=$((active + 1))
      if (( days_left <= 3 )); then
        flaky_add warning expiring-soon "$label は${days_left}日後に期限切れです（issue=#$issue owner=$owner）。延長するか修復してentryを削除してください。"
      fi
    else
      flaky_add failure expired "$label は期限切れです（until=$until_date）。隔離を継続するには期限を延長（最大14日）してください。"
    fi
    if flaky_date_valid "$first_seen"; then
      local span_days=$(( ( $(flaky_date_epoch "$until_date") - $(flaky_date_epoch "$first_seen") ) / 86400 ))
      if (( span_days < 0 || span_days > FLAKY_MAX_SPAN_DAYS )); then
        flaky_add failure span-too-long "$label のuntilがfirst_seenから${FLAKY_MAX_SPAN_DAYS}日を超えています（無期限隔離の禁止）。"
      fi
    fi
    local from_today_days=$(( ( $(flaky_date_epoch "$until_date") - $(flaky_date_epoch "$today") ) / 86400 ))
    if (( from_today_days > FLAKY_MAX_SPAN_DAYS )); then
      flaky_add failure too-far "$label のuntilが今日から${FLAKY_MAX_SPAN_DAYS}日を超えています（無期限隔離の禁止）。"
    fi
  done < <(flaky_registry_entries_tsv)

  if (( active > FLAKY_MAX_ACTIVE_ENTRIES )); then
    flaky_add failure too-many-active "有効な隔離entryが${FLAKY_MAX_ACTIVE_ENTRIES}件を超えています（現在: ${active}件）。"
  fi
  (( FLAKY_FAILURES == 0 ))
}


# --- rendering (bin/agentic-loop context: common.sh's json_escape) ---------
flaky_render_json() {
  local index sep=''
  printf '{"schema_version":1,"registry_valid":%s,"summary":{"failure":%d,"warning":%d},"findings":[' \
    "$([[ $FLAKY_FAILURES -eq 0 ]] && printf true || printf false)" "$FLAKY_FAILURES" "$FLAKY_WARNINGS"
  for index in "${!FLAKY_LEVELS[@]}"; do
    printf '%s{"level":"%s","code":"%s","message":"%s"}' "$sep" "${FLAKY_LEVELS[$index]}" "$(json_escape "${FLAKY_CODES[$index]}")" "$(json_escape "${FLAKY_MESSAGES[$index]}")"
    sep=','
  done
  printf ']}\n'
}


flaky_render_text() {
  local index unit
  for index in "${!FLAKY_LEVELS[@]}"; do
    case ${FLAKY_LEVELS[$index]} in warning) unit='警告' ;; failure) unit='失敗' ;; *) unit=${FLAKY_LEVELS[$index]} ;; esac
    printf '[%s] %s\n' "$unit" "${FLAKY_MESSAGES[$index]}"
  done
  printf '集計: 警告=%d 失敗=%d\n' "$FLAKY_WARNINGS" "$FLAKY_FAILURES"
}


# --- read-only CLI (bin/agentic-loop flaky) ---------------------------------
cmd_flaky() {
  if [[ ${1:-} == report ]]; then shift; cmd_flaky_report "$@"; return; fi
  local format=text
  [[ $# -eq 0 ]] || { [[ $# -eq 2 && $1 == --format && $2 == json ]] || { usage; return 2; }; format=$2; }
  local rc=0
  flaky_registry_validate "$(flaky_registry_file "$REPO_ROOT")" "$REPO_ROOT/.agentic-loop/guard-secrets.sh" || rc=1
  if [[ $format == json ]]; then flaky_render_json; else flaky_render_text; fi
  return $rc
}


# --- repair-tracking Issue creation (bin/agentic-loop flaky report) --------
# Deliberately separate from every test-execution path (make check never
# calls this): only an explicit `flaky report` invocation touches GitHub.
flaky_report_one() {
  local unit=$1 fingerprint=$2 verdict=$3 marker query search_json existing_open title body new_issue
  marker="agentic-loop:flaky unit=$unit fingerprint=$fingerprint"
  # One search-index lookup, not an enumeration of every open/closed Issue:
  # its cost does not grow with the repository's cumulative Issue count
  # (Issue #198). `in:body` only searches the Issue body, matching exactly
  # where the marker is embedded below, never a comment.
  query="repo:$(repo_name) \"$marker\" in:body state:open"
  if ! search_json=$(search_issues_api --method GET -f q="$query" -f per_page=5 --jq '[.items[] | {number, state}]' 2>/dev/null); then
    say "flaky report: 修復Issueの既存検索に失敗したため、安全のため作成を中止しました（unit=$unit）。" >&2
    return 1
  fi
  existing_open=$(yq -p json -r '[.[] | select(.state == "open")][0].number // ""' <<< "$search_json" 2>/dev/null)
  if [[ $existing_open =~ ^[1-9][0-9]*$ ]]; then
    comment_issue "$existing_open" "<!-- agentic-loop:flaky-recurred unit=$unit fingerprint=$fingerprint -->\\nflaky testの再発を検出しました（unit: \`$unit\`、fingerprint: \`$fingerprint\`、verdict: \`$verdict\`）。" || true
    say "既存のflaky修復Issue #$existing_open を再利用しました（unit=$unit）。"
    return 0
  fi
  title="flaky testを修復する: $unit ($fingerprint)"
  body="## 目的\\n\\nE2E検証単位 \`$unit\` がverdict=\`$verdict\`（fingerprint: \`$fingerprint\`）としてflaky判定されました。原因を特定し、隔離に依存せず決定的に成功するよう修復します。\\n\\n## 完了条件\\n\\n同一fingerprintの隔離entryが無くても該当unitが安定して成功する。\\n\\n"
  body+="<!-- $marker -->\\n<!-- agentic-loop:scope paths=tests/test-agentic-loop.sh,tests/run-e2e.sh env=unknown -->"
  new_issue=$(repo_api issues --method POST -f title="$title" -f body="$(unfold_body "$body")" --jq .number 2>/dev/null) || { say "flaky report: 修復Issueを作成できませんでした（unit=$unit）。" >&2; return 1; }
  [[ $new_issue =~ ^[1-9][0-9]*$ ]] || { say "flaky report: 修復Issueの番号を確認できませんでした（unit=$unit）。" >&2; return 1; }
  repo_api "issues/$new_issue/labels" --method PUT --input - <<< '{"labels":["flaky","category:improvement","agent:queued"]}' >/dev/null 2>&1 || true
  project_add_issue "$new_issue" || true
  project_sync_state "$new_issue" queued || true
  say "flaky修復Issue #$new_issue を作成しました（unit=$unit）。"
}


cmd_flaky_report() {
  local record_path="$REPO_ROOT/tmp/flaky-last.json"
  while (( $# > 0 )); do
    case $1 in
      --record) [[ $# -ge 2 ]] || { usage; return 2; }; record_path=$2; shift 2 ;;
      *) usage; return 2 ;;
    esac
  done
  [[ -r $record_path ]] || fail "flaky record not found: $record_path"
  command -v yq >/dev/null 2>&1 || fail 'yq is required'
  local record_json reported=0 unit fingerprint verdict
  record_json=$(yq -p json -o json '.' "$record_path" 2>/dev/null) || fail "cannot parse $record_path"
  while IFS="$FLAKY_FS" read -r unit fingerprint verdict; do
    [[ -n $unit ]] || continue
    case $verdict in flaky | flaky-unknown) ;; *) continue ;; esac
    flaky_report_one "$unit" "$fingerprint" "$verdict" && reported=$((reported + 1))
  done < <(yq -p json -r "$(printf '.verdicts[]? | [.unit, .fingerprint, .verdict] | join("%s")' "$FLAKY_FS")" <<< "$record_json")
  say "flaky report: ${reported}件の修復Issueを作成または再利用しました。"
}
