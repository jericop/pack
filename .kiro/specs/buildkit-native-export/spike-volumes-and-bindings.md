# Spike: `--volume` and CNB bindings in the `buildkit` backend

Status: research / spike (read-only investigation). No behavior changes proposed
here beyond a recommendation for how to treat `--volume` and bindings on the
`buildkit` build backend.

## Overview

The `buildkit` build backend runs the CNB lifecycle phases (analyzer, detector,
restorer, builder, exporter-in-emit-mode) as BuildKit LLB `RUN` steps inside a
sandboxed solve, then assembles the final image FROM the run image by copying the
emitted layer sources (`internal/build/multiplatform/native_buildfunc.go`). It is
publish-only, multi-arch, and pushes natively via BuildKit's image exporter
(`internal/build/multiplatform/backend_native.go`).

The `docker-daemon` backend instead runs each lifecycle phase in a real container
via the Docker daemon, and user `--volume` mounts are attached to those containers
as Docker bind mounts (`internal/build/lifecycle_execution.go` +
`internal/build/phase_config_provider.go`).

This spike answers:

1. What does `--volume` actually do today (docker-daemon path)?
2. What are "bindings" and how are they delivered today?
3. Can either be faithfully supported under BuildKit's sandboxed model, and if so
   how / with what tradeoffs?

Key up-front finding: **bindings are not a first-class concept in either fork.**
Neither `jericop/cnb-pack` nor `jericop/cnb-lifecycle` has any binding-specific
code path — bindings are purely a CNB *platform-directory convention* that users
deliver by pointing a `--volume` at the platform bindings directory. So the two
topics collapse into one mechanism today: **`--volume`**.

## What `--volume` does today (docker-daemon backend)

### Flag registration and options plumbing

- Flag defined: `internal/commands/build.go:61` (`Volumes []string` on
  `BuildFlags`) and registered at the bottom of `buildCommandFlags`
  (`internal/commands/build.go`, the `cmd.Flags().StringArrayVar(&buildFlags.Volumes, "volume", ...)`
  line). Help text documents the form `'<host path>:<target path>[:<options>]'`
  with options `ro` (default), `rw`, and `volume-opt=<key>=<value>`.
- An untrusted-builder warning is emitted when volumes are combined with an
  untrusted builder: `internal/commands/build.go:154-156` ("Using untrusted
  builder with volume mounts... this may present a security vulnerability").
- Volumes are copied into `client.BuildOptions.ContainerConfig.Volumes`:
  `internal/commands/build.go:219-222` (`ContainerConfig: client.ContainerConfig{ Network:..., Volumes: flags.Volumes }`).
- `ContainerConfig.Volumes` is documented in `pkg/client/build.go:290-299`: form
  `/path/in/host:/path/in/container`, and "strongly recommended you do not
  override any of the paths ... `/cnb`, `/layers`, anything below `/cnb/**`".

### Parsing / validation / normalization

- `processVolumes` is called in `pkg/client/build.go:633` and its result stored on
  the lifecycle-exec options as `Volumes: processedVolumes`
  (`pkg/client/build.go:688`).
- Platform-specific implementations:
  - Linux/Windows: `pkg/client/process_volumes.go:16` uses Docker's
    `docker/docker/volume/mounts` parser (`ParseMountRaw`). Default mode is `ro`
    (`processMode`, `process_volumes.go:47-53`).
  - macOS/other unix: `pkg/client/process_volumes_unix.go:16` uses
    `docker/cli/compose/loader.ParseVolume`; `parseVolume`
    (`process_volumes_unix.go:40-54`) rejects a 3rd field that is not `ro`, `rw`,
    or `volume-opt=...`. Default mode is `ro`; `:rw` only honored if explicitly
    set.
- Reserved / sensitive mount targets: both implementations WARN (do not reject)
  when the target is under `/cnb`, `/layers`, or `/workspace`
  (`process_volumes.go:32-40`, `process_volumes_unix.go:22-27`). Tests assert the
  warning for targets including `/cnb`, `/cnb/buildpacks`, `/layers`, `/workspace`,
  and notably **`/workspace/bindings`** (`pkg/client/build_test.go:2962-2988`).
- Normalized form handed to the lifecycle executor is `source:target:mode`
  (mode defaulting to `ro`).

### Which phases receive the user volumes, and how

User volumes reach the lifecycle phase containers via `WithBinds(l.opts.Volumes...)`
in `internal/build/lifecycle_execution.go`:

- **creator** (trusted single-container path): `lifecycle_execution.go:425` and
  `:427` (as `cacheBindOp`, appended alongside the cache bind).
- **detector**: `lifecycle_execution.go:498`.
- **builder**: `lifecycle_execution.go:781`.
- **extender (build)**: `lifecycle_execution.go:798`.
- **layout** op path: `lifecycle_execution.go:585`.

`WithBinds` appends onto the container `HostConfig.Binds`
(`internal/build/phase_config_provider.go:65-68` shows the base binds — the
layers volume and app volume — and `WithBinds` adds user volumes on top). The
`HostConfig` is a real Docker `container.HostConfig`
(`phase_config_provider.go:27,38,121-123`), i.e. these become genuine Docker bind
mounts on the phase containers.

Note the analyzer/restorer/exporter phases are NOT given the user volumes; user
volumes are primarily a **detect/build-time** affordance (plus the trusted
`creator` which runs everything in one container).

### Semantics (what a user volume is *for*)

A `--volume host:container[:ro|rw]` mounts a host path into the lifecycle phase
container so buildpacks (running in detect/build) can read (and, with `:rw`,
write) host data. Typical uses: injecting CA certs, private dependency caches,
credentials/config, or **bindings** trees. Default is read-only; read-write is
opt-in. The host path is live — with `:rw` a buildpack's writes land back on the
host filesystem.

## What bindings are, and how they are delivered today

### The CNB concept

In the CNB platform spec, **service bindings** are a directory tree the platform
exposes to buildpacks: under a bindings root, one subdirectory per binding, each
containing a `type` file (and optional `provider`) plus secret/config files
(key = filename, value = file contents). Buildpacks discover them via the
platform's bindings directory (historically `<platform>/bindings`, e.g.
`/platform/bindings`, and via env such as `CNB_BINDINGS`/`SERVICE_BINDINGS` in
older conventions). They are the CNB-native way to pass credentials/config (e.g. a
private registry cert, a maven settings binding) into a build.

