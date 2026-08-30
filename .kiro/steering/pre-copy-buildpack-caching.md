---
inclusion: manual
---

# Exploration: Pre-COPY Buildpack Execution & Multi-Stage Caching

## Status

**Future enhancement idea — NOT part of the current work.**

> Note: the Dockerfile / multi-stage examples below assume the DELETED
> generated-Dockerfile model. If this optimization is ever pursued it would target
> the implemented `llb.Copy`-based native assembly instead. The pre-copy /
> multi-stage caching idea itself is orthogonal to that and still stands.

This document captures an exploration of two related ideas for enabling BuildKit cross-build caching of runtime-installer buildpacks (JRE, Python, Node, etc.):
1. Splitting buildpack execution around the `COPY . /workspace` instruction (pre-copy vs post-copy)
2. Using a multi-stage Dockerfile to isolate source-independent work into cacheable stages

Both should be considered separate RFCs/features from the current BuildKit multi-arch and OCI layout work.

## The Problem

In the current BuildKit CNB flow, all lifecycle phases run AFTER `COPY . /workspace`:

```dockerfile
FROM <builder-image>
# ... setup (cacheable) ...
COPY --chown=1001:1001 . /workspace     # ← Invalidates everything below on source change
RUN ... /cnb/lifecycle/analyzer ...     # ← Always re-runs
RUN ... /cnb/lifecycle/detector ...     # ← Always re-runs
RUN ... /cnb/lifecycle/restorer ...     # ← Always re-runs
RUN ... /cnb/lifecycle/builder ...      # ← Always re-runs
RUN ... /cnb/lifecycle/exporter ...     # ← Always re-runs
```

Because app source changes on every meaningful build, the COPY invalidates all downstream instructions. Every buildpack runs on every build.

The lifecycle's `--mount=type=cache` avoids re-DOWNLOADING dependencies (JRE, Go modules), but the buildpack process still EXECUTES (1-3s each) to check its cache and decide to reuse.

## Idea 1: Pre-COPY vs Post-COPY Buildpack Split (Linear)

Some buildpacks install runtimes/dependencies that do NOT depend on app source content:
- **JRE/JDK install** — downloads a Java runtime based on a version config
- **Python install** — installs a Python interpreter version
- **Node.js install** — installs a Node runtime version
- **CA certificates** — adds certs

These buildpacks may READ the app to determine WHICH version to install, but the heavy work (download + extract) is a function of the version, not the source code.

If these ran BEFORE `COPY . /workspace`, their RUN instructions would be cacheable by BuildKit:

```dockerfile
FROM <builder-image>
# ... setup ...

# Pre-COPY: runtime installers (cacheable across builds)
RUN /cnb/lifecycle/builder -phase pre-copy   # runs JRE, Python, Node installers

COPY --chown=1001:1001 . /workspace          # source change doesn't invalidate above

# Post-COPY: app builders (always re-run)
RUN /cnb/lifecycle/builder -phase post-copy  # runs Maven, Go build, npm install, etc.
RUN /cnb/lifecycle/exporter ...
```

### Limitation of the linear approach

Even in the linear split, the pre-copy RUN instructions come after `FROM` and the setup steps. If ANY earlier instruction changes (an env var, the order.toml, a setup command), the cascade invalidates the pre-copy buildpacks too. The pre-copy stage is only as stable as everything above it in the single linear chain.

## Idea 2: Multi-Stage Dockerfile (Stronger Isolation)

A multi-stage Dockerfile isolates source-independent work into its own stage that is insulated from changes in other stages. This is a meaningful improvement over the linear split.

```dockerfile
# Stage 1: source — isolates app source into its own stage
FROM scratch AS source
COPY . /workspace

# Stage 2: runtime — pre-copy buildpacks (JRE, Python, Node)
# Inputs: builder image + buildpack versions. Does NOT depend on app source.
FROM <builder-image> AS runtime
RUN /cnb/lifecycle/builder -phase pre-copy

# Stage 3: detect — needs source
FROM runtime AS detect
COPY --from=source /workspace /workspace
RUN /cnb/lifecycle/detector -app /workspace

# Stage 4: build + export — app builders, needs source + detect output
FROM detect AS build
RUN /cnb/lifecycle/builder -phase post-copy
RUN /cnb/lifecycle/exporter ...
```

### Why multi-stage is stronger than the linear split

The key property: **BuildKit caches each stage independently based on that stage's own inputs.** The `runtime` stage (Stage 2) does NOT depend on the `source` stage. Its cache key is only:
- The builder base image
- The pre-copy buildpack RUN command (buildpack version + args)

