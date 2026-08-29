# Tasks: BuildKit-Native Export (Option C)

Experimental, opt-in. Local-first MVP (no new unit/integration tests; validate via
the local two-build strategy).

Branches: `jericop/cnb-lifecycle@buildkit-native-export` and
`jericop/cnb-pack@buildkit-native-export` (matching).

## MVP OUTCOME (as-built + validated)

The MVP is IMPLEMENTED and VALIDATED end-to-end. The final architecture differs
from the original C1 sketch below in one important way, discovered during
implementation:

- **A custom CNB BuildKit GATEWAY FRONTEND does the assembly, not pack-side plain
  `client.Solve` LLB.** A plain `client.Solve` with a raw `llb.State` CANNOT set
  the output image config (entrypoint/env/labels incl `io.buildpacks.lifecycle.metadata`)
  — that requires the gateway/frontend result API (`exptypes.ExporterImageConfigKey`).
  So the assembly lives in a custom frontend (`cnb-lifecycle/buildkit/cnbfrontend`,
  prior art: EricHripko/cnbp) that pack drives IN-PROCESS via `client.Client.Build`.
  See design.md "Two variants" / "C2 (frontend)".
- **Frontend flow:** builder base → COPY app → analyzer/detector/restorer/builder
  RUNs → lifecycle exporter in EMIT-MODE (`-emit-export-plan`) → assemble
  `FROM run-image` by extracting each emitted layer tar as its OWN layer (one RUN
  per emitted CNB layer, plan order) → set image config/labels from the emitted
  `config.json` via `AddMeta(ExporterImageConfigKey)` → return per-platform refs
  for native multi-arch. Layer data never leaves BuildKit's content store.
- **Emit-mode persists layer tars** under `<emit-dir>/buildkit/layers/` so the
  frontend can read them after the export RUN (the exporter's own temp dir is
  cleaned up).
- **Host-side metadata-SHA rewrite (pack, post-push):** the gateway `Reference`
  API does NOT expose the produced layer diffIDs, so the frontend cannot rewrite
  the metadata before returning. Instead, after BuildKit pushes, pack pulls the
  image CONFIG+manifest (tiny — NO layer egress), maps the emitted per-layer
  diffIDs to the actual assembled diffIDs (via the ordered
  `io.buildpacks.native.layer-order` label the frontend records), rewrites every
  `io.buildpacks.lifecycle.metadata` per-layer sha, and re-pushes config+manifest.
  Handles single image + manifest list. NOTE (revised): the MVP rewrite dropped the
  layer-order label after use; the PLAN (task 11) is to KEEP it as a durable,
  required output so every buildkit-native image stays self-heal-capable (see the
  self-healing build-time check + `--buildkit-fix-remote-image-metadata` flow, task 9).
- **Run image** is resolved from the analyzer-written `/layers/analyzed.toml`
  (digest-pinned), used as the `llb.Image` assembly base — no `-pull-run-image`,
  no disk-layout materialization.

### Validated (samples/go/no-imports, local registry)

- COLD build succeeds; produces a runnable CNB image (real layers, CNB labels,
  entrypoint `/cnb/process/workspace`, app binary present).
- WARM rebuild (previous image exists) succeeds — the analyzer's previous-image
  restore works because the metadata SHAs match the actual layers (this was the
  "analyzer flakiness"; now fixed by the SHA rewrite).
- Buildpack-layer rebase compatibility: ALL per-layer metadata SHAs
  (app/sbom/launcher/config/process-types + each buildpack layer) match the
  image's actual layer diffIDs → satisfies the `jab/buildpack-layer-patching` RFC
  prerequisite (NO LONGER deferred — it is supported).
- Multi-arch (linux/amd64 + linux/arm64): ONE native OCI image index, both arches
  well-formed, NO intermediate `build-*` tags.
- Rebase: `pack rebase` succeeds on the buildkit-native image.

### Local test env prerequisites (see command-execution-practices steering)

- Local `registry:2` connected to the buildx builder's docker network so the
  in-build RUNs can reach it by name; builder created with
  `--allow-insecure-entitlement network.host` + a buildkitd insecure-registry
  config for the local registry.
