# Design: Run-Image Native Mount

## Overview

Replace the analyzer's `-pull-run-image` (an in-build network pull into the OCI
layout) with a BuildKit-native, content-addressed presentation of the run image
as a read-only OCI layout that the lifecycle reads directly. The run image is
pulled ONCE on the host into an OCI content store; its blobs are surfaced into
the build via per-blob `llb.OCILayoutBlob` sources; pack generates a minimal
`index.json`; the lifecycle consumes the mounted layout without a network pull.

This targets the LLB backend only and builds on the two-phase OCI-layout flow in
the `oci-layout-tag-elimination` spec.

## Background: why a naive mount does not work

BuildKit's image sources present the UNPACKED ROOTFS, not the OCI layout:
- `llb.Image(ref)` and `llb.OCILayout(ref, OCIStore)` both resolve to a rootfs
  snapshot (buildkit `source/containerimage/pull.go`). Mounting that gives the
  extracted filesystem; re-tarring it would change diffIDs and BREAK rebase.

The lifecycle needs the run image as an OCI LAYOUT (config + manifest +
compressed layer blobs keyed by digest) so it can reuse the ORIGINAL blobs by
digest. So we must present a `blobs/sha256/` tree, not a rootfs.

## Key BuildKit facts (verified against moby/buildkit v0.30.0)

- `llb.OCILayoutBlob(ref, ...)` surfaces a SINGLE digest-addressed blob from an
  attached OCI store as ONE file (`client/llb/source.go`; server
  `source/containerblob` + `blobfetch/fetch.go` `fetchFromOCILayoutStore`). The
  bytes are streamed unmodified (diff-preserving). The ref must be digested.
- There is NO single LLB primitive that mounts a whole `blobs/sha256` tree. You
  assemble the tree yourself from N `OCILayoutBlob` sources (config + manifest +
  each layer), each `llb.Copy`'d to `blobs/sha256/<hex>`.
- The OCI store attached via `SolveOpt.OCIStores` (already used for Phase 2
  import) can back these `OCILayoutBlob` sources (same sessionID/storeID).

## Key lifecycle / go-containerregistry facts (verified)

- imgutil `layout.NewImage(path, layout.FromBaseImagePath(path))` →
  go-containerregistry `layout.FromPath(path)`.
- `layout.FromPath` only requires `index.json` to exist (the `oci-layout` file
  check is a literal TODO, not enforced). Blobs are read LAZILY by digest from
  `blobs/sha256/<algo>/<hex>`, streamed unmodified. A single-manifest index needs
  no digest lookup (`findDescriptor` allows empty hash when one manifest).
- Analyzer reads only config + labels (os/arch/variant/labels) — no layer bytes
  (`phase/analyzer.go`, `platform/target_data.go`).
- Exporter reuses run/base layers BY DIGEST and copies the original compressed
  blobs into the app image's `blobs/` (`phase/exporter.go` ReuseLayer /
  addExtensionLayers read `blobs/<algo>/<hex>` directly). Rebase-safe.
- The analyzer's `PullToLayout` already SELF-SKIPS when the run-image path exists
  (`image/pull_to_layout.go` os.Stat early-return). So a pre-populated path makes
  the pull a no-op even if the flag were left on.

## Data flow

```
pack (host)
  - resolve run image ref -> pull ONCE into an OCI content store (go-containerregistry
    -> containerd content store), OR reuse the store already attached for phase 2
  - read run image manifest: get config digest, manifest digest, layer digests
  - generate minimal index.json (single manifest descriptor -> manifest digest)
        │
        ▼
LLB graph (per platform), before the analyzer RUN:
  runLayoutState = scratch
    + Copy(OCILayoutBlob(config@dig),   -> blobs/sha256/<config-hex>)
    + Copy(OCILayoutBlob(manifest@dig), -> blobs/sha256/<manifest-hex>)
    + for each layer: Copy(OCILayoutBlob(layer@dig) -> blobs/sha256/<layer-hex>)
    + Copy(host index.json  -> index.json)
    + Copy(host oci-layout  -> oci-layout)   # optional
        │  mount READ-ONLY at /output/<ParseRefToPath(runImageRef)>
        ▼
  analyzer (NO -pull-run-image): reads run image from the mounted layout
  ...detector, restorer, builder...
  exporter: reuses run-image layer blobs BY DIGEST from the mounted layout
        │
        ▼
  Phase 1 export (unchanged): isolate nested app layout -> ExporterLocal -> host
```

