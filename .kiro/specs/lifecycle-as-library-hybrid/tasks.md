# Tasks: Lifecycle-as-Library Hybrid Build

Local-first MVP (no new unit/integration tests; validate via the local two-build
strategy). Branches: `jericop/cnb-pack@lifecycle-as-library-hybrid` and
`jericop/cnb-lifecycle@lifecycle-as-library-hybrid`.

- [ ] 1. Investigate the lifecycle library seam for analyze + export
  - Confirm how to construct `phase.Analyzer` and `phase.Exporter` outside the
    CLI: the factory/handlers that inject `imgutil.Image` (run/previous), the
    concrete `LayerFactory` (the `layers` package), and required `ExportOptions`
    inputs. Identify any minimal additive lifecycle change needed and make it on
    the cnb-lifecycle `lifecycle-as-library-hybrid` branch.
  - CRITICAL de-risk: confirm `Exporter.Export` can write to a NON-PUSHED
    `imgutil.Image` (in-memory / local OCI layout) — i.e. Save does not force a
    remote push — and that a `v1.ImageIndex` can be composed from N such per-arch
    images for a single `remote.WriteIndex` (no intermediate tags). This is the
    linchpin that keeps the manifest-list push tag-free.
  - _Requirements: 1.2, 2.2, 5.2, 5.4, 7.1, 7.2_

- [ ] 2. Add the `buildkit-hybrid` backend skeleton in pack
  - New `--build-backend=buildkit-hybrid` value + a backend type implementing the
    multi-platform builder interface. Wire flag plumbing in
    `internal/commands/build.go` and the multiplatform package.
  - Wire `--buildkit-cache-from` / `--buildkit-cache-to` into the detect+build
    solve options (reuse `parseCacheImports`/`parseCacheExports`) so a
    registry-backed remote cache is first-class from day one.
  - _Requirements: 1.1, 5.1, 4b.5_

- [ ] 3. Build the detect+build-only LLB graph (sandbox)
  - LLB graph with `llb.Image(builder)` base + `COPY app -> /workspace` + detector
    RUN + builder RUN. NO analyzer/exporter, NO `-layout`. Structure for
    cache-friendliness (stable args, buildpack `/cache` mount, avoid unnecessary
    IgnoreCache).
  - INVARIANT: the builder image is the content-addressed `llb.Image` base only;
    it is NEVER copied. Do not bring the run image into this graph at all.
  - _Requirements: 1.1, 1.3, 4.1, 4b.1_

- [ ] 4. Extract build outputs (/layers + /workspace) to a host dir per platform
  - Export the build result filesystem to a per-platform host directory
    (ExporterLocal + OutputDir of a scratch COPY of /layers + /workspace), incl.
    group.toml, plan.toml, project-metadata, SBOM. Preserve ownership/permissions.
  - INVARIANT: the extraction copy captures ONLY buildpack outputs
    (/layers, /workspace, metadata) — NEVER builder or run-image layers — so those
    images retain independent cacheability (do not scope the copy at a subtree that
    includes builder/run-image content).
  - _Requirements: 3.1, 3.2, 4b.4_

- [ ] 5. Host-side analyze via the lifecycle library
  - Construct run image + previous image as `imgutil.Image` (pack `pkg/image`),
    build `phase.Analyzer`, call `Analyze()` -> `files.Analyzed`.
  - INVARIANT: the run image is acquired as a content-addressed image here
    (host-side), NEVER materialized in full into the build graph. It must be a
    cache hit on rebuild, same as the builder image. IF the build graph needs
    specific run-image files, do a NARROW llb.Copy of only those paths from an
    llb.Image(runImage) source (never the whole image).
  - _Requirements: 2.1, 2.4, 7.1, 4b.2, 4b.3a, 4b.3b_

- [ ] 6. Host-side export via the lifecycle library
  - Construct `WorkingImage` as a NON-PUSHED imgutil.Image (in-memory / local OCI
    layout) on the run-image base, build `phase.Exporter`, call
    `Export(ExportOptions{WorkingImage, AppDir, LayersDir, OrigMetadata,
    RunImageForExport, RunImageRef, LauncherConfig, ...})` to assemble the per-arch
    app image with correct labels. VERIFY the exporter Save does NOT force a
    remote push (no per-arch intermediate tag). Do NOT push per-arch here.
  - _Requirements: 2.2, 2.3, 2.4, 5.3, 5.4, 7.1_

