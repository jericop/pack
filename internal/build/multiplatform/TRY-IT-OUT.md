# Try it out: multi-arch buildpacks builds with BuildKit

This guide shows how to build a **multi-architecture** app image with Cloud Native
Buildpacks using the experimental BuildKit `buildkit` backend in this `pack` fork.
It uses three published, pinned fork images that are designed to work together:

| Image | Tag | What it is |
|-------|-----|------------|
| `docker.io/jericop/lifecycle` | `buildkit-native-export-v0.1.0` | Patched lifecycle: `-skip-chown`, exporter emit-mode, and the post-push finalize step |
| `docker.io/jericop/ubuntu-noble-builder` | `buildkit-native-export` | Multi-arch builder that bundles the pinned lifecycle above (via `builder.toml [lifecycle].uri`) |
| `jericop/pack` (source) | `buildkit-native-export` branch | The fork of `pack` with the `--buildkit` / `--build-backend buildkit` support |

All three are multi-arch (`linux/amd64` + `linux/arm64`).

> **Experimental.** This is a proof of concept, not an official Cloud Native
> Buildpacks release. Expect rough edges.

## How it works (30-second version)

With `--build-backend buildkit`, `pack` runs the lifecycle **inside BuildKit** and
lets BuildKit build and push ONE multi-arch image (an OCI index) with **no
intermediate per-arch tags**. Immediately after the push, `pack` calls a
lifecycle-owned **finalize** step that authors the correct CNB metadata
(`io.buildpacks.lifecycle.metadata`) on the pushed image from its actual produced
layers. The result is a normal, runnable, rebuildable, rebaseable CNB image.

## Prerequisites

1. **Docker** with BuildKit (Docker Engine 23.0+ / recent Docker Desktop).
2. **QEMU** for cross-arch emulation (default on Docker Desktop; on Linux run
   `docker run --privileged --rm tonistiigi/binfmt --install all`).
3. **A `docker-container` buildx builder** (the default `docker` driver cannot do
   multi-platform):
   ```bash
   docker buildx create --name pack-multiplatform --driver docker-container --bootstrap
   docker buildx ls   # confirm pack-multiplatform is running
   ```
4. **The fork `pack` binary.** Either download a published release binary (if one
   has been cut) or build it from source:
   ```bash
   git clone -b buildkit-native-export https://github.com/jericop/cnb-pack pack-fork
   cd pack-fork
   go build -o /usr/local/bin/pack .
   pack version
   ```
5. **Enable experimental features** (the whole BuildKit feature is gated):
   ```bash
   pack config experimental true
   ```
6. **A registry you can push to** and are logged in to (`docker login ...`).
   Multi-arch images cannot be loaded into a local Docker daemon, so `--publish`
   is required.

## Build a multi-arch app image

Using a sample app (any buildpacks-compatible app works; the Paketo samples repo
has small ones such as `go/no-imports`):

```bash
pack build <your-registry>/my-app:latest \
  --path ./path/to/app \
  --builder jericop/ubuntu-noble-builder:buildkit-native-export \
  --run-image paketobuildpacks/ubuntu-noble-run:latest \
  --platforms linux/amd64,linux/arm64 \
  --buildkit --build-backend buildkit \
  --buildkit-builder pack-multiplatform \
  --publish --trust-builder
```

What each BuildKit-specific flag does:

- `--buildkit` — enable the BuildKit backend (requires experimental mode).
- `--build-backend buildkit` — select the single builder-agnostic buildkit backend
  (this is the default when `--buildkit` is set; shown here for clarity).
- `--platforms linux/amd64,linux/arm64` — the target architectures for the one
  multi-arch image.
- `--buildkit-builder pack-multiplatform` — the `docker-container` buildx builder
  to run the solves on.
- `--publish` — required for multi-arch (push the manifest list to the registry).
- `--trust-builder` — trust the fork builder.

The builder bundles the matching lifecycle, so you do **not** need
`--lifecycle-image`. If you use a builder that does NOT bundle an
emit/finalize-capable lifecycle, add:

```bash
  --lifecycle-image jericop/lifecycle:buildkit-native-export-v0.1.0
```

### Registry cache (optional, good for CI)

On ephemeral CI builders, import/export the BuildKit cache via a registry:

```bash
  --buildkit-cache-from type=registry,ref=<your-registry>/my-app-cache:latest \
  --buildkit-cache-to   type=registry,ref=<your-registry>/my-app-cache:latest,mode=max
```

## Verify the result

