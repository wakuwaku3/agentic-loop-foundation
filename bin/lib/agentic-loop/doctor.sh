# Module: doctor.sh. Sourced by bin/agentic-loop (see docs/decisions/0013-agentic-loop-modules.md).
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155



doctor_add() {
  local level=$1 name=$2 impact=$3 recovery=$4
  DOCTOR_LEVELS+=("$level"); DOCTOR_NAMES+=("$name")
  DOCTOR_IMPACTS+=("$impact"); DOCTOR_RECOVERIES+=("$recovery")
  case $level in warning) DOCTOR_WARNINGS=$((DOCTOR_WARNINGS + 1)) ;; failure) DOCTOR_FAILURES=$((DOCTOR_FAILURES + 1)) ;; esac
}


doctor_positive_config() {
  local name=$1 value=$2
  [[ $value =~ ^[1-9][0-9]*$ ]] || doctor_add failure "設定値: $name" 'Supervisorを安全な値で実行できません。' "docs/operations/issue-queue.md を確認し、$name を正の整数に修正してください。"
}


doctor_non_negative_config() {
  local name=$1 value=$2
  [[ $value =~ ^[0-9]+$ ]] || doctor_add failure "設定値: $name" 'Supervisorを安全な値で実行できません。' "docs/operations/issue-queue.md を確認し、$name を0以上の整数に修正してください。"
}


doctor_enum_config() {
  local name=$1 value=$2 allowed
  shift 2
  for allowed in "$@"; do [[ $value == "$allowed" ]] && return 0; done
  doctor_add failure "設定値: $name" 'Supervisorを安全な値で実行できません。' "docs/operations/issue-queue.md を確認し、$name を $* のいずれかに修正してください。"
}


