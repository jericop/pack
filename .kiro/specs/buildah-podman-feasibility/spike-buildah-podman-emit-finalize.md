# Spike: buildah/podman as a second build backend for the emit/finalize contract

> **STATUS — THEORETICAL feasibility analysis (no local buildah/podman).** This is
> a code/library review only. buildah and podman are Linux-only (no macOS/Windows
> native support), and this machine has neither installed, so nothing here has been
> run. Every claim is grounded in the buildah/podman/containers Go libraries and the
> emit/finalize contract as implemented on `buildkit-native-export`. Runnable
> validation is explicitly out of scope for this spike and is called out as
> follow-up.

## Question

Can buildah's / podman's Go libraries consume the **emit/finalize contract** we built
for the BuildKit-native backend to produce **multi-arch CNB images** — i.e. can a
`buildah` backend be dropped in behind the existing `BuildBackend` abstraction
without changing the lifecycle contract? (We call the backend `buildah` — that is
the build library; podman is the sibling container tool named alongside it for
context.)

## TL;DR

**Yes, it is feasible, with one real design decision to make (layer granularity).**

- The contract is already **builder-agnostic by construction** — the emit design
  reserves recorder namespaces for `buildah`/`podman`, and the `finalize` library and
  the `io.buildpacks.lifecycle.prepared-metadata` label carry no BuildKit specifics.
- **finalize is reusable as-is.** It is pure go-containerregistry (`remote`/`mutate`)
  operating on a pushed image or index; it does not know or care which engine built
  the image. A buildah backend reuses `finalize.Finalize` unchanged.
- The **assembler** role (run the lifecycle phases, then assemble `FROM run-image` +
  add each emitted layer + apply config + push) maps cleanly onto buildah's Go API
  (`buildah.NewBuilder` / `Builder.Add` / config setters / `Commit`) and onto the
  `buildah manifest` API for the multi-arch index.
- The **one decision** that actually matters: buildah's default `container → commit`
  model squashes all added content into a **single** layer, whereas the CNB contract
  and finalize assume **one image layer per CNB layer**. A buildah backend must
  produce per-CNB-layer image layers (commit-per-layer, or the containers/storage
  layer API). This is a known, supported pattern, but it is the crux of the port.
- Biggest *practical* caveats are environmental, not contractual: buildah/podman are
  **Linux-only** and multi-arch relies on **QEMU emulation** per arch (same tradeoff
  BuildKit has), so there is no macOS local-dev story like the current one.

## Background: what the contract actually requires of a backend

From the implemented contract (`phase/emit` + `phase/finalize` in cnb-lifecycle,
consumed by `internal/build/multiplatform` in cnb-pack), a build backend has exactly
two responsibilities. Neither is BuildKit-specific.

### Role 1 — Assembler (engine-specific)

Per target platform:

1. **Run the lifecycle phases** (analyzer → detector → restorer → builder → exporter
   in emit-mode) against the builder image, as the CNB user, with the buildpack cache
   available and `CNB_REGISTRY_AUTH` for the credentialed phases. Emit-mode writes:
   - `plan.json` — the ordered layer plan (each NEW layer carries a filesystem
     **Source** ref: `dir`/`file`, optional `include` for app slices, `uid`/`gid`,
     optional `mode`/`dest`); reused layers carry only a `diffID`.
   - `config.json` — entrypoint/cmd/workingDir/env/labels.
   - `build-metadata.json` — the serialized `BuildMetadata` (plan + emitted labels)
     that becomes the `io.buildpacks.lifecycle.prepared-metadata` image label.
