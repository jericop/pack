# Follow-ups (buildkit-native-export) — RETIRED

> **RETIRED — do not use as source of truth.** The PLATFORM-1662 fork follow-ups now live
> in the spec at `.kiro/specs/platform-1662-buildkit-followups/`
> (`requirements.md` / `design.md` / `tasks.md`). Make all updates there.
>
> **Current required task:** there is exactly ONE actionable pack change right now — FR-7 /
> Task 7: **flatten the ephemeral builder** so extra buildpacks don't hit `max depth
> exceeded`, then publish a new fork pack image on
> `buildkit-native-export-with-history-and-kiro` and hand back for testing. Everything else
> is reference (fixed / WON'T FIX / deferred / Jenkins-library-owned).
>
> The content below is HISTORICAL and kept only for git history.

---

Issues and deferred improvements for the BuildKit multi-arch (`--build-backend
buildkit`) fork work. Found while benchmarking the Rapid7 Jenkins PLATFORM-1662
multi-arch build performance comparison (buildkit emulation vs the existing multi-agent
native strategy), which drives the fork `pack` binary from Jenkins against five sample
apps (go, nodejs, python, java, agent-patcher) across `linux/amd64,linux/arm64`.
(`pd-rds-postgres-password-lambda` was excluded — its PyGreSQL dep needs the postgres
client, which the patched noble builder lacks; not a valid emulation test subject.)

Severity legend:
- **BLOCKER** — currently fails a build; needs a fix (or a documented workaround).
- **PAPERCUT** — build succeeds but the UX/output is poor.

## Resolution status (2026-09)

| # | Item | Decision | Status |
|---|------|----------|--------|
| 1 | Empty `--buildkit-builder` resolves to non-existent `pack-multiplatform` | Resolve the ACTUAL current buildx builder from buildx on-disk state; clear error for the docker driver | **FIXED** |
| 2 | App context sync `changes out of order` (nested dirs) | Stage kept files to a temp dir and hand BuildKit a plain `fsutil.NewFS` (drop the `FilterFS` Map wrapper) | **FIXED** |
| 3 | Untrusted fork builder needs `--trust-builder` | Accept that `--trust-builder` is required (builder is our own, technically untrusted) | **WON'T FIX (by decision)** — no code change |
| 4 | `platform env:` vertex per env var floods output | Collapse all env-file writes into ONE vertex | **FIXED** |
| 5 | Repeated `DONE` lines per vertex; gate progress behind `--verbose` | Optional polish; leave for a later pass | **DEFERRED (optional)** |

Details per item below, annotated with what changed.

### Validation (2026-09)

- **Local** (fork `pack` built from source, `pd-sample-python-app` — the nested
  `src/<pkg>/` + `project.toml` include-filter repro):
  - #1: with no `--buildkit-builder` and the macOS default `desktop-linux` (docker
    driver) selected, the build now fails with the actionable docker-container error
    (not the old cryptic `buildx_buildkit_pack-multiplatform0 not found`).
  - #2: the multi-arch build gets PAST `copy app source` into the lifecycle phases with
    NO "changes out of order" — the fsutil ordering failure is gone.
  - #4: progress shows a single `write platform env (N vars)` vertex.
  - (See `local-registry-testing.md` for the registry-networking setup; use
    `pack-local-registry:5000` + `PACK_HOST_REGISTRY_REMAP`, not `localhost:5050`.)
- **CI** (`benchmark-dockerfile-vs-buildpacks.yml` built from the fix branch): all 12
  cells succeeded, including all three `python-poetry` cells via the buildkit backend;
  the `benchmark-perf-smoke.yml` run also succeeded.

> NOTE: `pd-sample-python-app` currently has a STALE `poetry.lock` (out of sync with its
> `pyproject.toml`), so a full local build of THAT fixture fails in the Poetry buildpack
> (`pyproject.toml changed significantly since poetry.lock was last generated`). That is
> an app-fixture problem, unrelated to these fixes — the build reaches dependency
> install, well past the app-context sync that #2 addresses. CI uses
> `paketo-buildpacks/samples` (clean lockfile) and builds green.

---

## 1. [FIXED] Empty `--buildkit-builder` resolves to a non-existent `pack-multiplatform` builder

> **Resolved.** `resolveBuildkitAddr` no longer hard-codes `pack-multiplatform`. When
> `--buildkit-builder` is empty it now resolves the ACTUAL current buildx builder by
> reading buildx's on-disk state (`$DOCKER_CONFIG/buildx/current` +
> `.../instances/<name>`, honoring `BUILDX_BUILDER`) — no shelling out to docker, no
> buildx module import. If the current builder uses the `docker` driver (which cannot
> serve multi-platform buildkit), it returns an actionable error telling the user to
> create/select a `docker-container` builder. New code: `buildx_state.go`
> (`resolveCurrentBuildxBuilder`, `driverSupportsMultiPlatform`), covered by
> `buildx_state_internal_test.go`. The Jenkins workaround (create + pass
> `--buildkit-builder pack-multiplatform`) still works and is no longer required for the
> empty-builder case.

**Symptom.** With `--build-backend buildkit` and NO `--buildkit-builder`, the build fails:

```
ERROR: failed to build: multi-platform build: connecting to buildkit:
builder container buildx_buildkit_pack-multiplatform0 not found;
ensure builder is running: docker buildx inspect --bootstrap pack-multiplatform
```

**Cause.** `internal/build/multiplatform/buildkit_client.go`, `resolveBuildkitAddr`:

```go
builderName := b.buildkitOpts.Builder
if builderName == "" {
    builderName = "pack-multiplatform"           // <-- hard-coded default name
}
containerName := fmt.Sprintf("buildx_buildkit_%s0", builderName)
```

When `--buildkit-builder` is empty, the code defaults the builder NAME to the literal
`pack-multiplatform` and then connects to the container `buildx_buildkit_pack-multiplatform0`.
If no buildx builder of that exact name exists, the connect fails. The `--buildkit-builder`
flag help says it "defaults to the current buildx default", but the code does NOT fall
back to the actual current buildx default builder — it invents a fixed name.

**Desired behavior.** When `--buildkit-builder` is empty, resolve the ACTUAL current
buildx default builder (e.g. parse `docker buildx inspect` / the buildx state) and use
its `buildx_buildkit_<name>0` container — or, if the current default is a `docker`-driver
builder (which cannot serve multi-platform buildkit), emit a clear, actionable error that
tells the user to create/select a `docker-container` (or `remote`) builder. Do NOT
silently assume a `pack-multiplatform` builder exists. We should be deriving the default builder using the buildkit package in the go code rather than shelling out to run docker commands.

**Workaround in use (PLATFORM-1662).** The Jenkins library creates a `docker-container`
buildx builder named `pack-multiplatform` (+ QEMU) and bootstraps it, then passes
`--buildkit-builder pack-multiplatform` explicitly. So the fork works today only because
the caller supplies both the builder and the matching name.

**Where.** `internal/build/multiplatform/buildkit_client.go`, `resolveBuildkitAddr`
(lines ~34-53).

---

## 2. [FIXED] App context sync fails: `changes out of order`

> **Resolved.** The `fsutil.NewFilterFS(..., Map: ...)` wrapper (which could make
> fsutil's diff/send stream entries in an order BuildKit's receiver rejects for nested
> layouts) was dropped. When a project-descriptor file filter is in effect, pack now
> STAGES the kept files into a temp dir (`stageFilteredAppDir` in `appcontext.go`,
> preserving relative layout, modes, and symlinks) and hands BuildKit a plain
> `fsutil.NewFS(stagedDir)`, which walks in the standard sorted (parents-before-children)
> order the receiver accepts — mirroring the daemon backend's filtered-tar approach. The
> temp dir is cleaned up via a deferred func. Regression test:
> `appcontext_internal_test.go` builds a nested `src/<pkg>/...` tree (the reported
> failure shape), stages it, and asserts the result walks cleanly through
> `fsutil.Validator` (the same ordering contract the receiver enforces), plus
> exclude-set and symlink-preservation cases.

**Symptom.** For some apps (reproduced on `pd-sample-python-app`, whose source has a
nested package dir `src/pd_sample_python_app/`), the build fails during app-context sync:

```
#1 local://context ERROR: changes out of order: "src/pd_sample_python_app" "server.py"
ERROR: failed to build: multi-platform build: buildkit-native build: failed to solve:
changes out of order: "src/pd_sample_python_app" "server.py"
```

Other apps (e.g. `pd-sample-go-app`) build fine, so it is triggered by a particular
file/dir layout.

**Cause (area).** The app source is provided to BuildKit as an `fsutil.FS` local mount in
`internal/build/multiplatform/backend_native.go` (`driveNative`):

```go
appFS, err := fsutil.NewFS(opts.AppPath)
...
if opts.FileFilter != nil {
    appFS, err = fsutil.NewFilterFS(appFS, &fsutil.FilterOpt{
        Map: func(p string, _ *fstypes.Stat) fsutil.MapResult { ... },
    })
}
...
localMounts := map[string]fsutil.FS{ contextLocalName: appFS }
```

The `changes out of order` error originates in the vendored
`github.com/tonistiigi/fsutil` diff/send protocol, which requires directory-walk entries
to arrive in a strict (lexicographic, parents-before-children) order. The wrapping
(`NewFilterFS` Map, and/or how the project-descriptor include/exclude filter maps paths)
appears to emit entries in an order fsutil's receiver rejects for nested package
directories — hence "`src/pd_sample_python_app`" then "`server.py`" being flagged as out
of order.

**Desired behavior.** The app context must sync for arbitrary (nested) source layouts.
Investigate whether the `FilterFS`/`Map` wrapper or the underlying `NewFS` walk is
producing out-of-order entries; ensure a stable, sorted walk (parents before children)
that satisfies fsutil's ordering contract. Add a regression test using a nested source
tree (e.g. `src/<pkg>/...`) that reproduces the failure.

**Where.** `internal/build/multiplatform/backend_native.go`, `driveNative` app-context
`fsutil.NewFS` / `fsutil.NewFilterFS` construction (lines ~124-146). Root cause may be in
the vendored `tonistiigi/fsutil` interaction; confirm whether upstream fsutil handles this
without the filter wrapper.

---

## 3. [WON'T FIX — by decision] Untrusted fork builder fails: `Lifecycle 0.0.0 does not have an associated lifecycle image`

> **Decision: accept that `--trust-builder` is required.** The fork builder is one we
> built ourselves and is technically untrusted, so requiring `--trust-builder` (as the
> PLATFORM-1662 pipeline already passes via `env.ADDITIONAL_PACK_ARGS`) is the accepted
> behavior. No code change. The other options below (give the lifecycle a resolvable
> version / publish a lifecycle image / soften the error for the buildkit backend) were
> considered and NOT chosen.

**Symptom.** Without `--trust-builder`, a build with the fork builder fails:

```
ERROR: failed to build: Lifecycle 0.0.0 does not have an associated lifecycle image.
Builder must be trusted.
```

**Cause.** The fork builder (`jericop/ubuntu-noble-builder:buildkit-native-export`) bundles
a dev-versioned lifecycle (version `0.0.0`). For an UNTRUSTED builder, pack tries to look
up a published lifecycle IMAGE matching the builder's lifecycle version (to run phases in
separate containers); version `0.0.0` has no such image, so it errors. Trusting the
builder lets pack run the builder's bundled lifecycle in place, avoiding the image lookup.

**Desired behavior (pick one).**
- Give the fork builder's lifecycle a real (resolvable) version, OR publish a lifecycle
  image for it, so an untrusted build can find a lifecycle image; OR
- For the buildkit backend specifically (which runs the lifecycle inside the builder
  image via LLB regardless), don't require a separate lifecycle image / make the error
  actionable ("this builder must be trusted: pass --trust-builder or `pack config
  trusted-builders add <builder>`"). 
- Accept that `--trust-builder` is required because we are using a builder that we built which is technically untrusted. I PICK THIS ONE

**Workaround in use (PLATFORM-1662).** Pass `--trust-builder` (via
`env.ADDITIONAL_PACK_ARGS`). This is required today for every emulation build.

**Where.** Lifecycle-image resolution / trust gating in the build path (the error text is
the lifecycle-image-descriptor lookup). Relevant to the buildkit backend because it always
runs the lifecycle from the builder image.

---

## 4. [FIXED] `platform env:` vertices are always shown — quiet unless `--verbose`

> **Resolved** via the simplest/biggest-win option: collapse all env-file writes into a
> SINGLE vertex. `buildEmitLLB` now chains every `/platform/env/<NAME>` write into one
> `llb.Mkfile` FileAction with a single custom name `[<plat>] write platform env (<N>
> vars)`, instead of one named vertex per variable. Keys are still sorted first so the
> op is deterministic and LLB stays cache-stable. This removes the per-env-var flood
> regardless of verbosity, so no `--verbose` threading was needed for this item.

**Symptom.** A multi-arch buildkit build prints one progress vertex per build-time
env var, per platform, e.g.:

```
#85 [linux/amd64] platform env: JOB_BASE_NAME DONE 0.2s
#86 [linux/amd64] platform env: JOB_DISPLAY_URL DONE 0.1s
#87 [linux/amd64] platform env: JOB_NAME
...
```

When the caller passes many env vars (e.g. a CI system like Jenkins passing its whole
environment via `--env-file`), this floods the output with dozens of vertices per
platform and buries the meaningful lifecycle steps.

**Cause.** In `internal/build/multiplatform/native_buildfunc.go` (`buildEmitLLB`), each
build-time env var is written as its own `llb.Mkfile` under `/platform/env/<NAME>` with
a distinct custom vertex name:

```go
llb.WithCustomNamef("[%s] platform env: %s", plat, k)
```

BuildKit renders every named vertex in the default progress output, so all of these
are always visible regardless of the pack log level.

**Desired behavior.** These per-env-var vertices should only be surfaced when the
caller opts into verbose output (`pack ... --verbose`). By default they should be
hidden/collapsed.

**Options to consider (pick during implementation):**
- Collapse all env-file writes into a SINGLE vertex (e.g. one `llb.Mkfile`-equivalent
  step named `[<plat>] write platform env (<N> vars)`), instead of one vertex per var.
  This is the simplest and biggest readability win, and is arguably correct regardless
  of verbosity.
- OR keep per-var vertices but only attach the custom (visible) name when the pack
  logger is in verbose/debug mode; otherwise omit `WithCustomName` (or mark them so the
  progress printer treats them as internal) so they don't clutter default output.
- Ensure whichever approach is taken still keeps deterministic key ordering (the code
  sorts keys today) so LLB stays cache-stable.

**Where.** `internal/build/multiplatform/native_buildfunc.go`, the
`if len(in.buildEnv) > 0 { ... }` loop that calls
`llb.WithCustomNamef("[%s] platform env: %s", ...)`. Thread the pack log level /
verbose flag into `nativeBuildInputs` if the "only when --verbose" approach is chosen.

---

## 5. [DEFERRED — optional] Repeated `DONE` lines per vertex — de-dupe completion, quiet unless `--verbose`

> **Deferred as optional polish.** Left for a later pass. The trivial part (de-dupe the
> completion line via a `vertexCompleted` guard in `startProgressDisplay`) can be done
> on its own; the larger `--verbose`-gating of the whole progress printer is the
> bigger change and is not required for the comparison. The env-vertex flood (the worst
> offender) is already addressed by #4.

**Symptom.** The same completed vertex prints its `DONE` line many times, with a
slightly increasing duration each time, e.g.:

```
#6 docker-image://docker.io/jericop/ubuntu-noble-builder:buildkit-native-export DONE 38.3s
#6 docker-image://docker.io/jericop/ubuntu-noble-builder:buildkit-native-export DONE 39.0s
#6 docker-image://docker.io/jericop/ubuntu-noble-builder:buildkit-native-export DONE 39.4s
#6 docker-image://docker.io/jericop/ubuntu-noble-builder:buildkit-native-export DONE 39.7s
...
```

**Cause.** In `internal/build/multiplatform/buildkit_client.go`, `startProgressDisplay`
consumes the BuildKit `SolveStatus` stream. BuildKit re-delivers the same vertex on
successive status refreshes. The code guards the START line (only prints when
`vertexStartTimes[id] == 0`), but the COMPLETED branch has NO such guard:

```go
if v.Completed != nil {
    // ... prints "#N ... DONE %.1fs" EVERY time this vertex appears again,
    // so a long-running/among-many-refresh vertex prints DONE repeatedly.
}
```

So any vertex that stays in the status stream across multiple refreshes after
completing (notably the base-image pull `docker-image://...`) prints `DONE` once per
refresh.

