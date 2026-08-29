---
inclusion: manual
---

# Verification: llb.OCILayout() for Native Image Export (BuildKit v0.30.0)

## Purpose

Verify whether `llb.OCILayout()` can be used to import an OCI layout produced by an earlier build step and re-export it as a (multi-platform) image via BuildKit's native `ExporterImage` — enabling elimination of intermediate tags without pack shelling out to `docker buildx` or re-pushing via go-containerregistry.

BuildKit version in the fork: **v0.30.0** (`github.com/moby/buildkit v0.30.0` in go.mod).

## Findings: The API Exists and Works As Needed

### 1. `llb.OCILayout()` — confirmed

From `client/llb/source.go` (v0.30.0):

```go
func OCILayout(ref string, opts ...OCILayoutOption) State {
    gi := &OCILayoutInfo{}
    for _, o := range opts {
        o.SetOCILayoutOption(gi)
    }
    attrs := map[string]string{}
    if gi.sessionID != "" { attrs[pb.AttrOCILayoutSessionID] = gi.sessionID }
    if gi.storeID != "" { attrs[pb.AttrOCILayoutStoreID] = gi.storeID }
    ...
    source := NewSource("oci-layout://"+ref, attrs, gi.Constraints)
    return NewState(source.Output())
}

func OCIStore(sessionID string, storeID string) OCILayoutOption { ... }
```

`llb.OCILayout(ref, llb.OCIStore("", storeID))` returns an `llb.State` sourced from an OCI layout that lives in a **named content store attached to the solve session**. The `ref` is a digest/tag within that store.

### 2. How the store is attached — `SolveOpt.OCIStores` — confirmed

From `client/solve.go` (v0.30.0):

```go
type SolveOpt struct {
    Exports   []ExportEntry
    OCIStores map[string]content.Store   // ← attach named content stores
    ...
}
```

And in `solve()`:

```go
contentStores := map[string]content.Store{}
for key, store := range opt.OCIStores {
    key2 := "oci:" + key
    contentStores[key2] = store
}
...
if len(contentStores) > 0 {
    s.Allow(sessioncontent.NewAttachable(contentStores))
}
```

So pack can attach an OCI layout directory as a content store: build a `content.Store` from the directory (via `contentlocal.NewStore(dir)`) and pass it in `OCIStores["<storeID>"]`. The LLB graph then references it with `llb.OCILayout(ref, llb.OCIStore("", "<storeID>"))`.

### 3. Native image export — `ExporterImage` — confirmed

The `ExportEntry` supports image export with push:

```go
type ExportEntry struct {
    Type        string
    Attrs       map[string]string
    Output      filesync.FileOutputFunc
    OutputDir   string
    OutputStore content.Store
}
```

With `Type: "image"` (ExporterImage) and `Attrs: {"name": "<ref>", "push": "true"}`, BuildKit pushes the resulting image natively. For multi-platform, when the LLB graph produces per-platform states and the solve is configured for multi-platform, BuildKit assembles and pushes the manifest list itself.

## Viable Design (Confirmed)

The two-phase LLB flow works within a single build session:

### Phase 1: Run lifecycle, produce OCI layout
- Construct the LLB graph for lifecycle phases (analyze/detect/restore/build/export)
- The exporter runs with `-layout -layout-dir /output -pull-run-image -skip-chown`
- Export Phase 1's `/output` to a local OCI store on the host via `ExporterLocal` or `ExporterOCI` (OutputDir)
- Result: a real OCI layout on disk with real layer blobs, config, manifest — identical bytes to what a registry push would produce (PARITY GUARANTEED)

### Phase 2: Import the layout, export as image
- Build a `content.Store` from the Phase 1 output dir (`contentlocal.NewStore`)
- Attach it via `SolveOpt.OCIStores["applayout"] = store`
- LLB graph: `finalState := llb.OCILayout("<image>@<digest>", llb.OCIStore("", "applayout"))`
- Export with `ExporterImage`, `push=true`
- BuildKit pushes natively; for multi-arch it assembles the manifest list

