# Requirements: BuildKit-Native Export (Option A — build-then-finalize)

## Introduction

This spec captures an EXPERIMENTAL, fully OPT-IN approach where BuildKit is the
orchestrator that BUILDS and PUSHES the app image natively, and a subsequent
lifecycle-owned FINALIZE step makes that pushed image buildpacks-compliant
(rebuildable + rebaseable) by authoring the correct
`io.buildpacks.lifecycle.metadata` from the image's ACTUAL produced layers.

### Why this design (history + decision)

Earlier iterations of this backend used a custom CNB BuildKit gateway FRONTEND that
re-assembled the image `FROM run-image` by re-extracting each emitted layer tar
(`RUN tar -xf`). BuildKit then re-snapshotted those layers and assigned its OWN
diffIDs, which no longer matched the lifecycle's emitted per-layer SHAs in
`io.buildpacks.lifecycle.metadata`. Pack "fixed" this with a host-side, post-push
metadata-SHA REWRITE. That worked (and was validated), but it meant mutating an
image right after pushing it — a workaround for a divergence WE caused by
re-assembling.

A spike (see the frontend spec's `spike-eliminate-metadata-rewrite.md`) established:

- BuildKit's image exporter ALWAYS derives the final diffIDs from the result ref's
  actual layer chain; a frontend CANNOT inject pre-built blobs via the gateway
  result. So "make BuildKit adopt the lifecycle's exact diffIDs" is not possible
  through the gateway.
- Therefore the only clean choices are (a) reconcile metadata to the produced
  diffIDs after they exist, or (b) avoid re-assembly by importing a
  lifecycle-produced OCI layout (disk materialization + run image pulled in-build).

**Chosen: Option A.** Let BuildKit build and push a normal image (no frontend, no
re-extraction, no OCI-layout disk round-trip), then FINALIZE it: read the pushed
image's produced diffIDs + a small build-phase metadata label, and author the
correct CNB metadata. The mutation becomes the INTENDED two-phase contract
("build phase" → "CNB finalize phase"), authored against reality the first time,
rather than a patch of a self-inflicted mismatch. The finalize logic lives in the
LIFECYCLE as a library (pack consumes it the way it consumes `phase.Rebaser`), so
CNB metadata authorship stays in one place.

This RETIRES the frontend (`buildkit/cnbfrontend`, `cmd/cnb-frontend`), the
per-layer `tar -xf` re-extraction, emit-mode's layer-tar persistence, and the
host-side metadata-SHA rewrite as a pack-owned workaround.

### The two phases

1. **Build phase (BuildKit):** run the lifecycle phases (analyzer → detector →
   restorer → builder → exporter), assemble the app image `FROM run-image`, and let
   BuildKit push ONE native multi-arch image (manifest list) with NO intermediate
   tags. The build also surfaces an ordered plan as a single build-phase LABEL
   (`io.buildpacks.buildkit.native.build-metadata`). The image is runnable but NOT
   yet CNB-compliant (its `io.buildpacks.lifecycle.metadata`, if present, does not
   yet match the produced layers).
2. **Finalize phase (lifecycle library, pack calls it):** read the pushed image's
   ACTUAL produced diffIDs + the `io.buildpacks.buildkit.native.build-metadata`
   label, author the correct `io.buildpacks.lifecycle.metadata` (per-layer SHAs =
   produced diffIDs; `RunImage.TopLayer` = the run-image boundary), and re-push ONLY
   the image config + manifest (+ index for multi-arch). No layers are modified or
   re-uploaded; the tag update is atomic.

## Two-repo split (read this first)

- **cnb-lifecycle** (`jericop/cnb-lifecycle@buildkit-native-export`,
  `.kiro/specs/buildkit-native-export/`) owns: (a) emit-mode producing the ordered
  plan, and (b) the FINALIZE library API + subcommand that authors CNB metadata from
  a built image. It DEFINES the `io.buildpacks.buildkit.native.build-metadata` label
  schema.
- **cnb-pack** (THIS spec) owns the CONSUMER: the `buildkit-native` backend that
  drives the BuildKit build+push and then calls the lifecycle finalize library.

Keep the label/schema versions in sync with the lifecycle spec.

## Glossary

- **Build phase**: BuildKit builds + pushes a normal multi-arch image (runnable,
  not yet CNB-compliant).
- **Finalize phase**: a lifecycle-owned step that authors correct CNB metadata on
  the pushed image from its actual layers + the build-metadata label, then re-pushes
  config+manifest only.
- **`io.buildpacks.buildkit.native.build-metadata`**: a build-phase image LABEL
  carrying the ordered layer plan (order, new-vs-reused, intended diffIDs, history,
  run-image boundary, semantic identity). Namespaced `io.buildpacks.buildkit.native.*`;
  distinct from the final `io.buildpacks.lifecycle.metadata`. It is explicitly a
  BUILD-PHASE artifact, only partially valid until finalize runs.
- **Produced diffID**: the diffID BuildKit actually assigned to a layer at export
  (read from the pushed image config). This is authoritative for the finalized
  metadata.
- **Finalize library**: the lifecycle Go API (+ subcommand) that performs the
  authoring; pack imports it like `phase.Rebaser`.

## Requirements

### Requirement 1: BuildKit builds + pushes the image natively; no layer-data egress

**User Story:** As a developer, I want BuildKit to build and push the app image so
that large app/dependency layers never leave BuildKit and never get re-uploaded,
giving fast, efficient multi-arch builds.

#### Acceptance Criteria

1. THE app image SHALL be assembled inside BuildKit: `FROM <run-image>` base + the
   builder-phase layers, expressed as LLB (the existing LLB assembly is the
   precedent — an earlier iteration assembled `FROM run-image` in LLB without
   issue).
2. BuildKit SHALL PUSH the final image; pack SHALL NOT re-push layer blobs. The only
   host-side registry write after the build is the finalize config+manifest update
   (Requirement 4), which touches NO layers.
3. THE build outputs (`/layers`, `/workspace`) and layer tars SHALL NOT be egressed
   from BuildKit for host-side image assembly.
4. THE run image SHALL be a BuildKit content-addressed source (`llb.Image`), reused
   as the base with its original layer blobs/diffIDs preserved (rebase boundary).

### Requirement 2: No frontend; no re-extraction; no post-push layer changes

**User Story:** As a maintainer, I want the simplest BuildKit-idiomatic path, so we
avoid a custom gateway frontend and any post-push layer surgery.

#### Acceptance Criteria

1. THE backend SHALL NOT require a custom BuildKit gateway frontend
   (`buildkit/cnbfrontend` / `cmd/cnb-frontend` are RETIRED from this design).
2. THE backend SHALL NOT re-extract emitted layer tars (`RUN tar -xf`) to force
   layer boundaries; the layers are whatever the lifecycle exporter produced,
   pushed by BuildKit.
3. THE finalize step SHALL modify ONLY the image config/manifest (metadata); it
   SHALL NOT add, remove, re-tar, or re-upload any layer.

### Requirement 3: Surface the ordered plan as a build-phase LABEL (not a layer)

**User Story:** As the finalize step, I need the ordered layer plan (order,
new-vs-reused, intended diffIDs, semantic identity, history, run-image boundary) to
author correct metadata, and I want it carried as image metadata, not a runtime
layer.

#### Acceptance Criteria

1. THE build phase SHALL write the ordered plan into a single image LABEL
   `io.buildpacks.buildkit.native.build-metadata` (serialized JSON). It SHALL NOT
   add a layer for this purpose.
2. THE label SHALL be namespaced `io.buildpacks.buildkit.native.*` and SHALL be
   DISTINCT from the final `io.buildpacks.lifecycle.metadata` (the build phase SHALL
   NOT pre-write a valid final label with stale SHAs).
3. THE label contents SHALL be produced by the lifecycle (emit-mode already computes
   the ordered plan); pack SHALL NOT hand-author CNB layer semantics.
4. THE label SHALL carry everything finalize needs: ordered layers with
   new-vs-reused flags, intended diffIDs, layer identity (app/sbom/launcher/config/
   process-types/buildpack layers), history, and the run-image reference + top
   layer. Additional fields MAY be added to the label schema without adding layers.

### Requirement 4: Lifecycle-owned FINALIZE authors CNB metadata from the produced image

**User Story:** As a buildpacks user, I want the pushed image to become fully
CNB-compliant (rebuildable + rebaseable) via a lifecycle-owned step, so metadata
authorship is not duplicated in pack.

#### Acceptance Criteria

1. THE finalize logic SHALL live in the LIFECYCLE as a library API (+ a subcommand
   wrapper), and pack SHALL CONSUME it (import + call), the way pack consumes
   `phase.Rebaser` for rebase. Pack SHALL NOT re-implement CNB metadata authorship.
2. FINALIZE SHALL read the pushed image's ACTUAL produced layer diffIDs (from the
   image config) and the `io.buildpacks.buildkit.native.build-metadata` label, then
   author `io.buildpacks.lifecycle.metadata` such that every per-layer `sha`
   (`App[]`, `Launcher`, `Config`, `ProcessTypes`, each
   `Buildpacks[].layers[<name>].sha`, and `sbom`) equals the produced diffID for
   that layer, and `RunImage.TopLayer`/`Reference` correctly identify the run-image
   boundary.
