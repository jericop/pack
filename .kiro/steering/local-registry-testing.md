---
inclusion: always
---

# Local registry testing for the buildkit backend (the localhost:5050 trap)

When validating `--build-backend buildkit` locally against the `pack-local-registry`
you MUST reference the registry by its **container name on the shared docker network**,
not `localhost:5050`. Getting this wrong wastes a full multi-arch build cycle (~7+ min)
that fails at the analyzer with a registry dial timeout.

## The trap (what fails and why)

Building with an image ref of `localhost:5050/...` fails at the FIRST lifecycle phase:

```
#8 [linux/arm64] lifecycle: analyzer
#8 ERROR: failed to initialize analyzer: validating registry read access:
   failed to ensure registry read access to localhost:5050/pyapp:fix2:
   Get "http://localhost:5050/v2/": dial tcp [::1]:5050: connect: connection timed out
```

Why: the lifecycle phases run INSIDE the buildkit builder container
(`buildx_buildkit_pack-multiplatform0`), not on the host. Inside that container
`localhost:5050` is the container's own loopback, not the host's registry, so the
analyzer's registry read-access check times out. The app-context sync and env vertices
already succeeded by this point — a `localhost:5050` failure at the analyzer is NOT a
code bug, it's this networking trap.

## The working setup

The `pack-multiplatform` buildx builder is created on the `pack-test` docker network
(`DriverOpts: {network: pack-test}`), and `pack-local-registry` is attached to the same
network. So the builder can reach the registry by **container name**:

- Registry ref for `pack build`: **`pack-local-registry:5000/...`** (NOT `localhost:5050`).
- Host-side finalize (go-containerregistry, runs on the HOST where the name is
  `localhost:5050`) is bridged with the remap env var:
  **`PACK_HOST_REGISTRY_REMAP="pack-local-registry:5000=localhost:5050"`**.

`isLikelyInsecureRegistry` already treats a bare container name like
`pack-local-registry` (no dot) as insecure/plain-HTTP, so `registry.insecure=true` and
`-insecure-registry` are set automatically — no extra flags needed.

### Verify the network wiring (cheap pre-flight)

```bash
# registry reachable from the host:
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:5050/v2/         # expect 200
# registry reachable from INSIDE the builder by container name:
docker exec buildx_buildkit_pack-multiplatform0 sh -c 'wget -qO- http://pack-local-registry:5000/v2/'   # expect {}
```

If the second command fails, attach both to the network:
`docker network connect pack-test pack-local-registry` (and recreate the builder with
`--driver-opt network=pack-test` if needed).

## Canonical local build command (copy this)

Set env in a SCRIPT (never inline `VAR=...`; see command-execution steering), then:

```bash
export GIT_PAGER=cat
export PACK_HOST_REGISTRY_REMAP="pack-local-registry:5000=localhost:5050"

/tmp/pack-poc build pack-local-registry:5000/<app>:<tag> \
  --path <app-path> \
  --builder jericop/ubuntu-noble-builder:buildkit-native-export \
  --run-image paketobuildpacks/ubuntu-noble-run:latest \
  --platform linux/amd64 --platform linux/arm64 \
  --build-backend buildkit \
  --buildkit-builder pack-multiplatform \
  --pull-policy if-not-present \
  --publish --trust-builder
```

- `--buildkit-builder pack-multiplatform` is required unless the CURRENT buildx builder
  is already a docker-container/remote driver. The macOS default is `desktop-linux`
  (docker driver), which correctly errors now (see FOLLOWUPS #1) — so pass the flag.
- `--pull-policy if-not-present` avoids re-pulling the builder image every iteration
  (saves time AND Docker Hub rate limit).
- `--trust-builder` is required for the fork builder (FOLLOWUPS #3, by decision).

## Nested-source apps (fsutil ordering regression)

`/Users/jpena/.repos/r7/pd-sample-python-app` is the canonical repro for the fsutil
"changes out of order" bug (FOLLOWUPS #2): it has a nested `src/pd_sample_python_app/`
layout AND a `project.toml` `include = [...]` filter. Use it to confirm the app-context
sync fix: the build must pass the `[plat] copy app source` vertex with no "changes out
of order". For a fast fsutil-only check the single-arch build is enough (the ordering
bug is not arch-specific), but the registry ref rule above still applies.

## Docker Hub rate limits — prefer local, fall back to a single-agent Actions run

Iterating locally avoids Docker Hub pull rate limits. If local registry networking is
blocking progress AND you would otherwise hammer Docker Hub, iterate in GitHub Actions
using a SINGLE-cell workflow (e.g. `benchmark-perf-smoke.yml`, one app/one arch) rather
than the full matrix — but only as a LAST RESORT, since Actions runs are slower to
iterate on than a local build.
