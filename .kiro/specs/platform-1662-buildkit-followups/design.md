# Design: PLATFORM-1662 BuildKit fork follow-ups

This document maps each finding to the code that must change (or changed). This spec is the
single source of truth (the former `FOLLOWUPS.md` is retired). File paths are relative to
the repo root of the fork (`buildkit-native-export-with-history-and-kiro` branch).

> **Only Item 7 (below) is a current pack code change.** All other sections are reference:
> implemented, WON'T FIX, deferred, or owned by the Jenkins library.

## ▶ CURRENT REQUIRED TASK — Item 7: flatten the ephemeral builder (FR-7)

### The failure, traced to source
- `pkg/client/build.go` (~L512-520): `hasAdditionalBuildpacks && !opts.TrustExtraBuildpacks`
  (or `hasExtensions`) → logs `Builder is trusted but additional modules were added; using
  the untrusted (5 phases) build flow` and sets `useCreator = false`.
- The untrusted flow synthesizes an ephemeral builder image `pack.local/builder/<hex>` via
  `createEphemeralBuilder` → `Builder.Save` (`internal/builder/builder.go` ~L466).
- `Builder.Save` calls `b.image.AddLayer(...)` repeatedly. The added modules go through
  **`addExplodedModules` = one image layer per module**. `addFlattenedModules` (a single
  combined layer) already exists in the same file but is NOT used for these added modules.
- On the deep noble builder base, the per-module layers push past Docker's ~125-layer cap →
  the daemon rejects the load with `max depth exceeded`, before any lifecycle phase runs.

### The fix (Option B — flatten; chosen and only approach)
- Route the added builder modules through the **flattened** path so all added modules land in
  ONE image layer instead of one-per-module. Concretely, in `Builder.Save` /
  `createEphemeralBuilder`, the additional buildpacks that currently populate
  `additionalBuildpacks.ExplodedModules()` must be treated as flattened
  (`addFlattenedModules` over the combined set), OR — on the buildkit-backend path — assembled
  over `/cnb/buildpacks` and emitted as a single merged diff (`llb.Merge` / one squashed
  layer) rather than a chain of per-module copies.
- INVARIANT: adding N extra modules adds O(1) builder image layers, not O(N). The synthesized
  builder must load cleanly on a deep base builder.
- Where to look:
  - `internal/builder/builder.go`: `Save`, `addExplodedModules`, `addFlattenedModules`,
    `additionalBuildpacks` (`ExplodedModules()` / `FlattenedModules()`).
  - `pkg/client/create_builder.go`: `createEphemeralBuilder`, `addBuildpacksToBuilder`.
  - `pkg/client/build.go`: the `useCreator=false` decision (~L512-520) — do NOT need to keep
    the creator path; the requirement is that the ephemeral builder it produces is shallow.
- Do NOT: squash the final app image (export is never reached), chmod, or tweak daemon
  storage. The fix is the builder-image layer count.

### Publish + test (handoff)
- After the fix passes the MVP local build, publish a new fork pack image from the
  `buildkit-native-export-with-history-and-kiro` branch: push the fix to that remote branch,
  then run the branch-driven `publish-pack.yml` workflow
  (`gh workflow run publish-pack.yml --repo jericop/pack --ref fork-main -f ref=buildkit-native-export-with-history-and-kiro`),
  which builds that branch and publishes the moving image tag
  `jericop/pack:buildkit-native-export-with-history-and-kiro` (no git tag required — see
  `dot-kiro-files/publish-images-runbook.md` section 3A). Record the pushed tag/digest in
  `tasks.md` (Task 7, AC-3).
- Then (in jenkins-core-shared-libraries, not here) point `env.PACK_FORK_IMAGE` at the new
  image and re-run the nodejs emulation build to confirm end to end (AC-4).

---

## Reference (NOT current tasks)

## Fixed items (already implemented on this branch)

### Item 1 — resolve real current buildx builder (`buildx_state.go`)
- `resolveBuildkitAddr` no longer hard-codes `pack-multiplatform`.
- New `resolveCurrentBuildxBuilder` reads buildx on-disk state
  (`$DOCKER_CONFIG/buildx/current` + `.../instances/<name>`, honoring `BUILDX_BUILDER`);
  no docker shell-out, no buildx module import.
- `driverSupportsMultiPlatform` returns an actionable error for the `docker` driver.
- Tests: `buildx_state_internal_test.go`.

