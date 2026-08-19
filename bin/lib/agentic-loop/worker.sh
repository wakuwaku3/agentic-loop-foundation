# Module: worker.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034,SC2153



issue_prompt() {
  local issue=$1 repository=$2 cap
  cat <<EOF
$repository の GitHub Issue #$issue を、リポジトリの submit-requirement workflow に従って完了してください。
Issueとそのコメントを要求として扱い、この専用worktreeだけで作業します。まず調査し、安全で可逆的な判断を行い、全検証とsecret guardを実行し、commit、push、PR作成、checksとreview feedbackの確認、不具合修正、mergeまで完遂します。既存のCodex loginとworkspace-write sandboxだけを使用し、別途課金される認証情報、有料の外部service、安全でないsandbox bypass、hook bypassは使用しません。GitHubのIssue、Label、comment、PR、checkなどRESTで可能なcore操作は \`gh api\` のREST endpointを使い、GraphQLはbest-effortのProjects操作だけに限定します。GitHubのIssue、PR、それらへのコメントとレビューは、コード、ログ、識別子、固有名詞、引用の必要な原文を除き、日本語で記述します。Issueに進捗と検証証跡をコメントします。人の判断が必要な重大な決定または回復不能な権限問題がある場合に限り、正確な質問をコメントし、最終応答を AGENTIC_LOOP_RESULT=needs-input で終えます。needs-inputの根拠には、このリポジトリに実在するcommand・設定ファイル・状態（例: 実在するCLIサブコマンド、\`.agentic-loop/capabilities.toml\` に実際に記載された宣言、Git/GitHubから観測できる回復不能な状態）だけを用い、実在しない承認コマンド・設定・権限フローを創作して停止してはいけません。変更scopeの重大さについての自己判断だけを理由に、実在しない承認取得が必要だと創作しないでください（例えば \`devbox.lock\` を含む開発環境toolchainの変更は通常のPRレビューで足り、事前承認の取得は不要です）。mergeと検証が完了した場合は AGENTIC_LOOP_RESULT=completed で終えます。実施が不要（既に満たされている、要求が誤りや重複など）または実施すべきでないと判断した場合は、理由を説明し AGENTIC_LOOP_RESULT=declined で終えます（\`agent:needs-input\` に載り、workerはcloseしません。認可済みの運用者の判断を待ちます）。それ以外は安全に再試行できる失敗理由を説明し、AGENTIC_LOOP_RESULT=failed で終えます（Supervisorが自動再試行し、上限到達でもcloseせず \`agent:parked\`（人間トリアージ待ち）へ移します）。
EOF
  cap=$(capability_summary_block "$REPO_ROOT")
  [[ -z $cap ]] || printf '%s\n' "$cap"
}


# plan stage prompt: investigate only and emit a concrete plan. On a retry the
# previous exec failure is appended so the plan can address the root cause. If
# resume_context is non-empty (a resumed Issue whose branch already carries
# work), it is prepended so the plan does not propose redoing completed work.
plan_prompt() {
  local issue=$1 repository=$2 failure_context=${3:-} resume_context=${4:-} cap
  [[ -n $resume_context ]] && printf '%s\n' "$resume_context"
  cap=$(capability_summary_block "$REPO_ROOT")
  [[ -z $cap ]] || printf '%s\n' "$cap"
  cat <<EOF
$repository の GitHub Issue #$issue に対する実装計画を作成してください。
Issueとそのコメントを要求として扱い、この専用worktreeとリポジトリを調査し、変更方針、影響範囲、検証手順、観測可能な受け入れ条件を日本語で具体化してください。このplan段階ではファイルの変更、commit、push、PR作成、GitHubの状態変更を行わず、最終メッセージに計画本文だけを出力してください。
計画本文の最後の行に、変更が触れる見込みのpathとexternal環境を示すmarkerを1行だけ出力してください: \`<!-- agentic-loop:scope paths=path1,path2 env=name1,name2 -->\`。repository全体に及ぶ場合は \`paths=*\` としてください。fileのrename/moveやdirectory再編など構造的変更を含む計画では \`structural=1\` も付け加えてください（構造的変更はpathが重なる他Issueと直列化されます）。見積もれない場合はこの行を省略してください（安全な既定動作にフォールバックします）。このmarkerはSupervisorが並列worker間の変更scope競合を避けるための機械可読情報であり、実装の一部ではありません。
一つのworkerで安全に完遂できない要求だけは、scopeが独立し個別にmerge・rollback・検証できる直接の子が2〜6件である場合に限り、計画の末尾（scope markerの前）へ JSON の \`agentic-loop:decomposition\` code blockを置けます。schema=1、mode=children、integration_acceptance_criteria、children（key/title/purpose/acceptance_criteria/scope/depends_on）を含めてください。keyは英小文字・数字・\`-\`のみで一意、scopeは既存scope markerと同じ \`paths=... env=...\` 形式、depends_onは同じmanifestのkeyだけです。循環、共有変更、上限超過、統合条件を定義できない場合はmanifestを出さず、通常の単一PRとして計画してください。
計画本文の中（scope markerより前）に、JSON の \`agentic-loop:preflight\` code blockを1つ出力してください。schema=1、issue=$issue、risks（security/confidentiality/integrity/availability/data_migration/external_environment/cost/compatibility/release_deploy/rollbackの10軸すべてを1回ずつ、各軸に level=low|medium|high|unknown。lowでなければreason必須、200文字以内・改行/backtick禁止。unknownなら追加でmissingに不足情報を200文字以内で明記し、低リスク扱いにしないこと）、change（scope/tests/external_operations/rollbackの短い説明、いずれも200文字以内・改行/backtick禁止）、approval（required真偽値、triggersはdestructive/irreversible/cost/security/permission/external-deploy/data-migration/rollback-blockedのうち該当するものの配列）を含めてください。破壊的・不可逆・重大costまたはsecurity上のリスクがあるなら、必ず該当軸をhighにするか対応するtriggerを含め、approval.required=trueとしてください。秘密情報や攻撃に利用できる詳細は書かないこと。リポジトリの方針や\`.agentic-loop/capabilities.toml\`の実在する宣言に基づかない、変更の重大さについての自己判断だけを理由にaxisをhighにしたりapproval.required=trueにしないでください（例えば\`devbox.lock\`など開発環境toolchainの通常の更新は、それ自体を理由にhigh/approval.required=trueとする根拠にはなりません）。
計画本文の中（scope markerより前）に、JSON の \`agentic-loop:workload\` code blockを1つ出力してください。外部I/O・pagination・探索・retry・並列処理を追加/変更しない計画では \`{"schema": 1, "issue": $issue, "external_io": "none"}\` の1行で済ませてよいです。追加/変更する場合は schema=1、issue=$issue、external_io（added または changed）、units（各要素にoperation/per_unit/growth/stop_condition/reuseを200文字以内・改行/backtick禁止で1〜10件）、verification（呼び出し回数上限または規模別testの名前を1〜10件）を含めてください。任意でamplification（idle/failure/multi_hostの非増幅根拠）とexceptions（site/reason/任意のtrack=#N）を含められます。有限資源とスケーラビリティのポリシー（docs/policies/resource-scalability.md）に従い、処理量・増加率・停止条件・再取得回避を自己申告し、安全弁の存在だけを根拠に効率的な設計を省略しないこと。
EOF
  if [[ -n $failure_context ]]; then
    cat <<EOF
直前のexec段は完了条件を満たせませんでした。次の記録を踏まえ、原因に対処するよう計画を見直してください。
$failure_context
EOF
  fi
}


# exec stage prompt: the full completion workflow plus the plan to follow.
exec_prompt() {
  local issue=$1 repository=$2 plan_file=$3 resume_context=${4:-}
  [[ -n $resume_context ]] && printf '%s\n' "$resume_context"
  issue_prompt "$issue" "$repository"
  if [[ -n ${PREFLIGHT_VERDICT:-} && ${PREFLIGHT:-off} != off ]]; then
    printf '変更影響とリスクのpreflight判定: %s（detail=%s）%s。実装中に、宣言した変更scope・外部操作・リスク水準を超える破壊的・不可逆・重大costまたはsecurity上のリスクを新たに発見した場合は、実装や変更を進めず、最終応答を AGENTIC_LOOP_RESULT=needs-input で終えてください。\n' \
      "$PREFLIGHT_VERDICT" "${PREFLIGHT_DETAIL:-}" "${PREFLIGHT_APPROVAL_TOKEN:+ (承認済みenvelope token=$PREFLIGHT_APPROVAL_TOKEN)}"
  fi
  if [[ -n ${WORKLOAD_VERDICT:-} && ${WORKLOAD:-off} != off ]]; then
    printf '有限資源とスケーラビリティのworkload判定: %s（detail=%s）。実装中に、宣言した処理量モデル・停止条件を超える外部呼び出しが必要になった場合は、宣言なしに全件取得・無制限pagination・無制限retryを追加せず、実装や変更を進めず、最終応答を AGENTIC_LOOP_RESULT=needs-input で終えてください。\n' \
      "$WORKLOAD_VERDICT" "${WORKLOAD_DETAIL:-}"
  fi
  cat <<'EOF'
PR本文に、各受け入れ条件と実装変更・検証結果を対応付ける `agentic-loop:traceability` code blockを含めてください（`.github/PULL_REQUEST_TEMPLATE.md` の雛形と `docs/operations/traceability.md` を参照）。schema=1、issue番号、criteria配列（各要素はid=`ac-`+条件文正規化のsha256先頭8桁、source、status、verification、必要に応じてchanges/checks/reason/superseded_by）を持つJSONです。行番号やcommit SHAではなくidとpathで対象を示し、秘密やlog全文は含めないでください。この記録はGitHubの観測結果（checks結論、変更path）と照合されるため、実際に行った変更・実行した検証とだけ一致させてください。
CI・required checks・AI reviewなどの外部完了待ちは、このturn内で前景実行してください。`gh pr checks --watch` 等にはtimeoutを付け、未確定なら状態を再確認して同じturn内で繰り返します。background process、別agent、別sessionへ待機を委譲してはいけません。checksがpending、review feedbackが未対応、またはmergeとdefault branch上の検証が未完了のまま最終応答を書いてはいけません。「待機中です」などの待機報告で終了してはいけません。
正当な終端に到達した最終応答は、最後の非空行を `AGENTIC_LOOP_RESULT=completed`、`AGENTIC_LOOP_RESULT=failed`、`AGENTIC_LOOP_RESULT=needs-input`、`AGENTIC_LOOP_RESULT=declined` のいずれか一行だけにしてください。markerの後に説明、コードフェンス、または別の非空行を置いてはいけません。
EOF
  if [[ -s $plan_file ]]; then
    cat <<EOF
次の実装計画（plan段階で作成）に従って作業を進め、必要に応じて安全に調整してください。
$(cat "$plan_file")
EOF
  fi
}


# The provider adapters preserve their native output format (some wrap the final
# message in JSON), so validate a bounded terminal marker rather than treating a
# successful process exit or free-form status text as a completion result.
agent_result_terminal_marker() {
  local result_path=$1 last
  [[ -r $result_path ]] || return 1
  # A marker quoted in prose, followed by a status line, or with an unknown
  # value must never complete a worker. Providers are normalized above so this
  # same rule applies to Codex text, Claude .result, and opencode text events.
  last=$(awk 'NF { line=$0 } END { print line }' "$result_path")
  case $last in
    AGENTIC_LOOP_RESULT=completed|AGENTIC_LOOP_RESULT=failed|AGENTIC_LOOP_RESULT=needs-input|AGENTIC_LOOP_RESULT=declined)
      printf '%s\n' "${last#AGENTIC_LOOP_RESULT=}"
      ;;
    *) return 1 ;;
  esac
}


agent_result_is() {
  local result_path=$1 wanted=$2 actual
  actual=$(agent_result_terminal_marker "$result_path") || return 1
  [[ $actual == "$wanted" ]]
}


resolve_worker_git_common_dir() {
  local worktree=$1 common_dir git_dir worktree_root home_dir
  worktree_root=$(cd "$worktree" && pwd -P) || return 1
  common_dir=$(git -C "$worktree_root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || return 1
  git_dir=$(git -C "$worktree_root" rev-parse --path-format=absolute --git-dir 2>/dev/null) || return 1
  [[ $common_dir == /* && $git_dir == /* && -d $common_dir && -d $git_dir ]] || return 1
  common_dir=$(cd "$common_dir" && pwd -P) || return 1
  git_dir=$(cd "$git_dir" && pwd -P) || return 1
  [[ $(git -C "$worktree_root" rev-parse --is-inside-work-tree 2>/dev/null) == true ]] || return 1
  [[ $git_dir == "$common_dir" || $git_dir == "$common_dir"/* ]] || return 1
  [[ -f $common_dir/HEAD && -d $common_dir/objects && -d $common_dir/refs ]] || return 1
  home_dir=${HOME:-}
  [[ $common_dir != / && $common_dir != "$worktree_root" && ( -z $home_dir || $common_dir != "$home_dir" ) ]] || return 1
  printf '%s\n' "$common_dir"
}


resolve_worker_agents_dir() {
  local worktree=$1 worktree_root agents_dir
  worktree_root=$(cd "$worktree" && pwd -P) || return 1
  agents_dir="$worktree_root/.agents"
  [[ -d $agents_dir && ! -L $agents_dir ]] || return 1
  agents_dir=$(cd "$agents_dir" && pwd -P) || return 1
  [[ $agents_dir == "$worktree_root/.agents" ]] || return 1
  printf '%s\n' "$agents_dir"
}


cleanup_completed_worker() {
  local worktree=$1 branch=$2 merged_oid=$3 worktree_root registered_branch branch_ref local_oid
  worktree_root=$(cd "$worktree" && pwd -P) || return 1
  [[ $worktree_root == "$WORKTREE_ROOT"/issue-[0-9]* && ! -L $worktree ]] || return 1
  branch_ref="refs/heads/$branch"
  registered_branch=$(git -C "$REPO_ROOT" worktree list --porcelain | awk -v path="$worktree_root" '
    $1 == "worktree" {matched=($2 == path)}
    matched && $1 == "branch" {print $2; exit}
  ')
  [[ $registered_branch == "$branch_ref" ]] || return 1
  [[ $(git -C "$worktree_root" symbolic-ref -q HEAD) == "$branch_ref" ]] || return 1
  [[ -z $(git -C "$worktree_root" status --porcelain) ]] || return 1
  local_oid=$(git -C "$REPO_ROOT" rev-parse --verify "$branch_ref") || return 1
  [[ $local_oid == "$merged_oid" ]] || return 1
  git -C "$REPO_ROOT" worktree remove "$worktree_root" || return 1
  git -C "$REPO_ROOT" update-ref -d "$branch_ref" "$local_oid" || return 1
  [[ ! -e $worktree_root ]] || return 1
  ! git -C "$REPO_ROOT" show-ref --verify --quiet "$branch_ref"
}


# Merge freshly observed scope tokens into a running Issue's cache, commenting
# only when the effective (merged) scope actually changes so re-planning
# passes and repeated diffs do not spam the Issue. The merge only ever grows
# the cached scope, so an early under-estimate cannot let a later, larger
# conflict go undetected.
worker_update_scope() {
  local issue=$1 new_tokens=$2 current merged
  [[ -n $new_tokens ]] || return 0
  current=$(scope_cache_read "$issue")
  merged=$(sort -u <<< "$(printf '%s\n%s\n' "$current" "$new_tokens" | sed '/^$/d')")
  [[ $merged == "$current" ]] && return 0
  scope_cache_write "$issue" "$merged"
  comment_issue "$issue" "<!-- agentic-loop:scope tokens=$(paste -sd, - <<< "$merged") -->\nこのIssueが影響する変更scopeを更新しました: $(paste -sd', ' - <<< "$merged")" || true
}


worker_refine_scope_from_plan() {
  local issue=$1 plan_file=$2 raw tokens
  raw=$(scope_marker_from_body "$(cat "$plan_file" 2>/dev/null || true)")
  [[ -n $raw ]] || return 0
  tokens=$(scope_tokens_normalize "$raw")
  [[ -n $tokens ]] || return 0
  worker_update_scope "$issue" "$(scope_apply_exclusive_paths "$tokens")"
}


# Detect structural changes in the measured diff between the default branch and
# the worker's HEAD: explicit renames (git rename detection, -M) and directory
# reorganizations where a parent directory gained and lost several files in one
# pass. Emits the affected paths (renamed files and reorganized parent
# directories) so worker_update_scope can union them with the ordinary measured
# paths and mark the scope structural. Over-detection is safe: it only forces
# serialization, never parallelism.
worker_diff_structural() {
  local worktree=$1 default_branch=$2 base=${3:-}
  base=${base:-origin/$default_branch}
  git -C "$worktree" diff --name-status -M "$base" HEAD 2>/dev/null |
    awk -F '\t' '
      function parent(p,  i) {
        i = index(p, "/")
        return (i ? substr(p, 1, i - 1) : p)
      }
      {
        st = substr($1, 1, 1)
        if (st == "R") { renames[parent($NF)]++; renamed[$NF] = 1 }
        else if (st == "D") dels[parent($2)]++
        else if (st == "A") adds[parent($2)]++
      }
      END {
        for (d in renames) if (renames[d] >= 2) reorg[d] = 1
        for (d in dels) if (dels[d] >= 2 && adds[d] >= 2) reorg[d] = 1
        for (f in renamed) print f
        for (d in reorg) print d
      }'
}


worker_refine_scope_from_diff() {
  local issue=$1 worktree=$2 default_branch=$3 measured structural
  measured=$(git -C "$worktree" diff --name-only "origin/$default_branch" HEAD 2>/dev/null | sed 's#^#path:#' | sort -u)
  [[ -n $measured ]] || return 0
  structural=$(worker_diff_structural "$worktree" "$default_branch")
  if [[ -n $structural ]]; then
    measured=$(printf '%s\nstructural\n%s\n' "$measured" "$(printf '%s\n' "$structural" | sed 's#^#path:#' | sort -u)" | sed '/^$/d' | sort -u)
  fi
  worker_update_scope "$issue" "$(scope_apply_exclusive_paths "$measured")"
}


# --- Parent/child decomposition ------------------------------------------------
# The plan is untrusted provider output.  Keep it as JSON inside a fenced block
# and reject it before making any GitHub mutation.  This deliberately has a
# small, explicit schema: GitHub native sub-issues and dependencies remain the
# source of truth, rather than a second relationship database in comments.
decomposition_manifest_from_plan() {
  local plan=$1
  awk '/^```agentic-loop:decomposition[[:space:]]*$/{on=1;next} on && /^```[[:space:]]*$/{exit} on{print}' "$plan"
}


decomposition_validate() {
  local manifest=$1 parent_depth=${2:-0} keys key dep count scope child_depth
  [[ -n $manifest ]] || return 1
  command -v yq >/dev/null 2>&1 || return 1
  [[ $(yq -p json -r '.schema // ""' <<< "$manifest" 2>/dev/null) == "$DECOMPOSITION_SCHEMA_VERSION" ]] || return 1
  [[ $(yq -p json -r '.mode // ""' <<< "$manifest" 2>/dev/null) == children ]] || return 1
  [[ -n $(yq -p json -r '.integration_acceptance_criteria // ""' <<< "$manifest" 2>/dev/null) ]] || return 1
  count=$(yq -p json -r '.children | length' <<< "$manifest" 2>/dev/null) || return 1
  [[ $count =~ ^[0-9]+$ ]] && (( count >= DECOMPOSITION_MIN_DIRECT_CHILDREN && count <= DECOMPOSITION_MAX_DIRECT_CHILDREN )) || return 1
  (( count <= DECOMPOSITION_MAX_DESCENDANTS )) || return 1
  (( parent_depth < DECOMPOSITION_MAX_DEPTH )) || return 1
  keys=$(yq -p json -r '.children[].key // ""' <<< "$manifest" 2>/dev/null) || return 1
  [[ $(sort -u <<< "$keys" | sed '/^$/d' | wc -l) -eq $count ]] || return 1
  local title purpose
  while IFS=$'\t' read -r key title purpose child_depth scope; do
    [[ $key =~ ^[a-z0-9][a-z0-9-]*$ && -n $title && -n $purpose && -n $child_depth && -n $scope ]] || return 1
    # Reuse the existing normalizer; a malformed scope never becomes an
    # optimistic empty scope.
    [[ -n $(scope_marker_from_body "<!-- agentic-loop:scope $scope -->") ]] || return 1
  done < <(yq -p json -r '.children[] | [.key, (.title // ""), (.purpose // ""), (.acceptance_criteria // ""), .scope] | @tsv' <<< "$manifest" 2>/dev/null)
  while IFS= read -r dep; do
    [[ -z $dep ]] && continue
    grep -Fxq "$dep" <<< "$keys" || return 1
  done < <(yq -p json -r '.children[].depends_on[]? // ""' <<< "$manifest" 2>/dev/null)
  # A dependency can only point to an earlier key: this is both a cheap DAG
  # proof and makes materialization/retry deterministic.
  local seen=''
  while IFS= read -r key; do
    while IFS= read -r dep; do [[ -z $dep ]] || grep -Fxq "$dep" <<< "$seen" || return 1; done < <(yq -p json -r --arg k "$key" '.children[] | select(.key == $k) | .depends_on[]? // ""' <<< "$manifest")
    seen+="$key"$'\n'
  done <<< "$keys"
}


decomposition_materialize() {
  local parent=$1 plan=$2 manifest hash category key title purpose criteria scope deps child body child_id existing
  manifest=$(decomposition_manifest_from_plan "$plan")
  [[ -z $manifest ]] && return 1
  decomposition_validate "$manifest" 0 || return 2
  hash=$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')
  category=$(repo_api "issues/$parent" --jq '[.labels[].name | select(startswith("category:"))][0] // "category:improvement"' 2>/dev/null) || return 3
  declare -A children=()
  while IFS=$'\t' read -r key title purpose criteria scope deps; do
    existing=$(repo_api issues --method GET -f state=all -f per_page=100 --jq '.[] | select((.body // "") | contains("agentic-loop:child parent='"$parent"' key='"$key"' plan='"$hash"'")) | .number' 2>/dev/null | head -n1 || true)
    if [[ $existing =~ ^[1-9][0-9]*$ ]]; then child=$existing
    else
      body="## 目的\n\n$purpose\n\n## 親Issue\n\n#$parent の共通制約と統合受け入れ条件に従います。\n\n## 個別受け入れ条件\n\n$criteria\n\n<!-- agentic-loop:child parent=$parent key=$key plan=$hash -->\n<!-- agentic-loop:scope $scope -->"
      child=$(repo_api issues --method POST -f title="$title" -f body="$(unfold_body "$body")" --jq .number 2>/dev/null) || return 3
      [[ $child =~ ^[1-9][0-9]*$ ]] || return 3
    fi
    children["$key"]=$child
    child_id=$(repo_api "issues/$child" --jq .id 2>/dev/null) || return 3
    repo_api "issues/$parent/sub_issues" --method POST -f sub_issue_id="$child_id" >/dev/null 2>&1 || return 3
    repo_api "issues/$parent/dependencies/blocked_by" --method POST -f issue_id="$child" >/dev/null 2>&1 || return 3
  done < <(yq -p json -r '.children[] | [.key,.title,.purpose,.acceptance_criteria,.scope,([.depends_on[]?] | join(","))] | @tsv' <<< "$manifest")
  while IFS=$'\t' read -r key deps; do
    child=${children[$key]}
    IFS=, read -ra dep_list <<< "$deps"
    for dep in "${dep_list[@]}"; do [[ -z $dep ]] || repo_api "issues/$child/dependencies/blocked_by" --method POST -f issue_id="${children[$dep]}" >/dev/null 2>&1 || return 3; done
    repo_api "issues/$child/labels" --method PUT --input - <<< "{\"labels\":[\"$category\",\"agent:queued\"]}" >/dev/null 2>&1 || return 3
    project_add_issue "$child" || true; project_sync_state "$child" queued || true
  done < <(yq -p json -r '.children[] | [.key,([.depends_on[]?] | join(","))] | @tsv' <<< "$manifest")
  comment_issue "$parent" "<!-- agentic-loop:decomposition plan=$hash -->\n分解planを検証し、native sub-issueとして子Issueを作成・照合しました。親は全子の検証済み完了後に統合検証を行います。plan hash: \`$hash\`。"
}


# --- Worker resume from existing artifacts (see docs/decisions/0004) ---

# Re-confirm this worker still legitimately owns the Issue immediately before
# touching any Git state or starting a provider, defending against a stale or
# duplicate _worker invocation whose Issue was already requeued or reassigned
# underneath it. Silent on refusal (no label/comment/Git writes) so a benign
# race does not spam the Issue.
worker_confirm_running_label() {
  local issue=$1
  repo_api "issues/$issue" --jq '.labels[].name' 2>/dev/null | grep -Fxq "$(state_label running)"
}


# Observe Git and GitHub REST state for an Issue's dedicated branch/worktree
# and derive a resume phase, never trusting a prior worker's self-report (the
# whole result is computed from observable reality). Sets the RESUME_* globals
# consumed by worker(). Costs 0 REST(core) calls when the branch does not yet
# exist or has no commits beyond the default branch, otherwise at most 2 (a
# combined PR lookup, its detail, and check-runs only when an open PR exists).
# The detail request is deliberately REST (rather than GraphQL): it supplies
# the PR base/head and GitHub's mergeability observation.  When GitHub has not
# computed that observation yet, merge-tree against the already fetched refs
# provides a read-only Git fallback.  Thus an open PR costs at most 3 REST
# calls; a missing branch or a branch equal to the default tip still costs 0.
resume_probe() {
  local issue=$1 branch=$2 worktree=$3 default_branch=$4 repository=$5
  local branch_ref="refs/heads/$branch" worktree_root registered_branch other main_sha
  RESUME_PHASE="fresh" RESUME_DIRTY=0 RESUME_DIVERGED=0 RESUME_HEAD='' RESUME_REMOTE=''
  RESUME_PR='' RESUME_PR_STATE='' RESUME_PR_URL='' RESUME_CHECKS=''
  RESUME_BASE_BRANCH='' RESUME_BEHIND=0 RESUME_MERGEABLE='unknown' RESUME_MERGE_COMMIT=''

  if [[ -e $worktree ]]; then
    if worktree_root=$(cd "$worktree" && pwd -P 2>/dev/null); then
      registered_branch=$(git -C "$REPO_ROOT" worktree list --porcelain 2>/dev/null | awk -v path="$worktree_root" '
        $1 == "worktree" {matched=($2 == path)}
        matched && $1 == "branch" {print $2; exit}
      ')
      # Only a worktree that is genuinely registered to a different branch is
      # unsafe; a path that is not a registered worktree at all (corrupted, or
      # a plain directory) is left to the existing git-common-dir/.agents
      # resolvers immediately after this probe, which classify that case with
      # their own specific diagnostics.
      if [[ -n $registered_branch && $registered_branch != "$branch_ref" ]]; then
        RESUME_PHASE="unsafe-foreign"; return 0
      fi
      [[ -n $(git -C "$worktree_root" status --porcelain 2>/dev/null) ]] && RESUME_DIRTY=1
    fi
  else
    other=$(git -C "$REPO_ROOT" worktree list --porcelain 2>/dev/null | awk -v ref="$branch_ref" '
      $1 == "worktree" {path=$2} $1 == "branch" && $2 == ref {print path}
    ')
    [[ -n $other ]] && { RESUME_PHASE="unsafe-foreign"; return 0; }
  fi

  git -C "$REPO_ROOT" show-ref --verify --quiet "$branch_ref" || return 0
  RESUME_HEAD=$(git -C "$REPO_ROOT" rev-parse --verify "$branch_ref" 2>/dev/null) || return 0

  main_sha=$(git -C "$REPO_ROOT" rev-parse --verify -q "refs/remotes/origin/$default_branch" 2>/dev/null || true)
  if [[ -n $main_sha && $RESUME_HEAD == "$main_sha" ]]; then RESUME_PHASE="worktree-ready"; return 0; fi

  git -C "$REPO_ROOT" fetch origin "$branch" >/dev/null 2>&1 || true
  RESUME_REMOTE=$(git -C "$REPO_ROOT" rev-parse --verify -q "refs/remotes/origin/$branch" 2>/dev/null || true)
  if [[ -n $RESUME_REMOTE ]]; then
    local counts ahead behind
    counts=$(git -C "$REPO_ROOT" rev-list --left-right --count "$branch_ref...refs/remotes/origin/$branch" 2>/dev/null || printf '0\t0')
    read -r ahead behind <<< "$counts"
    [[ ${ahead:-0} -gt 0 && ${behind:-0} -gt 0 ]] && RESUME_DIVERGED=1
  fi

  # Joined with U+001F (not tab, see dependency_satisfied's identical note):
  # bash `read` treats consecutive/leading tabs as a single whitespace-class
  # delimiter and silently collapses an empty leading field, which happens
  # whenever there is no merged PR yet.
  local pr_tsv pr_jq merged_pr='' merged_sha='' merged_url='' open_pr='' open_url='' merged_merge_commit='' merged_base='' sep=$'\x1f'
  pr_jq='
    # resume-probe-prs
    (map(select(.merged_at != null)) | sort_by(.merged_at) | last) as $m |
    (map(select(.state == "open")) | sort_by(.created_at) | last) as $o |
    [$m.number // "", $m.head.sha // "", $m.html_url // "", $o.number // "", $o.html_url // "", $m.merge_commit_sha // "", $m.base.ref // ""] | join("SEP")
  '
  pr_jq=${pr_jq/SEP/$sep}
  pr_tsv=$(repo_api pulls --method GET -f state=all -f head="${repository%%/*}:$branch" -f per_page=100 --jq "$pr_jq" 2>/dev/null) || true
  [[ -n $pr_tsv ]] && IFS=$'\x1f' read -r merged_pr merged_sha merged_url open_pr open_url merged_merge_commit merged_base <<< "$pr_tsv"

  if [[ $merged_pr =~ ^[0-9]+$ ]]; then
    if [[ $merged_sha == "$RESUME_HEAD" && $RESUME_DIVERGED -eq 0 ]]; then
      RESUME_PHASE="pr-merged"; RESUME_PR=$merged_pr; RESUME_PR_URL=$merged_url; RESUME_PR_STATE="merged"
      RESUME_MERGE_COMMIT=${merged_merge_commit:-$merged_sha}; RESUME_BASE_BRANCH=${merged_base:-$default_branch}
    else
      RESUME_PHASE="needs-decision"; RESUME_PR=$merged_pr; RESUME_PR_URL=$merged_url; RESUME_PR_STATE="merged-mismatch"
    fi
    return 0
  fi

  if (( RESUME_DIVERGED )); then RESUME_PHASE="needs-decision"; RESUME_PR_STATE="diverged"; return 0; fi

  if [[ $open_pr =~ ^[0-9]+$ ]]; then
    RESUME_PR=$open_pr; RESUME_PR_URL=$open_url; RESUME_PR_STATE="open"; RESUME_PHASE="pr-open"
    local pr_detail base_ref='' base_sha='' pr_head='' mergeable='' mergeable_state='' merge_tree_status
    pr_detail=$(repo_api "pulls/$open_pr" --jq '[.base.ref // "", .base.sha // "", .head.sha // "", (if .mergeable == null then "null" else (.mergeable | tostring) end), .mergeable_state // "unknown"] | join("SEP")' 2>/dev/null) || true
    [[ -n $pr_detail ]] && IFS=$'\x1f' read -r base_ref base_sha pr_head mergeable mergeable_state <<< "${pr_detail/SEP/$sep}"
    RESUME_BASE_BRANCH=$base_ref
    # The target default branch was fetched by worker() immediately before this
    # probe.  Count only commits present on it but absent from the PR head.
    if [[ $base_ref == "$default_branch" && -n $pr_head ]] && git -C "$REPO_ROOT" cat-file -e "$pr_head^{commit}" 2>/dev/null; then
      RESUME_BEHIND=$(git -C "$REPO_ROOT" rev-list --count "$pr_head..refs/remotes/origin/$default_branch" 2>/dev/null || printf 0)
    fi
    case $mergeable in
      true) RESUME_MERGEABLE=true ;;
      false) RESUME_MERGEABLE=false ;;
      *)
        # GitHub can return null/unknown while asynchronously calculating this
        # field. merge-tree writes no worktree state and is therefore safe here.
        if [[ $mergeable_state == unknown && -n $base_sha && -n $pr_head ]] && git -C "$REPO_ROOT" cat-file -e "$base_sha^{commit}" 2>/dev/null && git -C "$REPO_ROOT" cat-file -e "$pr_head^{commit}" 2>/dev/null; then
          if git -C "$REPO_ROOT" merge-tree --write-tree "$base_sha" "$pr_head" >/dev/null 2>&1; then
            RESUME_MERGEABLE=true
          else
            merge_tree_status=$?
            [[ $merge_tree_status -eq 1 ]] && RESUME_MERGEABLE=false
          fi
        fi
        ;;
    esac
    RESUME_CHECKS=$(repo_api "commits/$RESUME_HEAD/check-runs" --jq '
      ([.check_runs[].conclusion, .check_runs[].status] | map(select(. != null))) as $s |
      if ($s | any(. == "failure" or . == "timed_out" or . == "cancelled")) then "failure"
      elif ($s | any(. == "in_progress" or . == "queued")) then "in_progress"
      elif (($s | length) > 0 and ($s | all(. == "success" or . == "neutral" or . == "skipped"))) then "success"
      else "unknown" end
    ' 2>/dev/null) || true
    [[ -n $RESUME_CHECKS ]] || RESUME_CHECKS=unknown
    if [[ $RESUME_BEHIND -gt 0 || $RESUME_MERGEABLE == false || $mergeable_state == dirty ]]; then
      RESUME_PHASE="needs-rebase"
    fi
    return 0
  fi

  if [[ -n $RESUME_REMOTE && $RESUME_REMOTE == "$RESUME_HEAD" ]]; then RESUME_PHASE="pushed-no-pr"; return 0; fi
  RESUME_PHASE="committed-unpushed"
}


# Remove only the local branch for an Issue whose PR is already merged when no
# worktree exists to remove alongside it (e.g. the worktree was already cleaned
# up by a previous, interrupted worker run). Mirrors cleanup_completed_worker's
# safety checks minus the worktree-specific ones.
cleanup_completed_branch_only() {
  local branch=$1 merged_oid=$2 local_oid
  local branch_ref="refs/heads/$branch"
  local_oid=$(git -C "$REPO_ROOT" rev-parse --verify "$branch_ref" 2>/dev/null) || return 1
  [[ $local_oid == "$merged_oid" ]] || return 1
  git -C "$REPO_ROOT" worktree list --porcelain 2>/dev/null | grep -Fxq "branch $branch_ref" && return 1
  git -C "$REPO_ROOT" update-ref -d "$branch_ref" "$local_oid"
}


# Human-readable (Japanese) hint of the next safe action for a resume phase,
# embedded in the handoff comment so a worker picking up mid-stream does not
# need to re-derive it from the raw fields.
resume_next_hint() {
  case $1 in
    fresh) printf '計画を作成し実装を開始する。' ;;
    worktree-ready) printf '既存のworktree/branchを再利用して計画を作成し実装を開始する。' ;;
    committed-unpushed) printf '既存のlocal commitを踏まえて実装を継続し、pushとPR作成まで進める。' ;;
    pushed-no-pr) printf '既存のpush済みcommitを踏まえてPRを作成する。' ;;
    pr-open) printf '既存PRのchecksとreview対応を継続する。' ;;
    needs-rebase) printf 'default branchをgit mergeで通常取り込み、競合を解消して通常pushする。checksとreviewを再確認し、既存PRをmergeしてdefault branch上の検証まで完了する。rebase、reset、force-push、履歴書き換えは行わない。' ;;
    pr-merged) printf 'worker側で自動的にcleanupし完了報告する（providerは起動しない）。' ;;
    needs-decision) printf '人による分岐またはmerge済みcommitとの不一致の解消を待つ。' ;;
    unsafe-foreign) printf '人によるworktree/branchの競合解消を待つ。' ;;
    *) printf '状態を確認する。' ;;
  esac
}


resume_handoff_file() { printf '%s/workers/%s.handoff' "$STATE_ROOT" "$1"; }


# Render the single handoff comment body. Every field is derived from Git/
# GitHub observation (see resume_probe), never from a provider's self-report,
# so this artifact cannot smuggle secrets or log content and does not need a
# secret-guard pass of its own.
resume_handoff_body() {
  local phase=$1 branch=$2 head=$3 remote=$4 pr=$5 pr_state=$6 checks=$7 dirty=$8 diverged=$9 now=${10} next
  next=$(resume_next_hint "$phase")
  printf '<!-- agentic-loop:handoff schema=1 phase=%s branch=%s head=%s remote=%s pr=%s pr_state=%s checks=%s base=%s behind=%s mergeable=%s dirty=%s diverged=%s updated=%s -->\\n### 再開のための引き継ぎ（Git/GitHub観測結果から自動生成。workerの自己申告ではありません）\\n- phase: %s\\n- branch: `%s` (head=%s, remote=%s, dirty=%s, diverged=%s)\\n- PR: %s (state=%s, checks=%s, base=%s, behind=%s, mergeable=%s)\\n- 次の安全な操作: %s' \
    "$phase" "$branch" "${head:-none}" "${remote:-none}" "${pr:-none}" "${pr_state:-none}" "${checks:-none}" "${RESUME_BASE_BRANCH:-none}" "${RESUME_BEHIND:-0}" "${RESUME_MERGEABLE:-unknown}" "$dirty" "$diverged" "$now" \
    "$phase" "$branch" "${head:-none}" "${remote:-none}" "$dirty" "$diverged" "${pr:-none}" "${pr_state:-none}" "${checks:-none}" "${RESUME_BASE_BRANCH:-none}" "${RESUME_BEHIND:-0}" "${RESUME_MERGEABLE:-unknown}" "$next"
}


# Create or (via a single PATCH) update the one durable handoff comment for an
# Issue, mirroring the lease comment's PATCH-in-place pattern (see docs/
# decisions/0003) so repeated resumes never accumulate duplicate comments.
resume_handoff_upsert() {
  local issue=$1 body=$2 file id
  file=$(resume_handoff_file "$issue")
  if [[ -r $file ]]; then
    read -r id < "$file" || id=''
    if [[ $id =~ ^[0-9]+$ ]] && comment_patch "$id" "$body" >/dev/null 2>&1; then
      return 0
    fi
  fi
  id=$(comment_post "$issue" "$body" --jq '.id' 2>/dev/null | tr -d '[:space:]' || true)
  [[ $id =~ ^[0-9]+$ ]] && { mkdir -p "$STATE_ROOT/workers"; printf '%s\n' "$id" > "$file"; }
}


resume_handoff_write() {
  local issue=$1 branch=$2
  printf '%s\n' "$RESUME_PHASE" > "$(worker_phase_file "$issue")" 2>/dev/null || true
  # Cache the same Git/GitHub-observed fields locally (mirrors the handoff
  # comment) so `status` can show phase/branch/PR/checks without any further
  # API call. Every field here is already derived purely from Git and REST
  # observation by resume_probe, so it cannot smuggle secrets (see docs/
  # decisions/0004).
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$RESUME_PHASE" "$branch" "${RESUME_PR:-}" "${RESUME_PR_URL:-}" "${RESUME_PR_STATE:-}" "${RESUME_CHECKS:-}" "${RESUME_DIRTY:-0}" "${RESUME_DIVERGED:-0}" > "$(worker_resume_file "$issue")" 2>/dev/null || true
  resume_handoff_upsert "$issue" "$(resume_handoff_body "$RESUME_PHASE" "$branch" "$RESUME_HEAD" "$RESUME_REMOTE" "$RESUME_PR" "$RESUME_PR_STATE" "$RESUME_CHECKS" "$RESUME_DIRTY" "$RESUME_DIVERGED" "$(date +%s)")" || true
}


# Local-only read of the cached resume fields written above: phase, branch, PR
# number, PR URL, PR state, checks, dirty (0/1), diverged (0/1).
worker_resume_read() {
  local file="$(worker_resume_file "$1")"
  [[ -r $file ]] || return 1
  IFS=$'\t' read -r RESUME_LOCAL_PHASE RESUME_LOCAL_BRANCH RESUME_LOCAL_PR RESUME_LOCAL_PR_URL RESUME_LOCAL_PR_STATE RESUME_LOCAL_CHECKS RESUME_LOCAL_DIRTY RESUME_LOCAL_DIVERGED < "$file"
}


# Prepended to the plan/exec prompts so a resumed worker treats the observed
# Git/PR state as ground truth instead of restarting work already done.
resume_context_block() {
  local branch=$1
  [[ $RESUME_PHASE == fresh ]] && return 0
  cat <<EOF
再開コンテキスト（Git/GitHubの観測結果。信頼してよい）:
- phase: $RESUME_PHASE
- branch: $branch (head=${RESUME_HEAD:0:12}, remote=${RESUME_REMOTE:0:12}, dirty=$RESUME_DIRTY)
EOF
  if [[ -n $RESUME_PR ]]; then
    printf -- '- 既存PR: #%s (state=%s, checks=%s) %s\n' "$RESUME_PR" "$RESUME_PR_STATE" "$RESUME_CHECKS" "$RESUME_PR_URL"
    printf -- '既存のbranch `%s` とPR #%s を再利用してください。新しいPRを作成せず、force-pushや履歴の書き換えは行わないでください。\n' "$branch" "$RESUME_PR"
    if [[ $RESUME_PHASE == needs-rebase ]]; then
      printf -- '対象default branchは `%s`、behind=%s、mergeable=%s です。`git merge origin/%s` による通常mergeでbaseを取り込み、競合を解消してください。rebase、reset、force-push、履歴書き換えは禁止です。解消後は同じbranchへ通常pushし、checksとreviewを再確認して既存PRをmergeし、default branch上の検証まで完了してください。\n' "$RESUME_BASE_BRANCH" "$RESUME_BEHIND" "$RESUME_MERGEABLE" "$RESUME_BASE_BRANCH"
    fi
  else
    printf -- '既存のbranch `%s` を再利用してください（新規branchは作成しないでください）。\n' "$branch"
  fi
}


resume_needs_decision_body() {
  local worker=$1 branch=$2
  case $RESUME_PR_STATE in
    merged-mismatch)
      printf '<!-- agentic-loop:needs-input worker=%s reason=resume-merged-mismatch pr=%s -->\\n再開時の検査で、branch `%s` の現在のcommit（%s）が、merge済みPR #%s のhead commitと一致しませんでした。mergeの後に別途commitが追加されたか、branchが書き換えられた可能性があります。自動ではforce-pushやreset、branch/worktreeの削除を行いません。既存のworktree/branch/remote branchを確認し、次のいずれかを人手で選んでから、このIssueへ返信してください: (1) 追加分を新しいIssueとして切り出す、(2) branchをmerge済みcommitにresetして完了扱いにする、(3) その他の対応方針を指示する。' \
        "$worker" "$RESUME_PR" "$branch" "${RESUME_HEAD:0:12}" "$RESUME_PR" ;;
    *)
      printf '<!-- agentic-loop:needs-input worker=%s reason=resume-diverged -->\\n再開時の検査で、branch `%s` のlocal commit（%s）とremote commit（%s）が分岐していることを検出しました（双方に相手の持たない独自commitがあります）。自動でのforce-pushやmergeは行いません。どちらのcommitを正とするか人手で判断し、このIssueへ返信してください。既存のworktree/local branch/remote branchは変更していません。' \
        "$worker" "$branch" "${RESUME_HEAD:0:12}" "${RESUME_REMOTE:0:12}" ;;
  esac
}


worker() {
  local issue=$1 worker=$2 repository default_branch branch worktree result git_common_dir agents_dir exit_code=0 heartbeat_pid merged_pr='' merged_oid=''
  worker_confirm_running_label "$issue" || { clear_worker_local "$issue"; return 0; }
  mkdir -p "$STATE_ROOT/workers"
  date +%s > "$(worker_started_file "$issue")" 2>/dev/null || true
  progress_touch "$issue" claim
  repository=$(repo_name)
  default_branch=$(repo_api '' --jq .default_branch)
  branch="agent/issue-$issue"
  worktree="$WORKTREE_ROOT/issue-$issue"
  if ! git -C "$REPO_ROOT" fetch origin "$default_branch"; then
    progress_touch "$issue" failed
    set_issue_state "$issue" failed || true
    comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker -->\n最新のdefault branchをfetchできませんでした。リポジトリへのアクセスが復旧した後、このIssueは安全に再キューできます。" || true
    clear_worker_local "$issue"
    return 0
  fi

  resume_probe "$issue" "$branch" "$worktree" "$default_branch" "$repository"
  resume_handoff_write "$issue" "$branch"
  progress_touch "$issue" resume
  case $RESUME_PHASE in
    unsafe-foreign)
      progress_touch "$issue" failed
      set_issue_state "$issue" failed || true
      comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker reason=foreign-artifact -->\n専用worktreeまたはbranch \`$branch\` が別Issueの成果物、または想定外の状態で使用中のため、workerを起動しませんでした。既存のworktree/branch/commitは変更していません。\`git worktree list\` と \`git branch -a\` の出力を確認し、競合を解消した上で \`agent:queued\` を付け直してください。" || true
      clear_worker_local "$issue"
      return 0
      ;;
    needs-decision)
      progress_touch "$issue" needs-input
      set_issue_state "$issue" needs-input || true
      project_sync_state "$issue" needs-input || true
      comment_issue "$issue" "$(resume_needs_decision_body "$worker" "$branch")" || true
      clear_worker_local "$issue"
      return 0
      ;;
    pr-merged)
      local cleaned=1
      worker_critical_begin "$issue"
      if ! preflight_reevaluate_diff "$issue" "$RESUME_HEAD" "$default_branch"; then
        :
      elif trace_gate "$issue" "$RESUME_PR" "$RESUME_HEAD" "${RESUME_MERGE_COMMIT:-$RESUME_HEAD}" "${RESUME_BASE_BRANCH:-$default_branch}"; then
        if [[ -e $worktree ]]; then
          cleanup_completed_worker "$worktree" "$branch" "$RESUME_HEAD" || cleaned=0
        else
          cleanup_completed_branch_only "$branch" "$RESUME_HEAD" || cleaned=0
        fi
        if [[ $cleaned -eq 1 ]]; then
          clear_attempts "$issue"
          progress_touch "$issue" 'done'
          set_issue_state "$issue" completed
          project_sync_state "$issue" completed
          comment_issue "$issue" "<!-- agentic-loop:completed pr=$RESUME_PR resumed=1 -->\n再開時の検査で、対応PR $RESUME_PR_URL が既にmerge済みであることを確認しました（providerは起動していません）。専用worktreeとlocal branch \`$branch\` を削除しました。remote branchは復旧とGitHubのPR設定との責務分離のため、Supervisorからは削除しません。"
          repo_api "issues/$issue" --method PATCH -f state=closed >/dev/null
        else
          progress_touch "$issue" failed
          set_issue_state "$issue" failed
          project_sync_state "$issue" failed
          comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker reason=merge-or-cleanup resumed=1 -->\n再開時の検査でPR $RESUME_PR_URL のmergeを確認しましたが、安全なcleanupを完了できませんでした。Issueはcloseせず、既存のworktree/branchを保持します。原因確認後に再試行するには \`agent:queued\` を再度付与してください。"
        fi
      else
        progress_touch "$issue" failed
        set_issue_state "$issue" failed
        project_sync_state "$issue" failed
        comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker reason=traceability-invalid detail=$TRACE_INVALID_REASON resumed=1 -->\n再開時の検査でPR $RESUME_PR_URL のmergeを確認しましたが、トレーサビリティ記録の検証に失敗したため完了処理を保留しました（detail=$TRACE_INVALID_REASON）。Issueはcloseせず、既存のworktree/branchを保持します。PR本文の \`agentic-loop:traceability\` code blockを確認し、修正後に \`agent:queued\` を再度付与してください。"
      fi
      worker_critical_end "$issue"
      clear_worker_local "$issue"
      return 0
      ;;
  esac

  if [[ ! -e $worktree ]]; then
    git -C "$REPO_ROOT" worktree prune
    if git -C "$REPO_ROOT" show-ref --verify --quiet "refs/heads/$branch"; then
      git -C "$REPO_ROOT" worktree add "$worktree" "$branch" || exit_code=$?
    else
      git -C "$REPO_ROOT" worktree add -b "$branch" "$worktree" "origin/$default_branch" || exit_code=$?
    fi
    if [[ $exit_code -ne 0 ]]; then
      progress_touch "$issue" failed
      set_issue_state "$issue" failed || true
      comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker -->\n専用worktreeを準備できませんでした。調査できるように既存のbranch dataは保持しています。" || true
      clear_worker_local "$issue"
      return 0
    fi
  fi
  progress_touch "$issue" worktree
  result="$STATE_ROOT/issue-$issue-result.txt"
  if ! git_common_dir=$(resolve_worker_git_common_dir "$worktree"); then
    progress_touch "$issue" failed
    set_issue_state "$issue" failed || true
    comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker -->\n専用worktreeのGit common directoryを安全に解決できなかったため、workerを起動しませんでした。追加の書き込み可能pathは許可していません。Git metadataとworktreeの整合性を確認・復旧した後、このIssueを安全に再キューしてください。" || true
    clear_worker_local "$issue"
    return 0
  fi
  if ! agents_dir=$(resolve_worker_agents_dir "$worktree"); then
    progress_touch "$issue" failed
    set_issue_state "$issue" failed || true
    comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker -->\n専用worktreeの.agents directoryを安全に解決できなかったため、workerを起動しませんでした。保護対象のrepository pathは書き込み可能にしていません。.agents directoryを安全な通常directoryとして復旧した後、このIssueを安全に再キューしてください。" || true
    clear_worker_local "$issue"
    return 0
  fi
  (
    while :; do sleep $((LEASE_SECONDS / 3 + 1)); lease_heartbeat "$issue" "$worker" || true; done
  ) & heartbeat_pid=$!
  local plan_usage="$STATE_ROOT/issue-$issue-plan-usage.txt" exec_usage="$STATE_ROOT/issue-$issue-usage.txt"
  local plan_file="$STATE_ROOT/issue-$issue-plan.txt" attempt=0 protocol_retry=0 max_retries started failure_context='' exhausted=0 exec_rc plan_rc
  local resume_context; resume_context=$(resume_context_block "$branch")
  max_retries=$(agent_plan_max_retries)
  while :; do
    # A pause/abort's cooperative stop request is checked only at this stage
    # boundary (never mid-provider-call): an authorized operator's own
    # subsequent Label/comment transition owns the outcome, so this exit stays
    # silent (see docs/decisions/0019-issue-level-execution-control.md).
    if worker_stop_requested "$issue"; then
      kill "$heartbeat_pid" 2>/dev/null || true; wait "$heartbeat_pid" 2>/dev/null || true
      lease_release "$issue" "$worker"
      return 0
    fi
    started=$(date +%s)
    progress_touch "$issue" plan
    plan_rc=0
    run_stage_candidates plan "$worktree" "$git_common_dir" "$agents_dir" "$plan_file" "$plan_usage" "$(plan_prompt "$issue" "$repository" "$failure_context" "$resume_context")" || plan_rc=$?
    plan_rc=$STAGE_RC
    agent_post_usage "$issue" "$worker" "$plan_usage" "$STAGE_EXIT_CODE" "$(($(date +%s) - started))"
    # Pool exhaustion (or no usable candidate) on plan is not a task failure:
    # re-queue without burning attempts so the next claim can use a recovered
    # pool. Do not proceed to exec while every pool is spent.
    if (( plan_rc == 1 || plan_rc == 2 )); then
      if (( plan_rc == 2 )); then agent_mark_exhausted; fi
      exhausted=1
      exit_code=$STAGE_EXIT_CODE
      break
    fi
    if (( plan_rc != 0 )); then
      : > "$plan_file"
      say 'plan段の全候補が失敗したため、plan結果なしでexecへ進みます。' >&2
    fi
    worker_refine_scope_from_plan "$issue" "$plan_file"
    progress_touch "$issue" preflight
    if ! preflight_gate "$issue" "$plan_file"; then
      kill "$heartbeat_pid" 2>/dev/null || true; wait "$heartbeat_pid" 2>/dev/null || true
      lease_release "$issue" "$worker"
      clear_worker_local "$issue"
      return 0
    fi
    if ! workload_gate "$issue" "$plan_file"; then
      kill "$heartbeat_pid" 2>/dev/null || true; wait "$heartbeat_pid" 2>/dev/null || true
      lease_release "$issue" "$worker"
      clear_worker_local "$issue"
      return 0
    fi
    if [[ -n $(decomposition_manifest_from_plan "$plan_file") ]]; then
      local existing_children
      existing_children=$(repo_api "issues/$issue/sub_issues" --jq 'length' 2>/dev/null || true)
      if [[ ${existing_children:-0} == 0 ]]; then
        # Creating native sub-Issues/dependencies is unsafe to interrupt
        # mid-sequence (see docs/decisions/0019); a pause/abort drain waits
        # out this marker (bounded by pause_grace_seconds) before signaling.
        worker_critical_begin "$issue"
        if decomposition_materialize "$issue" "$plan_file"; then
          worker_critical_end "$issue"
          kill "$heartbeat_pid" 2>/dev/null || true; wait "$heartbeat_pid" 2>/dev/null || true
          lease_release "$issue" "$worker"
          progress_touch "$issue" blocked
          set_issue_state "$issue" blocked; project_sync_state "$issue" blocked || true
          comment_issue "$issue" '<!-- agentic-loop:decomposition-wait -->\n子Issueを公開しました。親は子の検証済み完了後に既存の依存回復処理で再キューされ、統合検証だけを実行します。' || true
          scope_cache_clear "$issue"; clear_conflict_wait "$issue"; clear_worker_local "$issue"
          return 0
        fi
        worker_critical_end "$issue"
        progress_touch "$issue" needs-input
        set_issue_state "$issue" needs-input; project_sync_state "$issue" needs-input || true
        comment_issue "$issue" '<!-- agentic-loop:decomposition-invalid -->\n分解manifestをGitHub変更前に検証または構成できませんでした。子Issueはqueueへ公開していません。manifestのscope・依存DAG・受け入れ条件・GitHub sub-issues権限を確認してください。' || true
        kill "$heartbeat_pid" 2>/dev/null || true; wait "$heartbeat_pid" 2>/dev/null || true; lease_release "$issue" "$worker"; clear_worker_local "$issue"
        return 0
      fi
    fi
    started=$(date +%s)
    exec_rc=0
    progress_touch "$issue" exec
    run_stage_candidates exec "$worktree" "$git_common_dir" "$agents_dir" "$result" "$exec_usage" "$(exec_prompt "$issue" "$repository" "$plan_file" "$resume_context")" || exec_rc=$?
    exec_rc=$STAGE_RC
    exit_code=$STAGE_EXIT_CODE
    agent_post_usage "$issue" "$worker" "$exec_usage" "$exit_code" "$(($(date +%s) - started))"
    worker_refine_scope_from_diff "$issue" "$worktree" "$default_branch"
    # A pool-quota exhaustion (or no usable candidate at all) is not a task
    # failure: mark what we can and re-queue the Issue so the next claim tries
    # the next pool/model without burning attempts. Every candidate failing
    # with a model-specific error (STAGE_RC=3) is transient like a task
    # failure: it flows into the bounded replan/failed path below.
    if (( exec_rc == 1 || exec_rc == 2 )); then
      if (( exec_rc == 2 )); then agent_mark_exhausted; fi
      exhausted=1
      break
    fi
    # A clean provider exit without a terminal marker is a protocol violation,
    # not an implementation failure. Retry exec once with the same plan before
    # spending a flagship replan; a second violation deterministically fails.
    if (( exec_rc == 0 )) && [[ $exit_code -eq 0 ]] && ! agent_result_terminal_marker "$result" >/dev/null; then
      if (( protocol_retry == 0 )); then
        protocol_retry=1
        comment_issue "$issue" "<!-- agentic-loop:protocol-retry worker=$worker attempt=1 -->\nexec段が終了markerなしで正常終了しました。再計画せず同じ計画でexecを1回だけ再実行し、同一turn内の待機完遂と終了marker返却を求めます。" || true
        started=$(date +%s)
        exec_rc=0
        progress_touch "$issue" exec
        run_stage_candidates exec "$worktree" "$git_common_dir" "$agents_dir" "$result" "$exec_usage" "$(exec_prompt "$issue" "$repository" "$plan_file" "$resume_context")
前回のexecは有効な終了markerなしで終了しました。同じ計画を継続し、外部待機をこのturn内で完遂したうえで、指定された終了markerを最後の非空行として必ず返してください。" || exec_rc=$?
        exec_rc=$STAGE_RC
        exit_code=$STAGE_EXIT_CODE
        agent_post_usage "$issue" "$worker" "$exec_usage" "$exit_code" "$(($(date +%s) - started))"
        worker_refine_scope_from_diff "$issue" "$worktree" "$default_branch"
        if (( exec_rc == 1 || exec_rc == 2 )); then
          if (( exec_rc == 2 )); then agent_mark_exhausted; fi
          exhausted=1
          break
        fi
      fi
      if [[ $exit_code -eq 0 ]] && ! agent_result_terminal_marker "$result" >/dev/null; then
        break
      fi
    fi
    { [[ $exit_code -eq 0 ]] && agent_result_is "$result" completed; } && break
    { [[ $exit_code -eq 0 ]] && agent_result_is "$result" needs-input; } && break
    { [[ $exit_code -eq 0 ]] && agent_result_is "$result" declined; } && break
    if worker_stop_requested "$issue"; then
      kill "$heartbeat_pid" 2>/dev/null || true; wait "$heartbeat_pid" 2>/dev/null || true
      lease_release "$issue" "$worker"
      return 0
    fi
    (( attempt >= max_retries )) && break
    attempt=$((attempt + 1))
    failure_context=$(tail -c 2000 "$result" 2>/dev/null || true)
    progress_touch "$issue" replan
    comment_issue "$issue" "<!-- agentic-loop:replan worker=$worker attempt=$attempt -->\nexec段が完了条件を満たさなかったため、flagshipモデルで計画を見直して再実行します（$attempt/$max_retries）。" || true
  done
  kill "$heartbeat_pid" 2>/dev/null || true
  wait "$heartbeat_pid" 2>/dev/null || true
  # Provider execution is over. Release durable ownership before the terminal
  # Label transition so a failed/requeued Issue can be claimed immediately on
  # another host instead of waiting for the last heartbeat to expire. While
  # the Label is still running, new contenders still fail their queued check.
  lease_release "$issue" "$worker"
  # A postmortem `link` turn (see bin/lib/agentic-loop/postmortem.sh) already
  # made its own complete, intentional Label transition (agent:blocked) and
  # released its own lease/scope/conflict state from inside the exec turn's
  # provider call. It has no PR to look for -- the ordinary completion path
  # below assumes one exists -- so honor that already-made decision instead of
  # re-evaluating this exec turn's own result marker (see ADR 0024).
  if [[ $(postmortem_turn_marker_read "$issue") == link ]]; then
    postmortem_turn_marker_clear "$issue"
    clear_worker_local "$issue"
    return 0
  fi
  project_add_pull_requests "$branch"
  if [[ $exit_code -eq 0 ]] && agent_result_is "$result" completed; then
    # Cleanup + the final Label/close transition is unsafe to interrupt
    # mid-sequence (see docs/decisions/0019); a pause/abort drain waits out
    # this marker (bounded by pause_grace_seconds) before signaling.
    worker_critical_begin "$issue"
    progress_touch "$issue" merge
    # A postmortem `complete` turn (see bin/lib/agentic-loop/postmortem.sh)
    # already gate-checked that every linked action item is verified and the
    # body's analysis is filled in, then wrote this marker instead of closing
    # the Issue itself. It made no commit for this Issue (only GitHub API
    # calls), so completion here means "the branch never advanced past the
    # default branch" rather than "a PR merged" -- reuse cleanup_completed_
    # worker's existing oid-match safety check with the default branch tip in
    # place of a merged commit. If the branch DID advance (real work happened
    # in this turn), the check below fails closed and this falls through to
    # the ordinary merged-PR completion path, so unmerged work is never
    # silently discarded.
    local postmortem_marker='' postmortem_default_tip=''
    postmortem_marker=$(postmortem_turn_marker_read "$issue")
    [[ $postmortem_marker == complete ]] && postmortem_default_tip=$(git -C "$REPO_ROOT" rev-parse --verify -q "refs/remotes/origin/$default_branch" 2>/dev/null || true)
    if [[ $postmortem_marker == complete && -n $postmortem_default_tip ]] && cleanup_completed_worker "$worktree" "$branch" "$postmortem_default_tip"; then
      postmortem_turn_marker_clear "$issue"
      clear_attempts "$issue"
      progress_touch "$issue" 'done'
      set_issue_state "$issue" completed
      project_sync_state "$issue" completed
      comment_issue "$issue" "<!-- agentic-loop:completed postmortem=1 -->\nWorker \`$worker\` の完了報告と \`postmortem complete\` によるaction item検証済みの確認により、専用worktreeとlocal branch \`$branch\` を削除しました（このturnにcommitはありません）。"
      repo_api "issues/$issue" --method PATCH -f state=closed >/dev/null
    else
      [[ $postmortem_marker == complete ]] && postmortem_turn_marker_clear "$issue"
      local merged_number='' merged_base='' merged_commit=''
      IFS=$'\t' read -r merged_pr merged_oid merged_number merged_base merged_commit < <(repo_api pulls --method GET -f state=closed -f head="${repository%%/*}:$branch" -f per_page=100 --jq 'map(select(.merged_at != null and .head.ref == "'"$branch"'")) | first | [.html_url, .head.sha, (.number|tostring), .base.ref, (.merge_commit_sha // .head.sha)] | @tsv' 2>/dev/null || true) || true
      progress_touch "$issue" cleanup
      if [[ -n $merged_pr && $merged_oid =~ ^[0-9a-fA-F]{40}$ ]]; then
        if ! preflight_reevaluate_diff "$issue" "$merged_oid" "$default_branch"; then
          :
        elif trace_gate "$issue" "$merged_number" "$merged_oid" "$merged_commit" "$merged_base"; then
          if cleanup_completed_worker "$worktree" "$branch" "$merged_oid"; then
            clear_attempts "$issue"
            progress_touch "$issue" 'done'
            set_issue_state "$issue" completed
            project_sync_state "$issue" completed
            comment_issue "$issue" "<!-- agentic-loop:completed pr=$merged_pr -->\nWorker \`$worker\` の完了報告と対応PRのmergeをGitHubで確認し、専用worktreeとlocal branch \`$branch\` を削除しました: $merged_pr\nremote branchは復旧とGitHubのPR設定との責務分離のため、Supervisorからは削除しません。"
            repo_api "issues/$issue" --method PATCH -f state=closed >/dev/null
          else
            exit_code=1
            progress_touch "$issue" failed
            set_issue_state "$issue" failed
            project_sync_state "$issue" failed
            comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker reason=merge-or-cleanup -->\nWorkerは完了を報告しましたが、branch \`$branch\` のmerge済みPRと一致するcommitを確認できないか、安全なcleanupを完了できませんでした。Issueはcloseせず、残っているworktreeまたはbranch dataを保持します。原因確認後に再試行するには \`agent:queued\` を再度付与してください。"
          fi
        else
          exit_code=1
          progress_touch "$issue" failed
          set_issue_state "$issue" failed
          project_sync_state "$issue" failed
          comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker reason=traceability-invalid detail=$TRACE_INVALID_REASON -->\nWorkerは完了を報告しmergeを確認しましたが、トレーサビリティ記録の検証に失敗したため完了処理を保留しました（detail=$TRACE_INVALID_REASON）。Issueはcloseせず、既存のworktree/branchを保持します。PR本文の \`agentic-loop:traceability\` code blockを確認し、修正後に \`agent:queued\` を再度付与してください。"
        fi
      else
        exit_code=1
        progress_touch "$issue" failed
        set_issue_state "$issue" failed
        project_sync_state "$issue" failed
        comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker reason=merge-or-cleanup -->\nWorkerは完了を報告しましたが、branch \`$branch\` のmerge済みPRと一致するcommitを確認できないか、安全なcleanupを完了できませんでした。Issueはcloseせず、残っているworktreeまたはbranch dataを保持します。原因確認後に再試行するには \`agent:queued\` を再度付与してください。"
      fi
    fi
    worker_critical_end "$issue"
  elif [[ $exit_code -eq 0 ]] && agent_result_is "$result" needs-input; then
    progress_touch "$issue" needs-input
    set_issue_state "$issue" needs-input
    project_sync_state "$issue" needs-input
    comment_issue "$issue" "<!-- agentic-loop:needs-input worker=$worker -->\n重大な決定または権限が必要です。直前のworker commentを確認し、自動的に再キューするにはここへ返信してください。"
  elif [[ $exit_code -eq 0 ]] && agent_result_is "$result" declined; then
    clear_attempts "$issue"
    progress_touch "$issue" needs-input
    set_issue_state "$issue" needs-input
    project_sync_state "$issue" needs-input
    comment_issue "$issue" "<!-- agentic-loop:declined worker=$worker -->\nWorkerはこのIssueを実施不要または実施不能と提案しました（理由は直前のcommentを参照）。worker自身はcloseしません。認可済みの運用者が確認し、必要なら \`bin/agentic-loop dispose $issue --reason cancelled\` 等で終了してください。"
  elif (( exhausted )); then
    clear_attempts "$issue"
    progress_touch "$issue" queued
    set_issue_state "$issue" queued
    project_sync_state "$issue" queued
    comment_issue "$issue" "<!-- agentic-loop:exhausted worker=$worker pool=${LAST_EXHAUSTED_POOL:-} -->\nprovider の利用上限（token/rate limit）に達したため、Issueを failed にせず再キューします。全プールが利用不可の間だけSupervisorのclaimを一時停止し、枠回復後に自動再開します。"
  else
    progress_touch "$issue" failed
    set_issue_state "$issue" failed
    project_sync_state "$issue" failed
    comment_issue "$issue" "<!-- agentic-loop:failed worker=$worker exit=$exit_code -->\nWorkerは検証済みのmergeを完了せずに停止しました。Logはlocalにのみ保持され、秘密情報はここに転記されません。Supervisorが自動的に再試行し、上限回数に達してもcloseせず \`agent:parked\`（人間トリアージ待ち）へ移します。"
  fi
  scope_cache_clear "$issue"
  clear_conflict_wait "$issue"
  clear_worker_local "$issue"
}
