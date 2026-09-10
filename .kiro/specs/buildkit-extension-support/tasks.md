# Tasks: BuildKit-native CNB image extension support

Full-parity extension support (generate + extend-build + extend-run) on the buildkit backend,
kaniko-free via a hand-rolled restricted Dockerfile→LLB translator. Reuse daemon logic via a
shared generated-layout discovery helper; the Dockerfile application is buildkit-only (LLB).
Command execution follows the `command-execution` steering (stable `bash run.sh <file>`).

## Task 1: Restricted Dockerfile → LLB translator (FR-2)
- [x] New `internal/build/multiplatform/dockerfile_llb.go`: `parseExtensionDockerfile` accepts
      only `FROM/ADD/ARG/COPY/ENV/LABEL/RUN/SHELL/USER/WORKDIR`; rejects other verbs + 2nd `FROM`
- [x] `toLLB(extendInput)`: FROM→base, RUN→Run, COPY/ADD→File(Copy from contextSrc), ENV/ARG→env,
      LABEL→config (incl io.buildpacks.rebasable), USER/WORKDIR/SHELL→state; `build_id` in RUN→IgnoreCache
- [x] Build-arg substitution: base_image, build_id (UUID), user_id, group_id, extend-config.toml kv
- [x] Unit tests `dockerfile_llb_test.go` (AC-4 + substitution + context selection). build+vet+test

## Task 2: Shared generated-layout discovery (NFR-2, FR-1 support)
- [x] New `internal/build/generated_layout.go`: `DiscoverGeneratedLayout(generatedDir)` returns
      build/run Dockerfiles (ordered) + context dir + extend-config args, across old/new platform layouts
- [x] Refactor daemon `hasExtensionsForBuild`/`hasExtensionsForRun` to call it (behavior preserved;
      run existing lifecycle_execution_test.go to confirm no regression)
- [x] Unit tests for discovery (build-only, run-only, both, none; old `generated/build/<bp>` +
      new `generated/<bp>/build.Dockerfile`)

## Task 3: Generator phase in LLB (FR-1)
- [x] `buildEmitLLB`: when order has extensions + platformAPI>=0.13, add
      `/cnb/lifecycle/generator ... -generated <dir>` RUN between detector and restorer
- [x] Deliver extension modules to /cnb/extensions per-arch using the FR-8b classification
      (reuse moduleIsPlatformAgnostic + per-platform child pull; agnostic staged once)
- [x] `nativeBuildInputs` + backend wiring gain extension inputs (generated dir, ext image refs,
      hasExtensions). No-extension path unchanged (AC-5). build+vet

## Task 4: Extend-build in LLB (FR-3)
- [x] After generate, discover build Dockerfiles; translate→LLB applied to the builder state
      BEFORE the builder phase; multiple apply in order (each FROM the previous intermediate)
- [x] Supply base_image/build_id/user_id/group_id (user/group from builder image config) +
      extend-config args; context.build|context|app as contextSrc. No volume-cache requirement. build+vet

## Task 5: Extend-run in LLB (FR-4)
- [x] Discover run Dockerfiles; translate→LLB applied to the run-image state (patch or FROM-switch)
- [x] Carry the extended run image + extImageConfig (esp. io.buildpacks.rebasable) through the
      per-platform emit + finalize so the final manifest references the extended run image per arch
- [x] build+vet

## Task 6: Revisit ephemeral-builder fallback (FR-5)
- [x] Once LLB drives generate+extend, stop forcing the daemon ephemeral-builder synthesis for
      buildkit + extensions (revisit `skipEphemeralBuilderSave = isBuildkitBackend && !hasExtensions`
      in build.go). Coordinate with buildkit-ephemeral-builder-in-llb spec. Preserve daemon path (FR-5)

## Task 7: Verify locally (MVP, NFR-3) — AC-1..AC-3
- [x] Synthetic extension: run.Dockerfile (patch: add a file / RUN) + build.Dockerfile (install a tool)
- [x] Build multi-arch (linux/amd64 + linux/arm64) against the LOCAL registry with the fork pack;
      publish a builder to the LOCAL registry if needed (NOT committed)
- [x] Confirm: generator runs both legs; run-image change present both arches; build-tool available
      to buildpacks; build reaches Finalized CNB metadata for manifest list. Command files under
      ~/ai-development/commands/

## Task 8: Unit tests round-up + spec updates (AC-6)
- [x] Ensure translator + discovery unit tests are complete and green (build+vet+test)
- [x] Note integration tests as a later follow-up (NFR-3)

## Task 9: Update the platform-1662-buildkit-followups spec
- [x] Mark the extension follow-up (FR to be added there) as implemented + locally validated;
      status table row + tasks

## Task 10: FINAL — flip the buildkit-native-export spec
- [x] Only after platform-1662 spec is updated: change Req 12.7 + Req 13.8 from
      "extensions unsupported / out of scope" to IMPLEMENTED, describing the kaniko-free LLB
      generate+extend mechanism and the local validation
