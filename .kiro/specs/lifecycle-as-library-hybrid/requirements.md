# Requirements: Lifecycle-as-Library Hybrid Build

## Introduction

This spec explores a fundamentally different architecture for multi-arch (and
single-arch) BuildKit builds in the `jericop/pack` fork. Instead of running the
entire CNB lifecycle as a CLI inside BuildKit `RUN` steps and exporting via the
OCI-layout two-phase dance, we split responsibilities by trust boundary:

- **BuildKit runs the untrusted, sandboxed work** — the `detector` and `builder`
  lifecycle phases (which execute buildpack code) run as `RUN` statements in the
  build graph, so BuildKit's layer caching applies to them.
- **Pack + the lifecycle-as-a-LIBRARY do the trusted image work host-side** — the
  `analyzer` and `exporter` phases are invoked as Go library calls
  (`phase.Analyzer.Analyze`, `phase.Exporter.Export(ExportOptions{WorkingImage
  imgutil.Image, ...})`). Pack owns image pulling, layer/blob access, and pushing
  NATIVELY via imgutil / go-containerregistry / the daemon — the same tooling
  pack already uses in its non-BuildKit build path and in `pkg/client/rebase.go`
  (which already calls `phase.Rebaser` with pack-constructed `imgutil.Image`).

Goals:
- **Avoid the OCI-layout export mechanism entirely** — it is inefficient (nested
  layouts, filesystem exports, run-image materialization) and was a workaround for
  running export inside the sandbox.
- **Maximize BuildKit caching** in all areas that run in BuildKit (detect/build),
  and move image I/O host-side where it is content-addressed and cache-friendly.
- **Reuse the lifecycle reference implementation** for all buildpacks-specific
  logic (detect, build, analyze metadata, export assembly, rebase boundary) — no
  reimplementation of CNB semantics in pack.
- **Deliver a locally-testable MVP** that can be compared against the existing
  approaches (registry-mode Dockerfile backend, LLB OCI-layout backend).

Scope note: this is a NEW, alternative backend/architecture. It does not remove
the existing backends; it exists to be compared. Detector and builder stay as
`RUN` statements using the lifecycle binary to keep the MVP simple.

Key advantage over Option A (lifecycle export inside BuildKit / the OCI-layout
backend): because pack pushes host-side, this approach WORKS WITH A LOCAL REGISTRY
for testing and CI (the docker-container builder cannot reach a host-local registry
when it pushes from inside the sandbox — a limitation we hit with Option A). Known
cost: build outputs must be egressed from BuildKit to the host so the host-side
exporter can assemble the image; this scales with app layer size (small for
Go/Java, larger for Node/Python). See design "Design tradeoffs". A future approach
that keeps assembly in BuildKit natively (no egress) is captured separately as the
`buildkit-native-export` spec (Option C).

Related work in matching branches: `jericop/cnb-lifecycle@lifecycle-as-library-hybrid`
(any lifecycle library-surface changes) and
`jericop/cnb-pack@lifecycle-as-library-hybrid` (this spec + pack changes).

## Glossary

- **Lifecycle-as-library**: importing `github.com/buildpacks/lifecycle/phase` and
  calling `Analyzer.Analyze` / `Exporter.Export` / `Rebaser.Rebase` directly,
  rather than exec-ing `/cnb/lifecycle/*`.
- **`imgutil.Image`**: the image handle interface the lifecycle's Export/Rebase
  APIs accept; pack constructs registry- or daemon-backed instances.
- **Trusted vs untrusted phases**: detect/build run buildpack code (untrusted →
  sandbox); analyze/export are metadata + image assembly (trusted → host-side).
- **Build outputs**: the `/layers` (buildpack layers, metadata, SBOM) and
  `/workspace` (app) produced by the detect+build RUN, which pack extracts and
  feeds to the host-side exporter.
- **Hybrid backend**: the new `pack` multi-platform backend implementing this
  split.

## Requirements

### Requirement 1: Split the lifecycle by trust boundary

**User Story:** As a pack maintainer, I want buildpack-executing phases isolated
in BuildKit and image-assembly phases run host-side, so that each runs where it
belongs and BuildKit caching applies to the heavy build work.

#### Acceptance Criteria

1. THE detector and builder phases SHALL run inside BuildKit as `RUN` statements
   using the lifecycle binary from the builder image.
