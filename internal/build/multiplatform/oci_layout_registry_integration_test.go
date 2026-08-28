package multiplatform

// Registry-based Tier 3 integration test (spec Task 10, design "Tier 3:
// Registry-based integration tests (OPTIONAL, env-var gated)", FR-1, FR-2, FR-5).
//
// This is the OPTIONAL, env-var-gated end-to-end verification of the LLB backend's
// native publish path. Unlike the Tier 2 tests (Tasks 7-9, which are on-disk and
// NEVER touch a registry), this test actually PUBLISHES a multi-arch app to a
// registry via the LLB backend in OCI layout mode and then inspects the
// registry-hosted artifacts. It exists to prove, against a real registry, the two
// guarantees the whole spec is about:
//
//   - NO intermediate per-arch tags land on the registry (FR-5) — only the final
//     manifest-list tag appears.
//   - The pushed manifest list has exactly one entry per requested platform, each
//     with the correct os/arch(/variant) descriptor, and each per-platform image
//     pulls and is a well-formed launchable CNB image.
//
// # Package-level equivalent of the CLI flags
//
// The task frames this as "build a multi-arch app with
// `--build-backend=buildkit-llb --buildkit-export-mode=oci-layout --publish`".
// This test drives the LLB backend DIRECTLY (NewLLBBackend + BuildMultiPlatform)
// rather than shelling out to the pack CLI, because that is the package-level
// equivalent and keeps the test inside the `multiplatform` package it verifies.
// The mapping is:
//
//	--build-backend=buildkit-llb      -> NewLLBBackend(...)
//	--buildkit-export-mode=oci-layout -> PlatformBuildOpts.ExportMode = ExportOCILayout
//	--publish                         -> PlatformBuildOpts.Publish = true, ImageName = <registry ref>
//	--platforms linux/amd64,linux/arm64 -> platforms passed to BuildMultiPlatform
//
// In OCI layout mode the LLB backend's BuildMultiPlatform runs Phase 1 per
// platform (produce the per-arch OCI layout in its own content store, NO push)
// and then, for multi-arch, assembles + pushes ONE manifest list atomically via
// PushPerArchLayoutsAsManifestList (go-containerregistry remote.WriteIndex) — so
// no "<img>-build-<id>-<arch>" intermediate tag is ever written. Single-arch uses
// the native ExporterImage push under the final name. Either way there are no
// intermediate tags, which is exactly what this test asserts against the live
// registry.
//
// # Gating: env-var, SKIPPED BY DEFAULT (design "Tier 3", NFR-5)
//
// This test MUST be skipped unless the Tier 3 registry env vars are set. It
// reuses the Tier 3 gating consts declared alongside the Task 9 rebase test
// (envRegistryEnabled = PACK_TEST_REGISTRY_ENABLED, envRegistryRef =
// PACK_TEST_REGISTRY_REF) and the Task 7 builder/app consts (envBuildkitEnabled,
// envBuilderImage, envAppPath, envBuildkitBuilder, envPlatforms, envPlatformAPI)
// — NOTHING is redefined here.
//
//	PACK_TEST_REGISTRY_ENABLED=1   (required) opt in to the Tier 3 registry test.
//	                               UNSET => this test SKIPS (the default).
//	PACK_TEST_REGISTRY_REF=<ref>   (required when enabled) the target image ref to
//	                               publish to, e.g. docker.io/you/app:multiarch or
//	                               ghcr.io/you/app:latest or test-registry:5000/app:latest.
//	PACK_TEST_BUILDER_IMAGE=<img>  (required when enabled) multi-arch builder image
//	                               bundling the patched lifecycle.
//	PACK_TEST_APP_PATH=<dir>       (required when enabled) local sample app to build.
//	PACK_TEST_BUILDKIT_BUILDER=... (optional) buildx builder name; default pack-multiplatform.
//	PACK_TEST_PLATFORMS=...        (optional) comma-separated platforms; default linux/amd64,linux/arm64.
//	PACK_TEST_PLATFORM_API=...     (optional) CNB Platform API; default 0.12.
//
// When PACK_TEST_REGISTRY_ENABLED is set but a prerequisite is missing (no
// registry ref, no builder image, no app path, an inaccessible app path, or an
// unreachable builder) the test SKIPS with a clear message describing what is
// missing rather than FAILING. That keeps CI green (skip by default) while letting
// a developer with a reachable registry run the real end-to-end path.
//
// Registry credentials: the LLB backend's native push uses a Docker auth session
// provider seeded from the default Docker config (see newDockerAuthProvider), and
// the manifest-list assembly path (PushPerArchLayoutsAsManifestList) authenticates
// via go-containerregistry's DefaultKeychain (also the Docker config). So
// `docker login <registry>` before running is sufficient for both. The inspection
// side of this test pulls with remote.WithAuthFromKeychain(authn.DefaultKeychain)
// for the same reason.
//
// # Registry setup options (design "Tier 3", documented for the operator)
//
// Two ways to provide a registry the builder can reach:
//
//	(a) PREFERRED — a real registry the builder can already reach (Docker Hub /
//	    GHCR / ECR scratch repo), as the pr-compliance-app CI does. Example:
//
//	        docker login ghcr.io
//	        export PACK_TEST_REGISTRY_ENABLED=1
//	        export PACK_TEST_REGISTRY_REF=ghcr.io/you/pack-multiarch-test:latest
//	        export PACK_TEST_BUILDER_IMAGE=you/multiarch-builder:latest
//	        export PACK_TEST_APP_PATH=/path/to/sample/app
//	        go test ./internal/build/multiplatform/ -run TestOCILayoutRegistryIntegration -v
//
//	(b) ALTERNATIVE — an ephemeral registry on the builder's SHARED network. The
//	    docker-container buildx driver runs on its own network and CANNOT reach a
//	    host-local registry unless the builder was created on the same network.
//	    Reference the registry by its CONTAINER NAME (NFR-4 caveat):
//
//	        docker network create pack-test
//	        docker run -d --name test-registry --network pack-test -p 5000:5000 registry:2
//	        docker buildx create --name pack-multiplatform --driver docker-container \
//	          --driver-opt network=pack-test --bootstrap
//	        export PACK_TEST_REGISTRY_ENABLED=1
//	        export PACK_TEST_REGISTRY_REF=test-registry:5000/app:latest
//	        export PACK_TEST_BUILDKIT_BUILDER=pack-multiplatform
//	        # (plus PACK_TEST_BUILDER_IMAGE / PACK_TEST_APP_PATH as above)
//
//	    CAVEAT: the builder MUST be created with --driver-opt network=pack-test or
//	    it cannot reach test-registry:5000.
//
// # Documented limitations / deferrals
//
//   - "Runs correctly": actually starting a container and asserting its exit code
//     requires a Docker daemon run/exec and a run environment this package does
//     not wire up. Instead, for each per-platform image this test asserts the
//     image PULLS and is a WELL-FORMED LAUNCHABLE CNB image (config readable, an
//     entrypoint set = the launcher, a non-root user, layers readable). Full
//     run-execution ("docker run ... && assert exit") is exercised by the
//     acceptance suite; this is documented here and in tasks.md.
//   - Stronger cross-mode parity: the IDEAL is to also build the SAME app in
//     registry mode (the Dockerfile MVP backend) and compare the registry-hosted
//     per-arch artifacts across modes. A full registry-mode dual-build needs a
//     larger harness (the Dockerfile backend's own publish path + a second build).
//     What this test actually asserts is the achievable, meaningful parity: it
//     pulls each LLB-pushed per-arch image from the registry and compares it
//     against the on-disk Phase 1 layout the SAME build produced (matching config
//     RootFS diff IDs, config runtime fields, and non-lifecycle labels), proving
//     the artifacts that landed on the registry are exactly what was produced on
//     disk. The full registry-mode-vs-LLB cross-build comparison is documented as
//     the ideal and left to a future harness / the acceptance suite.
//   - RFC note: "Document the env var(s) and setup; note in the RFC when the
//     feature is working" — the RFC update itself is deferred (Task 11 / project
//     decision). The env vars and setup are documented in this file header; the
//     RFC note is deferred.
//   - FUTURE (not now): auto-detecting whether the current builder supports local
//     network access (to reach a local ephemeral registry) and enabling these
//     tests automatically instead of the env var is deferred — it is harder to
//     implement reliably (inspecting builder driver options / network config). The
//     env-var gate is the pragmatic starting point.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	ggcrname "github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestOCILayoutRegistryIntegration(t *testing.T) {
	spec.Run(t, "oci_layout_registry_integration", testOCILayoutRegistryIntegration, spec.Report(report.Terminal{}))
}

