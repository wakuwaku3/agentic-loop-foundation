# Module: metrics.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



# --- Loop metrics (see docs/decisions/0007, docs/operations/loop-metrics.md) ---
# Read-only aggregation over existing Issue/PR/comment history; never mutates
# GitHub or the working tree, and never writes local state. Three REST(core)
# reads only (Issues, repo-wide Issue comments, Pull Requests): no Actions/CI,
# no Projects, no GraphQL, so this adds no cost beyond REST(core) quota already
# budgeted for supervisor operation (see docs/policies/cost.md). Only the
# numeric `agentic-loop:priority` marker value of each Issue body and the
# `<!-- agentic-loop:...-->` marker substring of each comment are ever read;
# the surrounding Japanese bodies/comments, worker logs and prompts are never
# requested separately or stored, and `worker=` is never parsed out, so no
# per-worker breakdown is possible from this data (see docs/operations/
# loop-metrics.md "収集境界").

metrics_since_iso() { date -u -d "@$1" '+%Y-%m-%dT%H:%M:%SZ'; }

metrics_in_window() { (( $1 >= WINDOW_START && $1 <= WINDOW_END )); }


metrics_fetch_issues() {
  local since=$1
  repo_api issues --method GET -f state=all -f since="$since" -f per_page=100 --paginate --jq '
    .[] | select(.pull_request == null) |
    [.number, (.created_at | fromdateiso8601),
     (if .closed_at == null then 0 else (.closed_at | fromdateiso8601) end), .state,
     (([.labels[].name | select(startswith("category:"))] | join(",")) as $v | if $v == "" then "none" else $v end),
     '"$(queue_priority_jq)"',
     (([.labels[].name | select(startswith("agent:"))] | join(",")) as $v | if $v == "" then "none" else $v end)
    ] | @tsv' 2>/dev/null
}


metrics_fetch_events() {
  local since=$1
  repo_api issues/comments --method GET -f since="$since" -f sort=created -f direction=asc -f per_page=100 --paginate --jq '
    .[] | select(.body | test("<!-- agentic-loop:")) |
    [(.issue_url | split("/") | last), (.created_at | fromdateiso8601),
     (.body | capture("<!-- agentic-loop:(?<m>[^>]*)-->").m | gsub("[\t\n\r]"; " "))
    ] | @tsv' 2>/dev/null
}


