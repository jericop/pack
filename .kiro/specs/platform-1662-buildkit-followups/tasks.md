# Tasks: PLATFORM-1662 BuildKit fork follow-ups

How to look up the build data: steering `platform-1662-benchmark-data.md`. How to publish
the fork image: steering `fork-release-process.md`. This spec is the single source of truth
(the former `FOLLOWUPS.md` is retired).

> **▶ There is exactly ONE current task: Task 7 (flatten the ephemeral builder).** Do it,
> publish a new fork pack image on `buildkit-native-export-with-history-and-kiro`, then hand
> back for testing. All other tasks below are reference (done / WON'T FIX / deferred /
> Jenkins-library-owned) — do NOT action them unless explicitly asked.

## ▶ CURRENT REQUIRED TASK

## Task 7: Flatten the ephemeral builder so extra buildpacks don't exceed the layer cap (Item 7, BLOCKER)
- [x] Reproduce: `pack build --build-backend buildkit --builder <deep noble builder>
      --trust-builder --buildpack docker.io/paketobuildpacks/nodejs:latest --platform
      linux/amd64 --platform linux/arm64 ...` → confirm `pack.local/builder/<hex>` load fails
      with `max depth exceeded` before any lifecycle phase (AC-1 baseline)
- [x] Implement the flatten: route the ADDED builder modules through the flattened path
      (`addFlattenedModules` over the combined set) instead of `addExplodedModules`
      (one-layer-per-module) in `internal/builder/builder.go` `Builder.Save`, OR emit a single
      merged diff over `/cnb/buildpacks` (`llb.Merge`/squash) on the buildkit-backend path
- [x] Enforce the invariant: N extra modules add O(1) builder image layers, not O(N); the
      synthesized builder loads cleanly on a deep base builder
- [x] AC-1: the repro command completes without `max depth exceeded` and produces the
      multi-arch image
- [ ] AC-2: regression test asserts the synthesized builder adds a flattened (single) layer
      for N extra modules, not per-module layers
- [x] AC-3 (publish): push the fix to the remote branch
      `buildkit-native-export-with-history-and-kiro`, then run the branch-driven
      `publish-pack.yml` (`--ref fork-main -f ref=buildkit-native-export-with-history-and-kiro`)
      → publishes `jericop/pack:buildkit-native-export-with-history-and-kiro` (no git tag; see
      `dot-kiro-files/publish-images-runbook.md` section 3A). Record the pushed image tag/digest HERE:
      - fix commit: `f8d9eca4` (pushed to origin/buildkit-native-export-with-history-and-kiro)
      - published image: `docker.io/jericop/pack:buildkit-native-export-with-history-and-kiro`
      - digest: `(pending publish-pack.yml run 33667638490 — https://github.com/jericop/pack/actions/runs/33667638490)`
- [ ] AC-4 (handoff/test — in jenkins-core-shared-libraries, not here): point
      `env.PACK_FORK_IMAGE` at the new image and re-run the nodejs emulation build to confirm
      the fix end to end
- Do NOT: squash the final app image (export is never reached), chmod, or tweak daemon
  storage — the fix is the builder-image layer count.
- References: FR-7 (see requirements.md "Current required task"), design.md "CURRENT
  REQUIRED TASK"

## Task 8b-impl: Deliver buildkit extra buildpacks per-arch / staged-once (FR-8b-impl, BLOCKER — FIXED)
The actionable fix for FR-8b (agent-patcher cpython `python3` ENOENT). Extra buildpacks were
staged once from the host arch and copied to both legs; now classified + delivered arch-correct.
- [x] Path A: pin the per-platform CHILD digest of the builder/lifecycle image so each leg
      runs its own arch binaries (`native_buildfunc.go`; `build.go` sets lifecycle image =
      builder tag on the buildkit path)
- [x] Path B classify declared extra buildpacks (`--buildpack`, else `project.toml
      build.buildpacks`) via `collectBuildkitPerArchBuildpackImages` +
      `verifyBuildpackImageSupportsPlatforms`
- [x] Multi-arch registry image (OCI index): verify it covers every requested `--platform`
      (error naming missing ones otherwise), pull per-platform child image in LLB per leg
- [x] Platform-agnostic (inline script, local dir/tarball, `urn:cnb:registry`,
      single-manifest image): `stageAgnosticExtraBuildpacks` selects agnostic modules via
      `moduleIsPlatformAgnostic` (empty/wildcard `Descriptor().Targets()`) + reuses
      `stageExtraBuildpacks`; copy the SAME tree to every leg. Single-manifest = agnostic, NOT
      an error
