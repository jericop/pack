---
inclusion: manual
---

# Lifecycle Output Contract for External Image Assembly

> **STATUS — EXPLORED, NOT IMPLEMENTED.** This proposes a `-export-mode layers`
> lifecycle contract (decomposed layer tarballs/dirs + manifest.json) for external
> assembly tools. It was NOT chosen. The implemented design uses the single
> `buildkit` backend: the lifecycle records per-layer Source refs in emit-mode
> (`io.buildpacks.lifecycle.prepared-metadata`), pack assembles via in-process
> `llb.Copy`, and a post-push `phase/finalize` step authors the final CNB metadata.
> There is no `-export-mode layers` flag, no manifest.json contract, and the
> generated-Dockerfile / `-layout` consumption examples below are historical. The
> layer-order/rebase reasoning remains a useful reference. See `buildkit-changes.md`
> (lifecycle) and `buildkit-multiarch.md` (pack) for the current design.

## Purpose

This document defines the contract between the lifecycle exporter's output and external image assembly tools (BuildKit, Buildah, Podman). The goal is a universal, tool-agnostic output format that any container build tool can consume to produce the final app image with full caching support.

The lifecycle handles all complex logic (layer optimization, metadata, SBOM, process types). The consuming tool handles image assembly, caching, push, and manifest list creation.

## Design Principles

1. **Tool-agnostic** — The output format does not assume BuildKit, Buildah, or any specific tool
2. **Opt-in** — A new lifecycle flag enables this mode; without it, lifecycle behaves as today
3. **Decomposed layers** — Each buildpack-contributed layer is a separate artifact so tools can cache individually
4. **Self-describing** — The output includes all metadata needed to assemble the image without external knowledge
5. **Filesystem-based** — Communication is via the filesystem (works in any container runtime)
6. **Deterministic** — Same inputs produce the same output structure

## Proposed Lifecycle Flag

```
/cnb/lifecycle/exporter -export-mode layers -output-dir /output ...
```

- `-export-mode layers` — New export mode (alongside `registry`, `daemon`, `layout`)
- `-output-dir /output` — Directory where the decomposed output is written

When this mode is active, the lifecycle:
- Does NOT push to a registry
- Does NOT load to a daemon
- Writes a structured directory with all information needed to assemble the image externally

## Output Directory Structure

```
/output/
├── manifest.json              # Assembly manifest (the contract)
├── config.json                # OCI image config (labels, env, entrypoint, etc.)
├── run-image.txt              # Run image reference (base image for the final image)
├── layers/
│   ├── 00-launcher.tar        # CNB launcher layer
│   ├── 01-buildpack-layer.tar # First buildpack launch layer
│   ├── 02-buildpack-layer.tar # Second buildpack launch layer
│   ├── ...
│   └── NN-app.tar             # Application layer
├── sbom/                      # SBOM artifacts (if generated)
│   ├── launch/
│   │   └── <buildpack-id>/
│   │       └── <layer-name>/
│   │           └── sbom.<format>
│   └── build/
│       └── ...
└── report.toml                # Standard lifecycle report
```

## manifest.json (The Core Contract)

This file is the primary interface between the lifecycle and the consuming tool. It contains everything needed to assemble the final image.

