#!/usr/bin/env bash
#
# Single-architecture backend comparison harness.
#
# Compares, on IDENTICAL footing, the two pack build backends for a single-arch
# build of each sample app:
#   - docker-daemon : the standard container-based build against the local Docker
#                     daemon (pack's default). Relies on the docker volume cache
#                     pack creates automatically and reuses on rebuild.
#   - buildkit      : the native BuildKit backend (--build-backend buildkit), which
#                     uses BuildKit's own vertex/layer cache.
#
# Everything else is held constant so the ONLY variable is the backend:
#   - the SAME (fork) pack binary,
#   - the SAME builder (jericop/ubuntu-noble-builder:buildkit-native-export, which
#     bundles the patched lifecycle — so this also verifies the daemon build is
#     backward-compatible with that builder),
#   - the SAME run image,
#   - both --publish,
#   - a SINGLE platform = the HOST platform (native, no QEMU emulation).
#
# For each app + backend it measures WALL TIME (real elapsed seconds — see the
# mvp-build-testing-strategy steering doc) of a cold build and a warm rebuild, and
# emits a side-by-side Markdown + CSV table.
#
# Usage:
#   internal/build/multiplatform/benchmarks/compare-backends.sh
#   APPS="go/mod nodejs/npm" internal/build/multiplatform/benchmarks/compare-backends.sh
#
# Key env vars (all optional; defaults shown):
#   PACK_BIN            pack binary to drive                     (pack on PATH)
#   SAMPLES_DIR         path to the buildpacks/samples checkout  (./samples)
#   BENCH_APPS          space-separated "lang/app" list          (the 5 defaults)
#   REGISTRY_PUSH       registry name builds push to             (localhost:5050)
#   REGISTRY_HOST       host-reachable registry (buildkit finalize) (localhost:5050)
#   PACK_HOST_REGISTRY_REMAP  "pushName=hostName" bridge          (auto if the two differ)
#   BUILDER             builder image (SAME for both backends)   (jericop/ubuntu-noble-builder:buildkit-native-export)
#   RUN_IMAGE           run image                                (paketobuildpacks/ubuntu-noble-run:latest)
#   PLATFORM            single target platform                   (host platform, auto-detected)
#   BUILDKIT_BUILDER    buildx builder name (buildkit backend)   (pack-multiplatform)
#   OUT_DIR             output dir for table + logs              (./compare-out)
#
set -uo pipefail

# ---- configuration (env with defaults) -------------------------------------
PACK_BIN="${PACK_BIN:-pack}"
SAMPLES_DIR="${SAMPLES_DIR:-./samples}"
BENCH_APPS="${BENCH_APPS:-python/poetry nodejs/npm java/maven java/java-node go/mod}"
REGISTRY_PUSH="${REGISTRY_PUSH:-localhost:5050}"
REGISTRY_HOST="${REGISTRY_HOST:-localhost:5050}"
BUILDER="${BUILDER:-jericop/ubuntu-noble-builder:buildkit-native-export}"
RUN_IMAGE="${RUN_IMAGE:-paketobuildpacks/ubuntu-noble-run:latest}"
BUILDKIT_BUILDER="${BUILDKIT_BUILDER:-pack-multiplatform}"
OUT_DIR="${OUT_DIR:-./compare-out}"
export GOTOOLCHAIN=auto

# Detect the host platform (linux/<arch>) so BOTH backends build natively. We map
# `uname -m` to the Go/OCI arch names pack expects.
detect_host_platform() {
  local m; m="$(uname -m)"
  case "$m" in
    x86_64|amd64)      echo "linux/amd64" ;;
    aarch64|arm64)     echo "linux/arm64" ;;
    *)                 echo "linux/amd64" ;; # conservative default
  esac
}
PLATFORM="${PLATFORM:-$(detect_host_platform)}"

