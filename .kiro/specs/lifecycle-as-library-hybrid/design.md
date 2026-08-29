# Design: Lifecycle-as-Library Hybrid Build

## Overview

A new multi-platform build backend for `jericop/pack` that splits the CNB
lifecycle by trust boundary:

- **BuildKit (sandbox):** run `detector` + `builder` as `RUN` statements (the
  phases that execute untrusted buildpack code). BuildKit layer caching applies.
- **Host (pack + lifecycle library):** run `analyzer` + `exporter` as Go calls
  into `github.com/buildpacks/lifecycle/phase`. Pack owns image pull/read/push
  natively via imgutil / go-containerregistry / daemon.

No lifecycle `-layout` / OCI-layout export. No `-pull-run-image`. No nested-layout
filesystem exports. The run image is a host-constructed `imgutil.Image`.

This is validated locally and compared against the existing backends. Detector
and builder stay as `RUN`s to keep the MVP simple.

## Why this architecture

The OCI-layout backend forced trusted, I/O-heavy work (pull run image, read/write
layout dirs, push) INTO the sandbox, causing every friction point we hit:
host-registry unreachable from the builder, unprivileged chown, network pulls
opaque to caching, nested-layout wrapper bugs, and run-image materialization. The
lifecycle already exposes `phase.Analyzer`, `phase.Exporter`, `phase.Rebaser` as
library types keyed on `imgutil.Image` (evidence: `phase/exporter.go`
`Export(ExportOptions{WorkingImage imgutil.Image, ...})`; and pack already uses
`phase.Rebaser` this way in `pkg/client/rebase.go`). So the trusted image work can
move host-side with tooling pack already has.

## Established precedent in pack

- `pkg/client/rebase.go` imports `github.com/buildpacks/lifecycle/phase` and calls
  `phase.Rebaser.Rebase(workingImage, newBaseImage, ...)` with pack-constructed
  `imgutil.Image` handles (via `pkg/image`).
- Pack already imports lifecycle `api`, `buildpack`, `launch`, `platform`,
  `platform/files`, `layers`, `auth`, and `imgutil`.
- Pack's non-BuildKit build path (`internal/build/`, `pkg/client/build.go`)
  already constructs images and drives lifecycle phases.

So the dependency and patterns exist; this spec extends them to analyze + export
for the BuildKit multi-platform path.

## High-level flow

```
pack build --buildkit --build-backend=buildkit-hybrid --platforms ... [--publish]
        │
        ▼
Host: pack resolves builder + run image (imgutil.Image), buildpack order, auth
        │
        ▼  per platform (BuildKit multi-platform solve)
┌───────────────────────────────────────────────────────────┐
│ BuildKit RUN graph (sandbox):                               │
│   base = builder image                                      │
│   COPY app -> /workspace                                    │
│   RUN /cnb/lifecycle/detector -> /layers/group.toml, plan   │
│   RUN /cnb/lifecycle/builder  -> /layers/<bp> outputs, SBOM │
│   (cache mounts for buildpack caches; NO analyzer/exporter) │
│ Export the /layers + /workspace to the host (filesync dir)  │
└───────────────────┬───────────────────────────────────────┘
                    │  build outputs on host (per platform)
                    ▼
Host (pack + lifecycle library), per platform:
   analyzed = phase.Analyzer{...}.Analyze()      # uses host-resolved run/prev image
   workingImage = imgutil.New(base = run image)  # pack constructs (registry/daemon)
   report = phase.Exporter{LayerFactory,...}.Export(ExportOptions{
              WorkingImage: workingImage,
              AppDir: <extracted /workspace>,
              LayersDir: <extracted /layers>,
              OrigMetadata: analyzed.LayersMetadata,
              RunImageForExport: ..., RunImageRef: ...,
              LauncherConfig: ..., ...})
   # Exporter assembles app image (run-image base + buildpack layers + launcher),
   # sets io.buildpacks.lifecycle.metadata, and saves/pushes host-side
        │
        ▼
Host: assemble ONE manifest list from the per-arch app images (go-containerregistry
      remote.WriteIndex), push under the final ref. No intermediate tags.
```

## Component design

### 1. New backend `buildkit-hybrid` (pack)
Add a `--build-backend=buildkit-hybrid` value alongside `buildkit-dockerfile` and
`buildkit-llb`. It implements the multi-platform builder interface but:
- builds an LLB graph containing ONLY setup + detector + builder RUNs (no
  analyzer/exporter, no `-layout`),
