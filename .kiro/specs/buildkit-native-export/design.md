# Design: BuildKit-Native Export (Option C)

## Overview

Make BuildKit the primary orchestrator for the WHOLE build, including image
assembly, while keeping the CNB lifecycle as the source of buildpacks truth via a
new opt-in "emit/plan" mode. The app image is assembled natively by BuildKit —
conceptually `FROM <run-image>` + ADD builder-phase layers + apply CNB config —
with NO layer-data egress to the host and NO host-side exporter.

Experimental, opt-in backend `--build-backend=buildkit-native`. Existing backends
(buildkit-dockerfile, buildkit-llb/oci-layout, buildkit-hybrid) are unchanged.

Two-repo split: this (cnb-pack) spec is the CONSUMER side. The lifecycle emit-mode
and the emit contract (`plan.json` + `config.json` + layer tars, schema
`buildkit-native-export/v1`) are owned by
`jericop/cnb-lifecycle@buildkit-native-export`
(`.kiro/specs/buildkit-native-export/`). Read that spec for the contract; this
spec references it and consumes it.

## The key decomposition (from investigating the lifecycle exporter)

The lifecycle exporter (`phase/exporter.go`, `layers/`) does two separable things,
currently interleaved:

1. **Layer creation** — `layers.Factory` tars directories into layers
   (`DirLayer`, `TarLayer`, `SliceLayers`, `LauncherLayer`, `ProcessTypesLayer`).
   This is mechanically what BuildKit does natively.
2. **Image config/metadata** — `SetEntrypoint(/cnb/process/<default>)`, `SetCmd`
   (empty), `SetEnv` (CNB_LAYERS_DIR, CNB_APP_DIR, PATH, ...), `SetWorkingDir`,
   `SetLabel(io.buildpacks.lifecycle.metadata / build.metadata / project.metadata
   / exec-env)`, and per-layer `History` + `ReuseLayerWithHistory` (the run-image
   rebase boundary). This is CNB semantics BuildKit does not know.

Option C separates these: BuildKit does (1) natively; the lifecycle emits (2) plus
the ORDERED PLAN for (1) (which layers, order, diffIDs, history, which run-image
layers are reused). The heavy layer DATA stays in BuildKit; only small PLAN +
CONFIG metadata is produced by the lifecycle.

## Two variants

- **C1: lifecycle emit-mode + pack builds the assembly via plain `client.Solve`.**
  Pack reads the emit plan/config and constructs the `FROM run-image` + add-layers
  graph itself. BLOCKED BY A BUILDKIT CONSTRAINT (see below): a plain
  `client.Solve` with a raw `llb.State` CANNOT set the final image config
  (Entrypoint/Env/WorkingDir/Labels incl `io.buildpacks.lifecycle.metadata`) — that
  requires the gateway/frontend API. So C1 alone cannot produce a valid CNB image.
- **C2 (RECOMMENDED — custom CNB BuildKit frontend): lifecycle emit-mode + a
  gateway frontend assembles + sets config.** A custom BuildKit frontend (invoked
  by pack) runs the build graph and, crucially, sets the image config/labels on the
  result via `res.AddMeta(exptypes.ExporterImageConfigKey, ...)` and returns
  per-platform refs via `res.AddRef` for native multi-arch. This removes ALL the
  plain-`client.Solve` limitations.

### The BuildKit config-setting constraint (why C2)

A plain `client.Solve` + `llb.State` produces a filesystem; it does NOT let the
caller set arbitrary output image config (Entrypoint/Env/WorkingDir/Labels). Those
are attached via the gateway result's `exptypes.ExporterImageConfigKey` meta, which
only a gateway/frontend `client.Build(...)` flow can set. Since a CNB image REQUIRES
an entrypoint + the `io.buildpacks.lifecycle.metadata` label (basic run-image rebase
reads `RunImage.TopLayer` FROM that label), C1-via-plain-Solve is a dead end for
producing a valid image. The frontend is the mechanism that sets config.

