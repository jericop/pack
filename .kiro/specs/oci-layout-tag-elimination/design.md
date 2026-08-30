# Design: LLB Native Image Export to Eliminate Intermediate Tags

> **STATUS — SUPERSEDED (historical spike).** The OCI-layout approach described here
> (LLB backend + `llb.OCILayout()` + native `ExporterImage` + `-pull-run-image`) was
> NOT the final implementation. It, the LLB/Dockerfile backends, the `oci_layout_*.go`
> files, and the `-pull-run-image` flag have all been REMOVED. The implemented
> approach is the single `buildkit` backend (build-then-finalize) — see the
> `buildkit-native-export` spec and `buildkit-multiarch` steering. Retained only as a
> record of the spike.

## Scope

This work targets the **LLB backend only**. The Dockerfile backend remains as-is: it is an MVP that proves the concept using registry mode with intermediate per-arch tags. Intermediate tags on the Dockerfile path are acceptable and out of scope for elimination — that path exists only to demonstrate feasibility and will not be the shipped approach.

The LLB backend is the long-term target. It uses BuildKit's `llb.OCILayout()` source and native `ExporterImage` to push images and assemble the manifest list without intermediate tags, without shelling out to `docker buildx`, and without pack re-pushing via go-containerregistry.

## Approach: Option A (verified)

See steering `llb-ocilayout-verification.md` for the API verification against BuildKit v0.30.0. Summary:

- The lifecycle exports a complete OCI image to `/output` (existing `-layout -layout-dir` + `-pull-run-image` + `-skip-chown`)
- BuildKit imports that layout via `llb.OCILayout(ref, llb.OCIStore("", storeID))`, with the layout attached through `SolveOpt.OCIStores`
- BuildKit re-exports the image via `ExporterImage` with `push=true`; for multi-platform it assembles the manifest list natively
- Parity is guaranteed because the pushed image uses the lifecycle's exact layer blobs (no re-tarring, no diff-ID drift)

## Architecture

```
pack build --buildkit --build-backend=buildkit-llb --buildkit-export-mode=oci-layout
           --platforms linux/amd64,linux/arm64 --publish ...
                        │
                        ▼
┌─────────────────────────────────────────────────────┐
│  Pack (pkg/client/build.go)                          │
│  - Resolves builder, run image, buildpack order      │
│  - Sets ExportMode = oci-layout                      │
│  - Dispatches to LLBBackend                          │
└───────────────────┬───────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────────────────┐
│  LLBBackend.BuildMultiPlatform()                     │
│  Per platform (parallel errgroup):                   │
│                                                      │
│  PHASE 1 — produce OCI layout:                       │
│   - LLB graph: lifecycle phases                      │
│     exporter runs: -layout -layout-dir /output       │
│       -pull-run-image -skip-chown                    │
│   - Solve with an OCI-layout export to a per-arch    │
│     content store (ExporterOCI → OutputStore/Dir)    │
│                                                      │
│  PHASE 2 — import layout, export image:              │
│   - Attach the store via SolveOpt.OCIStores          │
│   - finalState = llb.OCILayout(ref, OCIStore(...))   │
│   - Solve with ExporterImage {name, push=true}       │
└───────────────────┬───────────────────────────────────┘
                    │  (per-arch images now in BuildKit's content store / pushed)
                    ▼
┌─────────────────────────────────────────────────────┐
│  Manifest list assembly                              │
│  - BuildKit assembles + pushes the manifest list     │
│    natively from the multi-platform result           │
│  - NO intermediate tags, NO pack-side push           │
└─────────────────────────────────────────────────────┘
```

## Current State

The fork already has:
- `backend_llb.go` — LLB backend that runs lifecycle phases and solves per platform (registry mode; no exports configured today)
- `oci_layout_push.go` with `PushOCILayoutAsManifestList()` — a go-containerregistry fallback (may be retained as a non-LLB fallback, but the LLB path prefers native `ExporterImage`)
- `dockerfile_generator.go` with `convertToOCILayoutArgs()` — the LLB backend mirrors these phase args
- Lifecycle flags `-pull-run-image` and `-skip-chown` now exist (were unavailable when the code was first written)

