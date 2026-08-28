package multiplatform

// Unit tests for the offline rebase-readiness precondition check (spec Task 9,
// Deliverable A, design "Rebase tests", FR-7). These are fast, daemon-free,
// deterministic tests: they build SYNTHETIC on-disk OCI layouts with
// go-containerregistry, control the recorded io.buildpacks.lifecycle.metadata
// (including the runImage.topLayer/reference boundary) and the config RootFS
// diff IDs explicitly, and assert CheckRebaseReadiness reports Ready / not-ready
// with the right reason.
//
// The KEY trick (mirrors oci_layout_parity_test.go): to make a recorded
// runImage.topLayer "coherent" (present among the image's config RootFS diff
// IDs) we read the synthetic image's ACTUAL config diff IDs and use one of them
// as the topLayer in the metadata. That reproduces, offline, the real invariant
// the rebaser relies on — the boundary diff ID exists in the image.

import (
	"encoding/json"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestOCILayoutRebase(t *testing.T) {
	spec.Run(t, "oci_layout_rebase", testOCILayoutRebase, spec.Report(report.Terminal{}))
}

// rebaseFixture describes the recorded rebase-relevant data to stamp onto a
// synthetic image's io.buildpacks.lifecycle.metadata label.
type rebaseFixture struct {
	// appSHA / launcherSHA are the non-run-image (preserved) layers the metadata
	// records above the boundary. At least one is needed for readiness.
	appSHA      string
	launcherSHA string
	// runImageTopLayer is written to runImage.topLayer. Tests set this to one of
	// the image's ACTUAL config diff IDs to make the boundary coherent, or to a
	// bogus / empty value to exercise the not-ready reasons.
	runImageTopLayer string
	// runImageReference is written to runImage.reference.
	runImageReference string
	// omitRunImage, when true, omits the runImage object entirely.
	omitRunImage bool
	// omitLifecycleLabel, when true, writes no lifecycle metadata label at all.
	omitLifecycleLabel bool
	// omitPreservedLayers, when true, records a runImage but no app/launcher
	// layers (nothing preserved above the boundary).
	omitPreservedLayers bool
}

func testOCILayoutRebase(t *testing.T, when spec.G, it spec.S) {
	// lifecycleMetadataJSON builds an io.buildpacks.lifecycle.metadata value from
	// a rebaseFixture, mirroring the lifecycle's label shape (app / launcher /
	// runImage{topLayer,reference}).
	lifecycleMetadataJSON := func(t *testing.T, fx rebaseFixture) string {
		t.Helper()
		md := map[string]interface{}{}
		if !fx.omitPreservedLayers {
			if fx.appSHA != "" {
				md["app"] = []map[string]string{{"sha": fx.appSHA}}
			}
			if fx.launcherSHA != "" {
				md["launcher"] = map[string]string{"sha": fx.launcherSHA}
			}
		}
		if !fx.omitRunImage {
			md["runImage"] = map[string]string{
				"topLayer":  fx.runImageTopLayer,
				"reference": fx.runImageReference,
			}
		}
		b, err := json.Marshal(md)
		h.AssertNil(t, err)
		return string(b)
	}

	// writeLayout writes a synthetic on-disk OCI layout (2 random layers) whose
	// lifecycle metadata is described by fx. It returns the layout dir and the
	// image's actual config diff IDs so a test can align runImage.topLayer with a
	// real layer when it wants a coherent boundary.
	writeLayout := func(t *testing.T, fx rebaseFixture, mutateLabel func(fx rebaseFixture, diffIDs []string) rebaseFixture) (string, []string) {
		t.Helper()
		dir := t.TempDir()
		img, err := random.Image(1024, 2)
		h.AssertNil(t, err)

		cfg, err := img.ConfigFile()
		h.AssertNil(t, err)
		diffIDs := make([]string, 0, len(cfg.RootFS.DiffIDs))
		for _, d := range cfg.RootFS.DiffIDs {
			diffIDs = append(diffIDs, d.String())
		}

		// Let the test adjust the fixture using the real diff IDs (e.g. set a
		// coherent topLayer) before we render the label.
		if mutateLabel != nil {
			fx = mutateLabel(fx, diffIDs)
		}

		cfg = cfg.DeepCopy()
		labels := map[string]string{
			"io.buildpacks.stack.id": "io.buildpacks.stacks.jammy",
		}
		if !fx.omitLifecycleLabel {
			labels[lifecycleMetadataLabel] = lifecycleMetadataJSON(t, fx)
		}
		cfg.Config.Labels = labels
		img, err = mutate.ConfigFile(img, cfg)
		h.AssertNil(t, err)

		idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
		_, err = layout.Write(dir, idx)
		h.AssertNil(t, err)
		return dir, diffIDs
	}

	// coherentTopLayer sets runImage.topLayer to the image's FIRST actual config
	// diff ID, making the recorded boundary exist among the image layers.
	coherentTopLayer := func(fx rebaseFixture, diffIDs []string) rebaseFixture {
		if len(diffIDs) > 0 {
			fx.runImageTopLayer = diffIDs[0]
		}
		return fx
	}

	defaultReady := func() rebaseFixture {
		return rebaseFixture{
			appSHA:            "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			launcherSHA:       "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			runImageReference: "index.docker.io/library/run@sha256:aaaa",
			// runImageTopLayer set to a real diff ID by coherentTopLayer.
		}
	}

	when("#CheckRebaseReadiness", func() {
		when("the image records a coherent run-image boundary and preserved layers", func() {
			it("reports Ready with no reasons", func() {
				dir, _ := writeLayout(t, defaultReady(), coherentTopLayer)

				readiness, err := CheckRebaseReadinessLayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, readiness.Ready, true)
				h.AssertEq(t, len(readiness.Reasons), 0)
				h.AssertEq(t, readiness.Error(), "")
			})
		})

		when("runImage.topLayer is missing", func() {
			it("reports not-ready citing the missing boundary", func() {
				fx := defaultReady()
				fx.omitRunImage = true // no runImage object at all
				dir, _ := writeLayout(t, fx, nil)

				readiness, err := CheckRebaseReadinessLayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, readiness.Ready, false)
				h.AssertSliceContainsMatch(t, readiness.Reasons, "runImage.topLayer is empty")
				// reference also comes from the runImage object, so it is flagged too.
				h.AssertSliceContainsMatch(t, readiness.Reasons, "runImage.reference is empty")
			})
		})

		when("runImage.reference is empty", func() {
			it("reports not-ready citing the missing reference", func() {
				fx := defaultReady()
				fx.runImageReference = "" // empty reference, coherent topLayer
				dir, _ := writeLayout(t, fx, coherentTopLayer)

				readiness, err := CheckRebaseReadinessLayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, readiness.Ready, false)
				h.AssertSliceContainsMatch(t, readiness.Reasons, "runImage.reference is empty")
			})
		})

		when("the recorded topLayer is not among the image config diff IDs", func() {
			it("reports not-ready citing the incoherent boundary", func() {
				fx := defaultReady()
				// A topLayer diff ID that does NOT exist in the image.
				fx.runImageTopLayer = "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
				dir, _ := writeLayout(t, fx, nil)

				readiness, err := CheckRebaseReadinessLayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, readiness.Ready, false)
				h.AssertSliceContainsMatch(t, readiness.Reasons, "is not among the image config RootFS diff IDs")
			})
		})

		when("the lifecycle metadata label is absent", func() {
			it("reports not-ready citing the missing label", func() {
				fx := defaultReady()
				fx.omitLifecycleLabel = true
				dir, _ := writeLayout(t, fx, coherentTopLayer)

				readiness, err := CheckRebaseReadinessLayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, readiness.Ready, false)
				h.AssertEq(t, len(readiness.Reasons), 1)
				h.AssertSliceContainsMatch(t, readiness.Reasons, "io.buildpacks.lifecycle.metadata label is absent")
			})
		})

		when("no app/launcher/buildpack layers are recorded above the boundary", func() {
			it("reports not-ready citing nothing to preserve", func() {
				fx := defaultReady()
				fx.omitPreservedLayers = true // runImage present, but no preserved layers
				dir, _ := writeLayout(t, fx, coherentTopLayer)

				readiness, err := CheckRebaseReadinessLayout(dir)
				h.AssertNil(t, err)
				h.AssertEq(t, readiness.Ready, false)
				h.AssertSliceContainsMatch(t, readiness.Reasons, "nothing to preserve on rebase")
			})
		})

		when("the inspection is not a complete image", func() {
			it("reports not-ready without evaluating boundary fields", func() {
				readiness := CheckRebaseReadiness(OCILayoutInspection{LayoutDir: "/x", Complete: false})
				h.AssertEq(t, readiness.Ready, false)
				h.AssertEq(t, len(readiness.Reasons), 1)
				h.AssertSliceContainsMatch(t, readiness.Reasons, "not a complete image")
			})
		})
	})

	when("#CheckRebaseReadinessLayout", func() {
		it("returns an error and a not-ready result when the dir is not a layout", func() {
			dir := t.TempDir() // empty, not an OCI layout

			readiness, err := CheckRebaseReadinessLayout(dir)
			h.AssertNotNil(t, err)
			h.AssertEq(t, readiness.Ready, false)
			h.AssertSliceContainsMatch(t, readiness.Reasons, "inspecting layout")
		})
	})

	when("#CheckMultiArchRebaseReadiness", func() {
		it("reports Ready when every per-arch layout is rebase-ready", func() {
			amdDir, _ := writeLayout(t, defaultReady(), coherentTopLayer)
			armDir, _ := writeLayout(t, defaultReady(), coherentTopLayer)

			results := []PlatformBuildResult{
				{Platform: Platform{OS: "linux", Arch: "amd64"}, OCIStoreDir: amdDir},
				{Platform: Platform{OS: "linux", Arch: "arm64"}, OCIStoreDir: armDir},
			}

			multi := CheckMultiArchRebaseReadiness(results)
			h.AssertEq(t, multi.Ready, true)
			h.AssertEq(t, len(multi.PerPlatform), 2)
			h.AssertEq(t, multi.PerPlatform["linux/amd64"].Ready, true)
			h.AssertEq(t, multi.PerPlatform["linux/arm64"].Ready, true)
			h.AssertEq(t, multi.Error(), "")
		})

		it("reports not-ready when any platform is not rebase-ready", func() {
			readyDir, _ := writeLayout(t, defaultReady(), coherentTopLayer)
			// The arm64 layout omits the runImage → not rebase-ready.
			badFx := defaultReady()
			badFx.omitRunImage = true
			badDir, _ := writeLayout(t, badFx, nil)

			results := []PlatformBuildResult{
				{Platform: Platform{OS: "linux", Arch: "amd64"}, OCIStoreDir: readyDir},
				{Platform: Platform{OS: "linux", Arch: "arm64"}, OCIStoreDir: badDir},
			}

			multi := CheckMultiArchRebaseReadiness(results)
			h.AssertEq(t, multi.Ready, false)
			h.AssertEq(t, multi.PerPlatform["linux/amd64"].Ready, true)
			h.AssertEq(t, multi.PerPlatform["linux/arm64"].Ready, false)
			h.AssertContains(t, multi.Error(), "linux/arm64")
		})

		it("reports not-ready when a platform has no on-disk layout", func() {
			readyDir, _ := writeLayout(t, defaultReady(), coherentTopLayer)
			results := []PlatformBuildResult{
				{Platform: Platform{OS: "linux", Arch: "amd64"}, OCIStoreDir: readyDir},
				{Platform: Platform{OS: "linux", Arch: "arm64"}, OCIStoreDir: ""},
			}

			multi := CheckMultiArchRebaseReadiness(results)
			h.AssertEq(t, multi.Ready, false)
			h.AssertSliceContainsMatch(t, multi.PerPlatform["linux/arm64"].Reasons, "no on-disk OCI layout")
		})

		it("reports not-ready for an empty result set", func() {
			multi := CheckMultiArchRebaseReadiness(nil)
			h.AssertEq(t, multi.Ready, false)
		})
	})
}