**Desired behavior.**
1. **De-dupe completion (always correct):** track a `vertexCompleted map[string]bool`
   (like `vertexStartTimes`) and only print the CACHED/ERROR/DONE line the FIRST time
   a vertex completes. This alone removes the repeated-`DONE` flood.
2. **Gate progress behind `--verbose` (readability):** this custom stderr progress
   printer (the numbered vertices, `DONE`/`CACHED` lines, and per-log-line output)
   should only be shown when the caller passes `pack ... --verbose` (or the pack
   logger is in verbose/debug mode). By default, show a concise summary (e.g. just
   phase-level lifecycle steps and errors), not every buildkit vertex. Thread the pack
   log level / verbose flag into the backend (`BuildkitBackend`) and into
   `startProgressDisplay` so it can no-op / collapse when not verbose.

**Where.** `internal/build/multiplatform/buildkit_client.go`, `startProgressDisplay`
(the `for status := range ch` loop). The verbose flag likely comes from the same place
that would gate follow-up #4 (thread it through `nativeBuildInputs` / `BuildkitBackend`
from the pack logger).

### Note (preferred UX for #4 and #5)

Rather than gating everything behind `--verbose`, the simplest good default is to print
exactly TWO lines per vertex: a START line when it begins and a single END line with the
total duration when it completes (`DONE`/`CACHED`/`ERROR`) — and NEVER re-emit interim
updates. That removes both the repeated-`DONE` flood (#5) and, combined with collapsing
the env-file writes into one vertex (#4), keeps the default output to one start + one end
per real step. `--verbose` can then add the per-refresh/per-log-line detail on top. Review
this approach when implementing.


---

## 6. [BLOCKER — Jenkins library, NOT the fork] File OWNERSHIP: root-run container vs jenkins user

> **Corrected diagnosis.** This is NOT a `--binding` file-mode bug and NOT a pack/fork bug.
> It is file OWNERSHIP: a container running as **root** writes files onto the shared
> workspace volume that the **jenkins** user (which runs `packBuildContainer` in the jnlp
> container) then cannot cleanly read. The native arch tolerated it; the QEMU-emulated arch
> surfaced it as `permission denied`. There are TWO related-but-SEPARATE instances — fix and
> VERIFY each individually (fixing one does not prove the other). We must NOT chmod
> app-source/binding files as a general workaround; fix OWNERSHIP at the point the root-run
> step creates the files. The interim `chmod -R a+rX` on the binding dirs (added in the
> buildkit-emulation `packBuildContainer`) is the WRONG fix and MUST be removed once 6a/6b
> land. Both fixes are in the Jenkins shared library; the fork needs no change.

**Symptom (as observed under emulation).** The NON-NATIVE (emulated, e.g. linux/arm64)
platform fails while buildpacks read the maven-settings binding:

```
#21 [linux/arm64] unable to read platform bindings /platform
#21 [linux/arm64] unable to create new binding from /platform/bindings/maven-settings
#21 [linux/arm64] open /platform/bindings/maven-settings/settings.xml: permission denied
```

The NATIVE platform (linux/amd64) reads the same files fine.

### 6a. mvnPipeline: root-owned `mvn package` outputs (`target/`)
- In `mvnPipeline`, `mvn package` runs in the **maven** container as **root**, so build
  outputs (e.g. `target/`, consumed by the buildpack build via
  `BP_MAVEN_BUILT_ARTIFACT=target/*.jar`) are root-owned. `packBuildContainer` runs in the
  **jnlp** container as the **jenkins** user on the SAME shared workspace volume and can't
  cleanly read them.
- **Fix (library `mvnPipeline`):** chown the outputs to jenkins BEFORE `packBuildContainer`,
  e.g. `container('maven') { sh 'chown -R jenkins target' }`.
- **Verify individually:** re-run a `mvnPipeline` app (`pd-sample-java-app`) emulation build.
- Applies to maven/JVM apps.

### 6b. Library-created maven-settings / git-credential binding files
- Observed on `pd-sample-go-app` (build #11), which uses `containerReleasePipeline`, NOT
  `mvnPipeline` — so 6a does NOT explain it. The `maven-settings` binding is created by the
  library itself (`configFileProvider` → `cp` into the binding dir); those files weren't
  readable by the jenkins/CNB user on the emulated arch. The `chmod -R a+rX` workaround is
  what let go-app get past bindings (builds #12/#13).
- **Fix (library `packBuildContainer`):** create the binding files (maven-settings and
  git-credential) OWNED BY / readable by the jenkins user by construction, rather than a
  blanket chmod.
- **Verify individually:** re-run a `containerReleasePipeline` app (`pd-sample-go-app`)
  emulation build with the `chmod -R a+rX` workaround REMOVED; bindings must be readable.

**Where.** Jenkins `jenkins-core-shared-libraries` (PLATFORM-1662 branches): `mvnPipeline`
(6a) and the binding-dir creation in `vars/packBuildContainer.groovy` (6b, the
`mkdir -p ... && cp ...` for maven-settings / git-credential in the buildkit multi-arch
path). The `jericop/pack` fork needs NO change for Item 6.

**Context.** Surfaced once the `-base` image fixed #1/#2. 6b found on go-app #11; 6a is the
maven/JVM analog. NOTE: `pd-sample-java-app` (mvnPipeline) emulation SUCCEEDED at ~435s in
the run set — so 6a may not always block (depends on which files the build reads and their
mode); still fix it so ownership is correct by construction rather than relying on umask.


---

## 7. [BLOCKER] Extra buildpacks + trusted builder → ephemeral builder create fails: `max depth exceeded`

**Symptom.** When the build adds extra buildpack modules (`--buildpack ...`) on top of a
trusted builder, the buildkit build fails:

```
Warning: Builder is trusted but additional modules were added; using the untrusted (5 phases) build flow
ERROR: failed to build: failed to write image to the following tags:
[pack.local/builder/<hex>:latest: loading image "pack.local/builder/<hex>:latest".
 first error: embedded daemon response: max depth exceeded]
```

Reproduced on `pd-sample-nodejs-app` build #3 (jenkins-pd) with the
`-with-history-and-kiro-base` image; its Jenkinsfile adds
`--buildpack docker.io/paketobuildpacks/nodejs:latest`.

**Cause.** Adding extra buildpack modules forces pack onto the "untrusted (5 phases)"
flow even though the builder is trusted, and that flow CREATES an ephemeral builder image
and loads it into the local docker daemon (`pack.local/builder/<hex>`). The daemon load
fails with `max depth exceeded` (overlay2 has a ~128 layer limit; the fork builder is
already deep, and the ephemeral-builder create adds more layers, exceeding it). Under the
buildkit backend this ephemeral-builder-via-daemon path should not be needed (the buildkit
backend runs the lifecycle from the builder image in LLB and can overlay extra buildpacks
in the build graph — see the `hasExtraBuildpacks` handling in `native_buildfunc.go`).

**Desired behavior.** With `--build-backend buildkit` and extra buildpacks on a TRUSTED
builder, do NOT fall back to the daemon ephemeral-builder create. Use the buildkit-native
extra-buildpacks overlay path (copy the staged modules over `/cnb/buildpacks` in LLB, which
the backend already supports) so no `pack.local/builder/...` image is created/loaded, and
the overlay2 depth limit is never hit.

**Where.** The trusted-vs-untrusted flow selection when extra buildpacks are present (pack
build orchestration), and the buildkit backend's extra-buildpacks handling
(`internal/build/multiplatform/native_buildfunc.go`, `hasExtraBuildpacks` /
`extraBuildpacksLocalName`).

**Context.** PLATFORM-1662 pd-sample-nodejs-app emulation build. Distinct from the QEMU-cgo
crash (#8); this one fails before the arch build even starts, during ephemeral-builder
creation.

---

## 8. [ENVIRONMENT, not a pack bug] QEMU-emulated cgo/gcc segfaults on the non-native arch

**Symptom.** With everything else fixed, a multi-arch build gets both arches through
detect and into the build phase; the NATIVE arch completes, but the EMULATED arch crashes
the toolchain:

```
#24 [linux/arm64] runtime/cgo: gcc: signal: segmentation fault (core dumped)
#24 [linux/arm64] failed to execute 'go build': exit status 1
lifecycle: builder ERROR: process "/cnb/lifecycle/builder ..." did not complete successfully: exit code: 51
```

Reproduced on `pd-sample-go-app` build #12 (linux/arm64 under QEMU on an amd64 agent).

**Cause.** This is QEMU user-mode emulation instability compiling the non-native arch: a
cgo build invokes an emulated `gcc` that segfaults. It is NOT a pack/lifecycle/builder bug
— it is the fundamental cost/risk of emulation for native-compilation workloads. (The
multi-agent native strategy avoids it by compiling each arch on its own native agent.)

**Mitigations / notes.**
- For pure-Go apps, `CGO_ENABLED=0` avoids invoking the emulated gcc entirely. VERIFIED:
  with `env.CGO_ENABLED='0'` set on the go-app emulation branch, build #13 SUCCEEDED
  (~379s, `ci.pipeline.run.result=SUCCESS`), vs build #12 (cgo enabled) which crashed with
  the emulated-gcc segfault. Not a general fix — apps that genuinely need cgo/native
  extensions can't just disable it.
- This is a KEY PLATFORM-1662 finding, not something to "fix" in pack: emulation is
  unreliable for native compilation; prefer native multi-agent for such workloads.

---

## 8b. [FIXED, uncommitted] Extra buildpacks on the buildkit backend must be delivered per-arch (multi-arch images) OR staged once (platform-agnostic/inline)

**Symptom.** With the buildkit backend and a multi-arch build (`--platform linux/amd64
--platform linux/arm64`), extra buildpacks added via `--buildpack` or the project
descriptor were staged ONCE from the host arch and copied to BOTH platform legs. So the
emulated arch leg ran host-arch buildpack binaries. Concretely, on `agent-patcher-service`
the arm64 leg ran amd64 `python3` from the cpython buildpack → `SIGTRAP` / `ENOENT` under
Jenkins emulation. Root cause: buildpack binaries are arch-specific, but only one arch's
tree was delivered.

**Fix (two coexisting delivery paths, buildkit-specific; single-arch daemon path
untouched).** Classify each extra buildpack and deliver it correctly:

- **Per-arch (multi-arch registry image = an OCI index).** Pull the per-platform CHILD
  image in LLB (`llb.Image(ref, llb.Platform(p))`) so each leg overlays its own
  arch-matching `/cnb/buildpacks/...`. `verifyBuildpackImageSupportsPlatforms` confirms the
  index covers every requested platform (else error early with a clear message).
- **Platform-agnostic (inline `project.toml` script buildpack, local dir/tarball URI,
  `urn:cnb:registry` ref, OR a single-manifest — non-index — registry image).** Stage ONCE
  (reusing `stageExtraBuildpacks`) and copy the SAME tree to every leg via a single
  `llb.Local(extraBuildpacksLocalName)`. Single-manifest registry images are ALLOWED as
  agnostic (NOT hard-errored): shell-script buildpacks using built-in `tar`/`cp` are
  legitimately arch-neutral; an old amd64-only buildpack used this way is the author's
  responsibility (it will fail at exec on the other arch).

**Classification signal (authoritative).** The fetched module's `Descriptor().Targets()`:
empty targets / `OS==""` / `Arch==""` ⇒ platform-agnostic (matches
`dist.BuildpackDescriptor.EnsureTargetSupport` "supports all", and `createInlineBuildpack`
emits an empty `dist.Target{}`). Concrete `{OS,Arch}` targets ⇒ that buildpack came from a
multi-arch image and is excluded from agnostic staging to avoid double-injection. Per-arch
images are derived from "PackageLocator whose manifest is an index"; agnostic staging =
`fetchedBPs` filtered to agnostic Targets. No locator string-matching.

**LLB overlay order in `buildEmitLLB`:** builder `/cnb/buildpacks` → agnostic staged tree
(same on every leg) → each per-arch image child (later overrides earlier).

**Verified end-to-end (local: Apple-Silicon host, amd64 emulated).**
- Multi-arch registry image (per-arch path): `agent-patcher-service` + `--buildpack`
  multi-arch cpython → arm64 leg `GOARCH=arm64`, arm64 python tarball, `pip --version`
  succeeded on both legs, `Finalized CNB metadata for manifest list`.
- Inline + agnostic (staged-once path) coexisting with per-arch registry buildpacks:
  `agent-patcher-service` built with `--descriptor inline-test-project.toml` (its real
  order: go-dist → build-plan → INLINE `registry-assember` → python), NO `--buildpack`.
  `add platform-agnostic buildpacks` vertex fired on BOTH legs; the inline
  `registry-assember` ran `go install rapid7.com/registry_assembler/` on BOTH `linux/amd64`
  and `linux/arm64`; the multi-arch registry buildpacks were delivered per-arch
  (`add user buildpack 1/3..3/3`); build reached `Finalized CNB metadata for manifest
  list`.

**Testing lesson.** `--buildpack` OVERRIDES the `project.toml` order entirely
(`processBuildpacks`: `declaredBPs := opts.Buildpacks`, only falls back to
`opts.ProjectDescriptor.Build.Buildpacks` when `opts.Buildpacks` is empty). To exercise a
descriptor-driven INLINE buildpack you must NOT pass `--buildpack`.

**Files (uncommitted, in this worktree).**
- `pkg/client/build.go` — classification + staging split (per-arch image list vs agnostic
  staged dir), `verifyBuildpackImageSupportsPlatforms` (index only; single-manifest ⇒
  agnostic, not error), `buildMultiPlatform` signature (+ agnostic dir),
  `collectBuildkitPerArchBuildpackImages` / `stageAgnosticExtraBuildpacks` /
  `moduleIsPlatformAgnostic`; import `ggcrremote`.
- `internal/build/multiplatform/native_buildfunc.go` — `buildEmitLLB` copies agnostic local
  + per-arch image children; `nativeBuildInputs` carries `hasAgnostic` +
  `extraBuildpackImages`.
- `internal/build/multiplatform/backend_native.go` — re-add agnostic local mount
  (`extraBuildpacksLocalName`) + pass per-arch images.
- `internal/build/multiplatform/backend.go` — signature plumbing.

**Remaining before spec/commit.**
- Revert instrumented cpython buildpack (troubleshooting only): repo
  `/Users/jpena/repos/jericop/cpython` branch `PLATFORM-1662-buildkit-emulation`, files
  `pl1662_diag.go`, `build.go`, `pip_cleanup.go` (the `[PL1662] cpython.Build` /
  `pip --version` log lines originate there, NOT in pack). No pack-side debug prints remain.
- Fold FR-8b (both delivery paths + agnostic/inline classification) into the spec at
  `.kiro/specs/platform-1662-buildkit-followups/` and add unit tests
  (`moduleIsPlatformAgnostic` classification; single-manifest ⇒ agnostic;
  index-missing-platform ⇒ error; overlay order).
- All pack changes above are UNCOMMITTED in worktree
  `/Users/jpena/.repos/jericop/cnb-pack/buildkit-native-export-with-history-and-kiro`.
