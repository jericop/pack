package multiplatform

// On-disk OCI layout integration test (spec Task 7, Tier 2 "PRIMARY, no registry").
//
// This is the daemon-gated companion to the fast, synthetic-fixture unit tests
// in oci_layout_inspect_test.go. Those unit tests prove the extended
// InspectOCILayout surfaces everything the design's Tier 2 verification needs
// (layer count/order, diff IDs, image config, lifecycle metadata label, SBOM
// presence) against hand-built layouts, with no daemon. This file exercises the
// SAME verification against a REAL per-arch OCI layout produced by the LLB
// backend's Phase 1 solve — the actual "build a multi-arch app with the LLB
// backend in OCI layout mode, capture the per-arch OCI layout on disk, then read
// each layout directly from disk" path from Task 7.
//
// # Why this is gated and skipped by default
//
// The real Phase 1 solve requires a running BuildKit daemon (a docker-container
// buildx builder) plus a multi-arch builder image bundling the patched
// lifecycle and a sample app. None of that is available in the default unit-test
// environment, and standing it up is exactly the heavyweight setup the design's
// testing strategy keeps optional. So this test SKIPS unless explicitly enabled.
//
// # Gating: PACK_TEST_BUILDKIT_ENABLED
//
// Task 7 is Tier 2 (on-disk, NO registry), so the design's Tier 3 gate
// PACK_TEST_REGISTRY_ENABLED is deliberately NOT reused here — this test must
// never touch a registry. Instead it uses a BuildKit-only gate whose name
// mirrors the design's env-var convention:
//
//	PACK_TEST_BUILDKIT_ENABLED=1   (required) opt in to the daemon-backed test
//	PACK_TEST_BUILDKIT_BUILDER=... (optional) buildx builder name;
//	                               defaults to "pack-multiplatform"
//	PACK_TEST_BUILDER_IMAGE=...    (required when enabled) multi-arch builder
//	                               image bundling the patched lifecycle
//	PACK_TEST_APP_PATH=...         (required when enabled) local path to a sample
//	                               app to build
//	PACK_TEST_PLATFORMS=...        (optional) comma-separated platforms;
//	                               defaults to "linux/amd64,linux/arm64"
//	PACK_TEST_PLATFORM_API=...     (optional) CNB Platform API; defaults to "0.12"
//
// When PACK_TEST_BUILDKIT_ENABLED is set but a prerequisite is missing (no
// builder container running, or no builder image / app path provided) the test
// SKIPS with a clear message describing what is missing rather than FAILING.
// This keeps CI green (skip by default) while letting a developer with a builder
// run the real path by exporting the env vars above.
//
// It NEVER publishes and NEVER assembles a manifest list: it drives Phase 1 only
// (per-arch OCI layout to a content store on disk) and inspects the result. No
// push, no registry, no builder network dependency (Task 7 constraint).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

const (
	// envBuildkitEnabled opts in to the daemon-backed on-disk integration test.
	envBuildkitEnabled = "PACK_TEST_BUILDKIT_ENABLED"
	// envBuildkitBuilder overrides the buildx builder name (docker-container driver).
	envBuildkitBuilder = "PACK_TEST_BUILDKIT_BUILDER"
	// envBuilderImage is the multi-arch builder image (with patched lifecycle).
	envBuilderImage = "PACK_TEST_BUILDER_IMAGE"
	// envAppPath is the local path to a sample app to build.
	envAppPath = "PACK_TEST_APP_PATH"
	// envPlatforms overrides the platforms to build (comma-separated).
	envPlatforms = "PACK_TEST_PLATFORMS"
	// envPlatformAPI overrides the CNB Platform API version.
	envPlatformAPI = "PACK_TEST_PLATFORM_API"
	// envRunImage overrides the run image the analyzer resolves in OCI layout
	// mode. The lifecycle needs an explicit -run-image in layout mode (it has no
	// -pull-run-image flag), matching the pr-compliance-app CI which passes
	// --run-image paketobuildpacks/ubuntu-noble-run-tiny:latest.
	envRunImage = "PACK_TEST_RUN_IMAGE"

	defaultBuildkitBuilder = "pack-multiplatform"
	defaultPlatforms       = "linux/amd64,linux/arm64"
	defaultPlatformAPI     = "0.12"
	defaultRunImage        = "paketobuildpacks/ubuntu-noble-run-tiny:latest"
)

func TestOCILayoutOnDiskIntegration(t *testing.T) {
	spec.Run(t, "oci_layout_ondisk_integration", testOCILayoutOnDiskIntegration, spec.Report(report.Terminal{}))
}

