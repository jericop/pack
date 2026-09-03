---
inclusion: manual
---

# CPython buildpack: build / package / publish / consume (PLATFORM-1662 troubleshooting)

How to build a CUSTOM `paketo-buildpacks/cpython` buildpack from the fork and use it in a
local `pack build` so we can add diagnostics and see exactly what the cpython buildpack does
(resolved dependency name/arch/url/source/checksum, extract path, symlinks, the commands it
runs — e.g. `python3 -m pip --version`). Used to investigate PLATFORM-1662 FR-8b (the cpython
`python3` failure: ENOENT on emulated arm64 in Jenkins / SIGTRAP on native arm64 locally).

## Repo + branch

- Source: `/Users/jpena/repos/jericop/cpython`
- Branch: `PLATFORM-1662-buildkit-emulation` (already created)
- Do NOT add tests to the cpython buildpack for this work. The Go binary must COMPILE, then
  be built + packaged + published. That's the whole loop.

## The loop

Run all steps from `/Users/jpena/repos/jericop/cpython`.

1. **Make the code change** (e.g. add diagnostics to `build.go` / `installer.go` /
   `pip_cleanup.go` / dependency resolution). Ensure it compiles:
   ```bash
   go build ./...
   ```
   (No tests are added or required.)

2. **Package** the buildpack (produces the buildpackage the CNB tooling consumes):
   ```bash
   ./scripts/package.sh -v 0.0.0
   ```

3. **Publish** it to the LOCAL registry (host port 5001; see `local-test-environment.md`):
   ```bash
   ./scripts/publish.sh \
     --image-ref localhost:5001/jericop/cpython:buildkit-emulation \
     --buildpack-type buildpack
   ```

4. **Copy to ttl.sh** so the buildkit builder can pull it on the regular docker network
   (the docker-container builder cannot resolve the `pack-test-registry`/localhost dev
   registry for an arbitrary `--buildpack` image, but it CAN pull from ttl.sh):
   ```bash
   crane cp localhost:5001/jericop/cpython:buildkit-emulation \
     ttl.sh/jericop/cpython:buildkit-emulation
   ```
   Note: `ttl.sh` images are ephemeral (short TTL); re-copy before each test session.

5. **Consume it in the build** by adding the extra buildpack flag:
   ```bash
   --buildpack ttl.sh/jericop/cpython:buildkit-emulation
   ```
   Adding `--buildpack <cpython>` overrides the builder's bundled cpython with this custom
   one (same id/version, higher precedence), so the diagnostics run in place of the stock
   cpython during detect/build.

## Notes / gotchas

- **Print unconditionally for troubleshooting.** `BP_LOG_LEVEL=DEBUG` passed via pack
  `--env-file` did NOT reliably reach the buildpack's DEBUG logger in the buildkit backend,
  so diagnostics we want to see MUST be emitted at normal log level (e.g. `logger.Process`/
  `logger.Subprocess`/plain stdout), not behind `logger.Debug`.
- Keep diagnostics loud but bounded (a few lines per interesting step): resolved dependency
  (id, version, arch/stack, URI, Source, checksum), whether `URI == Source` (extract vs
  compile), the extract destination, the exact symlink source→target for `bin/python`,
  `bin/python3`, `bin/python3.11`, a `readlink`/`os.Stat`/ELF-header sniff of the produced
  `python3`, and the exact argv + env (PATH/LD_LIBRARY_PATH) of every `python3` invocation
  (esp. the `BP_CPYTHON_RM_SETUPTOOLS` cleanup `python3 -m pip --version`).
- This custom buildpack is a TROUBLESHOOTING artifact, not a release. Revert/park the
  diagnostics once the root cause is found.
- Relationship: the failing step is gated by `BP_CPYTHON_RM_SETUPTOOLS` (set by
  agent-patcher-service). See the `platform-1662-buildkit-followups` spec FR-8b.
