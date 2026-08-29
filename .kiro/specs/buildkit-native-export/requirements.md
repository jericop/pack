# Requirements: BuildKit-Native Export (Option C)

## Introduction

This spec captures an EXPERIMENTAL, fully OPT-IN approach where BuildKit is the
primary orchestrator for the ENTIRE build — including image assembly — and the CNB
lifecycle is made "BuildKit-aware" so its buildpacks-specific logic integrates with
BuildKit natively, without ever egressing layer data to the host.

The mental model (the user's framing): assembling the final app image is
conceptually a "Dockerfile of sources" — `FROM <run-image>`, ADD the layers the
builder phase produced, set the CNB image config/labels — and BuildKit already
does exactly this kind of thing extremely fast and efficiently. The buildpacks
semantics (which layers, their order/history, the run-image rebase boundary, the
`io.buildpacks.*` labels, entrypoint/env) live in the lifecycle. So we keep the
lifecycle as the source of CNB truth but add an opt-in mode that lets BuildKit
perform the actual layer assembly and image production.

Contrast with the other approaches:
- **Option A (oci-layout-tag-elimination):** lifecycle exporter runs INSIDE
  BuildKit and produces an OCI layout; data stays in BuildKit but with sandbox
  friction (no local registry, chown, run-image materialization, wrapper bugs).
- **Option B (lifecycle-as-library-hybrid):** lifecycle exporter runs HOST-SIDE as
  a library; native image I/O + local-registry testing, but build outputs must be
  EGRESSED from BuildKit (cost scales with app size).
- **Option C (this spec):** BuildKit assembles the image natively (no egress, no
  host-side exporter, no sandbox push of intermediate tags); the lifecycle emits
  only the ordered layer plan + small image-config metadata that BuildKit applies.

This is the most ambitious and the most BuildKit-idiomatic. It is experimental;
the existing backends remain.

## Two-repo split (read this first)

Option C spans two repos with distinct ownership:

- **cnb-lifecycle** (`jericop/cnb-lifecycle@buildkit-native-export`,
  `.kiro/specs/buildkit-native-export/`) owns the LIFECYCLE emit-mode and DEFINES
  the emit contract (`plan.json` + `config.json` + referenced layer tars, schema
  `buildkit-native-export/v1`). That is the source of truth for the interface.
- **cnb-pack** (THIS spec) owns the CONSUMER: the `buildkit-native` backend that
  reads the emitted plan/config and layer tars, builds the BuildKit-native image
  assembly (`FROM run-image` + add layers + apply config), and does native
  multi-arch export/push.

This spec references the emit contract; it does not re-specify the lifecycle
changes. Keep the `schema` version in sync with the lifecycle spec.

## Glossary

- **BuildKit-native export**: producing the final CNB image via BuildKit's own
  image assembly (`FROM run-image` base + added layers + applied config), rather
  than the lifecycle exporter mutating/pushing an image.
- **Emit/plan mode (new lifecycle mode)**: an opt-in lifecycle path that, instead
  of assembling+pushing an image, EMITS (a) the ordered list of layers to add
  (with diffIDs, history, and which are reused run-image layers) and (b) the final
  image CONFIG (entrypoint, env, workingdir, labels incl `io.buildpacks.*`,
  per-layer history). All the CNB decisions; none of the image mutation.
- **Layer plan**: the emitted ordered set of layer descriptors (buildpack,
  launcher, app slices, SBOM) that BuildKit adds onto the run-image base.
- **Image config metadata**: the small JSON the lifecycle emits that BuildKit
  applies to the assembled image config.
- **Content store / LLB integration**: the run image and produced layers live in
  BuildKit's content store; the export is expressible as an LLB graph that BuildKit
  solves and pushes.

## Transport model (how the emit output reaches pack)

Pack already consumes the lifecycle as a Go library (e.g. `phase.Rebaser.Rebase(...)`
in `pkg/client/rebase.go`), and this backend does the same: it imports the
lifecycle's `phase/emit` package, so consuming the emit types adds no new dependency
surface. What differs from an in-process library call is TRANSPORT, driven by WHERE
each side runs:

- The emit-mode PRODUCER (the lifecycle exporter in emit-mode) runs INSIDE BuildKit,
  in a RUN step, and writes `buildkit/plan.json` + `buildkit/config.json` to the
  build filesystem.
- The CONSUMER (pack's Go code) runs on the HOST, in a different process and
  filesystem.

Because producer and consumer are in different processes, the data MUST cross as
serialized bytes. Therefore:

1. **The JSON files are the transport (wire format).** They are how the metadata
   crosses the BuildKit→host boundary. This is unavoidable — it is a process/fs
   boundary, not a design preference.
2. **The imported `emit` structs are the schema.** Pack does
   `json.Unmarshal(bytes, &emit.Plan{})` / `&emit.ImageConfig{}` using the types
   IMPORTED from the lifecycle (single source of truth, no drift). Pack does NOT
   mirror/re-declare the schema.
3. **Layer DATA stays in BuildKit.** `plan.json` references layer tar PATHS that are
   inside the BuildKit build (e.g. `/tmp/lifecycle.exporter.layer*/….tar`). Pack on
   the host does NOT read those tars. The LLB assembly references them WITHIN
   BuildKit by the emitted diffID; only the small plan/config metadata crosses to
   the host. This is what preserves the "no egress" property.

Contrast with `rebase.go`: there the lifecycle library call is IN-PROCESS (same
address space), so Go structs pass directly with no serialization. Emit-mode cannot
do that because the producer is in a separate process (inside BuildKit), so it
serializes to JSON. Same library, different transport — dictated by the process
boundary, not by a structs-vs-JSON decision.

## Requirements

### Requirement 1: BuildKit assembles the image; no layer-data egress

**User Story:** As a developer, I want the app image assembled inside BuildKit so
that large app/dependency layers never leave BuildKit, giving fast, efficient
builds like a normal `docker build`.

#### Acceptance Criteria

1. THE final per-arch app image SHALL be assembled by BuildKit natively:
   `FROM <run-image>` base + the builder-phase layers + the CNB image config.
2. THE build outputs (`/layers`, `/workspace`) SHALL NOT be egressed from BuildKit
   to the host for image assembly (contrast Option B).
3. THE run image SHALL be a BuildKit content-addressed source (`llb.Image` /
   content store), reused as the base, with its original layer blobs/diffIDs
   preserved (rebase-safe).

### Requirement 2: Consume the lifecycle emit-mode output (do not reimplement CNB)

**User Story:** As a pack maintainer, I do not want to reimplement CNB semantics
in pack or BuildKit, so that we stay spec-compliant; pack should CONSUME what the
lifecycle emit-mode produces.

#### Acceptance Criteria

1. ALL buildpacks-specific decisions (which layers, order, history, run-image
   rebase boundary, `io.buildpacks.*` labels, entrypoint/env/workingdir) SHALL come
   from the lifecycle emit-mode output (the emit contract), NOT be reimplemented in
   pack or hand-authored.
2. PACK SHALL run the lifecycle emit-mode (opt-in) after the builder phase to
   obtain `plan.json` + `config.json` (+ the in-BuildKit layer tars they reference),
   per the cnb-lifecycle `buildkit-native-export` emit contract (schema
   `buildkit-native-export/v1`).
3. PACK SHALL IMPORT the lifecycle's `phase/emit` types and unmarshal the emitted
   JSON into them (the JSON is the transport across the BuildKit→host boundary; the
   structs are the schema — see "Transport model"). Pack SHALL NOT re-declare/mirror
   the schema and SHALL NOT depend on lifecycle internals beyond the `emit` package.
   Contract changes are coordinated with the lifecycle spec.
4. THE layer tar DATA referenced by `plan.json` SHALL remain inside BuildKit; pack
   SHALL reference those layers within the BuildKit assembly by their emitted diffID
   and SHALL NOT read/egress the tars to the host (only the plan/config metadata
   crosses to the host).

### Requirement 3: Express the assembly as LLB / content-store operations

**User Story:** As a pack maintainer, I want the export expressed as BuildKit
operations so that BuildKit caches and pushes it efficiently.

#### Acceptance Criteria

1. GIVEN the lifecycle emit-mode plan + config THE backend SHALL construct a
   BuildKit assembly (LLB or equivalent): run-image base + add each planned layer
   (from the in-BuildKit builder outputs / content store) + apply the config.
2. THE produced layers SHALL be sourced from data ALREADY in BuildKit (the builder
   phase output), not re-imported from the host.
3. THE reused run-image layers SHALL be referenced by digest from the run-image
   source (no re-tar, rebase-safe).

### Requirement 4: BuildKit-idiomatic caching throughout

**User Story:** As a developer, I want fast rebuilds with normal BuildKit caching,
so that the experience matches a fast `docker build`.

#### Acceptance Criteria

1. THE assembly steps SHALL be cacheable BuildKit operations; unchanged inputs
   SHALL cache-hit on rebuild.
2. THE builder image and run image SHALL be content-addressed sources that
   cache-hit on rebuild independent of app changes (same guarantee as the other
   specs).
3. THE approach SHALL support a registry-backed BuildKit remote cache
   (`--buildkit-cache-from`/`--buildkit-cache-to`).

### Requirement 5: Multi-arch, no intermediate tags

**User Story:** As a user, I want multi-arch images published as one manifest list
so the output matches the other backends.

#### Acceptance Criteria

1. WHEN building multiple platforms THE per-arch images SHALL be assembled by
   BuildKit per platform and combined into ONE manifest list.
2. THE publish SHALL create NO intermediate per-arch tags. (BuildKit's native
   multi-platform image export assembles the manifest list directly.)
3. THE per-arch images SHALL have correct os/arch and be well-formed launchable
   CNB images.

### Requirement 6: Opt-in, experimental, and comparable

**User Story:** As a maintainer, I want this behind an explicit opt-in and
locally testable, so it can be compared to Options A and B without risk.

#### Acceptance Criteria

1. THE approach SHALL be a distinct opt-in backend (e.g.
   `--build-backend=buildkit-native`) that does not change the other backends.
2. THE MVP SHALL build `samples/go/no-imports` (and a large Node/Python app for
   the egress/size comparison) to the local registry via the local MVP strategy,
   with the runnable check (real layers, CNB labels incl
   `io.buildpacks.lifecycle.metadata`, launch binary present).
3. THE cold/warm durations AND the absence of host egress SHALL be recorded and
   compared to Option A (oci-layout) and Option B (host-side export).

### Requirement 7: Rebaseability (consistent behavior, NOT diffID identity)

**User Story:** As a buildpacks user, I want rebase to keep working for images
built with the buildkit-native backend — consistent with how registry/oci-layout
modes rebase — so swapping the run image later does not require a rebuild.

#### Acceptance Criteria

1. THE assembled image SHALL be rebaseable by the lifecycle Rebaser: its base
   layers SHALL be the run image's layers, and its
   `io.buildpacks.lifecycle.metadata` label SHALL carry a `RunImage.TopLayer` that
   correctly identifies the run-image/app boundary in the assembled image.
2. THE app-layer diffIDs NEED NOT match a registry/oci-layout export of the same
   build. Different diffIDs are acceptable because rebase depends only on the
   metadata `RunImage.TopLayer` boundary (empirically verified against the real
   `phase.Rebaser`), not on app-layer diffIDs.
3. AFTER assembly THE backend SHALL rewrite the `io.buildpacks.lifecycle.metadata`
   label so every per-layer `sha` — `App[]`, `Launcher`, `Config`, `ProcessTypes`,
   and each `Buildpacks[].layers[<name>].sha` — equals the ACTUAL produced layer
   diffID (and `RunImage.TopLayer` equals the run image's actual top layer). This is
   REQUIRED for functional parity with buildpack-contributed-layer patching (see
   below), not merely cosmetic.

### Requirement 7a: Support buildpack-contributed-layer patching (functional parity)

**User Story:** As a platform operator, I want images built with the buildkit-native
backend to support the same buildpack-contributed-layer patching as other modes, so
I can selectively patch buildpack layers during rebase.

#### Acceptance Criteria

1. THE assembled image's `io.buildpacks.lifecycle.metadata` `Buildpacks[].layers[<name>].sha`
   values SHALL correspond to the ACTUAL layer diffIDs in the assembled image, so the
   layer-patching feature (buildpacks/rfcs `jab/buildpack-layer-patching`) can locate
   a target buildpack layer by its metadata sha and replace it.
2. IF the per-layer metadata SHAs did not match the actual layers (as with a naive
   pure-LLB assembly that recomputes diffIDs), buildpack-layer patching would fail to
   locate the target layer; therefore the metadata-SHA rewrite (Req 7.3) is mandatory.

### Requirement 7b: Durable layer-order label + correct behavior across REPEATED rebuilds/rebases

**User Story:** As a CI/CD operator, I want repeated rebuilds and rebases of a
buildkit-native image to keep working (not just the first one), and I want a
mid-build rewrite failure to be self-healable on any later build, so my pipeline is
reliable over time.

#### Acceptance Criteria

1. THE `io.buildpacks.native.layer-order` label (ordered emitted diffIDs of the
   non-reused layers) SHALL be a REQUIRED, DURABLE output of every buildkit-native
   build — it SHALL remain on the final pushed image (the metadata-SHA rewrite SHALL
   NOT remove it), so any subsequent build/tool can perform the positional remap.
2. WHEN a rebuild occurs, THE frontend SHALL RE-RECORD the layer-order label from
   THAT build's emit plan (its contents legitimately differ as reused-vs-new layers
   change), and pack's rewrite SHALL map against THAT build's label — NEVER a stale
   label carried from the previous image.
3. WHEN `pack rebase` occurs, THE app/buildpack layer diffIDs SHALL be unchanged, so
   the `io.buildpacks.native.layer-order` label SHALL remain valid without
   regeneration and SHALL NOT be stripped; a REBUILD after a REBASE SHALL succeed.
4. THE metadata-SHA rewrite / self-healing fix SHALL be IDEMPOTENT: applying it to an
   already-correct image is a no-op, and a fixed image SHALL itself be
   rebuild/rebase-capable so the NEXT cycle also self-heals — with no unbounded growth
   or drift of the label across cycles.
5. Verification SHALL cover REPEATED cycles (≥2 rebuilds; ≥2 rebases; rebuild after
   rebase; self-heal then repeated rebuild/rebase), not only the first
   build/rebase.
6. THE metadata-SHA rewrite / self-healing fix SHALL be TAG-ATOMIC and
   FAILURE-SAFE: it re-pushes to the same tag via a single manifest `PUT` (index
   `PUT` for a manifest list), so the tag always resolves to either the previous or
   the updated image, never a partially-written one. IF the rewrite fails before its
   final `PUT`, THE tag SHALL still point at the original pushed image, which SHALL
   remain pullable/runnable (only stale for rebuild/rebase) — no corruption.
7. IT IS ACKNOWLEDGED that the BuildKit push and the host-side rewrite are two
   SEPARATE registry operations (not a single transaction); a fully transactional
   push+rewrite is not possible against a standard registry. THE design SHALL rely on
   tag-atomic overwrites + an IDEMPOTENT, self-correcting fix (per criterion 4) so
   the window of stale metadata is always recoverable on a later build.
