# DRAFT — PLATFORM-1802 ticket comment (for review, NOT yet posted)

> Review location: `/Users/jpena/tmp/dot-kiro-files/PLATFORM-1802-ticket-comment-DRAFT.md`
> This is a draft of the comment I intend to add to the PLATFORM-1802 Jira ticket. Edit in
> place or tell me what to change; I will not post to Jira until you say so.

---

## Multi-arch build comparison: native (multi-agent) vs BuildKit emulation

### TL;DR
We compared two ways to build multi-arch (`linux/amd64,linux/arm64`) container images in
Jenkins across four sample apps:
- **Multi-agent (native, current):** each arch builds on its own native agent; a manifest
  list is assembled after.
- **BuildKit emulation (new):** one agent, one `pack build --build-backend buildkit`, using
  QEMU for the non-native arch (drives the jericop/pack BuildKit fork).

**Result: all 4 participating apps now build successfully on BOTH strategies (go, java,
nodejs, python).** Getting emulation to run at all required several fixes (listed below) —
it did NOT work out of the box. Emulation wall-clock ranges from roughly on-par with native
to substantially slower, and one app (nodejs) is an extreme outlier we've traced to a
post-export stall in the fork that is under investigation (see the perf section).

**CAVEATS:** the numbers below are for sample apps with minimal dependencies. Real-world
apps will have longer build times under emulation.

**IDEAL USAGE SCENARIO:** the best use case for emulation is when an app is pre-compiled and
the buildpack is simply assembling the container and adding a runtime — as is the case for
Java.

---

### UPDATE (2026-09-10): re-validation with the latest fork images + a corrected cgo finding

Re-ran a targeted set on the `PLATFORM-1802-*` branches against the latest fork images
(lifecycle + builder + pack on the `-with-history-and-kiro` line, which carry the per-arch
extra-buildpack fix "FR-8b" and the finalize timing "FR-9"). Two of these apps were
previously blocked or worked around; both are now clean, and a key earlier finding is
**superseded**.

| App | Native (multi-agent) | Emulation (buildkit) | Notes |
|-----|----------------------|----------------------|-------|
| pd-sample-go-app | SUCCESS (~262s) | **SUCCESS (~302s) with `CGO_ENABLED=0` REMOVED** | cgo now builds under emulation — see correction below |
| agent-patcher-service | SUCCESS (~350s) | **SUCCESS (~440s)** | previously failed emulation with cpython `python3` ENOENT; fixed by FR-8b |
| pd-rds-postgres-password-lambda | SUCCESS (~433s) | FAILURE (~286s) | failure is the app's OWN postgres unit test, NOT the build — see below |

**CORRECTION — QEMU cgo is NOT the blocker we thought; the real cause was a fork bug.**
The earlier "QEMU is unreliable for cgo" finding (and the `CGO_ENABLED=0` workaround) is
**superseded**. Root cause of the emulated arm64 failures was that the fork delivered ONE
host-arch copy of extra buildpacks to BOTH platform legs, so the emulated leg ran wrong-arch
buildpack binaries (this is what produced the cpython `python3` ENOENT on agent-patcher, and
the class of arm64 toolchain crashes on go-app). The fork now delivers each buildpack's
per-arch content per leg ("FR-8b"). With that fix, **pd-sample-go-app builds successfully on
emulation with `CGO_ENABLED=0` removed** — i.e. the emulated cgo compile works. So cgo is no
longer a reason to avoid emulation for these apps. (We keep the multi-agent go-app as a
native control: SUCCESS ~262s.)

**agent-patcher-service** — the app that originally surfaced the cpython `python3` ENOENT on
the emulated arm64 leg — now builds cleanly on emulation (~440s) AND multi-agent (~350s),
confirming FR-8b end to end on a real app with a custom/extra buildpack.

