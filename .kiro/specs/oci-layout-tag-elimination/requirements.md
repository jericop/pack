# Requirements: LLB Native Image Export to Eliminate Intermediate Tags

> **STATUS — SUPERSEDED (historical spike).** This entire spec covers the
> OCI-layout tag-elimination approach (LLB backend + `llb.OCILayout()` +
> `-pull-run-image`). That approach, the LLB/Dockerfile backends, the
> `oci_layout_*.go` files, and the `-pull-run-image` flag have all been REMOVED.
> The implemented approach is the single builder-agnostic `buildkit` backend
> (build-then-finalize) documented in the `buildkit-native-export` spec and the
> `buildkit-multiarch` steering file. Retained only as a record of the spike.

## Overview

Eliminate intermediate per-architecture registry tags in the BuildKit multi-arch build flow by having the **LLB backend** use BuildKit's `llb.OCILayout()` source and native `ExporterImage` push. The lifecycle exports each architecture's complete image to OCI layout inside the build; BuildKit imports that layout and pushes it natively, assembling the manifest list without ever creating intermediate tags and without pack shelling out to `docker buildx` or re-pushing via go-containerregistry.

This approach requires NO new lifecycle export mode — it uses the lifecycle's existing `-layout -layout-dir` flags plus the already-implemented `-pull-run-image` and `-skip-chown` flags.

## Scope

- **LLB backend only.** This is the long-term, shipped approach.
- **The Dockerfile backend is explicitly OUT of scope.** It remains an MVP that proves the concept using registry mode with intermediate tags. Its intermediate tags are acceptable and will not be eliminated. It exists only to demonstrate feasibility.

## Background

The current registry export mode creates intermediate tags:
```
registry.example.com/myapp:latest-build-abc123-amd64
registry.example.com/myapp:latest-build-abc123-arm64
```

These remain on the registry after the manifest list is assembled. The LLB OCI layout approach eliminates them because the per-arch images are held in BuildKit's content store and pushed natively as a manifest list — no per-arch tags ever land on the registry.

## Functional Requirements

### FR-1: Enable OCI layout export mode for the LLB backend
- `--build-backend=buildkit-llb --buildkit-export-mode=oci-layout` MUST activate the native LLB OCI layout path
- The LLB backend MUST NOT create intermediate per-arch tags in this mode
- The default export mode remains `registry` (unchanged behavior)

### FR-2: Experimental gating (unchanged)
- The OCI layout mode remains fully experimental
- It requires `pack config experimental true` (as the entire `--buildkit` feature does)
- It requires the `--buildkit` flag
- It requires explicit opt-in via `--buildkit-export-mode=oci-layout`
- No feature becomes non-experimental as a result of this work

### FR-3: Lifecycle phase configuration (LLB backend)
- In OCI layout mode, the LLB backend MUST configure the exporter phase with `-layout -layout-dir /output`
- The analyzer MUST use `-pull-run-image` to self-populate the run image (pack cannot pre-populate inside BuildKit)
- The `-skip-chown` flag MUST be passed where needed for the unprivileged BuildKit environment
- The exporter writing the OCI layout does NOT need registry credentials (it writes locally)

### FR-4: Two-phase LLB solve (produce layout → import → push)
- Phase 1: the LLB backend MUST run the lifecycle graph and export the resulting OCI layout to a per-arch content store (via `ExporterOCI` with `OutputStore`/`OutputDir`)
- Phase 2: the LLB backend MUST import that layout via `llb.OCILayout(ref, llb.OCIStore("", storeID))` with the store attached through `SolveOpt.OCIStores`, and export it via `ExporterImage` with `push=true`
- Each parallel platform MUST use its own content store to avoid collisions
- The `/output` OCI layout (not the full container root) MUST be the export subject in Phase 1

### FR-5: Native manifest list push (no intermediate tags, no pack-side push)
- BuildKit MUST assemble and push the multi-platform manifest list natively via `ExporterImage`
- Pack MUST NOT push the manifest list via go-containerregistry for the LLB path (that was the OCI-layout-extract fallback)
- NO intermediate tags MUST be created on the registry
- Registry credentials MUST be provided to the push via a session auth provider (pack-resolved keychain)