### Item 2 — app-context sync (`appcontext.go`, `backend_native.go`)
- Dropped the `fsutil.NewFilterFS(..., Map: ...)` wrapper on the app context.
- `stageFilteredAppDir` stages kept files into a temp dir (preserving layout/modes/
  symlinks) and hands BuildKit a plain `fsutil.NewFS(stagedDir)` (sorted, parents-first
  walk). Temp dir cleaned up via defer.
- Tests: `appcontext_internal_test.go` (nested `src/<pkg>` tree, exclude set, symlinks;
  validated against `fsutil.Validator`).

### Item 4 — single env vertex (`native_buildfunc.go`)
- `buildEmitLLB` chains all `/platform/env/<NAME>` writes into ONE `llb.Mkfile` FileAction
  named `[<plat>] write platform env (<N> vars)`. Keys sorted → deterministic/cache-stable.

## By-decision / deferred items

### Item 3 — `--trust-builder` required (WON'T FIX)
- No code change. Decision: the fork builder is self-built/untrusted; requiring
  `--trust-builder` is accepted. Ensure docs/help make it clear callers must pass it (or
  `pack config trusted-builders add <builder>`).

### Item 5 — progress printer polish (DEFERRED, optional)
- `startProgressDisplay` in `buildkit_client.go`:
  - Quick win: add a `vertexCompleted map[string]bool` guard so `DONE`/`CACHED`/`ERROR`
    prints once per vertex (removes the repeated-`DONE` flood).
  - Larger: thread the pack log level / `--verbose` into `BuildkitBackend` →
    `startProgressDisplay`; default to a concise summary, `--verbose` for full per-vertex/
    per-log detail. Preferred end state: one START + one END(+duration) line per vertex by
    default.

## Open items (need work)

### Item 6 — file OWNERSHIP: two related-but-separate issues (Jenkins library, NOT the fork)
Both are ownership problems (root-run container writes files the jenkins user can't read on
the shared volume), both surfaced as `settings.xml: permission denied` on the emulated arch,
but they have different sources and must be fixed + verified INDIVIDUALLY. Do NOT chmod
app-source/binding files as a general fix; correct ownership at creation.

- **6a — mvnPipeline `target/`:** `mvn package` runs as root in the maven container;
  root-owned `target/` (consumed via `BP_MAVEN_BUILT_ARTIFACT=target/*.jar`) is unreadable
  by the jenkins/CNB user in jnlp. Fix in `mvnPipeline`:
  `container('maven') { sh 'chown -R jenkins target' }` before `packBuildContainer`. Verify
  with a `mvnPipeline` app (pd-sample-java-app).