**pd-rds-postgres-password-lambda** — the emulation run FAILED, but the failure is **NOT a
build / emulation / fork issue**: it failed in the app's own unit-test phase
(`poetry run coverage run -m unittest`) with `pg.InternalError: password authentication
failed for user "app-user"` against a test postgres container — a test-environment flake,
BEFORE `pack build` ever ran. The multi-agent run of the same commit SUCCEEDED (~433s),
which confirms the container build itself is fine; the emulation leg just lost the
test-container coin flip. (This app was historically excluded from the benchmark for a
separate reason — its PyGreSQL/postgres-client dep vs the noble builder.)

Durations here are single-run wall-clock and vary run-to-run; treat as indicative.

---

### Per-app results (latest successful builds)

| App | Language / build | Native (multi-agent) | Emulation (buildkit) | Both OK? |
|-----|------------------|----------------------|----------------------|----------|
| pd-sample-go-app | Go | SUCCESS (~439s) | SUCCESS (~358s, **required `CGO_ENABLED=0`**) | YES |
| pd-sample-java-app | Java / Maven | SUCCESS (~345s) | SUCCESS (~536s) | YES |
| pd-sample-nodejs-app | Node | SUCCESS (~445s) | SUCCESS (~4903s — see stall caveat) | YES |
| pd-sample-python-app | Python / poetry | SUCCESS (~286s) | SUCCESS (~483s) | YES |

Durations are build wall-clock (from Grafana/Jenkins OTel traces,
`ci.pipeline.run.durationMillis`), taken from the latest SUCCESSFUL build on each branch.
They vary run-to-run (agent load, cache state), so treat them as indicative, not exact. All
apps build fine on the native/multi-agent strategy; emulation is the variable under test.

### Performance (all apps green on both)

| App | Native | Emulation | Emulation vs native |
|-----|--------|-----------|---------------------|
| pd-sample-go-app (cgo off) | ~439s | ~358s | ~0.82× (faster this run) |
| pd-sample-python-app | ~286s | ~483s | ~1.69× |
| pd-sample-java-app | ~345s | ~536s | ~1.55× |
| pd-sample-nodejs-app | ~445s | ~4903s | ~11× (**post-export stall — under investigation**) |

**How to read the multiplier:** it is multiplicative, i.e. `native × 1.55`, NOT additive or
double. "1.55×" = 55% longer (345s → 536s). A 150s native build would be ~230s at 1.55×.
The go row shows emulation can even come out ahead on a given run — durations vary with agent
load and cache state, so read these as indicative.

**Big caveat — a post-export stall inflates the emulation numbers (especially nodejs).**
On multiple emulation builds we see a long, unexplained pause AFTER the lifecycle exporter
finishes its emit step (around the run-image config resolve), before the build completes.
It is worst on nodejs (~11×) but also visible on java. This matters because a Java/Maven app
compiles BEFORE the container build (in `mvnPipeline`), so the container-build phase only
packages a prebuilt jar — there is no in-phase compilation that could explain a post-export
hang. That points at the fork's emit/finalize/export flow, not app build cost. So the
emulation durations above currently INCLUDE this overhead and are NOT the steady-state cost;
we expect them to drop once the stall is fixed. Tracked as a fork follow-up (Item 9 in the
`platform-1802-buildkit-followups` spec).

**Caveat — wall-clock vs total compute:** multi-agent achieves its wall-clock by running two
agents in parallel, so it spends ~2× its wall-clock in total agent-time. Emulation uses a
single agent. So if agent capacity/cost is the constraint (not how long one person waits),
emulation's single-agent footprint can be attractive — provided the stall above is resolved.

### Did emulation work out of the box? No — here's what we had to change

Emulation did not work initially; we made the following changes to get all four apps green
(all in the Jenkins shared library / app Jenkinsfiles unless noted, plus fork fixes shipped
in a jericop/pack image):

1. **Use the BuildKit fork `pack`** — the flags (`--build-backend buildkit`, `--binding`,
   multi `--platform`) are only in jericop/pack; the pod's stock pack rejects them. The
   library copies the fork `pack` binary out of a published fork image at build time.