```json
{
  "schema_version": "1.0",
  "platform_api": "0.15",
  "run_image": {
    "reference": "paketobuildpacks/run-jammy-tiny:latest",
    "target": {
      "os": "linux",
      "arch": "amd64",
      "variant": ""
    }
  },
  "layers": [
    {
      "path": "layers/00-launcher.tar",
      "diff_id": "sha256:abc123...",
      "size": 12345,
      "media_type": "application/vnd.oci.image.layer.v1.tar+gzip",
      "metadata": {
        "type": "launcher",
        "buildpack_id": "",
        "layer_name": "launcher"
      }
    },
    {
      "path": "layers/01-go-dist.tar",
      "diff_id": "sha256:def456...",
      "size": 234567,
      "media_type": "application/vnd.oci.image.layer.v1.tar+gzip",
      "metadata": {
        "type": "buildpack",
        "buildpack_id": "paketo-buildpacks/go-dist",
        "layer_name": "go",
        "cache": true,
        "launch": true,
        "build": false
      }
    },
    {
      "path": "layers/02-go-build.tar",
      "diff_id": "sha256:789abc...",
      "size": 45678,
      "media_type": "application/vnd.oci.image.layer.v1.tar+gzip",
      "metadata": {
        "type": "buildpack",
        "buildpack_id": "paketo-buildpacks/go-build",
        "layer_name": "targets",
        "cache": true,
        "launch": true,
        "build": false
      }
    },
    {
      "path": "layers/03-app.tar",
      "diff_id": "sha256:fedcba...",
      "size": 8901,
      "media_type": "application/vnd.oci.image.layer.v1.tar+gzip",
      "metadata": {
        "type": "app",
        "buildpack_id": "",
        "layer_name": "app"
      }
    }
  ],
  "image_config": {
    "user": "1001:1001",
    "working_dir": "/workspace",
    "entrypoint": ["/cnb/process/web"],
    "cmd": [],
    "env": [
      "CNB_LAYERS_DIR=/layers",
      "CNB_APP_DIR=/workspace"
    ],
    "labels": {
      "io.buildpacks.lifecycle.metadata": "{...}",
      "io.buildpacks.build.metadata": "{...}",
      "io.buildpacks.project.metadata": "{...}"
    },
    "exposed_ports": {
      "8080/tcp": {}
    }
  },
  "processes": [
    {
      "type": "web",
      "command": ["/cnb/process/web"],
      "args": [],
      "default": true,
      "working_dir": "/workspace"
    }
  ],
  "buildpack_metadata": {
    "buildpacks": [
      {
        "id": "paketo-buildpacks/go-dist",
        "version": "2.5.1"
      },
      {
        "id": "paketo-buildpacks/go-build",
        "version": "3.1.0"
      }
    ]
  }
}
```

## How Each Tool Consumes This

### BuildKit (via generated Dockerfile)

Pack generates a final Dockerfile stage:

```dockerfile
# ... lifecycle phases run in earlier stages ...

# Final image assembly (generated by pack from manifest.json)
FROM paketobuildpacks/run-jammy-tiny:latest AS final
COPY --from=lifecycle-stage /output/layers/00-launcher.tar /tmp/layer.tar
RUN tar -xf /tmp/layer.tar -C / && rm /tmp/layer.tar
COPY --from=lifecycle-stage /output/layers/01-go-dist.tar /tmp/layer.tar
RUN tar -xf /tmp/layer.tar -C / && rm /tmp/layer.tar
COPY --from=lifecycle-stage /output/layers/02-go-build.tar /tmp/layer.tar
RUN tar -xf /tmp/layer.tar -C / && rm /tmp/layer.tar
COPY --from=lifecycle-stage /output/layers/03-app.tar /tmp/layer.tar
RUN tar -xf /tmp/layer.tar -C / && rm /tmp/layer.tar
USER 1001:1001
WORKDIR /workspace
ENTRYPOINT ["/cnb/process/web"]
LABEL io.buildpacks.lifecycle.metadata=...
```

Or more efficiently using direct COPY of layer contents (if lifecycle writes expanded layers):

```dockerfile
FROM paketobuildpacks/run-jammy-tiny:latest AS final
COPY --from=lifecycle-stage /output/layers/00-launcher/ /
COPY --from=lifecycle-stage /output/layers/01-go-dist/ /
COPY --from=lifecycle-stage /output/layers/02-go-build/ /
COPY --from=lifecycle-stage /output/layers/03-app/ /
USER 1001:1001
WORKDIR /workspace
ENTRYPOINT ["/cnb/process/web"]
LABEL io.buildpacks.lifecycle.metadata=...
```

Each COPY produces one BuildKit layer — cached independently.

### BuildKit (via LLB backend)

Pack constructs LLB operations programmatically (same pattern as cnbp):

