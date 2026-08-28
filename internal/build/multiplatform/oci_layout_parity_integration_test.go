package multiplatform

// Daemon-gated dual-build parity integration test (spec Task 8, Deliverable B,
// design "Tier 2 parity check — the confidence check, no registry").
//
// This is the daemon-backed companion to the fast, synthetic-fixture unit tests
// in oci_layout_parity_test.go. Those unit tests prove CompareParity flags
// exactly the perturbed field over controlled on-disk layouts. This file
// exercises the SAME CompareParity over TWO REAL on-disk OCI layouts produced by
// building the same app twice, then asserts they are at parity (FR-7/FR-8).
//
// # Gating (reuses Task 7's convention — see oci_layout_ondisk_integration_test.go)
//
// This test reuses the EXACT env-var gating from the Task 7 on-disk integration
// test (envBuildkitEnabled / envBuildkitBuilder / envBuilderImage / envAppPath /
// envPlatforms / envPlatformAPI and the default* consts) rather than redefining
// them. It is Tier 2 (BuildKit, NO registry), so the design's Tier 3
// PACK_TEST_REGISTRY_ENABLED gate is intentionally NOT used — this test never
// touches a registry.
//
//	PACK_TEST_BUILDKIT_ENABLED=1   (required) opt in to the daemon-backed test
//	PACK_TEST_BUILDER_IMAGE=...    (required when enabled) multi-arch builder image
//	PACK_TEST_APP_PATH=...         (required when enabled) local sample app path
//	PACK_TEST_BUILDKIT_BUILDER=... (optional) buildx builder; default pack-multiplatform
//	PACK_TEST_PLATFORMS=...        (optional) default linux/amd64,linux/arm64
//	PACK_TEST_PLATFORM_API=...     (optional) default 0.12
//
// When enabled but a prerequisite is missing (no builder image / app path, or the
// builder is unreachable) it SKIPS with a clear message rather than FAILING, so
// the default `go test` stays green and a developer with a builder can opt in.
//
// # What this test builds, and the documented limitation
//
// Task 8's design goal is to compare a REGISTRY-mode reference against the LLB
// OCI-layout output, both captured on disk without a live registry. A true
// cross-mode comparison (registry-mode Dockerfile MVP build vs LLB OCI-layout
// build) needs a materially larger harness: the Dockerfile backend pushes to a
// registry by design, so capturing its artifact on disk "without a registry"
// would require a daemon-save/local-output path this package does not yet wire
// up here.
//
// Per the task's explicit guidance ("If producing two genuinely different-mode
// builds is impractical without more harness, structure the test to build the
// app in OCI layout mode for BOTH the reference capture and the LLB output ...
// and clearly document the limitation, deferring true cross-mode (registry vs
// oci-layout) artifact parity to Task 10"), this test builds the SAME app TWICE
// via the LLB backend's Phase 1 OCI-layout path — once as the on-disk "reference
// capture" and once as the "LLB output" — and runs CompareParity over the two
// resulting on-disk layouts.
//
// This is a real, meaningful check: it proves the parity comparison holds over
// genuine lifecycle-produced layouts (identical recorded diff IDs, config, and
// labels for the same app), and it validates the Deliverable A machinery end to
// end against real data. What it deliberately does NOT do is compare a
// REGISTRY-mode artifact against an OCI-layout artifact — that stronger
// cross-mode / registry-hosted parity is Tier 3 and is DEFERRED to Task 10
// (registry-based integration test), which pulls the pushed per-arch images and
// compares them against a registry-mode build of the same app.