metrics_fetch_pulls() {
  local since=$1 page=1 rows oldest count
  while :; do
    rows=$(repo_api pulls --method GET -f state=all -f sort=updated -f direction=desc -f per_page=100 -f page="$page" --jq '
      .[] | [.number, (.created_at | fromdateiso8601),
       (if .merged_at == null then 0 else (.merged_at | fromdateiso8601) end), .head.ref,
       .updated_at
      ] | @tsv' 2>/dev/null) || return 1
    [[ -n $rows ]] || break
    awk -F '\t' '{print $1 "\t" $2 "\t" $3 "\t" $4}' <<< "$rows"
    count=$(wc -l <<< "$rows")
    oldest=$(tail -n 1 <<< "$rows" | cut -f5)
    [[ $count -lt 100 || $oldest < $since ]] && break
    page=$((page + 1))
  done
}


# The value of one "key=value" token inside an already-extracted marker
# string, or empty. Every field this reads is enum or numeric by construction
# (see agent_post_usage, comment_issue call sites); never a free-form value.
metrics_field() {
  local marker=$1 key=$2 tok
  tok=$(grep -oE "(^|[[:space:]])$key=[^[:space:]]*" <<< "$marker" | tail -n 1) || true
  printf '%s' "${tok#*=}"
}


# n / mean / p50 / p90 / max (nearest-rank, floor) over one number per stdin
# line, as a single TSV row; "0\t\t\t\t" when empty. Computed over sorted
# input, so the result never depends on event-processing order, keeping
# `metrics --as-of` output reproducible.
metrics_distribution() {
  sort -n | awk '
    { a[NR] = $1; sum += $1 }
    END {
      if (NR == 0) { print "0\t\t\t\t"; exit }
      p50 = a[int((NR - 1) * 0.50) + 1]
      p90 = a[int((NR - 1) * 0.90) + 1]
      printf "%d\t%.0f\t%d\t%d\t%d\n", NR, sum / NR, p50, p90, a[NR]
    }'
}


# Close this Issue's currently-open attempt (if any): record its duration when
# the closing event falls inside the window, add its window-clipped span to the
# worker-utilization sum, and tally a failure reason bucket for reason-carrying
# closes. An attempt with no local ATTOPEN entry (window starts mid-attempt) is
# simply not sampled, per docs/operations/loop-metrics.md's stated limitation.
metrics_close_attempt() {
  local issue=$1 created=$2 reason=$3 dur clipped_start
  if [[ -n ${ATTOPEN[$issue]:-} ]]; then
    if metrics_in_window "$created"; then
      dur=$((created - ATTOPEN[$issue]))
      DUR_ATTEMPT+=("$dur")
      clipped_start=${ATTOPEN[$issue]}
      (( clipped_start < WINDOW_START )) && clipped_start=$WINDOW_START
      UTIL_SUM=$((UTIL_SUM + created - clipped_start))
      case $reason in
        failed:*) FAILREASON[${reason#failed:}]=$(( ${FAILREASON[${reason#failed:}]:-0} + 1 )) ;;
        worker-timeout | exhausted | recover-exhausted) FAILREASON[$reason]=$(( ${FAILREASON[$reason]:-0} + 1 )) ;;
      esac
    fi
    unset -v "ATTOPEN[$issue]"
  fi
}


# Advance the small per-Issue lifecycle state machine by one chronological
# marker event. Unrecognized or purely informational marker types (scope
# refinements, category reconciliation, lease heartbeats already folded into
# the one "lease" row) are intentionally no-ops.
metrics_process_event() {
  local issue=$1 created=$2 type=$3 marker=$4 stage seconds phase reason qstart
  case $type in
    lease)
      qstart=${QSTART[$issue]:-${ISSUE_CREATED[$issue]:-$created}}
      metrics_in_window "$created" && DUR_QUEUE+=("$((created - qstart))")
      unset -v "QSTART[$issue]"
      ATTOPEN[$issue]=$created
      CNT_ATTEMPTS=$((CNT_ATTEMPTS + 1))
      ;;
    usage)
      stage=$(metrics_field "$marker" stage); seconds=$(metrics_field "$marker" seconds)
      [[ $seconds =~ ^[0-9]+$ ]] || return 0
      case $stage in
        plan) DUR_PLANSEC+=("$seconds") ;;
        exec) DUR_EXECSEC+=("$seconds") ;;
      esac
      ;;
    handoff)
      phase=$(metrics_field "$marker" phase)
      if [[ -n $phase && $phase != fresh ]]; then
        CNT_RESUME=$((CNT_RESUME + 1))
      fi
      ;;
    retry)
      metrics_close_attempt "$issue" "$created" retry
      CNT_RETRY=$((CNT_RETRY + 1)); CNT_REQUEUE=$((CNT_REQUEUE + 1))
      QSTART[$issue]=$created
      ;;
    recovered)
      metrics_close_attempt "$issue" "$created" recovered
      CNT_RECOVERED=$((CNT_RECOVERED + 1)); CNT_REQUEUE=$((CNT_REQUEUE + 1))
      QSTART[$issue]=$created
      ;;
    shutdown)
      metrics_close_attempt "$issue" "$created" shutdown
      CNT_REQUEUE=$((CNT_REQUEUE + 1))
      QSTART[$issue]=$created
      ;;
    exhausted)
      metrics_close_attempt "$issue" "$created" exhausted
      CNT_EXHAUSTED=$((CNT_EXHAUSTED + 1)); CNT_REQUEUE=$((CNT_REQUEUE + 1))
      QSTART[$issue]=$created
      ;;
    worker-timeout)
      metrics_close_attempt "$issue" "$created" worker-timeout
      CNT_WORKERTIMEOUT=$((CNT_WORKERTIMEOUT + 1))
      ;;
    recover-exhausted)
      metrics_close_attempt "$issue" "$created" recover-exhausted
      ;;
    scope-conflict)
      CONFLSTART[$issue]=$created
      CNT_SCOPECONFLICT=$((CNT_SCOPECONFLICT + 1))
      ;;
    scope-resolved)
      if [[ -n ${CONFLSTART[$issue]:-} ]]; then
        metrics_in_window "$created" && DUR_CONFLICT+=("$((created - CONFLSTART[$issue]))")
        unset -v "CONFLSTART[$issue]"
      fi
      ;;
    dependency-blocked)
      DEPSTART[$issue]=$created
      CNT_DEPBLOCK=$((CNT_DEPBLOCK + 1))
      ;;
    dependency-ready)
      if [[ -n ${DEPSTART[$issue]:-} ]]; then
        metrics_in_window "$created" && DUR_DEPENDENCY+=("$((created - DEPSTART[$issue]))")
        unset -v "DEPSTART[$issue]"
      fi
      ;;
    needs-input)
      metrics_close_attempt "$issue" "$created" needs-input
      NISTART[$issue]=$created
      CNT_NEEDSINPUT=$((CNT_NEEDSINPUT + 1))
      ;;
    answer-detected)
      if [[ -n ${NISTART[$issue]:-} ]]; then
        metrics_in_window "$created" && DUR_NEEDSINPUT+=("$((created - NISTART[$issue]))")
        unset -v "NISTART[$issue]"
      fi
      QSTART[$issue]=$created
      CNT_REQUEUE=$((CNT_REQUEUE + 1))
      ;;
    completed)
      metrics_close_attempt "$issue" "$created" completed
      LAST_COMPLETED_AT[$issue]=$created
      ;;
    failed)
      reason=$(metrics_field "$marker" reason); [[ -n $reason ]] || reason=unspecified
      metrics_close_attempt "$issue" "$created" "failed:$reason"
      ;;
    declined)
      metrics_close_attempt "$issue" "$created" declined
      HAS_DECLINED[$issue]=1
      ;;
    replan)
      CNT_REPLAN=$((CNT_REPLAN + 1))
      ;;
    parked)
      # The attempt interval was already closed by the failed/recover-exhausted
      # marker that preceded this park; this is purely a disposition counter.
      CNT_PARKED=$((CNT_PARKED + 1))
      ;;
    *) : ;;
  esac
  return 0
}