- [ ] 7. Multi-arch manifest list assembly + publish (host, pack)
  - Compose a v1.ImageIndex from the N per-arch NON-PUSHED app images and do ONE
    atomic `remote.WriteIndex(finalRef, index)` (reuse
    PushPerArchLayoutsAsManifestList). Per-arch images referenced by digest only;
    NO intermediate tags. Single-arch: one direct push under the final ref.
    (Non-publish: load into the daemon instead.)
  - _Requirements: 5.1, 5.2, 5.3_

- [ ] 8. Local validation: correctness + runnable + rebase parity
  - Build `samples/go/no-imports` multi-arch to the local registry; runnable check
    (real layers, CNB labels incl io.buildpacks.lifecycle.metadata, launch binary
    present); compare run-image base layer digests for rebase parity.
  - _Requirements: 6.1, 6.2, 2.4_

- [ ] 9. Local validation: caching (cold vs rebuild) + comparison
  - Two-build comparison. Measure BOTH cache paths:
    (a) unchanged rebuild → BuildKit vertex cache HITS on detector + builder RUNs
    (fast path);
    (b) changed-app rebuild → builder vertex re-executes but the CNB `/cache`
    mount gives incremental buildpack layer reuse.
    Capture cold/warm durations and compare to the LLB OCI-layout numbers.
  - _Requirements: 4.1, 4.2, 4.3, 4.4, 6.3_

- [ ] 8a. Cleanup: use a local registry (no PACK_TEST_* env-var gating)
  - Do NOT add `PACK_TEST_*` env-var gates for this backend's validation; use a
    locally-managed registry like pack's existing test suite (testhelpers) + the
    MVP strategy. Where related tests still rely on `PACK_TEST_REGISTRY_ENABLED` /
    `PACK_TEST_REGISTRY_REF` / `PACK_TEST_BUILDKIT_ENABLED` gates, remove them in
    favor of a local registry. Keep validation local-first/MVP.
  - _Requirements: 6.1_

- [ ] 9a. Measure data-egress cost on a large-dependency app
  - Repeat the build with a Node.js or Python sample (large node_modules /
    virtualenv) and MEASURE the volume + time to egress `/layers` (+ `/workspace`)
    from BuildKit to the host per platform. Record it to quantify the ceiling of
    the host-side-export tradeoff vs the OCI-layout (Option A) backend.
  - _Requirements: 3.1, 6.3_

- [ ] 10. Local validation: image acquisition decoupled + remote cache
  - Confirm that on a rebuild with CHANGED app source, BOTH the builder AND the
    run image are NOT re-downloaded (cache hit) — the run image gets the same
    guarantee as the builder — proving acquisition is independent of the
    copy/build inputs.
  - If any run-image files are needed in the sandbox, confirm only a NARROW copy
    of those paths occurs (not the whole run image) and the llb.Image(runImage)
    source still cache-hits.
  - Configure `--buildkit-cache-to` to the local registry, then simulate a fresh
    environment (fresh buildx builder) and build with `--buildkit-cache-from`;
    confirm common layers (builder/buildpack) import from the registry cache
    instead of re-pulling from origin.
  - _Requirements: 4b.3, 4b.3a, 4b.3b, 4b.5, 4b.6_

## Task Dependency Graph

```
1 (lifecycle library seam)
├─> 5 (host analyze)
└─> 6 (host export)
2 (backend skeleton + cache-from/to wiring) ─> 3 (detect+build LLB) ─> 4 (extract outputs to host)
4 ─> 5 ─> 6 ─> 7 (manifest list) ─> 8 (correctness/runnable/rebase) ─> 9 (caching+comparison)
8 ─> 10 (image acquisition decoupled + registry remote cache)
```

## Notes

- Detector/builder stay as sandboxed RUNs for the MVP (explicit non-goal to move
  them host-side).
- Builder runs as a SINGLE RUN (lifecycle executes buildpacks) backed by a CNB
  `/cache` mount. Finer-grained builder caching (decomposing into per-buildpack /
  per-layer RUNs so BuildKit caches them individually) is a documented FUTURE
  experiment (Requirement 4a), evaluated only if the CNB cache mount's incremental
  path is insufficient. Two caches interact: BuildKit vertex cache (coarse,
  per-RUN, skips unchanged RUNs) + CNB `/cache` mount (fine, in-RUN buildpack layer
  reuse when the vertex cache misses).
- If the lifecycle library surface is missing something, make a MINIMAL additive
  change on the cnb-lifecycle `lifecycle-as-library-hybrid` branch and note it.
- This backend is ADDITIVE; it does not remove buildkit-dockerfile or buildkit-llb.
- Compare against recorded LLB OCI-layout rebuild data (lifecycle phases NOT
  cached, ~2m12s warm) — the hybrid's expected win is detect/build vertex caching.