# Bridge the buildkit-push name to the host-reachable name for the buildkit
# backend's host-side finalize (see PACK_HOST_REGISTRY_REMAP in the steering docs).
if [ -z "${PACK_HOST_REGISTRY_REMAP:-}" ] && [ "$REGISTRY_PUSH" != "$REGISTRY_HOST" ]; then
  export PACK_HOST_REGISTRY_REMAP="${REGISTRY_PUSH}=${REGISTRY_HOST}"
fi

RUN_TS="$(date -u +%Y%m%d-%H%M%S)"
LOG_DIR="${OUT_DIR}/logs"
TABLE_MD="${OUT_DIR}/compare-table-${RUN_TS}.md"
TABLE_CSV="${OUT_DIR}/compare-table-${RUN_TS}.csv"
mkdir -p "$LOG_DIR"

# ---- helpers ----------------------------------------------------------------
now_s() { date +%s.%N 2>/dev/null || date +%s; }
elapsed() { awk -v s="$1" -v e="$2" 'BEGIN { printf "%.2f", (e - s) }'; }
speedup() { awk -v c="$1" -v r="$2" 'BEGIN { if (r>0) printf "%.2fx", c/r; else printf "n/a" }'; }
ratio() { awk -v a="$1" -v b="$2" 'BEGIN { if (b>0) printf "%.2fx", a/b; else printf "n/a" }'; }

# do_build_daemon runs a standard docker-daemon build (default backend). No cache
# flags: pack auto-creates and reuses docker volume caches across builds. Single
# host platform, published.
do_build_daemon() {
  local image="$1" app_path="$2" logfile="$3"
  "$PACK_BIN" build "$image" \
    --path "$app_path" \
    --builder "$BUILDER" \
    --run-image "$RUN_IMAGE" \
    --platform "$PLATFORM" \
    --publish --trust-builder --verbose \
    >"$logfile" 2>&1
  return $?
}

# do_build_buildkit runs the native buildkit backend for the SAME single platform.
do_build_buildkit() {
  local image="$1" app_path="$2" logfile="$3"
  "$PACK_BIN" build "$image" \
    --path "$app_path" \
    --builder "$BUILDER" \
    --run-image "$RUN_IMAGE" \
    --platform "$PLATFORM" \
    --build-backend buildkit \
    --buildkit-builder "$BUILDKIT_BUILDER" \
    --publish --trust-builder --verbose \
    >"$logfile" 2>&1
  return $?
}

# run_pair runs cold+rebuild for one backend and echoes "cold rebuild speedup result".
run_pair() {
  local backend="$1" image="$2" app_path="$3" tag="$4"
  local cold_log rebuild_log s e rc cold rebuild sp result="OK"
  cold_log="${LOG_DIR}/${tag}-${backend}-cold.log"
  rebuild_log="${LOG_DIR}/${tag}-${backend}-rebuild.log"

  s=$(now_s); "do_build_${backend}" "$image" "$app_path" "$cold_log"; rc=$?; e=$(now_s)
  cold=$(elapsed "$s" "$e")
  if [ "$rc" -ne 0 ]; then echo "$cold — — FAIL(cold rc=$rc)"; return 1; fi

  s=$(now_s); "do_build_${backend}" "$image" "$app_path" "$rebuild_log"; rc=$?; e=$(now_s)
  rebuild=$(elapsed "$s" "$e")
  if [ "$rc" -ne 0 ]; then echo "$cold $rebuild — FAIL(rebuild rc=$rc)"; return 1; fi

  sp=$(speedup "$cold" "$rebuild")
  echo "$cold $rebuild $sp $result"
}

