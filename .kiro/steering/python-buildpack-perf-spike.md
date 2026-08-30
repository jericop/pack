---
inclusion: manual
---

# Spike: why was the python/poetry buildpacks multi-arch build so slow? (ROOT CAUSE + FIX)

## TL;DR

It was a **real bug in our buildkit-native backend**: we did not advertise
`CNB_STACK_ID` / `CNB_TARGET_OS` / `CNB_TARGET_ARCH` (+ distro) to the lifecycle
phases running inside BuildKit. Without `CNB_STACK_ID`, the Paketo cpython
buildpack's dependency resolver (packit `postal`) could not match the
**stack-specific PREBUILT** CPython binary and fell back to the **wildcard-stack
SOURCE** dependency — i.e. it **compiled CPython from source** (~95s) instead of
extracting a prebuilt binary (~9–15s). Setting the env vars fixed it:

| | Installing CPython | Total (single-arch arm64 native) |
|---|---|---|
| before fix | ~95s (compile from source) | 198.8s |
| after fix  | ~9s (prebuilt extract)      | 101.0s |

So the earlier "buildpacks just do more/heavier work" explanation was WRONG. The
buildpack was doing the right thing; our backend was starving it of the target
metadata it needs to pick the prebuilt dependency.

## How it was diagnosed

1. Native (no-QEMU) spike showed pack ~1.8x the Dockerfile even without emulation,
   so QEMU was not the cause.
2. Verbose phase timing: the `builder` phase was 134s of 199s, and inside it
   `Installing CPython 3.14.7 → Completed in 1m35s` dominated.
3. Read the cpython buildpack source (added to workspace at
   `/Users/jpena/.repos/paketo-buildpacks/cpython`):
   - `build.go`: `if dependency.URI == dependency.Source { <compile from source> } else { <extract prebuilt> }`.
   - `installer.go`: the source path runs `configure` + `make` + `make altinstall`
     (a real compile).
4. `buildpack.toml` for 3.14.7 has SIX entries: 2 source (`stacks=["*"]`, python.org
   `.tgz`) and 4 prebuilt (`stacks=["io.buildpacks.stacks.jammy"|"...noble"]`,
   artifacts.paketo.io per-arch binaries). So a prebuilt noble arm64 binary DOES
   exist.
5. packit `postal.Resolve` (v2.25.7):
   - filters deps by `stacksInclude(dep.Stacks, stack)` AND
     `supportsPlatform(targetOs, targetArch, dep)`;
   - `targetOs/targetArch` come from `CNB_TARGET_OS`/`CNB_TARGET_ARCH` env (fallback
     to runtime.GOOS/GOARCH);
   - the sort DE-prioritizes wildcard `*` stacks, so a matching prebuilt entry always
     beats the source entry.
   - Therefore the only way source wins is if the prebuilt (stack-specific) entries
     were filtered out — i.e. the `stack` passed to Resolve was NOT
     `io.buildpacks.stacks.noble`. That value comes from `CNB_STACK_ID`.
6. Our LLB in `native_buildfunc.go` set only `CNB_PLATFORM_API`, `CNB_USER_ID`,
   `CNB_GROUP_ID` (+ optional `CNB_REGISTRY_AUTH`) — NO `CNB_STACK_ID` / target vars.
   The builder image DOES carry `CNB_STACK_ID` in its config env, but the buildpack's
   resolved `context.Stack` was not being populated from it in this path.

## The fix

- `internal/build/multiplatform/native_buildfunc.go`: add to the lifecycle RUN env:
  `CNB_TARGET_OS`, `CNB_TARGET_ARCH` (from the per-vertex platform `p`),
  `CNB_TARGET_ARCH_VARIANT` (if any), `CNB_STACK_ID`, `CNB_TARGET_DISTRO_NAME`,
  `CNB_TARGET_DISTRO_VERSION`.
- `nativeBuildInputs` gains `stackID`, `targetDistroName`, `targetDistroVersion`.
- `PlatformBuildOpts` (backend.go) gains `StackID`, `TargetDistroName`,
  `TargetDistroVersion`.
- `pkg/client/build.go` (`buildMultiPlatform`): populate those from the builder image
  labels — `io.buildpacks.stack.id`, and distro from
  `io.buildpacks.base.distro.{name,version}` (fallback `io.buildpacks.stack.distro.*`).

Verified: `Installing CPython` dropped to ~9s and total single-arch build to 101s.
Package tests + vet pass.

## Implications

- This likely helped OTHER buildpacks too (JRE/Liberica, node-engine, go-dist all
  publish stack/target-specific prebuilt deps). Any dependency with a
  source-vs-prebuilt split would have hit the same fallback.
- The dockerfile-vs-buildpacks RFC numbers taken BEFORE this fix overstate the
  buildpacks cold time (they include the CPython source compile). Re-run the CI
  comparison after this fix ships and refresh the RFC; the buildpacks column should
  drop noticeably for python (and somewhat for the others).
- Version note: with the app's `python = "^3.10"` the buildpack selects the newest
  (3.14.7). That's fine now that prebuilt is used; pinning would only matter for an
  exact apples-to-apples version match with the Dockerfile's 3.12.


## Follow-up: full CNB + user env parity (not just CNB_STACK_ID)

The CNB_STACK_ID gap was one symptom of a broader issue — the buildkit-native path
was not passing the full environment that standard pack passes. Fixed to match pack:

- **CNB platform env**: CNB_PLATFORM_API, CNB_USER_ID, CNB_GROUP_ID, CNB_STACK_ID,
  CNB_TARGET_OS, CNB_TARGET_ARCH, CNB_TARGET_ARCH_VARIANT, CNB_TARGET_DISTRO_NAME,
  CNB_TARGET_DISTRO_VERSION, CNB_EXPERIMENTAL_MODE, SOURCE_DATE_EPOCH,
  CNB_REGISTRY_AUTH.
- **Proxy vars** (UPPER + lower): HTTP_PROXY/HTTPS_PROXY/NO_PROXY — resolved via
  Client.processProxyConfig (explicit opts, else host env), matching
  WithLifecycleProxy.
- **User build env** (pack --env / --env-file + project.toml [[build.env]]): written
  as files under /platform/env/<NAME> in the LLB (the CNB platform contract), so
  buildpacks read them as BP_* configuration. Verified: `--env BP_CPYTHON_VERSION=3.12.*`
  makes the cpython buildpack select 3.12.13 instead of 3.14.7.

Wiring: PlatformBuildOpts (BuildEnv, ExperimentalMode, SourceDateEpoch, HTTPProxy/
HTTPSProxy/NoProxy, StackID, TargetDistro*) -> nativeBuildInputs -> the lifecycle RUN
env / a /platform/env write step in native_buildfunc.go. Values sourced in
buildMultiPlatform (pkg/client/build.go) from opts + the builder image labels.

Not applicable to the publish-only buildkit path (intentionally skipped):
CNB_USE_LAYOUT / CNB_LAYOUT_DIR (OCI-layout local export only).
