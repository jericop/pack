---
inclusion: manual
---

# PLATFORM-1662 benchmark data: how to look up builds, logs, and branches

Reference for investigating the `jericop/pack` BuildKit fork issues found during the
Rapid7 PLATFORM-1662 multi-arch build performance comparison. Use this to reproduce a
failing build, read its logs, or re-run the comparison after a fork fix. (See the spec
`.kiro/specs/platform-1662-buildkit-followups/` and `FOLLOWUPS.md` for the issues.)

## What the comparison is

Two multi-arch (`linux/amd64,linux/arm64`) build strategies, compared per app:
- **multi-agent (native):** branch `PLATFORM-1662-multi-agent` — each arch builds on its
  own native Jenkins agent; manifest list assembled after. Baseline.
- **buildkit-emulation:** branch `PLATFORM-1662-buildkit-emulation` — one agent, one
  `pack build --build-backend buildkit --platform linux/amd64 --platform linux/arm64`,
  QEMU for the non-native arch. Drives the fork `pack`.

## The fork pack binary the emulation builds use

- The Jenkins library COPIES the `pack` binary out of a published fork IMAGE at build time
  (`docker create <img>` + `docker cp <cid>:/usr/local/bin/pack`), selected by
  `env.PACK_FORK_IMAGE`.
- Iteration image (bug fixes): `jericop/pack:buildkit-native-export-with-history-and-kiro-base`.
- Default/stable image: `jericop/pack:buildkit-native-export`.
- Builder image: `jericop/ubuntu-noble-builder:buildkit-native-export` (bundles the modified
  lifecycle; version 0.0.0 → must be trusted, see Item 3). Emulation passes
  `--build-backend buildkit --builder <that> --trust-builder` via `env.ADDITIONAL_PACK_ARGS`.
- To ship a fork fix into the pipeline: publish a new fork image tag and point
  `env.PACK_FORK_IMAGE` at it on the emulation app branches (no library change needed).

## Participating app repos (all rapid7 org, all just `pack build`ed)

`-lambda` is only a repo NAME (a Python service); there is no AWS-Lambda build path.

| repo | language / build | Jenkins instance |
|------|------------------|------------------|
| pd-sample-go-app | Go (containerReleasePipeline) | jenkins-pd |
| pd-sample-java-app | Java/Maven (mvnPipeline) | jenkins-pd |
| pd-sample-nodejs-app | Node (containerReleasePipeline; extra `--buildpack`) | jenkins-pd |
| pd-sample-python-app | Python/poetry | jenkins-pd |
| agent-patcher-service | Python (also spawns sub-build `patcher-collection-cleanup-lambda`) | jenkins-asgard |

EXCLUDED: `pd-rds-postgres-password-lambda` (Python) — depends on PyGreSQL, which needs the
postgres client installed; that client is not in the patched noble builder, so it can't
build under emulation. Builder/app-dependency limitation, not an emulation/fork issue.
Dropped from the set (participating apps: 5).

Each repo has two comparison branches: `PLATFORM-1662-multi-agent` and
`PLATFORM-1662-buildkit-emulation`. The emulation branch has the `@Library(
'jenkins-core-shared-libraries@PLATFORM-1662-buildkit-emulation') _` override,
`env.ARCH='linux/amd64,linux/arm64'`, `env.PACK_FORK_IMAGE`, and the buildkit
`env.ADDITIONAL_PACK_ARGS`. Push a commit to a branch to trigger its build.

## The Jenkins shared library (emulation packBuildContainer)

- Repo: `rapid7/jenkins-core-shared-libraries`, branch `PLATFORM-1662-buildkit-emulation`
  (git@github.com:rapid7/jenkins-core-shared-libraries.git).
- `vars/packBuildContainer.groovy` (multi-arch path) is what invokes the fork pack:
  copies the fork binary out of `PACK_FORK_IMAGE`, creates+bootstraps a `docker-container`
  buildx builder named `pack-multiplatform` (via `containerReleaseFunctions.prepareMultiarchBuilder`
  then `docker buildx inspect --bootstrap`), `chmod -R a+rX`s the binding dirs (Item 6
  workaround), and runs `pack-bin build <ttlsh> --platform ... --buildkit-builder
  pack-multiplatform --publish --binding ... ${ADDITIONAL_PACK_ARGS}`.
- Local worktrees used during the work (Rapid7 dev machine):
  `/Users/jpena/.repos/r7/_pl1662_worktrees/<app>-{multi-agent,buildkit-emulation}` and the
  library at `/Users/jpena/.repos/r7/jenkins-core-shared-libraries/PLATFORM-1662-buildkit-emulation`.

## Reading build results + logs

### Grafana (primary; works for BOTH Jenkins instances, no Jenkins auth needed)
- All Jenkins instances export OpenTelemetry traces to the same Grafana. Tempo datasource
  uid: `kubernetes-traces`.
- Search builds (newest-first) via the datasource proxy:
  ```
  GET /api/datasources/proxy/uid/kubernetes-traces/api/search
      ?q={ span.ci.pipeline.id =~ "<app>.*PLATFORM-1662-<strategy>" && span.type = "job" }
      &start=<unixSec>&end=<unixSec>&limit=50
  ```
  Returns `rootServiceName` (which Jenkins instance: `jenkins-pd` or `jenkins-asgard`),
  `durationMs`, `traceID`, `startTimeUnixNano` (sort desc for newest).
- Hydrate one build for authoritative result:
  ```
  GET /api/datasources/proxy/uid/kubernetes-traces/api/traces/<traceID>
  ```
  On the root `BUILD <app>/<branch>` span read: `ci.pipeline.run.result`
  (SUCCESS/FAILURE), `ci.pipeline.run.durationMillis` (wall-clock), `ci.pipeline.run.url`
  (the Jenkins run URL). `status.code` = STATUS_CODE_OK/ERROR.
- Loki logs datasource: `kubernetes-logs-prod` (secondary; the per-build console is easier
  via the Jenkins MCP below for jenkins-pd).

### Jenkins MCP (jenkins-pd only)
- The `jenkins-pd` MCP can read jenkins-pd builds: `getjob` (tree
  `builds[number,result,building,timestamp]`), `getbuild`
  (tree `number,result,building,durationMillis`), `getbuildlog` (paged with limit/cursor),
  `searchbuildlog`. `agent-patcher-service` runs on `jenkins-asgard` — NOT reachable by the
  jenkins-pd MCP; use Grafana for it.
- The `.../consoleText` HTTP endpoint requires auth (returns a 403 login-redirect stub);
  do not rely on it.
- BEST PRACTICE for troubleshooting: download the FULL console log ONCE into a local file
  (`getbuildlog` paged until `hasMoreContent` is false) and grep the file — do not make
  many `searchbuildlog` calls (buildkit progress `DONE` spam makes live search noisy).

## Reproducing locally (fastest iteration)

Prefer local reproduction over Jenkins for fork-code iteration:
- Build the fork `pack` from source, run a sample app to a LOCAL registry, and compare a
  cold build vs a rebuild. See steering `mvp-build-testing-strategy.md`,
  `local-registry-testing.md`, and `local-test-environment.md`.
- The nested-layout repro for Item 2 is `pd-sample-python-app` (has `src/<pkg>/` +
  `project.toml` include filter). Note that fixture currently has a stale `poetry.lock`;
  for a clean full build use the `paketo-buildpacks/samples` apps.