As a result:
- Change app source → `runtime` stage stays cached (it doesn't consume source)
- Change detect logic → `runtime` stage stays cached (it's upstream of detect)
- Change post-copy buildpacks → `runtime` stage stays cached

The `runtime` stage is **insulated** in a way the linear pre-copy RUN is not. In the linear model, a change to any earlier instruction cascades into the pre-copy step. In multi-stage, the runtime stage only rebuilds if ITS specific inputs change.

### Parallelism bonus

BuildKit builds independent stages concurrently. The `source` stage and the `runtime` stage don't depend on each other, so BuildKit can copy source while the JRE installs. A small, free win.

## Does Multi-Stage Help WITHOUT the Pre/Post Split?

No. Consider isolating just the source COPY into its own stage but keeping all buildpacks together:

```dockerfile
FROM scratch AS source
COPY . /workspace

FROM <builder-image> AS build
RUN ... analyzer ...
COPY --from=source /workspace /workspace   # ← still invalidates everything below
RUN ... detector ...
RUN ... builder ...
RUN ... exporter ...
```

The `COPY --from=source` in the build stage still invalidates all downstream steps when source changes. Moving source into a separate stage doesn't help by itself.

**The benefit of multi-stage only materializes when a stage does NOT consume the changing content.** That's why the split matters: the `runtime` stage must not depend on source for it to stay cached. Multi-stage and the pre/post split are complementary — you need both.

## The Layer Assembly Challenge

Splitting buildpack execution across stages means layer outputs are spread across stages:
- Runtime layers produced in Stage 2 (`runtime`)
- App layers produced in Stage 4 (`build`)

The final image needs ALL layers combined in the correct order. This requires gathering layers from multiple stages:

```dockerfile
FROM <run-image> AS final
COPY --from=runtime /layers/paketo-buildpacks_jre/ /...
COPY --from=build /layers/paketo-buildpacks_go-build/ /...
COPY --from=build /workspace /workspace
```

This connects directly to the cnbp-style export pattern and the layer-contract discussion. The exporter (or a final assembly stage) must reconstruct the full layer set from multiple source stages. Layer ordering and diff-ID consistency (for rebase) must be preserved across this reassembly.

## What Would Be Required

### Spec changes
- New buildpack.toml field to declare app-source independence:
  ```toml
  [buildpack.build]
  requires-app-source = false   # opt-in for pre-copy eligibility; default true
  ```
- Or a new Platform API concept for phased build execution
- Default MUST be `true` (post-copy) for full backward compatibility

### Lifecycle changes
- Builder phase must support split execution: `-phase pre-copy` and `-phase post-copy`
- Builder must partition the buildpack group by the `requires-app-source` flag
- Layer metadata and ordering must remain consistent (rebase compatibility) even when layers are produced across stages

### Platform (pack) changes
- Dockerfile generator must emit the multi-stage structure
- Must know the buildpack group and each buildpack's `requires-app-source` before generating the Dockerfile
- Must generate the final assembly stage that gathers layers from all stages in the correct order

### Buildpack author adoption
- Authors opt in by setting `requires-app-source = false`
- Only safe for buildpacks that genuinely don't read app source during build

## The Detection Challenge

Detection reads app source to decide which buildpacks participate. Detection must run after source is available. This complicates a pure pre-copy model.

**Resolution**: In flows where the buildpack group is pre-determined (order.toml injected, or `--buildpack` flags), detection can be skipped or simplified. The BuildKit multi-arch flow already injects order.toml from builder metadata — the group is known before the build starts. In this case, pre-copy execution of known runtime buildpacks in an isolated stage is feasible.

## Expected Benefit

| Scenario | Current (lifecycle cache) | Linear pre-copy | Multi-stage pre-copy |
|----------|--------------------------|-----------------|---------------------|
| First build (cold) | Full download (15-45s) | Full download (15-45s) | Full download (15-45s) |
| Rebuild, source changed, same runtime | Buildpack runs, validates cache (1-3s) | May re-run if earlier step changed | CACHED (0s) — runtime stage isolated |
| Rebuild, new runtime version | Full download (15-45s) | Full download (15-45s) | Full download (15-45s) |
| CI, ephemeral builder + registry cache | Depends on lifecycle cache image | Layer from registry cache | Stage from registry cache (strongest) |

The multi-stage approach provides the strongest cross-build caching because the runtime stage is insulated from source and downstream changes. For the common inner-loop case (edit app code, rebuild), the runtime install stage is fully cached.

## Why It's Separate From Current Work

1. **Requires spec changes** — Unlike the OCI layout / intermediate-tag work (no spec change needed), this needs a new buildpack.toml field or Platform API concept.
2. **Requires buildpack author adoption** — Runtime buildpacks must opt in.
3. **Broader contract implications** — Changes the fundamental assumption that all buildpacks run after source is available, and that all layers are produced in a single phase.
4. **Detection complexity** — Works cleanly only in the pre-determined-group case.
5. **Layer reassembly** — Requires gathering layers across stages while preserving order and diff IDs for rebase.

## Recommendation

- Document as a future enhancement in the RFC (mention BuildKit's multi-stage caching as a potential optimization)
- Pursue as its own RFC + lifecycle enhancement AFTER the current BuildKit multi-arch + tag-elimination work lands
- The current work (the single buildkit-native backend, no intermediate tags) does not depend on this and should proceed independently
- The multi-stage variant is the more promising of the two (stronger isolation) and should be the primary design if pursued

## Relationship to Overall BuildKit Strategy

These ideas are uniquely enabled by BuildKit's execution model (stage-level and instruction-level layer caching). In pack's traditional container-per-phase flow, there's no equivalent — so pre-copy/multi-stage execution wouldn't provide cross-build caching there. This makes it a BuildKit-specific optimization that could differentiate BuildKit-backed builds.

If pursued, it would also benefit Buildah/Podman flows (which have similar layer caching), reinforcing the value of a universal, tool-agnostic lifecycle contract. The layer reassembly requirement also aligns with the cnbp-style export pattern — building the final image from individual layers produced across the build.