### How pack/lifecycle handle bindings today: they don't (explicitly)

Exhaustive search of both repos found **no first-class bindings support**:

- `jericop/cnb-lifecycle`: grep for `binding` across all `*.go` → **no matches**.
- `jericop/cnb-pack`: grep for `binding` / `CNB_BINDINGS` / `SERVICE_BINDINGS` /
  `platform/bindings` / `ServiceBinding` across all `*.go` → the only hits are
  unrelated (Docker *port* bindings in test/acceptance helpers, and an SSH-dialer
  comment referencing podman's `bindings` package). The one meaningful reference
  is a test that asserts the sensitive-directory warning for a volume target of
  `/workspace/bindings` (`pkg/client/build_test.go:2971`).

So today, **bindings are delivered as data via `--volume`**: a user mounts a host
bindings tree at the platform bindings path, e.g.
`--volume /host/bindings:/platform/bindings:ro` (or `/workspace/bindings`), and the
lifecycle/buildpacks read it as ordinary files. There is no dedicated
`--binding`-style flag or binding parser; pack only knows about the generic volume
mount, and the CNB spec directory layout does the rest.

### Relationship between `--volume` and bindings

Bindings are a *consumer* of the `--volume` mechanism. `--volume` is the transport;
a bindings tree is one particular payload delivered over it, mounted at the
platform's bindings directory (read-only in practice). Any BuildKit story for
bindings is therefore mostly a BuildKit story for "get a read-only data/secret tree
to a known path during detect/build."

## BuildKit primitives available (buildkit module `v0.32.2`)

From `go.mod:24` — `github.com/moby/buildkit v0.32.2`. What the current backend
already uses, and what else is available:

Already used by the backend:
- `llb.Local("context")` — the app source is synced in as a local context
  (`native_buildfunc.go:88-89,455`), wired via `SolveOpt.LocalMounts`
  (`backend_native.go:159-161`, `contextLocalName: appFS`).
- Persistent cache mount — `llb.AddMount("/cache", llb.Scratch(), llb.AsPersistentCacheDir(...))`
  (`native_buildfunc.go`, `cacheMount`). Content-addressed build cache, not a data
  channel.
- Session attachables — currently just an auth provider
  (`backend_native.go:123,162`; `buildkit_client.go:184-187`
  `authprovider.NewDockerAuthProvider`).
- `SolveOpt` fields in play: `LocalMounts`, `Session`, `FrontendAttrs`,
  `CacheImports`/`CacheExports`, `AllowedEntitlements` (network.host), `Exports`
  (`backend_native.go`).

Available but NOT yet wired:
- `llb.AddSecret(dest, opts...)` and `llb.AddSecretWithDest(...)` —
  confirmed present at `client/llb/exec.go:767` and `:779` in
  `moby/buildkit@v0.32.2`. Secret mounts expose a secret at a path for the duration
  of a single `RUN`; the bytes are provided host-side by a session secrets
  provider (`session/secrets`, present in the module) and are **not** persisted
  into any layer.
- `llb.AddSSHSocket(opts...)` — `client/llb/exec.go:710`. For forwarding an ssh
  agent socket to a `RUN`.
- `llb.AddMount(dest, state, opts...)` — `client/llb/exec.go:704`. Mounts another
  LLB **state** (e.g. an `llb.Local` or `llb.Image`) at a path in a `RUN`. This is
  the closest thing to a bind mount, but the source is a BuildKit state, not an
  arbitrary live host directory.

Fundamental BuildKit constraint: a solve runs in a **sandbox**. There is no
equivalent of `docker run -v /host:/container` where the container reads/writes a
live host path. Inputs must enter as (a) a synced local context (`llb.Local`),
(b) a secret/ssh session channel, or (c) another LLB state. Outputs only leave via
the exported image or explicitly read files (`ref.ReadFile`), never by writing back
to a host path mid-build.

Backend capabilities already anticipate secrets:
`BackendCapabilities.SupportsSecretMounts` is declared `true` for the buildkit
backend (`backend_native.go:57`, `backend.go:264-266,306`) — but nothing is wired
to consume it yet.

## Feasibility for `--volume`

Assessment: **cannot be faithfully reproduced; partial support only, and it should
be rejected-with-guidance for the MVP.**

Break `--volume` into its two modes:

- **Read-write host volumes (`:rw`)** — NOT possible under BuildKit. There is no
  primitive that lets a sandboxed `RUN` mutate a live host directory. Buildpacks
  that expect to write back to a `:rw` host path (e.g. populating a host-side
  dependency cache the user reuses across builds) fundamentally cannot be honored.
  BuildKit's own `--mount=type=cache` covers *in-BuildKit* caching (the backend
  already does this for `/cache`), but that is not the same as writing to the
  user's host filesystem.

