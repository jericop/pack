---
inclusion: auto
---

# BuildKit Multi-Architecture Build Feature

## Overview

This workspace contains a proof-of-concept implementation of BuildKit-based multi-architecture builds for `pack`. The feature lives in `internal/build/multiplatform/` and is gated behind `--buildkit` (requires `pack config experimental true`).

The `buildkit-native-export` branch is dedicated to a SINGLE builder-agnostic build backend called `buildkit`. Earlier proof-of-concept backends (`buildkit-dockerfile`, `buildkit-llb`) and the OCI-layout export mode have been removed. The `BuildBackend` interface, `BackendType` enum, factory, and `--build-backend` flag are intentionally KEPT so a future buildah-podman backend can be added without reworking the abstraction.

> Source of truth: this steering file is the canonical summary of the BuildKit
> multi-arch feature. The comprehensive doc at
> `internal/build/multiplatform/buildkit-multi-arch-readme.md` is kept in sync with
> it — update this file first, then propagate changes to the readme.

## Key Architecture Decisions

### Single Build Backend: `buildkit`

- **`--build-backend buildkit`** (the only value; `auto` resolves to it): BuildKit BUILDS and PUSHES the app image natively (one multi-arch image = one OCI index, NO intermediate per-arch tags, layer data never egresses to intermediate tags); then a lifecycle-owned FINALIZE step authors the correct CNB metadata on the pushed image from its ACTUAL produced layers. See "Build-then-finalize" below.
- Uses the BuildKit Go SDK directly (`github.com/moby/buildkit/client`), connecting to the buildkit daemon via the `docker-container://` scheme; per-arch solves run in parallel. Requires a patched lifecycle with `-skip-chown` (the BuildKit LLB API does not support uid/gid on cache mounts the way the Dockerfile frontend did).
- The abstraction (interface/enum/factory/`--build-backend`) is retained for a future buildah-podman backend, but `buildkit` is the only implemented value today.

### Build-then-finalize

- **Two phases.** (1) BUILD: run the lifecycle phases + assemble `FROM run-image` in BuildKit; BuildKit pushes one image (multi-arch = one OCI index), no intermediate tags; the build attaches a single label `io.buildpacks.lifecycle.prepared-metadata` (the ordered plan + emitted CNB labels) and does NOT write a valid final `io.buildpacks.lifecycle.metadata`. (2) FINALIZE: pack calls the lifecycle `phase/finalize` library (like `phase.Rebaser`), which reads the pushed image's produced diffIDs + the prepared-metadata label, authors `io.buildpacks.lifecycle.metadata` (per-layer SHAs = produced diffIDs; RunImage boundary), removes the prepared-metadata label, and re-pushes config+manifest(+index) only — NO layer changes.
- **Why authoring, not rewriting.** BuildKit's exporter always assigns diffIDs at export; a frontend cannot inject blobs via the gateway result (verified, moby/buildkit v0.32.2). An earlier spike emitted metadata with the INTENDED diffIDs then pack REWROTE the SHAs post-push (a patch). The current design never writes a wrong final label: finalize AUTHORS the metadata against the produced diffIDs the first time, in the lifecycle (one source of truth), not pack.
- **Finalize atomicity + failure.** Finalize is a separate registry op after the push; it re-pushes to the SAME tag via a single manifest/index `PUT` (tag-atomic). On failure the tag still resolves to the pushed (pre-finalize) image — runnable, just not yet rebuildable/rebaseable. Finalize is IDEMPOTENT. A future opt-in `--buildkit-fix-image-metadata` can self-heal an interrupted build's image in place using its retained prepared-metadata label (finalize `KeepPreparedMetadataLabel`). Not implemented.
- **Layer diffIDs differ** from a normal registry-mode build (BuildKit recomputes them). Rebase depends only on the run-image `TopLayer` boundary, and finalize makes per-layer metadata SHAs match the actual layers, so rebase + buildpack-layer patching both work. Validated across REPEATED rebuilds + rebases + rebuild-after-rebase + multi-arch.
- **No frontend; assembly is `llb.Copy` from emitted sources.** The `buildkit/cnbfrontend` package + `cmd/cnb-frontend` are DELETED. Pack drives an IN-PROCESS gateway BuildFunc (`internal/build/multiplatform/native_buildfunc.go`) that assembles `FROM run-image` by `llb.Copy`-ing each CNB layer from its emitted filesystem SOURCE (buildpack layers from `/layers/<bp>/<layer>`, app from `/workspace`, launcher file; chown to CNB uid:gid). Synthesized layers (process-types) are copied from a tiny tree the emit step extracts in Go. NO re-materialization of large layers. emit-mode records per-layer Source refs (lifecycle `layers.Layer.Source` → `emit.LayerOp.Source`). App slices are honored via `llb.Copy` `IncludePatterns` per slice.
- **Run image** resolved from the analyzer-written `/layers/analyzed.toml` (digest-pinned), used as the `llb.Image` base; never modified. A custom `--run-image` and rebase onto a different run image are both supported (finalize authors the correct `runImage.reference` + `topLayer`).
- **Local dev prereqs:** phase RUNs use `llb.Network(HOST)` + `network.host` entitlement; a local registry on the builder's docker network + insecure-registry config; `PACK_HOST_REGISTRY_REMAP` bridges the buildkit-vs-host registry-name split for host-side finalize (test-env only, no-op in prod).