func testOCILayoutRegistryIntegration(t *testing.T, when spec.G, it spec.S) {
	when("publishing a multi-arch app via the LLB backend in OCI layout mode (registry, Tier 3)", func() {
		it("pushes only the final manifest-list tag (no intermediate tags), one entry per platform, each image pulls and is a well-formed launchable image", func() {
			// 1. GATE. Skip unless the Tier 3 registry test is explicitly enabled.
			// This is the design's "skipped by default" (NFR-5): an unset
			// PACK_TEST_REGISTRY_ENABLED means this heavy, network-touching test
			// never runs in a normal `go test`.
			h.SkipIf(t, os.Getenv(envRegistryEnabled) == "",
				"registry integration test is disabled; set "+envRegistryEnabled+"=1 (plus "+envRegistryRef+", "+
					envBuilderImage+", and "+envAppPath+") to run it")

			// Enabled but prerequisites missing: SKIP with a clear message (never
			// FAIL), so a developer who only set the enable flag gets guidance.
			registryRef := os.Getenv(envRegistryRef)
			builderImage := os.Getenv(envBuilderImage)
			appPath := os.Getenv(envAppPath)
			h.SkipIf(t, registryRef == "",
				envRegistryEnabled+" is set but "+envRegistryRef+" is not; provide a target image ref the builder can reach (e.g. ghcr.io/you/app:latest or test-registry:5000/app:latest)")
			h.SkipIf(t, builderImage == "",
				envRegistryEnabled+" is set but "+envBuilderImage+" is not; provide a multi-arch builder image bundling the patched lifecycle")
			h.SkipIf(t, appPath == "",
				envRegistryEnabled+" is set but "+envAppPath+" is not; provide a local sample app path to build")
			if _, err := os.Stat(appPath); err != nil {
				t.Skipf("%s=%q is not accessible: %s", envAppPath, appPath, err)
			}

			builderName := envOrDefault(envBuildkitBuilder, defaultBuildkitBuilder)
			platformAPI := envOrDefault(envPlatformAPI, defaultPlatformAPI)

			platforms, err := ParsePlatforms(envOrDefault(envPlatforms, defaultPlatforms))
			h.AssertNil(t, err)
			h.AssertEq(t, len(platforms) >= 2, true) // Tier 3 verifies a multi-arch manifest list

			logger := logging.NewSimpleLogger(os.Stderr)
			backend := NewLLBBackend(logger, BuildkitOpts{Builder: builderName})

			// Skip (not fail) if the builder is unreachable — a "prereq missing"
			// condition, not a test failure.
			ctx := context.Background()
			bkClient, err := backend.connectToBuildkit(ctx)
			if err != nil {
				t.Skipf("BuildKit builder %q is not reachable (%s); ensure it is running: docker buildx inspect --bootstrap %s",
					builderName, err, builderName)
			}
			bkClient.Close()

			// Parse the registry ref up front so a malformed ref skips clearly
			// rather than failing deep in the push.
			targetRef, err := ggcrname.ParseReference(registryRef)
			if err != nil {
				t.Skipf("%s=%q is not a valid image reference: %s", envRegistryRef, registryRef, err)
			}
			repo := targetRef.Context()

			// Snapshot the tags that already exist in the target repository BEFORE
			// the build, so the "no intermediate tags" assertion is robust to a
			// repo that already contains unrelated tags (we only reason about tags
			// this build could have created).
			tagsBefore := listRepoTags(t, repo)

			// 2. BUILD + PUBLISH. The package-level equivalent of
			// `--build-backend=buildkit-llb --buildkit-export-mode=oci-layout --publish`.
			// OutputDir holds the per-arch Phase 1 layouts on disk; they are ALSO
			// used below for the on-disk-vs-registry parity comparison.
			outputDir := t.TempDir()
			opts := PlatformBuildOpts{
				BuilderImage: builderImage,
				AppPath:      appPath,
				ImageName:    registryRef, // publish target = final manifest-list ref
				BuildID:      "registry" + h.RandString(6),
				CacheID:      "oci-layout-registry-test",
				PlatformAPI:  platformAPI,
				BuilderUID:   1000,
				BuilderGID:   1000,
				OutputDir:    outputDir,
				ExportMode:   ExportOCILayout, // --buildkit-export-mode=oci-layout
				Publish:      true,            // --publish
				Phases:       defaultLifecyclePhases(registryRef),
			}

			// BuildMultiPlatform runs Phase 1 per platform then assembles + pushes
			// ONE manifest list under registryRef (multi-arch) with NO intermediate
			// tags — the native publish path this test verifies end to end.
			results, err := backend.BuildMultiPlatform(ctx, platforms, opts)
			h.AssertNil(t, err)
			h.AssertEq(t, len(results), len(platforms))

			// 3. NO INTERMEDIATE TAGS. List the repo's tags AFTER the build and
			// assert the build introduced ONLY the final manifest-list tag — no
			// "<img>-build-<buildID>-<arch>" intermediate tags exist. (remote.List
			// lists tag NAMES in the repo; the intermediate per-arch tag shape from
			// registry mode is "<img>-build-<id>-<arch>".)
			tagsAfter := listRepoTags(t, repo)
			newTags := diffNewTags(tagsBefore, tagsAfter)

			finalTag := refTag(targetRef)
			// Every tag this build created must be the single final tag; assert no
			// intermediate build/arch tags leaked onto the registry (FR-5).
			for _, tag := range newTags {
				if isIntermediateBuildTag(tag, opts.BuildID) {
					t.Fatalf("intermediate tag leaked onto registry: %q (build ID %q); FR-5 requires NO intermediate per-arch tags", tag, opts.BuildID)
				}
			}
			// And, more strongly, if the ref was tag-qualified, the ONLY new tag
			// must be that final tag.
			if finalTag != "" {
				for _, tag := range newTags {
					if tag != finalTag {
						t.Fatalf("unexpected tag created by build: %q (expected only the final manifest-list tag %q)", tag, finalTag)
					}
				}
			}
			t.Logf("no intermediate tags: build created %d new tag(s) %v (final tag %q, build ID %q)",
				len(newTags), newTags, finalTag, opts.BuildID)

			// 4. MANIFEST LIST HAS ONE ENTRY PER PLATFORM. Pull the pushed ref as an
			// index and assert one manifest entry per requested platform with the
			// correct os/arch(/variant) descriptor.
			idx, err := remote.Index(targetRef, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
			h.AssertNil(t, err)
			idxManifest, err := idx.IndexManifest()
			h.AssertNil(t, err)

			h.AssertEq(t, len(idxManifest.Manifests), len(platforms))
			for _, want := range platforms {
				desc, ok := findManifestForPlatform(idxManifest, want)
				if !ok {
					t.Fatalf("manifest list is missing an entry for platform %s", want.String())
				}
				h.AssertNotNil(t, desc.Platform)
				h.AssertEq(t, desc.Platform.OS, want.OS)
				h.AssertEq(t, desc.Platform.Architecture, want.Arch)
				h.AssertEq(t, desc.Platform.Variant, want.Variant)
			}
			t.Logf("manifest list %s has %d entries, one per platform", registryRef, len(idxManifest.Manifests))

			// 5. EACH PLATFORM IMAGE PULLS AND IS A WELL-FORMED LAUNCHABLE IMAGE.
			// For each requested platform pull the child image from the registry
			// (resolving the per-platform child manifest by descriptor), assert its
			// config + layers are readable, and assert it is a launchable CNB image
			// (entrypoint = launcher, non-root user). Full "docker run" execution is
			// deferred to the acceptance suite (documented above).
			for _, want := range platforms {
				desc, ok := findManifestForPlatform(idxManifest, want)
				h.AssertEq(t, ok, true)

				childRef := repo.Digest(desc.Digest.String())
				img, err := remote.Image(childRef, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
				if err != nil {
					t.Fatalf("pulling per-platform image for %s (%s): %s", want.String(), childRef, err)
				}

				cfgFile, err := img.ConfigFile()
				h.AssertNil(t, err)
				// Well-formed launchable CNB image: an entrypoint (the launcher) and
				// a non-root user.
				h.AssertEq(t, len(cfgFile.Config.Entrypoint) > 0, true)
				h.AssertEq(t, cfgFile.Config.User != "" && cfgFile.Config.User != "0" && cfgFile.Config.User != "root", true)

				// Layers are readable (the blobs actually exist on the registry).
				layers, err := img.Layers()
				h.AssertNil(t, err)
				h.AssertEq(t, len(layers) > 0, true)
				for _, l := range layers {
					rc, err := l.Compressed()
					if err != nil {
						t.Fatalf("layer blob unreadable on registry for %s: %s", want.String(), err)
					}
					_ = rc.Close()
				}
				t.Logf("per-platform image for %s pulls; %d layers, launchable (entrypoint set, non-root user %q)",
					want.String(), len(layers), cfgFile.Config.User)
			}

			// 6. STRONGER PARITY (achievable form): confirm the artifacts that
			// landed on the registry are exactly what this build produced ON DISK.
			// For each platform, pull the registry-hosted per-arch image and compare
			// it against the SAME build's Phase 1 on-disk OCI layout using the
			// existing parity machinery (config RootFS diff IDs, config runtime
			// fields, non-lifecycle labels; the lifecycle-metadata label's parity is
			// via extracted diff IDs). This proves identical artifacts on the
			// registry, not just matching on-disk diff IDs.
			//
			// DOCUMENTED LIMITATION: the IDEAL "stronger parity" also builds the app
			// in registry mode (Dockerfile MVP) and compares registry-hosted
			// artifacts ACROSS modes. That full cross-mode registry dual-build needs
			// a larger harness and is left to a future harness / the acceptance
			// suite; here we assert the achievable, meaningful registry-vs-on-disk
			// parity of the LLB-pushed artifacts.
			for _, res := range results {
				onDisk, err := InspectOCILayout(res.OCIStoreDir)
				h.AssertNil(t, err)

				desc, ok := findManifestForPlatform(idxManifest, res.Platform)
				h.AssertEq(t, ok, true)
				childRef := repo.Digest(desc.Digest.String())
				img, err := remote.Image(childRef, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
				h.AssertNil(t, err)

				registryInspection, err := inspectionFromRemoteImage(childRef.String(), img)
				h.AssertNil(t, err)

				report := CompareParity(onDisk, registryInspection)
				if !report.Match() {
					t.Fatalf("registry-vs-on-disk parity mismatch for %s:\n%s", res.Platform.String(), report.Error())
				}
				t.Logf("registry artifact matches on-disk Phase 1 layout for %s (config diff IDs, config fields, labels equal)", res.Platform.String())
			}
		})
	})
}

// listRepoTags lists the tag names in repo via remote.List. It returns an empty
// slice (and does NOT fail the test) when the repository does not yet exist —
// remote.List errors for an absent repo, which simply means "no tags yet", the
// expected state before the first push.
func listRepoTags(t *testing.T, repo ggcrname.Repository) []string {
	t.Helper()
	tags, err := remote.List(repo, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		// A not-found / empty repository is normal before the first push; treat
		// any listing error as "no tags known" rather than a hard failure, since
		// the "no intermediate tags" assertion works on the BEFORE/AFTER diff.
		t.Logf("listing tags for %s returned %s (treating as no tags yet)", repo.Name(), err)
		return nil
	}
	return tags
}

// diffNewTags returns the tags present in after but not in before, i.e. the tags
// the build created. Order is not significant.
func diffNewTags(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, t := range before {
		seen[t] = struct{}{}
	}
	var added []string
	for _, t := range after {
		if _, ok := seen[t]; !ok {
			added = append(added, t)
		}
	}
	return added
}

// refTag returns the tag portion of a parsed reference, or "" when the reference
// is by digest (no tag) — in which case the "only the final tag" assertion is
// skipped in favor of the intermediate-tag-shape check.
func refTag(ref ggcrname.Reference) string {
	if tagged, ok := ref.(ggcrname.Tag); ok {
		return tagged.TagStr()
	}
	return ""
}

// isIntermediateBuildTag reports whether tag looks like a per-arch intermediate
// build tag of the shape the registry-mode path uses: "<img>-build-<buildID>-<arch>".
// The tag names listed by remote.List are the trailing tag component only, so we
// match on the "build-<buildID>" segment that this build would have used.
func isIntermediateBuildTag(tag, buildID string) bool {
	return strings.Contains(tag, "build-"+buildID) || strings.Contains(tag, "-build-"+buildID+"-")
}

// findManifestForPlatform returns the child manifest descriptor in idxManifest
// whose platform matches want (os/arch/variant), so per-platform assertions pair
// the correct registry-hosted image regardless of manifest order.
func findManifestForPlatform(idxManifest *v1.IndexManifest, want Platform) (v1.Descriptor, bool) {
	for _, m := range idxManifest.Manifests {
		if m.Platform == nil {
			continue
		}
		if m.Platform.OS == want.OS && m.Platform.Architecture == want.Arch && m.Platform.Variant == want.Variant {
			return m, true
		}
	}
	return v1.Descriptor{}, false
}

// inspectionFromRemoteImage builds an OCILayoutInspection-equivalent view from a
// registry-pulled image so the existing CompareParity machinery can compare a
// registry-hosted artifact against an on-disk Phase 1 layout. It mirrors the
// fields InspectOCILayout populates from an on-disk layout: config RootFS diff
// IDs, the runtime config fields, the labels map, and the parsed lifecycle
// metadata. It sets Complete=true because remote.Image already resolved the
// manifest + config; layer readability is asserted separately by the caller.
func inspectionFromRemoteImage(ref string, img v1.Image) (OCILayoutInspection, error) {
	inspection := OCILayoutInspection{LayoutDir: ref, Complete: true}

	digest, err := img.Digest()
	if err != nil {
		return OCILayoutInspection{}, err
	}
	inspection.ManifestDigest = digest.String()

	manifest, err := img.Manifest()
	if err != nil {
		return OCILayoutInspection{}, err
	}
	inspection.MediaType = string(manifest.MediaType)
	inspection.ConfigDigest = manifest.Config.Digest.String()

	configFile, err := img.ConfigFile()
	if err != nil {
		return OCILayoutInspection{}, err
	}
	for _, d := range configFile.RootFS.DiffIDs {
		inspection.DiffIDs = append(inspection.DiffIDs, d.String())
	}
	inspection.Config = imageConfigFields(configFile.Config)
	inspection.Labels = configFile.Config.Labels
	inspection.LifecycleMetadata = parseLifecycleMetadata(configFile.Config.Labels)

	for _, l := range manifest.Layers {
		inspection.LayerDigests = append(inspection.LayerDigests, l.Digest.String())
	}
	return inspection, nil
}