- **Read-only data volumes (`:ro`)** — technically reproducible by syncing the host
  dir as an additional `llb.Local` and either `llb.AddMount`-ing it onto the
  detect/build `RUN`s or `llb.Copy`-ing it to the target path. This would require:
  1. adding an entry per volume to `SolveOpt.LocalMounts`,
  2. threading target paths into `nativeBuildInputs`, and
  3. mounting/copying at the same target for the detector + builder RUNs.
  Tradeoffs: the data is *synced into the build* (transfer cost proportional to
  size; not a live view), and if copied (not mounted) it can land in an
  intermediate state and potentially a layer. Mount (not copy) avoids layer
  persistence but the mount only exists for that one `RUN`.

Because the flag's documented default and common uses include host caches and
credentials, and because `:rw` is unsupportable, silently accepting `--volume` on
buildkit would be misleading. The honest MVP behavior is to **reject `--volume`
with a clear message** on the buildkit backend (mirroring how the backend already
rejects `--cache`, `--cache-image`, `--clear-cache`, `--previous-image` for
capability reasons in `validateBuildFlags`, `internal/commands/build.go`), and
point read-only-data users at a future secrets/bindings mechanism.

## Feasibility for bindings

Assessment: **the most viable path, as a dedicated read-only mechanism — partial /
future support.**

Bindings map onto BuildKit far more naturally than generic `--volume` because:

- They are **read-only** by contract (buildpacks read `type`/`provider`/secret
  files; they do not write back to the host). The unsupportable `:rw` problem does
  not apply.
- They are **small** (config + secrets), so syncing them into the build is cheap.
- They have a **well-known target** (the platform bindings dir, e.g.
  `/platform/bindings`), so no arbitrary target-path mapping is needed.

Two candidate implementations:

