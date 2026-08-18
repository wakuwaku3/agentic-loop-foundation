# Module: capability.sh. Sourced by bin/agentic-loop (see docs/decisions/
# 0013-agentic-loop-modules.md). Unlike the other modules here, every function
# in this file takes an explicit repo_root argument and depends on nothing but
# yq, so scripts/install-target.sh, scripts/upgrade/migrations/0003-capability-
# manifest.sh, and scripts/lint.sh can `source` it standalone, outside
# bin/agentic-loop's global state. See docs/decisions/0018-repository-
# capability-manifest.md and docs/operations/capability-manifest.md.
# shellcheck shell=bash
# shellcheck disable=SC2016,SC2155,SC2034,SC2153

# Highest capabilities.toml schema_version this Foundation revision
# understands. An unknown (higher) or missing schema_version fails closed
# rather than silently falling back to defaults (see ADR 0018).
readonly CAPABILITY_SCHEMA_SUPPORTED=1

capability_file() { printf '%s/.agentic-loop/capabilities.toml' "$1"; }


# Populate CAPABILITY_JSON from a repository's capabilities.toml.
# Return codes: 0 = loaded, 2 = file does not exist (the manifest is
# optional), 1 = present but unreadable, unparsable, or yq is missing.
capability_load() {
  local repo_root=$1 file
  file=$(capability_file "$repo_root")
  CAPABILITY_JSON=''
  [[ -e $file ]] || return 2
  [[ -r $file ]] || return 1
  command -v yq >/dev/null 2>&1 || return 1
  CAPABILITY_JSON=$(yq -p toml -o json "$file" 2>/dev/null) || return 1
  [[ -n $CAPABILITY_JSON ]] || return 1
  return 0
}


capability_query() { yq -p json -r "$1" <<< "$CAPABILITY_JSON" 2>/dev/null; }


# --- safety validation -------------------------------------------------------
# A declared path must be a plain repository-relative path: only safe
# characters, never absolute, never containing a ".." segment, and (once
# resolved against repo_root) must stay inside the repository even through a
# symlink. This is the only thing standing between an untrusted manifest and
# command injection / workspace-escaping writes (see ADR 0018).
capability_path_safe() {
  local repo_root=$1 path=$2 resolved seg
  [[ -n $path ]] || return 1
  [[ $path =~ ^[A-Za-z0-9._/-]+$ ]] || return 1
  [[ $path != /* ]] || return 1
  IFS='/' read -ra seg <<< "$path"
  for seg in "${seg[@]}"; do [[ $seg != '..' ]] || return 1; done
  resolved=$(cd "$repo_root" 2>/dev/null && realpath -m -- "$path" 2>/dev/null) || return 1
  case $resolved in
    "$repo_root" | "$repo_root"/*) return 0 ;;
    *) return 1 ;;
  esac
}


# A declared command must contain no shell metacharacters (it is only ever
# tokenized, never eval'd or interpolated into a shell) and its first token
# (the executable) must be one of a small allowlist: a known wrapper (devbox,
# make, bin/agentic-loop) or an executable file inside the repository. No
# manifest value is ever executed by this Foundation itself (see ADR 0018);
# this check exists so consumers that choose to run it (a human, a future
# feature) inherit the same safety guarantee.
capability_command_safe() {
  local repo_root=$1 command=$2 first
  [[ -n $command ]] || return 1
  case $command in
    *';'* | *'|'* | *'&'* | *'$'* | *'`'* | *'('* | *')'* | *'<'* | *'>'* | *'*'* | *'?'* | *$'\n'* | *$'\r'*) return 1 ;;
  esac
  first=${command%% *}
  case $first in
    devbox | make) return 0 ;;
    bin/agentic-loop) return 0 ;;
    */*) [[ -x $repo_root/$first ]] && return 0 ;;
  esac
  return 1
}


capability_add() {
  local level=$1 code=$2 message=$3
  CAPABILITY_LEVELS+=("$level"); CAPABILITY_CODES+=("$code"); CAPABILITY_MESSAGES+=("$message")
  case $level in warning) CAPABILITY_WARNINGS=$((CAPABILITY_WARNINGS + 1)) ;; failure) CAPABILITY_FAILURES=$((CAPABILITY_FAILURES + 1)) ;; esac
}


