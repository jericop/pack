# BuildKit-native (Option A) benchmark harness

`benchmark.sh` drives the real `pack` binary with the experimental
`--build-backend buildkit-native` backend against a matrix of buildpacks sample
apps and measures, per app:

- **cold build** wall time (no BuildKit cache),
- **rebuild** wall time (identical command; warm cache),
- **rebase** wall time (swap the run image; metadata-only),
- a **cache signal** (count of BuildKit `CACHED` vertices on the rebuild), and
- the finalized image's **layer count**.

It emits a Markdown table (and a CSV) plus per-step logs. "Wall time" means real
elapsed seconds — see `.kiro/steering/mvp-build-testing-strategy.md`.

This harness is the fork's Option A performance check. It is SEPARATE from the
upstream Go micro-benchmark in `benchmarks/build_test.go` (which measures pack CLI
overhead, not real multi-language BuildKit-native builds).

## Why these apps

The default matrix covers meaningfully different layer profiles so results are
interpretable:

| App                | Exercises                                             |
|--------------------|-------------------------------------------------------|
| `python/poetry`    | interpreted app + dependency layer (Poetry)           |
| `nodejs/npm`       | interpreted app + `node_modules` dependency layer     |
| `java/maven`       | JVM app; larger JRE + built artifact layers           |
| `java/java-node`   | multi-language (JRE **and** Node) — most layers       |
| `go/mod`           | compiled Go app with module deps; small runtime image |

The no-materialization change (copy layers by reference instead of extracting
tars) should show its biggest wins on the larger multi-layer apps (`java/*`).

## Run locally

Prereqs match the MVP local-testing steering (local registry on `localhost:5050`,
a `pack-multiplatform` buildx builder, a built `pack` binary, and — for layer
counts — `crane`).

```bash
PACK_BIN=/tmp/pack-poc-optA \
SAMPLES_DIR=/Users/jpena/.repos/paketo-buildpacks/samples \
LIFECYCLE_IMAGE=pack-local-registry:5000/lifecycle:native-updated \
REGISTRY_PUSH=pack-local-registry:5000 \
REGISTRY_HOST=localhost:5050 \
  internal/build/multiplatform/benchmarks/benchmark.sh
```

Restrict the matrix or platforms while iterating:

```bash
BENCH_APPS="python/poetry" PLATFORMS="linux/arm64" \
  internal/build/multiplatform/benchmarks/benchmark.sh
```

## Run in GitHub Actions

The workflow `.github/workflows/buildkit-native-benchmark.yml` runs this harness
and uploads the Markdown table, CSV, and logs as a build artifact. It is
`workflow_dispatch` (manual) so it does not run on every push.

## Configuration

All inputs are environment variables with defaults (see the header of
`benchmark.sh`): `PACK_BIN`, `SAMPLES_DIR`, `BENCH_APPS`, `REGISTRY_PUSH`,
`REGISTRY_HOST`, `PACK_HOST_REGISTRY_REMAP`, `BUILDER`, `LIFECYCLE_IMAGE`,
`RUN_IMAGE`, `REBASE_RUN_IMAGE`, `PLATFORMS`, `BUILDKIT_BUILDER`, `OUT_DIR`,
`DO_REBASE`.

## Output

- `benchmark-out/benchmark-table-<ts>.md` — the Markdown table.
- `benchmark-out/benchmark-table-<ts>.csv` — same data as CSV.
- `benchmark-out/logs/<app>-<cold|rebuild|rebase>.log` — full per-step output.
