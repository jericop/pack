---
inclusion: always
---

# MVP Build Testing Strategy (BuildKit multi-arch / OCI-layout)

While the BuildKit multi-arch feature is in MVP, we validate it by driving the
real `pack` binary against a sample app and publishing to a LOCAL registry, then
comparing an initial build vs a rebuild to observe caching behavior. We do NOT
iterate through unit/integration test files for this — we use the CLI directly so
we can read raw build output and timing.

## Test app

Always use `samples/go/no-imports`:
`/Users/jpena/.repos/paketo-buildpacks/samples/go/no-imports`

## Prerequisites (one-time per session)

1. **Build the pack binary from source** (the fork with the BuildKit multi-arch
   work), e.g.:
   ```bash
   go build -o /tmp/pack-poc .
   /tmp/pack-poc version
   ```
   Rebuild the binary whenever pack source changes.

2. **Local registry for push tests.** Run a `registry:2` in daemon mode. Note on
   macOS port 5000 is taken by ControlCenter/AirPlay and 5001 by Docker, so use
   5050:
   ```bash
   docker run -d --restart unless-stopped --name pack-local-registry -p 5050:5000 registry:2
   curl -s -o /dev/null -w "%{http_code}\n" http://localhost:5050/v2/   # expect 200
   ```

3. **A buildx builder that can access the local registry.** The docker-container
   buildx builder performs the per-arch solves; multi-arch manifest assembly/push
   is host-side (go-containerregistry), so the HOST pushes to localhost:5050. Use
   an existing `pack-multiplatform` docker-container builder, or create/configure
   one that can reach the local registry (e.g. create it on a shared docker
   network and reference the registry by container name if the builder itself must
   reach it). Verify it is running:
   ```bash
   docker buildx ls
   ```

## The two-build comparison (core of the strategy)

Run the SAME `pack build` twice and save full output + duration for each, so we
can compare a cold build vs a warm rebuild (BuildKit layer cache, builder image
already resolved, etc.).

1. **Initial build (cold):** capture output + duration to a timestamped file.
2. **Rebuild (warm):** run the identical command again; capture output + duration
   to a separate timestamped file.
3. Compare the two logs and durations to see how caching behaves (which vertices
   are `CACHED`, whether the builder/run image is re-pulled, total wall time).

Save logs and timing under `/tmp/kiro-command-logs/` (see the command-output
logging steering). Include duration in each log. Example pattern:

```bash
mkdir -p /tmp/kiro-command-logs
build_cmd() {
  /tmp/pack-poc build localhost:5050/no-imports:multiarch \
    --path /Users/jpena/.repos/paketo-buildpacks/samples/go/no-imports \
    --builder jericop/ubuntu-noble-builder:buildkit-multi-arch-poc \
    --run-image paketobuildpacks/ubuntu-noble-run-tiny:latest \
    --platforms linux/amd64,linux/arm64 \
    --buildkit --build-backend buildkit-llb --buildkit-export-mode oci-layout \
    --buildkit-builder pack-multiplatform \
    --publish --trust-builder --verbose
}

# Cold build
log1=/tmp/kiro-command-logs/pack-build-initial-$(date +%Y%m%d-%H%M%S).log
{ echo "START: $(date -u +%FT%TZ)"; time build_cmd; echo "END: $(date -u +%FT%TZ)"; } 2>&1 | tee "$log1"

# Warm rebuild (identical command)
log2=/tmp/kiro-command-logs/pack-build-rebuild-$(date +%Y%m%d-%H%M%S).log
{ echo "START: $(date -u +%FT%TZ)"; time build_cmd; echo "END: $(date -u +%FT%TZ)"; } 2>&1 | tee "$log2"
```

Record the `real` time from `time` (and/or START/END) in each log so rebuild
speedups are quantifiable.

## Final step: verify the image is runnable (binary in layers)

After a successful build AND rebuild, verify the produced per-arch image is a
real, runnable app image — not a wrapper — by confirming the app binary exists
inside the image layers. For `samples/go/no-imports` the lifecycle produces a Go
binary (the workspace app). Check each per-arch image:

- Pull/inspect the per-arch image from the local registry (or read the on-disk
  per-arch OCI layout), resolve its config + layers, and confirm:
  - the config carries CNB labels (esp. `io.buildpacks.lifecycle.metadata`),
  - there are multiple real layers (run-image base + launcher + app layer),
  - the app binary is present in a layer (e.g. extract layers and find the
    compiled binary under the workspace/app path, or the launcher entry).
- A build that pushes an image whose single layer is a tarball of `/output` (an
  OCI layout tree) is WRONG — that is the wrapper bug, not a runnable image.

Save the verification output to `/tmp/kiro-command-logs/`.

## Reference: Dockerfile backend (run ONCE)

Once the LLB (buildkit-llb) backend build + rebuild + runnable check all pass,
run the SAME `pack build` twice with the Dockerfile backend
(`--build-backend buildkit-dockerfile`, which uses registry export mode) and save
output + durations. This is a ONE-TIME reference capture: the Dockerfile backend
code is NOT changing, so its logs/timings serve as the comparison baseline for
future LLB changes. Do not re-run the Dockerfile backend on every iteration —
reuse the saved reference logs. Use a distinct image tag (e.g.
`localhost:5050/no-imports:dockerfile`) so it does not clobber the LLB result.

## On errors: iterate, then resume

If a build ERRORS, switch to fixing it: read the full log, diagnose, change code
(prefer changing pack/lifecycle over tweaking tests), rebuild the binary, and
re-run. Once the build succeeds end-to-end again, RESUME this two-build
comparison strategy. Do not abandon the timing comparison because of a transient
failure — get back to it after the fix.

## What to look for (caching / perf)

- Is the builder image re-pulled by pack (host) and/or re-resolved by BuildKit on
  the rebuild? (Use `--pull-policy if-not-present` to avoid pack's host re-pull.)
- Are lifecycle phase vertices `CACHED` on the rebuild?
- Is the run image re-pulled inside the analyzer RUN each time (a known caching
  liability of `-pull-run-image` inside a RUN)? This is a key thing to optimize.
- Total wall time cold vs warm, and per-arch behavior (amd64 emulated vs arm64
  native on Apple silicon).