1. **Local-synced tree (simplest, matches today's semantics).** Add the bindings
   directory as an `llb.Local` and `llb.AddMount` it read-only at the platform
   bindings path on the detector + builder RUNs. Faithful to the current
   "bindings-as-files" model. Risk: if implemented as a `llb.Copy` instead of a
   mount, the secret files could be captured in an intermediate layer; must use a
   *mount* scoped to the RUN, not a copy, and ensure the bindings path is not under
   a directory that gets copied into the assembled image.

2. **BuildKit secret mounts (most secure for the secret files).** Deliver each
   binding secret via `llb.AddSecret` / a session secrets provider so the bytes
   never enter the LLB graph or any layer. This is the cleaner security story but
   requires reshaping bindings into individual secret entries and reconstructing
   the expected directory layout at build time, which is more work than option 1.

For an MVP, option 1 (read-only local mount at the bindings dir) is the pragmatic
choice; option 2 is the hardening follow-up. Either way this is **new, dedicated
functionality** (a `--binding`-style input or a bindings-dir option), not a
by-product of `--volume`, because the buildkit path has no container to bind onto.

## Fundamental tensions / recommendation

Fundamental tensions:

1. **Sandbox vs. live host mount.** BuildKit builds from an LLB graph in a sandbox
   and has no `docker run -v` equivalent. Read-write host volumes — where a
   buildpack's writes must appear on the host — are structurally impossible. This
   is not a plumbing gap; it is the execution model.
2. **Secrets vs. layers.** Any data injected by copying (rather than a RUN-scoped
   mount or a session secret) risks being captured in a layer. Since the assembled
   image is pushed to a registry, a leaked credential is worse here than in a
   throwaway daemon container. Bindings/certs must use RUN-scoped mounts or
   `llb.AddSecret`, never a plain `llb.Copy` into the image tree.
3. **Transport vs. live view.** `llb.Local` *syncs* data into the build; it is a
   point-in-time copy, not the live host directory the docker-daemon bind provides.
   Semantics differ subtly even for the read-only case.

Recommendation:

- **`--volume`: reject on the buildkit backend, with a clear capability error**
  (consistent with the existing `validateBuildFlags` capability rejections for
  cache/previous-image). Read-write host mounts cannot be honored, and silently
  accepting the flag would mislead users into thinking their host caches/writes
  work. Document that generic host volumes are a docker-daemon-only affordance.
- **Bindings: support as a dedicated, read-only mechanism (future work, not MVP
  blocker).** Because bindings are read-only, small, and target a well-known path,
  they fit BuildKit via a RUN-scoped read-only mount from an `llb.Local` (MVP) and,
  as hardening, via `llb.AddSecret`/session secrets for the secret files. The
  backend already advertises `SupportsSecretMounts: true`, so the capability model
  is ready; only the wiring (a bindings input, `SolveOpt.LocalMounts`/session
  entries, and mounts on the detector/builder RUNs) is missing.

Net: treat `--volume` and bindings differently. `--volume` is a leaky abstraction
over a live host bind that BuildKit cannot provide — reject-and-document. Bindings
are a bounded, read-only, well-located payload that BuildKit *can* carry safely —
implement as its own read-only mechanism when prioritized.

## Code reference index

- `internal/commands/build.go:61` — `Volumes` flag field.
- `internal/commands/build.go:154-156` — untrusted-builder + volumes warning.
- `internal/commands/build.go:219-222` — volumes into `BuildOptions.ContainerConfig`.
- `internal/commands/build.go` (`buildCommandFlags`) — `--volume` registration + help text.
- `pkg/client/build.go:290-299` — `ContainerConfig.Volumes` docs + reserved-path guidance.
- `pkg/client/build.go:633,688` — `processVolumes` call + result stored as `Volumes`.
- `pkg/client/process_volumes.go:16-45` — linux/windows volume parse + sensitive-dir warn.
- `pkg/client/process_volumes_unix.go:16-54` — macOS volume parse/validate, `:ro`/`:rw`.
- `pkg/client/build_test.go:2962-2988` — sensitive-dir warning tests incl. `/workspace/bindings`.
- `internal/build/lifecycle_execution.go:425,427,498,781,798,585` — `WithBinds(l.opts.Volumes...)` per phase.
- `internal/build/phase_config_provider.go:27,38,65-68,121-123` — binds -> Docker `HostConfig.Binds`.
- `internal/build/multiplatform/native_buildfunc.go:88-89,455-457` — `llb.Local` app context; cache mount.
- `internal/build/multiplatform/backend_native.go:57,123,159-166` — secret-mounts capability, auth session, `LocalMounts`.
- `internal/build/multiplatform/backend.go:264-266,304-308` — `SupportsSecretMounts` capability.
- `go.mod:24` — `github.com/moby/buildkit v0.32.2`.
- `moby/buildkit@v0.32.2/client/llb/exec.go:704,710,767,779` — `AddMount`, `AddSSHSocket`, `AddSecret`, `AddSecretWithDest`.
- lifecycle repo: grep `binding` across `*.go` → no matches (no first-class bindings).
