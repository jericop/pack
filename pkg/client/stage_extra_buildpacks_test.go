package client

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildpacks/pack/pkg/archive"
	"github.com/buildpacks/pack/pkg/buildpack"
	h "github.com/buildpacks/pack/testhelpers"
)

// readerBlob is a minimal buildpack.Blob backed by an in-memory tar reader, so we
// can construct real BuildModules without touching disk (mirrors the pattern in
// pkg/buildpack/buildpack_test.go).
type readerBlob struct {
	openFn func() io.ReadCloser
}

func (b *readerBlob) Open() (io.ReadCloser, error) { return b.openFn(), nil }

// newTestBuildModule builds a real BuildModule whose Open() yields a distribution
// tar rooted at /cnb/buildpacks/{escapedID}/{version}/*.
func newTestBuildModule(t *testing.T, id, version string) buildpack.BuildModule {
	t.Helper()
	bp, err := buildpack.FromBuildpackRootBlob(&readerBlob{
		openFn: func() io.ReadCloser {
			tb := archive.TarBuilder{}
			tb.AddFile("buildpack.toml", 0700, time.Now(), []byte(`
api = "0.10"

[buildpack]
id = "`+id+`"
version = "`+version+`"

[[stacks]]
id = "*"
`))
			tb.AddDir("bin", 0700, time.Now())
			tb.AddFile("bin/detect", 0755, time.Now(), []byte("#!/bin/sh\nexit 0\n"))
			tb.AddFile("bin/build", 0755, time.Now(), []byte("#!/bin/sh\nexit 0\n"))
			return tb.Reader(archive.DefaultTarWriterFactory())
		},
	}, archive.DefaultTarWriterFactory(), nil)
	h.AssertNil(t, err)
	return bp
}

func TestStageExtraBuildpacks(t *testing.T) {
	t.Run("no modules returns empty dir and a no-op cleanup", func(t *testing.T) {
		dir, cleanup, err := stageExtraBuildpacks(nil)
		h.AssertNil(t, err)
		h.AssertEq(t, dir, "")
		// cleanup must be safe to call even with no staging dir.
		cleanup()
	})

	t.Run("stages modules under /cnb/buildpacks/{id}/{version}", func(t *testing.T) {
		bpA := newTestBuildModule(t, "example/inject-marker", "0.0.1")
		bpB := newTestBuildModule(t, "example/second", "1.2.3")

		dir, cleanup, err := stageExtraBuildpacks([]buildpack.BuildModule{bpA, bpB})
		h.AssertNil(t, err)
		defer cleanup()
		h.AssertNotEq(t, dir, "")

		// EscapedID replaces '/' with '_' per the distribution spec.
		aRoot := filepath.Join(dir, "cnb", "buildpacks", "example_inject-marker", "0.0.1")
		bRoot := filepath.Join(dir, "cnb", "buildpacks", "example_second", "1.2.3")

		for _, root := range []string{aRoot, bRoot} {
			assertFileExists(t, filepath.Join(root, "buildpack.toml"))
			assertFileExists(t, filepath.Join(root, "bin", "detect"))
			assertFileExists(t, filepath.Join(root, "bin", "build"))
		}
	})

	t.Run("cleanup removes the staging dir", func(t *testing.T) {
		bp := newTestBuildModule(t, "example/x", "0.0.1")
		dir, cleanup, err := stageExtraBuildpacks([]buildpack.BuildModule{bp})
		h.AssertNil(t, err)
		h.AssertNotEq(t, dir, "")

		_, statErr := os.Stat(dir)
		h.AssertNil(t, statErr)

		cleanup()
		_, statErr = os.Stat(dir)
		h.AssertEq(t, os.IsNotExist(statErr), true)
	})
}

func TestExtractModuleToDir(t *testing.T) {
	t.Run("preserves the /cnb/buildpacks tree and file contents", func(t *testing.T) {
		bp := newTestBuildModule(t, "example/marker", "9.9.9")
		dest := t.TempDir()

		h.AssertNil(t, extractModuleToDir(bp, dest))

		root := filepath.Join(dest, "cnb", "buildpacks", "example_marker", "9.9.9")
		assertFileExists(t, filepath.Join(root, "buildpack.toml"))
		assertFileExists(t, filepath.Join(root, "bin", "detect"))

		// buildpack.toml should carry the declared id/version.
		data, err := os.ReadFile(filepath.Join(root, "buildpack.toml"))
		h.AssertNil(t, err)
		h.AssertContains(t, string(data), `id = "example/marker"`)
		h.AssertContains(t, string(data), `version = "9.9.9"`)
	})
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist at %s: %v", path, err)
	}
}