- [x] `buildEmitLLB` overlay order: builder `/cnb/buildpacks` → agnostic staged tree →
      per-arch image children (later overrides); backend mounts agnostic dir as
      `extraBuildpacksLocalName`, passes per-arch image list
- [x] Preserve existing code: single-arch daemon fetch/stage path unchanged; new logic is
      buildkit-specific + additive
- [x] AC-1 (per-arch correctness): agent-patcher-service + multi-arch cpython → arm64 leg
      `GOARCH=arm64`, `pip --version` succeeded both legs, `Finalized CNB metadata`
- [x] AC-2 (agnostic + inline coexist): agent-patcher-service `--descriptor
      inline-test-project.toml` (no `--buildpack`) → `add platform-agnostic buildpacks` vertex
      both legs, inline `registry-assember` `go install` ran on amd64 AND arm64, build finalized
- [x] Unit tests: `moduleIsPlatformAgnostic` classification (empty/wildcard/concrete targets);
      single-manifest ⇒ agnostic; index-missing-platform ⇒ error
- References: FR-8b-impl (requirements.md), design.md "Item 8b-impl"

---

## Reference tasks (NOT current — context only)

## Done (fixed on this branch)

## Task 1: Resolve real current buildx builder when `--buildkit-builder` empty (Item 1)
- [x] `resolveBuildkitAddr` resolves current buildx builder from on-disk state (no
      hard-coded `pack-multiplatform`, no docker shell-out); honors `BUILDX_BUILDER`
- [x] Actionable error when the current builder uses the `docker` driver
- [x] Tests: `buildx_state_internal_test.go`
- References: FR-1

## Task 2: Fix app-context sync for nested layouts (Item 2)
- [x] Drop `fsutil.NewFilterFS` Map wrapper; stage filtered files to a temp dir and use
      plain `fsutil.NewFS` (`stageFilteredAppDir` in `appcontext.go`)
- [x] Tests: `appcontext_internal_test.go` (nested tree + exclude + symlinks vs `fsutil.Validator`)
- References: FR-2

## Task 4: Collapse `platform env:` writes into one vertex (Item 4)
- [x] Single `llb.Mkfile` FileAction `[<plat>] write platform env (<N> vars)`, keys sorted
- References: FR-4

## By decision / deferred

## Task 3: `--trust-builder` required — accept + document (Item 3)
- [x] Decision recorded: WON'T FIX (self-built builder is untrusted; passing
      `--trust-builder` is expected)
- [ ] (doc) Ensure pack help / fork docs state the fork builder must be trusted
- References: FR-3

## Task 5: Quiet the progress printer (Item 5, OPTIONAL — do later)
- [ ] De-dupe completion: add `vertexCompleted` guard in `startProgressDisplay` so
      `DONE`/`CACHED`/`ERROR` prints once per vertex
- [ ] Thread `--verbose` / log level into `BuildkitBackend` → `startProgressDisplay`;
      concise summary by default, full per-vertex/per-log detail only with `--verbose`
- [ ] Target UX: one START + one END(+duration) line per vertex by default
- References: FR-5

## Open — needs implementation

## Task 6: Fix file ownership — TWO related but separate issues (Item 6, BLOCKER — Jenkins library, not the fork)
Related (both = root-run container writes files the jenkins user can't read; both surfaced
as `permission denied` on the emulated arch) but fix + VERIFY each individually.

### Task 6a: mvnPipeline root-owned `target/`
- [ ] Fix in `mvnPipeline`: `chown -R jenkins` the maven outputs (e.g.
      `container('maven') { sh 'chown -R jenkins target' }`) BEFORE `packBuildContainer`
- [ ] VERIFY: re-run pd-sample-java-app (mvnPipeline) emulation build → no permission error

### Task 6b: library-created binding files ownership
- [ ] Fix: create maven-settings + git-credential binding files owned-by/readable-by the
      jenkins user by construction (not a blanket chmod), in the buildkit-emulation
      `packBuildContainer`
- [ ] VERIFY: re-run pd-sample-go-app (containerReleasePipeline) emulation with the
      `chmod -R a+rX` workaround REMOVED → bindings readable

