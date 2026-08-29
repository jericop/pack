# Tasks: BuildKit-Native Export (Option A — build-then-finalize)

Experimental, opt-in. Local-first MVP with REPEATED-cycle validation (no new
`PACK_TEST_*` gates). Consumer side of Option A; the lifecycle side (emit-mode +
finalize library) is `jericop/cnb-lifecycle@buildkit-native-export`.

Branch: `jericop/cnb-pack@buildkit-native-export`.

## Design summary (see design.md + the spike decision record)

BuildKit builds + pushes a normal multi-arch image (runnable, not yet CNB-compliant)
carrying the `io.buildpacks.buildkit.native.build-metadata` label; pack then calls
the lifecycle FINALIZE library to author the correct `io.buildpacks.lifecycle.metadata`
from the produced diffIDs and re-push config+manifest only. No frontend, no per-layer
re-extraction, no post-push layer changes.

## SUPERSEDED (previous MVP — retained for history)

The previous MVP (custom frontend re-extraction + pack host-side metadata-SHA
rewrite) is SUPERSEDED by Option A. `metadata_rewrite.go` is being replaced by a
thin caller of the lifecycle finalize library; the frontend is retired.

## Tasks

- [ ] 1. Build phase: assemble FROM run-image via LLB + push natively (no frontend)
  - Rework `NativeBackend` so Phase 1 builds the app image via the LLB machinery
    (reuse/adapt `LLBBackend.buildLLBState`): FROM builder → COPY app → analyzer →
    detector → restorer → builder → exporter, assembling `FROM run-image`, with the
    unprivileged-BuildKit flags (`-skip-chown`). NO `tar -xf` re-extraction; the
    image layers are the exporter's produced layers.
  - BuildKit pushes ONE native multi-arch image (ExporterImage, push=true) — no
    intermediate tags. Layer data stays in BuildKit.
  - _Requirements: 1, 2, 5, 6_

- [ ] 2. Attach the build-metadata label during the build
  - Ensure the built image carries `io.buildpacks.buildkit.native.build-metadata`
    (the lifecycle emit-mode plan, serialized). Decide + document HOW it is attached
    (options in design): (a) the lifecycle exporter writes it directly in the build,
    or (b) pack obtains the emitted plan and sets the label via the image config
    result. Prefer (a) so the lifecycle owns it end to end.
  - The build phase MUST NOT pre-write a valid final `io.buildpacks.lifecycle.metadata`.
  - _Requirements: 3_

- [ ] 3. Call the lifecycle FINALIZE library post-push
  - After the push, `NativeBackend` calls the lifecycle finalize library
    (imported like `phase.Rebaser`) on the pushed image ref. Finalize authors
    `io.buildpacks.lifecycle.metadata` from produced diffIDs + the build-metadata
    label and re-pushes config+manifest(+index) only.
  - Replace `metadata_rewrite.go` with a thin caller of finalize, or remove it once
    `NativeBackend` calls finalize directly. Retain the `PACK_HOST_REGISTRY_REMAP`
    test-env shim where finalize needs a host-reachable ref locally.
  - _Requirements: 4_

- [ ] 4. Retire the frontend from the pack path
  - Remove pack's dependence on `cnbfrontend` (in-process `client.Build` driving).
    Pack no longer imports the frontend for assembly. (The lifecycle may keep the
    frontend package around as history, but pack does not use it.)
  - _Requirements: 2_

- [ ] 5. Local validation — REPEATED rebuilds + rebases (MVP acceptance bar)
  - Build `samples/go/no-imports` to the local registry; runnable check (real
    layers, correct `io.buildpacks.lifecycle.metadata` after finalize, launch binary
    present). Then verify REPEATED cycles:
    - ≥2 REBUILDS: each rebuild's analyzer previous-image restore succeeds; after
      each, ALL per-layer metadata SHAs == the image's actual diffIDs.
    - ≥2 REBASES: each `pack rebase` succeeds.
    - REBUILD after REBASE succeeds.
    - MULTI-ARCH (linux/amd64 + linux/arm64): one index, no intermediate tags.
    - Confirm finalize is config+manifest only (no layer blob re-upload).
  - _Requirements: 7, 8, 9_

- [ ] 6. (DEFERRED — after MVP) Automated tests
  - After the MVP is confirmed with repeated cycles, add automated coverage for the
    native backend + the finalize integration. Local registry like testhelpers; no
    `PACK_TEST_*` gates.
  - _Requirements: 7_

- [ ] 7. (DEFERRED — after MVP) Self-healing build-time check + fix flag
  - Build-time metadata validity check on an existing remote image; opt-in
    `--buildkit-fix-remote-image-metadata` runs finalize in place on an image whose
    finalize did not complete. Uses the durable build-metadata label. Out of scope
    for the MVP.
  - _Requirements: 10_

## Task Dependency Graph

```
[cnb-lifecycle: emit-mode plan-as-label + finalize library]  (upstream dependency)
        │
        ▼
1 (build FROM run-image via LLB + native push) ─> 2 (attach build-metadata label) ─> 3 (call finalize post-push) ─> 4 (retire frontend from pack) ─> 5 (repeated-cycle validation)
                                                                                                                              │
                                                                                    (deferred) 6 (tests), 7 (self-healing)
```

## Notes

- No intermediate tags come for free from BuildKit's native multi-arch push;
  finalize only updates config+manifest at the same tag, so pack's
  `PushPerArchLayoutsAsManifestList` is not needed here.
- Recomputed diffIDs do not affect BuildKit's build cache (keys on the op graph).
- DOC follow-up: update README + steering + RFC to Option A once the MVP is
  confirmed.