# Validate every path/command declaration in a loaded manifest, plus
# schema_version and the undetermined list. Populates CAPABILITY_LEVELS/
# CODES/MESSAGES (parallel arrays, mirroring doctor.sh's doctor_add) and
# returns 1 iff at least one entry is level=failure. A manifest that does not
# exist is not a failure (the manifest is optional); a present-but-broken one
# always is. expected_verify_command and worker_timeout_seconds, when given,
# are compared against validation.full_check / full_check_seconds and
# reported as warning-level drift on mismatch (see docs/decisions/0018,
# section on drift detection); callers without access to that configuration
# (scripts/install-target.sh, scripts/lint.sh) may omit them.
capability_validate() {
  local repo_root=$1 expected_verify_command=${2:-} worker_timeout_seconds=${3:-} rc=0
  declare -ga CAPABILITY_LEVELS=() CAPABILITY_CODES=() CAPABILITY_MESSAGES=()
  declare -g CAPABILITY_WARNINGS=0 CAPABILITY_FAILURES=0
  capability_load "$repo_root"; rc=$?
  if (( rc == 2 )); then
    capability_add warning not-installed 'capabilities.toml が存在しません（任意の宣言です）。'
    return 0
  elif (( rc != 0 )); then
    capability_add failure unreadable 'capabilities.toml を解釈できません（TOML構文が不正か、yqが利用できません）。'
    return 1
  fi

  local schema_version
  schema_version=$(capability_query '.schema_version // ""')
  if [[ ! $schema_version =~ ^[0-9]+$ ]]; then
    capability_add failure schema-version 'schema_versionが数値として宣言されていません。'
    return 1
  elif (( schema_version > CAPABILITY_SCHEMA_SUPPORTED )); then
    capability_add failure schema-version "schema_version=$schema_version は未対応です（このFoundation revisionの対応上限: $CAPABILITY_SCHEMA_SUPPORTED）。既定値へは暗黙fallbackしません。"
    return 1
  fi

  local key value
  for key in validation.full_check validation.fast_check validation.secret_guard validation.affected_check validation.impact_map; do
    value=$(capability_query ".$key // \"\"")
    [[ -n $value ]] || continue
    case $key in
      validation.full_check | validation.affected_check)
        capability_command_safe "$repo_root" "$value" || capability_add failure "unsafe-command:$key" "$key の値が安全なcommand形式ではありません: $value" ;;
      *)
        capability_path_safe "$repo_root" "$value" || capability_add failure "unsafe-path:$key" "$key の値が安全なpathではありません: $value"
        [[ $key != validation.impact_map || -e $repo_root/$value ]] || capability_add warning "missing-path:$key" "$key が実在しません: $value" ;;
    esac
  done

  local path
  while IFS= read -r path; do
    [[ -n $path ]] || continue
    capability_path_safe "$repo_root" "$path" || capability_add failure 'unsafe-path:environment.definition' "environment.definition の値が安全なpathではありません: $path"
  done < <(capability_query '.environment.definition[]?')

  while IFS= read -r path; do
    [[ -n $path ]] || continue
    capability_path_safe "$repo_root" "$path" || capability_add failure 'unsafe-path:ownership' "ownership[].path の値が安全なpathではありません: $path"
    [[ -e $repo_root/$path ]] || capability_add warning 'missing-path:ownership' "ownership[].path が実在しません: $path"
  done < <(capability_query '.ownership[]?.path')

  while IFS= read -r path; do
    [[ -n $path ]] || continue
    capability_path_safe "$repo_root" "$path" || capability_add failure 'unsafe-path:protected' "protected[].path の値が安全なpathではありません: $path"
    [[ -e $repo_root/$path ]] || capability_add warning 'missing-path:protected' "protected[].path が実在しません: $path"
  done < <(capability_query '.protected[]?.path')

  local command
  while IFS= read -r command; do
    [[ -n $command ]] || continue
    capability_command_safe "$repo_root" "$command" || capability_add failure 'unsafe-command:external_environment' "external_environment[].apply の値が安全なcommand形式ではありません: $command"
  done < <(capability_query '.external_environment[]?.apply')

  local undetermined_count
  undetermined_count=$(capability_query '.undetermined | length') || undetermined_count=0
  [[ $undetermined_count =~ ^[0-9]+$ ]] || undetermined_count=0
  if (( undetermined_count > 0 )); then
    local undetermined_list
    undetermined_list=$(capability_query '.undetermined[]?' | paste -sd', ' -)
    capability_add warning undetermined "未確定として宣言された項目があります（推測で確定していません）: $undetermined_list"
  fi

  if [[ -n $expected_verify_command ]]; then
    local full_check
    full_check=$(capability_query '.validation.full_check // ""')
    if [[ -n $full_check && $full_check != "$expected_verify_command" ]]; then
      capability_add warning 'drift:verify_command' "validation.full_check（$full_check）が .agentic-loop.toml の [foundation].verify_command（$expected_verify_command）と一致していません。"
    fi
  fi

  local full_check_seconds
  full_check_seconds=$(capability_query '.validation.full_check_seconds // 0')
  [[ $full_check_seconds =~ ^[0-9]+$ ]] || full_check_seconds=0

  if [[ $worker_timeout_seconds =~ ^[0-9]+$ ]] && (( worker_timeout_seconds > 0 )); then
    if (( full_check_seconds > 0 && full_check_seconds >= worker_timeout_seconds )); then
      capability_add warning 'drift:worker_timeout' "validation.full_check_seconds（${full_check_seconds}秒）が queue.worker_timeout_seconds（${worker_timeout_seconds}秒）以上です。完全検証がworker timeoutより長くなる矛盾があります。"
    fi
  fi

  local affected_check_seconds
  affected_check_seconds=$(capability_query '.validation.affected_check_seconds // 0')
  [[ $affected_check_seconds =~ ^[0-9]+$ ]] || affected_check_seconds=0
  if (( affected_check_seconds > 0 && full_check_seconds > 0 && affected_check_seconds >= full_check_seconds )); then
    capability_add warning 'drift:affected_check_seconds' "validation.affected_check_seconds（${affected_check_seconds}秒）が validation.full_check_seconds（${full_check_seconds}秒）以上です。短時間検証が完全検証より遅くなる矛盾があります。"
  fi

  (( CAPABILITY_FAILURES == 0 ))
}