- Lifecycle phase RUNs use `llb.Network(NetMode_HOST)`; pack requests the
  `network.host` entitlement in the solve.
- `registry.insecure` export attr + `PACK_HOST_REGISTRY_REMAP` handle the
  local-registry HTTP + host-vs-buildkit registry-name split (test-env only).

- [~] 1. Depend on + consume the lifecycle emit-mode (see lifecycle spec)
  - PREREQUISITE (owned by cnb-lifecycle@buildkit-native-export): the lifecycle
    emit-mode that produces `plan.json` + `config.json` + layer tars (schema
    `buildkit-native-export/v1`). This pack task is to RUN emit-mode after the
    builder phase and PARSE that output — not to implement it.
  - CONSUME by IMPORTING the lifecycle's `phase/emit` types
    (`emit.Plan`/`LayerOp`/`ImageConfig`, `emit.Schema`, `emit.RecorderDir`). This
    is NOT a "structs vs JSON" choice: pack already imports the lifecycle as a
    library (e.g. `phase.Rebaser` in `pkg/client/rebase.go`), so importing
    `phase/emit` adds ZERO new dependency surface. The JSON files are the TRANSPORT
    across the BuildKit→host boundary (the emit producer runs in a BuildKit RUN; pack
    runs on the host), and the imported structs are the SCHEMA pack unmarshals into.
    Import the structs (single source of truth, no drift) AND read the JSON (the
    wire format) — they are two halves of one flow, not alternatives. See the
    "Transport model" section in requirements.
  - _Requirements: 2.1, 2.2, 2.3_
  - IN PROGRESS — dependency wiring DONE:
    - Added to pack `go.mod`:
      `replace github.com/buildpacks/lifecycle => /Users/jpena/.repos/jericop/cnb-lifecycle`
      (local clone) for the MVP iterate-both-repos loop. Existing
      `require github.com/buildpacks/lifecycle v0.21.0` is unchanged; the replace
      redirects it to the local fork.
    - Verified: `go list github.com/buildpacks/lifecycle/phase/emit` resolves and
      `go build .` produces the pack binary (`/tmp/pack-poc-emit`) — pack compiles
      against the local fork lifecycle incl. `phase/emit`.
    - TOOLCHAIN NOTE: the fork lifecycle `go.mod` declares `go 1.26.6`; local Go is
      1.26.5. `GOTOOLCHAIN=auto` (already set) auto-fetches 1.26.6, so NO edit to the
      lifecycle `go.mod` `go` directive was needed. Build pack with
      `GOTOOLCHAIN=auto go build .` (auto is the default env here).
    - VERSIONING PLAN (for CI/PR reproducibility): tag cnb-lifecycle `v100.0.1` — a
      deliberately high, collision-proof semver (upstream is v0.x) — and repoint the
      replace at `github.com/jericop/cnb-lifecycle v100.0.1`. Consumed via `replace`
      so Go's `/vN`-path major-version rule does NOT apply (no lifecycle module-path
      rename). Bump the patch or add a pre-release id (e.g. `v100.0.1-emit2`) for
      later iterations. LOCAL replace now → tagged replace at PR time.
    - REMAINING: add the plan/config parser + validation in pack that reads
      `<emit-dir>/buildkit/plan.json` + `config.json` into the imported `emit` types.

- [x] 2. Pack: add opt-in `buildkit-native` backend skeleton
  - New `--build-backend=buildkit-native` value + backend type. DONE:
    `BackendBuildkitNative` const (backend.go), factory case -> `NewNativeBackend`
    (backend_factory.go), flag help (internal/commands/build.go),
    `internal/build/multiplatform/backend_native.go` (holds an `*LLBBackend` for
    shared plumbing; `PushesNatively: true`). Drives the frontend in-process.
  - _Requirements: 6.1, 4.2, 4.3_