metrics_dist_text() {
  local label=$1 tsv=$2 n mean p50 p90 max
  IFS=$'\t' read -r n mean p50 p90 max <<< "$tsv"
  if (( n > 0 )); then
    printf '  %s: n=%s mean=%ss p50=%ss p90=%ss max=%ss\n' "$label" "$n" "$mean" "$p50" "$p90" "$max"
  else
    printf '  %s: n=0（欠損）\n' "$label"
  fi
}


metrics_dist_json() {
  local tsv=$1 n mean p50 p90 max
  IFS=$'\t' read -r n mean p50 p90 max <<< "$tsv"
  if (( n > 0 )); then
    printf '{"n":%s,"mean":%s,"p50":%s,"p90":%s,"max":%s}' "$n" "$mean" "$p50" "$p90" "$max"
  else
    printf '{"n":0,"mean":null,"p50":null,"p90":null,"max":null}'
  fi
}


metrics_render_unavailable() {
  local format=$1
  if [[ $format == json ]]; then
    printf '{"schema_version":1,"generated_at":%s,"window":{"start":%s,"end":%s,"days":%s},"github_available":false}\n' \
      "$(date +%s)" "$WINDOW_START" "$WINDOW_END" "$DAYS"
  else
    say "対象期間: $(date -u -d "@$WINDOW_START" '+%Y-%m-%d') 〜 $(date -u -d "@$WINDOW_END" '+%Y-%m-%d')（${DAYS}日間）"
    say 'GitHub REST APIの残量がreserve閾値を下回っているため、metrics集計を実行しませんでした（heartbeatや復旧など必須操作の枠を優先します）。しばらく待って再実行してください。'
  fi
}


