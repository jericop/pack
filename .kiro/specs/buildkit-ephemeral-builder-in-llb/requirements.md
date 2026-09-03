# Requirements: Skip the daemon-synthesized ephemeral builder on the buildkit backend

## Overview

When a build adds modules to the builder (extra `--buildpack`/`--extension`, a custom
`project.toml` order, builder-baked build env, a custom run image, or system buildpacks),
pack synthesizes an **ephemeral builder image** and **loads it into the local Docker
daemon** (`pack.local/builder/<hex>`) via `createEphemeralBuilder` → `Builder.Save`.

On the `buildkit` backend this daemon load is **wasted work**: the buildkit backend does
NOT consume that image. It builds `FROM` the **base** builder image and injects the added
buildpacks + resolved `order.toml` **over LLB** (this already works today — see below). The
ephemeral daemon image is built, loaded (~16–85s observed; the origin of the FR-7 `max depth
exceeded`), and then never used by the buildkit path.

This spec's change is therefore small and surgical: **on the buildkit backend, do not
synthesize/load the ephemeral daemon builder at all.** Keep using the base builder image +
the existing over-LLB inject-and-update-order path. Source the few builder attributes the
buildkit path still reads (UID/GID, stack/distro labels, platform API) from the **base**
builder object rather than the synthesized one.

This is the performance follow-up to **FR-7** in `platform-1662-buildkit-followups`. FR-7
flattened the ephemeral builder so it was *loadable* on a deep base. This spec removes the
daemon load from the buildkit path entirely, which also makes the `max depth exceeded` class
impossible there by construction.

## Confirmed current behavior (traced to source)

The buildkit backend already assembles the "builder + added modules" over LLB, using the
BASE builder image:

- `pkg/client/build.go`, buildkit branch: `stageExtraBuildpacks(fetchedBPs)` stages the FULL
  resolved buildpack set (builder + `--buildpack` + pre/post) to `extraBPDir`, then calls
  `buildMultiPlatform(..., extraBPDir, order, orderExtensions)`.
- `buildMultiPlatform`: serializes the resolved order with `builder.OrderTOML(order, ...)`
  into `orderToml`; sets `platformBuildOpts.BuilderImage = lifecycleOpts.BuilderImage`
  (= `builderRef.Name()`, the BASE builder) and `ExtraBuildpacksDir = extraBPDir`.
- `internal/build/multiplatform/native_buildfunc.go` `buildEmitLLB`:
  - `base := llb.Image(in.builderImage)` — the BASE builder.
  - `[plat] add user buildpacks`: `llb.Copy(bpSrc, "/cnb/buildpacks", "/cnb/buildpacks",
    CopyDirContentsOnly)` — injects the added modules, overriding same-id/version.
  - `[plat] write order.toml`: writes `/cnb/order.toml` from `in.orderTOML`.
  - detector runs `/cnb/lifecycle/detector -app ... -layers /layers` (NO `-order` flag) →
    reads the on-disk `/cnb/order.toml`. No `io.buildpacks.buildpack.layers` label is set,
    and `--buildpack` builds SUCCEED → the lifecycle resolves added buildpacks from the
    on-disk `/cnb/buildpacks` tree + `order.toml`, not from a builder image label.

Meanwhile, EARLIER in `Client.build`, `createEphemeralBuilder` runs unconditionally and (for
added modules) calls `bldr.Save()` on a `local.NewImage` — the daemon load. The buildkit
path never consumes the resulting `pack.local/builder/<hex>`.

### Residual couplings to the ephemeral Builder object (all base-derivable)

`platformBuildOpts` reads a few things from `lifecycleOpts.Builder` (the ephemeral builder
Go object), NOT from the ephemeral IMAGE content:
- `BuilderUID()` / `BuilderGID()`
- `StackID` / `TargetDistroName` / `TargetDistroVersion` via
  `builderImageLabel(lifecycleOpts.Builder.Image(), ...)`
