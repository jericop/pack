---
inclusion: manual
---

# Eliminating Intermediate Tags in BuildKit Multi-Arch Builds

## Problem Statement

The current BuildKit multi-architecture build flow creates intermediate per-architecture tags on the registry:

```
registry.example.com/myapp:latest-build-abc123-amd64
registry.example.com/myapp:latest-build-abc123-arm64
```

These exist because:
1. Each architecture builds in parallel inside BuildKit — they cannot share a single tag
2. The lifecycle exporter pushes directly to the registry during the build (it owns the push)
3. Pack then assembles the manifest list from these tags post-build

The intermediate tags remain on the registry as clutter. They don't consume additional storage (the manifest list references the same blobs), but they create visual noise and may confuse users.

## Goal

Eliminate intermediate tags entirely while preserving:
- All lifecycle exporter functionality (layer reuse, CNB metadata, SBOM, process types)
- BuildKit's caching capabilities
- Multi-platform parallel builds
- The opt-in/experimental nature of the feature

## Approaches Considered

### Approach 1: Push by Digest Only (No Intermediate Tag)

Instead of telling the lifecycle exporter to push to `<image>-build-<id>-<arch>`, have it push to the base image name. Each arch push creates a manifest under a unique digest. Pack then references images by digest when building the manifest list.

**Pros:**
- Minimal code change (generator + executor wiring)
- Works with existing lifecycle — no new flags
- No intermediate tags

**Cons:**
- Brief race window where the tag points to a single-arch image (last arch to finish overwrites it)
- Relies on correct digest parsing from BuildKit's interleaved multi-platform output (both arches output simultaneously — fragile parsing)
- Registry tag is in an inconsistent state during the build

**Verdict:** Fragile. The interleaved output parsing and race condition make this unreliable for production use.

### Approach 2: OCI Layout Export + Pack Pushes Manifest List

The lifecycle exports to OCI layout on local disk instead of pushing to a registry. BuildKit extracts the per-arch OCI layouts via `--output type=local`. Pack reads both layouts, assembles the manifest list in-memory, and pushes everything atomically.

**Pros:**
- Zero intermediate tags
- Atomic push — registry never sees partial state
- Code largely already exists (`oci_layout_push.go` in the fork, `-layout` and `-pull-run-image` lifecycle flags)
- Lifecycle handles all complex image assembly logic

**Cons:**
- Requires local disk space for OCI layouts (temporary)
- Requires `-pull-run-image` lifecycle flag (already implemented on `buildkit-multi-arch-support` branch)
- BuildKit's `--output type=local` with multi-platform creates per-platform subdirs needing path handling
- Data leaves BuildKit and is stored on host disk — pack must re-push all blobs to the registry
- No BuildKit layer-level caching of the final image (only buildpack cache mounts benefit)

**Verdict:** Works and is implementable today with existing lifecycle code. But suboptimal because data must transit through local disk and pack re-pushes everything — BuildKit's caching of output layers is not utilized.

### Approach 3: Ephemeral Registry

Spin up a temporary registry container on a shared Docker network. Lifecycle pushes to it. Pack reads from it, assembles manifest list, pushes to real registry. Tear down temp registry.

**Pros:**
- No intermediate tags on real registry
- Works with existing lifecycle

**Cons:**
- Significant infrastructure code (start/stop registry, network management)
- Requires purpose-built BuildKit builder on same network
- Cannot reuse existing builders
- Most complex to implement and debug

**Verdict:** Over-engineered. The operational complexity outweighs the benefit.

### Approach 4: BuildKit-Native Output (Hybrid — CHOSEN APPROACH)

Combine the lifecycle's export capabilities with BuildKit's native image output. The lifecycle exports to OCI layout on disk (inside the build container), and a final Dockerfile stage reconstructs the image as BuildKit-native operations (`FROM <run-image>` + `COPY` per-layer). BuildKit then owns the final image and handles push, caching, and manifest list assembly natively.

This is inspired by the `cnbp` project (see `cnbp-buildkit-frontend.md`), which proved that expressing the final image as LLB/Dockerfile operations gives BuildKit full ownership of the output.

**How it works:**

```dockerfile
# ... lifecycle phases (analyze, detect, restore, build) ...

# Lifecycle exports to OCI layout inside the build container
RUN /cnb/lifecycle/exporter -layout -layout-dir /output -pull-run-image \
    -app /workspace -cache-dir /cache <image-name>

# Final stage: reconstruct the image from lifecycle's layout output
# BuildKit sees this as a normal multi-stage build — it owns the output
FROM <run-image> AS final
COPY --from=0 /output/<layer-1>/ /
COPY --from=0 /output/<layer-2>/ /
COPY --from=0 /output/<app-layer>/ /workspace
LABEL io.buildpacks.lifecycle.metadata=...
ENTRYPOINT ["/cnb/process/web"]
```

