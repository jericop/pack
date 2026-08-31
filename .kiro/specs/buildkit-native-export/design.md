# Design: BuildKit-Native Export (Option A — build-then-finalize)

## Overview

The `buildkit` backend lets BuildKit BUILD and PUSH the CNB app image
natively, then runs a lifecycle-owned FINALIZE step that authors the correct CNB
metadata on the pushed image from its ACTUAL produced layers. No custom BuildKit
gateway frontend, no per-layer re-extraction, no post-push layer changes.

Experimental, opt-in: `--build-backend buildkit`. The build backend is now a
first-class, capability-driven concept:

- **`docker-daemon`** is an official `BackendType` and the DEFAULT (`""` and
  `auto` resolve to it). It is the standard single-arch container build against the
  local Docker daemon; A-lite routing keeps its execution on the existing daemon
  lifecycle executor (it is not a `BuildBackend.Build` implementation).
- **`buildkit`** is the native build-then-finalize backend described here.
- **`buildah`** is planned; adding it is a new `--build-backend` value, not a new
  flag.

Platform selection is unified under a single repeatable `--platform` flag (the old
separate `--platforms` was removed). How many platforms a build may specify is a
backend CAPABILITY, not a per-backend CLI branch: `BackendCapabilities.MaxPlatforms`
is `1` for `docker-daemon` (single-arch) and `0` (unlimited) for `buildkit`. The CLI
resolves the backend, then rejects more `--platform` values than the backend allows.
When no `--platform` is given, the build defaults to the HOST platform
(`runtime.GOOS/GOARCH`) for both backends — the buildkit backend no longer defaults
to the builder image's arch (which could force emulation on a non-amd64 host).

Backend-specific FLAGS are grouped with the backend the same way, via capabilities
rather than per-backend CLI branches. `BackendCapabilities.UsesLifecycleCache` is
`true` for `docker-daemon` and `false` for `buildkit`: the lifecycle-cache flags
`--cache`, `--cache-image`, and `--clear-cache` belong to the docker-daemon backend
(the buildkit backend uses BuildKit's own vertex cache via `--buildkit-cache-from`/
`--buildkit-cache-to` and never deletes from a registry, so `--clear-cache` is
meaningless). The CLI rejects these flags on a backend whose `UsesLifecycleCache` is
false with a message pointing to the buildkit equivalents, rather than silently
ignoring them. `--tag` is universal (all registry backends can push multiple names)
and IS supported by buildkit. Deciding NOT to overload `--cache` with buildkit cache
config was deliberate: BuildKit's directional import/export + `mode`/`type` cache
semantics do not map cleanly onto `--cache`'s `type=build/launch;format=...` grammar,
and `--buildkit-cache-from`/`--buildkit-cache-to` already mirror `docker buildx`.

The `BuildBackend` interface / `BackendType` enum / factory / `--build-backend` flag
are retained for the future `buildah` backend.

> NOTE (as-implemented): this spec was written during the spike. The backend value
> is `buildkit` (was `buildkit-native`); the build-phase label is
> `io.buildpacks.lifecycle.prepared-metadata` (was
> `io.buildpacks.buildkit.native.build-metadata`); the (implemented) self-heal flag
> is `--fix-image-metadata`. The earlier `buildkit-dockerfile`/`buildkit-llb`
> backends and the OCI-layout export mode have been removed. Read the older names
> below as the current ones.

This design SUPERSEDES the earlier "custom frontend + host-side metadata-SHA
rewrite" design. See `cnb-lifecycle/.kiro/specs/cnb-buildkit-frontend/spike-eliminate-metadata-rewrite.md`
for the full decision record and rejected alternatives. Sections below marked
"(SUPERSEDED)" are retained for history only.

## Why Option A (decision summary)

- BuildKit's image exporter ALWAYS derives the final layer diffIDs from the LLB
  result ref's actual layer chain; a frontend CANNOT inject pre-built blobs/diffIDs
  via the gateway result (verified in moby/buildkit v0.32.2:
  `exporter/containerimage/writer.go` `patchImageConfig`; `frontend/gateway/client`
  `Reference` has no blob-attach). So we cannot make BuildKit adopt the lifecycle's
  emitted diffIDs.
- Therefore metadata must either be RECONCILED to the produced diffIDs after they
  exist, or we must avoid re-assembly (OCI-layout import — disk materialization +
  run image pulled in-build). Option A chooses the first, but authored cleanly by
  the lifecycle from a build-phase plan label, not patched as a pack workaround.
- Recomputed diffIDs do NOT hurt BuildKit's build cache (it keys on the LLB op
  graph, not output diffIDs). They only matter for the CNB analyzer's previous-image
  restore and cross-image blob dedup — both addressed by making the metadata match
  the produced layers.

## Architecture

