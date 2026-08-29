---
inclusion: auto
---

# BuildKit Multi-Architecture Build Feature

## Overview

This workspace contains a proof-of-concept implementation of BuildKit-based multi-architecture builds for `pack`. The feature lives in `internal/build/multiplatform/` and is gated behind `--buildkit` (requires `pack config experimental true`).

> Source of truth: this steering file is the canonical summary of the BuildKit
> multi-arch feature. The comprehensive doc at
> `internal/build/multiplatform/buildkit-multi-arch-readme.md` is kept in sync with
> it — update this file first, then propagate changes to the readme.

## Key Architecture Decisions

### Three Build Backends

- **Dockerfile backend** (`--build-backend buildkit-dockerfile`, default): Generates a Dockerfile, shells out to `docker buildx build`. Supports parallel multi-platform via single buildx invocation. Has full BuildKit caching support (layer cache + lifecycle cache mounts with uid/gid via Dockerfile frontend).
- **LLB backend** (`--build-backend buildkit-llb`): Uses BuildKit Go SDK directly (`github.com/moby/buildkit/client`). Connects to buildkit daemon via `docker-container://` scheme. Parallel solves via errgroup. Requires patched lifecycle with `-skip-chown` flag for cache mounts (BuildKit LLB API doesn't support uid/gid on cache mounts like the Dockerfile frontend does).
- **BuildKit-native backend** (`--build-backend buildkit-native`, experimental, MVP validated): BuildKit BUILDS and PUSHES the app image natively (one multi-arch image, NO intermediate tags, layer data never egresses); then a lifecycle-owned FINALIZE step authors the correct CNB metadata on the pushed image from its ACTUAL produced layers. See "BuildKit-Native Export Backend (Option A)" below and the readme.

### BuildKit-Native Export Backend (Option A — build-then-finalize)

- **Two phases.** (1) BUILD: run the lifecycle phases + assemble `FROM run-image` in BuildKit; BuildKit pushes one image (multi-arch = one OCI index), no intermediate tags; the build attaches a single label `io.buildpacks.buildkit.native.build-metadata` (the ordered plan + emitted CNB labels) and does NOT write a valid final `io.buildpacks.lifecycle.metadata`. (2) FINALIZE: pack calls the lifecycle `phase/finalize` library (like `phase.Rebaser`), which reads the pushed image's produced diffIDs + the build-metadata label, authors `io.buildpacks.lifecycle.metadata` (per-layer SHAs = produced diffIDs; RunImage boundary), removes the build-metadata label, and re-pushes config+manifest(+index) only — NO layer changes.
- **Why authoring, not rewriting.** BuildKit's exporter always assigns diffIDs at export; a frontend cannot inject blobs via the gateway result (verified, moby/buildkit v0.32.2). The earlier design emitted metadata with the INTENDED diffIDs then pack REWROTE the SHAs post-push (a patch). Option A never writes a wrong final label: finalize AUTHORS the metadata against the produced diffIDs the first time, in the lifecycle (one source of truth), not pack.
- **Finalize atomicity + failure.** Finalize is a separate registry op after the push; it re-pushes to the SAME tag via a single manifest/index `PUT` (tag-atomic). On failure the tag still resolves to the pushed (pre-finalize) image — runnable, just not yet rebuildable/rebaseable. Finalize is IDEMPOTENT. A future opt-in `--buildkit-fix-remote-image-metadata` can self-heal an interrupted build's image in place using its retained build-metadata label (finalize `KeepBuildMetadataLabel`). Not implemented.
- **Layer diffIDs differ** from registry/oci-layout modes (BuildKit recomputes them). Rebase depends only on the run-image `TopLayer` boundary, and finalize makes per-layer metadata SHAs match the actual layers, so rebase + buildpack-layer patching both work. Validated across REPEATED rebuilds + rebases + rebuild-after-rebase + multi-arch.
- **No frontend; assembly is `llb.Copy` from emitted sources.** The `buildkit/cnbfrontend` package + `cmd/cnb-frontend` are DELETED. Pack drives an IN-PROCESS gateway BuildFunc (`internal/build/multiplatform/native_buildfunc.go`) that assembles `FROM run-image` by `llb.Copy`-ing each CNB layer from its emitted filesystem SOURCE (buildpack layers from `/layers/<bp>/<layer>`, app from `/workspace`, launcher file; chown to CNB uid:gid). Synthesized layers (process-types) are copied from a tiny tree the emit step extracts in Go. NO `tar -xf`, no run-image shell/tar, no re-materialization of large layers. emit-mode records per-layer Source refs (lifecycle `layers.Layer.Source` → `emit.LayerOp.Source`); the retired parts are the frontend, the per-layer metadata-SHA REWRITE (now finalize authoring), and the `io.buildpacks.native.layer-order` label (superseded by `io.buildpacks.buildkit.native.build-metadata`).
- **Run image** resolved from the analyzer-written `/layers/analyzed.toml` (digest-pinned), used as the `llb.Image` base; never modified.
- **Local dev prereqs:** phase RUNs use `llb.Network(HOST)` + `network.host` entitlement; a local registry on the builder's docker network + insecure-registry config; `PACK_HOST_REGISTRY_REMAP` bridges the buildkit-vs-host registry-name split for host-side finalize (test-env only, no-op in prod).

### Registry Authentication

Pack resolves credentials from the Docker keychain (including credential helpers like `docker-credential-desktop`) and passes them as `CNB_REGISTRY_AUTH` environment variable in the Dockerfile/LLB. This is the same mechanism the lifecycle uses in pack's normal flow. No secret file mounts needed.

For the LLB backend, a Docker auth session (`authprovider.NewDockerAuthProvider`) is also attached for buildkit's own operations (pulling images, pushing/pulling registry cache).

### Intermediate Tags

In **registry mode** (the default) and in the **Dockerfile MVP backend**, the lifecycle exporter pushes per-arch images to ephemeral tags: `<image>-build-<8char-hex>-<arch>`. After both platforms complete, pack assembles the manifest list at the final tag using its built-in manifest list functionality (imgutil + go-containerregistry), eliminating the dependency on `docker buildx imagetools` for manifest assembly. These intermediate tags remain on the registry — this is by design for the Dockerfile MVP.

The **LLB backend's `oci-layout` export mode** (`--build-backend=buildkit-llb --buildkit-export-mode=oci-layout`, functional but experimental) **eliminates intermediate tags entirely**: each platform's image is exported to an on-disk OCI layout, imported via `llb.OCILayout()`, and pushed natively (`ExporterImage`, single-arch) or assembled into one manifest list from the per-arch layouts (multi-arch) — no per-arch tags ever land on the registry.

### Phase Isolation

Each lifecycle phase is its own `RUN` command in the Dockerfile (or `llb.Run` node in LLB). The `CNB_REGISTRY_AUTH` env var is available to all phases. The buildpack code (detector, builder) cannot exfiltrate credentials because the env var is set at build time and not persisted in the buildkit output image (which uses `--output type=cacheonly`).

### Buildpack Order Injection

The builder's buildpack order is extracted from its `io.buildpacks.buildpack.order` label and written to `/cnb/order.toml` in the generated Dockerfile. This ensures consistent detection behavior across platforms regardless of the builder image's default order.

## File Structure

```
internal/build/multiplatform/
├── backend.go                    # BuildBackend interface, types, ExportMode, Platform
├── backend_dockerfile.go         # DockerfileBackend: generates Dockerfile, shells out to buildx
├── backend_factory.go            # NewBackend() factory, auto-detection
├── backend_llb.go                # LLBBackend: direct BuildKit SDK, parallel solves
├── buildkit-multi-arch-readme.md # Comprehensive documentation
├── docker.go                     # Docker CLI helpers (runDockerCommand, etc.)
├── dockerfile_generator.go       # GenerateDockerfileMultiPlatform()
├── backend_native.go             # NativeBackend (buildkit-native): drives the BuildKit build+push, then calls the lifecycle finalize.Finalize library post-push (Option A)
├── metadata_rewrite.go           # Now only the test-env applyHostRegistryRemap shim; the rewrite logic moved to the lifecycle phase/finalize library
├── emit_contract.go              # Parser for the lifecycle emit contract (plan.json/config.json)
├── executor.go                   # MultiPlatformExecutor orchestration (skips own assembly when backend PushesNatively)
├── multiplatform_test.go         # Unit tests for Dockerfile generation
├── oci_layout_push.go            # OCI layout manifest-list assembly + native push (functional, LLB)
├── oci_layout_inspect.go         # On-disk OCI layout inspector (layers, diff IDs, config, lifecycle metadata, SBOM)
├── oci_layout_parity.go          # Offline parity comparison between two on-disk layouts
├── oci_layout_rebase.go          # Offline rebase-readiness precondition checks
└── *_test.go / *_integration_test.go  # Tier 1 unit tests (default) + Tier 2/Tier 3 integration tests (env-var gated, skipped by default)
```

## CLI Flags

| Flag | Description |
|------|-------------|
| `--buildkit` | Enable BuildKit backend (experimental) |
| `--platforms` | Comma-separated platforms (e.g., `linux/amd64,linux/arm64`) |
| `--build-backend` | `buildkit-dockerfile` (default), `buildkit-llb`, or `buildkit-native` (experimental) |
| `--buildkit-builder` | Name of the buildx builder |
| `--buildkit-cache-from` | Registry cache source |
| `--buildkit-cache-to` | Registry cache destination |
| `--buildkit-export-mode` | `registry` (default) or `oci-layout` (LLB backend only, functional/experimental — eliminates intermediate tags) |
| `--buildkit-fix-remote-image-metadata` | (PLANNED, not implemented) buildkit-native only: if the target image already exists remotely with invalid/stale CNB metadata, fix it in place before building (self-healing). Default off = warn and stop. |
| `--lifecycle-image` | buildkit-native: emit-capable lifecycle image the frontend overlays/uses |

## Related Repositories

- **jericop/cnb-lifecycle** (`skip-chown` branch): Patched lifecycle with `-skip-chown` flag
- **jericop/cnb-lifecycle** (`buildkit-multi-arch-support` branch): Adds `-pull-run-image` flag for OCI layout mode
- **jericop/cnb-lifecycle** (`buildkit-native-export` branch): Exporter emit-mode computing the plan + build-metadata label + per-layer Source refs (`phase/emit`, `layers/`) and the FINALIZE library authoring CNB metadata from produced diffIDs (`phase/finalize`), consumed by pack like `phase.Rebaser`. The `buildkit/cnbfrontend` package + `cmd/cnb-frontend` are DELETED (assembly is now pack-side `llb.Copy`). Published image `jericop/lifecycle:buildkit-native-export`. Pack consumes it as a library via a `replace` directive.
- **jericop/cnb-pack** (`buildkit-native-export` branch): The `buildkit-native` backend — in-process BuildFunc (`native_buildfunc.go`) assembling via `llb.Copy` + `finalize.Finalize` post-push (`backend_native.go`, `executor.go` single-arch routing, `metadata_rewrite.go` test-env shim only).
- **jericop/ubuntu-noble-builder** (`skip-chown-lifecycle` branch): Builder with patched lifecycle
- **jericop/pr-compliance-app** (`pack-buildkit-poc` branch): CI testing workflow
- **jericop/cnb-rfcs** (`buildkit-mutliarch-build` branch): RFC document

## Testing

```bash
# Dockerfile backend
pack build registry.example.com/myapp:latest \
  --path ./app \
  --builder jericop/ubuntu-noble-builder:skip-chown-poc \
  --platforms linux/amd64,linux/arm64 \
  --buildkit --publish --trust-builder \
  --buildkit-builder pack-multiplatform

# LLB backend
pack build registry.example.com/myapp:latest \
  --path ./app \
  --builder jericop/ubuntu-noble-builder:skip-chown-poc \
  --platforms linux/amd64,linux/arm64 \
  --buildkit --publish --trust-builder \
  --buildkit-builder pack-multiplatform \
  --build-backend buildkit-llb

# BuildKit-native backend (experimental) — requires the emit-capable lifecycle +
# a builder that can reach the (local) registry; see the readme for the full
# local-dev prereqs (network.host entitlement, insecure registry, PACK_HOST_REGISTRY_REMAP).
pack build pack-local-registry:5000/myapp:latest \
  --path ./app \
  --builder jericop/ubuntu-noble-builder:buildkit-multi-arch-poc \
  --run-image paketobuildpacks/ubuntu-noble-run:latest \
  --platforms linux/amd64,linux/arm64 \
  --buildkit --build-backend buildkit-native \
  --buildkit-builder pack-multiplatform \
  --lifecycle-image pack-local-registry:5000/lifecycle:native-updated \
  --publish --trust-builder
```

For buildkit-native, verification MUST cover REPEATED cycles (≥2 rebuilds, ≥2
rebases, rebuild-after-rebase, self-heal-then-repeat), not just the first build —
to confirm the durable `io.buildpacks.native.layer-order` label is re-recorded
correctly each rebuild and survives rebase.

## Known Issues

- LLB backend requires a patched lifecycle: `-skip-chown` (BuildKit LLB API doesn't support uid/gid on cache mounts) and, for `oci-layout` export mode, also `-pull-run-image`. Both are bundled in `jericop/ubuntu-noble-builder:skip-chown-poc`.
- Intermediate tags remain on the registry in **registry mode** (the default) and for the **Dockerfile MVP** — by design. The LLB backend's `oci-layout` export mode eliminates them (functional, experimental).
- The LLB `oci-layout` end-to-end native push against a real registry is verified only by the env-var-gated Tier 3 test; on-disk correctness/parity is verified by default. The mode stays experimental until Tier 3 lands in CI.
- Local ephemeral registries are only reachable from the `docker-container` builder if it was created with `--driver-opt network=<shared-network>` (network-isolation caveat); the primary tests avoid this by running on-disk.
- `pack builder create` with `docker://` lifecycle URI requires the pack fork (stable pack doesn't support it)
- **buildkit-native**: layer diffIDs differ from other modes (BuildKit recomputes them); the post-push FINALIZE step is REQUIRED for rebuild/rebase (it authors the CNB metadata from the produced diffIDs). If finalize fails, the pushed image is runnable but not rebuildable/rebaseable until finalized (re-run the build, or the PLANNED self-healing `--buildkit-fix-remote-image-metadata`).
- **buildkit-native**: current per-layer assembly runs `tar` via the run image's shell (assumes it has sh/tar); temporary MVP limitation to be replaced by extracting with build-image `tar` (run images may be shell-less). Finalize is unaffected.

## Testing (verification approach)

Non-registry (on-disk) testing is the primary verification: Tier 1 unit tests and Tier 2 on-disk layout checks (`InspectOCILayout` / `CompareParity` / `CheckRebaseReadiness`) run by default with no registry or network dependency. The daemon-driven (Tier 2, `PACK_TEST_BUILDKIT_ENABLED`) and registry (Tier 3, `PACK_TEST_REGISTRY_ENABLED` + `PACK_TEST_REGISTRY_REF`) integration tests are env-var gated and **skipped by default**. See the readme's Testing section for the full env-var list and the optional ephemeral-registry-on-shared-network recipe.
