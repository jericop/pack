#!/usr/bin/env bash
#
# BuildKit-native (Option A) multi-language benchmark harness.
#
# Drives the REAL pack binary with --build-backend buildkit-native against a
# matrix of sample apps (python/poetry, nodejs/npm, java/maven, java/java-node,
# ...) and, for each app, measures the WALL TIME (real elapsed seconds — see the
# mvp-build-testing-strategy steering doc) of:
#   - cold build   (no BuildKit cache),
#   - rebuild      (identical command; warm BuildKit cache),
#   - rebase       (swap the run image; metadata-only).
# It also records cache signal (count of BuildKit `CACHED` vertices on the rebuild)
# and the finalized image's layer count, then emits a Markdown table artifact.
#
# It is designed to run BOTH locally and in GitHub Actions. Everything is
# parameterized via environment variables with sensible defaults matching the MVP
# local-testing setup. Reproduce a run by invoking with the same env.
#
# Usage:
#   internal/build/multiplatform/benchmarks/benchmark.sh
#   APPS="python/poetry nodejs/npm" internal/build/multiplatform/benchmarks/benchmark.sh
#
# Key env vars (all optional; defaults shown):
#   PACK_BIN            pack binary to drive                     (pack on PATH)
#   SAMPLES_DIR         path to the buildpacks/samples checkout  (./samples)
#   BENCH_APPS          space-separated "lang/app" list          (the 4 defaults)
#   REGISTRY_PUSH       registry name BuildKit pushes to         (localhost:5050)
#   REGISTRY_HOST       host-reachable registry (finalize/rebase)(localhost:5050)
#   PACK_HOST_REGISTRY_REMAP  "pushName=hostName" bridge          (auto if the two differ)
#   BUILDER             builder image                            (jericop/ubuntu-noble-builder:buildkit-multi-arch-poc)
#   LIFECYCLE_IMAGE     lifecycle image                          (unset -> builder default)
#   RUN_IMAGE           run image                                (paketobuildpacks/ubuntu-noble-run:latest)
#   REBASE_RUN_IMAGE    run image to rebase onto                 (RUN_IMAGE)
#   PLATFORMS           target platforms                         (linux/amd64,linux/arm64)
#   BUILDKIT_BUILDER    buildx builder name                      (pack-multiplatform)
#   OUT_DIR             output dir for table + logs              (./benchmark-out)
#   DO_REBASE           run the rebase step (true/false)         (true)
#
set -uo pipefail

# ---- configuration (env with defaults) -------------------------------------
PACK_BIN="${PACK_BIN:-pack}"
SAMPLES_DIR="${SAMPLES_DIR:-./samples}"
BENCH_APPS="${BENCH_APPS:-python/poetry nodejs/npm java/maven java/java-node go/mod}"
REGISTRY_PUSH="${REGISTRY_PUSH:-localhost:5050}"
REGISTRY_HOST="${REGISTRY_HOST:-localhost:5050}"
BUILDER="${BUILDER:-jericop/ubuntu-noble-builder:buildkit-multi-arch-poc}"
LIFECYCLE_IMAGE="${LIFECYCLE_IMAGE:-}"
RUN_IMAGE="${RUN_IMAGE:-paketobuildpacks/ubuntu-noble-run:latest}"
REBASE_RUN_IMAGE="${REBASE_RUN_IMAGE:-$RUN_IMAGE}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
BUILDKIT_BUILDER="${BUILDKIT_BUILDER:-pack-multiplatform}"
OUT_DIR="${OUT_DIR:-./benchmark-out}"
DO_REBASE="${DO_REBASE:-true}"
export GOTOOLCHAIN=auto