```
pack build --build-backend buildkit --platform linux/amd64 --platform linux/arm64 --publish
        │
        ▼  (host)
┌──────────────────────────────────────────────────────────────┐
│ NativeBackend (pack)                                           │
│  Phase 1 — BUILD (delegate to BuildKit / LLB):                 │
│    per platform (parallel):                                    │
│      FROM builder → COPY app → analyzer → detector → restorer  │
│      → builder → exporter (assemble FROM run-image)            │
│    + attach io.buildpacks.buildkit.native.build-metadata label │
│    BuildKit pushes ONE native multi-arch image (manifest list),│
│    no intermediate tags. Layer data stays in BuildKit.         │
│                                                                │
│  Phase 2 — FINALIZE (call lifecycle finalize library):         │
│    lifecycle.Finalize(imageRef):                               │
│      read produced diffIDs (image config) + build-metadata     │
│      label → author io.buildpacks.lifecycle.metadata → re-push │
│      config+manifest(+index) ONLY (no layers)                  │
└──────────────────────────────────────────────────────────────┘
        │
        ▼
   registry: runnable, rebuildable, rebaseable CNB image
```

## Components and Interfaces

### `NativeBackend` + in-process BuildFunc (pack, `internal/build/multiplatform/`)

NO separate gateway frontend (`cnbfrontend` package + `cmd/cnb-frontend` are
RETIRED). Pack drives the build via `client.Build` with an IN-PROCESS BuildFunc it
owns. (Setting the output image config/labels requires the gateway result API —
`client.Solve` alone cannot, verified in moby/buildkit v0.32.2 — but this is pack's
own build function, not a separately-deployed frontend component.) The BuildFunc,
per platform:

- runs the lifecycle phases as LLB RUNs (analyzer/detector/restorer/builder/exporter
  in emit-mode) with the unprivileged-BuildKit flags (`-skip-chown` etc.),
- reads the emit output — the ordered plan whose NEW layers carry SOURCE REFS
  (`source.dir` + optional `include`/`dest`/uid/gid) — plus build-metadata.json,
- assembles `FROM run-image` by, per NEW layer, `llb.Copy` from the emitted
  `source.dir` (applying `include`/`dest` and chown to the emitted uid:gid) onto the
  base. ONE `llb.Copy` per CNB layer (boundaries preserved). NO `tar -xf`, NO
  run-image shell/tar, NO materialization of large layers. Synthesized layers with
  no filesystem source (e.g. process-types) are copied from a tiny emitted tree.
- sets the image config (entrypoint/cmd/workingdir/env) + the
  `io.buildpacks.buildkit.native.build-metadata` label via the gateway result
  (`ExporterImageConfigKey`); does NOT write a valid final
  `io.buildpacks.lifecycle.metadata`,
- returns per-platform refs so BuildKit pushes ONE image (multi-arch = one index),
  no intermediate tags.

`NativeBackend` then calls `finalize.Finalize` (Phase 2). `Capabilities().PushesNatively = true`.

### Build environment (parity with standard pack) — Requirement 11

Because the lifecycle runs inside BuildKit (not in a pack-managed container), the
BuildFunc must reproduce the environment standard pack provides. Threaded as
`PlatformBuildOpts` → `nativeBuildInputs` and applied in `buildEmitLLB`:

- **CNB platform env** on every lifecycle RUN (process env via `llb.AddEnv`):
  `CNB_PLATFORM_API`, `CNB_USER_ID`, `CNB_GROUP_ID`, `CNB_STACK_ID`, `CNB_TARGET_OS`,
  `CNB_TARGET_ARCH`, `CNB_TARGET_ARCH_VARIANT` (if any), `CNB_TARGET_DISTRO_NAME`,
  `CNB_TARGET_DISTRO_VERSION`, `CNB_EXPERIMENTAL_MODE` (when experimental),
  `SOURCE_DATE_EPOCH` (when a creation time is set), `CNB_REGISTRY_AUTH`. The
  target/stack values are sourced in `pkg/client/build.go` (`buildMultiPlatform`)
  from the builder image labels (`io.buildpacks.stack.id`,
  `io.buildpacks.base|stack.distro.{name,version}`) and the per-platform target.
  Rationale: `CNB_STACK_ID` + the target vars are what let packit's `postal` resolver
  match stack/target-specific PREBUILT dependencies; without them a buildpack falls
  back to a wildcard-stack SOURCE build (observed: CPython compiled from source ~90s
  vs prebuilt ~10s).
- **User build env** — `--env` / `--env-file` + project descriptor `[[build.env]]`
  (merged, `--env` wins) — written as files `/platform/env/<NAME>` in the LLB (one
  `llb.Mkfile` per var, deterministic key order for cache stability). This is the CNB
  platform contract the lifecycle build phase reads to expose `BP_*` config to
  buildpacks. Mirrors standard pack's builder env layer (`envLayer` →
  `/platform/env`).
- **Proxy env** — `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` (upper + lower case), resolved
  via `Client.processProxyConfig` (explicit opts else host env), matching
  `WithLifecycleProxy`.
