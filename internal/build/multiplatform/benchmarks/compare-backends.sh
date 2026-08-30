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
#   - a SINGLE platform = the HOST platform (native, no QEMU emulation).
#
# Output target differs by backend (we measure the BUILD, not the publish): the
# docker-daemon backend builds to the local Docker daemon (its natural mode); the
# buildkit backend publishes to a registry (it cannot load to the daemon).
#
# Fairness controls:
#   - Builder + run images are WARMED UP once up front (docker pull for the daemon;
#     a 2-FROM cache-only buildx solve for buildkit) so image pull time is excluded
#     from every timed build and both backends start from the same warm base state.
#   - COLD builds are truly cold at the buildpack level: the daemon backend's pack
#     cache volumes for the image are removed before its cold build. The buildkit
#     cache is NOT pruned (base images stay warm, matching the daemon).
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
#   REGISTRY_PUSH       default push registry name (buildkit)    (localhost:5050)
#   REGISTRY_HOST       host-reachable registry (buildkit finalize) (localhost:5050)
#   BUILDKIT_REGISTRY_PUSH push name for the buildkit build       (REGISTRY_PUSH)
#     (split-name local setup: BUILDKIT_REGISTRY_PUSH=pack-local-registry:5000)
#   NOTE: the docker-daemon backend builds to the local daemon (no push/registry).
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
# The docker-daemon backend builds to the LOCAL DAEMON (no push), so it needs no
# registry. The buildkit backend must publish; it solves inside the
# docker-container builder, so in split-name local setups it uses the in-network
# registry name (e.g. pack-local-registry:5000) while the host-side finalize reaches
# the same registry by its host name (REGISTRY_HOST). BUILDKIT_REGISTRY_PUSH
# defaults to REGISTRY_PUSH for the simple same-name case (CI / Docker Hub).
BUILDKIT_REGISTRY_PUSH="${BUILDKIT_REGISTRY_PUSH:-$REGISTRY_PUSH}"
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

# Bridge the buildkit push name to the host-reachable name for the buildkit
# backend's host-side finalize (see PACK_HOST_REGISTRY_REMAP in the steering docs).
# Only the buildkit backend needs this: its solve pushes under the in-network name,
# but the host-side finalize must reach the same registry by its host name.
if [ -z "${PACK_HOST_REGISTRY_REMAP:-}" ] && [ "$BUILDKIT_REGISTRY_PUSH" != "$REGISTRY_HOST" ]; then
  export PACK_HOST_REGISTRY_REMAP="${BUILDKIT_REGISTRY_PUSH}=${REGISTRY_HOST}"
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

# warm_up_images makes the builder + run images available to BOTH backends BEFORE
# any timed build, so image pull/resolve is NOT counted in the build durations
# (both backends start from the same warm base-image state — a fair comparison):
#   - docker daemon: `docker pull` puts them in the local daemon image store.
#   - buildkit: build a tiny 2-stage Dockerfile (FROM builder / FROM run-image) on
#     the docker-container builder for the target platform, which pulls those images
#     into the builder's content cache. We do NOT prune this cache between builds, so
#     the buildkit base images stay warm just like the daemon's pulled images do.
warm_up_images() {
  local log="${LOG_DIR}/warmup-${RUN_TS}.log"
  echo "==> warming up images (excluded from timings): ${BUILDER}, ${RUN_IMAGE}" | tee "$log"
  {
    echo "--- docker pull (daemon store) ---"
    docker pull --platform "$PLATFORM" "$BUILDER"
    docker pull --platform "$PLATFORM" "$RUN_IMAGE"

    echo "--- seed buildkit builder cache (2-FROM Dockerfile) ---"
    local seed_dir; seed_dir="$(mktemp -d)"
    cat > "${seed_dir}/Dockerfile" <<EOF
FROM ${BUILDER} AS builder
FROM ${RUN_IMAGE} AS run
EOF
    # --output=type=cacheonly: solve both stages (pulling both images into the
    # builder's cache) without producing/exporting an image.
    docker buildx build \
      --builder "$BUILDKIT_BUILDER" \
      --platform "$PLATFORM" \
      --output=type=cacheonly \
      "$seed_dir"
    rm -rf "$seed_dir"
  } >>"$log" 2>&1
  echo "    warm-up done (log: $log)"
}