- `PlatformAPI` via `negotiatePlatformAPI(lifecycleOpts)` (builder lifecycle descriptor)
- `LifecycleImage = ephemeralBuilder.Name()` (only used by the untrusted-lifecycle-image
  path; the fork builder is trusted and runs its bundled lifecycle in place)

All of these are properties of the BASE builder and are unchanged by adding buildpacks, so
they can be sourced from the base builder object with no daemon `Save`.

## Functional Requirements

### FR-1: no ephemeral daemon builder on the buildkit backend
- With `--build-backend buildkit`, pack MUST NOT call `Builder.Save` to synthesize and load a
  `pack.local/builder/<hex>` image into the Docker daemon for the purpose of adding builder
  modules. No such image may be created during a buildkit build.

### FR-2: preserve current buildkit assembly semantics (no regression)
- The buildkit build MUST continue to: build `FROM` the base builder image, inject the full
  resolved buildpack set over `/cnb/buildpacks` (existing `ExtraBuildpacksDir` path), write
  the resolved `/cnb/order.toml`, and detect/build with the SAME buildpack group and produce
  the SAME runnable multi-arch image as today.

### FR-3: source builder attributes from the base builder (no daemon image)
- The attributes the buildkit path reads today from the ephemeral builder object
  (UID/GID, stack id, target distro name/version, platform API, and the lifecycle image when
  applicable) MUST be sourced from the BASE builder object/image instead, so no ephemeral
  image is required.

### FR-4: daemon (non-buildkit) backend unchanged
- For `--build-backend` values that use the Docker daemon executor, `createEphemeralBuilder`
  + `Builder.Save` (with the FR-7 flatten) MUST behave exactly as today. This spec only
  changes the buildkit path.

### FR-5: extensions / Dockerfile-extension flows
- If any added modules are EXTENSIONS (image extensions / Dockerfile flow), and the buildkit
  path does not yet fully cover them over LLB, pack MUST either (a) handle them over LLB with
  parity, or (b) explicitly fall back to the existing daemon synthesis for that case, with a
  clear reason. Buildpacks + order + env + run image are the primary in-scope path.

## Acceptance Criteria

- AC-1 (no daemon image): a `--build-backend buildkit` build that adds an extra buildpack
  (nodejs `--buildpack`) OR uses a custom `project.toml` order (agent-patcher-service)
  completes and NO `pack.local/builder/<hex>` image is created/loaded (verify via
  `docker images` and the absence of the daemon image-save step in logs).
- AC-2 (parity): the same builds detect the same buildpack group and produce a runnable
  multi-arch image, identical to the pre-change behavior.
- AC-3 (perf): the ephemeral-builder daemon synthesis + load time (~16–85s observed) is gone
  from the buildkit path; the FR-7 `max depth exceeded` class is impossible there by
  construction (no daemon load).
- AC-4 (daemon path unchanged): the daemon executor build is unchanged; existing
  `internal/builder` tests and the FR-7 flatten test still pass.

## Non-Functional Requirements

### NFR-1: reproducibility + MVP verification
- Verify via the MVP local-build strategy (`mvp-build-testing-strategy.md`) + local registry
  env (`local-test-environment.md`) against the nodejs `--buildpack` case and the
  agent-patcher-service custom-order case. Capture before/after timing.

### NFR-2: command execution conventions
- Follow `command-execution-practices.md` (stable runner, `export`ed env in command files,
  per-arch log split by the `[linux/<arch>]` prefix).

### NFR-3: relationship to the existing spec
- Performance follow-up to FR-7 in `platform-1662-buildkit-followups`. Cross-link, do not
  duplicate.

## Out of Scope
- Changing the `useCreator` / trusted-vs-untrusted decision.
- The daemon (non-buildkit) executor behavior and its ephemeral builder synthesis.
- Any lifecycle change (the on-disk `/cnb` + `order.toml` contract the detector reads is
  already satisfied by the existing inject path).
- FR-8 / FR-9 investigations (tracked in `platform-1662-buildkit-followups`).