### Prior art: cnbp (EricHripko) — a working CNB BuildKit frontend

`cnbp` (`/Users/jpena/.repos/EricHripko/cnbp`, steering: `cnbp-buildkit-frontend.md`)
is a real custom BuildKit frontend for CNB that proves this pattern end to end in the
same gateway APIs our BuildKit vendors:

- `cmd/cnbp-frontend/main.go`: `grpcclient.RunFromEnvironment(ctx, Build)` — the
  frontend entrypoint (shipped as an image, invoked via `#syntax=`).
- `frontend.go` `BuildWithService`: builds each platform in parallel (`errgroup`),
  and for each sets the image config via
  `res.AddMeta(fmt.Sprintf("%s/%s", exptypes.ExporterImageConfigKey, platformKey), config)`
  + `res.AddRef(platformKey, ref)`, then `res.AddMeta(exptypes.ExporterPlatformsKey, …)`.
  BuildKit assembles ONE multi-platform manifest list natively — no intermediate tags.
- `pkg/cnbp2llb/export.go` `Export`: `build.From(stack.RunImage.Image, platform)`
  (run image as an `llb.Image` content-store base, never copied out) + `llb.Copy(built,
  layer, layer)` for each launch layer/launcher/app — assembly happens entirely inside
  BuildKit from the solved `built` state. No host egress, no run-image disk-layout copy.

**What cnbp proves (in our vendored gateway APIs):** (a) set full image config +
labels → SUPPORTED via `AddMeta(ExporterImageConfigKey)`; (b) FROM run-image + add
layers using only content-store data, no copying out → SUPPORTED via `llb.Image` base
+ `llb.Copy` within the frontend; (c) native multi-platform manifest list in one
solve → SUPPORTED via `AddRef` + `ExporterPlatformsKey`.

**cnbp's trap we MUST avoid:** cnbp REIMPLEMENTS export in the frontend, so it lost
layer reuse, proper `io.buildpacks.*` metadata labels, SBOM, and process types
(see steering "Limitations"). We do NOT reimplement CNB export.

### RECOMMENDED synthesis: frontend assembly driven by lifecycle emit-mode

Combine cnbp's frontend assembly with our emit-mode so CNB fidelity is retained:

1. The frontend runs the build graph: builder image base → COPY app → detector RUN →
   builder RUN → **lifecycle emit-mode RUN** (our existing `-emit-export-plan`), which
   produces the REAL CNB layer plan + config + labels (NOT a reimplementation).
