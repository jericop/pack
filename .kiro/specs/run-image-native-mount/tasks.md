# Tasks: Run-Image Native Mount

Local-first MVP work (no new unit/integration tests; validate via the local
two-build strategy). Prerequisite: the `oci-layout-tag-elimination` LLB two-phase
flow (already implemented and verified).

- [ ] 1. Host-side run-image acquisition into an OCI store
  - Resolve the run image ref and pull it ONCE per build into a containerd
    content store (or reuse the store already attached via SolveOpt.OCIStores).
  - Read the run image manifest to collect config digest, manifest digest, and
    all layer digests (per target platform).
  - _Requirements: 1.2, 1.3, 2.2_

- [ ] 2. Generate the synthetic OCI layout metadata on the host
  - Build a minimal `index.json` with a single manifest descriptor pointing at
    the run image's manifest digest; optionally an `oci-layout` marker.
  - _Requirements: 3.3_

- [ ] 3. Assemble the run-image blobs tree in LLB via per-blob OCILayoutBlob
  - For config, manifest, and each layer: `llb.OCILayoutBlob(<repo>@sha256:<dig>,
    ImageBlobOCIStore(sessionID, storeID))` and `llb.Copy` into
    `blobs/sha256/<hex>` of a scratch state.
  - Copy the host-generated `index.json` (+ `oci-layout`) into that state.
  - _Requirements: 3.2, 3.3, 2.1_

- [ ] 4. Mount the assembled run-image layout read-only at the lifecycle path
  - Mount the assembled state read-only at
    `filepath.Join("/output", layout.ParseRefToPath(runImageRef))` for the
    analyzer and exporter RUNs.
  - _Requirements: 3.1, 3.4_

- [ ] 5. Lifecycle: consume the mounted run image without a network pull
  - Add/using a flag (or layout-mode behavior) so the analyzer SKIPS
    `PullToLayout` and uses the run image already present at the layout path.
  - Keep the read path (`FromBaseImagePath` → `FromPath` → lazy blob reads)
    unchanged. Republish the lifecycle image and rebuild the builder image (see
    ubuntu-noble-builder + cnb-lifecycle publish steering).
  - _Requirements: 4.1, 4.2, 4.3_

- [ ] 6. Remove -pull-run-image from the LLB analyzer args
  - Stop passing `-pull-run-image` in `buildLifecyclePhaseArgs` for OCI-layout
    mode once the mount is in place.
  - _Requirements: 1.1_

- [ ] 7. Local validation: correctness + rebase parity
  - Build `samples/go/no-imports` multi-arch to the local registry; confirm the
    per-arch image is runnable (real layers, CNB labels incl
    `io.buildpacks.lifecycle.metadata`, launch binary present).
  - Compare run-image base layer digests against a `-pull-run-image` build to
    prove rebase parity.
  - _Requirements: 2.1, 2.2, 2.3, 5.1, 5.2_

- [ ] 8. Local validation: caching effect (cold vs rebuild)
  - Run the two-build comparison; confirm the run image is no longer pulled over
    the network inside the build and record any rebuild wall-time change.
  - _Requirements: 1.1, 1.2, 5.3_

## Task Dependency Graph

```
1 (host pull into store)
├─> 2 (synthetic index.json)
└─> 3 (assemble blobs tree via OCILayoutBlob) ──> 4 (mount read-only at layout path)
5 (lifecycle: skip pull, consume mount) ──┐
4 ───────────────────────────────────────┼─> 6 (drop -pull-run-image)
                                          └─> 7 (validate correctness + rebase parity) ─> 8 (validate caching)
```

## Notes / deferrals

- Fallback if per-blob assembly is too heavy: persistent cache mount at the
  run-image path (keeps `-pull-run-image`, pull-once-then-persist). See design
  "Alternatives considered".
- The dominant rebuild cost (lifecycle `go build` vertex re-running) is a SEPARATE
  optimization (vertex caching) not covered here.