# Render the last capability_validate result as the --format json body for
# `bin/agentic-loop capabilities`. CAPABILITY_JSON (already-valid JSON text)
# is spliced in verbatim; every finding is escaped through json_escape.
capability_render_json() {
  local index sep=''
  printf '{"schema_version":1,"installed":%s,"valid":%s,"summary":{"failure":%d,"warning":%d},"findings":[' \
    "$([[ -n $CAPABILITY_JSON ]] && printf true || printf false)" \
    "$([[ $CAPABILITY_FAILURES -eq 0 ]] && printf true || printf false)" \
    "$CAPABILITY_FAILURES" "$CAPABILITY_WARNINGS"
  for index in "${!CAPABILITY_LEVELS[@]}"; do
    printf '%s{"level":"%s","code":"%s","message":"%s"}' "$sep" "${CAPABILITY_LEVELS[$index]}" "$(json_escape "${CAPABILITY_CODES[$index]}")" "$(json_escape "${CAPABILITY_MESSAGES[$index]}")"
    sep=','
  done
  printf '],"data":%s}\n' "${CAPABILITY_JSON:-null}"
}


capability_render_text() {
  local index unit
  if [[ -z $CAPABILITY_JSON ]]; then
    printf 'capabilities.toml は存在しません（任意の宣言です）。\n'
  else
    printf 'schema_version: %s\n' "$(capability_query '.schema_version // ""')"
    printf '全検証: %s\n' "$(capability_query '.validation.full_check // "(未宣言)"')"
    printf '短時間検証: %s\n' "$(capability_query '.validation.fast_check // "(未宣言)"')"
    printf 'secret guard: %s\n' "$(capability_query '.validation.secret_guard // "(未宣言)"')"
    printf '影響範囲検証（gateではない）: %s\n' "$(capability_query '.validation.affected_check // "(未宣言)"')"
  fi
  for index in "${!CAPABILITY_LEVELS[@]}"; do
    case ${CAPABILITY_LEVELS[$index]} in warning) unit='警告' ;; failure) unit='失敗' ;; *) unit=${CAPABILITY_LEVELS[$index]} ;; esac
    printf '[%s] %s\n' "$unit" "${CAPABILITY_MESSAGES[$index]}"
  done
  printf '集計: 警告=%d 失敗=%d\n' "$CAPABILITY_WARNINGS" "$CAPABILITY_FAILURES"
}


