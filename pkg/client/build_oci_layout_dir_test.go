package client

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestResolveOCILayoutOutputDir(t *testing.T) {
	spec.Run(t, "resolve_oci_layout_output_dir", testResolveOCILayoutOutputDir, spec.Report(report.Terminal{}))
}

func testResolveOCILayoutOutputDir(t *testing.T, when spec.G, it spec.S) {
	const buildID = "abc12345"

	var (
		subject *Client
		outBuf  bytes.Buffer
	)

	it.Before(func() {
		outBuf.Reset()
		subject = &Client{logger: logging.NewLogWithWriters(&outBuf, &outBuf)}
	})

	when("no output dir is supplied", func() {
		it("returns empty (executor uses and cleans up a temp dir)", func() {
			dir, err := subject.resolveOCILayoutOutputDir(BuildOptions{
				BuildkitExportMode: "oci-layout",
			}, buildID)
			h.AssertNil(t, err)
			h.AssertEq(t, dir, "")
		})

		it("errors when -clear is set without a dir", func() {
			_, err := subject.resolveOCILayoutOutputDir(BuildOptions{
				BuildkitExportMode:        "oci-layout",
				BuildkitOCILayoutDirClear: true,
			}, buildID)
			h.AssertError(t, err, "--buildkit-oci-layout-dir-clear requires --buildkit-oci-layout-dir")
		})
	})

	when("an output dir is supplied", func() {
		it("returns a unique per-build subdirectory and creates it", func() {
			base := t.TempDir()
			dir, err := subject.resolveOCILayoutOutputDir(BuildOptions{
				BuildkitExportMode:   "oci-layout",
				BuildkitOCILayoutDir: base,
			}, buildID)
			h.AssertNil(t, err)
			h.AssertEq(t, dir, filepath.Join(base, "build-"+buildID))
			h.AssertPathExists(t, dir)
		})

		it("errors when export mode is not oci-layout", func() {
			base := t.TempDir()
			_, err := subject.resolveOCILayoutOutputDir(BuildOptions{
				BuildkitExportMode:   "registry",
				BuildkitOCILayoutDir: base,
			}, buildID)
			h.AssertError(t, err, "only valid with --buildkit-export-mode=oci-layout")
		})

		it("produces distinct subdirectories for different build IDs", func() {
			base := t.TempDir()
			opts := BuildOptions{BuildkitExportMode: "oci-layout", BuildkitOCILayoutDir: base}

			dir1, err := subject.resolveOCILayoutOutputDir(opts, "build0001")
			h.AssertNil(t, err)
			dir2, err := subject.resolveOCILayoutOutputDir(opts, "build0002")
			h.AssertNil(t, err)
			h.AssertNotEq(t, dir1, dir2)
		})
	})

	when("-clear is set with an output dir", func() {
		it("removes prior contents of the base dir before creating the per-build subdir", func() {
			base := t.TempDir()
			// A stale artifact from a "previous build".
			stale := filepath.Join(base, "build-oldbuild")
			h.AssertNil(t, os.MkdirAll(stale, 0755))
			h.AssertPathExists(t, stale)

			dir, err := subject.resolveOCILayoutOutputDir(BuildOptions{
				BuildkitExportMode:        "oci-layout",
				BuildkitOCILayoutDir:      base,
				BuildkitOCILayoutDirClear: true,
			}, buildID)
			h.AssertNil(t, err)

			// The stale build dir is gone; the new per-build subdir exists.
			assertPathDoesNotExist(t, stale)
			h.AssertPathExists(t, dir)
			h.AssertEq(t, dir, filepath.Join(base, "build-"+buildID))
		})
	})
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected path %q to not exist, but it does (err=%v)", path, err)
	}
}
