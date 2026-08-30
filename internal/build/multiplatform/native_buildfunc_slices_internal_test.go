package multiplatform

import (
	"context"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
	"google.golang.org/protobuf/proto"

	"github.com/buildpacks/lifecycle/phase/emit"
	h "github.com/buildpacks/pack/testhelpers"
)

// This is the pack-side (consumer) seam of app slicing, the counterpart to the
// lifecycle SliceLayers Source-recording seam tested in cnb-lifecycle
// layers/slices_test.go. A sliced app layer arrives as an emit.LayerOp whose
// Source.Include holds the EXACT relative paths that slice contains. copyFromSource
// must turn that into an llb.Copy that restricts the copy to those paths via
// IncludePatterns (with CopyDirContentsOnly + chown to the slice uid/gid), so the
// assembled image reproduces the same per-slice split the lifecycle computed —
// WITHOUT extracting any tar.
//
// NOTE (RFC): app slices are not exercised end-to-end by the default paketo
// buildpacks used in our sample matrix (slices come from a buildpack's launch.toml,
// and the samples' buildpacks don't emit them), so slice correctness is verified at
// the two seams by unit tests: this test (pack consumer: Include -> llb.Copy
// IncludePatterns) and layers/slices_test.go (lifecycle producer: SliceLayers ->
// Source.Include). A full end-to-end slice build would need a custom buildpack that
// writes [[slices]] to launch.toml.

func TestCopyFromSourceSlices(t *testing.T) {
	// findCopyActions marshals a state and returns every FileActionCopy in it.
	findCopyActions := func(t *testing.T, st llb.State) []*pb.FileActionCopy {
		t.Helper()
		def, err := st.Marshal(context.Background())
		h.AssertNil(t, err)
		var copies []*pb.FileActionCopy
		for _, dt := range def.Def {
			var op pb.Op
			if err := proto.Unmarshal(dt, &op); err != nil {
				continue
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

	when := func(name string, fn func(t *testing.T)) { t.Run(name, fn) }

	when("a slice layer with Include patterns", func(t *testing.T) {
		base := llb.Image("scratch")
		built := llb.Image("built")
		layer := emit.LayerOp{
			ID: "slice-1",
			Source: &emit.LayerSource{
				Dir:     "/workspace",
				Include: []string{"some-dir", "some-dir/file.md", "some-dir/some-file.txt"},
				UID:     1234,
				GID:     4321,
			},
		}

		st := copyFromSource(base, built, layer, "linux/arm64")
		copies := findCopyActions(t, st)
		if len(copies) == 0 {
			t.Fatalf("expected at least one copy action")
		}
		c := copies[len(copies)-1] // the copy we added is the topmost file action

		// IncludePatterns must equal the slice's Include list exactly (order preserved).
		h.AssertEq(t, c.IncludePatterns, layer.Source.Include)
		// Directory-contents copy for a dir-backed slice.
		h.AssertEq(t, c.DirCopyContents, true)
		// Ownership normalized to the slice uid/gid (by-id form).
		if c.Owner == nil || c.Owner.User == nil || c.Owner.Group == nil {
			t.Fatalf("expected chown owner set on the copy, got %+v", c.Owner)
		}
		h.AssertEq(t, int(c.Owner.User.GetByID()), 1234)
		h.AssertEq(t, int(c.Owner.Group.GetByID()), 4321)
	})

	when("a whole-dir layer (no Include) copies everything", func(t *testing.T) {
		base := llb.Image("scratch")
		built := llb.Image("built")
		layer := emit.LayerOp{
			ID: "app",
			Source: &emit.LayerSource{
				Dir: "/workspace",
				UID: 1000,
				GID: 1000,
			},
		}
		st := copyFromSource(base, built, layer, "linux/arm64")
		copies := findCopyActions(t, st)
		if len(copies) == 0 {
			t.Fatalf("expected a copy action")
		}
		c := copies[len(copies)-1]
		// No Include patterns for a whole-dir copy.
		h.AssertEq(t, len(c.IncludePatterns), 0)
		h.AssertEq(t, c.DirCopyContents, true)
	})
}