### Alternative: Single-session, in-memory store
Instead of writing to disk between phases, the same `content.Store` instance can be shared: the OCI exporter writes to a store (via `ExportEntry.OutputStore`), and the next solve reads from the same store via `OCIStores`. This avoids the disk round-trip but requires careful session/store lifecycle management. Start with the disk-based approach (simpler, debuggable) and optimize later if needed.

## Why This Preserves Parity

The lifecycle produces the OCI layout using the SAME export code path as registry/daemon mode (`initLayoutAppImage` alongside `initRemoteAppImage`/`initDaemonAppImage`). The layer blobs, diff IDs, config, and labels are the actual final image bytes. When BuildKit imports the layout via `llb.OCILayout()` and re-exports via `ExporterImage`, it uses those exact blobs — no re-tarring, no serialization drift. The pushed image is bit-for-bit what the lifecycle produced.

This is the decisive advantage over the decomposed `-export-mode layers` contract, which requires the consuming tool to re-tar expanded directories (introducing diff-ID drift risk).

## Caching Implications: Options A, B, C

Recall the earlier options for how the lifecycle delivers output:
- **Option A**: Lifecycle writes OCI layout (expanded), BuildKit consumes via `llb.OCILayout()`
- **Option B**: Lifecycle writes OCI tarball, BuildKit consumes via OCI store
- **Option C**: Lifecycle writes decomposed layers (the `-export-mode layers` contract)

### Option A: OCI layout + llb.OCILayout()

- **Layer caching**: The layout is imported as image blobs. BuildKit's `ExporterImage` push is content-addressable — unchanged blobs already at the registry are not re-uploaded. But BuildKit does NOT re-cache individual layers as build steps (the layout is a single source node).
- **Build-step caching**: The lifecycle RUN steps (Phase 1) are cached per BuildKit's normal rules (everything after source COPY re-runs). The import+export (Phase 2) is cheap regardless.
- **Cross-build benefit**: Minimal beyond registry-level blob dedup. The layout source node's cache key is the layout digest, which changes whenever the app changes.
- **Verdict**: Best parity, simplest, adequate caching. Registry dedup handles unchanged layers.

### Option B: OCI tarball + OCI store

- **Layer caching**: Similar to A — the tarball is imported as image content. Content-addressable dedup on push applies.
- **Build-step caching**: Same as A.
- **Difference from A**: A tarball is a single file vs. an expanded directory tree. `ExporterOCI` produces a tarball natively; importing it may require unpacking into a content store first. Marginally more I/O to pack/unpack, but no caching advantage over A.
- **Verdict**: Functionally equivalent to A for caching. Choose based on which is easier to wire (A's expanded layout maps directly to `contentlocal.NewStore`; B's tarball needs an unpack step).

### Option C: Decomposed layers (-export-mode layers)

- **Layer caching**: This is the ONLY option that enables per-layer BuildKit build-step caching, because each layer becomes a separate `llb.Copy` node. If a layer's content is unchanged, that COPY node is a cache hit.
- **Build-step caching**: Same lifecycle-phase limitation (phases after source re-run), BUT the final image assembly COPYs can be cached individually.
- **Cross-build benefit**: Highest IF combined with pre-copy/multi-stage execution (see `pre-copy-buildpack-caching.md`). Without that, the benefit is marginal because the lifecycle already ran and produced the layers.
- **Cost**: Requires the consuming tool to re-tar layers → diff-ID parity risk. Requires lifecycle work to decompose and write the contract.
- **Verdict**: Best theoretical caching, but only meaningfully better when paired with pre-copy execution. Introduces parity risk. Deferred.

### Caching Summary

| | Option A (layout) | Option B (tarball) | Option C (decomposed) |
|---|---|---|---|
| Parity guarantee | Strong (real image bytes) | Strong (real image bytes) | Weaker (re-tar risk) |
| Registry blob dedup | Yes | Yes | Yes |
| Per-layer build-step cache | No | No | Yes |
| Cross-build cache benefit | Registry dedup only | Registry dedup only | High only WITH pre-copy |
| Lifecycle work | None (existing -layout) | Small (tarball format) | Significant (contract) |
| Consuming-tool complexity | Low (llb.OCILayout) | Medium (unpack tarball) | Medium (COPY per layer) |
| Diff-ID drift risk | None | None | Yes |

## Recommendation