2. THE analyzer and exporter phases SHALL run host-side as Go library calls into
   `github.com/buildpacks/lifecycle/phase`, not as CLI/exec or in-sandbox RUNs.
3. THE build SHALL NOT use the lifecycle `-layout` / OCI-layout export mechanism.

### Requirement 2: Pack owns image I/O natively

**User Story:** As a pack maintainer, I want pack to pull, read, and push images
using native Go tooling, so that image I/O is content-addressed, cacheable, and
not constrained by the BuildKit sandbox.

#### Acceptance Criteria

1. WHEN the exporter needs a run image THE run image SHALL be provided as an
   `imgutil.Image` handle constructed host-side by pack (registry- or
   daemon-backed), NOT pulled inside a BuildKit RUN.
2. WHEN the final app image is assembled THE assembly SHALL use the lifecycle
   `Exporter.Export(ExportOptions{WorkingImage: <pack-constructed imgutil.Image>,
   ...})` API.
3. WHEN publishing THE app image push SHALL be performed host-side (imgutil /
   go-containerregistry), consistent with pack's existing publish path.
4. THE run image's original layer blobs and diffIDs SHALL be preserved so rebase
   continues to work.

### Requirement 3: Extract build outputs from BuildKit to the host

**User Story:** As a pack maintainer, I want the buildpack layer outputs produced
in BuildKit made available to the host-side exporter, so that the exporter can
assemble the final image.

#### Acceptance Criteria

1. WHEN the detect+build RUN completes THE resulting `/layers` and `/workspace`
   (and required metadata: group.toml, plan.toml, project-metadata, SBOM) SHALL
   be made available to the host (e.g. via a BuildKit filesystem export or
   equivalent) for the exporter inputs (`LayersDir`, `AppDir`, `OrigMetadata`).
2. THE extraction SHALL preserve file ownership/permissions sufficient for the
   exporter to construct correct layers.

### Requirement 4: Maximize BuildKit caching for the sandboxed work

**User Story:** As a developer, I want fast rebuilds, so that an unchanged app
does not re-run the expensive build.

Note: two caches interact (see design "Two caching systems interact"): BuildKit's
coarse per-RUN vertex cache, and the fine-grained CNB/buildpack cache in the
`/cache` mount. The detector is expected to cache at the BuildKit-vertex level;
the builder relies on the vertex cache for the fully-unchanged fast path and on
the CNB `/cache` mount for incremental rebuilds when inputs change (BuildKit
cannot cache inside a RUN).

#### Acceptance Criteria

1. THE detect+build RUN steps SHALL be structured so BuildKit vertex caching
   applies on rebuild when inputs are unchanged (avoid unnecessary `IgnoreCache`,
   use stable inputs).
2. THE builder RUN SHALL use a persistent buildpack `/cache` mount so that, when
   the BuildKit vertex cache misses due to changed inputs, buildpacks reuse their
   cached layers (in-RUN incrementality) rather than rebuilding from scratch.
3. WHEN the app source and builder are unchanged on a rebuild THE build SHALL
   demonstrate BuildKit vertex cache hits on the detector and builder RUNs
   (measured via the local two-build comparison).
4. WHEN the app source changes on a rebuild THE builder SHALL demonstrate
   incremental reuse via the CNB cache mount (buildpack layer reuse), even though
   the BuildKit builder vertex re-executes.

### Requirement 4a (future, non-MVP): finer-grained builder caching

**User Story:** As a maintainer, I want to record the option to make BuildKit
cache buildpack work at a finer granularity than one RUN, so that it can be
evaluated later.

#### Acceptance Criteria

1. THE MVP SHALL run the builder as a SINGLE RUN using the lifecycle to execute
   buildpacks (no decomposition).
2. THE design SHALL document (not implement) the future option of decomposing the
   builder into multiple RUNs / LLB operations to obtain per-buildpack or
   per-layer BuildKit cache hits, to be evaluated only if the CNB-cache-mount
   incremental path proves insufficient.

### Requirement 4b: Image acquisition decoupled from copies; registry-backed remote cache

**User Story:** As a developer, I want the builder and run image to be pulled as
content-addressed image sources that always cache-hit after the first build, and
I want to be able to point BuildKit at a nearby registry cache, so that repeated
builds and shared-team environments do not re-download large common layers.

#### Acceptance Criteria

1. THE builder image SHALL be brought into the build ONLY as a content-addressed
   image source (`llb.Image(builder)` base), NEVER as the result of a `COPY` /
   filesystem-export operation, so its pull is an independent, cacheable step.
