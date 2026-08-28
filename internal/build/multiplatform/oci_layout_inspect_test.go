package multiplatform

import (
	"encoding/json"
	"os"
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

func TestOCILayoutInspect(t *testing.T) {
	spec.Run(t, "oci_layout_inspect", testOCILayoutInspect, spec.Report(report.Terminal{}))
}

func testOCILayoutInspect(t *testing.T, when spec.G, it spec.S) {
	// writeSyntheticLayout creates an on-disk OCI layout containing a single
	// image with the given number of layers, mirroring the format BuildKit's
	// ExporterOCI writes to a contentlocal store (oci-layout marker + index.json
	// + blobs/sha256/...). Returns the layout dir and the image.
	writeSyntheticLayout := func(t *testing.T, numLayers int64) (string, v1.Image) {
		t.Helper()
		dir := t.TempDir()
		img, err := random.Image(1024, numLayers)
		h.AssertNil(t, err)

		idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
		_, err = layout.Write(dir, idx)
		h.AssertNil(t, err)
		return dir, img
	}

	when("#InspectOCILayout", func() {
		when("the layout is a complete single image", func() {
			it("reports Complete and resolves manifest, config, and all layers", func() {
				dir, img := writeSyntheticLayout(t, 3)

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, result.Complete, true)
				h.AssertEq(t, result.LayoutDir, dir)

				// Manifest digest matches the image.
				wantDigest, err := img.Digest()
				h.AssertNil(t, err)
				h.AssertEq(t, result.ManifestDigest, wantDigest.String())

				// Config digest matches.
				manifest, err := img.Manifest()
				h.AssertNil(t, err)
				h.AssertEq(t, result.ConfigDigest, manifest.Config.Digest.String())

				// All three layers were resolved, in order.
				layers, err := img.Layers()
				h.AssertNil(t, err)
				h.AssertEq(t, len(result.LayerDigests), 3)
				for i, l := range layers {
					d, err := l.Digest()
					h.AssertNil(t, err)
					h.AssertEq(t, result.LayerDigests[i], d.String())
				}

				// Diff IDs come from the config and match the image config.
				cfg, err := img.ConfigFile()
				h.AssertNil(t, err)
				h.AssertEq(t, len(result.DiffIDs), len(cfg.RootFS.DiffIDs))
				for i, d := range cfg.RootFS.DiffIDs {
					h.AssertEq(t, result.DiffIDs[i], d.String())
				}
			})

			it("handles an image with zero layers (config only)", func() {
				dir, _ := writeSyntheticLayout(t, 0)

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, result.Complete, true)
				h.AssertEq(t, len(result.LayerDigests), 0)
				h.AssertNotEq(t, result.ConfigDigest, "")
			})

			it("records a non-empty media type", func() {
				dir, _ := writeSyntheticLayout(t, 1)
				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertNotEq(t, result.MediaType, "")
			})
		})

		when("the directory is not an OCI layout", func() {
			it("returns an error and Complete=false", func() {
				dir := t.TempDir() // empty dir, no index.json / oci-layout marker

				result, err := InspectOCILayout(dir)
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "opening OCI layout")
				h.AssertEq(t, result.Complete, false)
			})
		})

		when("a referenced layer blob is missing", func() {
			it("returns an error indicating the incomplete layout", func() {
				dir, img := writeSyntheticLayout(t, 2)

				// Delete one layer blob to simulate an incomplete store.
				layers, err := img.Layers()
				h.AssertNil(t, err)
				missing, err := layers[0].Digest()
				h.AssertNil(t, err)

				lp, err := layout.FromPath(dir)
				h.AssertNil(t, err)
				h.AssertNil(t, lp.RemoveBlob(missing))

				result, err := InspectOCILayout(dir)
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "missing or unreadable")
				h.AssertEq(t, result.Complete, false)
			})
		})

		when("the config blob is missing", func() {
			it("returns an error and Complete=false", func() {
				dir, img := writeSyntheticLayout(t, 1)

				manifest, err := img.Manifest()
				h.AssertNil(t, err)

				lp, err := layout.FromPath(dir)
				h.AssertNil(t, err)
				h.AssertNil(t, lp.RemoveBlob(manifest.Config.Digest))

				result, err := InspectOCILayout(dir)
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "config blob")
				h.AssertEq(t, result.Complete, false)
			})
		})

		when("the index contains no manifests", func() {
			it("returns an error", func() {
				dir := t.TempDir()
				_, err := layout.Write(dir, empty.Index)
				h.AssertNil(t, err)

				result, err := InspectOCILayout(dir)
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "no manifests")
				h.AssertEq(t, result.Complete, false)
			})
		})

		when("the index contains more than one image manifest", func() {
			it("returns an ambiguity error (a per-arch layout must hold exactly one image)", func() {
				dir := t.TempDir()
				img1, err := random.Image(1024, 1)
				h.AssertNil(t, err)
				img2, err := random.Image(1024, 1)
				h.AssertNil(t, err)

				idx := mutate.AppendManifests(empty.Index,
					mutate.IndexAddendum{Add: img1},
					mutate.IndexAddendum{Add: img2},
				)
				_, err = layout.Write(dir, idx)
				h.AssertNil(t, err)

				result, err := InspectOCILayout(dir)
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "image manifests")
				h.AssertEq(t, result.Complete, false)
			})
		})

		when("the image config sets runtime fields", func() {
			it("surfaces entrypoint, env, user, workdir, and exposed ports", func() {
				dir := t.TempDir()
				img, err := random.Image(1024, 2)
				h.AssertNil(t, err)

				// Set a realistic runtime config via mutate.Config, mirroring what a
				// lifecycle-exported image config carries.
				cfg, err := img.ConfigFile()
				h.AssertNil(t, err)
				cfg = cfg.DeepCopy()
				cfg.Config.Entrypoint = []string{"/cnb/lifecycle/launcher"}
				cfg.Config.Cmd = []string{"web"}
				cfg.Config.Env = []string{"PATH=/usr/bin", "CNB_LAYERS_DIR=/layers"}
				cfg.Config.User = "1000:1000"
				cfg.Config.WorkingDir = "/workspace"
				cfg.Config.ExposedPorts = map[string]struct{}{"8080/tcp": {}, "443/tcp": {}}
				img, err = mutate.ConfigFile(img, cfg)
				h.AssertNil(t, err)

				idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
				_, err = layout.Write(dir, idx)
				h.AssertNil(t, err)

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, result.Complete, true)

				h.AssertEq(t, result.Config.Entrypoint, []string{"/cnb/lifecycle/launcher"})
				h.AssertEq(t, result.Config.Env, []string{"PATH=/usr/bin", "CNB_LAYERS_DIR=/layers"})
				h.AssertEq(t, result.Config.User, "1000:1000")
				h.AssertEq(t, result.Config.WorkingDir, "/workspace")
				// ExposedPorts are sorted for deterministic comparison.
				h.AssertEq(t, result.Config.ExposedPorts, []string{"443/tcp", "8080/tcp"})
			})

			it("reports layer order lines up with diff-ID order", func() {
				dir, img := writeSyntheticLayout(t, 4)

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)

				// Count/order: manifest-ordered layer digests and config-ordered diff
				// IDs are parallel (one diff ID per layer).
				h.AssertEq(t, result.LayersMatchDiffIDs(), true)
				h.AssertEq(t, len(result.LayerDigests), 4)
				h.AssertEq(t, len(result.DiffIDs), 4)

				// The i-th layer's diff ID equals the config's i-th diff ID (order).
				layers, err := img.Layers()
				h.AssertNil(t, err)
				for i, l := range layers {
					diffID, err := l.DiffID()
					h.AssertNil(t, err)
					h.AssertEq(t, result.DiffIDs[i], diffID.String())
				}
			})
		})

		when("the image has a lifecycle metadata label", func() {
			// buildLifecycleMetadataJSON builds a small hand-crafted
			// io.buildpacks.lifecycle.metadata value with app/config/launcher and
			// (optionally) an sbom layer, mirroring the lifecycle's label shape.
			buildLifecycleMetadataJSON := func(t *testing.T, withSBOM bool) (string, []string, string) {
				t.Helper()
				appSHA := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
				configSHA := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
				launcherSHA := "sha256:3333333333333333333333333333333333333333333333333333333333333333"
				bpSHA := "sha256:4444444444444444444444444444444444444444444444444444444444444444"
				sbomSHA := "sha256:5555555555555555555555555555555555555555555555555555555555555555"

				md := map[string]interface{}{
					"app":      []map[string]string{{"sha": appSHA}},
					"config":   map[string]string{"sha": configSHA},
					"launcher": map[string]string{"sha": launcherSHA},
					"buildpacks": []map[string]interface{}{
						{"key": "example/bp", "layers": map[string]interface{}{
							"some-layer": map[string]string{"sha": bpSHA},
						}},
					},
					"runImage": map[string]string{"reference": "run@sha256:abc", "topLayer": "sha256:6666666666666666666666666666666666666666666666666666666666666666"},
				}
				want := []string{appSHA, configSHA, launcherSHA, bpSHA}
				wantSBOM := ""
				if withSBOM {
					md["sbom"] = map[string]string{"sha": sbomSHA}
					wantSBOM = sbomSHA
				}
				b, err := json.Marshal(md)
				h.AssertNil(t, err)
				return string(b), want, wantSBOM
			}

			writeLayoutWithLabel := func(t *testing.T, label string) string {
				t.Helper()
				dir := t.TempDir()
				img, err := random.Image(1024, 1)
				h.AssertNil(t, err)
				cfg, err := img.ConfigFile()
				h.AssertNil(t, err)
				cfg = cfg.DeepCopy()
				cfg.Config.Labels = map[string]string{
					"io.buildpacks.lifecycle.metadata": label,
					"io.buildpacks.stack.id":           "io.buildpacks.stacks.jammy",
				}
				img, err = mutate.ConfigFile(img, cfg)
				h.AssertNil(t, err)
				idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
				_, err = layout.Write(dir, idx)
				h.AssertNil(t, err)
				return dir
			}

			it("exposes the label value and parses recorded diff IDs and SBOM presence", func() {
				label, wantDiffIDs, wantSBOM := buildLifecycleMetadataJSON(t, true)
				dir := writeLayoutWithLabel(t, label)

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, result.Complete, true)

				// The label is exposed on the Labels map.
				h.AssertEq(t, result.Labels["io.buildpacks.lifecycle.metadata"], label)

				// Parsed metadata is present with the recorded diff IDs in order.
				h.AssertEq(t, result.LifecycleMetadata.Present, true)
				h.AssertEq(t, result.LifecycleMetadata.Raw, label)
				h.AssertEq(t, result.LifecycleMetadata.DiffIDs, wantDiffIDs)

				// SBOM presence is derived from the metadata's sbom key.
				h.AssertEq(t, result.LifecycleMetadata.HasSBOM, true)
				h.AssertEq(t, result.LifecycleMetadata.SBOMDiffID, wantSBOM)

				// The run-image rebase boundary (runImage.topLayer + reference) is
				// surfaced for the offline rebase-readiness check (Task 9, FR-7).
				h.AssertEq(t, result.LifecycleMetadata.RunImageReference, "run@sha256:abc")
				h.AssertEq(t, result.LifecycleMetadata.RunImageTopLayer, "sha256:6666666666666666666666666666666666666666666666666666666666666666")
			})

			it("reports HasSBOM=false when the metadata has no sbom layer", func() {
				label, _, _ := buildLifecycleMetadataJSON(t, false)
				dir := writeLayoutWithLabel(t, label)

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, result.LifecycleMetadata.Present, true)
				h.AssertEq(t, result.LifecycleMetadata.HasSBOM, false)
				h.AssertEq(t, result.LifecycleMetadata.SBOMDiffID, "")
			})

			it("does not fail and reports the label absent when there is no lifecycle label", func() {
				// Synthetic fixtures have no lifecycle label; InspectOCILayout must
				// still succeed and report the metadata as not present.
				dir, _ := writeSyntheticLayout(t, 2)

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, result.Complete, true)
				h.AssertEq(t, result.LifecycleMetadata.Present, false)
				h.AssertEq(t, result.LifecycleMetadata.Raw, "")
				h.AssertEq(t, len(result.LifecycleMetadata.DiffIDs), 0)
				h.AssertEq(t, result.LifecycleMetadata.HasSBOM, false)
				// The lifecycle label specifically must be absent from the labels
				// map (whether the map is nil or empty is an implementation detail
				// of the fixture builder).
				_, hasLabel := result.Labels["io.buildpacks.lifecycle.metadata"]
				h.AssertEq(t, hasLabel, false)
			})

			it("does not fail when the lifecycle label is present but not valid JSON", func() {
				dir := writeLayoutWithLabel(t, "not-json{{")

				result, err := InspectOCILayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, result.Complete, true)
				// The raw value is still surfaced even though structured parsing failed.
				h.AssertEq(t, result.Labels["io.buildpacks.lifecycle.metadata"], "not-json{{")
				h.AssertEq(t, result.LifecycleMetadata.Present, false)
				h.AssertEq(t, result.LifecycleMetadata.Raw, "not-json{{")
			})
		})

		when("the on-disk layout has the expected marker files", func() {
			it("has an oci-layout marker, index.json, and a blobs dir", func() {
				dir, _ := writeSyntheticLayout(t, 1)

				// Sanity-check the fixture matches the real ExporterOCI on-disk shape,
				// so the inspector is validated against a realistic layout.
				h.AssertPathExists(t, filepath.Join(dir, "oci-layout"))
				h.AssertPathExists(t, filepath.Join(dir, "index.json"))
				info, err := os.Stat(filepath.Join(dir, "blobs"))
				h.AssertNil(t, err)
				h.AssertEq(t, info.IsDir(), true)
			})
		})
	})

	when("#resolveImageDescriptor", func() {
		it("treats an empty media type descriptor as an image manifest", func() {
			idxManifest := &v1.IndexManifest{
				Manifests: []v1.Descriptor{{MediaType: ""}},
			}
			desc, err := resolveImageDescriptor("/some/dir", idxManifest)
			h.AssertNil(t, err)
			h.AssertEq(t, string(desc.MediaType), "")
		})
	})
}
