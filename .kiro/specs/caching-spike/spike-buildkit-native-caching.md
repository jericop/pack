# Spike: caching in the buildkit-native backend (cnbp comparison + rebuild behavior)

> **STATUS — analysis spike.** Grounded in the cnbp source
> (`/Users/jpena/.repos/EricHripko/cnbp`), our current implementation
> (`internal/build/multiplatform/native_buildfunc.go`), and real rebuild logs under
> `/tmp/kiro-command-logs/final-benchmark-out/logs/`. No new builds were run for this
> spike; the empirical section cites the existing final-benchmark rebuild logs.

## Questions

1. cnbp mentions a *custom cacher* — how did its caching work, and how does that
   compare to our current cache?
2. On a rebuild with **unchanged app source**, does BuildKit skip buildpack execution
   entirely (a cache hit on the phase RUNs)?

## TL;DR

- **Yes — on an unchanged-source rebuild, buildpack execution is skipped.** BuildKit
  marks the `detector`, `restorer`, `builder`, and `exporter` RUN vertices `CACHED`
  and does not execute them. The `builder` (buildpacks) phase being `CACHED` is the
  direct evidence. Only the `analyzer` may re-run (~1.5–2s) because it inspects the
  remote target image, which is outside BuildKit's cache key.
- **We do NOT need cnbp's custom cacher.** cnbp shipped a `cacher` binary only because
  it *replaced* the lifecycle exporter (which is what normally writes the buildpack
  cache). We keep the real exporter (in emit-mode), so the standard lifecycle cache
  writing happens for free — no extra binary.
- There are **three independent cache layers**, and it helps not to conflate them:
  the BuildKit vertex cache (skips whole phases), the lifecycle buildpack cache mount
  (`/cache`, used when the builder actually runs), and the BuildKit registry cache
  (`--buildkit-cache-from/to`, for ephemeral CI).

## Background: the three cache layers

| Layer | What it is | When it helps | Effect |
|---|---|---|---|
| **BuildKit vertex cache** | Each lifecycle phase is a BuildKit `RUN`; BuildKit content-addresses each vertex by its inputs (parent FS state incl. the copied-in app source). | Rebuild where an input is unchanged. | The `RUN` is `CACHED` and **does not execute** — e.g. buildpacks don't run at all. |
| **Lifecycle buildpack cache** | A persistent cache mount at `/cache` (`llb.AsPersistentCacheDir`), mounted on analyzer/restorer/exporter; the lifecycle stores/reuses buildpack layers (downloaded deps, compiled artifacts) there. | When the builder phase *does* run (e.g. source changed). | Buildpacks run but reuse their cached layers instead of redoing work. |
| **BuildKit registry cache** | `--buildkit-cache-from` / `--buildkit-cache-to type=registry`. | Ephemeral/cold CI runners with no local BuildKit state. | Import a warm vertex cache from a registry so the first build behaves like a rebuild. |

On a pure no-change rebuild, the vertex cache short-circuits everything, so the
`/cache` mount is not even exercised. When source changes, the vertex cache misses on
the affected phases, the builder runs, and the `/cache` mount makes that run fast.

## Q1 — cnbp's custom cacher vs our cache

### How cnbp cached (source-verified)

cnbp used the same BuildKit primitive we do — a persistent cache mount at `/cache`:

```go
// cmd/cnbp-frontend/frontend.go
cache := llb.AddMount(
    cnbp2llb.CacheDir,               // "/cache"
    llb.Scratch().File(/* mkdir /cache 0755 chown uid:gid */),
    llb.SourcePath(cnbp2llb.CacheDir),
    llb.AsPersistentCacheDir("buildpacks-cache", llb.CacheMountPrivate),
)
```

That `cache` RunOption is attached to `Analyze` and `Restore` (see
`pkg/cnbp2llb/analyze.go`, `restore.go`). So far, identical in spirit to ours.

The difference is the **custom `cacher` binary** (`cmd/cacher/main.go`), run as a
final LLB step in `Export` ("Populating cache"):

```go
// pkg/cnbp2llb/export.go
built = built.Run(
    llb.Args([]string{"/frontend/go/bin/cacher"}),
    llb.WithCustomName("Populating cache"),
    llb.AddMount("/frontend", llb.Image("erichripko/cnbp")), // inject the binary
    cache,
).Root()
```

`cmd/cacher/main.go` re-implements cache export: it reads `group.toml`, builds a
`lifecycle.Exporter`, opens a `cache.NewVolumeCache("/cache")`, and calls
`exporter.Cache(LayersDir, cacheStore)` — i.e. it manually runs *just the cache-export
half* of the lifecycle.

### Why cnbp needed it (and why we don't)

