---
inclusion: manual
---

# Fork Release Process (jericop/pack)

This fork ships experimental builds of `pack` (the BuildKit LLB OCI-layout
multi-arch work) to the owner's own GitHub releases and Docker Hub, without
touching the upstream `buildpacks/*` destinations. The upstream release pipeline
was disabled for the fork and replaced by a slim, tag-driven pipeline.

## TL;DR — cut a release

1. Commit your work on the working branch (`buildkit-multi-arc-poc`).
2. Push an annotated tag `v<version>` at the commit you want to release, e.g.:
   ```bash
   git tag -a v0.0.1-buildkit-poc <commit> -m "pack v0.0.1-buildkit-poc"
   git push origin v0.0.1-buildkit-poc
   ```
   This triggers the `fork-release` workflow.
3. The workflow runs unit tests, builds the Linux binaries, and creates a
   **draft** GitHub Release AT THAT TAG with the `.tgz` artifacts attached.
4. **Publish the draft release** in the GitHub UI (makes it + artifacts public).
5. **Run the `delivery / docker` workflow manually** with `tag_name: v<version>`
   to build + push the multi-arch image to `docker.io/jericop/pack:<version>`.

Steps 4 and 5 are INDEPENDENT manual actions.

## Everything lines up (tag-driven)

Because you push the git tag first, all four "tags" match:

| Thing              | Value                                           |
|--------------------|-------------------------------------------------|
| git tag            | `v0.0.1-buildkit-poc`                           |
| GitHub release tag | `v0.0.1-buildkit-poc`                           |
| Docker image tag   | `docker.io/jericop/pack:0.0.1-buildkit-poc`     |

`delivery-docker.yml` checks out the source at the git tag (`ref: v<version>`);
since you pushed that tag, the checkout resolves. It strips the leading `v` for
the Docker tag.

## The active workflow: `fork-release.yml`

`.github/workflows/fork-release.yml` — the fork's Linux-only replacement for the
upstream multi-OS `build.yml`.

- Trigger: **push of a `v*` tag** (or manual `workflow_dispatch` with a `version`
  input — but prefer the tag push so the git tag exists for `delivery-docker`).
- `unit` job: runs `make unit` (`go test ./...`). This does NOT run the
  BuildKit/registry integration tests — they are gated behind `PACK_TEST_*` env
  vars that this job does not set, so they SKIP. Run those via `fork-integration.yml`.
- `release` job: derives the version from the tag, builds `linux/amd64` +
  `linux/arm64` (`make build`, version-stamped), packages `.tgz` + `.sha256`, and
  creates a **draft** (non-prerelease) GitHub Release with the artifacts. It
  requests `permissions: contents: write` so the default `GITHUB_TOKEN` can
  create the release (no PAT/secret needed).

The `.tgz` artifacts are built and attached by the `release` job; publishing the
draft only makes them public (it does not rebuild them).

## Publishing the Docker image (manual — preferred)

`.github/workflows/delivery-docker.yml` builds the multi-arch image from the repo
`Dockerfile` at the release tag and pushes to `docker.io/${DOCKERHUB_ORG}/pack`
(default org `jericop`). It builds pack from source for linux/amd64, arm64,
s390x, ppc64le — it does NOT reuse the release `.tgz` binaries.

Run it manually:

1. Actions → "delivery / docker" → "Run workflow".
2. `tag_name`: the release tag, e.g. `v0.0.1-buildkit-poc` (include the `v`).
   `tag_latest`: optional; check to also tag `:latest` (tiny) / `:base`.

CLI alternative (if `gh` is authenticated as the fork owner):

```bash
gh workflow run "delivery / docker" -f tag_name=v0.0.1-buildkit-poc -f tag_latest=false
```

## Integration tests: `fork-integration.yml` (manual)

`.github/workflows/fork-integration.yml` runs the BuildKit integration tests that
are gated off in the normal unit/release runs. It runs AUTOMATICALLY on every
push to `buildkit-multi-arc-poc`, and can also be run manually
(`workflow_dispatch`) with custom inputs.

- On a `push` event there are no workflow inputs, so each step falls back to the
  `DEFAULT_*` env vars in the workflow (builder image, platforms, ttl.sh, 24h,
  registry test enabled).
- On a manual `workflow_dispatch` run, the inputs you supply override those
  defaults (`${{ github.event.inputs.X || env.DEFAULT_X }}`).
- Heads-up: each run does a multi-arch build (arm64 under QEMU emulation) and a
  ttl.sh push, so a full run takes several minutes. If push-on-every-commit gets
  too heavy, narrow the `push:` `branches`/add `paths` filters in the workflow.