- [x] 3. Build graph: detector + builder + emit-mode RUNs (data stays in BuildKit)
  - DONE, but the graph lives in the FRONTEND (`cnb-lifecycle/buildkit/cnbfrontend`),
    not pack: builder base → COPY app → analyzer/detector/restorer/builder RUNs →
    lifecycle exporter EMIT-MODE RUN producing `/emit/buildkit/{plan.json,config.json}`
    + persisted layer tars. Buildpack `/cache` mount. Pack passes the inputs
    (builder/run image/platforms/uid/gid/order/registry-auth) as frontend options.
  - _Requirements: 1.2, 3.2, 4.1_

- [x] 4. Assembly: build the app image natively in BuildKit from the plan
  - Read the small plan.json + config.json; assemble `FROM run-image` base + the
    planned layers + apply the image config. No host layer egress.
  - APPROACH = PURE-LLB "FROM run-image" assembly (REVISED & empirically decided;
    see design.md "Design decision 5"). The earlier content-level-blob-append /
    A2-in-BuildKit-assembly plan is SUPERSEDED.
    - RATIONALE: the requirement is REBASEABILITY, not diffID identity with
      registry/oci modes. PROVEN by running the real phase.Rebaser against a
      "FROM run-image + app layer with recomputed diffID" image: rebase succeeds
      because it depends ONLY on the metadata label's RunImage.TopLayer boundary,
      not app-layer diffIDs (log: DECISIVE-rebase-recomputed-diffids-PASS.log).
    - ASSEMBLY: `FROM llb.Image(run-image)` + add the app/buildpack/launcher layers
      from the in-build /layers (BuildKit snapshots + computes its OWN diffIDs) +
      apply emitted config (entrypoint/env/workingdir/labels incl
      io.buildpacks.lifecycle.metadata). Keeps ALL layer data in BuildKit (no
      egress), fully cacheable. The ONLY correctness requirement: the emitted
      RunImage.TopLayer matches the run image's top layer in the assembled image
      (emit contract already provides it).
    - REQUIRED post-assembly step (NOT optional — driven by the buildpack-layer-
      patching RFC): after LLB assembly, read the assembled image's actual layer
      diffIDs (from its config RootFS.DiffIDs) and REWRITE the
      io.buildpacks.lifecycle.metadata label so every per-layer sha (App/Launcher/
      Config/ProcessTypes + each Buildpacks[].layers[].sha) matches the actual
      produced diffID, and RunImage.TopLayer matches the run image top. WHY: the
      buildpacks/rfcs jab/buildpack-layer-patching feature locates a buildpack layer
      to patch BY its metadata sha and replaces it — if the metadata shas don't match
      real layers, patching breaks. Plain rebase only needs RunImage.TopLayer, but
      FUNCTIONAL PARITY includes layer patching. Mapping plan-entry -> produced-diffID
      is a positional zip (plan gives order; BuildKit produces in that order). Reads
      diffIDs from image config, NOT layer data -> still no egress.
    - CAVEAT (RFC + README, tracked): app/buildpack-layer diffIDs differ from other
      modes; fine because the rewritten metadata makes the image internally
      consistent for BOTH base rebase AND buildpack-layer patching.
    - REMOVES the earlier open question about run-image sourcing / OCIStore import:
      the run image is simply the llb.Image base; no disk-layout materialization, no
      -pull-run-image, no imgutil hand-assembly.
  - _Requirements: 1.1, 1.3, 3.1, 3.3, 7.1, 7a_
  - STATUS: DONE. Implemented in the frontend as PER-LAYER tar extraction (one
    layer per emitted CNB layer, plan order) FROM `llb.Image(run-image)`, config set
    via `AddMeta(ExporterImageConfigKey)`. The metadata-SHA rewrite runs HOST-SIDE
    in pack after push (the gateway Reference API can't expose produced diffIDs;
    see MVP OUTCOME above) — `metadata_rewrite.go`. Buildpack-layer patching is
    SUPPORTED (metadata SHAs match actual layers), not deferred.

- [x] 5. Native multi-arch export + push
  - DONE. The frontend returns per-platform refs (`AddRef` + `ExporterPlatformsKey`);
    BuildKit's `ExporterImage` (push=true) produces per-arch images + ONE manifest
    list natively with NO intermediate tags. Validated linux/amd64 + linux/arm64.
  - _Requirements: 5.1, 5.2, 5.3_