### FR-6: Cleanup
- Any temporary content stores / directories used for the intermediate OCI layout MUST be cleaned up after the push completes
- Cleanup MUST happen even if the push fails (deferred cleanup)

### FR-7: Rebase compatibility
- The pushed per-arch images MUST support `pack rebase` identically to registry-mode builds
- Layer order, diff IDs, and the `io.buildpacks.lifecycle.metadata` label MUST match what registry mode produces
- Parity is guaranteed by construction: BuildKit re-exports the lifecycle's exact layer blobs (no re-tarring)
- See cnb-lifecycle steering `layer-order-and-rebase.md` for the full rebase compatibility analysis

### FR-8: Parity verification against registry mode
- Building the same app in registry mode (Dockerfile MVP) and LLB OCI layout mode MUST produce equivalent per-arch images
- Diff IDs in `io.buildpacks.lifecycle.metadata` MUST match across modes
- This comparison is a first-class acceptance test, giving confidence the LLB path produces identical artifacts
- The parity check MUST be performable WITHOUT a registry by inspecting the on-disk OCI layout (manifest + config blobs). Registry-based verification is optional.

## Non-Functional Requirements

### NFR-1: No lifecycle changes required
- This work uses only existing lifecycle flags (`-layout`, `-layout-dir`, `-pull-run-image`, `-skip-chown`)
- No new lifecycle export mode or contract is introduced

### NFR-2: Backward compatibility
- Registry export mode (the default) MUST remain unchanged for both backends
- The Dockerfile backend MUST remain unchanged (still uses registry mode with intermediate tags)
- Existing tests MUST pass

### NFR-3: Disk space
- The intermediate OCI layout (Phase 1 output) requires temporary local disk space (disk-based two-solve)
- This is acceptable as a tradeoff; temporary space MUST be released promptly after push
- A shared in-memory content store is a possible future optimization

### NFR-4: Testing without a registry
- Core correctness tests (structure, diff IDs, config, parity) MUST run against the on-disk OCI layout without requiring a registry
- This avoids the BuildKit `docker-container` builder network-isolation problem (the builder cannot reach a host-local ephemeral registry unless created with a shared network)
- Registry-based integration testing is OPTIONAL and, when used, prefers a real reachable registry; the ephemeral-registry-on-shared-network recipe is documented as an alternative

### NFR-5: Registry-based tests are env-var gated and skipped by default
- Registry-based integration/parity tests MUST be gated behind an environment variable (e.g., `PACK_TEST_REGISTRY_ENABLED`) and skipped by default
- This keeps the default test run fast and free of network/registry/builder-network setup
- Rationale for env var over auto-detection: detecting whether the current builder supports local network access (to reach a local registry) is hard to implement reliably; the env var is the pragmatic starting point
- FUTURE consideration: auto-detect builder local-network support to enable these tests automatically (deferred)
- The chosen approach and required setup MUST be documented in the RFC once the feature is working

## Out of Scope

- The Dockerfile backend (remains an MVP with intermediate tags; no changes)
- Per-layer BuildKit caching of the final image (explored and deferred — see steering docs)
- Pre-copy buildpack caching / multi-stage builds (future enhancement)
- The custom `-export-mode layers` lifecycle contract (explored and deferred — see cnb-lifecycle steering `explored-export-mode-layers.md`)
- Ephemeral registry approach (rejected — too complex)
- Shared in-memory content store optimization (start disk-based; optimize later)

## Dependencies

- Lifecycle with `-pull-run-image` support (jericop/cnb-lifecycle `buildkit-multi-arch-support` branch)
- Lifecycle with `-skip-chown` support (jericop/cnb-lifecycle `skip-chown` branch)
- Builder image bundling the patched lifecycle (jericop/ubuntu-noble-builder `skip-chown-lifecycle` branch)
- BuildKit v0.30.0 `llb.OCILayout()` + `SolveOpt.OCIStores` + `ExporterImage` (verified — see steering `llb-ocilayout-verification.md`)