## Design decisions

### 1. Host-side single pull into an OCI store
Pack resolves and pulls the run image once (per build, not per platform) into a
containerd content store using go-containerregistry, or reuses the store already
attached via `SolveOpt.OCIStores`. This makes acquisition content-addressed and
cached, and dedups across platform solves. Pack already resolves the run image,
so it knows the digests needed for the `OCILayoutBlob` sources and the synthetic
index.

### 2. Assemble the blobs tree via per-blob OCILayoutBlob + Copy
Because BuildKit has no whole-tree mount, model each blob (config, manifest, every
layer) as an `llb.OCILayoutBlob(<repo>@sha256:<digest>, ImageBlobOCIStore(session,
store))` and `llb.Copy` it to `blobs/sha256/<hex>`. Copies move the ORIGINAL
compressed blobs (not a rootfs), preserving digests/diffIDs. This is the
"copy compressed blobs" cost — far smaller and diff-preserving vs re-tarring a
rootfs, and cacheable by BuildKit per blob.

### 3. Pack generates the synthetic index.json (and oci-layout)
Since pack must enumerate blob digests to emit the sources, generating the tiny
`index.json` (single manifest descriptor → manifest digest) on the host is nearly
free. Copy it (and an `oci-layout` marker) into the assembled layout. This is the
only metadata `layout.FromPath` requires.

### 4. Mount read-only at the lifecycle's expected path
Mount the assembled layout state read-only at
`filepath.Join("/output", layout.ParseRefToPath(runImageRef))`. The analyzer and
exporter only READ it; the app image is written to a DIFFERENT `/output` subpath,
so there is no write conflict.

### 5. Minimal lifecycle change
Add a lifecycle flag (e.g. `-run-image-layout` / rely on layout-mode + present
path) that makes the analyzer SKIP `PullToLayout` and use the run image already at
the layout path. The read path (`FromBaseImagePath` → `FromPath` → lazy blob
reads) is unchanged. Default behavior (network pull) stays available for non-LLB
callers. Note: even without a new flag, pre-populating the path makes the existing
`-pull-run-image` a no-op — but an explicit flag is cleaner and removes the
network dependency entirely.

## Alternatives considered

- **Persistent cache mount at the run-image path**: keep `-pull-run-image`, back
  its target path with an `llb.AsPersistentCacheDir` so the pull happens once and
  persists. Lower complexity, no lifecycle change, but keeps the network pull on
  first build and is a cache-mount hack rather than content-addressed. Viable
  fallback if the per-blob approach proves too heavy.
- **Copy the run image into the layout via an LLB Copy from `llb.Image`**: still
  needs layout-shape conversion; `llb.Image` is a rootfs, so this does not yield a
  layout. Rejected.

## Testing strategy (MVP, local)

Per `mvp-build-testing-strategy` steering: build + rebuild `samples/go/no-imports`
to the local registry, confirm the per-arch image is runnable (real layers, CNB
labels incl `io.buildpacks.lifecycle.metadata`, launch binary present), and
compare run-image base layer digests against a `-pull-run-image` build to prove
rebase parity (Requirement 2). Measure cold vs warm rebuild to quantify the
caching effect on run-image acquisition.

## Risks / open questions

- Number of `OCILayoutBlob` sources scales with run-image layer count; verify
  overhead is acceptable vs a single pull.
- Confirm `OCILayoutBlob` ref formatting (`<repo>@sha256:<digest>`) and the
  `ImageBlobOCIStore` option wiring against the attached store.
- Confirm the run image is single-platform per solve (the per-arch run image
  variant) so the synthetic index has exactly one manifest for that platform.
- Expected wall-time gain is MODEST until lifecycle-phase vertex caching (the
  builder `go build` re-run, the dominant rebuild cost) is separately addressed.
  This spec removes the network pull and makes run-image acquisition
  content-addressed; it is architecture cleanup more than a large speedup.