- exports `/layers` + `/workspace` to a host dir per platform (ExporterLocal /
  filesync dir — the one export type that reliably lands files on the host),
- then runs analyze + export host-side via the lifecycle library.

### 2. Detector + builder RUNs (sandbox)
Reuse the existing phase-arg construction MINUS the OCI-layout/skip-chown/analyzer
bits not needed here. Structure RUNs for cache-friendliness (Requirement 4):
stable args, buildpack cache via cache mounts, avoid `IgnoreCache` unless needed.
detector writes `group.toml`/`plan.toml`; builder writes buildpack layers + SBOM
under `/layers`.

### 3. Build-output extraction (BuildKit -> host)
Export the build result filesystem (at least `/layers` and `/workspace`) to a
per-platform host directory. Two candidate mechanisms:
- (a) `ExporterLocal` + `OutputDir` of a scratch state that COPYs `/layers` +
  `/workspace` (proven to land files on host in the LLB backend work), or
- (b) a solve ref + `ReadDir`/`ReadFile` via the gateway client.
Start with (a) for simplicity. Preserve ownership/permissions enough for the
exporter's `LayerFactory` to build correct layers.

### 4. Host-side analyze (pack + lifecycle library)
Construct the run image and previous image as `imgutil.Image` (registry- or
daemon-backed, using pack's `pkg/image`). Build a `phase.Analyzer` (via the
lifecycle factory/handlers) and call `Analyze()` to produce `files.Analyzed`
(run-image reference/target metadata + previous-image layer metadata for reuse).

### 5. Host-side export (pack + lifecycle library)
Construct `WorkingImage` as an `imgutil.Image` based on the run image. For the
multi-arch publish path this MUST be a NON-PUSHED image handle (in-memory or
local OCI layout), so no per-arch intermediate tag is created — see section 6.
Build a `phase.Exporter{Buildpacks, LayerFactory, Logger, PlatformAPI}` and call
`Export(ExportOptions{WorkingImage, AppDir, LayersDir, OrigMetadata,
RunImageForExport, RunImageRef, LauncherConfig, ...})`. The Exporter adds the
buildpack + launcher layers onto the run-image base and writes
`io.buildpacks.lifecycle.metadata` into the handle. The actual REGISTRY write is
done by pack afterward as ONE index push (multi-arch) or one direct push
(single-arch) — the exporter itself does not push per-arch intermediate tags.
(Non-publish/daemon builds load the assembled image into the daemon instead.)

### 6. Multi-arch manifest list (host) — who assembles/pushes, and why NO intermediate tags

This directly answers: does BuildKit hold the per-arch images? who assembles the
manifest list? and do we re-introduce intermediate tags?

- **BuildKit does NOT hold the final per-arch app images.** In this hybrid,
  BuildKit only produces the `/layers` + `/workspace` build outputs (extracted to
  the host). The app image is assembled HOST-SIDE by the lifecycle exporter
  (library call), entirely outside BuildKit's content store. So BuildKit is not
  involved in the manifest list at all.
- **Pack assembles and publishes** the manifest list, host-side, via imgutil +
  go-containerregistry.
- **The per-arch app images are NEVER individually pushed under intermediate
  tags.** This is the crux. Pack constructs each per-arch `WorkingImage` as a
  NON-PUSHED image handle (an in-memory / local-layout `imgutil.Image` /
  go-containerregistry `v1.Image`). `Exporter.Export` assembles into that handle
  WITHOUT a registry push. Then pack composes a `v1.ImageIndex` from the N per-arch
  `v1.Image` objects and does ONE atomic `remote.WriteIndex(finalRef, index)`:
  this uploads the per-arch images' blobs AND the index, with the per-arch images
  referenced ONLY BY DIGEST inside the index — never under a
  `<img>-build-<id>-<arch>` tag.

