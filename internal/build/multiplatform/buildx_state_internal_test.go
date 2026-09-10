package multiplatform

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestBuildxStateInternal(t *testing.T) {
	spec.Run(t, "buildx_state_internal", testBuildxStateInternal, spec.Report(report.Terminal{}))
}

func testBuildxStateInternal(t *testing.T, when spec.G, it spec.S) {
	when("resolveCurrentBuildxBuilder", func() {
		var cfgDir string
		var restoreEnv func()

		writeCurrent := func(content string) {
			h.AssertNil(t, os.MkdirAll(filepath.Join(cfgDir, "buildx"), 0o755))
			h.AssertNil(t, os.WriteFile(filepath.Join(cfgDir, "buildx", "current"), []byte(content), 0o644))
		}
		writeInstance := func(name, content string) {
			h.AssertNil(t, os.MkdirAll(filepath.Join(cfgDir, "buildx", "instances"), 0o755))
			h.AssertNil(t, os.WriteFile(filepath.Join(cfgDir, "buildx", "instances", name), []byte(content), 0o644))
		}

		it.Before(func() {
			var err error
			cfgDir, err = os.MkdirTemp("", "pack-buildx-cfg-")
			h.AssertNil(t, err)
			prevCfg, hadCfg := os.LookupEnv("DOCKER_CONFIG")
			prevBuilder, hadBuilder := os.LookupEnv("BUILDX_BUILDER")
			h.AssertNil(t, os.Setenv("DOCKER_CONFIG", cfgDir))
			h.AssertNil(t, os.Unsetenv("BUILDX_BUILDER"))
			restoreEnv = func() {
				if hadCfg {
					_ = os.Setenv("DOCKER_CONFIG", prevCfg)
				} else {
					_ = os.Unsetenv("DOCKER_CONFIG")
				}
				if hadBuilder {
					_ = os.Setenv("BUILDX_BUILDER", prevBuilder)
				} else {
					_ = os.Unsetenv("BUILDX_BUILDER")
				}
			}
		})

		it.After(func() {
			restoreEnv()
			_ = os.RemoveAll(cfgDir)
		})

		it("treats a missing buildx state as the docker-driver default", func() {
			b, err := resolveCurrentBuildxBuilder()
			h.AssertNil(t, err)
			h.AssertEq(t, b.Name, "")
			h.AssertEq(t, b.Driver, "docker")
			h.AssertEq(t, driverSupportsMultiPlatform(b.Driver), false)
		})

		it("treats current with empty Name (docker context) as the docker driver", func() {
			// This is the real-world failing case: `current` points at a docker
			// context, not an explicit buildx instance.
			writeCurrent(`{"Key":"desktop-linux","Name":"","Global":false}`)
			b, err := resolveCurrentBuildxBuilder()
			h.AssertNil(t, err)
			h.AssertEq(t, b.Name, "")
			h.AssertEq(t, b.Driver, "docker")
			h.AssertEq(t, driverSupportsMultiPlatform(b.Driver), false)
		})

		it("resolves an explicit docker-container instance selected in current", func() {
			writeInstance("pack-multiplatform", `{"Name":"pack-multiplatform","Driver":"docker-container","Nodes":[]}`)
			writeCurrent(`{"Key":"pack-multiplatform","Name":"pack-multiplatform","Global":false}`)
			b, err := resolveCurrentBuildxBuilder()
			h.AssertNil(t, err)
			h.AssertEq(t, b.Name, "pack-multiplatform")
			h.AssertEq(t, b.Driver, "docker-container")
			h.AssertEq(t, driverSupportsMultiPlatform(b.Driver), true)
		})

		it("honors BUILDX_BUILDER over current", func() {
			writeInstance("remote-builder", `{"Name":"remote-builder","Driver":"remote","Nodes":[]}`)
			writeCurrent(`{"Key":"desktop-linux","Name":"","Global":false}`) // would otherwise be docker
			h.AssertNil(t, os.Setenv("BUILDX_BUILDER", "remote-builder"))
			b, err := resolveCurrentBuildxBuilder()
			h.AssertNil(t, err)
			h.AssertEq(t, b.Name, "remote-builder")
			h.AssertEq(t, b.Driver, "remote")
			h.AssertEq(t, driverSupportsMultiPlatform(b.Driver), true)
		})

		it("errors when a selected instance file is missing", func() {
			writeCurrent(`{"Key":"ghost","Name":"ghost","Global":false}`)
			_, err := resolveCurrentBuildxBuilder()
			h.AssertError(t, err, "buildx builder \"ghost\" not found")
		})
	})

	when("driverSupportsMultiPlatform", func() {
		it("accepts container-backed drivers and rejects docker", func() {
			h.AssertEq(t, driverSupportsMultiPlatform("docker-container"), true)
			h.AssertEq(t, driverSupportsMultiPlatform("remote"), true)
			h.AssertEq(t, driverSupportsMultiPlatform("kubernetes"), true)
			h.AssertEq(t, driverSupportsMultiPlatform("docker"), false)
			h.AssertEq(t, driverSupportsMultiPlatform(""), false)
		})
	})
}
