# Design: Skip the daemon-synthesized ephemeral builder on the buildkit backend

File paths are relative to the fork repo root (`buildkit-native-export-with-history-and-kiro`
branch). Performance follow-up to FR-7 in `platform-1662-buildkit-followups`.

## Key finding (confirmed in source)

The buildkit backend ALREADY assembles the "base builder + added modules" over LLB and does
NOT consume the ephemeral daemon builder image:

- `pkg/client/build.go` buildkit branch: `stageExtraBuildpacks(fetchedBPs)` → `extraBPDir`;
  `buildMultiPlatform(..., extraBPDir, order, orderExtensions)`.
- `buildMultiPlatform`: `platformBuildOpts.BuilderImage = lifecycleOpts.BuilderImage`
  (= base `builderRef`), `ExtraBuildpacksDir = extraBPDir`, `OrderToml =
  builder.OrderTOML(order, ...)`.
- `native_buildfunc.go` `buildEmitLLB`: `base := llb.Image(in.builderImage)` (base builder),
  `[plat] add user buildpacks` copies over `/cnb/buildpacks`, `[plat] write order.toml`
  writes `/cnb/order.toml`; detector reads that on-disk order (no `-order` flag).

`createEphemeralBuilder` → `Builder.Save` (the `pack.local/builder/<hex>` daemon load) runs
EARLIER in `Client.build` for ALL backends, but the buildkit path never uses its output. So
the fix is to NOT do that daemon synthesis on the buildkit path — not to build any new LLB
assembly (it already exists).

## What still reads the ephemeral builder (and how to rebase to the base)

`platformBuildOpts` reads from `lifecycleOpts.Builder` (the ephemeral Builder Go object):

| Field | Current source | Base-derivable? |
|---|---|---|
| `BuilderUID`/`BuilderGID` | `lifecycleOpts.Builder.UID()/GID()` | Yes — base builder user/group (unchanged by adding bps) |
| `StackID` | `builderImageLabel(lifecycleOpts.Builder.Image(), "io.buildpacks.stack.id")` | Yes — base image label |
| `TargetDistroName/Version` | `builderDistroLabel(lifecycleOpts.Builder.Image(), ...)` | Yes — base image label |
| `PlatformAPI` | `negotiatePlatformAPI(lifecycleOpts)` (builder lifecycle descriptor) | Yes — base builder's lifecycle descriptor |
| `LifecycleImage` | `ephemeralBuilder.Name()` | Only used by untrusted-lifecycle-image path; fork builder is trusted → not needed for the buildkit trusted case |

All are properties of the BASE builder, unchanged by adding buildpacks. So a base
`*builder.Builder` (built with `builder.New(rawBuilderImage, rawBuilderImage.Name(),
builder.WithoutSave())` — the pattern `createEphemeralBuilder` already uses when no ephemeral
builder is needed) provides everything the buildkit path reads.

## Proposed change (surgical)

In `pkg/client/build.go` `Client.build`, branch the ephemeral-builder step by backend:

- **Daemon backend (unchanged):** `createEphemeralBuilder(...)` as today (with FR-7 flatten +
  `Save` daemon load). `lifecycleOpts.Builder = ephemeralBuilder`.
- **Buildkit backend:** do NOT synthesize/`Save`. Build a base builder object without saving
  (`builder.New(rawBuilderImage, rawBuilderImage.Name(), builder.WithoutSave())`) purely to
  read UID/GID/labels/lifecycle-descriptor, and set:
  - `lifecycleOpts.Builder = <base builder object>` (no daemon image),
  - `lifecycleOpts.BuilderImage = builderRef.Name()` (already the base),
  - keep `extraBPDir`/`order` flowing as today so `buildEmitLLB` injects them.

Because the buildkit branch's `platformBuildOpts` already reads only base-derivable fields
from `lifecycleOpts.Builder`, swapping the ephemeral object for a base (unsaved) object is
sufficient — no changes needed in `native_buildfunc.go`.

Two implementation options for the branch point:

1. **Preferred:** teach `createEphemeralBuilder` (or its caller) that the buildkit backend
   never needs a saved ephemeral image — return a `WithoutSave()` base builder object and
   skip `bldr.Save()`. The added modules are already carried separately via `fetchedBPs`
   → `stageExtraBuildpacks` → `ExtraBuildpacksDir`, and the order via `order`/`OrderTOML`, so
   nothing is lost by not baking them into an image.
2. Alternative: keep `createEphemeralBuilder` as-is but make its `Save` a no-op for the
   buildkit backend. (Less clean; option 1 avoids constructing an image object at all.)

## Extensions caveat (FR-5)

The `[plat] add user buildpacks` copy targets `/cnb/buildpacks`. Extensions live under
`/cnb/extensions` and drive a different (Dockerfile) flow. If a build adds EXTENSIONS, confirm
the buildkit path injects `/cnb/extensions` + the extension order equivalently; if not, fall
back to daemon synthesis for the extensions case and scope this spec to buildpacks + order +
env + run image first.

## Files to touch

- `pkg/client/build.go`: branch the ephemeral-builder creation by backend (option 1 above);
  for buildkit, use a `WithoutSave()` base builder object and skip the daemon load. No new LLB
  code.
- Possibly `createEphemeralBuilder` signature/behavior to accept a "backend needs no saved
  image" flag (or a sibling helper) — keep the daemon path byte-for-byte identical.

## Risks

- **Something in the daemon-only fields differs on the ephemeral vs base builder.** Mitigated
  by the table above: every field the buildkit path reads is a base property unchanged by
  adding buildpacks. Verified by reading `platformBuildOpts` construction.
- **Extensions.** Handled by FR-5 fallback.
- **Trusted-lifecycle assumption.** The buildkit path uses the builder's bundled lifecycle;
  `LifecycleImage` is only consumed by the untrusted-lifecycle-image path. If an untrusted
  buildkit build with a real lifecycle image is ever needed, that path would still need a
  lifecycle image name — out of scope here (fork builder is trusted).

## Verification

- Unit: assert the daemon path still synthesizes + saves (unchanged); assert the buildkit
  path constructs no `local.NewImage`/`pack.local/builder` and still passes the base
  UID/GID/stack/distro/platformAPI into `platformBuildOpts`.
- MVP/local: `--build-backend buildkit` builds of nodejs (`--buildpack`) and
  agent-patcher-service (custom `project.toml` order). Assert `docker images` shows NO
  `pack.local/builder/<hex>` (AC-1), same detected buildpacks + runnable multi-arch image
  (AC-2), and record the removed synthesis time (AC-3). Use the per-arch-split timed-build
  wrapper.
