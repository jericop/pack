package multiplatform

// Daemon/registry-gated rebase integration test (spec Task 9, Deliverable B,
// design "Rebase tests", FR-7).
//
// This is the daemon-backed companion to the fast, synthetic-fixture unit tests
// in oci_layout_rebase_test.go. Those unit tests prove the offline
// rebase-readiness precondition check (CheckRebaseReadiness /
// CheckMultiArchRebaseReadiness) reports Ready / not-ready correctly over
// controlled on-disk layouts. This file exercises the SAME readiness assertions
// against REAL per-arch OCI layouts produced by the LLB backend's Phase 1
// OCI-layout path, and documents where the ACTUAL rebase execution is exercised.
//
// # Gating (reuses Task 7's convention — see oci_layout_ondisk_integration_test.go)
//
// This test reuses the EXACT env-var gating from the Task 7 on-disk integration
// test (envBuildkitEnabled / envBuildkitBuilder / envBuilderImage / envAppPath /
// envPlatforms / envPlatformAPI and the default* consts) rather than redefining
// them. The offline rebase-readiness portion is Tier 2 (BuildKit, NO registry),
// so it runs under PACK_TEST_BUILDKIT_ENABLED alone:
//
//	PACK_TEST_BUILDKIT_ENABLED=1   (required) opt in to the daemon-backed test
//	PACK_TEST_BUILDER_IMAGE=...    (required when enabled) multi-arch builder image
//	PACK_TEST_APP_PATH=...         (required when enabled) local sample app path
//	PACK_TEST_BUILDKIT_BUILDER=... (optional) buildx builder; default pack-multiplatform
//	PACK_TEST_PLATFORMS=...        (optional) default linux/amd64,linux/arm64
//	PACK_TEST_PLATFORM_API=...     (optional) default 0.12
//
// # Local-first, registry only if the rebase path requires it (per the design)
//
// The design says: "validate against a locally-loaded image where possible; only
// use a registry if the rebase path requires remote layer mounting." Accordingly
// this test PREFERS a no-registry path: it drives Phase 1 ONLY (per platform) to
// produce on-disk OCI layouts and then asserts REBASE READINESS on them — the
// offline analogue of "rebase succeeds", per platform, with NO push and NO
// registry. Multi-arch readiness (both platforms rebase-ready) is asserted via
// CheckMultiArchRebaseReadiness — the offline analogue of "multi-arch rebase →
// both platforms rebased".
//
// # Why the ACTUAL rebase execution is deferred (documented limitation)
//
// Performing the REAL rebase needs pack's rebaser (pkg/client.Client.Rebase) or
// the lifecycle rebaser operating on a real image. pkg/client.Client.Rebase
// fetches the app image through an imageFetcher (daemon when !Publish, registry
// when Publish) and, for remote rebases, mounts run-image layers from a registry
// — none of which this multiplatform package wires up: it produces on-disk OCI
// layouts, not daemon-loaded images, and importing pkg/client here would create
// a heavy dependency/cycle plus require a running Docker daemon and a loaded
// image. Rather than build a flaky heavy harness, this test follows the task's
// guidance: it ALWAYS runs the offline rebase-readiness assertions when the build
// succeeds (the meaningful Task 9 coverage), and SKIPS-with-a-clear-message the
// actual-rebase execution, documenting that full rebase execution is exercised
// by the rebaser / acceptance suite (and, for the stronger registry-hosted path,
// by Task 10). The env vars that WOULD drive an actual rebase here are recognized
// and documented so a future harness can opt in:
//
//	PACK_TEST_REGISTRY_ENABLED=1   (Tier 3) opt in to a registry-backed rebase
//	PACK_TEST_REGISTRY_REF=...     (Tier 3) registry ref to publish/rebase against
//	PACK_TEST_NEW_RUN_IMAGE=...    the new run image to rebase onto
//
// When these are set the test still does not execute the rebase from within this
// package (see above); it logs that the actual-rebase execution remains deferred
// to the rebaser/acceptance suite, so setting them never causes a failure.

