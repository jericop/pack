# Requirements: BuildKit-native CNB image extension support

## Overview

The `jericop/pack` BuildKit multi-arch backend (`--build-backend buildkit`) currently does
NOT support Cloud Native Buildpacks **image extensions**. The `buildkit-native-export` spec
declared them out of scope (Req 12.7 / 13.8): the buildkit path runs a fixed
`analyzer → detector → restorer → builder → exporter(emit)` sequence with no generator or
extender, and when extensions are present pack silently falls back to the daemon-synthesized
ephemeral builder — which the multi-arch LLB build never consumes. So extensions are, in
practice, ignored on the buildkit path.

This spec adds **full-parity** extension support to the buildkit backend — the generation
phase and BOTH extend phases (build image and run image) — implemented **natively in LLB,
without kaniko**.

### Why kaniko-free (the core decision)

The daemon lifecycle applies extension Dockerfiles with **kaniko** (a daemonless Dockerfile
executor) because in that path there is no BuildKit available to interpret a Dockerfile. The
buildkit backend is the opposite situation: we are ALREADY authoring the entire build as an
LLB graph, and BuildKit is itself a Dockerfile-execution engine. Running kaniko inside an LLB
graph would be running a Dockerfile interpreter inside a Dockerfile interpreter — redundant,
slower, and it drags in kaniko's constraints (notably the daemon path's "build cache must be
volume cache when building with extensions").

The CNB image-extension spec (`buildpacks/spec/image_extension.md`) restricts extension
Dockerfiles to a **small, fixed instruction set**: `FROM`, `ADD`, `ARG`, `COPY`, `ENV`,
`LABEL`, `RUN`, `SHELL`, `USER`, `WORKDIR` — and "SHOULD NOT contain any other instructions."
Every one of those maps 1:1 onto an LLB operation:

| Dockerfile instruction | LLB expression |
|------------------------|----------------|
| `FROM ${base_image}`   | `llb.Image(base, llb.Platform(p))` — the leg's build/run image state |
| `RUN`                  | `state.Run(llb.Args(...))` |
| `COPY` / `ADD`         | `state.File(llb.Copy(ctxState, src, dst, ...))` |
| `ENV`                  | `llb.AddEnv` on subsequent runs + image config |
| `ARG`                  | build-arg substitution at translate time (`base_image`, `build_id`, `user_id`, `group_id`, `extend-config.toml` keys) |
| `LABEL`                | image config metadata (incl. `io.buildpacks.rebasable`) |
| `USER` / `WORKDIR`     | `llb.User` / `llb.Dir` on subsequent state |
| `SHELL`                | the argv prefix used for shell-form `RUN` |

So a **hand-rolled translator for exactly this 10-instruction subset** can express the full
extension contract in LLB. This is the right architecture for a BuildKit-native backend, and
it is bounded and dependency-free (no kaniko, no moby Dockerfile-frontend dependency). As a
safety property, the translator REJECTS any instruction outside the allowed set rather than
silently ignoring it.

Benefits that fall out of doing it in LLB:
- **Per-architecture correctness for free** — extend runs as ordinary vertices in the same
  per-platform graph we already emit, so each arch extends its own build/run image (matching
  PLATFORM-1662 FR-8b), with no extra machinery.
- **No kaniko volume-cache constraint** — extend-build no longer forces a volume build cache.
- **One execution engine** — the whole build, extensions included, is one LLB solve.

### Scope

FULL PARITY with the daemon extension behavior, kaniko-free:
1. **Generation phase** — run each participating extension's `bin/generate` to produce
   `build.Dockerfile` / `run.Dockerfile` (+ optional `context` folders + `extend-config.toml`)
   under the generated output dir, per the CNB generation contract.
2. **Extend build image** — translate `build.Dockerfile` to LLB and apply it to the build
   (builder) image state BEFORE the `builder` phase, so buildpacks see the added OS packages.
3. **Extend run image** — translate `run.Dockerfile` to LLB and apply it to the run image
   state that the exporter/finalize consumes.

All three run per-platform inside `buildEmitLLB`. The existing single-arch **daemon** path is
left unchanged; this is additive, buildkit-specific logic (with a shared discovery helper —
see NFR-2). Reuse daemon code by extracting shared sub-functions where it is clean to do so;
where the daemon delegates to kaniko (the actual Dockerfile application), the buildkit path
supplies its own LLB translator because there is no shared implementation to reuse.

## Functional Requirements

### FR-1: Generation phase in LLB
- When the resolved order contains one or more image extensions AND the platform API is
  >= 0.13, the buildkit backend MUST run the generation phase per platform: invoke the
  lifecycle generator (`/cnb/lifecycle/generator`, or the detector's generate output on the
  platform APIs where that applies) with the extension modules staged under
  `/cnb/extensions/{id}/{version}/*`, producing the generated output tree
  (`<generated>/<ext-id>/build.Dockerfile` and/or `run.Dockerfile`, `context*` folders,
  `extend-config.toml`).
- Extension modules MUST be delivered per-arch using the SAME classification the buildpacks
  use (FR-8b): a multi-arch extension image contributes its per-platform child; an
  agnostic/inline/single-manifest extension is staged once to every leg. (Extensions use the
  same `Descriptor().Targets()` agnostic semantics.)
- If there are no extensions in the order, the generation phase MUST NOT run and the graph is
  byte-for-byte the current 5-phase sequence (no behavior change, cache-stable).