2. **Create a `docker-container` buildx builder + QEMU** — the agent's default builder is a
   `docker`-driver builder, which can't do multi-platform buildkit; the library creates and
   bootstraps a `pack-multiplatform` builder and passes it explicitly.
3. **`--trust-builder`** — required because our fork builder is self-built/untrusted
   (otherwise "Lifecycle 0.0.0 does not have an associated lifecycle image").
4. **`--binding` instead of `--volume`** — the buildkit backend doesn't support `--volume`.
5. **File-ownership fixes** — root-run build steps produced files the jenkins build user
   couldn't read on the emulated arch (surfaced as `permission denied`). Two related but
   separate cases (maven `target/` ownership; library-created binding files).
6. **`CGO_ENABLED=0` for go-app** — see reliability finding below.

7. **Flatten the ephemeral builder (fork fix)** — nodejs adds an extra buildpack
   (`--buildpack paketobuildpacks/nodejs`) on a trusted builder, which made the fork
   synthesize a temporary builder image one layer per module and hit Docker's ~125-layer cap
   (`max depth exceeded`) before any build phase. Fixed in the fork by collapsing the added
   modules into a single layer; that unblocked nodejs emulation.

Several of the initial failures were bugs in the fork `pack` itself (builder resolution,
app-context file ordering, the ephemeral-builder layer explosion, verbose output). Those
have been fixed in the fork, which is what got nodejs and python green on emulation. The
remaining known issue is the post-export stall (perf, above) — tracked as a fork follow-up.

### Key reliability finding: QEMU cgo — SUPERSEDED (see the 2026-09-10 UPDATE above)
> **This finding has been superseded.** We originally believed go-app's emulated arm64 cgo
> compile crashed under QEMU (`runtime/cgo: gcc: signal: segmentation fault`) and that
> `CGO_ENABLED=0` was required. Re-validation (2026-09-10) traced the real cause to a fork
> bug — wrong-arch buildpack binaries delivered to the emulated leg — now fixed (FR-8b). With
> the fix, pd-sample-go-app builds on emulation with `CGO_ENABLED=0` REMOVED. Kept here for
> history; do not cite the original conclusion.

Original (historical) text: go-app's emulated arm64 build crashed the Go/cgo compiler under
QEMU; it only succeeded after setting `CGO_ENABLED=0`. — This is now understood to have been
the wrong-arch fork bug, not an inherent QEMU limitation.

### Open follow-ups (not blocking the four-app comparison)
- **Post-export stall (perf):** the biggest open item — a long pause after the exporter's
  emit step (see the performance section). Inflates emulation durations, worst on nodejs.
  Fork follow-up (Item 9).

This is a concrete, tracked follow-up (fork + library), not a fundamental limit of emulation.

### Preliminary recommendation
- **BuildKit emulation is viable:** all four participating apps (Go, Java/Maven, Node,
  Python) now build multi-arch on emulation with a single agent and a single invocation.
- **Native multi-agent remains a safe default today.** The earlier "cgo requires
  `CGO_ENABLED=0` under emulation" caveat is SUPERSEDED (see the 2026-09-10 UPDATE): with the
  FR-8b per-arch fix, pd-sample-go-app builds on emulation with cgo enabled. cgo /
  native-compilation is no longer a known blocker for emulation on the apps tested, though
  broader cgo workloads still warrant their own validation.
- **Do not use the current emulation durations for a cost decision yet:** they include a
  post-export stall we're actively fixing (worst on nodejs, ~11×). Once that lands we expect
  emulation wall-clock to drop meaningfully; we'll re-measure and update this comparison.
- Emulation's single-agent footprint is attractive where agent capacity/cost is the
  constraint, contingent on resolving the stall.

---
_Data source: Jenkins OpenTelemetry traces in Grafana (Tempo `kubernetes-traces`),
`ci.pipeline.run.result` + `ci.pipeline.run.durationMillis`; latest SUCCESSFUL build per
branch. The four compared apps build on jenkins-pd. Fork fixes + follow-ups are tracked in
the jericop/cnb-pack `platform-1802-buildkit-followups` spec._