### Registry Authentication

Pack resolves credentials from the Docker keychain (including credential helpers like `docker-credential-desktop`) and passes them as the `CNB_REGISTRY_AUTH` environment variable. This is the same mechanism the lifecycle uses in pack's normal flow. No secret file mounts needed. A Docker auth session (`authprovider.NewDockerAuthProvider`) is also attached for BuildKit's own operations (pulling images, pushing/pulling registry cache).

### Buildpack Order Injection

The builder's buildpack order is extracted from its `io.buildpacks.buildpack.order` label and written to `/cnb/order.toml`. This ensures consistent detection behavior across platforms regardless of the builder image's default order.

## File Structure

```
internal/build/multiplatform/
├── backend.go                    # BuildBackend interface, BackendType enum (BackendBuildkit/BackendAuto), Platform
├── backend_factory.go            # NewBackend() factory -> NewBuildkitBackend
├── backend_native.go             # BuildkitBackend: drives the BuildKit build+push, then calls the lifecycle finalize.Finalize library post-push
├── buildkit_client.go            # Shared BuildKit helpers (connect, resolve addr, progress display, cache import/export parsing, docker auth provider)
├── native_buildfunc.go           # In-process gateway BuildFunc: assembles FROM run-image via llb.Copy of each emitted CNB layer source
├── buildkit-multi-arch-readme.md # Comprehensive documentation
├── metadata_rewrite.go           # Test-env applyHostRegistryRemap shim only (the rewrite logic moved to the lifecycle phase/finalize library)
├── emit_contract.go              # Parser for the lifecycle emit contract (plan.json/config.json)
├── executor.go                   # MultiPlatformExecutor orchestration (skips own assembly when the backend PushesNatively)
└── *_test.go / *_internal_test.go  # Unit tests (backend assertions, slice seam, executor)
```

The Dockerfile backend, LLB backend, Dockerfile generator, and all `oci_layout_*.go` files have been DELETED.

## CLI Flags

| Flag | Description |
|------|-------------|
| `--buildkit` | Enable BuildKit backend (experimental) |
| `--platforms` | Comma-separated platforms (e.g., `linux/amd64,linux/arm64`) |
| `--build-backend` | `auto` (default) or `buildkit` — the only implemented backend; the flag is retained for a future buildah-podman backend |
| `--buildkit-builder` | Name of the buildx (docker-container) builder |
| `--buildkit-cache-from` | Registry cache source (`type=registry,ref=...`) |
| `--buildkit-cache-to` | Registry cache destination (`type=registry,ref=...,mode=max`) |
| `--buildkit-fix-image-metadata` | (PLANNED, not implemented) if the target image already exists remotely with invalid/stale CNB metadata, fix it in place before building (self-healing). Default off = warn and stop. |
| `--lifecycle-image` | Emit-capable lifecycle image (only needed when not using a builder that already bundles it) |

