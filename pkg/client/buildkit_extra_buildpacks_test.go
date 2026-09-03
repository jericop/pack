package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/buildpacks/pack/internal/fakes"
	"github.com/buildpacks/pack/pkg/buildpack"
	"github.com/buildpacks/pack/pkg/dist"
	h "github.com/buildpacks/pack/testhelpers"
)

// bpWithTargets builds a real fake BuildModule whose Descriptor().Targets() are exactly
// the provided targets, so we can exercise the buildkit agnostic-vs-per-arch classification
// (moduleIsPlatformAgnostic / stageAgnosticExtraBuildpacks) without any registry access.
func bpWithTargets(t *testing.T, id, version string, targets ...dist.Target) buildpack.BuildModule {
	t.Helper()
	bp, err := fakes.NewFakeBuildpack(dist.BuildpackDescriptor{
		WithInfo:    dist.ModuleInfo{ID: id, Version: version},
		WithTargets: targets,
	}, 0644)
	h.AssertNil(t, err)
	return bp
}

func TestModuleIsPlatformAgnostic(t *testing.T) {
	t.Run("no targets => agnostic (inline / old buildpack)", func(t *testing.T) {
		bp := bpWithTargets(t, "example/inline", "0.0.1")
		h.AssertEq(t, moduleIsPlatformAgnostic(bp), true)
	})

	t.Run("empty (zero-value) target => agnostic (createInlineBuildpack emits dist.Target{})", func(t *testing.T) {
		bp := bpWithTargets(t, "example/inline", "0.0.1", dist.Target{})
		h.AssertEq(t, moduleIsPlatformAgnostic(bp), true)
	})

	t.Run("wildcard OS (arch only) => agnostic", func(t *testing.T) {
		bp := bpWithTargets(t, "example/wild-os", "0.0.1", dist.Target{Arch: "amd64"})
		h.AssertEq(t, moduleIsPlatformAgnostic(bp), true)
	})

	t.Run("wildcard arch (os only) => agnostic", func(t *testing.T) {
		bp := bpWithTargets(t, "example/wild-arch", "0.0.1", dist.Target{OS: "linux"})
		h.AssertEq(t, moduleIsPlatformAgnostic(bp), true)
	})

	t.Run("single concrete os+arch target => NOT agnostic (per-arch image)", func(t *testing.T) {
		bp := bpWithTargets(t, "example/multiarch", "0.0.1", dist.Target{OS: "linux", Arch: "amd64"})
		h.AssertEq(t, moduleIsPlatformAgnostic(bp), false)
	})

	t.Run("multiple concrete os+arch targets => NOT agnostic", func(t *testing.T) {
		bp := bpWithTargets(t, "example/multiarch", "0.0.1",
			dist.Target{OS: "linux", Arch: "amd64"},
			dist.Target{OS: "linux", Arch: "arm64"},
		)
		h.AssertEq(t, moduleIsPlatformAgnostic(bp), false)
	})

	t.Run("any wildcard target among concrete ones => agnostic", func(t *testing.T) {
		bp := bpWithTargets(t, "example/mixed", "0.0.1",
			dist.Target{OS: "linux", Arch: "amd64"},
			dist.Target{OS: "linux"}, // wildcard arch
		)
		h.AssertEq(t, moduleIsPlatformAgnostic(bp), true)
	})
}

func TestStageAgnosticExtraBuildpacks(t *testing.T) {
	t.Run("stages only the agnostic modules; excludes concrete-target (per-arch image) modules", func(t *testing.T) {
		agnosticInline := bpWithTargets(t, "example/inline", "0.0.1")                                    // no targets
		agnosticWildcard := bpWithTargets(t, "example/wildcard", "0.0.2", dist.Target{OS: "linux"})      // wildcard arch
		perArch := bpWithTargets(t, "example/multiarch", "1.0.0", dist.Target{OS: "linux", Arch: "amd64"}) // concrete => excluded

		dir, cleanup, err := stageAgnosticExtraBuildpacks([]buildpack.BuildModule{agnosticInline, agnosticWildcard, perArch})
		h.AssertNil(t, err)
		defer cleanup()
		h.AssertNotEq(t, dir, "")

		// The two agnostic modules are staged under /cnb/buildpacks/{escapedID}/{version}.
		assertFileExists(t, filepath.Join(dir, "cnb", "buildpacks", "example_inline", "0.0.1", "buildpack.toml"))
		assertFileExists(t, filepath.Join(dir, "cnb", "buildpacks", "example_wildcard", "0.0.2", "buildpack.toml"))

		// The concrete-target (multi-arch image) module MUST NOT be staged here (it is
		// delivered per-arch via its per-platform child image, avoiding double-injection).
		perArchRoot := filepath.Join(dir, "cnb", "buildpacks", "example_multiarch", "1.0.0")
		_, statErr := os.Stat(perArchRoot)
		h.AssertEq(t, os.IsNotExist(statErr), true)
	})

	t.Run("all modules per-arch (concrete targets) => empty dir, no-op cleanup", func(t *testing.T) {
		perArchA := bpWithTargets(t, "example/a", "1.0.0", dist.Target{OS: "linux", Arch: "amd64"})
		perArchB := bpWithTargets(t, "example/b", "1.0.0", dist.Target{OS: "linux", Arch: "arm64"})

		dir, cleanup, err := stageAgnosticExtraBuildpacks([]buildpack.BuildModule{perArchA, perArchB})
		h.AssertNil(t, err)
		defer cleanup()
		h.AssertEq(t, dir, "")
	})

	t.Run("no modules => empty dir, no-op cleanup", func(t *testing.T) {
		dir, cleanup, err := stageAgnosticExtraBuildpacks(nil)
		h.AssertNil(t, err)
		h.AssertEq(t, dir, "")
		cleanup()
	})
}