### Common
- [ ] REMOVE the interim `chmod -R a+rX` workaround on the binding dirs once 6a + 6b land
- [ ] Fork: no change required for Item 6 (re-evaluate only if a real binding-permission
      issue remains after ownership is corrected)
- References: FR-6 (6a/6b)

## Task 8: QEMU emulation issues — 8a document, 8b investigate (Item 8, ENVIRONMENT / OPEN)

### Task 8a: Document cgo/gcc segfault under emulation (environment; NOT a fork bug)
- [ ] Document as a known emulation limitation (fork docs / RFC): emulated `gcc`/cgo can
      segfault on the non-native arch (`exit code: 51`)
- [ ] Note mitigations: `CGO_ENABLED=0` for pure-Go; native multi-agent for cgo/native
      workloads; optionally newer QEMU/binfmt in the builder image

### Task 8b: Investigate cpython `python3` ENOENT on emulated arm64 (OPEN — root cause UNCONFIRMED)
- [ ] DO NOT categorize as native-compile or as a fork bug yet. Correction on record:
      CPython 3.11.15 arm64 noble is PREBUILT in the cpython `buildpack.toml` (extracted, not
      compiled), so the earlier "source-compile under QEMU" explanation is wrong.
- [ ] Get evidence: `BP_LOG_LEVEL=DEBUG` emulation re-run of agent-patcher-service; capture
      the cpython buildpack output (extract vs any compile) and `file` + `readlink -f` on
      `/layers/paketo-buildpacks_cpython/cpython/bin/python3` and `.../bin/python3.11` (arm64).
      (jenkins-asgard console is MCP-unreadable; Tempo has no pack stdout — capture from the build.)
- [ ] Answer the divergence: compare the resolved CPython version + stack (and buildpack
      order) between agent-patcher-service and pd-sample-python-app (same builder + buildpack,
      one fails one succeeds → likely an input difference)
- [ ] Decide the bucket from evidence: H1 emulated cpython buildpack Go binary mis-extracts /
      mis-symlinks `bin/python3 -> python3.11`; H2 fork buildkit cross-arch layer/extraction
      issue (would make it a fork task); H3 per-app version/stack difference
- [ ] Record: FR-7/flatten is NOT implicated (build reaches the buildpack `builder` phase)
- References: FR-8 (8a/8b)

## Task 9: Re-run the PLATFORM-1662 comparison after 6 & 7 land
- [ ] With Items 6 & 7 fixed, re-run the emulation builds for nodejs + python apps and
      capture durations from Grafana (see steering `platform-1662-benchmark-data.md`)
- [ ] Update the comparison table (add the newly-unblocked languages) in the Rapid7
      PLATFORM-1662 spec/ticket
- References: NFR-2

## Task 10: Investigate the post-emit stall after `exporter (emit-mode) DONE` (Item 9, OPEN — perf)
Motivation (Grafana summary, emulation vs multi-agent last SUCCESS): nodejs ~4903s vs ~445s
(~11x, extreme outlier); java ~536s vs ~345s (~1.55x). Java is key evidence it is NOT
app-compile: Maven/JVM apps compile in `mvnPipeline` before the container build, so a
post-export hang can only be the emit/finalize/export flow.
- [ ] DO NOT categorize as a fork bug or environment issue yet — root cause UNCONFIRMED.
- [ ] Quantify: capture a TIMESTAMPED emulation build log from `exporter (emit-mode) DONE`
      through the final build line; measure the wall-clock gap and note anything that prints
      during it (seen across go/nodejs/java, around the run-image `resolve image config`).
      Start with nodejs (worst case, ~11x) and java (no compile in-phase, so cleanest signal).
- [ ] Isolate emulation vs general: check whether the SAME post-emit gap appears on a
      NATIVE-arch buildkit build (rules emulation in or out).
- [ ] Characterize the gap: determine whether it is a DATA COPY (run-image/app layer export
      or cross-arch layer materialization), a finalize/serialization step, or the build
      WAITING ON INPUT (rule out an interactive/stdin block).
- [ ] Only if evidence points to code: inspect the fork's post-emit/finalize path and
      progress plumbing (`native_buildfunc.go`, `buildkit_client.go` `startProgressDisplay`)
      for a long operation that runs without emitting a vertex.
- [ ] Note: distinct from Task 5 (that is cosmetic repeated `DONE` lines; this is real
      elapsed time with no obvious activity).
- References: FR-9