func testOCILayoutOnDiskIntegration(t *testing.T, when spec.G, it spec.S) {
	when("building a multi-arch app with the LLB backend in OCI layout mode (no registry)", func() {
		it("captures a per-arch OCI layout on disk and verifies structure/config/metadata/SBOM", func() {
			// Skip unless explicitly enabled. This keeps the default `go test`
			// fast and free of BuildKit/builder requirements.
			h.SkipIf(t, os.Getenv(envBuildkitEnabled) == "",
				"on-disk OCI layout integration test is disabled; set "+envBuildkitEnabled+"=1 (plus "+
					envBuilderImage+" and "+envAppPath+") to run it")

			// Enabled but prerequisites missing: skip with a clear message rather
			// than fail, so a developer who only set the enable flag gets guidance.
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
			h.AssertEq(t, len(platforms) >= 2, true) // Task 7: multi-arch

			const targetImageName = "pack.local/oci-layout-ondisk-test:latest"

			logger := logging.NewSimpleLogger(os.Stderr)
			backend := NewLLBBackend(logger, BuildkitOpts{Builder: builderName})

			// Verify the BuildKit daemon / builder is reachable before attempting a
			// solve. If it is not, skip (prerequisite missing) rather than fail.
			ctx := context.Background()
			bkClient, err := backend.connectToBuildkit(ctx)
			if err != nil {
				t.Skipf("BuildKit builder %q is not reachable (%s); ensure it is running: docker buildx inspect --bootstrap %s",
					builderName, err, builderName)
			}
			defer bkClient.Close()

			// Each platform writes its Phase 1 OCI layout under its own content
			// store beneath this output dir (see perArchStoreDir). We own this dir
			// and remove it at the end — no registry, no push.
			outputDir := t.TempDir()

			opts := PlatformBuildOpts{
				BuilderImage: builderImage,
				AppPath:      appPath,
				ImageName:    targetImageName,
				BuildID:      "ondisk" + h.RandString(6),
				CacheID:      "oci-layout-ondisk-test",
				PlatformAPI:  platformAPI,
				BuilderUID:   1000,
				BuilderGID:   1000,
				OutputDir:    outputDir,
				// Task 7 constraint: OCI layout mode, no publish, no registry.
				ExportMode: ExportOCILayout,
				Publish:    false,
				Phases:     defaultLifecyclePhases(targetImageName),
			}

			// Drive Phase 1 ONLY for each platform: produce the per-arch OCI layout
			// on disk without a native push and without manifest-list assembly
			// (pushPerArch=false skips Phase 2; we do NOT call the assembly step).
			// This is the "capture the Phase 1 output / content store" from Task 7.
			results := make([]PlatformBuildResult, len(platforms))
			for i, platform := range platforms {
				res, err := backend.solvePlatform(ctx, bkClient, platform, opts, false /* pushPerArch */)
				h.AssertNil(t, err)
				h.AssertEq(t, res.OCIStoreDir != "", true)
				results[i] = res
			}

			// Read each per-arch OCI layout directly from disk and verify it.
			for _, res := range results {
				assertPerArchLayout(t, res)
			}
		})
	})
}

// assertPerArchLayout reads one platform's on-disk Phase 1 OCI layout via the
// extended InspectOCILayout and asserts everything the design's Tier 2
// verification calls out: layer count and order, each layer's diff ID, the image
// config fields, the io.buildpacks.lifecycle.metadata label, and SBOM presence.
func assertPerArchLayout(t *testing.T, res PlatformBuildResult) {
	t.Helper()

	inspection, err := InspectOCILayout(res.OCIStoreDir)
	h.AssertNil(t, err)
	h.AssertEq(t, inspection.Complete, true)

	// Layer count and order: a lifecycle-exported image has at least one layer,
	// and layer digests (manifest order) line up with diff IDs (config order).
	h.AssertEq(t, len(inspection.LayerDigests) > 0, true)
	h.AssertEq(t, inspection.LayersMatchDiffIDs(), true)

	// Each layer's diff ID is recorded.
	for _, diffID := range inspection.DiffIDs {
		h.AssertEq(t, strings.HasPrefix(diffID, "sha256:"), true)
	}

	// Image config: a launchable CNB image sets an entrypoint (the launcher) and
	// runs as a non-root user. We assert the fields are surfaced (non-empty) —
	// exact values depend on the builder/app, so we avoid over-constraining.
	h.AssertEq(t, len(inspection.Config.Entrypoint) > 0, true)
	h.AssertEq(t, inspection.Config.User != "", true)

	// The CNB lifecycle metadata label MUST be present on a real
	// lifecycle-produced layout, and its recorded diff IDs must parse.
	h.AssertEq(t, inspection.LifecycleMetadata.Present, true)
	_, hasLabel := inspection.Labels[lifecycleMetadataLabel]
	h.AssertEq(t, hasLabel, true)
	h.AssertEq(t, len(inspection.LifecycleMetadata.DiffIDs) > 0, true)

	// SBOM layer presence: the CNB SBOM is recorded in the lifecycle metadata.
	// A real lifecycle-produced image carries it, so HasSBOM must be true.
	h.AssertEq(t, inspection.LifecycleMetadata.HasSBOM, true)
	h.AssertEq(t, strings.HasPrefix(inspection.LifecycleMetadata.SBOMDiffID, "sha256:"), true)

	t.Logf("verified on-disk OCI layout for %s: %d layers, lifecycle metadata present, SBOM=%t",
		res.Platform.String(), len(inspection.LayerDigests), inspection.LifecycleMetadata.HasSBOM)
}

// defaultLifecyclePhases builds the ordered lifecycle phase commands the LLB
// backend runs. buildLifecyclePhaseArgs augments these with the OCI-layout /
// unprivileged flags per phase; here we only provide the base binary + args and
// the target image name (substituted with the per-arch tag by the backend).
func defaultLifecyclePhases(imageName string) []PhaseCommand {
	runImage := envOrDefault(envRunImage, defaultRunImage)
	return []PhaseCommand{
		// The analyzer needs an explicit run image in OCI layout mode (this
		// lifecycle has no -pull-run-image flag), so pass -run-image just like
		// pack's --run-image does on the real CLI path.
		{Name: "analyzer", Binary: "/cnb/lifecycle/analyzer", Args: []string{"-run-image", runImage, imageName}},
		{Name: "detector", Binary: "/cnb/lifecycle/detector", Args: []string{"-app", "/workspace"}},
		{Name: "restorer", Binary: "/cnb/lifecycle/restorer", Args: []string{"-cache-dir", "/cache"}},
		{Name: "builder", Binary: "/cnb/lifecycle/builder", Args: []string{"-app", "/workspace"}},
		{Name: "exporter", Binary: "/cnb/lifecycle/exporter", Args: []string{"-app", "/workspace", "-cache-dir", "/cache", imageName}},
	}
}

// envOrDefault returns the value of env var key, or def when it is unset/empty.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