Why this does NOT re-introduce the intermediate-tag problem (the Dockerfile
backend's flaw): the Dockerfile backend pushed each per-arch image to the registry
under an intermediate TAG (because the in-sandbox lifecycle exporter pushed to a
registry ref, and `docker buildx imagetools` referenced those tags to assemble the
list). Here, the exporter writes to a host-side non-pushed image handle, and the
single `remote.WriteIndex` publishes blobs + index by digest. This is the SAME
host-side, no-intermediate-tags push already PROVEN in the LLB OCI-layout backend
(`PushPerArchLayoutsAsManifestList`); the only difference is the per-arch image
comes from the host-side exporter rather than an on-disk OCI layout.

- **Single-arch**: pack pushes the one per-arch image directly under the final ref
  (still no intermediate tag).
- **Reuse** the existing `PushPerArchLayoutsAsManifestList` / `remote.WriteIndex`
  path, sourced from the host-side per-arch `v1.Image` objects.

VERIFY (design risk): confirm `phase.Exporter.Export` can write to a NON-PUSHED
`imgutil.Image` (in-memory or local OCI layout) — i.e. `Save` respects the image
type and does not force a remote push — and that we can compose a `v1.ImageIndex`
from N such per-arch images for a single `remote.WriteIndex`. pack's existing
non-publish (daemon) export path and the LLB backend's manifest assembly are the
precedents.

## Caching analysis (the point of this design)

- **detect/build in BuildKit**: cacheable vertices. On rebuild with unchanged app
  + builder, these should hit cache (Requirement 4) — this is where the ~100s
  `go build` lives, so this is the big rebuild win vs the OCI-layout backend where
  the whole lifecycle re-ran.
- **run image acquisition**: host-side, content-addressed by pack/imgutil; no
  in-build network pull, no re-materialization.
- **analyze/export**: host-side Go calls; fast, no sandbox overhead.

### Two caching systems interact (detector vs builder)

There are TWO distinct caches in play, at different granularities, and the design
relies on BOTH. Understanding their interaction is central to this backend.

- **BuildKit vertex cache (coarse, per-RUN):** BuildKit hashes a RUN's inputs
  (base state, args, mounted inputs like `/workspace`) and, on a match, reuses the
  RUN's ENTIRE output layer WITHOUT executing it or looking inside. It cannot
  cache work *within* a RUN.
- **CNB lifecycle/buildpack cache (fine, semantic, via the `/cache` mount):** each
  buildpack decides which of its layers are cacheable and reuses/rebuilds
  individual layers based on its own inputs. The lifecycle + buildpacks own this;
  it lives in the `/cache` mount.

How they combine per phase:

- **Detector RUN**: expected to cache well at the BuildKit-vertex level. Its
  inputs (app source, order/group, builder metadata) are file inputs; if
  unchanged, BuildKit reuses the whole detector output. The BuildKit vertex cache
  alone likely suffices here.
- **Builder RUN**: two cases —
  - *Nothing relevant changed*: BuildKit vertex cache HITS → the whole builder RUN
    is skipped and its output reused (fast). The buildpacks' own cache logic does
    not even need to run.
  - *App source (or other inputs) changed*: BuildKit vertex cache MISSES → the
    ENTIRE builder RUN re-executes. Here the CNB `/cache` mount is what provides
    the incremental speedup — buildpacks reuse their cached layers and only
    rebuild what changed. BuildKit cannot cache *inside* the RUN, so for partial
    changes we depend on the CNB cache mount, NOT the BuildKit vertex cache.

Design consequence: for the builder we deliberately rely on the CNB cache mount
for incremental rebuilds (BuildKit can't see inside the RUN), and on the BuildKit
vertex cache only for the fully-unchanged fast path. This is DIFFERENT from the
detector, where BuildKit's vertex cache alone is expected to carry it. Both must
be wired: cache-friendly RUN inputs (for vertex hits) AND a persistent buildpack
`/cache` mount (for in-RUN incrementality).

### Image acquisition is decoupled from copies (invariant)

A core invariant of this backend: the builder image and run image are always
pulled as CONTENT-ADDRESSED image sources, never entangled with a copy vertex.
This is what guarantees they cache-hit after the first build regardless of what
else changed, and what makes a registry-backed remote cache effective.

- **Builder image**: it is the LLB base (`llb.Image(opts.BuilderImage)`). BuildKit
  pulls it as content-addressed layers and caches them. It is NEVER produced by a
  `COPY`/`ExporterLocal`. Its layers are shared across platforms and rebuilds.
- **Run image**: acquired HOST-SIDE as an `imgutil.Image` (registry- or
  daemon-backed) and used as the base of the exporter's `WorkingImage`. It is
  NEVER materialized in full into the build graph (a key departure from the
  OCI-layout backend, which copied/materialized it). It gets the SAME cache-hit
  guarantee as the builder image: on a rebuild with an unchanged run image, it is
  a cache hit and is NOT re-downloaded — both for the host-side exporter base
  (content-addressed via go-containerregistry/imgutil + the local content store)
  and for any `llb.Image(runImage)` source used in the build graph. Because the
  exporter reuses the run image's original blobs by digest, the run-image layers
  are content-addressed and cacheable.
- **If the build graph needs run-image files (narrow copy only)**: some builds may
  need specific files FROM the run image inside the sandbox (e.g. run-image
  metadata / `/cnb/run.toml`). In that case do a NARROW `llb.Copy` of ONLY those
  specific paths from an `llb.Image(runImage)` source — NEVER copy/materialize the
  whole run image. The `llb.Image(runImage)` source remains the cacheable unit
  (its layers cache-hit on rebuild); only the tiny file-selective copy vertex is
  build-specific. This keeps the run image's caching identical to the builder's
  while extracting just what is needed. For the MVP, the exporter runs host-side
  so most run-image consumption needs NO in-sandbox run-image files at all; this
  narrow-copy rule governs the case where a specific file is genuinely required.
- **Build-output extraction MUST be scoped**: the `ExporterLocal`/copy that brings
  `/layers` + `/workspace` to the host MUST NOT include builder or run-image
  layers. Only buildpack outputs cross the copy boundary; the builder/run-image
  layers stay as independent, cacheable image pulls. Violating this (e.g. copying
  a subtree that contains builder/run-image content) would tie those layers to the
  copy vertex and break their independent cacheability — explicitly disallowed.

### Registry-backed BuildKit remote cache (enabled by design)

To let a nearby/shared registry serve commonly-used layers (builder, run image,
buildpack layers) instead of re-downloading from the origin:

- Plumb pack's existing `--buildkit-cache-from` / `--buildkit-cache-to`
  (BuildKit `CacheImports` / `CacheExports`, i.e. `--import-cache` /
  `--export-cache`) through the hybrid backend's solve options for the detect+build
  RUNs. This is already wired in the LLB backend (`parseCacheImports` /
  `parseCacheExports`); the hybrid backend SHALL reuse it, not omit it.
- On a fresh environment (clean CI runner) with a configured registry cache,
  BuildKit imports the cached layers (builder base, buildpack layers) rather than
  pulling from origin — the "nearby registry cache" speedup.
- Because image acquisition is decoupled from copies (above), the builder's layers
  are exactly the kind of content-addressed data a registry cache can serve.
- Host-side run-image pulls can additionally benefit from a local
  daemon/registry mirror configured in the Docker/registry config; pack's imgutil
  pull honors the ambient registry configuration.

Design consequence: `CacheImports`/`CacheExports` are FIRST-CLASS in the hybrid
backend's solve options from day one, not a later add-on, so a team can point at a
shared registry cache without redesign.

### Future exploration (out of scope for MVP): finer-grained builder caching

To make BuildKit cache buildpack work at a finer granularity than one big RUN
(e.g. per-buildpack or per-layer BuildKit cache hits), the builder would need to
be DECOMPOSED into multiple RUNs / LLB operations that mirror what the lifecycle's
CNB cache does semantically. That effectively means teaching BuildKit what the
lifecycle already knows about buildpack layers — a much deeper change (and
potentially reimplementing CNB cache semantics in LLB). We explicitly DO NOT do
this in the MVP: the builder runs as a single RUN using the lifecycle to execute
buildpacks, backed by the CNB `/cache` mount. Finer-grained builder decomposition
is a documented future experiment to evaluate only if the CNB-cache-mount
incremental path proves insufficient.

## Open questions / risks

- **Analyzer construction as a library**: confirm how to build a `phase.Analyzer`
  outside the CLI — which factory/handlers wire the run/previous images. The
  Analyzer struct + `Analyze()` are exported; verify the factory seam
  (`phase/connected_factory.go`, `phase/handlers.go`, `image/handler.go`) accepts
  injected `imgutil.Image` handles.
- **Exporter LayerFactory**: identify the concrete `LayerFactory` implementation
  the lifecycle uses (the `layers` package) and construct it host-side with the
  extracted `/layers` dir.
- **Ownership/permissions of extracted layers**: the exporter builds layers from
  the extracted `/layers`; confirm UID/GID handling host-side matches what the
  lifecycle expects (the skip-chown work was about the sandbox; host-side we
  control this).
- **SBOM / metadata files**: ensure all files the exporter reads (group.toml,
  project-metadata.toml, SBOM under /layers) are extracted.
- **Platform API**: pin the Platform API used by the library calls to match the
  builder's lifecycle.
- **Windows/other**: out of scope for MVP (linux only, amd64+arm64).
- **Lifecycle library gaps**: if `phase.Analyzer`/`Exporter` need inputs only the
  CLI wires today, add a minimal additive lifecycle helper on the
  `lifecycle-as-library-hybrid` branch.

## Testing strategy (MVP, local)

Follow `mvp-build-testing-strategy` steering: build + rebuild
`samples/go/no-imports` to the local registry via the new backend; runnable check
(real layers, CNB labels incl `io.buildpacks.lifecycle.metadata`, launch binary
present); capture cold vs warm durations and compare to the LLB OCI-layout numbers.
Prove rebase parity by comparing run-image base layer digests.

### Testing cleanup: no env-var-gated registry tests — use a local registry

The Option A (oci-layout-tag-elimination) integration tests were gated behind
`PACK_TEST_*` env vars (e.g. `PACK_TEST_REGISTRY_ENABLED`, `PACK_TEST_REGISTRY_REF`,
`PACK_TEST_BUILDKIT_ENABLED`) so they skipped by default and required an operator to
provide a registry. This backend SHALL NOT carry that pattern forward. Instead, use
a LOCALLY-MANAGED registry the same way pack's EXISTING test suite already does
(pack's `testhelpers` spin up a local registry), plus the MVP local build/rebuild
strategy. Concretely:
- Do NOT introduce new `PACK_TEST_*` env-var gates for this backend's validation.
- Where any existing/related tests still use those env-var gates, treat REMOVING
  them (in favor of a local registry like pack's other tests) as a cleanup task.
- MVP validation remains local-first (build the sample, publish to the local
  registry, inspect the artifact) — the removal is about HOW the registry is
  provided (locally, like existing pack tests), not about adding heavyweight tests.

## Design tradeoffs (why host-side export, and its cost)

The central design axis is WHERE the lifecycle exporter runs, because the exporter
must read the build outputs (`/layers` + `/workspace`) wherever it runs:

- **Exporter inside BuildKit (Option A = the existing OCI-layout backend):** the
  layer data NEVER leaves BuildKit — the exporter runs where the data already is,
  and BuildKit handles the image. Best data locality (matters most for large apps).
  BUT it forces trusted, I/O-heavy work into the sandbox, which caused the friction
  we hit: cannot reach a host-local registry from the docker-container builder,
  unprivileged chown, network pulls opaque to caching, OCI-layout wrapper
  complexity, and run-image materialization.
- **Exporter host-side (Option B = THIS spec):** clean separation and native image
  I/O (pull/read/push via imgutil/go-containerregistry), and — importantly — it
  works with a LOCAL registry for testing/CI because the host pushes, not the
  sandboxed builder. The COST: the build outputs (`/layers` + `/workspace`) must be
  EGRESSED from BuildKit to the host so the exporter can assemble the image.

### Data egress cost (known, measured)

Because the exporter runs host-side, `/layers` (+ `/workspace`) is transferred out
of BuildKit per platform. This cost scales with app/dependency layer size:

- Small for Go / Java-native (compiled binary; tens of MB).
- Larger for Node.js / Python (`node_modules`, virtualenvs, pip caches; hundreds
  of MB to GB), where egressing then re-tarring host-side is wasteful and partly
  offsets the "let BuildKit own the heavy work" goal.

This is an ACCEPTED, DELIBERATE tradeoff for the MVP: we relocate the exporter
host-side to escape the sandbox limitations (esp. local-registry testing) and gain
native image I/O, ACCEPTING egress in exchange. It is NOT an oversight. To keep the
comparison honest we MEASURE egress volume + time on a large-dependency app (not
just `go/no-imports`) so the ceiling of this tradeoff is quantified against the
OCI-layout backend.

### There is no free lunch here

You cannot have "the lifecycle library assembles the image" AND "the data never
leaves BuildKit" AND "escape the sandbox" simultaneously: the exporter reads
`/layers` wherever it runs. Keeping assembly in BuildKit (data-local) means the
exporter runs in the sandbox (Option A). Running the exporter host-side (Option B)
means egress. The only way to keep data in BuildKit AND avoid the sandbox friction
is to have BuildKit itself assemble the image NATIVELY (FROM run-image + builder
layers) without the lifecycle exporter — which requires a buildkit-aware lifecycle
and is a SEPARATE, larger experiment (see the `buildkit-native-export` spec /
Option C). This spec (Option B) is the pragmatic, quickly-testable step and the
baseline that Option C would be compared against.

## Explicit non-goals (MVP)

- Not moving detector/builder host-side (they stay sandboxed RUNs).
- Not removing the existing backends.
- Not implementing registry cache import/export tuning (later).
- Not Windows.