- **Not set** (N/A for publish-only): `CNB_USE_LAYOUT` / `CNB_LAYOUT_DIR`.

### Build-metadata label (contract with the lifecycle)

`io.buildpacks.buildkit.native.build-metadata` — a JSON image label carrying the
ordered plan the lifecycle emit-mode computed. Owned/defined by the lifecycle spec.
Pack does not author its contents; it ensures the label is present on the built
image and passes the image ref to finalize. Contents (see lifecycle design):
ordered layers (new-vs-reused, intended diffID, identity, history) + run-image
reference/topLayer + a `schema` version.

### Finalize (lifecycle library, consumed by pack)

Pack imports the lifecycle finalize package and calls it after the push, the way
`pkg/client/rebase.go` calls `phase.Rebaser`. Signature (illustrative; the
lifecycle spec is authoritative):

```go
// in cnb-lifecycle, consumed by pack
finalize.Finalize(ctx, imageRef string, opts finalize.Options) error
```

Finalize reads the pushed image (config + manifest, or index → children), reads the
`build-metadata` label + the produced diffIDs, authors
`io.buildpacks.lifecycle.metadata` (per-layer SHAs = produced diffIDs; RunImage
boundary), and re-pushes config+manifest(+index) only. Handles single image and
manifest list. Idempotent and tag-atomic.

**Authenticated fetch (Requirement 4.5).** The finalize fetch immediately follows
the build's push to the SAME registry, so it must use the build's credentials, not
anonymous access. Pack threads its resolved keychain
(`Client.keychain` → `PlatformBuildOpts.Keychain` → `finalize.Options.Keychain`);
`finalize.Options.Keychain` defaults to `authn.DefaultKeychain` when nil. This was
added after observing that an anonymous finalize fetch failed builds under registry
load with Docker Hub `TOOMANYREQUESTS` (the build pushed successfully, then finalize
could not pull the config/manifest back to author metadata). Authenticating the
fetch uses the higher per-account rate limit. (Separately, the builder/run-image
pulls happen inside BuildKit via the session Docker auth provider — see
`newDockerAuthProvider`; heavy concurrent CI benchmarking can still exhaust registry
quotas and is mitigated at the workflow level, not in the backend.)

### `metadata_rewrite.go` (pack) — becomes a thin caller or is removed

The host-side rewrite logic moves into the lifecycle finalize library. Pack's
`metadata_rewrite.go` either (a) becomes a thin wrapper that calls
`finalize.Finalize`, or (b) is deleted once `NativeBackend` calls finalize directly.
The `PACK_HOST_REGISTRY_REMAP` test-env shim (buildkit-vs-host registry name split)
is retained where finalize needs a host-reachable ref in local testing.

## Data Models

- Build-metadata label: the serialized ordered plan (lifecycle-owned schema).
- Produced diffIDs: read from each per-arch image config `RootFS.DiffIDs`.
- `io.buildpacks.lifecycle.metadata`: authored by finalize (the lifecycle's
  `files.LayersMetadata`).

## Multi-arch + no intermediate tags

BuildKit's native multi-platform push produces ONE manifest list (per-arch images
held in the content store; index pushed atomically) — no intermediate per-arch
tags. Finalize updates each child image's config + the index in place (same final
tag), touching no layers, so it introduces no tags either. Pack's separate
`PushPerArchLayoutsAsManifestList` assembly is NOT needed for this backend.

## Caching

BuildKit caches by the LLB operation graph; unchanged app source + builder →
cache-hit on rebuild regardless of output diffIDs. Per-arch persistent cache mount
for buildpack layers (as the LLB backend does). Recomputed diffIDs never affect the
build cache.

## Error Handling

- Build phase failure → normal BuildKit error surfaced per platform.
- Missing build-metadata label on the pushed image → finalize errors clearly
  (the build did not attach it).
- Finalize failure → the pushed (pre-finalize) image remains at the tag, pullable
  and runnable but not yet rebuildable/rebaseable (Requirement 9.4). Re-running the
  build (or, post-MVP, the self-healing fix) finalizes it. Tag-atomic re-push means
  no partial/corrupt manifest at the tag.

## Testing Strategy

Local MVP (no new `PACK_TEST_*` gates): drive `samples/go/no-imports` to a local
registry and verify REPEATED cycles — ≥2 rebuilds (analyzer previous-image restore
succeeds; all per-layer metadata SHAs == produced diffIDs each time), ≥2 rebases
(each succeeds), rebuild-after-rebase, and multi-arch (one index, no intermediate
tags). Confirm finalize is config+manifest-only (no layer re-upload). Automated
tests follow after the MVP is confirmed.

---

# (SUPERSEDED) Earlier design: custom frontend + host-side metadata-SHA rewrite

The content below described the previous approach (a custom CNB BuildKit gateway
frontend that re-extracted layers, with pack rewriting the metadata SHAs post-push).
It is retained for history. The current design is Option A above; the frontend and
the pack-owned rewrite-as-workaround are retired. See the spike decision record.