2. The frontend's export reads the emit contract (`plan.json`/`config.json`) from the
   solved state, does `FROM llb.Image(run-image)` + `llb.Copy` the emitted layers
   (cnbp's pattern; BuildKit recomputes layer diffIDs — fine, rebase works), and sets
   the image config from `config.json` via `res.AddMeta(ExporterImageConfigKey, …)`,
   INCLUDING the emitted `io.buildpacks.lifecycle.metadata` label with the correct
   `RunImage.TopLayer`.
3. BuildKit exports natively, multi-arch, no intermediate tags, no egress.

This is cnbp's BuildKit-native assembly WITHOUT cnbp's loss of CNB fidelity, because
the lifecycle (emit-mode) still computes all CNB semantics. It also solves the
config-setting constraint (frontend `AddMeta`) and keeps ALL data in the content
store (no copying the run image, no host egress).

### AS-BUILT (MVP, validated) — reconciles the decisions below

The MVP was implemented as the RECOMMENDED synthesis above (C2 frontend), with two
refinements discovered during implementation. The earlier "pure-LLB assembly via
pack-side `client.Solve`" framing in "Design decision 5" is SUPERSEDED by the
frontend (a plain `client.Solve` cannot set image config/labels); the layer/rebase
reasoning in decision 5 still holds and is what the frontend implements.

1. **Per-layer assembly.** The frontend assembles ONE layer per emitted CNB layer
   (extract each emitted tar as its own layer, in plan order) — not a wholesale
   `/layers` copy. This gives the assembled image the same layer boundaries as the
   plan, so each buildpack layer is individually addressable. Emit-mode PERSISTS the
   layer tars under `<emit-dir>/buildkit/layers/` so they survive the exporter's
   temp-dir cleanup and the frontend can read them.
2. **Host-side metadata-SHA rewrite (NOT in the frontend).** The gateway `Reference`
   API does not expose the produced layer diffIDs, and BuildKit computes them at
   export time (after the frontend returns). So the metadata-SHA rewrite happens in
   PACK, post-push: pack pulls the image config+manifest (tiny, NO layer egress),
   maps emitted→actual diffIDs (via an ordered temp label the frontend records),
   rewrites `io.buildpacks.lifecycle.metadata`, drops the temp label, re-pushes
   config+manifest. This makes buildpack-layer patching SUPPORTED (see below) and
   fixes the analyzer's previous-image restore on rebuilds.

The "Fallback (A)" below was NOT needed — the frontend works and keeps data in the
content store.

**Fallback (A / "lifecycle assembles a layout in-build"):** if the frontend proves
too large for the MVP, the fallback is the lifecycle writing a finished OCI layout
in-build (it sets config/labels via its exporter) which pack imports via
`llb.OCILayout` + exports. Downside: needs the run image readable in-build (disk
layout / copy), which we want to AVOID — hence it is the fallback, not the primary.

### Buildpack-layer-patching: SUPPORTED (was originally deferred)

Originally we planned to defer buildpack-contributed-layer patching past the MVP.
During implementation it turned out to be REQUIRED anyway, because the analyzer's
previous-image restore on rebuilds ALSO relies on the metadata per-layer SHAs
matching the image's actual layers (same root cause). So the per-layer metadata-SHA
rewrite was implemented (host-side, see "AS-BUILT" above) and is validated: every
`Buildpacks[].layers[<name>].sha` (and app/sbom/launcher/config/process-types)
matches an actual layer diffID in the pushed image. This satisfies the
`jab/buildpack-layer-patching` RFC prerequisite — the feature is SUPPORTED by
buildkit-native images, not deferred.

The frontend (C2) is the assembly mechanism. The lifecycle provides emit-mode + the
frontend package (`buildkit/cnbfrontend`); pack drives it in-process via
`client.Client.Build` and does the host-side metadata rewrite.

## Data flow (C1)

```
BuildKit (per platform), one graph, data never leaves:
  base = llb.Image(builder)
  COPY app -> /workspace
  RUN detector  -> /layers/group.toml, plan.toml
  RUN builder   -> /layers/<bp> outputs, SBOM        (cache mounts)
  RUN lifecycle --emit-export-plan \                  (NEW opt-in mode)
        -run-image <ref> -analyzed ... \
        -> writes /export/plan.json + /export/config.json   (small metadata)
        (NO image assembly, NO push, NO layer-data egress)
        │
        ▼  pack reads ONLY the small plan.json + config.json (tiny egress) OR a
           frontend consumes them in-graph
  assembly (LLB):
    from = llb.Image(run-image)                        # content-addressed base
    for layer in plan (in order):
       if reuse-run-image-layer: reference base layer by digest (no re-add)
       else: add layer from /layers/<path> produced above (already in BuildKit)
    apply image config (entrypoint/env/workingdir/labels) from config.json
        │
        ▼
  BuildKit image export (ExporterImage), multi-platform:
    assembles per-arch images + ONE manifest list natively, push=true
    NO intermediate tags (BuildKit multi-platform export does this)
```

## Design decisions

### 1. Lifecycle emit-mode is the core new capability (owned by the lifecycle spec)
The opt-in lifecycle emit-mode — which reuses the existing layer-build + metadata
code paths but writes an ordered layer plan + image config instead of mutating an
image — is specified and implemented in the LIFECYCLE spec
(`jericop/cnb-lifecycle@buildkit-native-export`), which DEFINES the emit contract
(`plan.json` + `config.json` + referenced layer tars, schema
`buildkit-native-export/v1`). PACK CONSUMES that contract; it does not implement
the emit-mode. See that spec for the exact schema.

### 2. Layer data stays in BuildKit
Non-reused layers are added from the `/layers` produced by the builder RUN in the
SAME BuildKit graph (already content-addressed there). Reused run-image layers are
referenced by digest from the run-image source. Only the tiny plan/config JSON is
consumed by pack (or a frontend) — orders of magnitude smaller than the layers.

### 3. BuildKit does the multi-arch manifest list natively
Because BuildKit assembles and exports the image, its native multi-platform image
export produces the manifest list directly (push=true) with no intermediate tags —
the most idiomatic path, and stronger than the host-side index push.

### 4. Run image as content-addressed base
`FROM run-image` via `llb.Image`/content store; original blobs/diffIDs preserved
for rebase. No materialization, no copy.

### 5. Layer assembly: rebaseability, NOT diffID identity (REVISED — empirically decided)

**Reframing (decisive):** the goal is NOT that the buildkit-native image has the
SAME layer diffIDs as a registry/oci-layout export. The goal is that it is
REBASEABLE with consistent behavior to today's modes — i.e. you can later swap its
run-image base. DIFFERENT app-layer diffIDs are ACCEPTABLE as long as the image
rebases correctly.

**Empirical proof (see `/tmp/kiro-command-logs/DECISIVE-rebase-recomputed-diffids-PASS.log`):**
we ran the REAL `phase.Rebaser` against a synthetic image assembled as
"FROM run-image + one app layer with an ARBITRARY (recomputed) diffID", with the
`io.buildpacks.lifecycle.metadata` label's `RunImage.TopLayer` set to the run
image's real top layer. Rebase SUCCEEDED: the base layer was swapped for the new
base, the app layer (recomputed diffID) survived untouched, and the metadata
`TopLayer` updated to the new base. This is because `Rebaser.Rebase` calls
`workingImage.Rebase(origMetadata.RunImage.TopLayer, newBase)` — it depends ONLY on
(1) the metadata label's `RunImage.TopLayer` boundary and (2) the base layers
actually being the run image's layers. It is INDIFFERENT to app-layer diffIDs.

**DECISION (revised): pure-LLB "FROM run-image" assembly.** Assemble the image the
BuildKit-idiomatic way — `FROM llb.Image(run-image)` + add the app/buildpack/
launcher layers from the in-build `/layers` (BuildKit snapshots them and computes
its OWN diffIDs) + apply the emitted config (entrypoint/env/workingdir/labels
incl `io.buildpacks.lifecycle.metadata`). The ONLY correctness requirement is that
the emitted `RunImage.TopLayer` in the metadata label matches the run image's top
layer in the assembled image (the emit contract already provides this). No
content-store materialization, no host egress, no hand-assembled manifest.

**Why this beats the earlier "content-level blob append" plan:** it keeps data in
BuildKit (no egress), it uses BuildKit's native layer production (fully cacheable),
and rebase still works via the metadata boundary. The earlier plan (append
precomputed blobs to preserve exact diffIDs) is more complex and unnecessary once
rebaseability — not diffID identity — is the actual requirement.

**REQUIRED post-assembly step — rewrite the metadata layer SHAs (NOT optional):**

After the LLB assembly, pack MUST read back the assembled image's actual layer
diffIDs and REWRITE the `io.buildpacks.lifecycle.metadata` label so that every
per-layer `sha` (the `App[]`, `Launcher`, `Config`, `ProcessTypes`, AND each
`Buildpacks[].layers[<name>].sha`) equals the ACTUAL produced diffID, and
`RunImage.TopLayer` equals the run image's actual top layer.

WHY THIS IS REQUIRED (not just cosmetic): the "Rebase Buildpack Contributed Layers"
RFC (buildpacks/rfcs `jab/buildpack-layer-patching`,
`text/0000-rebase-buildpack-contributed-layers.md`) matches layers to patch by the
`buildpacks[].layers[<name>].sha` in this label and REPLACES the layer identified by
that sha. If the label's `sha` values do not correspond to real layers in the image,
buildpack-layer patching CANNOT locate the target layer → the feature breaks. Plain
run-image rebase only needs `RunImage.TopLayer` (proven above), but FUNCTIONAL PARITY
includes buildpack-contributed-layer patching, so the per-layer SHAs must be accurate.

This is cheap and keeps no-egress: pack reads the diffIDs from the assembled image's
CONFIG (RootFS.DiffIDs), not from layer data. The plan gives layer ORDER and BuildKit
produces layers in that order, so mapping emitted-plan-entry → actual-produced-diffID
is a positional zip. The result is internally consistent for BOTH base rebase AND
buildpack-layer patching.

**Remaining documented caveat (RFC + README):**

- **App/buildpack-layer diffIDs differ** from registry/oci-layout mode for the same
  build. This is acceptable: rebase and layer-patching both work because the metadata
  label is rewritten to match the actual layers (above), and matching is by
  buildpack/layer identity + data (+ the now-accurate sha), not by cross-mode diffID
  identity. Realistically a user rebases/patches with the same tooling they built with.
- **(Future) Rebase-mode signaling:** optionally record the export mode on the image
  so tooling can behave consistently across modes (the user's suggestion). Not needed
  once the metadata SHAs are accurate, but a nice-to-have.

**Alternatives considered and rejected:**

- **Content-level blob append preserving exact diffIDs (rejected as unnecessary).**
  Would keep diffIDs identical to other modes, but requires self-assembling the OCI
  manifest/config from the emitted blobs (imgutil `layout.Image` or go-cr) — more
  complexity for a property (diffID identity) we do NOT actually need for rebase.
- **(c) Host-side assembly with go-containerregistry (rejected).** Preserves
  diffIDs but EGRESSES layer tars to the host — defeats Option C's no-egress
  purpose. Rejected.

> DOC FOLLOW-UP (after MVP fully vetted): capture this decision + the rejected
> alternatives in `internal/build/multiplatform/buildkit-multi-arch-readme.md`
> and in the RFC (`jericop/cnb-rfcs/text/0000-buildkit-multiarch-build.md`).
> Tracked so the rationale lands in the PR-facing README and the RFC, not only
> the spec.

#### 5a. SUPERSEDED — earlier content-level-blob-append research

> The sub-sections below researched appending precomputed layer BLOBS to preserve
> exact diffIDs (and the A1/A2 in-BuildKit-assembly sub-fork). That plan is
> SUPERSEDED by the revised decision above: because rebase needs only the metadata
> `RunImage.TopLayer` boundary (empirically proven), the simpler pure-LLB
> "FROM run-image" assembly is used instead, and diffID identity is NOT required.
> Kept for the record / in case a future need for exact diffID identity arises.

Verified against the versions pack vendors (buildkit v0.32.2, go-containerregistry
v0.21.9, imgutil fork, containerd/v2 v2.3.3):

- **There is NO `llb` primitive to append a precomputed layer blob by diffID.**
  `llb.Diff`/`llb.Merge` re-snapshot filesystems and let the exporter RECOMPUTE
  diffIDs. (This is now FINE — we use recomputed diffIDs deliberately.)
- If exact diffID identity is ever needed: self-assemble the finished OCI image
  (imgutil `layout.NewImage(base=run image)` + `AddLayerWithDiffIDAndHistory` +
  `Save`, or go-cr `tarball.LayerFromFile` + `mutate.Append` + `layout.Write`),
  then import via `llb.OCILayout` + `SolveOpt.OCIStores` and re-export via
  `ExporterImage` (pack's existing Phase-2 mechanism). Not needed for the MVP.
- **STILL RELEVANT — Multi-arch:** BuildKit has NO single-solve way to combine N
  per-arch OCI-layout stores into one native multi-platform `ExporterImage` in
  pack's `client.Solve` model. Pack's EXISTING `PushPerArchLayoutsAsManifestList`
  (go-containerregistry
  `mutate.AppendManifests` + `remote.WriteIndex`) assembles + pushes ONE index with
  no intermediate tags. Reuse it.

#### 5b. Sub-decision: WHERE assembly runs (A1 host-side vs A2 in-BuildKit)

Because the imgutil/ggcr assembly is host-side Go code, it needs the layer tar
BYTES on the host. This creates a sub-fork of Option A:

- **A1 — host-side assembly:** the emit graph exports `/layers` (tars) + `/emit`
  (contract) to the host; pack assembles with imgutil + pushes. Simplest, reuses
  pack machinery — BUT egresses the app layer tars to the host (the same layer-data
  egress Option C set out to avoid for large apps).
- **A2 — in-BuildKit assembly:** run the assembly as a step INSIDE BuildKit (a
  small helper doing the imgutil layout assembly against the in-build `/layers`),
  producing a finished OCI layout in the build, then `llb.OCILayout` +
  `ExporterImage`. Keeps layer data in BuildKit (true no-egress) but needs an
  assembly helper available in the build environment.

**DECISION: A2 (in-BuildKit assembly).** A2 preserves the no-egress property that
motivates Option C over Option B; A1 would reintroduce the very layer-tar egress
Option C exists to avoid, so shipping A1 as "Option C" would be misleading. A1 is
NOT used, not even as an interim scaffold.

#### 5c. A2 assembler lives in the LIFECYCLE (in-BuildKit), invoked as a RUN step

The assembly Go code (imgutil `layout.NewImage` + `AddLayerWithDiffIDAndHistory` +
`Save`) must run INSIDE the BuildKit build so the layer tars never leave. The
natural home is the LIFECYCLE binary, because:

- it is ALREADY staged in the build (`/cnb/lifecycle`, from the builder image /
  the emit-capable lifecycle image), so no second helper binary to build/stage;
- it ALREADY imports imgutil and owns CNB image assembly semantics;
- it already produced the emit contract + `/layers` tars in the same build, so it
  can consume them directly with no serialization round-trip.

**Refined seam:** rather than pack parsing the emit contract and re-implementing
assembly, the lifecycle gains an in-BuildKit ASSEMBLY step that reads its own emit
output (`/emit/buildkit/plan.json` + `config.json`) and the `/layers` tars and
writes a FINISHED per-arch OCI layout to a build path (e.g. `/output`). Pack then
imports that finished layout via `llb.OCILayout` + `OCIStore` and exports it via
`ExporterImage` — reusing pack's existing Phase-2 push mechanism
(`buildImportLayoutState`/`solvePhase2Push`/`phase2ExportEntry`) and, for
multi-arch, `PushPerArchLayoutsAsManifestList`.

This means the emit CONTRACT is now consumed IN-BUILD by the lifecycle assembler,
not on the host by pack. Pack still imports the `emit` types (for validation /
debugging / potential host-side inspection), and the contract remains the
documented interface, but the primary consumer of `plan.json`/`config.json` is the
in-BuildKit assembler. The JSON is still the transport across the exporter→assembler
step boundary (separate lifecycle invocations in the same build).

**Assembly options within A2 (lifecycle-side):**

- **A2a (recommended): the emit exporter's RecordingImage backs onto a
  layout.Image.** Since the RecordingImage already records every layer/config op,
  the simplest faithful assembler is to have it (or a sibling in-build command)
  drive an `imgutil/layout.Image` rooted on the run image: replay the recorded
  `AddLayerWithDiffIDAndHistory`/`ReuseLayerWithHistory`/`SetLabel`/`SetEnv`/
  `SetEntrypoint`/`SetWorkingDir`/`SetCmd` against the layout image and `Save` a
  finished OCI layout. This literally reuses the exporter's own op sequence, so
  parity with a normal export is structural, not re-derived.
- **A2b: a standalone lifecycle `-assemble` step** that reads `plan.json` +
  `config.json` from disk and rebuilds the layout image from them. Equivalent
  output; useful if the assembler must run as a separate phase from emit.

Both keep data in BuildKit and preserve diffIDs (exact tar bytes). Start with the
approach that minimizes new lifecycle surface; validate assembled-image parity via
the same manual check used for the emit contract.

## Why this is the most effective BuildKit integration

- No layer-data egress (fixes Option B's large-app cost).
- No sandbox push of intermediate tags, no OCI-layout wrapper (fixes Option A).
- Full BuildKit caching on assembly + native multi-arch export.
- Lifecycle stays authoritative for CNB semantics (small emit surface).

## Risks / open questions

- **Biggest change is in the lifecycle**: adding a clean emit-mode requires
  separating layer-plan/config computation from image mutation in the exporter
  (today interleaved in `addBuildpackLayers`/`addLauncherLayers`/`addAppLayers`/
  `setLabels`). Scope this as an additive mode, not a rewrite.
- **Layer diffID determinism**: the plan's diffIDs must match what BuildKit
  produces when it tars `/layers/<path>`; confirm the tar settings
  (ownership/timestamps) the lifecycle uses vs BuildKit's layer creation, or have
  the lifecycle emit the exact tar (as a file) that BuildKit adds by digest.
  Simplest safe path: lifecycle emits the actual layer TARs (small metadata +
  the tars already exist under /layers) and BuildKit adds them by precomputed
  diffID, guaranteeing rebase parity.
- **History + config fidelity**: BuildKit must apply per-layer history and the CNB
  config exactly; verify BuildKit's image config controls (entrypoint, env,
  labels, history) are expressive enough (they are for standard fields; confirm
  per-layer history).
- **Frontend vs pack-side LLB (C1 vs C2)**: start pack-side (C1). A lifecycle-as-
  frontend (C2) is a larger follow-up.
- **Platform API / lifecycle version**: pin to a lifecycle that has the emit-mode
  (built on the matching branch).

## Testing strategy (MVP, local)

Per `mvp-build-testing-strategy`: build + rebuild `samples/go/no-imports` AND a
large Node/Python app to the local registry via `--build-backend=buildkit-native`;
runnable check (real layers, CNB labels incl `io.buildpacks.lifecycle.metadata`,
launch binary present); confirm NO host layer-data egress; capture cold/warm
durations and compare to Option A and Option B. Prove rebase parity via run-image
base layer digests.

### Testing cleanup: no env-var-gated registry tests — use a local registry

Do NOT carry forward the Option A `PACK_TEST_*` env-var gating (e.g.
`PACK_TEST_REGISTRY_ENABLED`, `PACK_TEST_REGISTRY_REF`, `PACK_TEST_BUILDKIT_ENABLED`)
for this backend's validation. Use a LOCALLY-MANAGED registry the same way pack's
EXISTING test suite does (`testhelpers` spin up a local registry) plus the MVP
local build/rebuild strategy. Where related tests still use those env-var gates,
removing them in favor of a local registry is a cleanup task. Validation stays
local-first/MVP; this is about HOW the registry is provided, not about adding
heavyweight tests.

## Relationship to other specs

- **Lifecycle half (the other half of THIS effort):**
  `jericop/cnb-lifecycle@buildkit-native-export`
  (`.kiro/specs/buildkit-native-export/`) — owns the emit-mode and the emit
  contract that this pack spec consumes. The two are two halves of one effort; keep
  the emit contract `schema` in sync.
- Supersedes the need for `run-image-native-mount` if adopted (no `-pull-run-image`
  at all; run image is a native base).
- An alternative to `lifecycle-as-library-hybrid` (Option B); Option B is the
  quicker baseline AND the FALLBACK if Option C proves non-viable. Option C is the
  chosen direction (the efficient end-state); B is the last resort.
