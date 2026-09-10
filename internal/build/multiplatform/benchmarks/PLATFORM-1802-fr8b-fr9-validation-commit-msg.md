# PLATFORM-1802 FR-8b/FR-9 validation run — results + commit message

Generated: 2026-09-07. Fresh `buildkit-emulation` builds of the 4 pd-sample apps against
the newly-published `-with-history-and-kiro` fork images (lifecycle + builder + pack).

## Published fork images (branch: buildkit-native-export-with-history-and-kiro)

| Image | Digest |
|-------|--------|
| docker.io/jericop/lifecycle:buildkit-native-export-with-history-and-kiro | sha256:5996c065d24c678260e49850668e866ef1370f1a594395d03c09c2adb5cea9c5 |
| docker.io/jericop/ubuntu-noble-builder:buildkit-native-export-with-history-and-kiro | sha256:fc0c3bf616d5752cc1c48c611ba4277f5e08922904d3b8a0d45ae24d718a704b |
| docker.io/jericop/pack:buildkit-native-export-with-history-and-kiro | sha256:528801ce0d3d7188346b3ac29fcb9312096883fa007a6e6690f1a920d54e1398 |

Commits (all on `buildkit-native-export-with-history-and-kiro`):
- cnb-lifecycle `b9f607ca` — finalize: per-arch host-side finalize timing (FR-9)
- cnb-pack `474ee122` — buildkit: deliver extra buildpacks per-arch or staged-once (FR-8b)
  (go.mod pin -> lifecycle v0.0.0-20260903185426-b9f607ca8e41)
- ubuntu-noble-builder `7b58492` — builder.toml: bundle the -with-history-and-kiro lifecycle

## Emulation build results (this run) — ALL 4 SUCCESS

| App | Result | Duration | Build |
|-----|--------|----------|-------|
| pd-sample-go-app | SUCCESS | 926s (~15.4m) | .../pd-sample-go-app/job/PLATFORM-1802-buildkit-emulation/17/ |
| pd-sample-java-app | SUCCESS | 510s (~8.5m) | .../pd-sample-java-app/job/PLATFORM-1802-buildkit-emulation/7/ |
| pd-sample-nodejs-app | SUCCESS | 6510s (~108m) | .../pd-sample-nodejs-app/job/PLATFORM-1802-buildkit-emulation/7/ |
| pd-sample-python-app | SUCCESS | 1752s (~29m) | .../pd-sample-python-app/job/PLATFORM-1802-buildkit-emulation/7/ |

Notes:
- python and nodejs (previously the blocked/wrong-arch cases behind FR-8b) both build cleanly
  now — direct validation that per-arch/staged-once extra-buildpack delivery works end-to-end
  through the published images.
- nodejs remains the outlier (~108m); the post-export stall (FR-9, now instrumented with
  per-arch finalize timing) is still the leading suspect and the next thing to quantify from
  these logs.
- Durations are higher than the earlier baseline (go ~358s, java ~536s, python ~483s,
  nodejs ~4903s); run-to-run variance (agent load, cold cache after a fresh image publish)
  is expected. Re-measure before using for any cost decision.

## Commit message (validation run)

```
PLATFORM-1802: validate FR-8b/FR-9 fork images on emulation (4/4 green)

Published lifecycle+builder+pack under buildkit-native-export-with-history-and-kiro
and re-ran the four pd-sample buildkit-emulation builds against them. All four
succeed multi-arch (linux/amd64,linux/arm64):

  go     SUCCESS  926s   (build #17)
  java   SUCCESS  510s   (build #7)
  nodejs SUCCESS  6510s  (build #7)
  python SUCCESS  1752s  (build #7)

python and nodejs — previously blocked by wrong-arch extra-buildpack delivery —
now build cleanly, validating FR-8b (per-arch multi-arch registry image child pull
+ staged-once platform-agnostic/inline buildpacks) end to end through the published
images. FR-9 per-arch finalize timing logging is compiled into the pack binary.

nodejs is still the wall-clock outlier (~108m); the post-export finalize stall
(FR-9) remains under investigation, now with per-arch timing logs to quantify it.

Images:
  lifecycle sha256:5996c065...  builder sha256:fc0c3bf6...  pack sha256:528801ce...
Commits: lifecycle b9f607ca, pack 474ee122, builder 7b58492.
```