- [x] 5a. Cleanup: use a local registry (no PACK_TEST_* env-var gating)
  - Do NOT add `PACK_TEST_*` env-var gates for this backend's validation; use a
    locally-managed registry like pack's existing test suite (testhelpers) + the
    MVP strategy. Where related tests still rely on `PACK_TEST_REGISTRY_ENABLED` /
    `PACK_TEST_REGISTRY_REF` / `PACK_TEST_BUILDKIT_ENABLED` gates, remove them in
    favor of a local registry. Keep validation local-first/MVP.
  - _Requirements: 6.2_

- [x] 6. Local validation: correctness + runnable + rebase parity
  - DONE. Built `samples/go/no-imports` (single + multi-arch) to the local
    registry; runnable check passed (11 layers, CNB labels incl
    io.buildpacks.lifecycle.metadata, launch binary `.../go-build/targets/bin/workspace`
    present); `pack rebase` succeeded; per-layer metadata SHAs match actual layer
    diffIDs (buildpack-layer patching prerequisite).
  - _Requirements: 6.2, 7.1, 7a_

- [~] 7. Local validation: no egress + caching + comparison
  - Layer data stays in BuildKit (only tiny plan/config metadata + the config/
    manifest for the SHA rewrite cross to the host — NO layer blobs). Cold/warm
    builds validated (warm reuses cache; analyzer previous-image restore works).
  - REMAINING (post-MVP hardening): large Node/Python app egress+timing comparison
    vs Option A (oci-layout) and Option B (host-side export); formal two-build
    timing capture.
  - _Requirements: 1.2, 4.1, 6.3_

- [ ] 8. (DEFERRED — after MVP confirmed) Add automated tests
  - ONLY after the MVP (tasks 6–7) confirms the backend works end-to-end, add
    automated coverage for the `buildkit-native` backend + emit-contract parser.
    Use a local registry like pack's existing testhelpers; no `PACK_TEST_*`
    env-var gating. Explicitly out of scope for the MVP milestone.
  - REPEATED-CYCLE COVERAGE (user requirement — do NOT only test the first
    build/rebase): tests MUST verify the FULL lifecycle repeats, not just once:
    1. build → REBUILD → REBUILD (≥2 rebuilds): each rebuild's analyzer
       previous-image restore succeeds, and after each rebuild ALL per-layer metadata
       SHAs match the image's actual diffIDs (label regenerated correctly each time,
       not carried stale).
    2. build → REBASE → REBASE (≥2 rebases): each rebase succeeds; verify
       `io.buildpacks.native.layer-order` survives and stays valid.
    3. build → REBASE → REBUILD: a rebuild AFTER a rebase still works.
    4. SELF-HEALING repeat: an image whose rewrite was skipped/failed → self-healing
       fix (Task 9) → REBUILD → REBUILD: confirm the fix is idempotent and the healed
       image supports repeated subsequent rebuilds/rebases.
  - _Requirements: 6.2, 7b_