Confirm the pushed tag is a real multi-arch manifest with both architectures:

```bash
docker manifest inspect <your-registry>/my-app:latest
# or:
crane manifest <your-registry>/my-app:latest | jq '.manifests[].platform'
```

Confirm the CNB metadata was authored by finalize (the build-phase
`io.buildpacks.lifecycle.prepared-metadata` label is removed and the final
`io.buildpacks.lifecycle.metadata` is present) on a per-arch image:

```bash
crane config --platform linux/arm64 <your-registry>/my-app:latest \
  | jq '.config.Labels | keys'
# expect: io.buildpacks.lifecycle.metadata present,
#         io.buildpacks.lifecycle.prepared-metadata absent
```

Run it (pull the arch that matches your machine):

```bash
docker run --rm <your-registry>/my-app:latest
```

Rebase onto a new run image (works like any CNB image — only the run base swaps):

```bash
pack rebase <your-registry>/my-app:latest \
  --run-image paketobuildpacks/ubuntu-noble-run:latest --publish
```

## Build your own custom builder (pin the lifecycle yourself)

If you want your own builder that bundles the pinned lifecycle, point your
`builder.toml`'s `[lifecycle]` at the published lifecycle tag:

```toml
# builder.toml (excerpt)
[lifecycle]
  uri = "docker://docker.io/jericop/lifecycle:buildkit-native-export-v0.1.0"

[[targets]]
  os = "linux"
  arch = "amd64"

[[targets]]
  os = "linux"
  arch = "arm64"
```

Then create + publish it per-arch and assemble the manifest list. Because the
`docker-container` builder builds each arch natively, the simplest reliable path is
one native runner per arch (or QEMU locally):

```bash
# per arch (repeat for arm64 on an arm64 runner, or with --target linux/arm64)
pack builder create <your-org>/my-builder:my-tag-amd64 \
  --config ./builder.toml --target linux/amd64 --publish
pack builder create <your-org>/my-builder:my-tag-arm64 \
  --config ./builder.toml --target linux/arm64 --publish

# assemble the manifest list
docker manifest create <your-org>/my-builder:my-tag \
  <your-org>/my-builder:my-tag-amd64 \
  <your-org>/my-builder:my-tag-arm64
docker manifest push <your-org>/my-builder:my-tag
```

`jericop/ubuntu-noble-builder`'s own `.github/workflows/publish-builder.yml` does
exactly this (per-arch on native runners + a manifest job); use it as a template.

## Use it in CI

The pattern is the same as local, with two additions: create the
`docker-container` builder on the runner, and use registry cache so ephemeral
runners still benefit from caching. A minimal GitHub Actions sketch:

```yaml
- uses: docker/setup-qemu-action@v3
- uses: docker/setup-buildx-action@v3
  with:
    name: pack-multiplatform
    driver: docker-container
- name: Build pack (fork)
  run: |
    git clone -b buildkit-native-export https://github.com/jericop/cnb-pack pack-fork
    (cd pack-fork && go build -o /usr/local/bin/pack .)
    pack config experimental true
- name: Build multi-arch app
  run: |
    pack build $REGISTRY/my-app:latest \
      --path ./app \
      --builder jericop/ubuntu-noble-builder:buildkit-native-export \
      --run-image paketobuildpacks/ubuntu-noble-run:latest \
      --platforms linux/amd64,linux/arm64 \
      --buildkit --build-backend buildkit \
      --buildkit-builder pack-multiplatform \
      --buildkit-cache-from type=registry,ref=$REGISTRY/my-app-cache:latest \
      --buildkit-cache-to   type=registry,ref=$REGISTRY/my-app-cache:latest,mode=max \
      --publish --trust-builder
```

## Troubleshooting

- **`multi-platform ... cannot be loaded`** — you must `--publish` for multi-arch;
  a local daemon can't hold a manifest list.
- **`docker` driver can't do multi-platform** — create and pass a
  `docker-container` builder via `--buildkit-builder`.
- **Cross-arch `exec format error`** — install QEMU
  (`docker run --privileged --rm tonistiigi/binfmt --install all`).
- **Pushing to a local/insecure registry** — the `docker-container` builder must be
  able to reach it (put builder + registry on a shared docker network and reference
  the registry by container name); see
  `.kiro/steering/local-test-environment.md` for the local recipe.

## More detail

See `internal/build/multiplatform/buildkit-multi-arch-readme.md` (the technical
reference) and the `.kiro/steering/buildkit-multiarch.md` summary for how the
build-then-finalize flow works internally.
