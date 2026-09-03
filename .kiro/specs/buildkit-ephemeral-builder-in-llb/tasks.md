# Tasks: Skip the daemon-synthesized ephemeral builder on the buildkit backend

Performance follow-up to FR-7 (`platform-1662-buildkit-followups`). The buildkit path already
injects buildpacks + writes `/cnb/order.toml` over LLB using the BASE builder; this work just
STOPS synthesizing/loading the wasted `pack.local/builder/<hex>` daemon image on that path.

## Task 1: Confirm the buildkit path needs nothing from the ephemeral IMAGE (done — recorded)
- [x] Confirmed: `platformBuildOpts.BuilderImage` = base `builderRef`; added buildpacks flow via
      `stageExtraBuildpacks(fetchedBPs)` → `ExtraBuildpacksDir`; order via `builder.OrderTOML`
      → `/cnb/order.toml`; detector reads on-disk order (no `-order`); no `buildpack.layers`
      label needed (--buildpack builds succeed without it).
- [x] Confirmed residual couplings to `lifecycleOpts.Builder` are all BASE-derivable
      (UID/GID, stack id, target distro, platform API; LifecycleImage only for untrusted
      lifecycle-image path).
- References: requirements "Confirmed current behavior", design "Key finding"

## Task 2: Branch the ephemeral-builder step by backend in Client.build
- [ ] For the buildkit backend, do NOT call `Builder.Save` / create `pack.local/builder/<hex>`.
      Construct a base builder object with `builder.New(rawBuilderImage, rawBuilderImage.Name(),
      builder.WithoutSave())` (the pattern `createEphemeralBuilder` already uses when no
      ephemeral builder is needed) and use it for `lifecycleOpts.Builder`.
- [ ] Keep `lifecycleOpts.BuilderImage = builderRef.Name()` (base) and keep the
      `fetchedBPs`→`stageExtraBuildpacks`→`ExtraBuildpacksDir` + `order`→`OrderTOML` flow
      exactly as today.
- [ ] Leave the daemon (non-buildkit) path calling `createEphemeralBuilder` + `Save`
      (with FR-7 flatten) byte-for-byte unchanged.
- References: FR-1, FR-3, FR-4, design "Proposed change"

## Task 3: Extensions handling (scope guard)
- [ ] Determine whether the buildkit path injects `/cnb/extensions` + extension order over LLB.
      If YES, keep. If NO, fall back to daemon synthesis when extensions are present and scope
      this change to buildpacks + order + env + run image.
- References: FR-5, design "Extensions caveat"

## Task 4: Tests
- [ ] Unit: daemon path still synthesizes+saves (unchanged); buildkit path constructs no
      `local.NewImage`/`pack.local/builder` and still passes base UID/GID/stack/distro/
      platformAPI into `platformBuildOpts`.
- [ ] Keep the FR-7 flatten test green (daemon path).
- References: FR-4, AC-4

## Task 5: MVP/local verification (AC-1/2/3)
- [ ] `--build-backend buildkit` build of nodejs (`--buildpack docker.io/paketobuildpacks/
      nodejs:latest`): assert `docker images` shows NO `pack.local/builder/<hex>` (AC-1), same
      buildpacks detected + runnable multi-arch image (AC-2).
- [ ] Same for agent-patcher-service (custom `project.toml` order).
- [ ] Record before/after wall-clock: the removed ephemeral-builder synthesis + load
      (~16–85s baseline) via the per-arch-split timed-build wrapper (AC-3).
- References: AC-1, AC-2, AC-3, NFR-1

## Notes
- No new LLB assembly code is needed; the inject-and-order path already exists. The change is
  removing the wasted daemon synthesis on the buildkit path + sourcing base builder
  attributes.
- All shell execution via the stable runner with `export`ed env in command files; per-arch log
  split by the `[linux/<arch>]` prefix (NFR-2).