With `docker buildx build --platform linux/amd64,linux/arm64 --push`, BuildKit:
- Builds both platforms in parallel
- Caches each layer independently in its content store
- Pushes only changed layers to the registry
- Assembles the manifest list atomically
- Zero intermediate tags

**Pros:**
- No intermediate tags — BuildKit assembles manifest list natively
- No data duplication — BuildKit's content store is the single source of truth
- Full layer-level caching — unchanged layers are never re-pushed
- Lifecycle retains all its functionality (layer reuse, metadata, SBOM, process types)
- Fully opt-in (new export mode; without it, lifecycle behaves as today)
- Works with `--push` directly — no separate manifest assembly step by pack

**Cons:**
- Requires the lifecycle to export to layout (existing `-layout` flag + `-pull-run-image` already implemented)
- The final Dockerfile stage needs to know the layer structure (pack must parse the layout output to generate the COPY instructions)
- More complex Dockerfile generation
- Requires a way to pass image config (labels, entrypoint, env) from lifecycle output to the final stage

**Verdict:** Best long-term approach. Gives us full BuildKit integration while keeping the lifecycle's export logic intact. The lifecycle doesn't need to "talk to BuildKit" — it just writes to disk, and the Dockerfile structure around it lets BuildKit take ownership of the result.

## Why Approach 4 Over Approach 2

The key difference is WHO pushes the final image:

| | Approach 2 | Approach 4 |
|---|---|---|
| Who pushes | Pack (via go-containerregistry) | BuildKit (native --push) |
| Layer caching | None for output image | Full — BuildKit caches each layer |
| Data path | BuildKit → local disk → registry | BuildKit → registry (direct) |
| Manifest list | Pack assembles and pushes | BuildKit assembles natively |
| Repeat builds | All blobs re-pushed | Only changed layers pushed |

Approach 4 is more efficient because BuildKit's content-addressable store means unchanged layers are never re-transferred. On repeat builds where only the app layer changes, BuildKit pushes just that one layer diff rather than the entire image.

## Implementation Strategy

### Phase 1 (immediate, already working): Registry mode with pack manifest assembly
- Current implementation: lifecycle pushes per-arch images to intermediate tags
- Pack assembles manifest list using built-in CreateManifest API (just implemented)
- Intermediate tags still exist but the assembly uses pack-native code (no docker CLI dependency)

### Phase 2 (next): OCI layout + pack push (Approach 2 as stepping stone)
- Enable the existing OCI layout export mode
- Lifecycle exports to layout inside BuildKit, extracted via --output type=local
- Pack pushes manifest list atomically using PushOCILayoutAsManifestList
- Eliminates intermediate tags
- Validates the lifecycle layout mode works end-to-end in BuildKit

### Phase 3 (target): BuildKit-native output (Approach 4)
- Lifecycle exports to layout inside the build container
- Pack generates a final Dockerfile stage that reconstructs the image from layout
- BuildKit handles push and manifest list assembly natively
- Full layer-level caching benefits

### Lifecycle requirements (already implemented in fork):
- `-layout -layout-dir` flags (upstream lifecycle, existing)
- `-pull-run-image` flag (jericop/cnb-lifecycle `buildkit-multi-arch-support` branch)
- `-skip-chown` flag (jericop/cnb-lifecycle `skip-chown` branch, needed for LLB backend)

## Open Questions for Phase 3

1. How does pack discover the layer structure from the lifecycle's OCI layout output to generate the final COPY instructions? Options:
   - Parse the OCI layout's manifest.json to find layer paths
   - Have the lifecycle write a separate "layer manifest" file listing layers in order
   - Use a fixed convention (layers are always in /output/blobs/sha256/...)

2. How to pass image config (labels, entrypoint, CMD, env) from lifecycle output to the final Dockerfile stage? Options:
   - Lifecycle writes a config.json alongside the layout
   - Pack parses the OCI layout's config blob
   - Use ARGs or a heredoc RUN that reads from the layout

3. Should this be a Dockerfile-only approach or should the LLB backend also support it?
   - The Dockerfile approach is simpler (multi-stage is well-understood)
   - The LLB backend could do it more precisely (programmatic graph construction like cnbp)

4. Can the lifecycle's layout output be structured to produce exactly one layer per buildpack contribution (matching how cnbp does individual COPY per layer)?
   - The lifecycle already produces layers this way internally
   - The OCI layout format preserves this structure

## Prior Art

- **cnbp** (EricHripko/cnbp): Custom BuildKit frontend that reimplements the export phase as LLB operations. Proves the pattern works but loses lifecycle functionality. See `cnbp-buildkit-frontend.md`.
- **HackMD discussion** (https://hackmd.io/@jromero/B1HfPTM__): Early exploration of BuildKit + CNB integration approaches.
- **RFC 0128**: Multi-platform support for builders and buildpack packages (established the ecosystem for multi-arch).
- **Docker buildx**: Native multi-platform build support that this feature builds upon.
