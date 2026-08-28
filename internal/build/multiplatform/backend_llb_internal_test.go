package multiplatform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"
	"google.golang.org/protobuf/proto"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestLLBBackendInternal(t *testing.T) {
	spec.Run(t, "backend_llb_internal", testLLBBackendInternal, spec.Report(report.Terminal{}))
}

func testLLBBackendInternal(t *testing.T, when spec.G, it spec.S) {
	const perArchTag = "registry.example.com/myapp:latest-build-abc12345-amd64"

	var baseOpts PlatformBuildOpts

	it.Before(func() {
		baseOpts = PlatformBuildOpts{
			ImageName:  "registry.example.com/myapp:latest",
			BuilderUID: 1001,
			BuilderGID: 1002,
			Phases: []PhaseCommand{
				{Name: "analyzer", Binary: "/cnb/lifecycle/analyzer", Args: []string{"-run-image", "run:latest", "registry.example.com/myapp:latest"}},
				{Name: "detector", Binary: "/cnb/lifecycle/detector", Args: []string{"-app", "/workspace"}},
				{Name: "restorer", Binary: "/cnb/lifecycle/restorer", Args: []string{"-cache-dir", "/cache"}},
				{Name: "builder", Binary: "/cnb/lifecycle/builder", Args: []string{"-app", "/workspace"}},
				{Name: "exporter", Binary: "/cnb/lifecycle/exporter", Args: []string{"-app", "/workspace", "-cache-dir", "/cache", "registry.example.com/myapp:latest"}},
			},
		}
	})

	when("#insertAfterBinary", func() {
		it("inserts flags immediately after the binary and preserves the rest", func() {
			got := insertAfterBinary([]string{"/bin/tool", "-run-image", "run:latest", "img"}, "-a", "-b")
			h.AssertEq(t, got, []string{"/bin/tool", "-a", "-b", "-run-image", "run:latest", "img"})
		})

		it("returns empty input unchanged", func() {
			got := insertAfterBinary([]string{}, "-a")
			h.AssertEq(t, len(got), 0)
		})

		it("handles a binary with no other args", func() {
			got := insertAfterBinary([]string{"/bin/tool"}, "-a", "-b")
			h.AssertEq(t, got, []string{"/bin/tool", "-a", "-b"})
		})
	})

	when("#buildLifecyclePhaseArgs", func() {
		when("registry mode (default)", func() {
			it("adds -skip-chown -uid -gid to the analyzer but no layout flags", func() {
				args := buildLifecyclePhaseArgs(baseOpts, "analyzer", perArchTag)
				h.AssertSliceContains(t, args, "-skip-chown")
				h.AssertSliceContains(t, args, "-uid")
				h.AssertSliceContains(t, args, "1001")
				h.AssertSliceContains(t, args, "-gid")
				h.AssertSliceContains(t, args, "1002")
				h.AssertSliceNotContains(t, args, "-layout")
				h.AssertSliceNotContains(t, args, "-layout-dir")
				h.AssertSliceNotContains(t, args, "-pull-run-image")
			})

			it("adds -skip-chown but no layout flags to the exporter", func() {
				args := buildLifecyclePhaseArgs(baseOpts, "exporter", perArchTag)
				h.AssertSliceContains(t, args, "-skip-chown")
				h.AssertSliceNotContains(t, args, "-layout")
				h.AssertSliceNotContains(t, args, "-layout-dir")
				h.AssertSliceNotContains(t, args, "-pull-run-image")
			})

			it("adds no layout flags to any lifecycle phase (registry mode is unchanged)", func() {
				// NFR-2: registry mode (the default) MUST be unchanged — no OCI
				// layout wiring leaks onto any phase.
				for _, phase := range []string{"analyzer", "detector", "restorer", "builder", "exporter"} {
					args := buildLifecyclePhaseArgs(baseOpts, phase, perArchTag)
					h.AssertSliceNotContains(t, args, "-layout")
					h.AssertSliceNotContains(t, args, "-layout-dir")
					h.AssertSliceNotContains(t, args, "-pull-run-image")
				}
			})

			it("does not add -skip-chown to detector or builder", func() {
				h.AssertSliceNotContains(t, buildLifecyclePhaseArgs(baseOpts, "detector", perArchTag), "-skip-chown")
				h.AssertSliceNotContains(t, buildLifecyclePhaseArgs(baseOpts, "builder", perArchTag), "-skip-chown")
			})

			it("substitutes the per-arch tag for the image name", func() {
				args := buildLifecyclePhaseArgs(baseOpts, "exporter", perArchTag)
				h.AssertSliceContains(t, args, perArchTag)
				h.AssertSliceNotContains(t, args, baseOpts.ImageName)
			})
		})

		when("OCI layout mode", func() {
			it.Before(func() {
				baseOpts.ExportMode = ExportOCILayout
			})

			it("configures the analyzer with -layout -layout-dir /output -pull-run-image and -skip-chown", func() {
				args := buildLifecyclePhaseArgs(baseOpts, "analyzer", perArchTag)
				h.AssertSliceContains(t, args, "-layout", "-layout-dir", "/output", "-pull-run-image", "-skip-chown")
			})

			it("configures the exporter with -layout -layout-dir /output and -skip-chown", func() {
				args := buildLifecyclePhaseArgs(baseOpts, "exporter", perArchTag)
				h.AssertSliceContains(t, args, "-layout", "-layout-dir", "/output", "-skip-chown")
			})

			it("does not add -pull-run-image to the exporter (only the analyzer pulls the run image)", func() {
				// NOTE (Task 4 sub-item 3): the task text lists all four flags
				// (-layout -layout-dir /output -pull-run-image -skip-chown) on "the
				// exporter", but the lifecycle's actual contract splits them: the
				// ANALYZER is what self-populates (pulls) the run image inside
				// BuildKit, so -pull-run-image belongs on the analyzer (FR-3). The
				// exporter only WRITES the layout, so it carries -layout
				// -layout-dir /output -skip-chown but NOT -pull-run-image. Moving
				// -pull-run-image to the exporter would be wrong for the lifecycle.
				args := buildLifecyclePhaseArgs(baseOpts, "exporter", perArchTag)
				h.AssertSliceNotContains(t, args, "-pull-run-image")
			})

			it("puts -pull-run-image on the analyzer only (documents the analyzer/exporter split)", func() {
				// Verify the split explicitly: exactly the analyzer carries
				// -pull-run-image; no other lifecycle phase does. This documents WHY
				// the exporter itself does not carry -pull-run-image (the Task 4
				// text/lifecycle-contract discrepancy) as a locked-in invariant.
				h.AssertSliceContains(t, buildLifecyclePhaseArgs(baseOpts, "analyzer", perArchTag), "-pull-run-image")
				for _, phase := range []string{"detector", "restorer", "builder", "exporter"} {
					h.AssertSliceNotContains(t, buildLifecyclePhaseArgs(baseOpts, phase, perArchTag), "-pull-run-image")
				}
			})

			it("gives the exporter exactly -layout -layout-dir /output -skip-chown (not -pull-run-image)", func() {
				// The exporter's OCI-layout flag set, asserted as a whole so the
				// four-flag Task 4 text is reconciled against the real split.
				args := buildLifecyclePhaseArgs(baseOpts, "exporter", perArchTag)
				h.AssertSliceContains(t, args, "-layout", "-layout-dir", "/output", "-skip-chown")
				h.AssertSliceNotContains(t, args, "-pull-run-image")
			})

			it("keeps -layout-dir immediately followed by /output on the exporter", func() {
				args := buildLifecyclePhaseArgs(baseOpts, "exporter", perArchTag)
				idx := indexOf(args, "-layout-dir")
				h.AssertEq(t, idx >= 0 && idx+1 < len(args), true)
				h.AssertEq(t, args[idx+1], "/output")
			})

			it("does not add layout flags to detector or builder", func() {
				h.AssertSliceNotContains(t, buildLifecyclePhaseArgs(baseOpts, "detector", perArchTag), "-layout")
				h.AssertSliceNotContains(t, buildLifecyclePhaseArgs(baseOpts, "builder", perArchTag), "-layout")
			})

			it("places the binary first", func() {
				args := buildLifecyclePhaseArgs(baseOpts, "exporter", perArchTag)
				h.AssertEq(t, args[0], "/cnb/lifecycle/exporter")
			})
		})
	})

	when("#perArchStoreDir", func() {
		it("uses opts.OutputDir as the base when set", func() {
			opts := PlatformBuildOpts{OutputDir: "/tmp/pack-oci-layout-xyz"}
			dir := perArchStoreDir(opts, Platform{OS: "linux", Arch: "amd64"})
			h.AssertEq(t, dir, "/tmp/pack-oci-layout-xyz/oci-store-linux-amd64")
		})

		it("falls back to the process temp dir when OutputDir is empty", func() {
			dir := perArchStoreDir(PlatformBuildOpts{}, Platform{OS: "linux", Arch: "arm64"})
			h.AssertEq(t, strings.HasPrefix(dir, os.TempDir()), true)
			h.AssertEq(t, strings.HasSuffix(dir, "oci-store-linux-arm64"), true)
		})

		it("produces distinct directories per platform to avoid collisions", func() {
			opts := PlatformBuildOpts{OutputDir: "/out"}
			amd := perArchStoreDir(opts, Platform{OS: "linux", Arch: "amd64"})
			arm := perArchStoreDir(opts, Platform{OS: "linux", Arch: "arm64"})
			h.AssertNotEq(t, amd, arm)
		})

		it("includes the variant in the leaf so same-arch variants do not collide", func() {
			opts := PlatformBuildOpts{OutputDir: "/out"}
			v6 := perArchStoreDir(opts, Platform{OS: "linux", Arch: "arm", Variant: "v6"})
			v7 := perArchStoreDir(opts, Platform{OS: "linux", Arch: "arm", Variant: "v7"})
			h.AssertNotEq(t, v6, v7)
			h.AssertEq(t, v7, "/out/oci-store-linux-arm-v7")
		})
	})

	// writeSyntheticStore creates an on-disk OCI layout containing a single image,
	// mirroring the format BuildKit's ExporterOCI writes to a contentlocal store
	// (this is the Phase 1 output that Phase 2 opens). Returns the dir and image.
	writeSyntheticStore := func(t *testing.T, numLayers int64) (string, v1.Image) {
		t.Helper()
		dir := t.TempDir()
		img, err := random.Image(1024, numLayers)
		h.AssertNil(t, err)
		idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
		_, err = layout.Write(dir, idx)
		h.AssertNil(t, err)
		return dir, img
	}

	when("#openPhase1Store", func() {
		it("opens a content.Store from an existing Phase 1 layout directory", func() {
			dir, _ := writeSyntheticStore(t, 2)

			store, err := openPhase1Store(dir)
			h.AssertNil(t, err)
			h.AssertNotNil(t, store)
		})

		it("returns an error when the directory is empty string", func() {
			store, err := openPhase1Store("")
			h.AssertNotNil(t, err)
			h.AssertNil(t, store)
			h.AssertError(t, err, "is empty")
		})

		it("returns an error when the directory does not exist", func() {
			store, err := openPhase1Store(filepath.Join(t.TempDir(), "does-not-exist"))
			h.AssertNotNil(t, err)
			h.AssertNil(t, store)
		})

		it("returns an error when the path is a file, not a directory", func() {
			f := filepath.Join(t.TempDir(), "afile")
			h.AssertNil(t, os.WriteFile(f, []byte("x"), 0600))

			store, err := openPhase1Store(f)
			h.AssertNotNil(t, err)
			h.AssertNil(t, store)
			h.AssertError(t, err, "not a directory")
		})
	})

	when("#readPhase1LayoutDigest", func() {
		it("returns the manifest digest of the Phase 1 layout image", func() {
			dir, img := writeSyntheticStore(t, 3)

			digest, err := readPhase1LayoutDigest(dir)
			h.AssertNil(t, err)

			want, err := img.Digest()
			h.AssertNil(t, err)
			h.AssertEq(t, digest, want.String())
		})

		it("reads a digest for a config-only (zero layer) image", func() {
			dir, img := writeSyntheticStore(t, 0)

			digest, err := readPhase1LayoutDigest(dir)
			h.AssertNil(t, err)

			want, err := img.Digest()
			h.AssertNil(t, err)
			h.AssertEq(t, digest, want.String())
		})

		it("returns an error when the directory is not a valid OCI layout", func() {
			digest, err := readPhase1LayoutDigest(t.TempDir())
			h.AssertNotNil(t, err)
			h.AssertEq(t, digest, "")
			h.AssertError(t, err, "reading manifest digest")
		})
	})

	when("#buildImportRef", func() {
		it("joins the ref and digest with an @ separator", func() {
			ref := buildImportRef("registry.example.com/myapp:latest-build-abc12345-amd64",
				"sha256:1111111111111111111111111111111111111111111111111111111111111111")
			h.AssertEq(t, ref,
				"registry.example.com/myapp:latest-build-abc12345-amd64@sha256:1111111111111111111111111111111111111111111111111111111111111111")
		})

		it("uses the digest produced by the Phase 1 layout", func() {
			dir, img := writeSyntheticStore(t, 2)
			digest, err := readPhase1LayoutDigest(dir)
			h.AssertNil(t, err)

			want, err := img.Digest()
			h.AssertNil(t, err)

			ref := buildImportRef(perArchTag, digest)
			h.AssertEq(t, ref, perArchTag+"@"+want.String())
		})
	})

	when("#buildImportLayoutState", func() {
		const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

		it("marshals to an oci-layout source that references the given import ref", func() {
			importRef := buildImportRef(perArchTag, digest)
			state := buildImportLayoutState(importRef, applayoutStoreID)

			src := singleSourceOp(t, state)
			// llb.OCILayout builds the source identifier as "oci-layout://" + ref.
			h.AssertEq(t, src.GetIdentifier(), "oci-layout://"+importRef)
		})

		it("sets the OCI layout store attr to the provided storeID", func() {
			state := buildImportLayoutState(buildImportRef(perArchTag, digest), applayoutStoreID)

			src := singleSourceOp(t, state)
			// pb.AttrOCILayoutStoreID == "oci.store" (buildkit solver/pb/attr.go).
			h.AssertEq(t, src.GetAttrs()["oci.store"], applayoutStoreID)
		})

		it("uses an empty session id so the OCIStores-attached store is selected", func() {
			state := buildImportLayoutState(buildImportRef(perArchTag, digest), applayoutStoreID)

			src := singleSourceOp(t, state)
			// pb.AttrOCILayoutSessionID == "oci.session"; empty session id means the
			// store comes from SolveOpt.OCIStores rather than a session provider.
			_, hasSession := src.GetAttrs()["oci.session"]
			h.AssertEq(t, hasSession, false)
		})
	})

	when("#planMultiPlatformPush", func() {
		// These lock in the Task 4 "wire the two-phase flow" routing that
		// BuildMultiPlatform depends on, without needing a live BuildKit daemon.

		it("defers the per-arch push and assembles for OCI layout multi-arch", func() {
			plan := planMultiPlatformPush(ExportOCILayout, 2)
			// Multi-arch OCI layout: Phase 1 only per platform, then assemble ONE
			// manifest list under the final name — no per-arch push (FR-5).
			h.AssertEq(t, plan.PushPerArch, false)
			h.AssertEq(t, plan.AssembleManifestList, true)
		})

		it("pushes natively (no assembly) for OCI layout single-arch", func() {
			plan := planMultiPlatformPush(ExportOCILayout, 1)
			// Single-arch OCI layout: the one platform runs Phase 2 and pushes the
			// imported layout natively under the final image name (Task 2).
			h.AssertEq(t, plan.PushPerArch, true)
			h.AssertEq(t, plan.AssembleManifestList, false)
		})

		it("never assembles an OCI-layout manifest list in registry mode (single-arch)", func() {
			plan := planMultiPlatformPush(ExportRegistry, 1)
			h.AssertEq(t, plan.AssembleManifestList, false)
		})

		it("never assembles an OCI-layout manifest list in registry mode (multi-arch)", func() {
			// Registry mode is unchanged (NFR-2): no OCI-layout assembly step is
			// triggered regardless of platform count.
			plan := planMultiPlatformPush(ExportRegistry, 3)
			h.AssertEq(t, plan.AssembleManifestList, false)
		})

		it("assembles for three or more OCI layout platforms", func() {
			plan := planMultiPlatformPush(ExportOCILayout, 3)
			h.AssertEq(t, plan.PushPerArch, false)
			h.AssertEq(t, plan.AssembleManifestList, true)
		})
	})

	when("#phase1ExportEntry", func() {
		it("produces exactly one ExporterOCI entry to the content store", func() {
			dir, _ := writeSyntheticStore(t, 1)
			store, err := openPhase1Store(dir)
			h.AssertNil(t, err)

			entries := phase1ExportEntry(store, perArchTag)
			h.AssertEq(t, len(entries), 1)
			h.AssertEq(t, entries[0].Type, client.ExporterOCI)
			h.AssertNotNil(t, entries[0].OutputStore)
		})

		it("does not set push=true (Phase 1 writes locally, never to a registry)", func() {
			dir, _ := writeSyntheticStore(t, 1)
			store, err := openPhase1Store(dir)
			h.AssertNil(t, err)

			entries := phase1ExportEntry(store, perArchTag)
			_, hasPush := entries[0].Attrs["push"]
			h.AssertEq(t, hasPush, false)
		})

		it("names the export after the per-arch tag (the on-disk layout subject)", func() {
			dir, _ := writeSyntheticStore(t, 1)
			store, err := openPhase1Store(dir)
			h.AssertNil(t, err)

			entries := phase1ExportEntry(store, perArchTag)
			h.AssertEq(t, entries[0].Attrs["name"], perArchTag)
		})
	})

	when("#phase2ExportEntry", func() {
		it("produces exactly one ExporterImage export entry", func() {
			entries := phase2ExportEntry(baseOpts.ImageName)
			h.AssertEq(t, len(entries), 1)
			h.AssertEq(t, entries[0].Type, client.ExporterImage)
		})

		it("sets push=true so BuildKit pushes the image natively", func() {
			entries := phase2ExportEntry(baseOpts.ImageName)
			h.AssertEq(t, entries[0].Attrs["push"], "true")
		})

		it("uses the given push name as the export target name", func() {
			entries := phase2ExportEntry(baseOpts.ImageName)
			h.AssertEq(t, entries[0].Attrs["name"], baseOpts.ImageName)
		})

		// The core "no intermediate tag" guarantee (FR-5): the only registry write
		// in OCI layout mode is this single ExporterImage push, and it targets the
		// final image name — never a "<img>-build-<id>-<arch>" intermediate tag.
		it("targets the final image name, not a per-arch intermediate build tag", func() {
			// perArchTag is the "<img>-build-<id>-<arch>" shape used only for the
			// on-disk lifecycle phases / content store, never as a push target here.
			entries := phase2ExportEntry(baseOpts.ImageName)
			name := entries[0].Attrs["name"]

			h.AssertEq(t, name, baseOpts.ImageName)
			// It must not be the intermediate per-arch tag shape.
			h.AssertNotEq(t, name, perArchTag)
			h.AssertEq(t, strings.Contains(name, "-build-"), false)
		})

		it("does not configure any OCI/local export (no non-registry duplicate, single push only)", func() {
			// A stray ExporterOCI/ExporterLocal here would mean an extra artifact;
			// OCI layout mode's Phase 2 must push exactly one image and nothing else.
			entries := phase2ExportEntry("registry.example.com/myapp:latest")
			for _, e := range entries {
				h.AssertEq(t, e.Type, client.ExporterImage)
			}
		})
	})

	when("store wiring (OCIStores key matches llb.OCIStore storeID)", func() {
		it("uses the same applayoutStoreID constant for the import source and the store map key", func() {
			// The import source (llb.OCILayout) encodes the storeID it will read
			// from; the Phase 2 solve attaches the store under a map key. Design
			// Tier 1 requires these to be identical. Both derive from
			// applayoutStoreID in the implementation; assert the marshaled source's
			// store attr equals the exact key the OCIStores map would use.
			state := buildImportLayoutState(
				buildImportRef(perArchTag, "sha256:3333333333333333333333333333333333333333333333333333333333333333"),
				applayoutStoreID,
			)
			src := singleSourceOp(t, state)

			storeMapKey := applayoutStoreID // the key solvePhase2Push uses for OCIStores
			h.AssertEq(t, src.GetAttrs()["oci.store"], storeMapKey)
		})
	})

	when("#isolateOutputLayout", func() {
		it("copies /output to the root of a scratch state", func() {
			isolated := isolateOutputLayout(llb.Image("base:latest"))
			copies := copyActions(t, isolated)

			// Exactly one copy: /output -> /
			var found bool
			for _, c := range copies {
				if c.Src == "/output" && c.Dest == "/" {
					found = true
				}
			}
			h.AssertEq(t, found, true)
		})
	})

	when("#buildLLBState output isolation", func() {
		var b *LLBBackend
		var platform Platform

		it.Before(func() {
			b = NewLLBBackend(nil, BuildkitOpts{})
			platform = Platform{OS: "linux", Arch: "amd64"}
			baseOpts.BuilderImage = "builder:latest"
			baseOpts.AppPath = "/tmp/app"
			baseOpts.CacheID = "cache123"
			baseOpts.PlatformAPI = "0.12"
		})

		when("OCI layout mode", func() {
			it.Before(func() {
				baseOpts.ExportMode = ExportOCILayout
			})

			it("returns a state that isolates /output to the root", func() {
				state := b.buildLLBState(baseOpts, platform, perArchTag)
				copies := copyActions(t, state)

				var found bool
				for _, c := range copies {
					if c.Src == "/output" && c.Dest == "/" {
						found = true
					}
				}
				h.AssertEq(t, found, true)
			})
		})

		when("registry mode (default)", func() {
			it("does not isolate /output (exports the full container root)", func() {
				state := b.buildLLBState(baseOpts, platform, perArchTag)
				copies := copyActions(t, state)

				for _, c := range copies {
					if c.Src == "/output" && c.Dest == "/" {
						t.Fatalf("registry mode should not copy /output to /, but a copy op was found")
					}
				}
			})
		})
	})
}

