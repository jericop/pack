package multiplatform

// Unit tests for the on-disk parity comparison (spec Task 8, Deliverable A,
// design "Tier 2 parity check"). These are the fast, daemon-free, deterministic
// tests the design leads with: they build two SYNTHETIC on-disk OCI layouts with
// go-containerregistry, control the parity-relevant recorded data (lifecycle
// metadata diff IDs, config runtime fields, labels) explicitly, and assert
// CompareParity flags exactly the perturbed field.
//
// Why synthetic fixtures with controlled metadata (not real builds): random.Image
// produces different random layer blobs, so blob DIGESTS never match across two
// images. But parity per FR-7/FR-8 is about the RECORDED metadata/config/labels
// (the lifecycle-metadata diff IDs, config fields, non-lifecycle labels), NOT the
// random blob digests. We therefore SET those recorded values explicitly on both
// synthetic images (identical → Match; one field perturbed → exactly that diff),
// which is precisely what the parity check compares.

import (
	"encoding/json"
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

func TestOCILayoutParity(t *testing.T) {
	spec.Run(t, "oci_layout_parity", testOCILayoutParity, spec.Report(report.Terminal{}))
}

// fixtureSpec describes the parity-relevant recorded data to stamp onto a
// synthetic image. Everything the parity check compares is set here explicitly
// so tests can control it independently of the random layer blobs.
type fixtureSpec struct {
	entrypoint []string
	env        []string
	user       string
	workingDir string
	ports      map[string]struct{}
	labels     map[string]string // non-lifecycle labels
	// lifecycle metadata contents:
	appSHA      string
	configSHA   string
	launcherSHA string
	bpSHA       string
	sbomSHA     string // when non-empty, an sbom layer is recorded
	// runImageRef is placed in the lifecycle metadata's runImage.reference — a
	// field that legitimately differs across modes and MUST NOT cause a mismatch.
	runImageRef string
	// omitLifecycleLabel, when true, does not write the lifecycle metadata label.
	omitLifecycleLabel bool
	// lifecycleLabelOverride, when non-empty, is written verbatim as the
	// lifecycle metadata label (used to test raw-JSON-differs-but-diff-IDs-match).
	lifecycleLabelOverride string
}

func testOCILayoutParity(t *testing.T, when spec.G, it spec.S) {
	// defaultFixture returns a spec with realistic, matching parity data. Tests
	// clone it and perturb one field to assert the parity check flags exactly
	// that field.
	defaultFixture := func() fixtureSpec {
		return fixtureSpec{
			entrypoint: []string{"/cnb/lifecycle/launcher"},
			env:        []string{"PATH=/usr/bin", "CNB_LAYERS_DIR=/layers"},
			user:       "1000:1000",
			workingDir: "/workspace",
			ports:      map[string]struct{}{"8080/tcp": {}},
			labels: map[string]string{
				"io.buildpacks.stack.id":           "io.buildpacks.stacks.jammy",
				"io.buildpacks.project.metadata":   `{"source":{"type":"git"}}`,
				"io.buildpacks.builder.metadata":   `{"buildpacks":[]}`,
			},
			appSHA:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			configSHA:   "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			launcherSHA: "sha256:3333333333333333333333333333333333333333333333333333333333333333",
			bpSHA:       "sha256:4444444444444444444444444444444444444444444444444444444444444444",
			sbomSHA:     "sha256:5555555555555555555555555555555555555555555555555555555555555555",
			runImageRef: "index.docker.io/library/run@sha256:aaaa",
		}
	}

	// lifecycleMetadataJSON builds the io.buildpacks.lifecycle.metadata label
	// value from a fixtureSpec, mirroring the lifecycle's label shape (app /
	// config / launcher / buildpacks / sbom / runImage).
	lifecycleMetadataJSON := func(t *testing.T, fs fixtureSpec) string {
		t.Helper()
		md := map[string]interface{}{
			"app":      []map[string]string{{"sha": fs.appSHA}},
			"config":   map[string]string{"sha": fs.configSHA},
			"launcher": map[string]string{"sha": fs.launcherSHA},
			"buildpacks": []map[string]interface{}{
				{"key": "example/bp", "layers": map[string]interface{}{
					"some-layer": map[string]string{"sha": fs.bpSHA},
				}},
			},
			"runImage": map[string]string{"reference": fs.runImageRef},
		}
		if fs.sbomSHA != "" {
			md["sbom"] = map[string]string{"sha": fs.sbomSHA}
		}
		b, err := json.Marshal(md)
		h.AssertNil(t, err)
		return string(b)
	}

	// baseImage builds ONE random image reused as the starting point for both the
	// reference and candidate fixtures in a given test. Sharing a base is what
	// makes the two fixtures' config RootFS diff IDs (and layer blobs) IDENTICAL —
	// independently generated random.Image values would have different random
	// blobs and therefore different config diff IDs, which is NOT what parity is
	// about. Parity here is over the RECORDED metadata/config/labels we stamp on
	// top, so both fixtures must share the same underlying layers.
	baseImage := func(t *testing.T) v1.Image {
		t.Helper()
		img, err := random.Image(1024, 2)
		h.AssertNil(t, err)
		return img
	}

	// writeFixtureLayout writes a synthetic on-disk OCI layout, starting from the
	// shared base image, whose recorded config + labels + lifecycle metadata are
	// exactly what fs describes. Passing the SAME base for the reference and
	// candidate guarantees identical config RootFS diff IDs so the comparison is
	// exercised purely over the fields fs controls.
	writeFixtureLayout := func(t *testing.T, base v1.Image, fs fixtureSpec) string {
		t.Helper()
		dir := t.TempDir()
		img := base

		cfg, err := img.ConfigFile()
		h.AssertNil(t, err)
		cfg = cfg.DeepCopy()
		cfg.Config.Entrypoint = fs.entrypoint
		cfg.Config.Env = fs.env
		cfg.Config.User = fs.user
		cfg.Config.WorkingDir = fs.workingDir
		cfg.Config.ExposedPorts = fs.ports

		labels := map[string]string{}
		for k, v := range fs.labels {
			labels[k] = v
		}
		if !fs.omitLifecycleLabel {
			if fs.lifecycleLabelOverride != "" {
				labels[lifecycleMetadataLabel] = fs.lifecycleLabelOverride
			} else {
				labels[lifecycleMetadataLabel] = lifecycleMetadataJSON(t, fs)
			}
		}
		cfg.Config.Labels = labels

		img, err = mutate.ConfigFile(img, cfg)
		h.AssertNil(t, err)

		idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
		_, err = layout.Write(dir, idx)
		h.AssertNil(t, err)
		return dir
	}

	when("#CompareParity (via on-disk fixtures)", func() {
		when("the two layouts have identical recorded config, labels, and lifecycle metadata", func() {
			it("reports Match with no differences", func() {
				base := baseImage(t)
				fs := defaultFixture()
				refDir := writeFixtureLayout(t, base, fs)
				candDir := writeFixtureLayout(t, base, fs)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), true)
				h.AssertEq(t, len(report.Differences), 0)
				h.AssertEq(t, report.Error(), "")
			})

			it("matches even when runImage.reference (a non-layer field) differs in the raw label", func() {
				// This is the crux of "compare extracted diff IDs, not raw JSON":
				// the two images record DIFFERENT runImage.reference values (as
				// registry mode vs oci-layout legitimately would) but IDENTICAL
				// layer diff IDs. Parity MUST hold.
				base := baseImage(t)
				ref := defaultFixture()
				ref.runImageRef = "index.docker.io/library/run@sha256:aaaa"
				cand := defaultFixture()
				cand.runImageRef = "gcr.io/other/run@sha256:bbbb"

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				// Sanity: the raw lifecycle-metadata labels really do differ.
				refInspect, err := InspectOCILayout(refDir)
				h.AssertNil(t, err)
				candInspect, err := InspectOCILayout(candDir)
				h.AssertNil(t, err)
				h.AssertNotEq(t, refInspect.LifecycleMetadata.Raw, candInspect.LifecycleMetadata.Raw)

				report := CompareParity(refInspect, candInspect)
				h.AssertEq(t, report.Match(), true)
				h.AssertEq(t, len(report.Differences), 0)
			})
		})

		when("a layer diff ID in the lifecycle metadata differs", func() {
			it("flags exactly the lifecycle-metadata diff ID mismatch", func() {
				base := baseImage(t)
				ref := defaultFixture()
				cand := defaultFixture()
				cand.bpSHA = "sha256:9999999999999999999999999999999999999999999999999999999999999999"

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), false)
				h.AssertEq(t, len(report.Differences), 1)
				h.AssertContains(t, report.Differences[0], "lifecycle-metadata layer diff ID")
				h.AssertContains(t, report.Differences[0], cand.bpSHA)
			})
		})

		when("an Env entry differs", func() {
			it("flags exactly the config Env mismatch", func() {
				base := baseImage(t)
				ref := defaultFixture()
				cand := defaultFixture()
				cand.env = []string{"PATH=/usr/bin", "CNB_LAYERS_DIR=/different"}

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), false)
				h.AssertEq(t, len(report.Differences), 1)
				h.AssertContains(t, report.Differences[0], "config Env element 1 differs")
			})
		})

		when("the User differs", func() {
			it("flags exactly the config User mismatch", func() {
				base := baseImage(t)
				ref := defaultFixture()
				cand := defaultFixture()
				cand.user = "0:0"

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), false)
				h.AssertEq(t, len(report.Differences), 1)
				h.AssertContains(t, report.Differences[0], "config User differs")
				h.AssertContains(t, report.Differences[0], `"0:0"`)
			})
		})

		when("a non-lifecycle label differs", func() {
			it("flags exactly the label mismatch", func() {
				base := baseImage(t)
				ref := defaultFixture()
				cand := defaultFixture()
				cand.labels = map[string]string{
					"io.buildpacks.stack.id":         "io.buildpacks.stacks.bionic", // changed
					"io.buildpacks.project.metadata": `{"source":{"type":"git"}}`,
					"io.buildpacks.builder.metadata": `{"buildpacks":[]}`,
				}

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), false)
				h.AssertEq(t, len(report.Differences), 1)
				h.AssertContains(t, report.Differences[0], `label "io.buildpacks.stack.id" differs`)
				h.AssertContains(t, report.Differences[0], "io.buildpacks.stacks.bionic")
			})
		})

		when("the SBOM presence differs", func() {
			it("flags the lifecycle-metadata SBOM mismatch", func() {
				base := baseImage(t)
				ref := defaultFixture()
				cand := defaultFixture()
				cand.sbomSHA = "" // no sbom layer recorded on the candidate

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), false)
				h.AssertEq(t, len(report.Differences), 1)
				h.AssertContains(t, report.Differences[0], "SBOM presence differs")
			})
		})

		when("the lifecycle metadata label is absent on the candidate", func() {
			it("flags the missing lifecycle metadata (cannot verify rebase parity)", func() {
				base := baseImage(t)
				ref := defaultFixture()
				cand := defaultFixture()
				cand.omitLifecycleLabel = true

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), false)
				h.AssertContains(t, report.Differences[0], "present on reference but absent/invalid on candidate")
			})
		})

		when("multiple fields differ at once", func() {
			it("reports each difference so the caller gets actionable output", func() {
				base := baseImage(t)
				ref := defaultFixture()
				cand := defaultFixture()
				cand.user = "0:0"
				cand.appSHA = "sha256:8888888888888888888888888888888888888888888888888888888888888888"

				refDir := writeFixtureLayout(t, base, ref)
				candDir := writeFixtureLayout(t, base, cand)

				report, err := CompareParityLayouts(refDir, candDir)
				h.AssertNil(t, err)
				h.AssertEq(t, report.Match(), false)
				// One lifecycle-metadata diff-ID mismatch + one User mismatch.
				h.AssertEq(t, len(report.Differences), 2)
				// Error() aggregates them into one actionable string.
				h.AssertContains(t, report.Error(), "2 difference(s)")
				h.AssertContains(t, report.Error(), "config User differs")
				h.AssertContains(t, report.Error(), "lifecycle-metadata layer diff ID")
			})
		})
	})

	when("#CompareParity (direct, unit-level)", func() {
		it("reports incomplete inspections as a non-match", func() {
			ref := OCILayoutInspection{LayoutDir: "/ref", Complete: true}
			cand := OCILayoutInspection{LayoutDir: "/cand", Complete: false}

			report := CompareParity(ref, cand)
			h.AssertEq(t, report.Match(), false)
			h.AssertContains(t, report.Differences[0], "candidate layout is not a complete image")
		})

		it("matches two minimal identical complete inspections", func() {
			mk := func(dir string) OCILayoutInspection {
				return OCILayoutInspection{
					LayoutDir: dir,
					Complete:  true,
					DiffIDs:   []string{"sha256:aaaa", "sha256:bbbb"},
					Config: OCIImageConfig{
						Entrypoint: []string{"/cnb/lifecycle/launcher"},
						User:       "1000:1000",
					},
					Labels: map[string]string{"io.buildpacks.stack.id": "jammy"},
					LifecycleMetadata: LifecycleMetadata{
						Present: true,
						DiffIDs: []string{"sha256:aaaa", "sha256:bbbb"},
					},
				}
			}
			report := CompareParity(mk("/ref"), mk("/cand"))
			h.AssertEq(t, report.Match(), true)
		})

		it("flags a config RootFS diff-ID mismatch independent of lifecycle metadata", func() {
			ref := OCILayoutInspection{
				LayoutDir: "/ref", Complete: true,
				DiffIDs:           []string{"sha256:aaaa"},
				LifecycleMetadata: LifecycleMetadata{Present: true, DiffIDs: []string{"sha256:aaaa"}},
			}
			cand := OCILayoutInspection{
				LayoutDir: "/cand", Complete: true,
				DiffIDs:           []string{"sha256:cccc"},
				LifecycleMetadata: LifecycleMetadata{Present: true, DiffIDs: []string{"sha256:aaaa"}},
			}
			report := CompareParity(ref, cand)
			h.AssertEq(t, report.Match(), false)
			h.AssertEq(t, len(report.Differences), 1)
			h.AssertContains(t, report.Differences[0], "config RootFS layer diff ID 0 differs")
		})
	})
}