# Whether every residual worktree/branch path (newline-separated, at most 3
# each per doctor_collect's sampling) belongs to an Issue GitHub currently
# reports as agent:running, so a running worker's own artifacts are not
# misclassified as orphaned garbage.
doctor_residual_belongs_to_running() {
  local worktrees=$1 branches=$2 running_numbers item num
  running_numbers=$(running_issues | sed -n 's/^#\([0-9][0-9]*\).*/\1/p')
  while IFS= read -r item; do
    [[ -n $item ]] || continue
    num=${item##*/issue-}
    grep -Fxq "$num" <<< "$running_numbers" || return 1
  done <<< "$worktrees"
  while IFS= read -r item; do
    [[ -n $item ]] || continue
    num=${item##*/issue-}
    grep -Fxq "$num" <<< "$running_numbers" || return 1
  done <<< "$branches"
  return 0
}


doctor_collect() {
  local origin='' default_branch='' hooks='' project_owner='' project_number='' project_id='' project_ok='' repository='' owner='' name='' unit_dir unit issue_worktrees='' agent_branches='' log_files=''
  if [[ -n $CONFIG_ERROR ]]; then
    doctor_add failure '設定ファイル' '設定を解釈できないためSupervisorを安全に起動できません。' '.agentic-loop.toml（および任意の .agentic-loop.local.toml）を有効なTOMLに修正し、yqを導入してください。'
  else
    doctor_positive_config POLL_SECONDS "$POLL_SECONDS"; doctor_positive_config POLL_MAX_SECONDS "$POLL_MAX_SECONDS"; doctor_positive_config MAX_WORKERS "$MAX_WORKERS"
    doctor_positive_config LEASE_SECONDS "$LEASE_SECONDS"; doctor_non_negative_config STOP_TIMEOUT "$STOP_TIMEOUT"
    doctor_non_negative_config STALE_DAYS "$STALE_DAYS"; doctor_non_negative_config GRAPHQL_RESERVE "$GRAPHQL_RESERVE"; doctor_non_negative_config CORE_RESERVE "$CORE_RESERVE"
    doctor_positive_config RATE_LIMIT_CACHE_SECONDS "$RATE_LIMIT_CACHE_SECONDS"; doctor_positive_config API_RETRY_ATTEMPTS "$API_RETRY_ATTEMPTS"
    doctor_non_negative_config API_RETRY_BASE_SECONDS "$API_RETRY_BASE_SECONDS"
    doctor_positive_config MAX_ATTEMPTS "$MAX_ATTEMPTS"; doctor_non_negative_config RETRY_COOLDOWN_SECONDS "$RETRY_COOLDOWN_SECONDS"
    doctor_non_negative_config WORKER_TIMEOUT_SECONDS "$WORKER_TIMEOUT_SECONDS"
    if (( WORKER_TIMEOUT_SECONDS > 0 && WORKER_TIMEOUT_SECONDS < WORKER_TIMEOUT_MIN_SAFE_SECONDS )); then
      doctor_add warning '設定値: WORKER_TIMEOUT_SECONDS' '正常に進行中のworkerを誤って停止する恐れがあります。' "docs/operations/issue-queue.md を確認し、worker_timeout_seconds を ${WORKER_TIMEOUT_MIN_SAFE_SECONDS}秒以上、または実測の所要時間に基づく値へ見直してください。"
    fi
    doctor_enum_config UNKNOWN_SCOPE "$UNKNOWN_SCOPE" isolated exclusive open
    doctor_enum_config TRACEABILITY "$TRACEABILITY" require warn off
    if [[ $TRACEABILITY == require || $TRACEABILITY == warn || $TRACEABILITY == off ]]; then
      doctor_add success 'トレーサビリティgate' "現在のmodeは $TRACEABILITY です（require=完了をblock、warn=助言のみ、off=無効）。" '対応は不要です。'
    fi
    doctor_enum_config PREFLIGHT "$PREFLIGHT" require warn off
    if [[ $PREFLIGHT == require || $PREFLIGHT == warn || $PREFLIGHT == off ]]; then
      doctor_add success '変更影響とリスクのpreflight gate' "現在のmodeは $PREFLIGHT です（require=非自律verdictをblock、warn=リスクの高いverdictのみblock、off=無効）。" '対応は不要です。'
    fi
    (( DOCTOR_FAILURES == 0 )) && doctor_add success '設定ファイル' '設定値を安全に解釈できます。' '対応は不要です。'
  fi

  if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    doctor_add success 'Git repository' '対象repositoryを参照できます。' '対応は不要です。'
  else doctor_add failure 'Git repository' 'origin、branch、worktreeを診断できません。' 'gitを導入し、Git repository内で実行してください。'; fi
  if origin=$(git -C "$REPO_ROOT" remote get-url origin 2>/dev/null) && [[ -n $origin ]]; then
    doctor_add success 'origin' 'originが設定されています。' '対応は不要です。'
  else doctor_add failure 'origin' 'GitHub repositoryを特定できません。' 'git remote add origin <GitHub repository URL> を実行してください。'; fi

  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    doctor_add success 'GitHub認証' 'GitHub CLIの認証を確認しました（token本体は表示しません）。' '対応は不要です。'
    if [[ $(gh api "repos/$(repo_name)" --jq '.permissions.push // false' 2>/dev/null) == true ]]; then
      doctor_add success 'repository権限' '対象repositoryのREST APIをread/writeできます。' '対応は不要です。'
    else doctor_add failure 'repository権限' 'Issue、PR、Labelを更新できません。' 'gh auth refresh で対象repositoryのread/write権限を付与してください。'; fi
  else doctor_add failure 'GitHub認証' 'IssueキューとPR lifecycleを利用できません。' 'gh auth login を実行し、対象repositoryのread/write権限を付与してください。'; fi

  default_branch=$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null || true)
  if [[ -n $default_branch && $default_branch == main ]]; then doctor_add success 'default branch' 'default branchはmainです。' '対応は不要です。'
  elif [[ -n $default_branch ]]; then doctor_add warning 'default branch' "default branchがmainではありません（$default_branch）。main前提の同期機能は利用できません。" 'repositoryのdefault branchと運用方針を確認してください。'
  else doctor_add failure 'default branch' 'default branchを確認できません。' 'GitHubへのrepositoryアクセスとoriginを確認してください。'; fi

  local phase_provider
  for phase_provider in $(agent_used_providers); do
    if ! provider_valid "$phase_provider"; then doctor_add failure "AI CLI ($phase_provider)" "provider $phase_provider は未対応です。" 'agent.provider / agent.plan.provider / agent.exec.provider を codex、claude、opencode のいずれかに設定してください。'
    elif provider_ready "$phase_provider"; then doctor_add success "AI CLI ($phase_provider)" 'Issue workerを起動できます。' '対応は不要です。'
    else doctor_add failure "AI CLI ($phase_provider)" 'Issue workerを安全に起動できません。' "$phase_provider CLIを導入または更新し、ログインしてください。"; fi
  done

  # Tiers schema validation (Issue #155): unknown providers and invalid
  # max_usage_percent are failures; a tier with no models is a warning (it
  # silently contributes no candidates).
  local tier_phase tier_provider tier_max tier_i tier_empty=0
  for tier_phase in plan exec diagnose; do
    while IFS="$CAND_FS" read -r _ _ _ tier_provider _ _ tier_max; do
      [[ -n $tier_provider ]] || continue
      if ! provider_valid "$tier_provider"; then
        doctor_add failure "tiers設定 ($tier_phase)" "tierのprovider $tier_provider は未対応です。" 'agent.*.tiers[].provider を codex、claude、opencode のいずれかに設定してください。'
      fi
      if [[ -n $tier_max ]] && { ! [[ $tier_max =~ ^[0-9]+(\.[0-9]+)?$ ]] || ! awk -v m="$tier_max" 'BEGIN { exit !(m >= 0 && m <= 100) }'; }; then
        doctor_add failure "tiers設定 ($tier_phase)" "models[].max_usage_percent は0〜100の数値にしてください（現在: ${tier_max:-空}）。" 'agent.*.tiers[].models[].max_usage_percent を0〜100の数値に修正してください。'
      fi
    done < <(agent_phase_tiers "$tier_phase")
    if agent_phase_has_tiers "$tier_phase"; then
      tier_empty=0
      for tier_i in $(agent_tier_indices "$tier_phase"); do
        [[ -z $(agent_model_indices "$tier_phase" "$tier_i") ]] && tier_empty=1
      done
      if (( tier_empty )); then
        doctor_add warning "tiers設定 ($tier_phase)" 'models配列が空のtierがあります。そのtierは候補にならず実質無視されます。' 'agent.*.tiers[].models に少なくとも1つのmodelを指定してください。'
      fi
    fi
  done
  if agent_used_providers | grep -qx opencode; then
    local go_auth go_has=0
    go_auth="${XDG_DATA_HOME:-$HOME/.local/share}/opencode/auth.json"
    if [[ -r $go_auth ]] && [[ -n $(yq -p json -r '."opencode-go".key // ""' "$go_auth" 2>/dev/null) ]]; then go_has=1; fi
    if (( go_has )); then
      doctor_add success 'OpenCode Go usage' 'opencode-go のusage認証keyの存在を確認しました（値は表示しません）。' '対応は不要です。'
    else
      doctor_add warning 'OpenCode Go usage' 'opencode-go のusage API認証keyが見つかりません。枠枯渇の回復検知は固定cooldownにフォールバックします。' 'opencode CLI で opencode-go アカウントにログインしてください（key値はリポジトリへ保存しません）。'
    fi
  fi
  local pool
  for pool in $(agent_all_pools); do
    if agent_pool_marker_active "$pool"; then
      doctor_add warning "プール $pool" '枠枯渇中です。回復するまでこのプールは選択から外れます。' "cooldownまたはusage回復後に自動復帰します。緊急復帰させたい場合は \`.git/agentic-loop/pools/$pool/exhausted\` を削除してください。"
    fi
  done
  if command -v devbox >/dev/null 2>&1; then doctor_add success 'Devbox' '固定環境の共通検証入口を利用できます。' '対応は不要です。'
  else doctor_add failure 'Devbox' '`devbox run --pure check` を実行できません。' 'https://www.jetify.com/devbox/docs/installing_devbox/ を参照してDevboxを導入してください。'; fi

  local runtime_profile="$RUNTIME_ROOT/.devbox/nix/profile/default" runtime_dirs_ok=1 runtime_dir
  if [[ -r $RUNTIME_PATH_FILE ]]; then
    local -a runtime_dirs=()
    IFS=: read -r -a runtime_dirs < "$RUNTIME_PATH_FILE"
    for runtime_dir in "${runtime_dirs[@]}"; do [[ -d $runtime_dir ]] || runtime_dirs_ok=0; done
    if [[ -L $runtime_profile && ! -e $runtime_profile ]]; then runtime_dirs_ok=0; fi
    if (( runtime_dirs_ok )); then
      doctor_add success '固定runtime' 'runtime.pathの記録先と永続Devbox profileが健全です。' '対応は不要です。'
    else
      doctor_add failure '固定runtime' 'runtime.pathが指すtoolディレクトリまたは永続Devbox profileが失われています。yqなどの起動に失敗する恐れがあります。' 'bin/agentic-loop の任意コマンドを実行すると自己修復を試みます。改善しない場合は install.sh を再実行してください。'
    fi
    if command -v nix-store >/dev/null 2>&1; then
      local yq_bin="$runtime_profile/bin/yq" yq_store='' gc_protected=0
      if [[ -x $yq_bin ]] && yq_store=$(readlink -f "$yq_bin" 2>/dev/null) && [[ -n $yq_store ]] &&
        nix-store --query --roots "$yq_store" 2>/dev/null | grep -Fq "$runtime_profile"; then
        gc_protected=1
      fi
      if (( gc_protected )); then
        doctor_add success '固定runtimeのGC保護' 'yqの実体がnix-storeのgcrootとして保護されています。' '対応は不要です。'
      else
        doctor_add warning '固定runtimeのGC保護' 'nix-collect-garbage実行後にyqなどが失われる可能性があります。' 'install.sh を再実行し、永続Devbox profileを再生成してください。'
      fi
    fi
  else
    doctor_add warning '固定runtime' 'nix GCから保護された永続runtimeがまだ生成されていません。' 'install.sh を再実行してください。'
  fi

  hooks=$(git -C "$REPO_ROOT" config --get core.hooksPath 2>/dev/null || true)
  if [[ $hooks == .githooks && -x $REPO_ROOT/.githooks/pre-commit && -x $REPO_ROOT/.githooks/pre-push ]]; then doctor_add success 'Git hooks' 'secret guardとpush前検証が有効です。' '対応は不要です。'
  else doctor_add failure 'Git hooks' '秘密混入または未検証pushを防止できません。' 'git config core.hooksPath .githooks を実行し、install.shを再実行してください。'; fi

  if pid_alive; then doctor_add success 'Supervisor' 'Issueキューを処理中です。' '対応は不要です。'
  else doctor_add failure 'Supervisor' 'queued Issueが処理されません。' 'bin/agentic-loop start を実行してください。'; fi

  unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
    unit=$(supervisor_unit_name)
    if [[ -f $unit_dir/$unit ]]; then doctor_add success 'systemd Supervisor service' 'Supervisorの再起動設定があります。' '対応は不要です。'
    else doctor_add warning 'systemd Supervisor service' '予期しない終了後の自動復旧がありません。' 'bin/agentic-loop start を実行してください。'; fi
    if compgen -G "$unit_dir/agentic-loop-main-sync-*.timer" >/dev/null; then doctor_add success 'main同期timer' 'default branchの定期同期が設定されています。' '対応は不要です。'; else doctor_add warning 'main同期timer' 'local mainが自動追従しません。' 'install.shを再実行してください。'; fi
    if compgen -G "$unit_dir/agentic-loop-diagnosis-*.timer" >/dev/null; then doctor_add success '定期診断timer' 'コードベースの定期診断が設定されています。' '対応は不要です。'; else doctor_add warning '定期診断timer' '定期的なdrift診断が実行されません。' 'install.shを再実行してください。'; fi
  else doctor_add warning 'systemd user manager' '自動起動とtimerを利用できませんが、手動CLIは利用できます。' 'systemd user sessionを有効化するか、bin/agentic-loop startを手動実行してください。'; fi

  if [[ -r $STATE_ROOT/project.env ]]; then
    while IFS='=' read -r unit project_number; do case $unit in PROJECT_OWNER) project_owner=$project_number ;; PROJECT_NUMBER) : ;; esac; done < "$STATE_ROOT/project.env"
    project_number=$(sed -n 's/^PROJECT_NUMBER=//p' "$STATE_ROOT/project.env" | head -n 1)
    project_id=$(gh project view "$project_number" --owner "$project_owner" --format json --jq .id 2>/dev/null || true)
    repository=$(repo_name 2>/dev/null || true); owner=${repository%%/*}; name=${repository#*/}
    if [[ -n $project_id && $owner != "$repository" && -n $name ]]; then
      project_ok=$(gh api graphql -f query='query($project: ID!, $owner: String!, $name: String!) {
        node(id: $project) { ... on ProjectV2 {
          fields(first: 100) { nodes { ... on ProjectV2SingleSelectField { name options { name } } } }
          views(first: 100) { nodes { name filter } }
        } }
        repository(owner: $owner, name: $name) { projectsV2(first: 100) { nodes { id } } }
      }' -F project="$project_id" -f owner="$owner" -f name="$name" --jq '
        .data.node.id as $id |
        (.data.repository.projectsV2.nodes | any(.id == $id)) and
        (.data.node.fields.nodes | any(.name == "Agent status" and (([.options[].name] | sort) == (["Inbox","Queued","Running","Needs input","In review","Done","Failed","Stale","Blocked"] | sort)))) and
        (.data.node.views.nodes | any(.name == "Open Issues" and .filter == "is:issue is:open")) and
        (.data.node.views.nodes | any(.name == "Closed Issues" and .filter == "is:issue is:closed")) and
        (.data.node.views.nodes | any(.name == "Open PRs" and .filter == "is:pr is:open")) and
        (.data.node.views.nodes | any(.name == "Closed PRs" and .filter == "is:pr is:closed"))
      ' 2>/dev/null || true)
    fi
    if [[ $project_ok == true ]]; then
      doctor_add success 'GitHub Project同期' 'repository link、Agent status field、4つのView filterが同期しています。' '対応は不要です。'
    else doctor_add warning 'GitHub Project同期' '任意の可視化層を参照できません。Issue Labelキューは継続できます。' 'gh auth refresh -s project,read:project の後に bin/agentic-loop setup を実行してください。'; fi
  else doctor_add warning 'GitHub Project同期' '任意のProject設定がありません。Issue Labelキューは継続できます。' 'bin/agentic-loop setup を実行してください。'; fi

  issue_worktrees=$(git -C "$REPO_ROOT" worktree list --porcelain 2>/dev/null | awk -v root="$WORKTREE_ROOT/issue-" '$1 == "worktree" && index($2, root) == 1 {print $2}' | head -n 3)
  agent_branches=$(git -C "$REPO_ROOT" for-each-ref --format='%(refname:short)' refs/heads/agent/ 2>/dev/null | head -n 3)
  log_files=$(find "$STATE_ROOT/logs" -type f -size +0c -print -quit 2>/dev/null || true)
  if [[ -n $issue_worktrees || -n $agent_branches || -n $log_files ]]; then
    if pid_alive && doctor_residual_belongs_to_running "$issue_worktrees" "$agent_branches"; then
      doctor_add success '残存状態' 'worktree、agent branch、またはlogがありますが、いずれもGitHub上でagent:runningのIssueに対応しています。' '対応は不要です。bin/agentic-loop status でphaseを確認できます。'
    else
      doctor_add warning '残存状態' 'agent:runningに対応しないworktree、agent branch、またはlogが残っています。実行中のworker由来か確認が必要です。' 'bin/agentic-loop status でrunning Issueの有無を確認してください。対応するIssueが無ければ、そのIssueのagentic-loop:handoffコメント（無ければ本文と直近のcomment）を確認した上で、docs/operations/issue-queue.md のcleanup手順に従って復旧してください。'
    fi
  else doctor_add success '残存状態' '不要なworktree、agent branch、非空logは見つかりません。' '対応は不要です。'; fi

  local manifest_path="$REPO_ROOT/.agentic-loop/manifest.json" foundation_revision drift_count=0 mpath mhash
  if [[ -r $manifest_path ]] && command -v yq >/dev/null 2>&1; then
    while IFS=$'\t' read -r mpath mhash; do
      [[ -n $mpath && -f $REPO_ROOT/$mpath ]] || continue
      [[ "$(sha256sum "$REPO_ROOT/$mpath" | cut -d' ' -f1)" == "$mhash" ]] || drift_count=$((drift_count + 1))
    done < <(yq -p json -o tsv '.files[] | select(.class == "shared") | [.path, .sha256] | @tsv' "$manifest_path" 2>/dev/null)
    if (( drift_count > 0 )); then
      doctor_add warning 'Foundation管理fileの変更' "Foundationが管理する${drift_count}件のfileが導入時から変更されています。" 'bin/agentic-loop upgrade（既定はdry-run）で差分を確認してください。意図した変更であれば対応不要です。'
    else
      doctor_add success 'Foundation manifest' '導入版数・適用履歴・管理fileの整合性を確認できます。' '対応は不要です。'
    fi
  else
    doctor_add warning 'Foundation manifest' '導入版数を追跡できないため、初回のupgradeは全fileの競合として報告されます。' 'bin/agentic-loop upgrade を実行し、報告される内容を確認してください。'
  fi

  foundation_revision=$(config_value 'foundation.revision')
  if [[ -z $foundation_revision ]]; then
    doctor_add warning 'Foundation revision pin' '未設定です。upgradeは未検証のrevisionへ暗黙追従しません（意図した安全動作です）。' '.agentic-loop.toml の [foundation].revision を固定revisionに設定するか、upgrade実行時に --revision を指定してください。'
  else
    doctor_add success 'Foundation revision pin' 'upgradeは明示されたrevisionとのみ比較します。' '対応は不要です。'
  fi

  if [[ -e $STATE_ROOT/upgrade-last-apply.json ]]; then
    doctor_add failure '中断したupgrade' '直前のupgrade適用が検証を完了せず中断しています。' 'bin/agentic-loop upgrade --rollback を実行するか、原因を解消して bin/agentic-loop upgrade --apply を再実行してください。'
  else
    doctor_add success '中断したupgrade' '未完了のupgrade適用はありません。' '対応は不要です。'
  fi

  local cap_rc=0 cap_index cap_code
  capability_validate "$REPO_ROOT" "$(config_value 'foundation.verify_command')" "$WORKER_TIMEOUT_SECONDS" || cap_rc=1
  for cap_index in "${!CAPABILITY_LEVELS[@]}"; do
    cap_code=${CAPABILITY_CODES[$cap_index]}
    case $cap_code in
      not-installed) doctor_add warning '能力manifest' "${CAPABILITY_MESSAGES[$cap_index]}" 'bin/agentic-loop upgrade を実行するか install.sh を再実行してください（既存repositoryでは検出可能な項目だけが安全に初期化されます）。' ;;
      unreadable | schema-version) doctor_add failure '能力manifest' "${CAPABILITY_MESSAGES[$cap_index]}" '.agentic-loop/capabilities.toml のTOML構文・schema_version・yqの利用可否を確認してください。' ;;
      unsafe-command:* | unsafe-path:*) doctor_add failure '能力manifest: 安全性検証' "${CAPABILITY_MESSAGES[$cap_index]}" '.agentic-loop/capabilities.toml の該当する宣言を、repository相対の安全なpathまたはcommand形式に修正してください。' ;;
      missing-path:*) doctor_add warning '能力manifest: 参照切れ' "${CAPABILITY_MESSAGES[$cap_index]}" '.agentic-loop/capabilities.toml の該当pathを、実在するpathに更新するか宣言を削除してください。' ;;
      undetermined) doctor_add warning '能力manifest: 未確定項目' "${CAPABILITY_MESSAGES[$cap_index]}" '検出できる情報が増え次第、該当キーを .agentic-loop/capabilities.toml へ明示してください（推測での確定は避けてください）。' ;;
      drift:*) doctor_add warning '能力manifest: drift' "${CAPABILITY_MESSAGES[$cap_index]}" '.agentic-loop.toml の [foundation].verify_command と .agentic-loop/capabilities.toml の validation.full_check を一致させてください。' ;;
      *) doctor_add warning '能力manifest' "${CAPABILITY_MESSAGES[$cap_index]}" '.agentic-loop/capabilities.toml を確認してください。' ;;
    esac
  done
  if (( cap_rc == 0 )) && [[ -n $CAPABILITY_JSON ]]; then
    doctor_add success '能力manifest' '検証済みのrepository capability manifestを利用できます。' '対応は不要です。'
  fi
  if [[ -n $(git -C "$REPO_ROOT" status --porcelain -- .agentic-loop/capabilities.toml 2>/dev/null) ]]; then
    doctor_add warning '能力manifest: 未commit' 'install/upgradeが生成・更新した .agentic-loop/capabilities.toml が未commitです。' '内容を確認し、意図した宣言であればcommitしてください。'
  fi

  local flaky_rc=0 flaky_index flaky_code
  flaky_registry_validate "$(flaky_registry_file "$REPO_ROOT")" "$REPO_ROOT/.agentic-loop/guard-secrets.sh" || flaky_rc=1
  for flaky_index in "${!FLAKY_LEVELS[@]}"; do
    flaky_code=${FLAKY_CODES[$flaky_index]}
    case $flaky_code in
      not-installed) doctor_add warning 'flaky test registry' "${FLAKY_MESSAGES[$flaky_index]}" 'tests/flaky-registry.toml を追加するか bin/agentic-loop upgrade を実行してください。' ;;
      expiring-soon) doctor_add warning 'flaky test registry: 期限間近' "${FLAKY_MESSAGES[$flaky_index]}" 'docs/operations/flaky-tests.md の手順で延長するか、修復を完了させてentryを削除してください。' ;;
      *) doctor_add failure 'flaky test registry' "${FLAKY_MESSAGES[$flaky_index]}" 'docs/operations/flaky-tests.md を確認し、tests/flaky-registry.toml を修正してください。' ;;
    esac
  done
  # doctor_collect is invoked unguarded (see cmd_doctor), so its final
  # statement must always exit 0 regardless of flaky_rc; a bare
  # `(( flaky_rc == 0 )) && ...` would make doctor_collect itself return 1
  # whenever a flaky-registry failure exists, silently aborting cmd_doctor
  # under set -e before it ever prints a report.
  if (( flaky_rc == 0 )); then
    doctor_add success 'flaky test registry' '隔離entryは検証済み、または宣言がありません。' '対応は不要です。'
  fi
}


