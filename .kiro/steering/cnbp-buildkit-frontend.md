---
inclusion: manual
---

# Prior Art: cnbp — BuildKit Frontend for Cloud Native Buildpacks

## Overview

`cnbp` (by EricHripko, ~2020-2021) is a **custom BuildKit frontend** that implements the Cloud Native Buildpacks Platform spec entirely within BuildKit's LLB graph system. It runs lifecycle phases as LLB operations and — critically — **replaces the lifecycle exporter** with a custom export step that constructs the final image as native BuildKit LLB operations.

Source: https://github.com/EricHripko/cnbp
Local clone: `/Users/jpena/.repos/EricHripko/cnbp`
HackMD discussion: https://hackmd.io/@jromero/B1HfPTM__

## Architecture

cnbp is a BuildKit frontend binary (invoked via `# syntax = erichripko/cnbp` in a Dockerfile-like project.toml). It:

1. Implements `grpcclient.RunFromEnvironment()` — the BuildKit frontend gateway protocol
2. Constructs the entire build as an LLB graph:
   - `BuildEnvironment()` → loads builder image, copies source, sets env
   - `Detect()` → runs `/cnb/lifecycle/detector`
   - `Analyze()` → runs `/cnb/lifecycle/analyzer` with cache mount
   - `Restore()` → runs `/cnb/lifecycle/restorer` with cache mount
   - `Build()` → runs `/cnb/lifecycle/builder`
   - `Export()` → **custom export** (does NOT use `/cnb/lifecycle/exporter`)
3. Returns the result to BuildKit, which handles image push, manifest list assembly, caching, etc.

## Key Insight: The Export Phase

The `Export()` function in `pkg/cnbp2llb/export.go` is the most relevant piece. Instead of calling the lifecycle exporter (which pushes directly to a registry), it:

1. Reads the build output (group.toml, layer metadata) from the solved LLB state
2. Identifies which buildpack layers are "launch" layers
3. Reads stack.toml to find the run image
4. Constructs a NEW LLB state starting `FROM <run-image>`
5. Uses `llb.Copy()` to add each launch layer, the launcher, the app, and metadata
6. Returns this state as the frontend's output reference

BuildKit then handles the actual image export (push to registry, load to daemon, or write to disk) using its native export pipeline. This means:
- BuildKit owns the final image
- BuildKit can produce multi-platform manifest lists natively
- All layers participate in BuildKit's content-addressable cache
- No intermediate tags needed

## Multi-Platform Support

The frontend already handles multi-platform builds (see `frontend.go`):
- Gets target platforms via `svc.GetTargetPlatforms()`
- Builds each platform in parallel via `errgroup`
- Returns per-platform references via `res.AddRef(platformKey, ref)`
- BuildKit automatically assembles the manifest list from the multi-platform result

This is exactly the pattern that eliminates intermediate tags — BuildKit holds all per-arch image data in its content store and pushes the manifest list atomically.

## Caching

The frontend uses `llb.AsPersistentCacheDir("buildpacks-cache", llb.CacheMountPrivate)` for the buildpack layer cache. BuildKit's native layer caching handles all other caching (detect, build, export steps are cached if inputs don't change).

## Limitations of This Implementation

1. **Platform API 0.5** — Very old; current is 0.15. Missing many features (extensions, run image mirrors, SBOM, etc.)
2. **No lifecycle exporter** — Reimplements export logic in Go using LLB operations. This means:
   - No layer reuse from previous images
   - No proper CNB lifecycle metadata labels
   - Missing process types / entrypoint configuration
   - No report.toml
   - No SBOM export
3. **Hardcoded "this-image-definitely-does-not-exist"** for analyzer previous image
4. **Custom cacher binary** — Ships a custom `cacher` binary because the lifecycle restorer/exporter weren't compatible with the model
5. **5 years old** — Dependencies are extremely outdated (BuildKit API has changed significantly)
6. **buildkit-fdk dependency** — Uses a custom framework (`github.com/EricHripko/buildkit-fdk`) that wraps the BuildKit gateway client

