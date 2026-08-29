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

- [x] 1. Build phase: assemble FROM run-image via LLB + push natively (no frontend)
  - DONE. `NativeBackend` drives an IN-PROCESS gateway BuildFunc (`native_buildfunc.go`)
    via `bkClient.Build` — NO separate frontend. Per platform: builder base → COPY
    app → analyzer/detector/restorer/builder/exporter(emit-mode) → assemble
    `FROM run-image` via per-layer `llb.Copy` from the emitted layer SOURCES. NO
    `tar -xf`, no re-extraction. BuildKit pushes ONE native image/index, no
    intermediate tags.
  - _Requirements: 1, 2, 5, 6_

- [x] 2. Attach the build-metadata label during the build
  - DONE. The BuildFunc sets `io.buildpacks.buildkit.native.build-metadata` (the
    serialized plan + emitted labels, read from `/emit/buildkit/build-metadata.json`)
    on the image config via the gateway result (`ExporterImageConfigKey`). It sets
    only the runtime config (entrypoint/cmd/workingdir/env) — NOT a valid final
    `io.buildpacks.lifecycle.metadata`.
  - _Requirements: 3_

- [x] 3. Call the lifecycle FINALIZE library post-push
  - DONE. `NativeBackend.driveNative` calls `finalize.Finalize` (imported like
    `phase.Rebaser`) on the pushed ref; it authors `io.buildpacks.lifecycle.metadata`
    from produced diffIDs + the build-metadata label and re-pushes config+manifest
    (+index) only. `metadata_rewrite.go` slimmed to the `PACK_HOST_REGISTRY_REMAP`
    test-env shim.
  - _Requirements: 4_

- [x] 4. Retire the frontend from the pack path
  - DONE. Pack no longer imports `cnbfrontend`; the lifecycle `buildkit/cnbfrontend`
    package + `cmd/cnb-frontend` are DELETED. Assembly is expressed with `llb.Copy`
    directly from pack's in-process BuildFunc. Pack's dead `emit_contract.go` (the
    old file-based parser) was removed.
  - _Requirements: 2_

- [x] 5. Local validation — REPEATED rebuilds + rebases (MVP acceptance bar)
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