cmd_doctor() {
  local format=text index separator=''
  [[ $# -eq 1 ]] || { [[ $# -eq 3 && $2 == --format && $3 == json ]] || { usage; return 2; }; format=json; }
  declare -ga DOCTOR_LEVELS=() DOCTOR_NAMES=() DOCTOR_IMPACTS=() DOCTOR_RECOVERIES=()
  declare -g DOCTOR_WARNINGS=0 DOCTOR_FAILURES=0
  doctor_collect
  if [[ $format == json ]]; then
    printf '{"schema_version":1,"summary":{"success":%d,"warning":%d,"failure":%d},"checks":[' "$(( ${#DOCTOR_LEVELS[@]} - DOCTOR_WARNINGS - DOCTOR_FAILURES ))" "$DOCTOR_WARNINGS" "$DOCTOR_FAILURES"
    for index in "${!DOCTOR_LEVELS[@]}"; do
      printf '%s{"level":"%s","name":"%s","impact":"%s","recovery":"%s"}' "$separator" "${DOCTOR_LEVELS[$index]}" "$(json_escape "${DOCTOR_NAMES[$index]}")" "$(json_escape "${DOCTOR_IMPACTS[$index]}")" "$(json_escape "${DOCTOR_RECOVERIES[$index]}")"
      separator=,
    done
    printf ']}\n'
  else
    for index in "${!DOCTOR_LEVELS[@]}"; do
      case ${DOCTOR_LEVELS[$index]} in success) unit='成功' ;; warning) unit='警告' ;; failure) unit='失敗' ;; esac
      printf '[%s] %s\n  影響: %s\n  復旧: %s\n' "$unit" "${DOCTOR_NAMES[$index]}" "${DOCTOR_IMPACTS[$index]}" "${DOCTOR_RECOVERIES[$index]}"
    done
    printf '集計: 成功=%d 警告=%d 失敗=%d\n' "$(( ${#DOCTOR_LEVELS[@]} - DOCTOR_WARNINGS - DOCTOR_FAILURES ))" "$DOCTOR_WARNINGS" "$DOCTOR_FAILURES"
  fi
  (( DOCTOR_FAILURES == 0 ))
}