- [ ] 9. (FOLLOW-UP — not for MVP) Self-healing: build-time metadata check + opt-in fix flag
  - MOTIVATION (user question): the host-side metadata-SHA rewrite (Task 4 /
    metadata_rewrite.go) runs AFTER BuildKit pushes. If that rewrite fails (e.g. a
    CI/CD flake, network blip, or the process dies between push and rewrite), the
    pushed image is STILL RUNNABLE (only the CNB metadata label is stale) but is NOT
    cleanly rebuildable (analyzer previous-image restore fails) NOR rebaseable/
    patchable (metadata SHAs don't match actual layers).
  - CHOSEN APPROACH (replaces the earlier "standalone repair command" idea — a new
    top-level command for fixing broken published images is confusing to document).
    Fold the repair into the normal build flow:
    1. PRE-BUILD CHECK: when a buildkit(-native) build targets an image ref that
       already exists remotely, pack FIRST inspects that existing image's
       `io.buildpacks.lifecycle.metadata` for rebuild/rebase VALIDITY (per-layer
       SHAs present in the image's actual diffIDs; runImage.topLayer coherent). This
       reuses the read-only half of metadata_rewrite.go (config+manifest only, NO
       layer egress).
    2. IF INVALID and the fix flag is NOT set: WARN the user (explain the image is
       runnable but not rebuildable/rebaseable) and STOP.
    3. IF INVALID and the fix flag IS set (e.g. `--buildkit-fix-remote-image-metadata`):
       pack fixes the existing image IN PLACE (positional remap using the
       `io.buildpacks.native.layer-order` label + rewrite the metadata label +
       re-push config+manifest) before proceeding. Idempotent.
  - RESULT: CI/CD pipelines can enable the flag by default → self-healing builds,
    with NO new top-level command to document. Opt-in; default off preserves
    fail-fast-with-warning behavior.
  - PREREQUISITE: the fix needs the emitted layer ORDERING to map metadata entries
    to actual diffIDs. Today the frontend records it in `io.buildpacks.native.layer-order`
    but the successful rewrite REMOVES that label. Task 11 makes the label a
    DURABLE, required build output (kept on the pushed image) so self-healing has
    the ordering it needs. Do NOT remove the label on rewrite.
  - ATOMICITY / FAILURE SEMANTICS (must be documented + preserved): the build push
    and the rewrite are two SEPARATE registry operations (not one transaction). Each
    rewrite re-pushes to the SAME tag via a single manifest PUT (index PUT for a
    manifest list) → tag-atomic: the tag resolves to either the old or the new image,
    never a partial one. A failed rewrite leaves the ORIGINAL pushed image at the tag
    → still pullable/runnable, just not rebuildable/rebaseable until healed. The fix
    is idempotent, so a later build self-corrects. (See metadata_rewrite.go:
    `remote.Write` / `remote.WriteIndex` are the atomic points.)
  - Reuses the positional-remap logic in metadata_rewrite.go. NOT IMPLEMENTED.
    Documented in the README ("Is the metadata update atomic?" + Future Work).
  - _Requirements: 7.1, 7b_

- [ ] 10. (FOLLOW-UP — not for MVP) Standalone frontend: full rebuild/rebase lifecycle
  - MOTIVATION (user question): the frontend can be published + used standalone
    (`cmd/cnb-frontend`, `#syntax=`-style) WITHOUT pack. In that mode the host-side
    metadata rewrite (which lives in pack) does NOT run, so: the FIRST build works
    and is runnable, but a REBUILD fails at the analyzer previous-image restore
    (same root cause as Caveat 1), and rebase operates on mismatched metadata. So
    standalone use is first-build-only today.
  - SCOPE: make standalone frontend use support the full rebuild/rebase lifecycle —
    either (a) the frontend/standalone image self-applies the metadata-SHA rewrite
    after export (needs a way to get produced diffIDs — a post-export hook or a
    thin wrapper that pulls+rewrites), or (b) clearly document + detect the
    first-build-only limitation and fail fast on a detected rebuild. Note the
    durable layer-order label (Task 11) also lets an external tool self-heal a
    standalone-built image the same way pack does (Task 9).
  - NOT IMPLEMENTED. Documented in the README (Caveat 2 + Future Work).
  - _Requirements: 6.1_

- [ ] 11. (REQUIRED for self-healing) Make `io.buildpacks.native.layer-order` a durable build output
  - The `io.buildpacks.native.layer-order` label (ordered emitted diffIDs of the
    non-reused layers) is what makes the metadata-SHA remap possible. Today the
    frontend records it and pack's post-push rewrite REMOVES it on success. For the
    self-healing build-time check + fix flow (Task 9) — and for any external tool to
    fix a published image — this ordering MUST survive on the pushed image.
  - CHANGE: pack's rewrite MUST NOT drop the label; it stays on the final pushed
    image as a durable, required build output. Adding this label is a REQUIREMENT of
    buildkit-native builds (not optional), so every buildkit-native image is
    self-heal-capable.
  - LABEL LIFECYCLE ACROSS REPEATED CYCLES (user question — critical to get right):
    - REBUILD: every buildkit-native build re-runs the exporter in emit-mode and the
      frontend RE-RECORDS `io.buildpacks.native.layer-order` from THIS build's emit
      plan. So the label is REGENERATED each build (its contents legitimately change:
      reused vs new layers differ build-to-build). Pack's rewrite MUST map against
      THIS build's freshly-recorded label — NEVER a stale label carried from the
      previous image. The previous image's label is only consulted by the
      self-healing PRE-BUILD CHECK/FIX (Task 9) on the EXISTING image, not merged
      into the new build.
    - REBASE: `pack rebase` swaps the run-image base layers and rewrites
      `io.buildpacks.lifecycle.metadata` RunImage.TopLayer/Reference; it does NOT
      change the app/buildpack layer diffIDs, so `io.buildpacks.native.layer-order`
      REMAINS VALID after a rebase and does NOT need regeneration. Rebase must not
      strip it. Verify a REBUILD after a REBASE still works.
    - IDEMPOTENCY: the rewrite/fix MUST be idempotent and repeatable — applying it to
      an already-correct image is a no-op; a fixed image must itself be
      rebuild/rebase-capable so the NEXT cycle also self-heals. No unbounded growth or
      drift of the label across cycles.
  - SPEC/LABEL NOTE: this label is pack/frontend-internal (namespaced
    `io.buildpacks.native.*`), not part of the CNB lifecycle metadata contract, so
    adding it should NOT require a lifecycle/platform spec change. If review finds it
    must be spec'd, track that separately; the goal is to keep it label-only.
  - _Requirements: 5.1, 7.1, 7b_

## Task Dependency Graph

```
[cnb-lifecycle@buildkit-native-export: emit-mode + frontend]  (upstream dependency)
        │
        ▼
1 (consume emit contract in pack) ───┐
2 (backend skeleton) ─> 3 (frontend graph: phases+emit RUNs) ─> 4 (per-layer assembly + host-side SHA rewrite)
1 ───────────────────────────────────┘        │
4 ─> 5 (native multi-arch export/push) ─> 6 (correctness/runnable/rebase) ─> 7 (no-egress/caching/comparison)
                                                                                    │
                                                          (post-MVP) 8 (tests)      │
                                                          (required) 11 (durable io.buildpacks.native.layer-order label)
                                                                        │
                                                          (follow-up) 9 (build-time check + --buildkit-fix-remote-image-metadata → self-healing)  <── from Caveat 1, needs 11
                                                          (follow-up) 10 (standalone frontend rebuild/rebase)  <── from Caveat 2
```

## Notes

- This is the most BuildKit-idiomatic and efficient end-state. The heavy lifting
  (the emit-mode) is in the LIFECYCLE spec
  (`jericop/cnb-lifecycle@buildkit-native-export`); this pack spec consumes the
  emit contract. Option C is the chosen direction; Option B
  (`lifecycle-as-library-hybrid`) is the fallback if C proves non-viable.
- If adopted, supersedes `run-image-native-mount` (run image is a native base,
  no -pull-run-image).
- Detector/builder still run as sandboxed RUNs using the lifecycle (untrusted
  buildpack code); only the EXPORT moves to BuildKit-native assembly.
- Rebase safety hinges on preserving run-image layer digests + emitting the CNB
  lifecycle-metadata label exactly; the lifecycle emit-mode emits actual layer
  tars + diffIDs (see lifecycle spec).
- Contract sync: the emit `schema` (`buildkit-native-export/v1`) is defined in the
  lifecycle spec; keep pack's consumer types in sync with it.
- DOC FOLLOW-UP (after MVP fully vetted): capture the Option A layer-assembly
  decision + rejected alternatives (b: pure-LLB extract breaks diffID parity;
  c: host-side go-cr assembly egresses data) in
  `internal/build/multiplatform/buildkit-multi-arch-readme.md` AND the RFC
  `jericop/cnb-rfcs/text/0000-buildkit-multiarch-build.md`. Spec captures it now;
  README + RFC to be updated once the MVP is confirmed.