- **6b — library-created binding files:** observed on pd-sample-go-app
  (`containerReleasePipeline`, NOT mvnPipeline — 6a doesn't explain it). The maven-settings
  binding files created by the library (`configFileProvider` → `cp`) weren't readable by the
  jenkins user. Fix: create the binding files owned-by/readable-by the jenkins user by
  construction (not a blanket chmod), for both maven-settings and git-credential bindings in
  the buildkit-emulation `packBuildContainer`. Verify with a `containerReleasePipeline` app
  (pd-sample-go-app) with the chmod workaround REMOVED.
- The fork needs NO change for either. Remove the earlier `chmod -R a+rX` workaround once
  6a/6b land.

### Item 7 — CURRENT REQUIRED TASK
See "▶ CURRENT REQUIRED TASK — Item 7: flatten the ephemeral builder (FR-7)" at the top of
this document for the full mechanism and fix. (The chosen approach is to FLATTEN the added
builder modules into a single layer so the ephemeral builder stays under the daemon layer
cap; earlier drafts mentioned an overlay-only path — the layer-flatten invariant is the
requirement.)

### Item 8 — QEMU emulation issues (8a documented; 8b OPEN)
Two distinct failures under this item. Both surface as `lifecycle: builder ... exit code: 51`
but they are NOT the same root cause.

**8a — cgo/gcc segfault (documented; environment; NOT a fork bug).**
- Emulated `gcc`/cgo on the non-native arch can segfault
  (`runtime/cgo: gcc: signal: segmentation fault`).
- Mitigation: `CGO_ENABLED=0` for pure-Go; native multi-agent for cgo/native workloads;
  optionally a newer QEMU/binfmt in the builder image.

**8b — cpython `python3` ENOENT on emulated arm64 (OPEN; root cause UNCONFIRMED).**
- agent-patcher-service #5 (2026-09-02), `linux/arm64` under QEMU (host amd64):
  `Installing CPython 3.11.15` → `pip --version failed` →
  `fork/exec /layers/paketo-buildpacks_cpython/cpython/bin/python3: no such file or directory`
  → `exit code: 51`.
- CORRECTION (supersedes an earlier draft that blamed a source compile): CPython is NOT
  compiled here. The cpython `buildpack.toml` ships a PREBUILT arm64 noble CPython 3.11.15
  (`python_3.11.15_linux_arm64_noble_8116cb7d.tgz`), so `dependency.URI != dependency.Source`
  → the buildpack EXTRACTS the tarball (`Deliver`), no `configure`/`make`. python is not
  executed by the cpython buildpack at this step.
- Leading hypotheses (see requirements FR-8b): H1 the emulated cpython buildpack Go `build`
  binary mis-extracts / mis-symlinks `bin/python3 -> python3.11` (dangling target) → later
  pip `fork/exec python3` ENOENT; H2 a cross-arch layer/extraction issue in the fork's
  buildkit assembly; H3 a per-app version/stack difference.
- Firm so far: FR-7/flatten is NOT implicated (build reaches the buildpack `builder` phase);
  CPython is extracted-prebuilt, not compiled. Open: why agent-patcher-service fails but
  pd-sample-python-app (same builder + buildpack) succeeds — likely an input difference.
- REQUIRED EVIDENCE before any categorization/fix: `BP_LOG_LEVEL=DEBUG` emulation re-run of
  agent-patcher-service; capture cpython buildpack output + `file`/`readlink -f` on
  `bin/python3` and `bin/python3.11` (arm64), and the resolved CPython version/stack for
  both apps. (jenkins-asgard console is MCP-unreadable and Tempo has no pack stdout, so the
  DEBUG output must come from the build.)

### Item 9 — post-emit stall after `exporter (emit-mode) DONE` (OPEN; perf; root cause UNCONFIRMED)
- Seen on MULTIPLE emulation builds (go, nodejs, java): a long, unexplained pause AFTER the
  exporter emit step, around the run-image config resolve, before the build finishes. Impact
  (emulation vs multi-agent, from the Grafana summary): nodejs ~4903s vs ~445s (~11x, extreme
  outlier); java ~536s vs ~345s (~1.55x). Java is strong evidence the stall is in the
  emit/finalize/export flow, NOT app compilation: a Maven/JVM app compiles in `mvnPipeline`
  BEFORE the container build, so the buildpack phase only packages a prebuilt jar — nothing
  in that phase should hang after export. Log window:
  ```
  #23 [linux/amd64] lifecycle: exporter (emit-mode) DONE 10.7s
  #24 resolve image config for docker-image://.../ubuntu-noble-run@sha256:536cbd7e...
  #24 resolve image config for docker-image://.../ubuntu-noble-run@sha256:536cbd7e... DONE 0.3s
  ```
  The `resolve image config` vertex reports fast (0.3s), so the elapsed time is spent
  elsewhere at/after this stage — not in that vertex.
- Candidate mechanisms (to disambiguate with evidence, not assume):
  - a DATA COPY off-vertex — e.g. run-image/app layer export or cross-arch layer
    materialization under emulation happening between emit and final assembly;
  - a finalize/serialization step between the emit phase and image assembly/push;
  - the build BLOCKED / waiting on input (must rule out an interactive/stdin wait).
- Where to look in the fork when evidence points to code (do NOT change anything yet):
  - the buildkit backend's post-emit / finalize path and progress plumbing
    (`native_buildfunc.go`, `buildkit_client.go` `startProgressDisplay`) — to see whether a
    long operation runs without emitting a vertex;
  - the exporter→run-image config resolve→assembly sequencing in the emit-mode path;
  - whether the gap is per-platform (emulated arm64) or also on native amd64.
- STATUS: OPEN, root cause UNCONFIRMED. Not a fork bug or environment issue until a
  timestamped log quantifies the gap AND a native-arch build is checked for the same pause.
  Distinct from Item 5 (that is cosmetic repeated `DONE` lines; this is real elapsed time).

## Item 8b-impl — extra buildpacks delivered per-arch / staged-once (FR-8b-impl, FIXED)

The ACTIONABLE resolution of FR-8b. Two wrong-arch delivery paths on the buildkit backend
caused the emulated leg to run host-arch buildpack binaries (agent-patcher cpython `python3`
SIGTRAP/ENOENT). Both fixed; single-arch daemon path untouched.

### Path A — builder + lifecycle base image per-arch
The backend pins the PER-PLATFORM CHILD digest of the builder/lifecycle image so each leg
runs its own arch binaries (via `remote.Get(WithPlatform).Image().Digest()` in
`native_buildfunc.go`; `build.go` sets the lifecycle image to the builder tag on the buildkit
path). This stopped the arm64 leg from running amd64 buildpack/lifecycle binaries.

### Path B — extra `--buildpack` / project.toml buildpacks
Classify each declared extra buildpack and deliver arch-correct content:

- **Multi-arch registry image (OCI index).** `collectBuildkitPerArchBuildpackImages`
  (`pkg/client/build.go`) walks the declared buildpacks (same precedence as
  `processBuildpacks`: `--buildpack`, else `project.toml build.buildpacks`), and for each
  `PackageLocator` calls `verifyBuildpackImageSupportsPlatforms(img, platforms)`:
  - index → verify it covers EVERY requested platform (else error naming the missing ones),
    add to `perArchImages`;
  - single manifest → `isIndex=false`, NOT an error; left for agnostic staging.
  `buildEmitLLB` then overlays each per-arch image child (`llb.Image(ref, llb.Platform(p))`)
  onto the matching leg.
- **Platform-agnostic** (inline script, local dir/tarball, `urn:cnb:registry`,
  single-manifest image). `stageAgnosticExtraBuildpacks(fetchedBPs)` selects the agnostic
  modules — those where `moduleIsPlatformAgnostic` is true (empty `Descriptor().Targets()`,
  or a wildcard `OS==""`/`Arch==""`) — and reuses the existing `stageExtraBuildpacks` to lay
  them out under `/cnb/buildpacks/{id}/{version}/*` in ONE dir. The backend mounts that dir
  as a single `llb.Local(extraBuildpacksLocalName)` and `buildEmitLLB` copies the SAME tree
  to every leg. Modules with concrete `{OS,Arch}` targets (from a multi-arch image) are
  EXCLUDED here to avoid double-injection (they arrive via the per-arch image child).

Classification uses the module's declared targets (authoritative, matches
`dist.BuildpackDescriptor.EnsureTargetSupport`; `createInlineBuildpack` emits an empty
`dist.Target{}`), NOT locator string-matching.