**Use Option A** (OCI layout + `llb.OCILayout()` + `ExporterImage`) for the LLB backend:
- Confirmed viable in BuildKit v0.30.0
- Strongest parity (uses the lifecycle's actual image bytes)
- No new lifecycle work (existing `-layout` + `-pull-run-image` + `-skip-chown`)
- No `docker buildx` shell-out; BuildKit pushes natively and assembles the manifest list
- Registry content-addressable dedup handles unchanged-layer efficiency

The per-layer caching advantage of Option C only becomes worthwhile if pre-copy/multi-stage buildpack execution is implemented. Revisit Option C then. For now, Option A gives us tag elimination, parity, and native push with the least risk.

## Open Items to Prototype

1. ~~Confirm the multi-platform manifest list is assembled correctly when each platform's image comes from a separate per-arch OCI layout imported via `llb.OCILayout()`.~~ **RESOLVED (Task 3):** native single-solve assembly across separate per-arch stores is not feasible with the `client.Solve` API; we use Option (b) — assemble the manifest list from the per-arch OCI layouts and push one index with no intermediate tags (the design's documented fallback). See "Phase 3 Decision & Findings" below.
2. Decide disk-based two-solve vs shared in-memory store (start disk-based).
3. Confirm `ExporterImage` with `push=true` uses the pack-resolved registry credentials (session auth provider) correctly.
4. Verify the run image base layers are correctly referenced (the layout includes them since the lifecycle exported a complete image FROM the run image).

## Phase 1 Prototype Findings & Limitations (oci-layout-tag-elimination, Task 1)

Recorded from prototyping Phase 1 (produce the `/output` OCI layout) in the LLB backend
(`internal/build/multiplatform/`). These consolidate what was learned while implementing
`buildLifecyclePhaseArgs()`, `isolateOutputLayout()`, the `client.ExporterOCI` +
`OutputStore` wiring to a per-arch content store, and `InspectOCILayout()`.
References: FR-3, FR-4, design.md "Open Items to Prototype".

### What Phase 1 resolved
- **Open item 5 (isolate `/output`)**: Resolved. `isolateOutputLayout()` copies `/output`
  into `llb.Scratch()` so the exported state root **is** the OCI layout (rather than the
  full container root). Confirmed via `InspectOCILayout()` reading `index.json` +
  `oci-layout` from the store root.
- **Exporter/analyzer phase args**: Confirmed. In OCI layout mode `buildLifecyclePhaseArgs()`
  emits `-layout -layout-dir /output` on the exporter/analyzer and `-pull-run-image` so the
  run image is self-populated inside BuildKit (`-skip-chown` retained for the unprivileged
  environment).
- **Phase 1 export**: Confirmed. `client.ExporterOCI` with a per-arch `OutputStore`
  (`perArchStoreDir`) produces a complete, inspectable OCI layout on disk.

### Limitations discovered (carry into later tasks)

1. **Pre-existing, unrelated test failure in the package.**
   `TestMultiplatform/.../GenerateDockerfileMultiPlatform/includes_secret_mount_only_on_analyzer_and_exporter`
   fails on the baseline, independent of this spec's changes. It lives in the Dockerfile
   generator (`dockerfile_generator.go`) and is a whitespace / secret-mount-vs-
   `CNB_REGISTRY_AUTH`-env mismatch in the Dockerfile backend (out of scope for this spec —
   the Dockerfile backend is the MVP). Do not attribute this failure to the LLB OCI layout
   work; it should be triaged separately.

2. **No live BuildKit-produced layout inspected in unit tests.**
   Verifying a real Phase 1 store requires a BuildKit daemon. Current unit verification uses
   a synthetic go-containerregistry OCI layout fixture. The fixture's byte format should be
   confirmed byte-compatible against a real Phase 1 store during integration (Task 7).

3. **`InspectOCILayout()` assumes exactly one image manifest per layout dir.**
   This matches the per-arch design (one layout per platform). If a multi-arch index ever
   lands in a single store, the inspector would need a platform selector to disambiguate.

4. **Inspector is shallow by design.**
   `InspectOCILayout()` checks blob existence/readability and manifest/config presence; it
   does NOT do deep digest re-hashing, SBOM validation, or lifecycle-metadata assertion.
   Those deeper parity checks are deferred to Task 7 (on-disk OCI layout test) and Task 8
   (parity check).

