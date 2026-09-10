#!/bin/bash
# PLATFORM-1802-grafana-build-summary.sh
#
# Self-service build-status/perf summary for the PLATFORM-1802 multi-arch comparison,
# pulled from Grafana Tempo via the Rapid7 MCP gateway (no Kiro session needed).
#
# For each app x strategy (multi-agent, buildkit-emulation) it finds, by default, the most
# recent SUCCESSFUL BUILD job trace in the window (ignoring running and failed jobs) and
# reports result + duration + URL from the root BUILD span's ci.pipeline.run.* attributes.
# This makes the output safe to paste into a ticket comment even while builds are running.
#
# Usage:
#   bash PLATFORM-1802-grafana-build-summary.sh              # last 6h, all apps, last SUCCESS
#   HOURS=24 bash PLATFORM-1802-grafana-build-summary.sh     # widen the window
#   bash PLATFORM-1802-grafana-build-summary.sh go java      # only named apps (substring)
#   RESULT_FILTER=ANY bash PLATFORM-1802-grafana-build-summary.sh   # latest of ANY result
#                                                             # (SUCCESS/FAILURE/ABORTED/RUNNING)
#   RAW=1 bash PLATFORM-1802-grafana-build-summary.sh         # also print traceIDs
#
# Modes:
#   RESULT_FILTER=SUCCESS (default) — newest trace whose run.result==SUCCESS; running/failed
#                                     builds are skipped. Best for posting to the ticket.
#   RESULT_FILTER=ANY               — newest trace regardless of result; RUNNING is shown for
#                                     builds with no terminal result yet (for live monitoring).
#
# Requires: r7aac (Okta token helper), curl, jq. If you get NO TOKEN, run: r7aac login
#
# Auth/transport proven working: POST JSON-RPC tools/call to the grafana-viewer gateway
# with a bearer token; the Grafana proxy response is wrapped in result.content[0].text.
set -o pipefail

GW="https://mcpgateway.acm.r7ops.com/servers/9562b4e7a97946bd94499fc22130b82e/mcp"
DS="kubernetes-traces"                 # Tempo datasource UID
# Per-script log folder, one file per invocation (N.log).
LOGDIR="${HOME}/ai-development/command-logs/$(basename "$0")"
mkdir -p "$LOGDIR"
n=1; while [ -e "${LOGDIR}/${n}.log" ]; do n=$((n+1)); done
log="${LOGDIR}/${n}.log"

HOURS="${HOURS:-6}"
now=$(date +%s); start=$(( now - HOURS*3600 ))
RESULT_FILTER="${RESULT_FILTER:-SUCCESS}"   # SUCCESS (default) or ANY

# APPS is overridable via the APPS env var (space-separated) so callers can track other
# repos (e.g. agent-patcher-service, pd-rds-postgres-password-lambda). Default = the 4 core
# sample apps.
if [ -n "${APPS:-}" ]; then read -r -a APPS <<< "${APPS}"; else APPS=(pd-sample-go-app pd-sample-java-app pd-sample-nodejs-app pd-sample-python-app); fi
STRATS=(multi-agent buildkit-emulation)