What it does on a Linux runner:
- sets up QEMU + a plain `docker-container` buildx builder,
- clones the sample app (`paketo-buildpacks/samples`, `go/no-imports`),
- runs the Tier 2 (no-registry) tests: `TestOCILayoutOnDiskIntegration`,
  `TestOCILayoutParityIntegration`, `TestOCILayoutRebaseIntegration`,
- optionally runs the Tier 3 registry test `TestOCILayoutRegistryIntegration`,
  pushing to the **ttl.sh** ephemeral, anonymous registry.

Why ttl.sh (and why NO code changes were needed):
- ttl.sh is a real HTTPS registry, so go-containerregistry's default secure
  transport works — no insecure/plaintext handling.
- It needs no auth (anonymous), so the existing `DefaultKeychain` path pack
  already uses works as-is.
- Images auto-expire, so there is nothing to clean up and no local
  registry/network to stand up.
- In multi-arch OCI-layout mode the manifest-list push is HOST-SIDE
  (`remote.WriteIndex`), so only the runner needs registry egress; the builder
  just produces on-disk per-arch layouts.

Inputs (workflow_dispatch):
- `builder_image` — multi-arch builder image with the patched lifecycle
  (default `jericop/ubuntu-noble-builder:skip-chown-poc`).
- `platforms` — default `linux/amd64,linux/arm64`.
- `run_registry_test` — also run the Tier 3 ttl.sh test (default true).
- `registry_base` — default `ttl.sh`. Point at GHCR/Docker Hub for a persistent
  registry (would then require auth/`docker login` in the workflow).
- `ttl` — ttl.sh image lifetime, default `24h` (ttl.sh MAX is 24h; longer values
  are rejected). The pushed ref is `ttl.sh/jericop-pack-<run_id>-<attempt>:<ttl>`.

Note on image lifetime: 24h is the ttl.sh ceiling. If you need images to live
longer for review, switch `registry_base` to a persistent registry you own
(GHCR / Docker Hub) — that would require wiring auth into the workflow.

## Cutting a release (tag + optional bookkeeping)

Keep iterating on `buildkit-multi-arc-poc`; the tag is the release pointer. There
is no longer a `release/**` branch or a separate `release-cut/*` marker tag — the
`v<version>` tag IS the marker and the trigger.

```bash
git tag -a v<version> <commit-or-branch> -m "pack v<version>"
git push origin v<version>
```

## Configuration (fork repo settings)

- Secrets: `DOCKER_USERNAME`, `DOCKER_PASSWORD` (Docker Hub creds for
  `delivery-docker.yml`).
- Variables (optional):
  - `DOCKERHUB_ORG` — override the Docker Hub org (default `jericop`).
  - `ENABLE_PACKAGE_DELIVERY=true` — opt back into the homebrew/chocolatey/
    ubuntu/archlinux delivery workflows (off by default).
  - `ENABLE_RELEASE_DISPATCH=true` — opt back into dispatching to `buildpacks/*`
    repos on release (off by default; you almost certainly do NOT want this).
  - `ENABLE_RELEASE_MERGE=true` — opt back into auto-merging `release/**` into
    `main` (off by default).
- Repo Actions setting: if the `release` job ever 403s creating the release, set
  Settings → Actions → General → Workflow permissions to "Read and write
  permissions". The workflow already requests `contents: write`, which is
  normally sufficient.

## Disabled / neutered workflows (fork-specific)

Changed to manual-only (`workflow_dispatch`) and/or gated behind opt-in repo
variables. Kept intact for an easy upstream re-sync:

- `build.yml` — upstream multi-OS build/release. Manual-only; replaced by
  `fork-release.yml`.
- `compatibility.yml` — was hanging; manual-only.
- `delivery-release-dispatch.yml` — gated by `ENABLE_RELEASE_DISPATCH`.
- `delivery-homebrew.yml`, `delivery-chocolatey.yml`, `delivery-ubuntu.yml`,
  `delivery-archlinux.yml` — gated by `ENABLE_PACKAGE_DELIVERY`.
- `release-merge.yml` — gated by `ENABLE_RELEASE_MERGE`.
- `delivery-docker.yml` — still auto-triggers on `release: released`, but is run
  MANUALLY in this fork (the release + Docker steps are decoupled).

## Known cosmetic gaps (not blocking)

- `check-latest-release.yml` scans `docker.io/buildpacksio/pack` on a schedule and
  files issues; harmless in a fork but points at the upstream image. Disable it
  if the scheduled issues become noise.