5. **`oci-layout` marker not hard-asserted by the inspector.**
   Layout validity is established via `layout.FromPath` (which reads/validates `index.json`)
   rather than an explicit assertion on the `oci-layout` marker file. Acceptable for Phase 1;
   noted in case a stricter structural check is wanted later.

### Impact on remaining open items
- Open items 1 (multi-platform manifest list assembly), 2 (disk vs in-memory store), and
  3 (native push credentials) are Phase 2 / Task 2–3 concerns and remain open.
- Open item 4 (run image base layers referenced in the layout) is expected to hold by
  construction (the lifecycle exported a complete image FROM the run image) but has only been
  observed via the shallow inspector — deep confirmation is deferred to Task 7/8.

## Phase 2 Prototype Findings & Verification (oci-layout-tag-elimination, Task 2)

Recorded from prototyping Phase 2 (import the Phase 1 OCI layout via `llb.OCILayout`
and push natively via `ExporterImage`) in the LLB backend
(`internal/build/multiplatform/backend_llb.go`). These cover the two-phase solve
wiring (`solvePhase2Push`, `buildImportLayoutState`, `buildImportRef`,
`phase2ExportEntry`) and the "no intermediate tag" guarantee.
References: FR-4, FR-5, design.md "Open Items to Prototype" item 3.

### What Phase 2 resolved
- **Import wiring**: Confirmed. `buildImportLayoutState(importRef, applayoutStoreID)`
  builds `llb.OCILayout(ref, llb.OCIStore("", storeID))`; the marshaled source is
  `oci-layout://<ref>` with `oci.store=<storeID>` and no `oci.session` attr, so the
  import resolves against the store attached via `SolveOpt.OCIStores[applayoutStoreID]`.
  The single `applayoutStoreID` constant feeds both the `llb.OCIStore` storeID and the
  `OCIStores` map key, so they cannot drift (design Tier 1 store-wiring requirement).
- **Native push export**: Confirmed. Phase 2 exports via a single
  `client.ExporterImage` entry with `Attrs{"name": pushName, "push": "true"}`
  (extracted into `phase2ExportEntry(pushName)` for unit-testability without a Solve).
- **Credentials (open item 3)**: Resolved by construction. A single
  `newDockerAuthProvider()` session provider is built once in `solvePlatform` and
  shared by both the Phase 1 and Phase 2 solves, so the native `ExporterImage` push
  authenticates with the same pack-resolved (Docker config) registry credentials.
  End-to-end confirmation that the push succeeds against a real registry is deferred
  to Task 10 (needs a live daemon + registry).

### The "no intermediate tag" guarantee — WHERE it comes from
The guarantee is structural, not a runtime check. In OCI layout mode, no code path
pushes a per-arch `<img>-build-<id>-<arch>` tag to a registry:

1. **Lifecycle (inside BuildKit) never pushes.** `buildLifecyclePhaseArgs()` runs the
   exporter/analyzer with `-layout -layout-dir /output` (and `-pull-run-image`), so
   the lifecycle writes a complete OCI image to `/output` on the build filesystem
   rather than performing a registry push. The `<img>-build-<id>-<arch>` string
   (`perArchTag`) is used only to name the on-disk lifecycle target and content-store
   leaf — it is never a registry push target.
2. **Phase 1 exports locally only.** `solvePlatform` configures `client.ExporterOCI`
   with an `OutputStore` pointing at a per-arch on-disk content store
   (`perArchStoreDir`). No registry interaction.
3. **Phase 2 is the sole registry write, under the final name.** `solvePhase2Push`
   exports exactly one `phase2ExportEntry(pushName)` = `ExporterImage` with
   `name=pushName`, `push=true`. For the single-arch prototype scope, `pushName` is
   the target image name passed through; the intermediate `-build-` shape is never
   the export name.

