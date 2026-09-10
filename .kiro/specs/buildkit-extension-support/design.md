# Design: BuildKit-native CNB image extension support

File paths are relative to the pack fork repo root
(`buildkit-native-export-with-history-and-kiro` branch).

## Background: how CNB extensions work

Image extensions let a build modify the **build image** and/or the **run image** via
generated Dockerfiles, rather than through buildpack layers. The lifecycle runs them in two
phases:

1. **Generation** — after detection selects a group (buildpacks + extensions), each
   extension's `/bin/generate` runs with `CNB_EXTENSION_DIR` (the extension root) and
   `CNB_OUTPUT_DIR` (a generated-output dir). It MAY write `build.Dockerfile` and/or
   `run.Dockerfile`, optional build-context folders (`context`, `context.build`,
   `context.run`), and `extend-config.toml` (key/value build args for `build.Dockerfile`).
   Extensions only contribute `provides` to the build plan; they never `require`.
2. **Extension (extend)** — the lifecycle applies the generated Dockerfiles to the base
   images. `build.Dockerfile` extends the build image before the buildpack `build` phase;
   `run.Dockerfile` extends (or switches) the run image used for export. Each Dockerfile is
   applied in generated order with build args `base_image` (previous stage / original),
   `build_id` (a UUID for cache invalidation), and `user_id`/`group_id` (original `User`).

The allowed instruction set for both Dockerfiles is fixed by
`buildpacks/spec/image_extension.md`: `FROM` (exactly one), `ADD`, `ARG`, `COPY`, `ENV`,
`LABEL`, `RUN`, `SHELL`, `USER`, `WORKDIR`. A `build.Dockerfile` MUST begin with
`ARG base_image` / `FROM ${base_image}`.

## Current state (what the buildkit backend does today)