**Overlay order in `buildEmitLLB`:** builder `/cnb/buildpacks` → agnostic staged tree →
per-arch image children (later overrides earlier).

### Wiring
- `pkg/client/build.go`: `buildMultiPlatform` collects `perArchImages` +
  `agnosticStagedDir`, passes both into the backend opts (keeps `ExtraBuildpackImages` +
  `ExtraBuildpacksDir`); import `ggcrremote "github.com/google/go-containerregistry/pkg/v1/remote"`.
- `internal/build/multiplatform/backend_native.go`: re-adds the agnostic local mount
  (`extraBuildpacksLocalName`) and passes the per-arch image list through.
- `internal/build/multiplatform/native_buildfunc.go`: `nativeBuildInputs` carries
  `hasAgnostic` + `extraBuildpackImages`; `buildEmitLLB` copies agnostic local + per-arch
  image children per leg.
- `internal/build/multiplatform/backend.go`: signature plumbing.

### Verified end-to-end (local; Apple-Silicon host, amd64 emulated)
- Multi-arch registry image (per-arch): agent-patcher-service + `--buildpack` multi-arch
  cpython → arm64 leg `GOARCH=arm64`, arm64 python tarball, `pip --version` succeeded both
  legs, `Finalized CNB metadata for manifest list`.
- Inline + agnostic coexisting with per-arch registry buildpacks: agent-patcher-service
  `--descriptor inline-test-project.toml` (no `--buildpack`) → `add platform-agnostic
  buildpacks` vertex on both legs, inline `registry-assember` `go install` ran on amd64 AND
  arm64, registry buildpacks per-arch, build finalized.

Testing lesson: `--buildpack` OVERRIDES the `project.toml` order (`processBuildpacks`:
`declaredBPs := opts.Buildpacks`, only falls back to `opts.ProjectDescriptor.Build.Buildpacks`
when empty), so a descriptor-driven inline is only exercised with NO `--buildpack`.

## PLATFORM-1662 performance context (why this matters)

For the apps that build on BOTH strategies, emulation wall-clock was ~1.2–1.4× the
multi-agent wall-clock (MULTIPLICATIVE, i.e. 17%–36% longer — NOT 2×):
- pd-sample-go-app (cgo off): multi-agent ~325s vs emulation ~379s (~1.17×).
- pd-sample-java-app: multi-agent ~321s vs emulation ~435s (~1.36×).

Caveat to carry into any report: multi-agent spends ~2× its wall-clock in total agent-time
(two parallel agents) vs emulation's single agent; if agent capacity/cost is the
constraint, emulation's single-agent footprint may be preferable despite slower wall-clock.
The remaining apps (nodejs, python x2) are blocked by Items 6/7 (and app-fixture issues),
so fixing those unblocks a fuller language comparison.