## Related Repositories

All three repos share the `buildkit-native-export` branch name and the `buildkit-native-export-v0.1.0` tag scheme.

- **jericop/cnb-lifecycle** (`buildkit-native-export` branch): Exporter emit-mode computing the plan + prepared-metadata label + per-layer Source refs (`phase/emit`, `layers/`) and the FINALIZE library authoring CNB metadata from produced diffIDs (`phase/finalize`), consumed by pack like `phase.Rebaser`. Keeps `-skip-chown`; `-pull-run-image` removed. The `buildkit/cnbfrontend` package + `cmd/cnb-frontend` are DELETED (assembly is now pack-side `llb.Copy`). Published multi-arch image `jericop/lifecycle:buildkit-native-export-v0.1.0` (from the same-named git tag). Pack consumes it as a library via a `replace` directive pinned to that tag.
- **jericop/cnb-pack** (`buildkit-native-export` branch): The single `buildkit` backend — in-process BuildFunc (`native_buildfunc.go`) assembling via `llb.Copy` + `finalize.Finalize` post-push (`backend_native.go`, `executor.go`, `metadata_rewrite.go` test-env shim only). `go.mod` `replace`s the lifecycle to the `buildkit-native-export-v0.1.0` tag (pseudo-version).
- **jericop/ubuntu-noble-builder** (`buildkit-native-export` branch): Builder that bundles the pinned lifecycle via `builders/builder/builder.toml` `[lifecycle].uri = docker://docker.io/jericop/lifecycle:buildkit-native-export-v0.1.0`. Published multi-arch as `jericop/ubuntu-noble-builder:buildkit-native-export`.
- **jericop/pr-compliance-app**: CI testing workflow
- **jericop/cnb-rfcs** (`buildkit-mutliarch-build` branch): RFC document

## Testing

```bash
# Single buildkit backend, multi-arch, against the published builder that bundles
# the pinned lifecycle. See the readme for the full local-dev prereqs
# (network.host entitlement, insecure registry, PACK_HOST_REGISTRY_REMAP).
pack build pack-local-registry:5000/myapp:latest \
  --path ./app \
  --builder jericop/ubuntu-noble-builder:buildkit-native-export \
  --run-image paketobuildpacks/ubuntu-noble-run:latest \
  --platforms linux/amd64,linux/arm64 \
  --buildkit --build-backend buildkit \
  --buildkit-builder pack-multiplatform \
  --publish --trust-builder
```

Verification should cover REPEATED cycles (≥2 rebuilds, ≥2 rebases,
rebuild-after-rebase), not just the first build — to confirm finalize re-authors
the CNB metadata correctly each rebuild and that rebase re-points the run-image
boundary. Registry cache import/export (`--buildkit-cache-from`/`--buildkit-cache-to`
`type=registry`) and app slices are also validated.

## Known Issues

- Requires a patched lifecycle with `-skip-chown` (the BuildKit LLB API doesn't support uid/gid on cache mounts). It is bundled in `jericop/ubuntu-noble-builder:buildkit-native-export`.
- `pack builder create` with a `docker://` lifecycle URI requires the pack fork (stable pack doesn't support it).
- Layer diffIDs differ from a normal registry-mode build (BuildKit recomputes them); the post-push FINALIZE step is REQUIRED for rebuild/rebase (it authors the CNB metadata from the produced diffIDs). If finalize fails, the pushed image is runnable but not rebuildable/rebaseable until finalized (re-run the build, or the PLANNED self-healing `--buildkit-fix-image-metadata`).
- Local ephemeral registries are only reachable from the `docker-container` builder if it was created on a shared docker network (reference the registry by container name from inside the builder, e.g. `pack-local-registry:5000`, and remap to the host `localhost:5050` via `PACK_HOST_REGISTRY_REMAP` for host-side finalize).
