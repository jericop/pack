#!/bin/bash
# PLATFORM-1802-retrigger-builds.sh
#
# Push an empty (no-op) commit to each PLATFORM-1802 app branch to re-trigger its
# Jenkins multibranch build, for BOTH strategies (multi-agent + buildkit-emulation).
# After running, use PLATFORM-1802-grafana-build-summary.sh to check results.
#
# Usage:
#   bash PLATFORM-1802-retrigger-builds.sh                 # both strategies, all apps
#   bash PLATFORM-1802-retrigger-builds.sh emulation       # only buildkit-emulation branches
#   bash PLATFORM-1802-retrigger-builds.sh multi-agent     # only multi-agent branches
#   bash PLATFORM-1802-retrigger-builds.sh emulation go java   # only named apps (substring match)
#
# Notes:
# - Pushes an EMPTY commit (git commit --allow-empty) so no file changes are made.
# - Logs to ~/ai-development/command-logs (persistent across restarts).
set -o pipefail

# Each sample app is a container of per-branch worktrees at ~/.repos/r7/<app>/<branch>/
# (converted from the old flat _pl1662_worktrees layout).
REPOS_DIR="/Users/jpena/.repos/r7"
# Per-script log folder, one file per invocation (N.log).
LOGDIR="${HOME}/ai-development/command-logs/$(basename "$0")"
mkdir -p "$LOGDIR"
n=1; while [ -e "${LOGDIR}/${n}.log" ]; do n=$((n+1)); done
log="${LOGDIR}/${n}.log"

EM_BRANCH="PLATFORM-1802-buildkit-emulation"
MA_BRANCH="PLATFORM-1802-multi-agent"

# The participating apps (4).
APPS=(pd-sample-go-app pd-sample-java-app pd-sample-nodejs-app pd-sample-python-app)

# ---- parse args: first non-app token is a strategy filter; remaining are app filters
STRATEGY="both"
APP_FILTERS=()
for a in "$@"; do
  case "$a" in
    emulation|buildkit-emulation) STRATEGY="emulation" ;;
    multi-agent|multiagent|native) STRATEGY="multi-agent" ;;
    both) STRATEGY="both" ;;
    *) APP_FILTERS+=("$a") ;;
  esac
done

app_matches() {  # $1 = app name; true if no filters or matches any filter substring
  [ ${#APP_FILTERS[@]} -eq 0 ] && return 0
  local app="$1" f
  for f in "${APP_FILTERS[@]}"; do [[ "$app" == *"$f"* ]] && return 0; done
  return 1
}

branches_for_strategy() {
  case "$STRATEGY" in
    emulation)   echo "$EM_BRANCH" ;;
    multi-agent) echo "$MA_BRANCH" ;;
    both)        echo "$EM_BRANCH $MA_BRANCH" ;;
  esac
}

push_one() {  # $1 = app, $2 = branch
  local app="$1" branch="$2" suffix
  case "$branch" in
    "$EM_BRANCH") suffix="buildkit-emulation" ;;
    "$MA_BRANCH") suffix="multi-agent" ;;
  esac
  # New layout: worktree dir is named after the branch, under the app container dir.
  local wt="${REPOS_DIR}/${app}/${branch}"
  if [ ! -d "$wt" ]; then echo "SKIP  ${app} [${suffix}]: worktree not found ($wt)"; return; fi
  local cur; cur="$(git -C "$wt" rev-parse --abbrev-ref HEAD 2>/dev/null)"
  if [ "$cur" != "$branch" ]; then
    echo "SKIP  ${app} [${suffix}]: worktree on '$cur', expected '$branch'"; return
  fi
  local msg="PLATFORM-1802: re-trigger ${suffix} build (no-op $(date -u +%FT%TZ))"
  if ! git -C "$wt" commit --allow-empty -q -m "$msg"; then
    echo "FAIL  ${app} [${suffix}]: empty commit failed"; return
  fi
  if git -C "$wt" push -q origin "$branch" 2>>"$log"; then
    local sha; sha="$(git -C "$wt" rev-parse --short HEAD)"
    echo "OK    ${app} [${suffix}]: pushed ${sha} -> ${branch}"
  else
    echo "FAIL  ${app} [${suffix}]: push failed (see $log)"
  fi
}

{
  echo "PLATFORM-1802 re-trigger  ($(date -u +%FT%TZ))"
  echo "strategy=${STRATEGY}  app_filters=[${APP_FILTERS[*]:-<all>}]"
  echo "----------------------------------------------------------------"
  for app in "${APPS[@]}"; do
    app_matches "$app" || continue
    for br in $(branches_for_strategy); do
      push_one "$app" "$br"
    done
  done
  echo "----------------------------------------------------------------"
  echo "Done. Check results with: bash PLATFORM-1802-grafana-build-summary.sh"
  echo "(Jenkins multibranch scan can take 1-3 min to notice the push and start the build.)"
} 2>&1 | tee "$log"

echo "LOG=$log"