cnbp **replaced the lifecycle exporter** with hand-written LLB (its `Export()` builds
the image via `llb.Copy` and never calls `/cnb/lifecycle/exporter`). But writing the
buildpack cache is normally the *exporter's* job. By dropping the exporter, cnbp lost
cache writing — so it bolted a standalone `cacher` back on to recover it. The comment
in `export.go` is explicit that this is a workaround, and that it must inject the
binary via a mounted image because BuildKit offered no way to supply it from the
frontend (references moby/buildkit#2063).

Our design does the opposite: **we keep the real lifecycle exporter**, run in
emit-mode (`-emit-export-plan`). The exporter still performs its normal cache write to
the `/cache` mount as part of running — so there is nothing to re-implement and no
`cacher` binary. This is a concrete, if small, dividend of the emit/finalize approach
over a custom frontend: not replacing the exporter means not re-creating the pieces of
it (cache export, SBOM, metadata, process types) that a replacement throws away.

### Side-by-side

| | cnbp | current (buildkit-native) |
|---|---|---|
| Buildpack cache primitive | `AsPersistentCacheDir("buildpacks-cache", Private)` at `/cache` | `AsPersistentCacheDir("cnb-buildpacks-cache-<arch>", Shared)` at `/cache` |
| Mounted on | analyze, restore (+ cacher step) | analyzer, restorer, exporter |
| Who writes the cache | a **custom `cacher` binary** (because the exporter was replaced) | the **real lifecycle exporter** (emit-mode) — no extra binary |
| Per-arch cache isolation | single id | id suffixed with `-<arch>` |
| Phase-skip on rebuild | BuildKit vertex cache (same primitive) | BuildKit vertex cache (verified below) |
| Registry cache | not wired | `--buildkit-cache-from/to type=registry` |

Two minor current-implementation notes: the cache mount id is per-arch
(`cnb-buildpacks-cache-<arch>`) so amd64/arm64 don't collide; and the initial "fix
cache mount permissions" `chmod` uses `llb.IgnoreCache` so it always runs (permissions
are cheap to reassert and must be correct before the phases use the mount).

## Q2 — does an unchanged-source rebuild skip buildpack execution? (verified)

**Yes.** From the go/mod warm rebuild in the final benchmark
(`final-benchmark-out/logs/go-mod-20260830-042342-rebuild.log`), the lifecycle phase
vertices are `CACHED`:

```
#17 [linux/arm64] lifecycle: detector CACHED
#18 [linux/arm64] lifecycle: restorer CACHED
#19 [linux/arm64] lifecycle: builder  CACHED      <-- buildpacks do NOT run
#20 [linux/arm64] lifecycle: exporter (emit-mode) CACHED
```

- `detector` / `restorer` / `builder` / `exporter` are all `CACHED` (25 such cached
  phase vertices across both arches in that one rebuild).
- None of `detector` / `builder` / `exporter` print a `DONE <time>` line — an actual
  execution prints `DONE`, a cache hit does not — confirming they were served from
  cache, not re-run.
- Even the per-layer assembly `llb.Copy`s are `CACHED`
  (`assemble layer (copy): ... CACHED`).

### Why

Each phase is a separate `RUN` vertex whose BuildKit cache key derives from its inputs,
principally the parent filesystem state — which includes the copied-in app source
(`copy app source CACHED` appears too). When the source is byte-identical, every
downstream vertex's key is unchanged, so BuildKit reuses the cached result and skips
execution. We get this for free by modeling the phases as `RUN`s; it is the same
mechanism that makes an unchanged Dockerfile `RUN` `CACHED`.

### The one nuance: the analyzer

The `analyzer` does not always cache. In the same logs it appears both as
`analyzer DONE 1.5s` (executed) and `analyzer CACHED`. The analyzer inspects the
**remote target image** (registry state), which is outside BuildKit's cache key, so
BuildKit cannot assume it is unchanged and may re-run it. It is cheap (~1.5–2s), so it
does not materially affect the ~7.6s go/mod rebuild vs the ~164s cold build.

**Precise statement:** on an unchanged-source rebuild, the buildpack-running phases
(detect/restore/build) and the exporter are skipped via BuildKit's vertex cache;
the analyzer may still run briefly because it reads external registry state.

## Implications / suggested follow-ups

- **RFC/steering wording:** our docs currently frame the cache as "layer cache +
  registry cache." The stronger, concrete claim — *buildpack execution is skipped
  entirely on an unchanged-source rebuild* (builder phase `CACHED`) — is worth stating
  explicitly, since it is the main reason rebuilds are single-digit seconds. Suggest
  adding a short "Caching" subsection distinguishing the three layers above.
- **cnbp comparison for the RFC Prior Art:** note that cnbp's custom `cacher` was a
  consequence of replacing the exporter; keeping the exporter (emit-mode) removes the
  need for it. This reinforces the "keep lifecycle fidelity" argument already in the
  RFC.
- **No code change proposed.** Caching works as intended; this spike is documentation
  + confirmation only.