```go
state, img := llb.Image(runImage)
for _, layer := range manifest.Layers {
    state = state.File(llb.Copy(lifecycleOutput, layer.Path, "/", ...))
}
// Set config from manifest.json
img.Config.Entrypoint = manifest.ImageConfig.Entrypoint
img.Config.Labels = manifest.ImageConfig.Labels
```

### Buildah

```bash
# Create container from run image
container=$(buildah from paketobuildpacks/run-jammy-tiny:latest)

# Add each layer
for layer in /output/layers/*.tar; do
  buildah add $container $layer /
done

# Apply config
buildah config --entrypoint '["/cnb/process/web"]' $container
buildah config --label io.buildpacks.lifecycle.metadata=... $container
buildah config --user 1001:1001 $container
buildah config --workingdir /workspace $container

# Commit
buildah commit $container registry.example.com/myapp:latest
```

### Podman

Similar to Buildah (Podman uses Buildah under the hood for builds):

```bash
# Using a Containerfile/Dockerfile is the simplest path
podman build --platform linux/amd64,linux/arm64 \
  -f <generated-containerfile> --manifest myapp:latest .
```

## Layer Format Options

### Option A: Tar archives (compressed)

Each layer is a `.tar.gz` file containing the filesystem diff for that layer. This is the standard OCI layer format.

**Pros:** Direct OCI compatibility, smallest disk footprint
**Cons:** Must extract to apply (slower for Dockerfile COPY approach), can't use direct COPY

### Option B: Expanded directories

Each layer is a directory containing the filesystem tree for that layer.

**Pros:** Can use `COPY --from` directly (no extraction step), BuildKit caches per-COPY
**Cons:** Larger on disk (no compression), harder to compute diff_id without tar-ing

### Option C: Both (tar + expanded)

Write both forms. The manifest.json references whichever is appropriate.

**Pros:** Tools can pick the best format
**Cons:** Disk space, complexity

### Recommendation

**Option B (expanded directories)** for the BuildKit/Dockerfile use case, because:
- Each `COPY --from=... /output/layers/NN-name/ /` becomes one BuildKit layer
- No extraction step needed in the Dockerfile
- BuildKit caches each COPY independently
- The diff_id can be computed lazily or stored in manifest.json

For Buildah/Podman, the expanded directories work equally well with `buildah add` or `COPY` in a Containerfile.

If tar archives are also needed (for OCI layout compatibility or direct registry push), the lifecycle could write both under different paths or the consuming tool could tar them.

## Relationship to Existing Layout Mode

The existing `-layout -layout-dir` flag writes a complete OCI image (all layers composed into a single image manifest). The proposed `-export-mode layers` is different:

| Aspect | `-layout` (existing) | `-export-mode layers` (proposed) |
|--------|---------------------|----------------------------------|
| Output | Complete OCI image | Decomposed layers + assembly manifest |
| Layers | Bundled into one image | Individual files, one per buildpack layer |
| Consumer | OCI-aware tools (crane, go-containerregistry) | Build tools (BuildKit, Buildah, Podman) |
| Caching | Consumer must diff layers itself | Each layer is pre-separated for tool-native caching |
| Metadata | Inside OCI config blob | Separate manifest.json (easy to parse) |
| Use case | Atomic push of complete image | Integration with build tool's native output pipeline |

The two modes serve different purposes and can coexist.

## Backward Compatibility

- Without `-export-mode layers`, the lifecycle behaves exactly as today (push to registry or daemon)
- The new flag is additive — no existing behavior changes
- The manifest.json schema has a version field for future evolution
- Tools that don't understand a field can ignore it (forward-compatible)

## Future Considerations

- **Layer reuse across builds**: The manifest.json includes diff_id, allowing tools to detect unchanged layers and skip re-processing
- **Incremental export**: On rebuild, lifecycle could write only changed layers (with a previous-manifest input)
- **Remote cache integration**: Tools can use diff_ids from manifest.json to check if layers already exist in a remote cache
- **Multi-platform coordination**: Each architecture produces its own output directory; the tool combines them into a manifest list
- **SBOM attachment**: The sbom/ directory structure allows tools to attach SBOMs to specific layers per OCI SBOM spec