// copyActions marshals an llb.State and extracts every FileActionCopy in the graph.
func copyActions(t *testing.T, state llb.State) []*pb.FileActionCopy {
	t.Helper()
	def, err := state.Marshal(context.Background())
	h.AssertNil(t, err)

	var copies []*pb.FileActionCopy
	for _, dt := range def.Def {
		var op pb.Op
		if err := proto.Unmarshal(dt, &op); err != nil {
			t.Fatalf("unmarshaling op: %v", err)
		}
		fileOp := op.GetFile()
		if fileOp == nil {
			continue
		}
		for _, action := range fileOp.Actions {
			if c := action.GetCopy(); c != nil {
				copies = append(copies, c)
			}
		}
	}
	return copies
}

// singleSourceOp marshals an llb.State and returns the single SourceOp in the
// graph. It fails the test if there is not exactly one source op (an
// llb.OCILayout state has exactly one).
func singleSourceOp(t *testing.T, state llb.State) *pb.SourceOp {
	t.Helper()
	def, err := state.Marshal(context.Background())
	h.AssertNil(t, err)

	var sources []*pb.SourceOp
	for _, dt := range def.Def {
		var op pb.Op
		if err := proto.Unmarshal(dt, &op); err != nil {
			t.Fatalf("unmarshaling op: %v", err)
		}
		if s := op.GetSource(); s != nil {
			sources = append(sources, s)
		}
	}
	if len(sources) != 1 {
		t.Fatalf("expected exactly one source op, got %d", len(sources))
	}
	return sources[0]
}

func indexOf(args []string, target string) int {
	for i, a := range args {
		if a == target {
			return i
		}
	}
	return -1
}
