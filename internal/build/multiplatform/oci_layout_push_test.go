package multiplatform

import (
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestOCILayoutPush(t *testing.T) {
	spec.Run(t, "oci_layout_push", testOCILayoutPush, spec.Report(report.Terminal{}))
}

func testOCILayoutPush(t *testing.T, when spec.G, it spec.S) {
	// writePerArchLayout writes a synthetic single-image OCI layout to its own
	// per-arch directory, mirroring a Phase 1 content store (one image per
	// platform, written to a distinct dir). Returns the dir and the image's
	// manifest digest.
	writePerArchLayout := func(t *testing.T, numLayers int64) (string, string) {
		t.Helper()
		dir := t.TempDir()
		img, err := random.Image(1024, numLayers)
		h.AssertNil(t, err)
		idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
		_, err = layout.Write(dir, idx)
		h.AssertNil(t, err)
		digest, err := img.Digest()
		h.AssertNil(t, err)
		return dir, digest.String()
	}

	// indexPlatforms returns the platform descriptors of every manifest entry in
	// an assembled image index, in index order.
	indexPlatforms := func(t *testing.T, idx v1.ImageIndex) []v1.Platform {
		t.Helper()
		manifest, err := idx.IndexManifest()
		h.AssertNil(t, err)
		platforms := make([]v1.Platform, 0, len(manifest.Manifests))
		for _, m := range manifest.Manifests {
			h.AssertNotNil(t, m.Platform)
			platforms = append(platforms, *m.Platform)
		}
		return platforms
	}

	when("#AssembleManifestList", func() {
		when("per-arch layouts from separate directories (the Phase 1 shape)", func() {
			it("produces one manifest entry per platform with correct os/arch/variant", func() {
				amdDir, amdDigest := writePerArchLayout(t, 2)
				armDir, armDigest := writePerArchLayout(t, 3)
				armV7Dir, armV7Digest := writePerArchLayout(t, 1)

				layouts := []PerArchLayout{
					{Platform: Platform{OS: "linux", Arch: "amd64"}, LayoutDir: amdDir, ManifestDigest: amdDigest},
					{Platform: Platform{OS: "linux", Arch: "arm64"}, LayoutDir: armDir, ManifestDigest: armDigest},
					{Platform: Platform{OS: "linux", Arch: "arm", Variant: "v7"}, LayoutDir: armV7Dir, ManifestDigest: armV7Digest},
				}

				idx, err := AssembleManifestList(layouts)
				h.AssertNil(t, err)

				platforms := indexPlatforms(t, idx)
				// Exactly one entry per platform.
				h.AssertEq(t, len(platforms), 3)

				// Each entry carries the correct platform descriptor.
				h.AssertEq(t, platforms[0].OS, "linux")
				h.AssertEq(t, platforms[0].Architecture, "amd64")
				h.AssertEq(t, platforms[0].Variant, "")

				h.AssertEq(t, platforms[1].OS, "linux")
				h.AssertEq(t, platforms[1].Architecture, "arm64")

				h.AssertEq(t, platforms[2].OS, "linux")
				h.AssertEq(t, platforms[2].Architecture, "arm")
				h.AssertEq(t, platforms[2].Variant, "v7")
			})

			it("references the exact per-arch image manifests by digest (content-addressed, no tags)", func() {
				amdDir, amdDigest := writePerArchLayout(t, 2)
				armDir, armDigest := writePerArchLayout(t, 3)

				layouts := []PerArchLayout{
					{Platform: Platform{OS: "linux", Arch: "amd64"}, LayoutDir: amdDir, ManifestDigest: amdDigest},
					{Platform: Platform{OS: "linux", Arch: "arm64"}, LayoutDir: armDir, ManifestDigest: armDigest},
				}

				idx, err := AssembleManifestList(layouts)
				h.AssertNil(t, err)

				manifest, err := idx.IndexManifest()
				h.AssertNil(t, err)
				h.AssertEq(t, len(manifest.Manifests), 2)

				// The assembled index points at the SAME manifest digests the
				// per-arch layouts hold — proof the assembly is by content, not by
				// any intermediate tag name.
				digests := map[string]bool{}
				for _, m := range manifest.Manifests {
					digests[m.Digest.String()] = true
				}
				h.AssertEq(t, digests[amdDigest], true)
				h.AssertEq(t, digests[armDigest], true)
			})

			it("resolves the single image manifest when no digest is recorded", func() {
				amdDir, amdDigest := writePerArchLayout(t, 2)

				layouts := []PerArchLayout{
					// ManifestDigest intentionally empty — assembler resolves the
					// sole image manifest from the layout index.
					{Platform: Platform{OS: "linux", Arch: "amd64"}, LayoutDir: amdDir},
				}

				idx, err := AssembleManifestList(layouts)
				h.AssertNil(t, err)

				manifest, err := idx.IndexManifest()
				h.AssertNil(t, err)
				h.AssertEq(t, len(manifest.Manifests), 1)
				h.AssertEq(t, manifest.Manifests[0].Digest.String(), amdDigest)
			})

			it("handles a two-platform manifest list (the common multi-arch case)", func() {
				amdDir, amdDigest := writePerArchLayout(t, 4)
				armDir, armDigest := writePerArchLayout(t, 4)

				idx, err := AssembleManifestList([]PerArchLayout{
					{Platform: Platform{OS: "linux", Arch: "amd64"}, LayoutDir: amdDir, ManifestDigest: amdDigest},
					{Platform: Platform{OS: "linux", Arch: "arm64"}, LayoutDir: armDir, ManifestDigest: armDigest},
				})
				h.AssertNil(t, err)

				platforms := indexPlatforms(t, idx)
				h.AssertEq(t, len(platforms), 2)
			})
		})

		when("inputs are invalid", func() {
			it("errors when no layouts are provided", func() {
				_, err := AssembleManifestList(nil)
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "no per-arch layouts")
			})

			it("errors when a layout directory is missing/empty", func() {
				_, err := AssembleManifestList([]PerArchLayout{
					{Platform: Platform{OS: "linux", Arch: "amd64"}, LayoutDir: ""},
				})
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "missing layout directory")
			})

			it("errors when the layout directory is not an OCI layout", func() {
				_, err := AssembleManifestList([]PerArchLayout{
					{Platform: Platform{OS: "linux", Arch: "amd64"}, LayoutDir: t.TempDir()},
				})
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "reading OCI layout")
			})

			it("errors when the recorded digest is not present in the layout", func() {
				amdDir, _ := writePerArchLayout(t, 1)
				_, err := AssembleManifestList([]PerArchLayout{
					{
						Platform:       Platform{OS: "linux", Arch: "amd64"},
						LayoutDir:      amdDir,
						ManifestDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
					},
				})
				h.AssertNotNil(t, err)
			})
		})
	})

	when("#perArchLayoutsFromResults", func() {
		it("maps OCIStoreDir + OCILayoutDigest + Platform from each result", func() {
			results := []PlatformBuildResult{
				{
					Platform:        Platform{OS: "linux", Arch: "amd64"},
					OCIStoreDir:     "/tmp/oci-store-linux-amd64",
					OCILayoutDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				},
				{
					Platform:        Platform{OS: "linux", Arch: "arm64", Variant: "v8"},
					OCIStoreDir:     "/tmp/oci-store-linux-arm64-v8",
					OCILayoutDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				},
			}

			layouts := perArchLayoutsFromResults(results)
			h.AssertEq(t, len(layouts), 2)

			h.AssertEq(t, layouts[0].Platform.Arch, "amd64")
			h.AssertEq(t, layouts[0].LayoutDir, "/tmp/oci-store-linux-amd64")
			h.AssertEq(t, layouts[0].ManifestDigest, "sha256:1111111111111111111111111111111111111111111111111111111111111111")

			h.AssertEq(t, layouts[1].Platform.Variant, "v8")
			h.AssertEq(t, layouts[1].LayoutDir, "/tmp/oci-store-linux-arm64-v8")
		})
	})

	// End-to-end (assembly-only) check that mirrors how BuildMultiPlatform feeds
	// per-arch results into the assembly: build per-arch layouts, wrap them in
	// PlatformBuildResult, map + assemble, and assert one entry per platform.
	// This exercises the full path from results → index without a network push.
	when("assembly from PlatformBuildResult (BuildMultiPlatform wiring)", func() {
		it("assembles an index with one entry per platform from per-arch results", func() {
			amdDir, amdDigest := writePerArchLayout(t, 2)
			armDir, armDigest := writePerArchLayout(t, 2)

			results := []PlatformBuildResult{
				{Platform: Platform{OS: "linux", Arch: "amd64"}, OCIStoreDir: amdDir, OCILayoutDigest: amdDigest},
				{Platform: Platform{OS: "linux", Arch: "arm64"}, OCIStoreDir: armDir, OCILayoutDigest: armDigest},
			}

			idx, err := AssembleManifestList(perArchLayoutsFromResults(results))
			h.AssertNil(t, err)

			platforms := indexPlatforms(t, idx)
			h.AssertEq(t, len(platforms), 2)
			h.AssertEq(t, platforms[0].Architecture, "amd64")
			h.AssertEq(t, platforms[1].Architecture, "arm64")
		})
	})

	when("#PushOCILayoutAsManifestList (legacy <outputDir>/<os>/<arch> layout)", func() {
		it("assembles from the os/arch subdirectory structure", func() {
			// The legacy path reads <outputDir>/<os>/<arch>. Recreate that shape
			// and assert assembly (no push) works via the shared assembler.
			base := t.TempDir()
			amdDir := filepath.Join(base, "linux", "amd64")
			armDir := filepath.Join(base, "linux", "arm64")

			amdImg, err := random.Image(1024, 1)
			h.AssertNil(t, err)
			armImg, err := random.Image(1024, 1)
			h.AssertNil(t, err)

			_, err = layout.Write(amdDir, mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: amdImg}))
			h.AssertNil(t, err)
			_, err = layout.Write(armDir, mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: armImg}))
			h.AssertNil(t, err)

			layouts := []PerArchLayout{
				{Platform: Platform{OS: "linux", Arch: "amd64"}, LayoutDir: amdDir},
				{Platform: Platform{OS: "linux", Arch: "arm64"}, LayoutDir: armDir},
			}
			idx, err := AssembleManifestList(layouts)
			h.AssertNil(t, err)

			platforms := indexPlatforms(t, idx)
			h.AssertEq(t, len(platforms), 2)
		})
	})
}