2. **Assemble the app image** = `FROM <run-image>` (digest-pinned, read from the
   analyzer's `/layers/analyzed.toml`) + for each NEW layer, copy its files from the
   emitted Source (buildpack layers from `/layers/<bp>/<layer>`, app from
   `/workspace`, launcher file; app slices honored by include patterns; synthesized
   layers like process-types from a tiny tree), chowned to the emitted uid/gid.
   Reused run-image layers are already in the base.
3. **Apply the image config** (entrypoint/cmd/workingDir/env) and set the single
   `io.buildpacks.lifecycle.prepared-metadata` label.
4. **Push** one image per platform, assembled into one OCI index (no intermediate
   per-arch tags).

The engine computes its **own** layer diffIDs during assembly (that is the whole
reason finalize exists).

### Role 2 — Finalizer (engine-agnostic; already done)

`finalize.Finalize(ctx, imageRef, Options{...})` reads the pushed image/index's
produced diffIDs + the prepared-metadata label, positionally maps intended→produced
diffIDs, authors `io.buildpacks.lifecycle.metadata`, and re-pushes config+manifest(+
index) only. It uses go-containerregistry against a registry — **no engine coupling**.
A buildah backend calls it unchanged.

**Consequence:** the port is entirely about Role 1. Role 2 is free.

## Mapping the assembler onto the buildah Go API

buildah exposes a daemonless, rootless-capable Go API (module `go.podman.io/buildah`,
formerly `github.com/containers/buildah`), backed by containers/storage and
containers/image. The relevant surface (confirmed from the buildah "include in your
build tool" tutorial + the config/commit/manifest docs):

| Contract step | buildah Go API |
|---|---|
| Run lifecycle phases | `builder.Run([]string{...}, RunOptions{Isolation, Terminal})` — runs a command in the working container (the analyzer/detector/restorer/builder/exporter binaries from the builder image). |
| `FROM run-image` | `buildah.NewBuilder(ctx, store, BuilderOptions{FromImage: <run-image>})` — working container based on the run image. |
| Copy a layer from its Source | `builder.Add(dest, extract=false, AddAndCopyOptions{Chown:"uid:gid", ...}, src...)` — copies files/dirs into the container rootfs. `include` (app slices) needs pre-selection (see caveats). |
| Apply config | `builder.SetEntrypoint`, `SetCmd`, `SetWorkingDir`, `SetEnv`, `SetLabel` (the same setters the `buildah config` CLI wraps). |
| Set prepared-metadata label | `builder.SetLabel("io.buildpacks.lifecycle.prepared-metadata", <json>)`. |
| Produce the image | `builder.Commit(ctx, imageRef, CommitOptions{...})`. |
| Push | `buildah.Push(ctx, image, dest, PushOptions{...})` or commit straight to a registry ref. |
| Multi-arch index | `buildah manifest` API: create a list, `manifest add` each per-arch image, `manifest push` the index. |

So the shape of a `BuildahBackend` implementing the existing `BuildBackend`
interface is straightforward:

```go
// sketch — NOT compiled; illustrates the mapping only
func (b *BuildahBackend) Build(ctx, opts PlatformBuildOpts) (PlatformBuildResult, error) {
    // 1. working container FROM builder image; Run() the lifecycle phases in emit-mode
    // 2. read plan.json/config.json/build-metadata.json out of the container rootfs
    // 3. NEW working container FROM run-image (from analyzed.toml)
    // 4. for each NEW layer in plan order: Add(source -> dest, Chown uid:gid)  // ONE layer each
    // 5. apply config setters + SetLabel(prepared-metadata)
    // 6. Commit -> push per-arch image
}
// then: manifest add per-arch -> manifest push index -> finalize.Finalize(indexRef)
```

`Capabilities()` would report `PushesNatively: true` (same as the buildkit backend),
so the existing executor skips its own assembly/push — no executor change needed.

## The one real design decision: per-CNB-layer image layers

This is the crux and the only part that is not a mechanical mapping.

**BuildKit** gives us one image layer per `llb.Copy`, so the emitted plan's NEW layers
map 1:1 onto produced image layers, and finalize's *positional* intended→produced
diffID mapping (the NEW layers occupy the trailing N positions of `RootFS.DiffIDs`, in
plan order) holds naturally.

**buildah's default model** is: mutate a working container's read-write layer, then
`Commit` → the commit produces a **single** new layer containing everything added
since the base (unless configured otherwise). A naive `NewBuilder(FROM run-image)` +
several `Add` + one `Commit` would yield **one** app layer, not one-per-CNB-layer.
That breaks:

- **finalize's positional mapping** (it expects `len(new layers)` trailing diffIDs,
  one per plan entry), and
- **rebase / buildpack-layer patching**, which operate per CNB layer.

Two supported ways to get per-CNB-layer granularity with buildah:

1. **Commit-per-layer chain.** Build the image as a chain: start `FROM run-image`;
   for each NEW plan layer, `Add` just that layer's files and `Commit` to an
   intermediate image; use that as the base for the next. Each commit adds exactly one
   layer, preserving order and 1:1 mapping. containers/storage supports layered
   commits (this is what `buildah build --layers` relies on for Dockerfile caching).
   Cost: N commits per arch (N = number of CNB layers). For our sample apps N ≈ 20–40,
   which is fine.
2. **containers/storage layer API directly.** Create each layer from its tar/diff via
   the storage driver and assemble the manifest+config by hand (closer to what
   go-containerregistry `mutate.Append` does). More control, more code; effectively
   re-implements what finalize+ggcr already do on the registry side, so likely not
   worth it.

**Recommendation:** option (1), commit-per-layer. It stays within buildah's supported
API, produces a real per-CNB-layer image, and keeps finalize's positional mapping
valid with zero contract change. The emitted plan already has everything needed
(ordered layers, per-layer Source, uid/gid), so the backend just walks the plan.

> Note: whichever option, buildah computes its **own** diffIDs (recreated tars are not
> byte-identical to the lifecycle's), exactly like BuildKit. That is expected and is
> precisely why finalize authors metadata from produced diffIDs. No lifecycle change.

## What maps for free (no contract change)

- **finalize** — reused verbatim (registry-side, ggcr).
- **prepared-metadata label** — already builder-agnostic
  (`io.buildpacks.lifecycle.prepared-metadata`); the emit code comments explicitly
  name buildah/podman as future producers.
- **emit recorder namespacing** — the emit output is under a `buildkit/` recorder
  subdir with a `schema` string precisely so a `buildah/` or `podman/` recorder could
  coexist. For a buildah backend we likely do **not** even need a new recorder: the
  same emit-mode output (plan/config/build-metadata) is engine-neutral; the backend
  just consumes it differently. A distinct recorder is only warranted if buildah needs
  a materially different emit shape (not anticipated).
- **The `BuildBackend` interface + factory + `--build-backend` flag** — retained for
  exactly this. Adding `BackendBuildah BackendType = "buildah"` + a factory branch
  is the entire wiring surface on the pack side.
- **App slices** — the plan carries per-slice `include` patterns; buildah `Add`
  copies a set of sources, so slices are honored by pre-selecting the include set
  (or copying into a staging dir first). Slightly more manual than `llb.Copy`
  `IncludePatterns`, but no contract change.

## Caveats / risks (mostly environmental, not contractual)

1. **Linux-only, cgo-heavy.** buildah/podman depend on containers/storage +
   containers/image, which need `libbtrfs`, `libgpgme`, `libassuan`, etc., and build
   tags. They do not run natively on macOS/Windows. The current pleasant macOS
   local-dev loop (BuildKit in a `docker-container` builder) has **no** buildah
   equivalent; dev/CI would be Linux-only or in a Linux VM/container.
2. **Multi-arch = QEMU emulation.** buildah/podman build non-native arches via
   `qemu-user-static` + binfmt_misc, per the podman multi-arch guidance. Same
   performance tradeoff BuildKit has under emulation; no better, no worse in principle.
   Native multi-node builds are not a buildah feature the way BuildKit `--append` is.
3. **Layer cache story differs.** BuildKit's content-addressed per-vertex cache
   (phase RUNs + assembly COPYs `CACHED`) is central to our rebuild speedups and to
   **remote registry cache** (`--buildkit-cache-to/from`). buildah's caching is
   `buildah build --layers` Dockerfile-instruction caching + storage reuse; there is
   **no direct equivalent to registry cache import/export** for a programmatic
   commit-per-layer assembly. Rebuild performance would need its own measurement — the
   emit/finalize *correctness* is unaffected, but the perf profile is an open question
   and likely worse for cold/ephemeral CI without remote cache.
4. **No layer-egress property needs re-checking.** BuildKit's win is that large layer
   data never leaves the engine's content store; only small metadata crosses to the
   host, and finalize pulls config+manifest only. With buildah, assembly happens in
   local containers/storage and push goes straight to the registry, so the "no egress"
   property is preserved *in spirit* (data stays in storage → registry), but the exact
   IO profile (storage driver writes, commit-per-layer) differs and should be measured.
5. **Running the lifecycle inside buildah.** The phases would run via `builder.Run`
   (chroot/oci isolation) rather than BuildKit RUN steps. `-skip-chown` is still
   needed (unprivileged), and rootless buildah adds user-namespace considerations. The
   phase invocations themselves are identical (same binaries, same flags), so this is
   plumbing, not a contract change.
6. **podman vs buildah split.** For *building*, buildah's Go API is the right library
   (podman wraps buildah for build and focuses on run/pods). "podman" in the backend
   name is really about the container-tools ecosystem; the actual build library is
   buildah. A future run/verify convenience could use podman, but it is not needed for
   the backend.

## Recommended next steps (if we pursue it)

1. **Prototype the commit-per-layer assembler on Linux** (VM or container): given an
   existing emit output (plan/config/build-metadata) from a real build, assemble
   `FROM run-image` with buildah commit-per-layer, push, then run the existing
   `finalize.Finalize` + `pack image-metadata verify`. This isolates Role 1 and proves
   the 1:1 layer mapping + finalize compatibility without wiring a full backend.
2. **Measure** cold/rebuild/rebase against the BuildKit numbers, especially without a
   remote-cache equivalent, to quantify the perf gap.
3. **Then** implement the `buildah` backend (`BackendBuildah`) behind the existing
   factory, gated Linux-only, reusing finalize and the existing emit-mode output.
4. Keep the contract unchanged; only add a `buildah/` emit recorder **if** step 1
   surfaces a concrete need (not anticipated).

## Bottom line

The emit/finalize design does what it was intended to do here: because it decouples
"who assembles the layers / owns the diffIDs" (engine) from "who authors CNB metadata"
(the lifecycle finalize library), a second engine is a **backend-only** addition.
buildah's Go API covers every assembler step, finalize is reused untouched, and the
`BuildBackend` abstraction was kept for precisely this. The single substantive
engineering decision is producing **one image layer per CNB layer** (commit-per-layer),
and the primary costs are environmental (Linux-only, QEMU, no drop-in remote cache),
not contractual. No lifecycle/spec change is required to support it.
