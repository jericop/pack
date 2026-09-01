---
inclusion: auto
---
# Benchmark workflows and how they map to the RFC performance tables

How the buildkit multi-arch fork measures build performance, which workflow produces
which RFC table, and how to read the results. All benchmark workflows live on the pack
fork's `fork-main` branch (dispatchable) and are mirrored on this history branch.

## The workflows

There are four benchmark workflows in `.github/workflows/`:

| Workflow | Trigger | Cells | Purpose |
|---|---|---|---|
| `benchmark-perf.yml` | `workflow_dispatch` | 5 apps × {docker-daemon-single, buildkit-single, buildkit-multi} = 15 | **The RFC performance matrix.** Source of RFC Tables 1 & 2. |
| `benchmark-dockerfile-vs-buildpacks.yml` | `workflow_dispatch` | 4 apps × {buildpacks-multi, cnb-like-dockerfile, generic-dockerfile} | buildpacks-overhead-vs-Dockerfile comparison. Source of RFC Table 3. |
| `benchmark-perf-smoke.yml` | `workflow_dispatch` | 1 app (default `go/mod`), daemon-single only | **Pre-flight smoke test only.** NOT an RFC data source. |
| `benchmark.yml` | push to `main` | Go `testing.B` micro-benchmark | Go-level micro-bench tracked by github-action-benchmark. Unrelated to the RFC build tables. |

Apps in `benchmark-perf.yml`: `go/mod`, `nodejs/npm`, `java/maven`, `java/java-node`,
`python/poetry`. `benchmark-dockerfile-vs-buildpacks.yml` covers 4 of these (java is out
of scope for that comparison; python/poetry is slow under QEMU but kept).

## Which workflow produces which RFC table

The RFC performance section (`cnb-rfcs .../0000-buildkit-multiarch-build.md`) has THREE
tables. Two of them come from ONE workflow:

1. **Table 1 — "Performance" (multi-arch buildkit)**
   Columns: `App | Cold | Rebuild | Speedup | Rebase`, 5 apps.
   Source: **`benchmark-perf.yml`**, the `buildkit-multi` cells. The RFC text states this
   explicitly ("Measured in CI (the `benchmark-perf.yml` workflow)").

2. **Table 2 — "Single-arch: buildkit backend vs the standard daemon build"**
   Columns: `App | daemon cold | daemon rebuild | buildkit cold | buildkit rebuild | cold Δ | rebuild Δ`, 5 apps.
   Source: **`benchmark-perf.yml`**, the `docker-daemon-single` + `buildkit-single` cells,
   pivoted side-by-side.

3. **Table 3 — "Multi-arch: buildpacks vs plain Dockerfiles"**
   Columns: `App | Build type | Cold | Rebuild`, 4 apps × 3 build types.
   Source: **`benchmark-dockerfile-vs-buildpacks.yml`**.

So: run **`benchmark-perf.yml`** to regenerate Tables 1 and 2, and
**`benchmark-dockerfile-vs-buildpacks.yml`** to regenerate Table 3.

### RFC tables are DERIVED from the raw workflow output

`benchmark-perf.yml` emits ONE combined table for all 15 cells:
`App | Build type | Cold (s) | Rebuild (s) | Rebase (s)`. The RFC author reshapes that
single table into the two presentations:

- Table 1 = the `buildkit-multi` rows, plus a computed **Speedup** column (cold ÷ rebuild).
- Table 2 = the `docker-daemon-single` and `buildkit-single` rows placed side-by-side,
  plus computed **Δ** columns (buildkit ÷ daemon).

The workflow does NOT emit the Speedup or Δ columns itself — those are added by hand when
transcribing into the RFC. Keep that in mind when refreshing the numbers: pull the raw
15-row table, then derive the two RFC tables from it.

## Common confusion: the smoke test is NOT an RFC source

`benchmark-perf-smoke.yml` runs a SINGLE app / SINGLE cell (`docker-daemon-single`,
default `go/mod`) on purpose — it is a quick check that the daemon-single publish path
works before spending ~15 runners on the full matrix. If you dispatch the smoke and see
"only one app," that is expected. It does not feed any RFC table. For the RFC's
multi-app tables you must run `benchmark-perf.yml` (5 apps) — not the smoke.

## How results are emitted (all workflows are consistent)

Every benchmark emits the SAME markdown table shape in THREE places:

1. the run's **Summary tab** in the web UI (`$GITHUB_STEP_SUMMARY`),
2. the step **log**, inside a `===== RAW MARKDOWN =====` block, and
3. an uploaded **`table.md` artifact**:
   - `benchmark-perf.yml` → `benchmark-perf-table`
   - `benchmark-dockerfile-vs-buildpacks.yml` → `dfbp-comparison-table`
   - `benchmark-perf-smoke.yml` → `benchmark-perf-smoke-table`

The step LOGS alone do not render a tidy table, so "no data" in `gh run view --log` is a
false alarm — the run succeeded and the numbers are in the Summary tab / artifact. The
multi-cell workflows use a separate `aggregate` job (each cell uploads a `bench-row-*`
CSV, then `aggregate` assembles the combined `table.md`); the smoke assembles its
one-row table inline.

## How to run + read them (zsh helpers in dot-kiro-files)

`~/tmp/dot-kiro-files/jericop-fork-functions.zsh` provides shortcuts. All dispatch from
`fork-main` and build the pack binary from `pack_ref` (default `buildkit-native-export`):

```bash
jericop_bench_perf                 # Tables 1 & 2 — full 5-app × 3-build-type matrix
jericop_bench_dockerfile           # Table 3 — buildpacks vs Dockerfiles
jericop_bench_smoke                # pre-flight only (one app)
jericop_bench_watch [perf|dockerfile|smoke]   # list recent runs (default perf)
jericop_bench_view  [perf|dockerfile|smoke]   # download + print the table.md artifact
```

`benchmark-perf.yml` can also build pack from a published image instead of source:
`jericop_bench_perf "" -f pack_image=jericop/pack:buildkit-native-export`.

Caveats:
- The full matrix runs 15 real multi-arch builds and can hit Docker Hub rate limits
  (`TOOMANYREQUESTS`) if run repeatedly — run it deliberately.
- Numbers are single-run CI values on GitHub-hosted amd64 runners (arm64 half runs under
  QEMU); treat cold builds as directional.

See `~/tmp/dot-kiro-files/benchmarks-runbook.md` for the full runbook.