# Bridge the buildkit-push name to the host-reachable name for host-side finalize
# and rebase (see PACK_HOST_REGISTRY_REMAP in the steering docs). Auto-set it when
# the two registry names differ and the caller has not set it explicitly.
if [ -z "${PACK_HOST_REGISTRY_REMAP:-}" ] && [ "$REGISTRY_PUSH" != "$REGISTRY_HOST" ]; then
  export PACK_HOST_REGISTRY_REMAP="${REGISTRY_PUSH}=${REGISTRY_HOST}"
fi

RUN_TS="$(date -u +%Y%m%d-%H%M%S)"
LOG_DIR="${OUT_DIR}/logs"
TABLE_MD="${OUT_DIR}/benchmark-table-${RUN_TS}.md"
TABLE_CSV="${OUT_DIR}/benchmark-table-${RUN_TS}.csv"
mkdir -p "$LOG_DIR"

# ---- helpers ----------------------------------------------------------------

# now_s prints a high-resolution epoch seconds value (fractional if the platform
# supports it). Used to compute wall time as an end-start delta, which is portable
# across GNU/BSD and CI (avoids depending on the `time` builtin's output format).
now_s() { date +%s.%N 2>/dev/null || date +%s; }

# elapsed prints the difference (seconds, 2dp) between two now_s values.
elapsed() { awk -v s="$1" -v e="$2" 'BEGIN { printf "%.2f", (e - s) }'; }

# count_cached prints the number of BuildKit "CACHED" vertices in a build log.
count_cached() { grep -c "CACHED" "$1" 2>/dev/null || echo 0; }

# layer_count prints the finalized image's layer count via crane (or "n/a").
layer_count() {
  local ref="$1"
  if command -v crane >/dev/null 2>&1; then
    crane config "$ref" 2>/dev/null | grep -o 'sha256:' | wc -l | tr -d ' '
  else
    echo "n/a"
  fi
}

# do_build runs one pack buildkit-native build of $app_path -> $image, teeing to
# $logfile. Returns pack's exit code.
do_build() {
  local image="$1" app_path="$2" logfile="$3"
  local lc_args=()
  [ -n "$LIFECYCLE_IMAGE" ] && lc_args=(--lifecycle-image "$LIFECYCLE_IMAGE")
  "$PACK_BIN" build "$image" \
    --path "$app_path" \
    --builder "$BUILDER" \
    "${lc_args[@]}" \
    --run-image "$RUN_IMAGE" \
    --platforms "$PLATFORMS" \
    --buildkit --build-backend buildkit-native \
    --buildkit-builder "$BUILDKIT_BUILDER" \
    --publish --trust-builder --verbose \
    >"$logfile" 2>&1
  return $?
}

# do_rebase rebases $image onto $REBASE_RUN_IMAGE, teeing to $logfile.
do_rebase() {
  local image="$1" logfile="$2"
  "$PACK_BIN" rebase "$image" \
    --run-image "$REBASE_RUN_IMAGE" \
    --publish \
    --insecure-registry "$REGISTRY_HOST" \
    --verbose \
    >"$logfile" 2>&1
  return $?
}

# ---- table header -----------------------------------------------------------
{
  echo "# BuildKit-native (Option A) benchmark — ${RUN_TS}"
  echo
  echo "All durations are **wall time** (real elapsed seconds). CACHED = count of"
  echo "BuildKit \`CACHED\` vertices observed on the rebuild (higher = better cache reuse)."
  echo
  echo "- pack: \`${PACK_BIN}\`"
  echo "- builder: \`${BUILDER}\`"
  echo "- lifecycle image: \`${LIFECYCLE_IMAGE:-<builder default>}\`"
  echo "- run image: \`${RUN_IMAGE}\` (rebase onto \`${REBASE_RUN_IMAGE}\`)"
  echo "- platforms: \`${PLATFORMS}\`"
  echo "- registry (push/host): \`${REGISTRY_PUSH}\` / \`${REGISTRY_HOST}\`"
  echo
  echo "| App | Cold build (s) | Rebuild (s) | Rebuild speedup | Rebase (s) | CACHED (rebuild) | Layers | Result |"
  echo "|-----|---------------:|------------:|----------------:|-----------:|-----------------:|-------:|--------|"
} > "$TABLE_MD"