# ---- table header -----------------------------------------------------------
{
  echo "# Single-arch backend comparison — ${RUN_TS}"
  echo
  echo "Standard **docker-daemon** build vs the native **buildkit** backend, single"
  echo "platform (\`${PLATFORM}\`, native — no emulation), identical builder / run"
  echo "image / pack binary. All durations are **wall time** (real elapsed seconds)."
  echo
  echo "- pack: \`${PACK_BIN}\`"
  echo "- builder (both): \`${BUILDER}\`"
  echo "- run image: \`${RUN_IMAGE}\`"
  echo "- platform: \`${PLATFORM}\`"
  echo "- registry (push/host): \`${REGISTRY_PUSH}\` / \`${REGISTRY_HOST}\`"
  echo
  echo "| App | daemon cold (s) | daemon rebuild (s) | daemon speedup | buildkit cold (s) | buildkit rebuild (s) | buildkit speedup | cold Δ (bk/daemon) | rebuild Δ (bk/daemon) | Result |"
  echo "|-----|----------------:|-------------------:|---------------:|------------------:|---------------------:|-----------------:|-------------------:|----------------------:|--------|"
} > "$TABLE_MD"
echo "app,daemon_cold_s,daemon_rebuild_s,daemon_speedup,buildkit_cold_s,buildkit_rebuild_s,buildkit_speedup,cold_ratio_bk_over_daemon,rebuild_ratio_bk_over_daemon,result" > "$TABLE_CSV"

# ---- run the matrix ---------------------------------------------------------
overall_rc=0
for app in $BENCH_APPS; do
  app_path="${SAMPLES_DIR}/${app}"
  tag="$(echo "$app" | tr '/' '-')-${RUN_TS}"
  echo "==> [$app]"
  if [ ! -d "$app_path" ]; then
    echo "    SKIP: sample app not found at $app_path"
    echo "| \`$app\` | — | — | — | — | — | — | — | — | SKIP (missing) |" >> "$TABLE_MD"
    echo "$app,,,,,,,,,SKIP_MISSING" >> "$TABLE_CSV"
    continue
  fi

  # docker-daemon backend
  d_image="${REGISTRY_PUSH}/cmp-daemon-${tag}:latest"
  read -r d_cold d_rebuild d_speedup d_result <<<"$(run_pair daemon "$d_image" "$app_path" "$tag")"
  [ "$d_result" = "OK" ] || overall_rc=1

  # buildkit backend
  b_image="${REGISTRY_PUSH}/cmp-buildkit-${tag}:latest"
  read -r b_cold b_rebuild b_speedup b_result <<<"$(run_pair buildkit "$b_image" "$app_path" "$tag")"
  [ "$b_result" = "OK" ] || overall_rc=1

  # cross-backend ratios (buildkit / daemon): <1 means buildkit is faster.
  cold_ratio="—"; rebuild_ratio="—"
  if [ "$d_result" = "OK" ] && [ "$b_result" = "OK" ]; then
    cold_ratio=$(ratio "$b_cold" "$d_cold")
    rebuild_ratio=$(ratio "$b_rebuild" "$d_rebuild")
  fi
  result="OK"
  [ "$d_result" = "OK" ] && [ "$b_result" = "OK" ] || result="daemon=${d_result}; buildkit=${b_result}"

  echo "| \`$app\` | $d_cold | $d_rebuild | $d_speedup | $b_cold | $b_rebuild | $b_speedup | $cold_ratio | $rebuild_ratio | $result |" >> "$TABLE_MD"
  echo "$app,$d_cold,$d_rebuild,$d_speedup,$b_cold,$b_rebuild,$b_speedup,$cold_ratio,$rebuild_ratio,$result" >> "$TABLE_CSV"
  echo "    $app: daemon cold=${d_cold}s rebuild=${d_rebuild}s | buildkit cold=${b_cold}s rebuild=${b_rebuild}s -> ${result}"
done

{
  echo
  echo "_Δ columns are buildkit/daemon ratios: < 1.00x means buildkit is faster._"
  echo "_Logs for each build are under \`${LOG_DIR}\`._"
} >> "$TABLE_MD"

echo
echo "=== comparison table ==="
cat "$TABLE_MD"
echo
echo "Markdown: $TABLE_MD"
echo "CSV:      $TABLE_CSV"
echo "Logs:     $LOG_DIR"
exit "$overall_rc"