# ---- optional app-name filters (substring)
FILTERS=("$@")
app_matches() {
  [ ${#FILTERS[@]} -eq 0 ] && return 0
  local app="$1" f; for f in "${FILTERS[@]}"; do [[ "$app" == *"$f"* ]] && return 0; done; return 1
}

TOK="$(r7aac token --client-id mcpgateway 2>/dev/null)"
if [ -z "$TOK" ]; then echo "NO TOKEN — run: r7aac login" | tee "$log"; exit 3; fi

# gw_get <grafana-proxy-endpoint> -> prints the inner Grafana JSON (unwrapped) to stdout
gw_get() {
  local ep="$1" payload
  payload=$(jq -n --arg ep "$ep" '{jsonrpc:"2.0",id:1,method:"tools/call",
    params:{name:"grafana-ro-engineering-grafana-api-request",
            arguments:{method:"GET",endpoint:$ep}}}')
  curl -sS -X POST "$GW" \
    -H "Authorization: Bearer ${TOK}" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -d "$payload" 2>/dev/null \
  | jq -r '.result.content[0].text // empty'
}

urlenc() { printf '%s' "$1" | jq -sRr @uri; }

# list_recent_traces <app> <strategy> -> lines of "startNs<TAB>durationMs<TAB>traceID",
# newest first (up to 20 in the window). durationMs is trace-level from the search response.
list_recent_traces() {
  local app="$1" strat="$2"
  local q="{ span.ci.pipeline.id =~ \"${app}.*PLATFORM-1802-${strat}\" && span.type = \"job\" }"
  local ep="/api/datasources/proxy/uid/${DS}/api/search?q=$(urlenc "$q")&start=${start}&end=${now}&limit=20"
  gw_get "$ep" | jq -r '
    ((.data.traces // .traces) // [])
    | sort_by(.startTimeUnixNano | tonumber) | reverse
    | .[]
    | "\(.startTimeUnixNano)\t\(.durationMs // "")\t\(.traceID)"'
}

# hydrate_result <traceID> -> "result<TAB>url" from the root BUILD span attributes.
# A build still RUNNING has no terminal ci.pipeline.run.result yet -> result is empty.
hydrate_result() {
  local tid="$1"
  local ep="/api/datasources/proxy/uid/${DS}/api/traces/${tid}"
  gw_get "$ep" | jq -r '
    [ (.data.batches // .batches)[]?.scopeSpans[]?.spans[]?
      | select(.name | startswith("BUILD "))
      | { r:[.attributes[]?|select(.key=="ci.pipeline.run.result")|.value.stringValue][0],
          u:[.attributes[]?|select(.key=="ci.pipeline.run.url")|.value.stringValue][0] } ]
    | (.[0] // {})
    | "\(.r // "")\t\(.u // "")"'
}

# find_build <app> <strategy> -> "result<TAB>durationMs<TAB>url<TAB>traceID", or empty.
# RESULT_FILTER=SUCCESS: newest trace whose result==SUCCESS (skips running/failed).
# RESULT_FILTER=ANY:     newest trace of any result; empty result is reported as RUNNING.
find_build() {
  local app="$1" strat="$2" line startNs durMs tid res r u
  while IFS=$'\t' read -r startNs durMs tid; do
    [ -z "$tid" ] && continue
    res="$(hydrate_result "$tid")"
    r="${res%%$'\t'*}"; u="${res#*$'\t'}"
    if [ "$RESULT_FILTER" = "SUCCESS" ]; then
      [ "$r" = "SUCCESS" ] || continue          # skip running (empty) and non-SUCCESS
      printf '%s\t%s\t%s\t%s\n' "$r" "$durMs" "$u" "$tid"; return 0
    else
      [ -z "$r" ] && r="RUNNING"                # ANY mode: no terminal result => running
      printf '%s\t%s\t%s\t%s\n' "$r" "$durMs" "$u" "$tid"; return 0
    fi
  done < <(list_recent_traces "$app" "$strat")
  return 0   # nothing matched
}

fmt_dur() {  # ms -> "Ns" (rounded), or "-" if empty
  local ms="$1"; [ -z "$ms" ] && { echo "-"; return; }
  awk -v ms="$ms" 'BEGIN{ printf "%ds", int(ms/1000 + 0.5) }'
}

{
  echo "PLATFORM-1802 build summary from Grafana Tempo (uid=${DS})"
  echo "window: last ${HOURS}h  (since $(date -u -r ${start} +%FT%TZ 2>/dev/null || date -u -d @${start} +%FT%TZ))"
  if [ "$RESULT_FILTER" = "SUCCESS" ]; then
    echo "mode: last SUCCESSFUL build per app x strategy (running/failed builds ignored)"
  else
    echo "mode: latest build of ANY result per app x strategy (RUNNING = no terminal result yet)"
  fi
  echo "app-filters: [${FILTERS[*]:-<all>}]   generated: $(date -u +%FT%TZ)"
  echo "==================================================================================="
  printf "%-26s %-18s %-9s %-8s %s\n" "APP" "STRATEGY" "RESULT" "DUR" "BUILD URL"
  echo "-----------------------------------------------------------------------------------"
  for app in "${APPS[@]}"; do
    app_matches "$app" || continue
    for strat in "${STRATS[@]}"; do
      row=$(find_build "$app" "$strat")
      if [ -z "$row" ]; then
        if [ "$RESULT_FILTER" = "SUCCESS" ]; then
          printf "%-26s %-18s %-9s %-8s %s\n" "$app" "$strat" "none" "-" "(no SUCCESS in window)"
        else
          printf "%-26s %-18s %-9s %-8s %s\n" "$app" "$strat" "none" "-" "(no trace in window)"
        fi
        continue
      fi
      r="${row%%$'\t'*}"; rest="${row#*$'\t'}"
      dur_ms="${rest%%$'\t'*}"; rest="${rest#*$'\t'}"
      u="${rest%%$'\t'*}"; tid="${rest#*$'\t'}"
      printf "%-26s %-18s %-9s %-8s %s\n" "$app" "$strat" "$r" "$(fmt_dur "$dur_ms")" "$u"
      [ "${RAW:-0}" = "1" ] && echo "    traceID=$tid"
    done
  done
  echo "==================================================================================="
  echo "RESULT is the root BUILD span's ci.pipeline.run.result (SUCCESS/FAILURE/ABORTED)."
  echo "Default mode reports the last SUCCESSFUL build and ignores running/failed jobs, so it"
  echo "is safe to paste into the ticket. Use RESULT_FILTER=ANY to see the latest of any result"
  echo "(RUNNING = in-flight build with no terminal result yet)."
  echo "'none' = no matching build in the window (widen with HOURS=24)."
  echo ""
  echo "CAVEATS: these numbers are for sample apps with minimal dependencies. Real-world apps"
  echo "will have longer build times under emulation."
  echo ""
  echo "IDEAL USAGE SCENARIO: the best use case for emulation is when an app is pre-compiled"
  echo "and the buildpack is simply assembling the container and adding a runtime (e.g. Java)."
} 2>&1 | tee "$log"

echo "LOG=$log"