- `pkg/client/build.go`: `processExtensions(...)` fetches extension modules (`fetchedExs`,
  `orderExtensions`) and `createEphemeralBuilder` stages them into the ephemeral builder;
  `hasExtensions` forces `useCreator = false`. On the buildkit backend,
  `skipEphemeralBuilderSave = isBuildkitBackend && !hasExtensions` — i.e. extensions DISABLE
  the ephemeral-builder-skip and fall back to the daemon-synthesized builder (a comment at
  ~L611 records that the buildkit path "does not yet inject /cnb/extensions or drive the
  Dockerfile-extension flow over LLB").
- `internal/build/multiplatform/native_buildfunc.go` `buildEmitLLB`: a FIXED per-platform
  sequence `analyzer → detector → restorer → builder → exporter(emit-mode)`. No generator, no
  extender. Any extensions staged in the builder are never consumed by the LLB build.

So today extensions are effectively ignored on the buildkit path (the daemon builder is
synthesized but the multi-arch LLB build doesn't run generate/extend).

## Daemon extension flow (the reference)

`internal/build/lifecycle_execution.go`:
- Detect/generate (platform API >= 0.13) produces `<layers>/generated/<ext-id>/build.Dockerfile`
  / `run.Dockerfile`; `CopyOutToMaybe(<layers>/generated, tmpDir)` copies it to the host.
- `hasExtensionsForBuild()` walks `<tmpDir>/generated` (either `generated/build/<bp>/Dockerfile`
  on old platforms or `generated/<bp>/build.Dockerfile` on new) to decide whether to extend
  the build image; `hasExtensionsForRun()` reads `analyzed.toml` for a run-image change.
- `ExtendBuild` / `ExtendRun` shell out to **kaniko** to apply the Dockerfiles, and
  `ExtendBuild` requires a volume build cache.

The reusable seam is the **generated-layout discovery** (the `hasExtensionsForBuild`-style
walk + locating `extend-config.toml` / `context*`); the Dockerfile APPLICATION is kaniko in
the daemon and is not reusable by LLB.

## The approach: translate the restricted Dockerfile to LLB (no kaniko)

BuildKit is a Dockerfile-execution engine, and the extension Dockerfile grammar is a fixed
10-instruction subset (see requirements). So the buildkit backend applies extension
Dockerfiles by translating them directly to LLB, as ordinary vertices in the per-platform
`buildEmitLLB` graph. Kaniko is neither needed nor appropriate here (it exists for the
no-BuildKit case), and dropping it also drops the volume-cache constraint and gives per-arch
correctness for free.

### Why not reuse BuildKit's `dockerfile2llb`

BuildKit ships a Dockerfile→LLB converter, but importing it pulls the full Dockerfile grammar
and a heavy moby dependency (GO-ARCH-2.8 dependency risk). We only need a fixed 10-instruction
subset, and we WANT to reject anything outside it (a build.Dockerfile with `VOLUME` or
`ENTRYPOINT` is a spec violation we should surface, not silently execute). A small hand-rolled
translator is bounded, dependency-free, and safer.

## Components

### 1. Restricted Dockerfile → LLB translator (new, buildkit-only)
`internal/build/multiplatform/dockerfile_llb.go` (new file).

```
type extendDockerfile struct { instructions []extInstr }        // parsed form
func parseExtensionDockerfile(src []byte) (*extendDockerfile, error)   // FR-2 parse + reject
type extendInput struct {
    base        llb.State          // the FROM base (build or run image for this leg)
    contextSrc  llb.State          // context / context.build / context.run, else app
    buildArgs   map[string]string  // base_image, build_id (UUID), user_id, group_id, + extend-config.toml
    platform    ocispecs.Platform
    plat        string             // "linux/arm64" progress prefix
}
func (d *extendDockerfile) toLLB(in extendInput) (llb.State, extImageConfig, error)  // FR-2 emit
```

- **Parser**: line-oriented, handles continuations (`\`), comments, `ARG`/`ENV` var
  interpolation, shell-form and exec-form `RUN`, and the `SHELL` override. Rejects any verb
  not in {`FROM`,`ADD`,`ARG`,`COPY`,`ENV`,`LABEL`,`RUN`,`SHELL`,`USER`,`WORKDIR`} and a second
  `FROM` (AC-4).
- **Translator**: `FROM ${base_image}` → `in.base`; `RUN` → `state.Run(argv, env, user, dir)`;
  `COPY`/`ADD` → `state.File(llb.Copy(in.contextSrc, src, dst))`; `ENV`/`ARG` → env carried to
  later `RUN`s + recorded in `extImageConfig`; `LABEL` → `extImageConfig.labels` (incl.
  `io.buildpacks.rebasable`); `USER`/`WORKDIR` → carried state; `SHELL` → argv prefix. `build_id`
  referenced in a `RUN` → mark that vertex `llb.IgnoreCache` (models "later layers rebuild").
- Returns the extended `llb.State` plus an `extImageConfig` (env/labels/user/workdir) that the
  emit/finalize applies to the image config.

### 2. Generated-layout discovery (shared sub-function — NFR-2)
Extract the daemon's generated-dir walking into a shared helper both paths call, e.g. in a
small shared location (candidate: `internal/build/generated_layout.go`):

```
type GeneratedLayout struct {
    BuildDockerfiles []GeneratedDockerfile   // per ext-id, in order
    RunDockerfiles   []GeneratedDockerfile
}
type GeneratedDockerfile struct {
    ExtID       string
    Path        string   // build.Dockerfile / run.Dockerfile
    ContextDir  string   // context.build|context.run|context|"" (=> app)
    ExtendArgs  map[string]string  // from extend-config.toml
}
func DiscoverGeneratedLayout(generatedDir string) (GeneratedLayout, error)
```

The daemon `hasExtensionsForBuild()` / `hasExtensionsForRun()` become thin wrappers over this
(behavior preserved). The buildkit path calls the same discovery against the generated output
it pulls out of the LLB generate-phase state. This is the honest reuse seam; the application
(kaniko vs LLB translator) stays path-specific.

### 3. Generator phase in LLB (new, in buildEmitLLB)
Between `detector` and `restorer`, when the order has extensions and platform API >= 0.13:

```
generatorArgs := {"/cnb/lifecycle/generator", "-app", workspace, "-layers", "/layers",
                  "-generated", generatedDirNBF, ...skipChown, ...insecure}
base = base.Run(generatorArgs..., WithCustomNamef("[%s] lifecycle: generator", plat)).Root()
```

Extension modules are delivered to `/cnb/extensions` per-arch using the SAME agnostic-vs-
multiarch classification the buildpacks use (FR-8b): reuse `moduleIsPlatformAgnostic` +
per-platform child pull; agnostic/inline/single-manifest extensions staged once to all legs.

### 4. Extend-build in LLB (new, in buildEmitLLB)
After generate, discover build Dockerfiles from the generated output; for each in order,
translate → LLB with `base = current build/builder state`, `contextSrc = context.build|context|app`,
build args (`base_image`, `build_id` UUID, `user_id`/`group_id` from the builder image config,
+ `extend-config.toml`). The resulting state becomes the base the `builder` phase runs on.

### 5. Extend-run in LLB (new)
Discover run Dockerfiles; translate → LLB applied to the run-image state
(`llb.Image(runImage, Platform(p))` or a switched `FROM`). The extended run image + its
`extImageConfig` (esp. `io.buildpacks.rebasable`) feed the per-platform emit + finalize so the
final manifest references the extended run image per arch (FR-4). This coordinates with the
existing emit/finalize run-image handling.

## Multi-solve execution model (why generate→read→extend needs more than one solve)

LLB is DECLARATIVE: the whole graph is constructed before it is solved, so we cannot read
the CONTENT of a generated `build.Dockerfile` / `run.Dockerfile` at graph-build time in order
to translate it. The generated Dockerfiles only exist after the generator RUN has executed.
So the buildkit backend uses a MULTI-SOLVE shape within the single `bkClient.Build` gateway
callback:

1. Build the LLB up through the generator phase; `Solve`/evaluate that state to a gateway
   `Reference`.
2. `ReadFile` the generated Dockerfiles (and `extend-config.toml`) from that reference; run
   the shared `DiscoverGeneratedLayout` over what was read.
3. Build MORE LLB from there — translate the Dockerfiles (FR-2) and continue the graph
   (extend-build → restorer → builder → extend-run → exporter emit).

**Efficiency.** Multiple solves are NOT multiple rebuilds. BuildKit's cache is keyed on the
LLB graph at the VERTEX level (each vertex's cache key derives from its inputs + operation),
not on solve boundaries. Because all solves in one build go through the SAME gateway
client/session against the same buildkitd, the shared prefix (base image pull, order.toml,
buildpack/extension staging, analyzer, detector, generator) is computed once; the second
solve gets cache hits for that prefix and only the new (extend/build/export) vertices run.
The only added cost is the gateway round-trip to evaluate + read the generated files, which
also correctly serializes generate-before-extend. This is the same generate→read→emit-more
pattern BuildKit's own Dockerfile frontend uses for multi-stage / `# syntax=` builds, so it
is idiomatic gateway usage, not a workaround.

Risks to watch: (a) all solves must share the gateway client so the cache is shared — do not
create a fresh client; (b) keep the prefix vertices deterministic (stable args) so cache keys
match across solves.

## Implementation staging (option 2 first, then option 1 = full parity)

The end goal is FULL PARITY with the daemon (both build-image and run-image extend, multiple
Dockerfiles in order). To de-risk the multi-solve shape, implementation proceeds in stages:
- **Stage 1 (prove it):** generator phase + ONE intermediate solve/read + a single
  `run.Dockerfile` PATCH applied to the run image, verified locally multi-arch. This proves
  generate→read→extend end-to-end with the smallest slice.
- **Stage 2 (full parity):** extend-build (build.Dockerfile → builder state before the builder
  phase), multiple Dockerfiles applied in generated order, run-image SWITCH, and the finalize
  wiring for the extended run image per arch.
Stage 1 is a foundation for Stage 2, not throwaway — Stage 2 builds on the same multi-solve
loop.

## Phase ordering (per platform, extensions present)
```
analyzer → detector → generator → [extend-build] → restorer → builder → [extend-run + emit] → exporter(emit)
```
No extensions → the current `analyzer → detector → restorer → builder → exporter(emit)` graph,
unchanged (AC-5).

## Files
- NEW `internal/build/multiplatform/dockerfile_llb.go` — translator (FR-2) + unit tests
  `dockerfile_llb_test.go`.
- NEW `internal/build/generated_layout.go` — shared discovery (NFR-2) + unit tests; daemon
  `hasExtensionsForBuild`/`hasExtensionsForRun` refactored to call it (behavior preserved).
- `internal/build/multiplatform/native_buildfunc.go` — generator + extend-build + extend-run in
  `buildEmitLLB`; `nativeBuildInputs` gains extension inputs (generated dir, ext image refs,
  hasExtensions flags).
- `internal/build/multiplatform/backend*.go` — thread extension inputs + per-arch extension
  image refs (mirrors the buildpack wiring).
- `pkg/client/build.go` — stop forcing the daemon ephemeral-builder fallback for buildkit +
  extensions once LLB drives generate/extend (FR-5; coordinate with buildkit-ephemeral-builder-in-llb).

## Testing
- **Unit (this spec):** translator allowed/disallowed instructions, second-`FROM` rejection,
  build-arg substitution (`base_image`/`build_id`/`user_id`/`group_id`/extend-config), context
  folder selection, `LABEL`/`ENV`/`USER`/`WORKDIR`/`SHELL` handling, `build_id`→IgnoreCache;
  `DiscoverGeneratedLayout` build vs run discovery across old/new platform layouts (model on
  `lifecycle_execution_test.go` fixtures + `fakes.NewFakeExtension`).
- **MVP local (NFR-3):** synthetic extension with a `run.Dockerfile` (patch: add a file) and a
  `build.Dockerfile` (install a tool), built multi-arch against the local registry; assert the
  change appears on both arches. A builder published to the LOCAL registry for this is NOT
  committed.
- **Integration (later follow-up):** CI acceptance test with a real extension, both arches.