### How it is verified without a daemon (this task's scope)
- **Code inspection**: the three points above.
- **Unit assertions** (`backend_llb_internal_test.go`, `#phase2ExportEntry`, no daemon):
  - exactly one export entry, `Type == client.ExporterImage`;
  - `Attrs["push"] == "true"` (native push);
  - `Attrs["name"] == <final target>`, and explicitly `!= perArchTag` and does not
    contain `-build-` (proves the push target is not an intermediate per-arch tag);
  - no stray `ExporterOCI`/`ExporterLocal` entry (single push, no duplicate artifact).
  - Existing `#buildImportLayoutState` / store-wiring tests confirm the import reads
    from the attached content store (not a registry).

### Deferred to Task 10 (env-var-gated registry integration test)
- Live end-to-end push against a real registry (`PACK_TEST_REGISTRY_ENABLED`),
  skipped by default per design Testing Strategy Tier 3.
- Registry-side confirmation that ONLY the final manifest-list / image tag exists and
  NO `<img>-build-<id>-<arch>` tag was created.
- Multi-platform manifest-list assembly across separate per-arch layouts is Task 3;
  the executor still returns a "not yet fully implemented" error for the publish +
  manifest-list path (adjusting that is Task 5's scope).

### Limitations / notes (carry forward)
1. **Single-arch scope.** This sub-item verifies the single-arch push shape only. The
   executor's `Execute` still errors on `ExportOCILayout` when assembling a manifest
   list (Task 5 removes that; Task 3 defines native multi-arch assembly).
2. **No live push exercised here.** As with Phase 1, unit verification uses the
   marshaled LLB/export config, not a BuildKit-produced push. Byte-level and
   registry-level confirmation is Task 7/8 (on-disk parity) and Task 10 (registry).
3. **Pre-existing, unrelated test failure remains.** The Dockerfile-generator
   `TestMultiplatform/.../includes_secret_mount_only_on_analyzer_and_exporter` failure
   noted under Phase 1 is still present on the baseline and is out of scope (Dockerfile
   backend MVP). Do not attribute it to the LLB OCI layout work.

## Phase 3 Decision & Findings — Multi-platform manifest list assembly (oci-layout-tag-elimination, Task 3)

Recorded from implementing multi-arch manifest-list assembly for the LLB OCI
layout path (`internal/build/multiplatform/backend_llb.go` +
`oci_layout_push.go`). References: FR-5, design.md "Risk: Multi-platform manifest
list assembly via ExporterImage", open item 1 (below).

### The question (open item 1)
How do we get a multi-arch manifest list from per-arch images that originate from
SEPARATE per-arch OCI layouts (one content store per platform), given the LLB
backend uses `client.Solve` per platform?

### Options considered
- **Option (a) — single multi-platform solve**: one solve produces all platforms
  and BuildKit's `ExporterImage` assembles + pushes the manifest list natively.
- **Option (b) — combine per-arch results**: keep per-arch Phase 1 (each arch in
  its own store), then assemble the final manifest list from the per-arch
  content-store images and push it atomically.

### DECISION: Option (b) — combine per-arch results (assemble from layouts)
Rationale (why not Option a):
- The LLB backend drives builds with the **`client.Solve` API, not the gateway /
  frontend API**, and each platform is solved independently against its OWN
  per-arch content store (FR-4: "Each parallel platform MUST use its own content
  store"). A single `llb.OCILayout()` source resolves to exactly ONE platform's
  layout. There is no `client.Solve` knob to feed N separate per-arch OCI-layout
  stores into a single multi-platform `ExporterImage`.
- Native cross-store assembly would require a **gateway frontend** returning a
  result that carries per-platform refs (`exptypes.RefsKey` /
  `exptypes.ExporterImageConfigKey` + `platforms`). That is a materially different
  execution model than the current `client.Solve`-per-platform code — a large
  rewrite for no correctness gain here.
- design.md explicitly authorizes this as the fallback: "If native assembly across
  separate solves is awkward, fall back to `PushOCILayoutAsManifestList` for the
  assembly step while still using native per-arch push."
- The assemble-from-layouts path creates **NO intermediate tags**: it reads the
  per-arch layouts and pushes ONLY the final image index via `remote.WriteIndex`.
  This directly satisfies FR-5's "no intermediate tags" for the assembly step.

The decision is also recorded in code as the package doc of `oci_layout_push.go`.

### Implementation
- **`oci_layout_push.go` refactored** to separate a pure, testable assembly from
  the network push:
  - `AssembleManifestList([]PerArchLayout) (v1.ImageIndex, error)` — PURE,
    network-free. Reads each per-arch OCI layout from disk, selects the recorded
    manifest digest (`OCILayoutDigest`, falling back to the sole image manifest
    when empty), and builds a `v1.ImageIndex` via `mutate.AppendManifests` with
    exactly one entry per platform, each carrying the correct os/arch/variant
    `v1.Platform` descriptor.
  - `pushIndex(...)` — the ONLY network step; `remote.WriteIndex` pushes the
    assembled index atomically (all per-arch blobs + manifests + index in one
    op), so the registry never sees a partial state and no per-arch tag appears.
  - `PushPerArchLayoutsAsManifestList(...)` — the LLB entry point; maps
    per-arch `PlatformBuildResult`s (`OCIStoreDir` + `OCILayoutDigest` + `Platform`)
    to `PerArchLayout`s, assembles, and pushes.
  - `PushOCILayoutAsManifestList(...)` retained as the legacy fallback for the
    `<outputDir>/<os>/<arch>` directory shape; now delegates to the shared
    assembler (no more separate per-arch source logic, no "NOT YET FUNCTIONAL"
    header).
- **`backend_llb.go`**:
  - `BuildMultiPlatform` now decides per-arch push behavior:
    `assembleManifestList = (ExportMode==oci-layout && len(platforms)>1)`,
    `pushPerArch = !assembleManifestList`.
  - Multi-arch OCI layout mode: platforms run Phase 1 ONLY (produce per-arch
    layout in their own store); after the errgroup, `assembleAndPushManifestList`
    → `PushPerArchLayoutsAsManifestList` pushes ONE index under the final name.
    No per-arch `-build-<id>-<arch>` push happens.
  - Single-arch OCI layout mode: unchanged Phase 2 native push, but under the
    FINAL image name (`opts.ImageName`), not the intermediate per-arch build tag.
  - Registry mode (default): completely unchanged.

### Verification WITHOUT a daemon (this task's scope)
- Unit tests in `oci_layout_push_test.go` assemble the index in-memory from
  synthetic per-arch OCI layout fixtures (`random.Image` per platform, each
  written to its own dir) and assert:
  - the resulting `v1.ImageIndex` has EXACTLY one manifest entry per platform;
  - each entry's `Platform` descriptor has the correct os/arch/variant;
  - the index references the per-arch images BY DIGEST (content-addressed) —
    proving assembly is by content, not by any intermediate tag name;
  - the results→layouts mapping (`perArchLayoutsFromResults`) carries
    `OCIStoreDir` + `OCILayoutDigest` + `Platform`;
  - error cases (no layouts, missing dir, non-layout dir, absent digest).
- The "no intermediate tags" property is structural, verified by construction +
  unit assertions: the only registry write is the final `remote.WriteIndex`; no
  code path pushes a `<img>-build-<id>-<arch>` tag when assembling.

### Deferred to Task 10 (env-var-gated registry integration)
- LIVE push verification (needs a daemon + registry): confirm the pushed manifest
  list has an entry per platform AND that NO intermediate per-arch tag exists on
  the registry. Deferred per design Testing Strategy Tier 3
  (`PACK_TEST_REGISTRY_ENABLED`), skipped by default.

### Note for Task 5 (executor)
The executor still returns "oci-layout export mode is not yet fully implemented"
for the publish + manifest-list path. That error path is **Task 5's scope** and
was intentionally NOT modified here. Task 5 must route the LLB OCI-layout publish
path to this new assembly: the LLB backend now pushes natively (single-arch) or
assembles the manifest list itself (multi-arch) inside `BuildMultiPlatform`, so
the executor MUST NOT double-push — it should skip its own assembly for the LLB
native path (e.g. via a `BackendCapabilities.PushesNatively` flag).

### Open item 1 status: RESOLVED (via Option b fallback)
Multi-platform manifest list assembly from separate per-arch OCI layouts is
implemented by combining per-arch results and pushing one index with no
intermediate tags. Native single-solve assembly (Option a) is NOT used and would
require switching to the gateway frontend API — revisit only if that migration
happens for other reasons.