What needs to change (LLB backend):
- Add the two-phase solve: produce OCI layout, then import + export as image
- Configure `SolveOpt.OCIStores` and `ExporterImage`
- Ensure the exporter phase uses `-layout -layout-dir /output -pull-run-image -skip-chown`
- Wire pack-resolved registry credentials into the push (session auth provider)

## Key Design Decisions

### 1. No new lifecycle work

The lifecycle's existing `-layout` mode produces the complete OCI image. `-pull-run-image` lets it self-populate the run image inside BuildKit. `-skip-chown` handles the unprivileged environment. Nothing new is needed in the lifecycle.

### 2. Native BuildKit push via ExporterImage (no pack-side push for LLB)

Rather than extracting the layout to disk and pushing via go-containerregistry (`PushOCILayoutAsManifestList`), the LLB backend imports the layout with `llb.OCILayout()` and lets BuildKit push it via `ExporterImage`. BuildKit assembles the multi-platform manifest list natively. This keeps image data in BuildKit's content store and produces an atomic, tag-free push.

### 3. Two-solve flow (start disk-based)

Phase 1 exports the OCI layout to a local content store (on disk). Phase 2 attaches that store and imports via `llb.OCILayout()`. Starting disk-based keeps the flow debuggable. A shared in-memory `content.Store` (via `ExportEntry.OutputStore` feeding the next solve's `OCIStores`) is a later optimization.

### 4. Parity guaranteed by construction

The lifecycle produces the OCI layout using the same export code path as registry/daemon mode. BuildKit imports and re-exports those exact blobs. The pushed image is bit-for-bit what the lifecycle produced — same diff IDs, same `io.buildpacks.lifecycle.metadata`, same config. Rebase works identically. See cnb-lifecycle steering `layer-order-and-rebase.md`.

### 5. Dockerfile backend unchanged

The Dockerfile backend keeps registry mode with intermediate tags. It is the MVP and is not part of the tag-elimination effort. No changes to `backend_dockerfile.go` or its OCI-layout stubs are required by this spec.

## Implementation Plan

### Files to modify:

| File | Change |
|------|--------|
| `internal/build/multiplatform/backend_llb.go` | Implement the two-phase solve: (1) lifecycle → OCI layout export to a per-arch content store; (2) `llb.OCILayout()` import → `ExporterImage` push. Wire registry auth into the push. This is the core work. |
| `internal/build/multiplatform/executor.go` | For LLB + OCI layout mode, the LLB backend handles the push natively; the executor should NOT call `PushOCILayoutAsManifestList` for this path. Adjust the `ExportOCILayout` branch so it does not error and does not double-push for the LLB backend. |
| `internal/build/multiplatform/backend.go` | If needed, extend `BackendCapabilities` or add a capability flag indicating the backend performs its own native push (so the executor skips manifest assembly). |
| `internal/build/multiplatform/oci_layout_push.go` | Retain as an optional fallback (e.g., for a future non-native path). Remove the "NOT YET FUNCTIONAL" comment if the fallback is kept; otherwise mark clearly as fallback-only. |

### backend_llb.go: the two-phase solve

Phase 1 — produce the OCI layout:
```go
// Build the lifecycle LLB graph (analyze/detect/restore/build/export)
// exporter args include: -layout -layout-dir /output -pull-run-image -skip-chown
state := b.buildLLBState(ociLayoutOpts, platform, imageName)
// isolate /output as the export root (or use ExporterOCI on the /output subtree)
def, _ := state.Marshal(ctx, llb.Platform(platformSpec))

// Export the OCI layout to a per-arch content store on disk
store, _ := contentlocal.NewStore(perArchStoreDir)
_, err := bkClient.Solve(ctx, def, client.SolveOpt{
    LocalMounts: map[string]fsutil.FS{"context": appFS},
    Exports: []client.ExportEntry{{
        Type:        client.ExporterOCI,
        OutputStore: store,       // or OutputDir: perArchStoreDir
    }},
}, ch)
```

Phase 2 — import the layout and push as an image:
```go
finalState := llb.OCILayout(
    fmt.Sprintf("%s@%s", imageName, layoutDigest),
    llb.OCIStore("", "applayout"),
)
finalDef, _ := finalState.Marshal(ctx, llb.Platform(platformSpec))
_, err = bkClient.Solve(ctx, finalDef, client.SolveOpt{
    OCIStores: map[string]content.Store{"applayout": store},
    Exports: []client.ExportEntry{{
        Type:  client.ExporterImage,
        Attrs: map[string]string{"name": manifestListName, "push": "true"},
    }},
    Session: []session.Attachable{authProvider}, // pack-resolved registry creds
}, ch)
```

Multi-platform: run the above per platform in the errgroup, then have BuildKit assemble the manifest list. Confirm during prototyping whether the manifest list is best assembled by a single multi-platform solve or by combining per-arch results (see open items).

## Testing Strategy

The strategy leads with **non-registry (on-disk) testing** and treats registry-based testing as optional. This is possible because Option A's Phase 1 produces a complete OCI layout on disk with the real image bytes — we can validate parity and structure without ever pushing.

### Why non-registry testing works here

The critical correctness properties — diff IDs, layer order, `io.buildpacks.lifecycle.metadata`, image config — are all present in the Phase 1 OCI layout on disk. Reading the layout's manifest and config blobs directly lets us verify everything that matters for parity and rebase, with no registry and no network dependency on the builder.

This also sidesteps the BuildKit builder network-isolation problem: the `docker-container` driver runs on its own network and cannot reach a host-local ephemeral registry unless the builder was created with a shared network. By validating on-disk, we avoid that entirely for the core tests.

### Tier 1: Unit tests (no registry, no BuildKit)
- LLB graph construction for OCI layout mode includes `-layout -layout-dir /output -pull-run-image -skip-chown` on the correct phases
- Store wiring: `OCIStores` key matches the `llb.OCIStore` storeID
- Path/store plumbing for per-arch outputs

### Tier 2: On-disk OCI layout tests (BuildKit, no registry) — PRIMARY
- Build with the LLB backend in OCI layout mode, stopping after Phase 1 (or capturing the Phase 1 store)
- Read the per-arch OCI layout from disk (manifest + config blobs)
- Verify: layer count and order, each layer's diff ID, `io.buildpacks.lifecycle.metadata`, image config (entrypoint, env, user, workdir, ports), SBOM layer presence
- No push, no registry, no network dependency on the builder

### Tier 2 parity check (the confidence check, no registry) — PRIMARY
- Build the same app two ways and compare on-disk artifacts:
  1. Registry mode (Dockerfile MVP) — capture the image (can inspect a locally saved copy or the layout the lifecycle would produce)
  2. LLB OCI layout mode — read the Phase 1 OCI layout on disk
- Compare diff IDs in `io.buildpacks.lifecycle.metadata` — must match
- Compare image config and labels — must match
- This runs offline and deterministically

Note: for the registry-mode reference, prefer capturing its layers/metadata without requiring a live registry (e.g., save to daemon or an OCI layout for comparison). The goal is an apples-to-apples on-disk comparison.

### Tier 3: Registry-based integration tests (OPTIONAL, env-var gated)
Needed to verify the end-to-end native push AND that the actual pushed artifacts match across modes (not just on-disk diff IDs, but the real registry-hosted blobs and manifest list). This is the stronger verification, but kept optional because of the builder network-isolation caveat and credential requirements.

**Gating: environment variable, skipped by default.** These tests MUST be skipped unless an env var (e.g., `PACK_TEST_REGISTRY_ENABLED=1`, plus a registry ref via something like `PACK_TEST_REGISTRY_REF`) is set. This keeps the default `go test` run fast and free of network/registry/builder-network requirements. When we ship, the RFC will clearly document how to enable these tests and what setup they require.

- Preferred: push to a real registry the builder can already reach (e.g., Docker Hub / GHCR / ECR scratch repo), as the existing `pr-compliance-app` CI does
- Alternative (local isolation): ephemeral registry on the builder's shared network:
  ```bash
  docker network create pack-test
  docker run -d --name test-registry --network pack-test -p 5000:5000 registry:2
  docker buildx create --name pack-multiplatform --driver docker-container \
    --driver-opt network=pack-test --bootstrap
  # reference the registry by container name: test-registry:5000
  ```
  Caveat: the builder MUST be created with `--driver-opt network=pack-test` or it cannot reach the ephemeral registry.
- Verify: NO intermediate tags on the registry (only the final manifest list tag); manifest list has an entry per platform; each platform image pulls and runs
- Verify (stronger parity): pull the pushed per-arch images and compare their layer blobs/manifests against a registry-mode build of the same app — confirms identical artifacts land on the registry, not just matching on-disk diff IDs

**Future alternative to env-var gating:** auto-detect whether the current default builder supports local network access (and thus can reach a local ephemeral registry), and enable the registry tests automatically when it does. This is harder to implement reliably (inspecting builder driver options, network config) and is deferred. Start with the env-var approach; revisit auto-detection later if it proves worthwhile. Document the chosen approach in the RFC when the feature is working.

### Rebase tests
- Rebase an LLB-OCI-layout-built image with a new run image → verify success
- Multi-arch rebase → verify both platforms rebased
- Rebase can be validated against a locally-loaded image where possible; a registry is only needed if the rebase path requires remote layer mounting
- See cnb-lifecycle steering `layer-order-and-rebase.md`

## Risks and Mitigations

### Risk: Isolating /output for the OCI export
The exporter writes the OCI layout under `/output`. Phase 1's export must capture that layout (not the whole container root).
**Mitigation**: Prototype early. Options: export `ExporterOCI` of the `/output` subtree, or copy `/output` into `llb.Scratch()` before export. Decide empirically.

### Risk: Multi-platform manifest list assembly via ExporterImage
Confirm BuildKit assembles the manifest list correctly when per-arch images originate from separate per-arch OCI layouts imported via `llb.OCILayout()`.
**Mitigation**: Prototype the multi-platform export path early (open item). If native assembly across separate solves is awkward, fall back to `PushOCILayoutAsManifestList` for the assembly step while still using native per-arch push.

### Risk: Registry credentials for the native push
`ExporterImage` with `push=true` needs registry auth.
**Mitigation**: Attach a Docker auth session provider (`authprovider.NewDockerAuthProvider`) to the Phase 2 solve, consistent with how the LLB backend already handles buildkit's own registry operations.

### Risk: Diff ID / parity drift
Any divergence between LLB OCI layout output and registry mode would break parity/rebase.
**Mitigation**: The same lifecycle export path is used, so blobs are identical. Verify with the parity test comparing diff IDs across modes.

### Risk: Disk space (two-solve disk-based)
The intermediate OCI layout store consumes local disk.
**Mitigation**: Per-arch stores in a temp dir, cleaned up after Phase 2. Consider the in-memory shared store optimization later.

## Open Items to Prototype (carry over from verification)

1. Confirm multi-platform manifest list assembly when per-arch images come from separate OCI layouts imported via `llb.OCILayout()`.
2. Decide disk-based two-solve vs shared in-memory store (start disk-based).
3. Confirm `ExporterImage` with `push=true` uses pack-resolved registry credentials correctly.
4. Verify run image base layers are correctly referenced (the layout includes them since the lifecycle exported a complete image FROM the run image).
5. Decide how to isolate `/output` as the Phase 1 export root.