echo "app,cold_s,rebuild_s,rebuild_speedup,rebase_s,cached_rebuild,layers,result" > "$TABLE_CSV"

# ---- run the matrix ---------------------------------------------------------
overall_rc=0
for app in $BENCH_APPS; do
  app_path="${SAMPLES_DIR}/${app}"
  # image tag: replace / with - and add a per-run suffix so cold builds are truly cold.
  tag="$(echo "$app" | tr '/' '-')-${RUN_TS}"
  image_push="${REGISTRY_PUSH}/bench-${tag}:latest"
  image_host="${REGISTRY_HOST}/bench-${tag}:latest"

  echo "==> [$app] app_path=$app_path image=$image_push"
  if [ ! -d "$app_path" ]; then
    echo "    SKIP: sample app not found at $app_path"
    echo "| \`$app\` | — | — | — | — | — | — | SKIP (missing) |" >> "$TABLE_MD"
    echo "$app,,,,,,,SKIP_MISSING" >> "$TABLE_CSV"
    continue
  fi

  result="OK"

  # cold build
  cold_log="${LOG_DIR}/${tag}-cold.log"
  s=$(now_s); do_build "$image_push" "$app_path" "$cold_log"; rc=$?; e=$(now_s)
  cold_s=$(elapsed "$s" "$e")
  if [ "$rc" -ne 0 ]; then result="FAIL(cold rc=$rc)"; overall_rc=1; fi

  # rebuild (warm) — only if cold succeeded
  rebuild_s="—"; speedup="—"; cached="—"
  if [ "$result" = "OK" ]; then
    rebuild_log="${LOG_DIR}/${tag}-rebuild.log"
    s=$(now_s); do_build "$image_push" "$app_path" "$rebuild_log"; rc=$?; e=$(now_s)
    rebuild_s=$(elapsed "$s" "$e")
    if [ "$rc" -ne 0 ]; then
      result="FAIL(rebuild rc=$rc)"; overall_rc=1
    else
      cached=$(count_cached "$rebuild_log")
      speedup=$(awk -v c="$cold_s" -v r="$rebuild_s" 'BEGIN { if (r>0) printf "%.2fx", c/r; else printf "n/a" }')
    fi
  fi

  # rebase — only if build path succeeded and enabled
  rebase_s="—"
  if [ "$result" = "OK" ] && [ "$DO_REBASE" = "true" ]; then
    rebase_log="${LOG_DIR}/${tag}-rebase.log"
    s=$(now_s); do_rebase "$image_host" "$rebase_log"; rc=$?; e=$(now_s)
    rebase_s=$(elapsed "$s" "$e")
    if [ "$rc" -ne 0 ]; then result="FAIL(rebase rc=$rc)"; overall_rc=1; fi
  fi

  layers=$(layer_count "$image_host")

  echo "| \`$app\` | $cold_s | $rebuild_s | $speedup | $rebase_s | $cached | $layers | $result |" >> "$TABLE_MD"
  echo "$app,$cold_s,$rebuild_s,$speedup,$rebase_s,$cached,$layers,$result" >> "$TABLE_CSV"
  echo "    $app: cold=${cold_s}s rebuild=${rebuild_s}s rebase=${rebase_s}s cached=${cached} layers=${layers} -> ${result}"
done

echo >> "$TABLE_MD"
echo "_Logs for each build/rebuild/rebase are under \`${LOG_DIR}\`._" >> "$TABLE_MD"

echo
echo "=== benchmark table ==="
cat "$TABLE_MD"
echo
echo "Markdown: $TABLE_MD"
echo "CSV:      $TABLE_CSV"
echo "Logs:     $LOG_DIR"
exit "$overall_rc"
