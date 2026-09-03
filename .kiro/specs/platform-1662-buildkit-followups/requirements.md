# Requirements: PLATFORM-1662 BuildKit fork follow-ups

## Overview

This spec captures the issues found in the `jericop/pack` BuildKit multi-arch fork
(`--build-backend buildkit`) while running the Rapid7 **PLATFORM-1662** benchmark — a
head-to-head of two multi-arch container-build strategies in Jenkins:

1. **multi-agent (native):** each arch built on its own native agent, then a manifest
   list is assembled.
2. **buildkit-emulation:** one agent runs a single `pack build --build-backend buildkit
   --platform linux/amd64 --platform linux/arm64`, using QEMU for the non-native arch.

The emulation side drives the fork `pack` binary (copied out of a published fork image)
against five Rapid7 sample-app repos across `linux/amd64,linux/arm64` (a sixth,
`pd-rds-postgres-password-lambda`, was excluded — its PyGreSQL/postgres-client dependency
is not in the patched noble builder). Doing so surfaced
8 findings — some already fixed in this branch, some deferred by decision, and some still
open. This spec consolidates ALL of them (including the ones marked WON'T FIX / DEFERRED)
so the pack development work has a single source of truth for context — but only ONE of
them is an actionable pack code change right now (see below).

> **CURRENT REQUIRED TASK — the ONLY pack code change to make now: FR-7, flatten the
> ephemeral builder (Item 7).** Everything else in this spec is reference/context:
> Items 1/2/4 are already FIXED on this branch, Item 3 is WON'T FIX, Item 5 is DEFERRED
> (optional), and Items 6/8 are NOT fork changes (6 is a Jenkins-library ownership fix, 8
> is an environment/documentation note). Do FR-7, publish a new fork pack image on the
> `buildkit-native-export-with-history-and-kiro` branch, then hand back for testing in the
> PLATFORM-1662 pipeline. Do not start any other pack code change under this spec unless
> explicitly asked.

This spec adds the requirements/design/tasks framing and the current status of each item.
The `FOLLOWUPS.md` file that previously held the long-form per-item reference is being
retired in favor of this spec — this spec (requirements/design/tasks) is now the single
source of truth; do not add new detail to `FOLLOWUPS.md`.

See the steering file `platform-1662-benchmark-data.md` for how to look up the Jenkins
builds, Grafana traces/logs, the Jenkins shared-library branches, and the sample-app repos
used to reproduce any of these. See `fork-release-process.md` for how to build and publish
the fork pack image on this branch.

## Status summary (2026-09)

| # | Item | Severity | Status |
|---|------|----------|--------|
| 7 | Extra buildpacks + trusted builder → ephemeral builder `max depth exceeded` | BLOCKER | **▶ CURRENT REQUIRED TASK — flatten the builder, publish image, then test** |
| 1 | Empty `--buildkit-builder` resolves to non-existent `pack-multiplatform` | BLOCKER | FIXED (reference) |
| 2 | App-context sync `changes out of order` (nested dirs + filter) | BLOCKER | FIXED (reference) |
| 3 | Untrusted fork builder needs `--trust-builder` (lifecycle 0.0.0) | usability | WON'T FIX by decision (reference) |
| 4 | `platform env:` one vertex per env var floods output | papercut | FIXED (reference) |
| 5 | Repeated `DONE` lines per vertex; gate progress behind `--verbose` | papercut | DEFERRED / optional (reference) |
| 6 | File OWNERSHIP, root-run container vs jenkins user (surfaced as `permission denied` on emulated arch). TWO related-but-separate: **6a** mvnPipeline `target/`, **6b** library-created binding files | BLOCKER | NOT a fork change — Jenkins-library fix, tracked elsewhere (reference) |
| 8 | QEMU emulation: (8a) cgo/gcc segfault — documented, environment; (8b) cpython `python3` ENOENT → `exit code: 51` on agent-patcher-service #5 — OPEN, root cause UNCONFIRMED (CPython is prebuilt/extracted here, NOT compiled; suspect emulated buildpack Go binary or fork layer assembly — needs DEBUG evidence) | environment / OPEN | 8a document; 8b investigate (reference) |
| 9 | Post-emit stall: long pause AFTER `lifecycle: exporter (emit-mode) DONE`, around run-image config resolve, before the build completes (seen on ≥2 emulation builds) | perf / OPEN | investigate — root cause UNCONFIRMED (reference) |
| 8b-impl | Extra buildpacks (`--buildpack`/`project.toml`) delivered as ONE host-arch tree to both legs → emulated leg runs wrong-arch buildpack binaries (agent-patcher cpython `python3` SIGTRAP/ENOENT). | BLOCKER | **FIXED (verified)** — see FR-8b-impl |

## Functional Requirements

## ▶ Current required task

### FR-7 (Item 7): flatten the ephemeral builder so extra buildpacks don't blow the layer cap
**This is the only pack code change to make now.**

**Symptom (verified).** With `--build-backend buildkit` and an extra buildpack module on a
TRUSTED builder — e.g. the nodejs app passes
`--buildpack docker.io/paketobuildpacks/nodejs:latest` — the build fails immediately after
image pulls, before any lifecycle phase, with:

```
Warning: Builder is trusted but additional modules were added; using the untrusted (5 phases) build flow
ERROR: failed to build: failed to write image to the following tags:
  [pack.local/builder/<hex>:latest: loading image "pack.local/builder/<hex>:latest".
   first error: embedded daemon response: max depth exceeded]
```

**Root cause (traced to source).**
1. `pkg/client/build.go` (~L512-520): when `hasAdditionalBuildpacks && !opts.TrustExtraBuildpacks`
   (or `hasExtensions`), pack logs that warning and sets `useCreator = false`, dropping into
   the 5-phase flow that synthesizes an ephemeral builder image `pack.local/builder/<hex>`.
2. That image is built by `Builder.Save` (`internal/builder/builder.go` ~L466), which calls
   `image.AddLayer(...)` many times: default dirs, lifecycle, **ONE LAYER PER ADDED MODULE**
   via `addExplodedModules`, order, system, stack, run image, build-config-env.
3. Stacked on the already-deep noble builder base, this exceeds Docker's ~125-layer hard cap;
   the daemon rejects the image load with `max depth exceeded`. It is the **builder** image
   that is too deep — NOT the app image, and NOT a buildpack failing.

**Required behavior — Option B, flatten the builder (chosen approach; the ONLY approach).**
- The added builder modules MUST be collapsed so the synthesized/ephemeral builder image does
  not grow one image layer per module. The fork ALREADY has the machinery for this:
  `Builder.Save` distinguishes `addFlattenedModules(...)` from `addExplodedModules(...)`. The
  added buildpacks currently take the exploded path (one layer each); they MUST instead be
  routed through the flattened path so all added modules land in a SINGLE image layer.
- Equivalent acceptable implementation under the buildkit backend: assemble the added
  modules over `/cnb/buildpacks` and emit a single merged diff (e.g. `llb.Merge` / one
  squashed layer) rather than a chain of per-module copies. Either way the invariant is:
  **adding N extra modules adds O(1) image layers to the builder, not O(N)**, and the loaded
  builder image stays comfortably under the daemon layer cap.
- The fix MUST apply regardless of `--build-backend` value for the layer-depth invariant, but
  at minimum MUST make the buildkit-backend path (the PLATFORM-1662 pipeline) succeed with an
  extra buildpack on a deep trusted builder.
- MUST NOT be worked around by squashing the final APP image (export is never reached) or by
  chmod/daemon-storage hacks.

**Acceptance criteria.**
- AC-1 (repro fixed): `pack build --build-backend buildkit --builder <deep noble builder>
  --trust-builder --buildpack docker.io/paketobuildpacks/nodejs:latest --platform
  linux/amd64 --platform linux/arm64 ...` completes WITHOUT `max depth exceeded` and produces
  the multi-arch image.
- AC-2 (layer invariant): the synthesized builder image adds O(1) layers for N extra modules
  (add a regression test asserting flattened, not per-module, layers).
- AC-3 (publish): a new fork pack image is published from the
  `buildkit-native-export-with-history-and-kiro` branch by pushing the fix to that remote
  branch and running the branch-driven `publish-pack.yml` workflow
  (`--ref fork-main -f ref=buildkit-native-export-with-history-and-kiro`), which builds
  that branch and publishes the moving image tag
  `jericop/pack:buildkit-native-export-with-history-and-kiro` (NO git tag required). See
  `dot-kiro-files/publish-images-runbook.md` section 3A. Record the pushed image tag/digest
  in the tasks file.
- AC-4 (handoff/test): after publish, the PLATFORM-1662 pipeline is pointed at the new image
  (`env.PACK_FORK_IMAGE`) and the nodejs emulation build is re-run to confirm the fix end to
  end. (This step happens in the jenkins-core-shared-libraries repo, not here.)

> **Performance follow-up (separate spec):** FR-7 makes the daemon-synthesized ephemeral
> builder *loadable* on a deep base (flatten to O(1) layers). But on the buildkit backend the
> synthesized `pack.local/builder/<hex>` image is never consumed — the buildkit path builds
> `FROM` the BASE builder and already injects the added buildpacks + resolved `/cnb/order.toml`
> over LLB. So that daemon synthesis + load (~16–85s observed; the `max depth exceeded` origin)
> is wasted work on the buildkit path. Skipping it there (keep base builder + the existing
> inject/order path; source builder UID/GID/labels/platform-API from the base builder) is
> tracked in the separate spec `.kiro/specs/buildkit-ephemeral-builder-in-llb/`.

## Reference requirements (NOT current tasks — context only)

The remaining FRs are recorded for context and traceability of ALL PLATFORM-1662 findings.
They are either already implemented, decided WON'T FIX, deferred/optional, or owned by the
Jenkins library. Do not action them under this spec unless explicitly asked.

### FR-1 (Item 1, FIXED): resolve the real current buildx builder when `--buildkit-builder` is empty
- When `--buildkit-builder` is empty, pack MUST resolve the ACTUAL current buildx builder
  from buildx on-disk state (not hard-code `pack-multiplatform`), honoring `BUILDX_BUILDER`.
- If the current builder uses the `docker` driver (cannot serve multi-platform buildkit),
  pack MUST emit an actionable error telling the user to create/select a `docker-container`
  (or `remote`) builder — NOT the cryptic `buildx_buildkit_pack-multiplatform0 not found`.
- MUST derive this by reading buildx state in Go (no shelling out to `docker`).
- Regression coverage MUST exist. (Done: `buildx_state.go` +
  `buildx_state_internal_test.go`.)

### FR-2 (Item 2, FIXED): app-context sync MUST handle arbitrary/nested layouts
- The app context MUST sync for arbitrary nested source layouts (e.g. `src/<pkg>/...`)
  without the fsutil `changes out of order` failure.
- When a project-descriptor include/exclude filter is in effect, pack MUST present BuildKit
  a filesystem that walks in the sorted (parents-before-children) order fsutil's receiver
  requires (e.g. stage kept files to a temp dir and hand BuildKit a plain `fsutil.NewFS`),
  rather than a `FilterFS` Map wrapper that can reorder entries.
- Regression coverage MUST exist for a nested tree + exclude set + symlink preservation.
  (Done: `appcontext.go` `stageFilteredAppDir` + `appcontext_internal_test.go`.)

### FR-3 (Item 3, WON'T FIX): `--trust-builder` is required for the fork builder — accepted
- No code change. The fork builder is self-built and technically untrusted; requiring
  `--trust-builder` is accepted behavior. Documented so callers know to pass it. (The
  PLATFORM-1662 pipeline passes it via `env.ADDITIONAL_PACK_ARGS`.)
- Recorded here (not dropped) so the decision is discoverable and not re-litigated.

### FR-4 (Item 4, FIXED): collapse `platform env:` writes into one progress vertex
- The per-`/platform/env/<NAME>` writes MUST be a SINGLE `llb.Mkfile` FileAction named
  `[<plat>] write platform env (<N> vars)`, not one named vertex per variable.
- Keys MUST stay sorted so the op is deterministic / cache-stable.
  (Done in `buildEmitLLB`.)

### FR-5 (Item 5, DEFERRED — optional): quiet the progress printer
- OPTIONAL polish. Two sub-parts:
  1. De-dupe the completion line: only print `DONE`/`CACHED`/`ERROR` the FIRST time a
     vertex completes (guard with a `vertexCompleted` set in `startProgressDisplay`).
  2. Gate the verbose per-vertex/per-log-line output behind `pack --verbose`; show a
     concise summary by default.
- Preferred end state: exactly one START line + one END-with-duration line per vertex by
  default; `--verbose` adds the per-refresh detail. Not required for the comparison.

### FR-6 (Item 6, OPEN): fix file OWNERSHIP (two related but SEPARATE issues) — Jenkins library, not the fork
Both are file-OWNERSHIP problems (NOT a pack/fork bug, NOT a `--binding` file-mode bug):
files written by a container running as **root** on the shared workspace volume are not
cleanly readable by the **jenkins** user that runs `packBuildContainer` in the jnlp
container. They surfaced together under the QEMU-emulated arch as
`open /platform/bindings/maven-settings/settings.xml: permission denied` (native arch
tolerated it). They are RELATED but must be addressed and VERIFIED INDIVIDUALLY — fixing
one does not prove the other.

GENERAL RULE: we MUST NOT chmod app-source or binding files to work around this. The fix is
correct OWNERSHIP at the point the root-run step produces the files. The earlier library
workaround (`chmod -R a+rX` on the binding dirs in the buildkit-emulation
`packBuildContainer`) is the WRONG fix and MUST be removed once 6a/6b are fixed.

#### FR-6a: mvnPipeline root-owned build outputs (`target/`)
- In `mvnPipeline`, `mvn package` runs in the **maven** container as **root**, so its
  outputs (e.g. `target/`, consumed by the buildpack build via
  `BP_MAVEN_BUILT_ARTIFACT=target/*.jar`) are root-owned. `packBuildContainer` (jnlp, jenkins
  user, same shared volume) then can't cleanly read them.
- FIX: chown the maven outputs to jenkins BEFORE `packBuildContainer`, e.g.
  `container('maven') { sh 'chown -R jenkins target' }` in `mvnPipeline`.
- VERIFY INDIVIDUALLY: re-run a `mvnPipeline` app (pd-sample-java-app) emulation build and
  confirm it no longer hits the ownership/permission error from this path.
- (Applies to maven/JVM apps built via `mvnPipeline`.)

#### FR-6b: library-created maven-settings binding files ownership
- Observed on `pd-sample-go-app` (which uses `containerReleasePipeline`, NOT mvnPipeline —
  so 6a does NOT explain it). The `maven-settings` binding files are created by the library
  itself (`configFileProvider` → `cp` into the binding dir) and were not readable by the
  jenkins/CNB build user on the emulated arch → the observed `settings.xml: permission
  denied`. The `chmod -R a+rX` workaround is what let go-app get past bindings.
- FIX: create the binding files so they are OWNED by / readable by the jenkins user by
  construction (correct ownership at creation), rather than a blanket chmod. Applies to
  both the maven-settings and git-credential bindings in the buildkit-emulation
  `packBuildContainer`.
- VERIFY INDIVIDUALLY: re-run a `containerReleasePipeline` app (pd-sample-go-app) emulation
  build with the chmod workaround REMOVED and confirm bindings are readable.

NOTE: both fixes are in the Jenkins shared library (`jenkins-core-shared-libraries`,
PLATFORM-1662 branches). The `jericop/pack` fork needs NO change for Item 6 (re-evaluate
only if a genuine binding-permission issue remains after ownership is corrected). Tracked
here for completeness of the PLATFORM-1662 findings.

### FR-7 — see "Current required task" above
FR-7 is the current required task and is specified in full at the top of this section.

### FR-8 (Item 8): QEMU-emulation instability — cgo/gcc segfault (documented) + cpython python3 ENOENT (OPEN, root cause UNCONFIRMED)
This item now covers TWO distinct failures. Only the first is understood; the second is
OPEN and must NOT be asserted as a native-compile issue (see the correction note).

**FR-8a (documented, environment): cgo/gcc segfault under emulation.**
- NOT a pack code bug: under QEMU on the non-native arch, a Go+cgo build invoking an
  emulated `gcc` can segfault (`runtime/cgo: gcc: signal: segmentation fault`), surfacing as
  `lifecycle: builder ... exit code: 51`.
- Mitigations: `CGO_ENABLED=0` for pure-Go; native multi-agent for cgo/native-extension
  workloads. Optionally test a newer QEMU/binfmt in the builder image.

**FR-8b (OPEN — root cause UNCONFIRMED): cpython `python3` ENOENT on emulated arm64.**
- Observed: agent-patcher-service build #5 (2026-09-02, jenkins-asgard), emulated
  `linux/arm64` (host is amd64):
  ```
  [linux/arm64]     Installing CPython 3.11.15
  [linux/arm64]     pip --version failed. Run with --env BP_LOG_LEVEL=DEBUG ...
  [linux/arm64] fork/exec /layers/paketo-buildpacks_cpython/cpython/bin/python3: no such file or directory
  [linux/arm64] lifecycle: builder ERROR: process "/cnb/lifecycle/builder ..." exit code: 51
  ```
- CORRECTION (supersedes an earlier draft of this note): this is almost certainly NOT a
  CPython "compiled-from-source under QEMU" failure. The cpython `buildpack.toml` ships a
  PREBUILT `arm64` CPython 3.11.15 for the noble stack
  (`python_3.11.15_linux_arm64_noble_8116cb7d.tgz`, `stacks = ["io.buildpacks.stacks.noble"]`),
  so postal resolves a prebuilt tarball → `dependency.URI != dependency.Source` → the
  buildpack EXTRACTS it (`Deliver`); it does NOT run `configure`/`make`. No emulated C
  compile happens at this step, and python itself is NOT executed by the cpython buildpack.
- What IS running under emulation at this step is the cpython buildpack's own Go `build`
  binary (extract + `os.Symlink` of `bin/python3 -> python3.11`, `bin/python -> python3`).
  So the leading hypotheses for the dangling/unusable `python3` are:
  - **H1 (leading):** the emulated buildpack Go binary mis-extracts the tarball or creates
    a `python3` symlink whose target was not actually written → later `pip --version`
    (pip buildpack) `fork/exec python3` returns ENOENT.
  - **H2:** cross-arch layer/extraction issue in the fork's buildkit assembly (arm64 layer
    gets an amd64 `python3`, or the layer doesn't materialize for the emulated platform).
    Considered less likely (backend emits per-platform LLB) but NOT excluded without evidence.
  - **H3:** a version/stack resolution difference specific to this app.
- OPEN QUESTION (must be answered): why does this fail for agent-patcher-service but
  pd-sample-python-app builds fine on the SAME builder + SAME cpython buildpack? Likely an
  INPUT difference (resolved CPython version/stack, or buildpack order — agent-patcher-service
  also spawns a sub-build), not the environment alone. Compare the resolved CPython
  version/stack between the two apps.
- STATUS: root cause UNCONFIRMED. Do NOT categorize as FR-8a (native-compile) or as a fork
  bug until evidence exists. The only firm conclusions: (a) FR-7/flatten is NOT implicated —
  the build reaches the buildpack `builder` phase, well past the ephemeral-builder load;
  (b) CPython is extracted-prebuilt here, not compiled.
- REQUIRED EVIDENCE to close this (no code change until then): a `BP_LOG_LEVEL=DEBUG`
  emulation re-run of agent-patcher-service capturing the cpython buildpack output
  (extract vs any compile) plus `file` and `readlink -f` on
  `/layers/paketo-buildpacks_cpython/cpython/bin/python3` and `.../bin/python3.11` on the
  arm64 side; and the resolved CPython version/stack for BOTH apps. Note: agent-patcher-service
  runs on jenkins-asgard (MCP-unreadable; Tempo carries no pack stdout) — the DEBUG output
  must be captured from the build itself.

### FR-9 (Item 9, OPEN — perf): long post-emit stall after `exporter (emit-mode) DONE`
- OBSERVED on MULTIPLE emulation builds across languages (go, nodejs, java): after the
  exporter's emit step completes, the build sits for a noticeably long time before finishing,
  apparently around resolving the run-image config. Quantified impact from the PLATFORM-1662
  Grafana summary (emulation wall-clock vs multi-agent, last SUCCESS):
  - pd-sample-nodejs-app: multi-agent ~445s vs **emulation ~4903s (~11x)** — extreme outlier.
  - pd-sample-java-app:   multi-agent ~345s vs **emulation ~536s (~1.55x)**.
  The java case is especially telling: for a Maven/JVM app the COMPILE happens in
  `mvnPipeline` BEFORE the container build, so the buildpack build only packages a prebuilt
  jar — there is no compilation in the container-build phase to explain a post-export hang.
  The stall therefore points at the emit/finalize/export flow itself, not app compilation,
  and is NOT explained by emulated-compile cost.
- The window seen in the logs (durations elided — the pause is between these lines and the
  subsequent completion):
  ```
  #23 [linux/amd64] lifecycle: exporter (emit-mode) DONE 10.7s
  #24 resolve image config for docker-image://index.docker.io/paketobuildpacks/ubuntu-noble-run@sha256:536cbd7e...
  #24 resolve image config for docker-image://index.docker.io/paketobuildpacks/ubuntu-noble-run@sha256:536cbd7e... DONE 0.3s
  ```
  (The `resolve image config` vertex itself reports fast — 0.3s — so the STALL is NOT that
  vertex's own duration; it is unexplained time spent at/after this stage.)
- UNKNOWN whether the pause is: (a) a large DATA COPY happening off-vertex (e.g. exporting/
  copying run-image or app layers, cross-arch layer materialization under emulation), (b) a
  serialization/finalize step between emit and the final image assembly, or (c) the build
  WAITING ON INPUT / blocked (needs confirmation it is not an interactive/stdin wait).
- STATUS: root cause UNCONFIRMED. Recorded as an OPEN perf item; NOT categorized as a fork
  bug or an environment issue until evidence exists. Do NOT assume it is emulation-only until
  a native-arch build is checked for the same gap.
- REQUIRED EVIDENCE to close (no code change until then):
  - a TIMESTAMPED emulation build log spanning `exporter (emit-mode) DONE` through the final
    build line, to quantify the gap (wall-clock seconds) and see what, if anything, prints
    during it;
  - confirmation of whether the same post-emit gap appears on a NATIVE-arch buildkit build
    (isolates emulation vs a general finalize cost);
  - if possible, what the process is doing during the gap (e.g. is a layer/data copy or a
    registry push in flight, or is it idle/blocked) — enough to distinguish data-copy vs
    finalize-serialization vs waiting-on-input.
- Relation to FR-5: this is NOT the progress-printer papercut; FR-5 is about noisy repeated
  `DONE` lines, whereas FR-9 is real elapsed time with no obvious activity.

### FR-8b-impl (FIXED, verified): buildkit extra buildpacks MUST be delivered per-arch (multi-arch image) or staged-once (platform-agnostic)
This is the ACTIONABLE fix for the failure recorded under FR-8b. Investigation confirmed
the agent-patcher `python3` ENOENT was NOT (only) emulation: on a multi-arch buildkit build,
extra buildpacks added via `--buildpack` or the project descriptor were staged ONCE from the
HOST arch and copied to BOTH platform legs, so the emulated leg ran wrong-arch buildpack
binaries (amd64 `python3` on the arm64 leg → SIGTRAP; ENOENT under Jenkins). Two wrong-arch
paths existed and are both fixed:
- **Path A** (builder + lifecycle base image): the buildkit backend now pins the per-platform
  CHILD digest of the builder/lifecycle image so each leg runs its own arch binaries.
- **Path B** (extra `--buildpack`/descriptor buildpacks): delivered per classification below.

**Required behavior (buildkit backend only; the single-arch daemon fetch path MUST be left
unchanged).** When `--build-backend buildkit` and one or more extra buildpacks are supplied
(via `--buildpack` or `project.toml`), classify each and deliver it so every platform leg
gets arch-correct content:
- **Multi-arch registry image (an OCI index).** MUST verify the index covers EVERY requested
  `--platform` (else error early with an actionable message naming the missing platform(s)),
  then pull the PER-PLATFORM CHILD image in LLB so each leg overlays its own arch-matching
  `/cnb/buildpacks/...`.
- **Platform-agnostic** — inline `project.toml` script buildpacks, local dir/tarball URIs,
  `urn:cnb:registry` refs, AND single-manifest (non-index) registry images. MUST be staged
  ONCE and the SAME tree copied to every leg. A single-manifest registry image MUST be
  ALLOWED (treated as agnostic), NOT hard-errored: arch-neutral shell-script buildpacks
  (using built-in `tar`/`cp`) are legitimate; an old amd64-only binary buildpack used this
  way will fail at exec, which is the author's responsibility (matching daemon behavior).

**Classification signal (authoritative, no locator string-matching).** The fetched module's
`Descriptor().Targets()`: empty targets, or a target with `OS==""` or `Arch==""` (wildcard)
⇒ platform-agnostic (matches `dist.BuildpackDescriptor.EnsureTargetSupport` "supports all",
and `createInlineBuildpack` emits an empty `dist.Target{}`). Concrete `{OS,Arch}` targets ⇒
the module came from a multi-arch image and is delivered per-arch (excluded from agnostic
staging to avoid double-injection).

**LLB overlay order** in `buildEmitLLB`: builder `/cnb/buildpacks` → agnostic staged tree
(same on every leg) → each per-arch image child (later overrides earlier).

**Acceptance criteria.**
- AC-1 (per-arch correctness): a multi-arch build with a multi-arch registry `--buildpack`
  runs the arch-matching buildpack binary on each leg (arm64 leg → arm64 binary), and the
  build reaches `Finalized CNB metadata for manifest list`. VERIFIED: agent-patcher-service
  + multi-arch cpython → arm64 leg `GOARCH=arm64`, `pip --version` succeeded both legs.
- AC-2 (agnostic + inline coexist): a build with an INLINE buildpack AND multi-arch registry
  buildpacks stages the inline once (`add platform-agnostic buildpacks` vertex on every leg),
  runs the inline on every arch, delivers the registry buildpacks per-arch, and completes.
  VERIFIED: agent-patcher-service `--descriptor inline-test-project.toml` (no `--buildpack`),
  inline `registry-assember` ran `go install` on amd64 AND arm64, build finalized.
- AC-3 (index missing a platform): if a multi-arch buildpack image does NOT provide a
  requested platform, pack errors early naming the missing platform(s).
- AC-4 (single-manifest allowed): a single-manifest registry buildpack is treated as agnostic
  (copied to all legs), not rejected.
- AC-5 (existing path preserved): the single-arch daemon fetch/stage path is unchanged; the
  new logic is buildkit-specific and additive.

**Where.** `pkg/client/build.go` (`collectBuildkitPerArchBuildpackImages`,
`stageAgnosticExtraBuildpacks`, `moduleIsPlatformAgnostic`,
`verifyBuildpackImageSupportsPlatforms`, `buildMultiPlatform` wiring);
`internal/build/multiplatform/{native_buildfunc.go,backend_native.go,backend.go}`
(`buildEmitLLB` copies agnostic local + per-arch image children; `nativeBuildInputs`,
`extraBuildpacksLocalName` mount). Testing note: `--buildpack` OVERRIDES the `project.toml`
order entirely, so a descriptor-driven inline buildpack is only exercised when `--buildpack`
is NOT passed.

## Non-Functional Requirements

### NFR-1: this spec is the single source of truth (FOLLOWUPS.md retired)
- This spec (requirements/design/tasks) is now the single source of truth for the
  PLATFORM-1662 fork follow-ups. The former `FOLLOWUPS.md` at the repo root is RETIRED — do
  not add new detail there; if it still exists it is historical only. When an item's status
  changes, update THIS spec.

### NFR-2: reproducibility
- Any investigation MUST be reproducible from the PLATFORM-1662 build data. The steering
  file `platform-1662-benchmark-data.md` documents the Jenkins jobs, Grafana queries,
  shared-library branches, sample-app repos, and the fork image used, so a developer can
  re-run or inspect any referenced build.

### NFR-3: command execution conventions
- ALL shell command execution for this spec (builds, local `pack build` MVP runs, git,
  `gh workflow run`, inspections) MUST follow the steering file
  `command-execution-practices.md` (marked `inclusion: always`): run every new/changing
  command through the stable pre-approved runner `bash /tmp/run.sh /tmp/cmds/NNN-desc.sh`,
  put env vars as `export` lines inside the command file (never inline `VAR=`), and route
  any command containing `&&`/`|`/`;`/`$(...)`/heredocs through a command file. This keeps
  the invocation string stable so an unattended session is not interrupted by re-approval
  prompts.

## Out of Scope
- The Jenkins shared-library changes themselves (they live in the Rapid7
  `jenkins-core-shared-libraries` repo, PLATFORM-1662 branches). This spec covers the
  `jericop/pack` fork side only.
- The multi-agent (native) build path — it is the baseline, not under change here.