### FR-2: Restricted Dockerfile → LLB translator
- A translator MUST parse a CNB extension `build.Dockerfile` / `run.Dockerfile` and emit the
  equivalent LLB, supporting exactly: `FROM`, `ADD`, `ARG`, `COPY`, `ENV`, `LABEL`, `RUN`,
  `SHELL`, `USER`, `WORKDIR`.
- It MUST reject (error, not ignore) any instruction outside that set, and any second `FROM`.
- It MUST honor the required build args the lifecycle supplies: `base_image` (the state being
  extended), `build_id` (a UUID; when referenced in a `RUN`, later layers rebuild — model by
  busting cache on that vertex), `user_id` / `group_id` (original image `User`), plus any
  key/value pairs from `<generated>/extend-config.toml` provided as build args to
  `build.Dockerfile`.
- It MUST resolve the build context per the spec: `context.build` for build-image extend,
  `context.run` for run-image extend, else `context`, else the app dir — as the LLB source
  for `COPY`/`ADD`.
- It MUST apply `LABEL` (including `io.buildpacks.rebasable`), `ENV`, `USER`, `WORKDIR`,
  `SHELL` to the resulting image config/state so the extended image config is correct.

### FR-3: Extend build image (kaniko-free)
- When an extension produced a `build.Dockerfile`, the backend MUST apply it (via FR-2) to
  the build/builder image state BEFORE the `builder` lifecycle phase, so the `builder` runs on
  the extended build image. Multiple Dockerfiles apply in generated order, each `FROM` the
  intermediate produced by the previous one.
- MUST NOT require a volume build cache (the daemon/kaniko constraint does not apply).

### FR-4: Extend run image (kaniko-free)
- When an extension produced a `run.Dockerfile`, the backend MUST apply it (via FR-2) to the
  run image state that the exporter/finalize consumes, so the exported image's run-image
  layers include the extension's changes. Includes run-image SWITCH (a `run.Dockerfile` whose
  `FROM` names a different run image) and run-image PATCH (`FROM ${base_image}` + `RUN`/`COPY`).
- The extended run image MUST be carried through the per-platform emit + finalize so the final
  multi-arch manifest references the extended run image per arch.

### FR-5: Preserve existing behavior
- The single-arch daemon extension path (kaniko-based) MUST be unchanged.
- With extensions on the buildkit path, the ephemeral-builder daemon fallback
  (`skipEphemeralBuilderSave = isBuildkitBackend && !hasExtensions`) MUST be revisited: once
  the buildkit path drives generate+extend in LLB, extensions no longer force the daemon
  ephemeral-builder synthesis. (Coordinate with the `buildkit-ephemeral-builder-in-llb` spec.)

## Acceptance Criteria
- AC-1 (generate): a build whose order includes an extension with a `bin/generate` runs the
  generation phase on every platform and produces the generated Dockerfile(s).
- AC-2 (extend-run, patch): an extension emitting a `run.Dockerfile` with `FROM ${base_image}`
  + a `RUN`/`COPY` yields a final image whose run-image layer shows the change, on BOTH
  `linux/amd64` and `linux/arm64`.
- AC-3 (extend-build): an extension emitting a `build.Dockerfile` that installs a tool makes
  that tool available to buildpacks during the `builder` phase, on both platforms.
- AC-4 (translator safety): a Dockerfile containing a disallowed instruction (e.g. `VOLUME`,
  `ENTRYPOINT`, a second `FROM`) is rejected with a clear error naming the instruction.
- AC-5 (no-extension no-op): a build with no extensions emits the exact current 5-phase graph.
- AC-6 (unit tests): the translator and the generated-layout discovery are unit-tested
  (allowed/again disallowed instructions, build-arg substitution, context-folder selection,
  build vs run Dockerfile discovery). Integration tests are a later follow-up (NFR-3).

## Non-Functional Requirements

### NFR-1: kaniko-free, dependency-free translator
The translator MUST be hand-rolled for the CNB-allowed subset and MUST NOT introduce kaniko or
a moby/dockerfile-frontend dependency (GO-ARCH-2.8 dependency risk; prefer stdlib). It lives in
the buildkit multiplatform package.

### NFR-2: reuse via shared sub-functions where clean
Where the daemon path has logic that is genuinely shared — notably discovery of the generated
layout (which extensions produced build/run Dockerfiles; locating `extend-config.toml` and
`context*` folders) — extract it into a shared sub-function that BOTH paths call, rather than
duplicating. The Dockerfile APPLICATION differs by construction (daemon = kaniko, buildkit =
LLB translator), so that part is not shared. Record honestly in the design what was extracted
vs what is necessarily buildkit-only.

### NFR-3: MVP now, integration tests later
Initial delivery is an MVP verified locally end-to-end (a synthetic extension built multi-arch
against the LOCAL registry), plus unit tests. A builder published to the LOCAL registry for MVP
testing is NOT committed and is replaced by the unit tests. Full integration tests in CI are a
tracked later follow-up.

### NFR-4: command execution conventions
All shell execution for this spec follows the `command-execution` steering: every command via
`bash ~/ai-development/run.sh ~/ai-development/commands/{MM-DD-YYYY}-<desc>.sh`, env vars as
`export` lines inside the file, no inline `VAR=`.

## Out of Scope
- Windows builds (the CNB generation/extension phases MUST NOT run for Windows).
- Changing the daemon (non-buildkit) extension path.
- The `extend-config.toml` `build.args` beyond simple key/value → build-arg passing.
