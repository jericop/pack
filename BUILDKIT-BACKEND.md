# BuildKit Multi-Architecture Build Backend for Pack

Quick-reference technical guide for this fork's BuildKit build backend. (This is a
local mirror of the design captured in the RFC at
`jericop/cnb-rfcs` `text/0000-buildkit-multiarch-build.md`; kept at the repo root so
it is easy to find.)

## Overview

This fork adds multi-architecture container image building to `pack` using
BuildKit. Instead of running lifecycle phases as individual Docker containers
(pack's default `docker-daemon` backend), the `buildkit` backend runs the lifecycle
inside BuildKit and produces one multi-arch app image (an OCI index) via QEMU
emulation and/or native runners. The implementation lives in
`internal/build/multiplatform/`.

The `buildkit-native-export` branch is dedicated to a SINGLE builder-agnostic
build backend named `buildkit` (build-then-finalize). Earlier proof-of-concept
backends (`buildkit-dockerfile`, `buildkit-llb`) and the OCI-layout export mode
have been removed. The `BuildBackend` interface, `BackendType` enum, factory, and
`--build-backend` flag are intentionally KEPT so a future buildah backend
can be added without reworking the abstraction. (The backend is named `buildah`;
podman is the sibling container tool in the same ecosystem, but the build library
is buildah.)

> The canonical internal summary lives in the `buildkit-multiarch` steering file
> (`.kiro/steering/buildkit-multiarch.md`). This root-level doc is the expanded
> technical reference. For a hands-on user walkthrough (consume the published
> images, build a multi-arch app, make your own builder, run it in CI), see
> [internal/build/multiplatform/TRY-IT-OUT.md](./internal/build/multiplatform/TRY-IT-OUT.md).

## How It Works: build-then-finalize

The `buildkit` backend has two phases:

1. **Build phase (BuildKit).** Run the lifecycle phases (analyzer → detector →
   restorer → builder → exporter in emit-mode), assemble the app image
   `FROM run-image` via native `llb.Copy` FileOps, and let BuildKit push ONE image
   (multi-arch = one OCI index) with **no intermediate per-arch tags**. Layer data
   never egresses to the host. The build attaches a single build-phase label,
   `io.buildpacks.lifecycle.prepared-metadata` (the ordered layer plan + the CNB
   labels the lifecycle computed). The image is runnable but does NOT yet carry a
   valid final `io.buildpacks.lifecycle.metadata` (its intended per-layer SHAs are
   not the diffIDs BuildKit assigns at export).
2. **Finalize phase (lifecycle library, pack calls it).** After the push, pack
   calls the lifecycle `phase/finalize` library (imported like `phase.Rebaser`).
   Finalize reads the pushed image's ACTUAL produced layer diffIDs + the
   prepared-metadata label, authors `io.buildpacks.lifecycle.metadata` (per-layer
   SHAs = produced diffIDs; `RunImage.TopLayer` = the run-image boundary), removes
   the prepared-metadata label, and re-pushes ONLY the image config + manifest
   (+ index for multi-arch). NO layer blobs are read, added, re-tarred, or
   re-uploaded.

### Why authoring, not rewriting

BuildKit's image exporter always derives the final layer diffIDs from what it
actually snapshots at export; a frontend cannot inject pre-built blobs/diffIDs via
the gateway result (verified in moby/buildkit v0.32.2). An earlier spike let the
lifecycle emit metadata with the INTENDED diffIDs, then pack REWROTE those SHAs to
the produced ones after push — a patch of a divergence we caused. The current
design never writes a wrong final label: the build carries the plan in the
prepared-metadata label, and finalize AUTHORS the final metadata against the
produced diffIDs the first time, in the lifecycle (one source of truth), not pack.

### Assembly is native `llb.Copy` (no frontend, no run-image shell/tar)

The build assembles `FROM run-image` by, per CNB layer, `llb.Copy`-ing the layer's
files from the emitted filesystem SOURCE (buildpack layers from
`/layers/<bp>/<layer>`, app from `/workspace`, launcher file) with chown to the CNB
uid:gid — a native BuildKit FileOp. It does NOT run `tar`/shell on the run image, so
distroless/static run images work; the run image is never modified. Large layers are
copied by reference from their existing built-state paths (not re-materialized); only
the tiny synthesized process-types layer is extracted (in Go, in emit-mode) into a
small tree that is copied. App slices are honored via per-slice `IncludePatterns` on
the `llb.Copy`. Per-layer `llb.Copy` also gives per-layer cache reuse on rebuild.

There is no custom BuildKit frontend — the `buildkit/cnbfrontend` package and
`cmd/cnb-frontend` have been deleted. Pack drives an in-process gateway BuildFunc
(`native_buildfunc.go`).

## Usage

### Basic multi-architecture build

```bash
pack build registry.example.com/myapp:latest \
  --path ./app \
  --builder jericop/ubuntu-noble-builder:buildkit-native-export \
  --run-image paketobuildpacks/ubuntu-noble-run:latest \
  --platform linux/amd64 --platform linux/arm64 \
  --build-backend buildkit \
  --buildkit-builder pack-multiplatform \
  --publish --trust-builder
```

The recommended builder is `jericop/ubuntu-noble-builder:buildkit-native-export`,
which bundles the matching pinned lifecycle
(`jericop/lifecycle:buildkit-native-export-v0.1.0`) via its `builder.toml`
`[lifecycle].uri`. When using a builder that does not already bundle an
emit/finalize-capable lifecycle, pass `--lifecycle-image` to supply one.

### With registry cache (CI / ephemeral builders)

```bash
pack build registry.example.com/myapp:latest \
  --path ./app \
  --builder jericop/ubuntu-noble-builder:buildkit-native-export \
  --platform linux/amd64 --platform linux/arm64 \
  --build-backend buildkit \
  --buildkit-builder pack-multiplatform \
  --buildkit-cache-from type=registry,ref=registry.example.com/myapp-cache:latest \
  --buildkit-cache-to type=registry,ref=registry.example.com/myapp-cache:latest,mode=max \
  --publish --trust-builder
```

Validated locally: a cold build exporting `type=registry` cache, then a build on a
pruned builder importing that cache, reuses the cached vertices.

## Prerequisites

- Docker with BuildKit support (Docker Engine 23.0+)
- Pack experimental mode enabled: `pack config experimental true`
- A `docker-container` buildx builder:
  `docker buildx create --name pack-multiplatform --driver docker-container --bootstrap`
- QEMU user-mode emulation registered (default on Docker Desktop; on Linux:
  `docker run --privileged --rm tonistiigi/binfmt --install all`)
- A patched lifecycle with `-skip-chown` (bundled in the recommended builder)
- `--publish` (multi-arch images cannot be loaded to a local Docker daemon)

## Registry Authentication

Pack resolves credentials from the Docker keychain (including credential helpers
like `docker-credential-desktop`) and passes them via `CNB_REGISTRY_AUTH`. A Docker
auth session provider (`authprovider.NewDockerAuthProvider`) is also attached for
BuildKit's own operations (pulling images, registry cache, the native push). The
host-side finalize step reuses the same pack-resolved credentials.

## Caching

- **BuildKit layer cache** caches each vertex; unchanged setup and per-layer copies
  are cache hits on rebuild.
- **Lifecycle buildpack cache** persists across builds via a BuildKit cache mount,
  scoped per-architecture, so buildpacks reuse dependency layers even when source
  changes.
- **Registry cache** (`--buildkit-cache-from`/`--buildkit-cache-to type=registry`)
  lets ephemeral CI builders import/export the build cache.
- `--clear-cache` disables the BuildKit layer cache and omits the lifecycle cache
  mount for a fully fresh build.

## Flags Reference

| Flag | Description |
|------|-------------|
| `--build-backend` | Build backend. `docker-daemon` (default; standard single-arch daemon build), `buildkit` (native multi-arch, experimental), or `auto` (resolves to `docker-daemon`). `buildah` planned. |
| `--platform` | Target platform; repeatable (or comma-separated), e.g. `--platform linux/amd64 --platform linux/arm64`. `docker-daemon` accepts one; `buildkit` accepts many. Defaults to the host platform when omitted. |
| `--buildkit-builder` | Name of the buildx (docker-container) builder |
| `--buildkit-cache-from` | External registry cache source (`type=registry,ref=...`) |
| `--buildkit-cache-to` | External registry cache destination (`type=registry,ref=...,mode=max`) |
| `--lifecycle-image` | Emit/finalize-capable lifecycle image (only needed when the builder does not already bundle one) |
| `--fix-image-metadata` | Self-healing: do NOT build; run finalize in place on the EXISTING pushed image (the image-name arg) from its retained prepared-metadata label. Idempotent. Standalone counterpart: `pack image-metadata fix`. |

## Caveats

### Finalize is required; on failure the image is runnable but not yet compliant

Finalize is a SEPARATE registry operation after BuildKit's push (not one
transaction). It is TAG-ATOMIC and FAILURE-SAFE: it re-pushes to the SAME tag via a
single manifest `PUT` (index `PUT` for a manifest list), so the tag always resolves
to either the pre-finalize or the finalized image, never a partial one. If finalize
fails before its final `PUT`, the pushed image remains **runnable** (only its CNB
metadata is not yet authored), so it cannot be cleanly rebuilt or rebased until
finalized. Finalize is IDEMPOTENT — re-running it (or the next build) authors
identical metadata. Two opt-in self-heal entry points finalize a
previously-interrupted image in place using its retained prepared-metadata label
(finalize's `KeepPreparedMetadataLabel` option): the build-time
`pack build --fix-image-metadata <image>` (no-build short-circuit) and the
standalone `pack image-metadata fix <image>`.

### Layer diffIDs differ from a normal registry-mode export

By design the app/buildpack layer diffIDs differ from a normal container-per-phase
registry export of the same build (BuildKit recomputes them). Rebase depends only on
the run-image boundary (`RunImage.TopLayer`), and finalize makes every per-layer
metadata SHA equal the ACTUAL produced diffID, so rebuild, rebase, and
buildpack-layer patching all work.

## Local validation results (samples/go/no-imports)

Validated end-to-end against a local `registry:2` with the patched builder +
lifecycle, covering REPEATED cycles (not just the first build):

- COLD build → runnable image; finalize authored the CNB metadata (new-layer diffID
  remaps); the final tag is created directly (no intermediate `-arch` tag).
- REBUILD → the analyzer's previous-image restore succeeds; after each rebuild all
  per-layer metadata SHAs equal the image's actual diffIDs.
- REBASE (including onto a different custom run image) → `pack rebase` succeeds; only
  the run base swaps, app/bp/launcher/config/process-types layer SHAs unchanged;
  `runImage.reference`/`topLayer` re-pointed correctly.
- REBUILD after REBASE → succeeds.
- REMOTE BUILDKIT CACHE → export `type=registry` then import on a pruned builder.
- APP SLICES → verified at the producer (lifecycle `SliceLayers`) and consumer
  (pack `copyFromSource` → `llb.Copy` `IncludePatterns`) seams; an end-to-end slice
  test needs a custom buildpack that writes `launch.toml [[slices]]` (documented gap).
- MULTI-ARCH (linux/amd64 + linux/arm64) → ONE OCI index, both children finalized
  (prepared-metadata label removed, valid `io.buildpacks.lifecycle.metadata`, correct
  per-arch `RunImage.TopLayer`), no intermediate tags.
- The e2e chain was re-verified against the PUBLISHED builder
  (`jericop/ubuntu-noble-builder:buildkit-native-export`, which pulls the pinned
  lifecycle from Docker Hub).

## Local test-environment prerequisites

The lifecycle phase `RUN`s must reach the registry, and the assembly needs host
networking, for local validation:

- Run a local `registry:2` on the buildx builder's docker network so in-build `RUN`s
  reach it by name (e.g. `pack-local-registry:5000`), while the host reaches it via a
  published port (e.g. `localhost:5050`).
- Create the builder with `--allow-insecure-entitlement network.host` + a buildkitd
  insecure-registry config for the local registry; the lifecycle phase `RUN`s use
  `llb.Network(NetMode_HOST)` and pack requests the `network.host` entitlement.
- The host-vs-buildkit registry name difference (BuildKit pushes to
  `pack-local-registry:5000`; the host-side finalize must use `localhost:5050`) is
  bridged by `PACK_HOST_REGISTRY_REMAP` (`src=dst`). Local test-env only; no-op in
  production.

See `.kiro/steering/local-test-environment.md` for the full recipe.

## Implemented self-heal (was future work)

Self-heal is shipped via two idempotent, opt-in entry points that finalize an
already-pushed-but-not-finalized image in place (config+manifest only, no layer
egress) using its retained prepared-metadata label: the build-time
`pack build --fix-image-metadata <image>` and the standalone
`pack image-metadata fix <image>` (which also has `inspect` + `verify` siblings).

## Future Work

- **buildah backend**: add a second `BuildBackend` implementation (the abstraction
  and `--build-backend` flag are retained for this; `buildah` would be a new
  `--build-backend` value).

## Related Repositories

All three repos share the `buildkit-native-export` branch name and the
`buildkit-native-export-v0.1.0` tag scheme.

### jericop/cnb-pack (branch: `buildkit-native-export`)

Fork of `buildpacks/pack` with the single `buildkit` backend
(`internal/build/multiplatform/`: `backend.go`, `backend_factory.go`,
`backend_native.go`, `buildkit_client.go`, `native_buildfunc.go`, `executor.go`,
`metadata_rewrite.go` test-env shim). `go.mod` `replace`s the lifecycle to the
`buildkit-native-export-v0.1.0` tag (Go pseudo-version).

### jericop/cnb-lifecycle (branch: `buildkit-native-export`)

Fork of `buildpacks/lifecycle` with `-skip-chown`, the exporter emit-mode
(`phase/emit`, `layers/`) recording the ordered plan + prepared-metadata label +
per-layer Source refs, and the FINALIZE library (`phase/finalize`) authoring CNB
metadata from produced diffIDs. Published multi-arch image
`jericop/lifecycle:buildkit-native-export-v0.1.0`.

### jericop/ubuntu-noble-builder (branch: `buildkit-native-export`)

Builder that bundles the pinned lifecycle via `builder.toml`
`[lifecycle].uri = docker://docker.io/jericop/lifecycle:buildkit-native-export-v0.1.0`.
Published multi-arch as `jericop/ubuntu-noble-builder:buildkit-native-export`.

### jericop/pr-compliance-app

A Go application used as a CI test subject for multi-architecture buildpacks builds.
