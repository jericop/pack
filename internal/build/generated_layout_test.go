package build

import (
	"os"
	"path/filepath"
	"testing"

	h "github.com/buildpacks/pack/testhelpers"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	h.AssertNil(t, os.MkdirAll(filepath.Dir(path), 0755))
	h.AssertNil(t, os.WriteFile(path, []byte(contents), 0644))
}

func TestDiscoverGeneratedLayout(t *testing.T) {
	t.Run("missing generated dir returns empty layout, no error", func(t *testing.T) {
		layout, err := DiscoverGeneratedLayout(filepath.Join(t.TempDir(), "does-not-exist"))
		h.AssertNil(t, err)
		h.AssertEq(t, layout.HasBuild(), false)
		h.AssertEq(t, layout.HasRun(), false)
	})

	t.Run("newer layout: build.Dockerfile only", func(t *testing.T) {
		gen := t.TempDir()
		writeFile(t, filepath.Join(gen, "ext-a", "build.Dockerfile"), "ARG base_image\nFROM ${base_image}\n")
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, layout.HasBuild(), true)
		h.AssertEq(t, layout.HasRun(), false)
		h.AssertEq(t, len(layout.BuildDockerfiles), 1)
		h.AssertEq(t, layout.BuildDockerfiles[0].ExtID, "ext-a")
	})

	t.Run("newer layout: run.Dockerfile only", func(t *testing.T) {
		gen := t.TempDir()
		writeFile(t, filepath.Join(gen, "ext-a", "run.Dockerfile"), "FROM some-run-image\n")
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, layout.HasBuild(), false)
		h.AssertEq(t, layout.HasRun(), true)
		h.AssertEq(t, layout.RunDockerfiles[0].ExtID, "ext-a")
	})

	t.Run("newer layout: both build and run", func(t *testing.T) {
		gen := t.TempDir()
		writeFile(t, filepath.Join(gen, "ext-a", "build.Dockerfile"), "ARG base_image\nFROM ${base_image}\n")
		writeFile(t, filepath.Join(gen, "ext-a", "run.Dockerfile"), "FROM some-run-image\n")
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, layout.HasBuild(), true)
		h.AssertEq(t, layout.HasRun(), true)
	})

	t.Run("older layout: generated/build/<ext-id> presence counts as build extension", func(t *testing.T) {
		gen := t.TempDir()
		// bare directory, no Dockerfile file (matches the daemon's historical fixture)
		h.AssertNil(t, os.MkdirAll(filepath.Join(gen, "build", "ext-old"), 0755))
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, layout.HasBuild(), true)
		h.AssertEq(t, layout.BuildDockerfiles[0].ExtID, "ext-old")
	})

	t.Run("context.build takes precedence over context for build Dockerfile", func(t *testing.T) {
		gen := t.TempDir()
		writeFile(t, filepath.Join(gen, "ext-a", "build.Dockerfile"), "ARG base_image\nFROM ${base_image}\n")
		h.AssertNil(t, os.MkdirAll(filepath.Join(gen, "ext-a", "context"), 0755))
		h.AssertNil(t, os.MkdirAll(filepath.Join(gen, "ext-a", "context.build"), 0755))
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, filepath.Base(layout.BuildDockerfiles[0].ContextDir), "context.build")
	})

	t.Run("shared context used when no image-specific folder", func(t *testing.T) {
		gen := t.TempDir()
		writeFile(t, filepath.Join(gen, "ext-a", "run.Dockerfile"), "FROM x\n")
		h.AssertNil(t, os.MkdirAll(filepath.Join(gen, "ext-a", "context"), 0755))
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, filepath.Base(layout.RunDockerfiles[0].ContextDir), "context")
	})

	t.Run("no context folder => empty context dir (app default)", func(t *testing.T) {
		gen := t.TempDir()
		writeFile(t, filepath.Join(gen, "ext-a", "build.Dockerfile"), "ARG base_image\nFROM ${base_image}\n")
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, layout.BuildDockerfiles[0].ContextDir, "")
	})

	t.Run("reads extend-config.toml build args", func(t *testing.T) {
		gen := t.TempDir()
		writeFile(t, filepath.Join(gen, "ext-a", "build.Dockerfile"), "ARG base_image\nFROM ${base_image}\n")
		writeFile(t, filepath.Join(gen, "ext-a", "extend-config.toml"), "[build.args]\n  foo = \"bar\"\n")
		layout, err := DiscoverGeneratedLayout(gen)
		h.AssertNil(t, err)
		h.AssertEq(t, layout.BuildDockerfiles[0].ExtendArgs["foo"], "bar")
	})
}