## Relevance to Our Work

### What we can learn:
- The **pattern** of constructing the final image as an LLB graph (FROM run-image + COPY layers) is exactly what allows BuildKit to own the output image
- Multi-platform is handled naturally by the BuildKit frontend protocol — no intermediate tags
- The cache mount pattern works (persistent named cache for buildpack layers)
- BuildKit's content store holds all layer data; no duplication

### What we would do differently:
- **Keep the lifecycle exporter** — Rather than reimplementing export logic, have the lifecycle export to OCI layout on disk, then use a final Dockerfile stage or LLB operations to reconstruct the image from that layout
- **Or**: Have the lifecycle produce a "layer manifest" (list of layer paths + metadata JSON) that pack's Dockerfile generator uses in a final assembly stage
- **Modern Platform API** — All the features the lifecycle exporter provides (layer reuse, SBOM, metadata, process types) are too valuable to reimplement

### The hybrid approach (combining cnbp's insight with lifecycle's capabilities):

1. Lifecycle runs detect → analyze → restore → build → **export to layout** (writes layers + config to disk)
2. A final Dockerfile stage reads the layout output and constructs the image using `FROM <run-image>` + `COPY` per-layer (similar to cnbp's export.go pattern but reading from lifecycle's layout output)
3. BuildKit handles the final image push, caching, and manifest list assembly

This gets us the best of both worlds:
- Lifecycle handles all the complex image assembly logic (layer optimization, metadata, SBOM, process types)
- BuildKit handles the output (layer-level caching, push, manifest list)
- No intermediate tags
- No data duplication (BuildKit's content store is the single source of truth)
- Fully opt-in (without the layout flag, lifecycle exports to daemon/registry as today)

## File Structure

```
cmd/
  cnbp-frontend/
    main.go              # Entry point: grpcclient.RunFromEnvironment(ctx, Build)
    frontend.go          # Build() — orchestrates the full LLB graph, handles multi-platform
    helper.go            # FetchUID/FetchGID from builder env
  cacher/
    main.go              # Custom cacher binary (replaces lifecycle cache export)
pkg/
  cnbp2llb/
    cnbp2llb.go          # Constants (paths, Platform API version)
    env.go               # BuildEnvironment() — loads builder, copies source
    detect.go            # Detect() — runs lifecycle detector
    analyze.go           # Analyze() — runs lifecycle analyzer with cache
    restore.go           # Restore() — runs lifecycle restorer with cache
    build.go             # Build() — runs lifecycle builder
    export.go            # Export() — CUSTOM export (no lifecycle exporter!)
  config/
    ...                  # project.toml parsing
```

## How cnbp's Export Works (Annotated)

```go
// From pkg/cnbp2llb/export.go

// 1. Read which buildpack layers are "launch" layers
groups := readToml(ref, "/layers/group.toml")
for _, group := range groups.Group {
    // Find layers with Launch=true in their .toml metadata
    launchLayers = append(launchLayers, layerPath)
}

// 2. Start the output image FROM the run image
state, img, _ := build.From(stack.RunImage.Image, platform, ...)

// 3. Copy each component into the output image as individual layers
state = state.File(llb.Copy(built, "/cnb/lifecycle/launcher", "/cnb/lifecycle/launcher"))
for _, layer := range launchLayers {
    state = state.File(llb.Copy(built, layer, layer))
}
state = state.File(llb.Copy(built, "/workspace", "/workspace"))
state = state.File(llb.Copy(built, "/layers/config/metadata.toml", ...))

// 4. Return to BuildKit — it handles push/caching/manifest list
ref, _ = build.Solve(ctx, state)
return ref, img, nil
```

This pattern is what allows BuildKit to:
- Cache each layer independently (if launcher doesn't change, that COPY is cached)
- Push only changed layers to the registry
- Assemble multi-platform manifest lists from its content store
- Avoid any intermediate tags