metrics_render_text() {
  local key
  say "対象期間: $(date -u -d "@$WINDOW_START" '+%Y-%m-%d') 〜 $(date -u -d "@$WINDOW_END" '+%Y-%m-%d')（${DAYS}日間、as-of epoch=$WINDOW_END)"
  printf '転帰: completed=%s unresolved=%s stale=%s declined=%s cancelled=%s superseded=%s duplicate=%s merged=%s parked=%s paused=%s open=%s other=%s\n' \
    "${DISP_COUNT[completed]}" "${DISP_COUNT[unresolved]}" "${DISP_COUNT[stale]}" "${DISP_COUNT[declined]}" "${DISP_COUNT[cancelled]}" "${DISP_COUNT[superseded]}" "${DISP_COUNT[duplicate]}" "${DISP_COUNT[merged]}" "${DISP_COUNT[parked]}" "${DISP_COUNT[paused]}" "${DISP_COUNT[open]}" "${DISP_COUNT[other]}"
  printf '待ち時間・所要時間:\n'
  metrics_dist_text queue_wait "$DIST_QUEUE"
  metrics_dist_text open_queue_wait "$DIST_OPENQUEUE"
  metrics_dist_text attempt_duration "$DIST_ATTEMPT"
  metrics_dist_text needs_input_wait "$DIST_NEEDSINPUT"
  metrics_dist_text open_needs_input_wait "$DIST_OPENNEEDSINPUT"
  metrics_dist_text conflict_wait "$DIST_CONFLICT"
  metrics_dist_text open_conflict_wait "$DIST_OPENCONFLICT"
  metrics_dist_text dependency_wait "$DIST_DEPENDENCY"
  metrics_dist_text open_dependency_wait "$DIST_OPENDEPENDENCY"
  metrics_dist_text lead_time "$DIST_LEADTIME"
  metrics_dist_text plan_seconds "$DIST_PLANSEC"
  metrics_dist_text exec_seconds "$DIST_EXECSEC"
  metrics_dist_text pr_review_wait "$DIST_PRWAIT"
  printf '件数: attempts=%s retry=%s recovered=%s worker_timeout=%s exhausted=%s scope_conflict=%s dependency_block=%s needs_input_round=%s resume=%s requeue=%s replan=%s parked=%s open_attempts=%s unmerged_pr=%s\n' \
    "$CNT_ATTEMPTS" "$CNT_RETRY" "$CNT_RECOVERED" "$CNT_WORKERTIMEOUT" "$CNT_EXHAUSTED" "$CNT_SCOPECONFLICT" "$CNT_DEPBLOCK" "$CNT_NEEDSINPUT" "$CNT_RESUME" "$CNT_REQUEUE" "$CNT_REPLAN" "$CNT_PARKED" "${#ATTOPEN[@]}" "$UNMERGED_PR_COUNT"
  if (( ${#FAILREASON[@]} > 0 )); then
    printf '失敗理由:\n'
    for key in $(printf '%s\n' "${!FAILREASON[@]}" | sort); do
      printf '  %s: %s件\n' "$key" "${FAILREASON[$key]}"
    done
  else
    say '失敗理由: none'
  fi
  printf 'category別件数（作成が対象期間内のIssue）:\n'
  for key in "${CATEGORY_LABELS[@]}"; do printf '  %s: %s件\n' "$key" "${CATCOUNT[$key]}"; done
  printf 'priority別件数（作成が対象期間内のIssue）:\n'
  if (( ${#PRICOUNT[@]} > 0 )); then
    local key
    for key in $(printf '%s\n' "${!PRICOUNT[@]}" | sort -n); do printf '  %s: %s件\n' "$key" "${PRICOUNT[$key]}"; done
  else
    say '  なし（窓内作成Issueにpriority markerがありません）'
  fi
  printf 'worker稼働率: %s（max_workers=%s, 対象期間=%s秒, 稼働=%s秒。個人やworker単位の速度ではなく設備全体の占有率です）\n' "$UTIL_RATIO" "$MAX_WORKERS" "$((WINDOW_END - WINDOW_START))" "$UTIL_SUM"
  if (( ${#WARNINGS[@]} > 0 )); then
    printf '警告:\n'
    local w; for w in "${WARNINGS[@]}"; do printf '  %s\n' "$w"; done
  else
    say '警告: none'
  fi
}


metrics_render_json() {
  local sep='' key
  printf '{"schema_version":1,"generated_at":%s,"window":{"start":%s,"end":%s,"days":%s},"github_available":true,' \
    "$(date +%s)" "$WINDOW_START" "$WINDOW_END" "$DAYS"
  printf '"dispositions":{"completed":%s,"unresolved":%s,"stale":%s,"declined":%s,"cancelled":%s,"superseded":%s,"duplicate":%s,"merged":%s,"parked":%s,"paused":%s,"open":%s,"other":%s},' \
    "${DISP_COUNT[completed]}" "${DISP_COUNT[unresolved]}" "${DISP_COUNT[stale]}" "${DISP_COUNT[declined]}" "${DISP_COUNT[cancelled]}" "${DISP_COUNT[superseded]}" "${DISP_COUNT[duplicate]}" "${DISP_COUNT[merged]}" "${DISP_COUNT[parked]}" "${DISP_COUNT[paused]}" "${DISP_COUNT[open]}" "${DISP_COUNT[other]}"
  printf '"durations":{"queue_wait":%s,"open_queue_wait":%s,"attempt_duration":%s,"needs_input_wait":%s,"open_needs_input_wait":%s,"conflict_wait":%s,"open_conflict_wait":%s,"dependency_wait":%s,"open_dependency_wait":%s,"lead_time":%s,"plan_seconds":%s,"exec_seconds":%s,"pr_review_wait":%s},' \
    "$(metrics_dist_json "$DIST_QUEUE")" "$(metrics_dist_json "$DIST_OPENQUEUE")" "$(metrics_dist_json "$DIST_ATTEMPT")" \
    "$(metrics_dist_json "$DIST_NEEDSINPUT")" "$(metrics_dist_json "$DIST_OPENNEEDSINPUT")" \
    "$(metrics_dist_json "$DIST_CONFLICT")" "$(metrics_dist_json "$DIST_OPENCONFLICT")" \
    "$(metrics_dist_json "$DIST_DEPENDENCY")" "$(metrics_dist_json "$DIST_OPENDEPENDENCY")" \
    "$(metrics_dist_json "$DIST_LEADTIME")" "$(metrics_dist_json "$DIST_PLANSEC")" "$(metrics_dist_json "$DIST_EXECSEC")" "$(metrics_dist_json "$DIST_PRWAIT")"
  printf '"counters":{"attempts":%s,"retry":%s,"recovered":%s,"worker_timeout":%s,"exhausted":%s,"scope_conflict":%s,"dependency_block":%s,"needs_input_round":%s,"resume":%s,"requeue":%s,"replan":%s,"parked":%s,"open_attempts":%s,"unmerged_pr":%s},' \
    "$CNT_ATTEMPTS" "$CNT_RETRY" "$CNT_RECOVERED" "$CNT_WORKERTIMEOUT" "$CNT_EXHAUSTED" "$CNT_SCOPECONFLICT" "$CNT_DEPBLOCK" "$CNT_NEEDSINPUT" "$CNT_RESUME" "$CNT_REQUEUE" "$CNT_REPLAN" "$CNT_PARKED" "${#ATTOPEN[@]}" "$UNMERGED_PR_COUNT"
  printf '"failures":{'
  sep=''
  for key in $(printf '%s\n' "${!FAILREASON[@]}" | sort); do
    printf '%s"%s":%s' "$sep" "$(json_escape "$key")" "${FAILREASON[$key]}"
    sep=','
  done
  printf '},'
  printf '"utilization":{"max_workers":%s,"window_seconds":%s,"busy_seconds":%s,"ratio":%s},' \
    "$MAX_WORKERS" "$((WINDOW_END - WINDOW_START))" "$UTIL_SUM" "$UTIL_RATIO"
  printf '"by_category":{'
  sep=''
  for key in "${CATEGORY_LABELS[@]}"; do printf '%s"%s":%s' "$sep" "$key" "${CATCOUNT[$key]}"; sep=','; done
  printf '},"by_priority":{'
  sep=''
  local key
  for key in $(printf '%s\n' "${!PRICOUNT[@]}" | sort -n); do printf '%s"%s":%s' "$sep" "$key" "${PRICOUNT[$key]}"; sep=','; done
  printf '},"warnings":['
  sep=''
  local w
  for w in "${WARNINGS[@]}"; do printf '%s"%s"' "$sep" "$(json_escape "$w")"; sep=','; done
  printf ']}\n'
}


cmd_metrics() {
  shift
  local format=text days=30 as_of=''
  while (( $# > 0 )); do
    case $1 in
      --format) (( $# >= 2 )) || { usage; return 2; }; format=$2; shift 2 ;;
      --days) (( $# >= 2 )) || { usage; return 2; }; days=$2; shift 2 ;;
      --as-of) (( $# >= 2 )) || { usage; return 2; }; as_of=$2; shift 2 ;;
      *) usage; return 2 ;;
    esac
  done
  [[ $format == text || $format == json ]] || { usage; return 2; }
  [[ $days =~ ^[1-9][0-9]*$ ]] || { usage; return 2; }
  [[ -z $as_of || $as_of =~ ^[1-9][0-9]*$ ]] || { usage; return 2; }

  declare -g WINDOW_END=${as_of:-$(date +%s)}
  declare -g DAYS=$days
  declare -g WINDOW_START=$((WINDOW_END - days * 86400))

  core_budget_allows "$((CORE_RESERVE + 3))" || { metrics_render_unavailable "$format"; return 0; }

  declare -gA ISSUE_CREATED=() ISSUE_CLOSED=() ISSUE_STATE=() ISSUE_CATEGORY=() ISSUE_PRIORITY=() ISSUE_AGENT=()
  declare -gA QSTART=() ATTOPEN=() NISTART=() CONFLSTART=() DEPSTART=() LAST_COMPLETED_AT=() HAS_DECLINED=() FAILREASON=()
  declare -gA DISP_COUNT=([completed]=0 [unresolved]=0 [stale]=0 [declined]=0 [cancelled]=0 [superseded]=0 [duplicate]=0 [merged]=0 [parked]=0 [paused]=0 [open]=0 [other]=0)
  declare -gA CATCOUNT=() PRICOUNT=()
  local label
  for label in "${CATEGORY_LABELS[@]}"; do CATCOUNT[$label]=0; done
  declare -ga DUR_QUEUE=() DUR_ATTEMPT=() DUR_NEEDSINPUT=() DUR_CONFLICT=() DUR_DEPENDENCY=() DUR_LEADTIME=() DUR_PLANSEC=() DUR_EXECSEC=() DUR_PRWAIT=()
  declare -ga OPEN_QUEUE=() OPEN_NEEDSINPUT=() OPEN_CONFLICT=() OPEN_DEPENDENCY=()
  declare -ga WARNINGS=()
  declare -g CNT_ATTEMPTS=0 CNT_RETRY=0 CNT_RECOVERED=0 CNT_WORKERTIMEOUT=0 CNT_EXHAUSTED=0 CNT_SCOPECONFLICT=0 CNT_DEPBLOCK=0 CNT_NEEDSINPUT=0 CNT_RESUME=0 CNT_REQUEUE=0 CNT_REPLAN=0 CNT_PARKED=0
  declare -g UTIL_SUM=0 UNMERGED_PR_COUNT=0

  local since_iso issues_raw events_raw pulls_raw
  since_iso=$(metrics_since_iso "$WINDOW_START")

  if ! issues_raw=$(metrics_fetch_issues "$since_iso"); then
    metrics_render_unavailable "$format"
    return 0
  fi
  local num created closed state category priority agent
  while IFS=$'\t' read -r num created closed state category priority agent; do
    [[ $num =~ ^[0-9]+$ ]] || continue
    ISSUE_CREATED[$num]=$created; ISSUE_CLOSED[$num]=$closed; ISSUE_STATE[$num]=$state
    ISSUE_CATEGORY[$num]=$category; ISSUE_PRIORITY[$num]=$priority; ISSUE_AGENT[$num]=$agent
  done <<< "$issues_raw"

  if events_raw=$(metrics_fetch_events "$since_iso"); then
    local eissue ecreated emarker etype
    while IFS=$'\t' read -r eissue ecreated emarker; do
      [[ $eissue =~ ^[0-9]+$ && $ecreated =~ ^[0-9]+$ ]] || continue
      [[ -n ${ISSUE_CREATED[$eissue]:-} ]] || continue
      (( ecreated <= WINDOW_END )) || continue
      etype=${emarker%% *}
      metrics_process_event "$eissue" "$ecreated" "$etype" "$emarker"
    done <<< "$events_raw"
  else
    WARNINGS+=('comment（イベントmarker）の取得に失敗したため、待ち時間・試行・失敗理由系の指標は欠損しています。')
  fi

  if pulls_raw=$(metrics_fetch_pulls "$since_iso"); then
    local _ pcreated pmerged pref pissue
    while IFS=$'\t' read -r _ pcreated pmerged pref; do
      [[ $pref =~ ^agent/issue-([0-9]+)$ ]] || continue
      pissue=${BASH_REMATCH[1]}
      [[ -n ${ISSUE_CREATED[$pissue]:-} ]] || continue
      (( pcreated <= WINDOW_END )) || continue
      if (( pmerged > 0 )); then
        metrics_in_window "$pmerged" && DUR_PRWAIT+=("$((pmerged - pcreated))")
      else
        UNMERGED_PR_COUNT=$((UNMERGED_PR_COUNT + 1))
      fi
    done <<< "$pulls_raw"
  else
    WARNINGS+=('pull requestの取得に失敗したため、pr_review_waitは欠損しています。')
  fi

  local n disposition mismatches=0 c p
  for n in "${!ISSUE_CREATED[@]}"; do
    if [[ ${ISSUE_STATE[$n]} == closed ]]; then
      # 転帰は close時刻で窓に帰属させる（作成が古くても、この窓でcloseしたIssueだけを数える）。
      if (( ${ISSUE_CLOSED[$n]} > 0 )) && metrics_in_window "${ISSUE_CLOSED[$n]}"; then
        if [[ -n ${HAS_DECLINED[$n]:-} ]]; then disposition=declined
        elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:cancelled,* ]]; then disposition=cancelled
        elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:superseded,* ]]; then disposition=superseded
        elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:duplicate,* ]]; then disposition=duplicate
        elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:merged,* ]]; then disposition=merged
        elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:stale,* ]]; then disposition=stale
        elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:completed,* ]]; then disposition=completed
        elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:failed,* ]]; then disposition=unresolved
        else disposition=other; mismatches=$((mismatches + 1)); fi
        DISP_COUNT[$disposition]=$(( DISP_COUNT[$disposition] + 1 ))
        [[ $disposition == completed && -n ${LAST_COMPLETED_AT[$n]:-} ]] && \
          DUR_LEADTIME+=("$((LAST_COMPLETED_AT[$n] - ISSUE_CREATED[$n]))")
      fi
    elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:parked,* ]]; then
      DISP_COUNT[parked]=$(( DISP_COUNT[parked] + 1 ))
    elif [[ ,${ISSUE_AGENT[$n]}, == *,agent:paused,* ]]; then
      DISP_COUNT[paused]=$(( DISP_COUNT[paused] + 1 ))
    else
      DISP_COUNT[open]=$(( DISP_COUNT[open] + 1 ))
    fi
    if (( ISSUE_CREATED[$n] >= WINDOW_START && ISSUE_CREATED[$n] <= WINDOW_END )); then
      c=${ISSUE_CATEGORY[$n]%%,*}; c=${c#category:}
      [[ -n $c && -n ${CATCOUNT[$c]+x} ]] && CATCOUNT[$c]=$(( CATCOUNT[$c] + 1 ))
      p=${ISSUE_PRIORITY[$n]}
      [[ $p =~ ^[0-9]+$ ]] && PRICOUNT[$p]=$(( ${PRICOUNT[$p]:-0} + 1 ))
    fi
  done
  (( mismatches > 0 )) && WARNINGS+=("label_marker_mismatch: 窓内でcloseしたIssueのうち${mismatches}件はLabelからdispositionを判定できませんでした（otherとして集計）。")

  for n in "${!QSTART[@]}"; do OPEN_QUEUE+=("$((WINDOW_END - QSTART[$n]))"); done
  for n in "${!NISTART[@]}"; do OPEN_NEEDSINPUT+=("$((WINDOW_END - NISTART[$n]))"); done
  for n in "${!CONFLSTART[@]}"; do OPEN_CONFLICT+=("$((WINDOW_END - CONFLSTART[$n]))"); done
  for n in "${!DEPSTART[@]}"; do OPEN_DEPENDENCY+=("$((WINDOW_END - DEPSTART[$n]))"); done

  declare -g DIST_QUEUE DIST_OPENQUEUE DIST_ATTEMPT DIST_NEEDSINPUT DIST_OPENNEEDSINPUT DIST_CONFLICT DIST_OPENCONFLICT DIST_DEPENDENCY DIST_OPENDEPENDENCY DIST_LEADTIME DIST_PLANSEC DIST_EXECSEC DIST_PRWAIT
  DIST_QUEUE=$(printf '%s\n' "${DUR_QUEUE[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_OPENQUEUE=$(printf '%s\n' "${OPEN_QUEUE[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_ATTEMPT=$(printf '%s\n' "${DUR_ATTEMPT[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_NEEDSINPUT=$(printf '%s\n' "${DUR_NEEDSINPUT[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_OPENNEEDSINPUT=$(printf '%s\n' "${OPEN_NEEDSINPUT[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_CONFLICT=$(printf '%s\n' "${DUR_CONFLICT[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_OPENCONFLICT=$(printf '%s\n' "${OPEN_CONFLICT[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_DEPENDENCY=$(printf '%s\n' "${DUR_DEPENDENCY[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_OPENDEPENDENCY=$(printf '%s\n' "${OPEN_DEPENDENCY[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_LEADTIME=$(printf '%s\n' "${DUR_LEADTIME[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_PLANSEC=$(printf '%s\n' "${DUR_PLANSEC[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_EXECSEC=$(printf '%s\n' "${DUR_EXECSEC[@]}" | sed '/^$/d' | metrics_distribution)
  DIST_PRWAIT=$(printf '%s\n' "${DUR_PRWAIT[@]}" | sed '/^$/d' | metrics_distribution)

  declare -g UTIL_RATIO
  UTIL_RATIO=$(awk -v s="$UTIL_SUM" -v w="$MAX_WORKERS" -v sec="$((WINDOW_END - WINDOW_START))" 'BEGIN { if (w > 0 && sec > 0) printf "%.4f", s / (w * sec); else print "0" }')

  if [[ $format == json ]]; then metrics_render_json; else metrics_render_text; fi
  return 0
}