# clean_pack_volumes removes the pack build/launch cache volumes for a given image
# name so the NEXT daemon build of it is a TRUE cold build (buildpacks re-run, no
# reused dependency layers). Pack names these `pack-cache-<sanitizedRef>-<hash>.build`
# / `.launch`; we match on the sanitized image tag, which is unique per run. Only the
# daemon backend uses these volumes; the buildkit backend does not.
clean_pack_volumes() {
  local image="$1"
  # Sanitized ref pack uses in the volume name: '/' and ':' -> '_'.
  local san; san="$(echo "$image" | tr '/:' '__')"
  local vols; vols="$(docker volume ls -q --filter "name=pack-cache-" 2>/dev/null | grep -F "$san" || true)"
  if [ -n "$vols" ]; then
    echo "$vols" | xargs -r docker volume rm >/dev/null 2>&1 || true
  fi
}

# do_build_daemon runs a standard docker-daemon build (default backend). It builds
# to the LOCAL DOCKER DAEMON (no --publish) — the natural stock-pack usage; we are
# measuring the BUILD, not the publish. No cache flags: pack auto-creates and reuses
# docker volume caches across builds. Single host platform.
do_build_daemon() {
  local image="$1" app_path="$2" logfile="$3"
  "$PACK_BIN" build "$image" \
    --path "$app_path" \
    --builder "$BUILDER" \
    --run-image "$RUN_IMAGE" \
    --platform "$PLATFORM" \
    --trust-builder --verbose \
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

  # Ensure a TRUE cold build: the daemon backend caches buildpack layers in pack
  # volumes, so remove this image's volumes before its cold build. (Base images
  # stay warm from warm_up_images — we only clear the buildpack/app cache.) The
  # buildkit backend uses no pack volumes; its base images are already warm and its
  # cache is intentionally NOT pruned.
  if [ "$backend" = "daemon" ]; then
    clean_pack_volumes "$image"
  fi

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
  echo "- output: docker-daemon -> local daemon (no push); buildkit -> registry \`${BUILDKIT_REGISTRY_PUSH}\` (host \`${REGISTRY_HOST}\`)"
  echo
  echo "| App | daemon cold (s) | daemon rebuild (s) | daemon speedup | buildkit cold (s) | buildkit rebuild (s) | buildkit speedup | cold Δ (bk/daemon) | rebuild Δ (bk/daemon) | Result |"
  echo "|-----|----------------:|-------------------:|---------------:|------------------:|---------------------:|-----------------:|-------------------:|----------------------:|--------|"
} > "$TABLE_MD"
echo "app,daemon_cold_s,daemon_rebuild_s,daemon_speedup,buildkit_cold_s,buildkit_rebuild_s,buildkit_speedup,cold_ratio_bk_over_daemon,rebuild_ratio_bk_over_daemon,result" > "$TABLE_CSV"

# ---- warm up images (excluded from all timings) -----------------------------
warm_up_images

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

  # docker-daemon backend: build to the local daemon (no registry prefix, no push).
  d_image="cmp-daemon-${tag}:latest"
  read -r d_cold d_rebuild d_speedup d_result <<<"$(run_pair daemon "$d_image" "$app_path" "$tag")"
  [ "$d_result" = "OK" ] || overall_rc=1

  # buildkit backend (solves in-container -> in-network name; finalize remaps to host)
  b_image="${BUILDKIT_REGISTRY_PUSH}/cmp-buildkit-${tag}:latest"
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
