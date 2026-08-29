---
inclusion: manual
---

# BuildKit Caching Analysis for CNB Lifecycle Builds

## How BuildKit Layer Caching Works

BuildKit caches the result of each instruction (RUN, COPY, etc.) based on its inputs. If the inputs haven't changed since the last build, the cached result is reused and the instruction is skipped entirely. This is what you see as "CACHED" in `docker buildx build` output.

Cache invalidation is cascading: if instruction N's inputs change, all instructions N+1, N+2, etc. also re-run because their "layer below" input changed.

## How Buildpacks Build Works Inside BuildKit

The generated Dockerfile for a BuildKit-based CNB build looks like:

```dockerfile
FROM <builder-image>                    # ← Cached after first pull
USER root
RUN cat > /cnb/order.toml ...           # ← Cached (static content)
ARG TARGETARCH
RUN mkdir -p /cache && chown ...        # ← Cached (static command)
ENV CNB_PLATFORM_API=0.15
ENV CNB_REGISTRY_AUTH=...
USER 1001:1001
COPY --chown=1001:1001 . /workspace     # ← INVALIDATES on any source change
WORKDIR /workspace

RUN ... /cnb/lifecycle/analyzer ...     # ← Always re-runs (after COPY)
RUN ... /cnb/lifecycle/detector ...     # ← Always re-runs
RUN ... /cnb/lifecycle/restorer ...     # ← Always re-runs
RUN ... /cnb/lifecycle/builder ...      # ← Always re-runs
RUN ... /cnb/lifecycle/exporter ...     # ← Always re-runs
```

**Key insight**: The `COPY . /workspace` instruction copies app source. Since source changes on every meaningful build, this invalidates ALL downstream instructions. Every lifecycle phase re-runs on every build.

## Two Layers of Caching (Currently)

### 1. BuildKit Layer Cache (instruction-level)

Only helps with instructions BEFORE `COPY . /workspace`:
- Builder image pull (cached after first build)
- Directory setup commands (static, always cached)
- Order.toml injection (cached if order doesn't change)

Everything after COPY always re-runs. This is inherent to the source-after-setup ordering.

### 2. Lifecycle Buildpack Cache (--mount=type=cache)

The lifecycle has its own caching mechanism via `--mount=type=cache,target=/cache`. This is a persistent named volume that survives across builds. The lifecycle's restorer/exporter use it to cache buildpack-contributed layers.

When buildpacks run, they check their own cached state:
- "Go modules haven't changed → reusing cached download" (skips download, uses cached layer)
- "JRE version is the same → reusing cached layer" (skips install)

This means the **buildpack binary always executes** (the RUN instruction runs), but the buildpack's internal logic decides whether to do real work or reuse cached results. The lifecycle's cache mount persists this state across builds.

## Where Time Actually Goes

```
Phase                          Typical Time    BuildKit Layer Cache Helps?
─────────────────────────────────────────────────────────────────────────────────
Pull builder image             5-30s           Yes (cached after first pull)
Setup (mkdir, order.toml)      <1s             Yes (always cached)
COPY source                    <1s             No (always changes)
Analyze                        1-3s            No (after COPY)
Detect                         1-5s            No (after COPY)
Restore (from lifecycle cache) 1-3s            No (but lifecycle cache helps)
Build (buildpack logic)        5-300s          No (but buildpack caches help)
Export                         2-10s           No (after COPY)
Manifest list assembly         1-3s            No (separate step)
```

## Why Per-Layer Caching of Final Image Provides Marginal Benefit

If we had a custom Dockerfile stage that assembled the final image layer by layer:

```dockerfile
FROM <run-image>
COPY --from=build /output/layers/00-launcher/ /      # BuildKit caches this
COPY --from=build /output/layers/01-go-dist/ /       # BuildKit caches this
COPY --from=build /output/layers/02-app/ /           # Changes when source changes
```

BuildKit COULD cache individual layers (launcher, go-dist) across builds if their content didn't change. But:

1. **The lifecycle already ran** — that's where all the time was spent. Caching the COPY step saves milliseconds of tar computation.

2. **Registries are content-addressable** — when pushing, if a blob (layer) already exists at the registry with the same digest, the push is a no-op. Unchanged layers aren't re-uploaded regardless of whether BuildKit cached them locally.

3. **The lifecycle's own cache handles the expensive part** — dependency downloads (Go modules, JRE, npm packages) are cached via `--mount=type=cache`. The buildpack decides internally whether to reuse or rebuild.

4. **Export time is small** — The export/push step (2-10s) is a fraction of builds dominated by compilation (5-300s).

## Registry Cache for CI (Ephemeral Builders)

In CI environments where the BuildKit daemon is ephemeral (new runner each time):
- `--cache-from type=registry,ref=...` imports previously cached layers
- `--cache-to type=registry,ref=...,mode=max` exports all layers to cache

This helps with the builder image pull and setup steps (they're served from registry cache instead of re-executing). But it doesn't help with lifecycle phases that run after COPY — those always re-execute regardless of registry cache.

The lifecycle's `--mount=type=cache` is also lost on ephemeral builders unless a cache image is used (`-cache-image`). This is separate from BuildKit's registry cache.

## Conclusion

Per-layer BuildKit caching of the final image is a marginal optimization because:
1. Buildpacks always run (source change invalidates everything downstream)
2. The lifecycle's own cache handles dependency reuse
3. Registries deduplicate blobs on push regardless of local caching
4. Export time is small relative to build time

The OCI layout approach (lifecycle writes complete image → pack pushes manifest list) achieves the same functional outcome (no intermediate tags, atomic push) without requiring a new lifecycle export mode or custom assembly contract.