# A short, worker-prompt-ready summary of the verified capability manifest:
# the common validation entry points and anything explicitly undetermined
# (which the worker must not guess). Empty (no injection) whenever the
# manifest is absent or fails validation, so a broken/missing manifest can
# never corrupt a prompt or silently degrade worker behavior.
capability_summary_block() {
  local repo_root=$1 rc=0 full_check fast_check secret_guard undetermined_list
  capability_validate "$repo_root" >/dev/null 2>&1 || rc=1
  [[ -n $CAPABILITY_JSON ]] || return 0
  (( rc == 0 )) || return 0
  full_check=$(capability_query '.validation.full_check // ""')
  fast_check=$(capability_query '.validation.fast_check // ""')
  secret_guard=$(capability_query '.validation.secret_guard // ""')
  undetermined_list=$(capability_query '.undetermined[]?' | paste -sd', ' -)
  [[ -n $full_check || -n $fast_check || -n $secret_guard || -n $undetermined_list ]] || return 0
  printf -- '--- repository capability manifest（.agentic-loop/capabilities.toml、検証済み） ---\n'
  [[ -n $full_check ]] && printf '全検証コマンド: %s\n' "$full_check"
  [[ -n $fast_check ]] && printf '短時間検証: %s\n' "$fast_check"
  [[ -n $secret_guard ]] && printf 'secret guard: %s\n' "$secret_guard"
  [[ -n $undetermined_list ]] && printf '未確定（推測で確定しないこと）: %s\n' "$undetermined_list"
  printf -- '--- ここまで ---\n'
}