import (
	"context"
	"os"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

const (
	// envRegistryEnabled opts in to the Tier 3 registry-backed rebase path
	// (design "Tier 3"). Recognized here so a future harness can drive an actual
	// registry rebase; this package defers the execution (see the file doc).
	envRegistryEnabled = "PACK_TEST_REGISTRY_ENABLED"
	// envRegistryRef is the registry ref to publish/rebase against (Tier 3).
	envRegistryRef = "PACK_TEST_REGISTRY_REF"
	// envNewRunImage is the new run image to rebase onto, when an actual rebase
	// entry point is available.
	envNewRunImage = "PACK_TEST_NEW_RUN_IMAGE"
)

func TestOCILayoutRebaseIntegration(t *testing.T) {
	spec.Run(t, "oci_layout_rebase_integration", testOCILayoutRebaseIntegration, spec.Report(report.Terminal{}))
}

func testOCILayoutRebaseIntegration(t *testing.T, when spec.G, it spec.S) {
	when("building a multi-arch app on disk and verifying rebase readiness (local-first, no registry)", func() {
		it("asserts every per-arch layout is rebase-ready and defers actual rebase with a clear message", func() {
			// Skip unless explicitly enabled — keeps the default `go test` fast and
			// free of BuildKit/builder requirements (reuses Task 7's gate).
			h.SkipIf(t, os.Getenv(envBuildkitEnabled) == "",
				"rebase integration test is disabled; set "+envBuildkitEnabled+"=1 (plus "+
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

			const targetImageName = "pack.local/oci-layout-rebase-test:latest"

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

			outputDir := t.TempDir()
			opts := PlatformBuildOpts{
				BuilderImage: builderImage,
				AppPath:      appPath,
				ImageName:    targetImageName,
				BuildID:      "rebase" + h.RandString(6),
				CacheID:      "oci-layout-rebase-test",
				PlatformAPI:  platformAPI,
				BuilderUID:   1000,
				BuilderGID:   1000,
				OutputDir:    outputDir,
				// Local-first: OCI layout mode, no publish, no registry.
				ExportMode: ExportOCILayout,
				Publish:    false,
				Phases:     defaultLifecyclePhases(targetImageName),
			}

			// Drive Phase 1 ONLY per platform: produce per-arch on-disk OCI layouts
			// (pushPerArch=false, no assembly, no push, no registry).
			results := make([]PlatformBuildResult, len(platforms))
			for i, platform := range platforms {
				res, err := backend.solvePlatform(ctx, bkClient, platform, opts, false /* pushPerArch */)
				h.AssertNil(t, err)
				h.AssertEq(t, res.OCIStoreDir != "", true)
				results[i] = res
			}

			// Offline rebase-readiness per platform — the local, no-registry
			// analogue of "rebase an LLB-OCI-layout-built image → verify success".
			// A real lifecycle-produced layout records a coherent run-image
			// boundary (runImage.topLayer + reference among the config diff IDs)
			// and preserved app/launcher/buildpack layers, so each MUST be ready.
			for _, res := range results {
				readiness, err := CheckRebaseReadinessLayout(res.OCIStoreDir)
				h.AssertNil(t, err)
				if !readiness.Ready {
					t.Fatalf("per-arch layout for %s is not rebase-ready:\n%s", res.Platform.String(), readiness.Error())
				}
				t.Logf("rebase-ready: %s (runImage.topLayer + reference recorded, boundary coherent, preserved layers present)",
					res.Platform.String())
			}

			// Multi-arch readiness — the offline analogue of "multi-arch rebase →
			// both platforms rebased": EVERY per-arch layout must be rebase-ready.
			multi := CheckMultiArchRebaseReadiness(results)
			if !multi.Ready {
				t.Fatalf("multi-arch rebase readiness failed:\n%s", multi.Error())
			}
			t.Logf("multi-arch rebase readiness OK across %d platform(s)", len(multi.PerPlatform))

			// Actual rebase execution: deferred. Log a clear message (never fail),
			// documenting where the real rebase is exercised. If the Tier 3 / new
			// run image env vars are set, note that this package still defers the
			// execution rather than wiring pack's rebaser here (see the file doc).
			newRunImage := os.Getenv(envNewRunImage)
			switch {
			case os.Getenv(envRegistryEnabled) != "" && newRunImage != "":
				t.Logf("actual rebase execution deferred: %s and %s are set (registry ref=%q, new run image=%q), "+
					"but this multiplatform package validates rebase READINESS on-disk and does not wire pack's rebaser "+
					"(pkg/client.Client.Rebase requires a daemon-loaded/registry image). Full rebase execution is exercised "+
					"by the rebaser / acceptance suite and by Task 10's registry-based integration test.",
					envRegistryEnabled, envNewRunImage, os.Getenv(envRegistryRef), newRunImage)
			case newRunImage != "":
				t.Logf("actual rebase execution deferred: %s=%q is set but no rebase entry point is wired in this package; "+
					"rebase readiness was verified on-disk. Full rebase execution is exercised by the rebaser / acceptance suite.",
					envNewRunImage, newRunImage)
			default:
				t.Logf("actual rebase execution deferred: rebase READINESS verified on-disk for all platforms (local, no registry). "+
					"To exercise a real rebase, provide %s (and, for the registry path, %s=1 + %s); note this package defers the "+
					"execution to the rebaser / acceptance suite regardless (see file doc).",
					envNewRunImage, envRegistryEnabled, envRegistryRef)
			}
		})
	})
}