2. THE run image SHALL be acquired as a content-addressed image (host-side
   imgutil / go-containerregistry, and — only if the build graph needs run-image
   files — an `llb.Image(runImage)` source), NEVER materialized in full via a
   `COPY` / `ExporterLocal` / OCI-layout copy, so its acquisition is independent
   of any copy vertex.
3. WHEN the builder image is unchanged on a rebuild THE build SHALL NOT
   re-download it (cache hit), independent of whether app source or other build
   inputs changed.
3a. WHEN the run image is unchanged on a rebuild THE run image SHALL be a cache
   hit (NOT re-downloaded) — the SAME guarantee as the builder image — both for
   the host-side exporter base and for any `llb.Image(runImage)` source used in
   the build graph, independent of whether app source or other build inputs
   changed.
3b. IF the build graph needs specific files from the run image (e.g. run-image
   metadata / run.toml) THE build SHALL perform a NARROW `llb.Copy` of ONLY those
   specific paths from an `llb.Image(runImage)` source, NEVER a copy/materialization
   of the entire run image. The `llb.Image(runImage)` source SHALL remain the
   cacheable unit; only the small file-selective copy vertex is build-specific.
4. THE build-output extraction (Requirement 3) SHALL capture ONLY the buildpack
   outputs (`/layers`, `/workspace`, metadata) and SHALL NOT pull builder or
   run-image layers into a copy, so those images' layers retain their independent
   cacheability.
5. THE backend SHALL support a registry-backed BuildKit remote cache
   (import/export) via pack's `--buildkit-cache-from` / `--buildkit-cache-to`
   (BuildKit `--import-cache` / `--export-cache`), so a nearby/shared registry can
   serve commonly-used layers (builder, run image, buildpack layers). This SHALL
   be enabled by design, not retrofitted.
6. WHERE a registry cache is configured THE builder/run-image/buildpack layers
   SHALL be fetchable from that cache instead of the origin registry on a fresh
   environment (e.g. clean CI runner), demonstrating remote-cache reuse.

### Requirement 5: Multi-arch support without OCI-layout

**User Story:** As a user, I want multi-arch images published as a single
manifest list, so that the output matches the other backends.

#### Acceptance Criteria

1. WHEN building for multiple platforms THE detect+build RUN SHALL run per
   platform (BuildKit multi-platform), and the host-side exporter SHALL assemble
   a per-arch app image for each.
2. WHEN publishing multi-arch THE per-arch app images SHALL be assembled
   host-side as NON-PUSHED image handles (in-memory / local layout) and combined
   into ONE manifest list via a single atomic index push (go-containerregistry
   `remote.WriteIndex`), with the per-arch images referenced ONLY BY DIGEST and
   NO intermediate per-arch tags written to any registry.
3. THE manifest-list assembly and publish SHALL be performed by PACK host-side
   (not BuildKit; BuildKit does not hold the final app images in this design).
4. THE per-arch app images SHALL have correct os/arch and be well-formed
   launchable CNB images.

### Requirement 6: Locally-testable MVP and comparison

**User Story:** As a maintainer, I want to validate and benchmark this approach
locally, so that I can compare it against the existing backends.

#### Acceptance Criteria

1. THE MVP SHALL build `samples/go/no-imports` and publish to the local registry
   using the local MVP build testing strategy (build + rebuild + runnable check).
2. WHEN a per-arch image is inspected THE image SHALL have all CNB labels (incl
   `io.buildpacks.lifecycle.metadata`), multiple real layers, and the launch
   process binary present.
3. THE cold and warm (rebuild) durations SHALL be captured for comparison against
   the LLB OCI-layout backend numbers already recorded.

### Requirement 7: Reuse the lifecycle reference implementation

**User Story:** As a maintainer, I do not want to reimplement CNB semantics in
pack, so that we stay spec-compliant.

#### Acceptance Criteria

1. ALL buildpacks-specific logic (detect, build, analyze metadata, export layer
   assembly + labels, rebase boundary) SHALL be performed by the lifecycle
   (binary for detect/build; library for analyze/export), NOT reimplemented in
   pack.
2. WHERE the lifecycle library surface is insufficient THE change SHALL be a
   minimal, additive lifecycle modification on the matching
   `lifecycle-as-library-hybrid` branch (documented), not a fork of CNB logic
   into pack.