cmd_capabilities() {
  local format=text rc=0
  [[ $# -eq 1 ]] || { [[ $# -eq 3 && $2 == --format && $3 == json ]] || { usage; return 2; }; format=json; }
  capability_validate "$REPO_ROOT" "$(config_value 'foundation.verify_command')" "$WORKER_TIMEOUT_SECONDS" || rc=1
  if [[ $format == json ]]; then capability_render_json; else capability_render_text; fi
  return $rc
}


# --- safe, detection-only generation for install/upgrade ---------------------
# Seed .agentic-loop/capabilities.toml for a repository that does not already
# have one, from a small, explicit detection table (see docs/operations/
# capability-manifest.md). Anything not positively detected is listed under
# undetermined rather than guessed. A no-op (exit 0) if the file already
# exists, so both scripts/install-target.sh and scripts/upgrade/migrations/
# 0003-capability-manifest.sh can call this unconditionally and idempotently.
capability_generate() {
  local repo_root=$1 file full_check='' fast_check='' secret_guard='' affected_check='' impact_map=''
  local -a undetermined=() env_defs=() skills=()
  file=$(capability_file "$repo_root")
  [[ -e $file ]] && return 0

  if [[ -r $repo_root/devbox.json ]] && command -v yq >/dev/null 2>&1 && [[ -n $(yq -p json -r '.shell.scripts.check // ""' "$repo_root/devbox.json" 2>/dev/null) ]]; then
    full_check='devbox run --pure check'
  elif [[ -r $repo_root/Makefile ]] && grep -Eq '^check:' "$repo_root/Makefile"; then
    full_check='make check'
  else
    undetermined+=(validation.full_check)
  fi
  if [[ -x $repo_root/.githooks/pre-commit ]]; then fast_check='.githooks/pre-commit'; else undetermined+=(validation.fast_check); fi
  if [[ -x $repo_root/.agentic-loop/guard-secrets.sh ]]; then secret_guard='.agentic-loop/guard-secrets.sh'; else undetermined+=(validation.secret_guard); fi
  if [[ -r $repo_root/Makefile ]] && grep -Eq '^affected:' "$repo_root/Makefile" && [[ -f $repo_root/tests/impact-map.toml ]]; then
    affected_check='devbox run --pure affected'
    impact_map='tests/impact-map.toml'
  else
    undetermined+=(validation.affected_check validation.impact_map)
  fi

  [[ -f $repo_root/devbox.json ]] && env_defs+=(devbox.json)
  [[ -f $repo_root/devbox.lock ]] && env_defs+=(devbox.lock)
  (( ${#env_defs[@]} == 0 )) && undetermined+=(environment.definition)
  # Which platforms a pinned toolchain actually runs on is never guessed from
  # file presence; it always requires a positive, out-of-band statement.
  undetermined+=(environment.platforms)
  undetermined+=(release.deploy release.distribution ownership protected external_environment validation.full_check_seconds validation.affected_check_seconds)

  local skill_manifest
  for skill_manifest in "$repo_root"/.claude/skills/*/SKILL.md; do
    [[ -f $skill_manifest ]] || continue
    skills+=("$(basename "$(dirname "$skill_manifest")")")
  done

  mkdir -p "$repo_root/.agentic-loop"
  {
    printf '# repository capability manifest (generated by scripts/install-target.sh from\n'
    printf '# a small detection table; see docs/operations/capability-manifest.md). Values\n'
    printf '# below were positively detected in this repository at install time and are\n'
    printf '# never guessed: anything undetected is listed under undetermined instead of a\n'
    printf '# fabricated default. This file is target-owned: `agentic-loop upgrade` never\n'
    printf '# overwrites or removes it.\n'
    printf 'schema_version = 1\n'
    printf 'undetermined = ['
    local sep='' item
    for item in "${undetermined[@]}"; do printf '%s"%s"' "$sep" "$item"; sep=', '; done
    printf ']\n\n'
    printf '[validation]\n'
    [[ -n $full_check ]] && printf 'full_check = "%s"\n' "$full_check"
    [[ -n $fast_check ]] && printf 'fast_check = "%s"\n' "$fast_check"
    [[ -n $secret_guard ]] && printf 'secret_guard = "%s"\n' "$secret_guard"
    [[ -n $affected_check ]] && printf 'affected_check = "%s"\n' "$affected_check"
    [[ -n $impact_map ]] && printf 'impact_map = "%s"\n' "$impact_map"
    printf '\n[environment]\n'
    if (( ${#env_defs[@]} > 0 )); then
      printf 'definition = ['
      sep=''
      for item in "${env_defs[@]}"; do printf '%s"%s"' "$sep" "$item"; sep=', '; done
      printf ']\n'
    fi
    printf '\n[skills]\n'
    printf 'available = ['
    sep=''
    for item in "${skills[@]}"; do printf '%s"%s"' "$sep" "$item"; sep=', '; done
    printf ']\n'
  } > "$file"
}