import (
	"context"
	"os"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestOCILayoutParityIntegration(t *testing.T) {
	spec.Run(t, "oci_layout_parity_integration", testOCILayoutParityIntegration, spec.Report(report.Terminal{}))
}

func testOCILayoutParityIntegration(t *testing.T, when spec.G, it spec.S) {
	when("building the same app twice on disk and comparing for parity (no registry)", func() {
		it("produces per-arch OCI layouts that are at parity (matching diff IDs, config, labels)", func() {
			// Skip unless explicitly enabled — keeps the default `go test` fast and
			// free of BuildKit/builder requirements (reuses Task 7's gate).
			h.SkipIf(t, os.Getenv(envBuildkitEnabled) == "",
				"dual-build parity integration test is disabled; set "+envBuildkitEnabled+"=1 (plus "+
					envBuilderImage+" and "+envAppPath+") to run it")

			// Enabled but prerequisites missing: skip with a clear message.
			builderImage := os.Getenv(envBuilderImage)
			appPath := os.Getenv(envAppPath)
			h.SkipIf(t, builderImage == "",
				envBuildkitEnabled+" is set but "+envBuilderImage+" is not; provide a multi-arch builder image bundling the patched lifecycle")
			h.SkipIf(t, appPath == "",
				envBuildkitEnabled+" is set but "+envAppPath+" is not; provide a local sample app path to build")
			if _, err := os.Stat(appPath); err != nil {
				t.Skipf("%s=%q is not accessible: %s", envAppPath, appPath, err)
			}

			builderName := envOrDefault(envBuildkitBuilder, defaultBuildkitBuilder)
			platformAPI := envOrDefault(envPlatformAPI, defaultPlatformAPI)

			platforms, err := ParsePlatforms(envOrDefault(envPlatforms, defaultPlatforms))
			h.AssertNil(t, err)
			h.AssertEq(t, len(platforms) >= 2, true) // multi-arch, mirroring Task 7

			logger := logging.NewSimpleLogger(os.Stderr)
			backend := NewLLBBackend(logger, BuildkitOpts{Builder: builderName})

			// Verify the builder is reachable before attempting a solve; skip (not
			// fail) if it is not — this is a "prereq missing" condition.
			ctx := context.Background()
			bkClient, err := backend.connectToBuildkit(ctx)
			if err != nil {
				t.Skipf("BuildKit builder %q is not reachable (%s); ensure it is running: docker buildx inspect --bootstrap %s",
					builderName, err, builderName)
			}
			defer bkClient.Close()

			// buildOnDisk drives Phase 1 ONLY (per platform) to a fresh output dir,
			// returning the per-arch results (each carrying its on-disk OCIStoreDir).
			// No push, no manifest-list assembly, no registry (pushPerArch=false and
			// we never call the assembly step) — exactly Task 8's "runs offline".
			buildOnDisk := func(t *testing.T, label string) []PlatformBuildResult {
				t.Helper()
				const targetImageName = "pack.local/oci-layout-parity-test:latest"
				opts := PlatformBuildOpts{
					BuilderImage: builderImage,
					AppPath:      appPath,
					ImageName:    targetImageName,
					BuildID:      "parity-" + label + "-" + h.RandString(6),
					CacheID:      "oci-layout-parity-test",
					PlatformAPI:  platformAPI,
					BuilderUID:   1000,
					BuilderGID:   1000,
					OutputDir:    t.TempDir(),
					ExportMode:   ExportOCILayout,
					Publish:      false,
					Phases:       defaultLifecyclePhases(targetImageName),
				}
				results := make([]PlatformBuildResult, len(platforms))
				for i, platform := range platforms {
					res, err := backend.solvePlatform(ctx, bkClient, platform, opts, false /* pushPerArch */)
					h.AssertNil(t, err)
					h.AssertEq(t, res.OCIStoreDir != "", true)
					results[i] = res
				}
				return results
			}

			// Build the SAME app twice on disk. "reference" stands in for the
			// registry-mode reference capture; "candidate" is the LLB OCI-layout
			// output. Both are real lifecycle-produced OCI layouts. (See the file
			// doc: true cross-mode registry-vs-oci-layout artifact parity is Tier 3,
			// deferred to Task 10.)
			referenceResults := buildOnDisk(t, "reference")
			candidateResults := buildOnDisk(t, "candidate")
			h.AssertEq(t, len(referenceResults), len(candidateResults))

			// Compare per platform, matching by os/arch/variant so a reordering
			// across the two builds does not cause a false mismatch.
			for _, refRes := range referenceResults {
				candRes, ok := findResultForPlatform(candidateResults, refRes.Platform)
				h.AssertEq(t, ok, true)

				report, err := CompareParityLayouts(refRes.OCIStoreDir, candRes.OCIStoreDir)
				h.AssertNil(t, err)
				if !report.Match() {
					t.Fatalf("parity mismatch for %s:\n%s", refRes.Platform.String(), report.Error())
				}
				t.Logf("parity OK for %s: %d layers match, config + labels + lifecycle-metadata diff IDs equal",
					refRes.Platform.String(), len(refRes.OCILayoutDigest))
			}
		})
	})
}

// findResultForPlatform returns the build result whose Platform matches want
// (os/arch/variant), so parity comparison pairs the correct per-arch layouts
// regardless of the order each build produced them in.
func findResultForPlatform(results []PlatformBuildResult, want Platform) (PlatformBuildResult, bool) {
	for _, r := range results {
		if r.Platform == want {
			return r, true
		}
	}
	return PlatformBuildResult{}, false
}