3. FINALIZE SHALL re-push ONLY the image config + manifest (+ the index for a
   multi-arch manifest list). It SHALL NOT modify layers.
4. AFTER finalize THE image SHALL be a well-formed CNB image: rebuildable (the
   analyzer's previous-image restore succeeds) and rebaseable (Rebaser succeeds),
   and it SHALL support buildpack-contributed-layer patching (metadata SHAs match
   actual layer diffIDs).

### Requirement 5: Multi-arch, no intermediate tags (native BuildKit push)

**User Story:** As a user, I want multi-arch images published as ONE manifest list
with no intermediate per-arch tags.

#### Acceptance Criteria

1. WHEN building multiple platforms BuildKit SHALL produce ONE native manifest list
   (per-arch images held in BuildKit's content store and the index pushed
   natively). NO intermediate per-arch tags SHALL be created.
2. Because finalize only updates config+manifest (no layer moves, same final tag),
   it SHALL NOT introduce intermediate tags either; pack's separate manifest-list
   assembly machinery (`PushPerArchLayoutsAsManifestList`) SHALL NOT be required for
   this backend (BuildKit's native push already yields the single index).
3. THE per-arch images SHALL have correct os/arch and be well-formed launchable CNB
   images after finalize.

### Requirement 6: BuildKit-idiomatic caching

**User Story:** As a developer, I want fast rebuilds with normal BuildKit caching.

#### Acceptance Criteria

1. THE build steps SHALL be cacheable BuildKit operations; unchanged inputs SHALL
   cache-hit on rebuild. (Recomputed layer diffIDs do NOT affect BuildKit's build
   cache — it keys on the operation graph, not output diffIDs.)
2. THE builder image and run image SHALL be content-addressed sources that cache-hit
   on rebuild independent of app changes.
3. THE approach SHALL support a registry-backed BuildKit remote cache
   (`--buildkit-cache-from`/`--buildkit-cache-to`).

### Requirement 7: Opt-in, experimental, and locally validated

**User Story:** As a maintainer, I want this behind an explicit opt-in and validated
locally, including REPEATED rebuilds and rebases.

#### Acceptance Criteria

1. THE approach SHALL be a distinct opt-in backend
   (`--build-backend=buildkit-native`) that does not change the other backends.
2. THE MVP SHALL build `samples/go/no-imports` to a local registry via the local MVP
   strategy, with a runnable check (real layers, CNB labels incl a correct
   `io.buildpacks.lifecycle.metadata` after finalize, launch binary present).
3. Validation SHALL cover REPEATED cycles — NOT just the first: at least TWO
   rebuilds (each rebuild's analyzer previous-image restore succeeds and all
   per-layer metadata SHAs match the produced diffIDs), at least TWO rebases (each
   succeeds), a rebuild AFTER a rebase, and multi-arch (linux/amd64 + linux/arm64,
   one index, no intermediate tags).
4. Validation SHALL confirm that after the build+finalize, NO layer blobs are
   re-uploaded by the host (finalize is config+manifest only), and that the
   finalized metadata is authored from the produced diffIDs (not patched from a
   stale label).

### Requirement 8: Rebaseability (consistent behavior, NOT diffID identity)

**User Story:** As a buildpacks user, I want rebase to keep working for
buildkit-native images, consistent with the other modes.

#### Acceptance Criteria

1. THE finalized image SHALL be rebaseable by the lifecycle Rebaser: its base layers
   SHALL be the run image's layers, and `io.buildpacks.lifecycle.metadata` SHALL
   carry a `RunImage.TopLayer` correctly identifying the run-image/app boundary.
2. THE app-layer diffIDs NEED NOT match a registry/oci-layout export of the same
   build. Different diffIDs are acceptable because rebase depends only on the
   metadata `RunImage.TopLayer` boundary (empirically verified against the real
   `phase.Rebaser`), and finalize makes the per-layer metadata SHAs match the actual
   produced layers (so buildpack-layer patching also works).

### Requirement 9: Behavior across REPEATED rebuilds and rebases

**User Story:** As a CI/CD operator, I want repeated rebuilds and rebases to keep
working over time, not just the first cycle.

#### Acceptance Criteria

1. WHEN a rebuild occurs, THE build phase SHALL re-produce the
   `io.buildpacks.buildkit.native.build-metadata` label from THAT build's plan (its
   contents legitimately differ as reused-vs-new layers change), and finalize SHALL
   author metadata against THAT build's label + produced diffIDs — never a stale
   label carried from the previous image.
2. WHEN `pack rebase` occurs, THE app/buildpack layer diffIDs SHALL be unchanged;
   rebase updates only the run-image boundary in `io.buildpacks.lifecycle.metadata`.
   A REBUILD after a REBASE SHALL succeed.
3. THE finalize step SHALL be IDEMPOTENT: finalizing an already-finalized image is a
   no-op (or re-authors identical metadata), so repeated cycles do not drift.
4. THE finalize config+manifest re-push SHALL be TAG-ATOMIC and FAILURE-SAFE: on
   failure the tag still resolves to the pushed (pre-finalize) image, which remains
   pullable/runnable (just not yet rebuildable/rebaseable until finalize succeeds).
5. Verification SHALL exercise REPEATED cycles (≥2 rebuilds; ≥2 rebases; rebuild
   after rebase), confirming correctness each time.

### Requirement 10: Self-healing (DEFERRED — after MVP)

**User Story:** As a CI/CD operator, I want a way to detect and fix an image whose
finalize did not complete (e.g. an interrupted build), without a separate confusing
command.

#### Acceptance Criteria

1. (DEFERRED, post-MVP) WHEN a buildkit-native build targets an image ref that
   already exists remotely, pack MAY inspect its metadata validity; if invalid and
   an opt-in flag (e.g. `--buildkit-fix-remote-image-metadata`) is set, pack MAY run
   FINALIZE on the existing image in place (using its retained
   `io.buildpacks.buildkit.native.build-metadata` label) before proceeding.
2. This is explicitly OUT OF SCOPE for the MVP and SHALL be added only after the
   build→finalize MVP is confirmed with repeated rebuilds/rebases.
