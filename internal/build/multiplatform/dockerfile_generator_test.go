package multiplatform_test

import (
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	mp "github.com/buildpacks/pack/internal/build/multiplatform"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestDockerfileGenerator(t *testing.T) {
	spec.Run(t, "dockerfile_generator", testDockerfileGenerator, spec.Report(report.Terminal{}))
}

func testDockerfileGenerator(t *testing.T, when spec.G, it spec.S) {
	when("#GenerateDockerfileMultiPlatform", func() {
		var opts mp.PlatformBuildOpts

		it.Before(func() {
			opts = mp.PlatformBuildOpts{
				BuilderImage: "paketobuildpacks/builder:latest",
				RunImage:     "paketobuildpacks/run:latest",
				ImageName:    "registry.example.com/myapp:latest",
				CacheID:      "pack-cache-myapp",
				BuildID:      "abc12345",
				BuilderUID:   1001,
				BuilderGID:   1001,
				PlatformAPI:  "0.15",
				Phases: []mp.PhaseCommand{
					{Name: "analyzer", Binary: "/cnb/lifecycle/analyzer", Args: []string{"-run-image", "run:latest", "registry.example.com/myapp:latest"}, NeedsRegistryAuth: true},
					{Name: "detector", Binary: "/cnb/lifecycle/detector", Args: []string{"-app", "/workspace"}},
					{Name: "restorer", Binary: "/cnb/lifecycle/restorer", Args: []string{"-cache-dir", "/cache"}, NeedsCache: true},
					{Name: "builder", Binary: "/cnb/lifecycle/builder", Args: []string{"-app", "/workspace"}},
					{Name: "exporter", Binary: "/cnb/lifecycle/exporter", Args: []string{"-app", "/workspace", "-cache-dir", "/cache", "registry.example.com/myapp:latest"}, NeedsRegistryAuth: true, NeedsCache: true},
				},
			}
		})

		it("includes syntax directive", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "# syntax=docker/dockerfile:1")
		})

		it("references the builder image", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "FROM paketobuildpacks/builder:latest")
		})

		it("includes TARGETARCH ARG", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "ARG TARGETARCH")
		})

		it("sets CNB_PLATFORM_API", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "ENV CNB_PLATFORM_API=0.15")
		})

		it("sets correct USER", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "USER 1001:1001")
		})

		it("copies app source with correct ownership", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "COPY --chown=1001:1001 . /workspace")
		})

		it("uses per-arch tag with build ID in image references", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "registry.example.com/myapp:latest-build-abc12345-${TARGETARCH}")
		})

		it("includes cache mount with TARGETARCH scoping", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			h.AssertContains(t, df, "--mount=type=cache,id=pack-cache-myapp-${TARGETARCH},target=/cache,uid=1001,gid=1001")
		})

		it("does not emit any secret mounts (auth is provided via the CNB_REGISTRY_AUTH env var)", func() {
			// The generator intentionally moved off `--mount=type=secret,id=docker-config`
			// to the CNB_REGISTRY_AUTH env var (see generateLifecycleRunMultiPlatform:
			// `secretMount = ""`). The LLB and Dockerfile backends both provide
			// registry auth via CNB_REGISTRY_AUTH now, so NO secret-mount lines
			// should ever be emitted. This asserts the current intended behavior.
			opts.RegistryAuth = "eyJhdXRocyI6e319" // base64-ish auth blob; enables the ENV assertion below
			df := mp.GenerateDockerfileMultiPlatform(opts)

			// No secret mount should appear anywhere in the generated Dockerfile.
			h.AssertNotContains(t, df, "type=secret")
			count := 0
			for _, line := range splitLines(df) {
				if contains(line, "mount=type=secret") {
					count++
				}
			}
			h.AssertEq(t, count, 0)

			// Auth is instead supplied through the env var when RegistryAuth is set.
			h.AssertContains(t, df, "ENV CNB_REGISTRY_AUTH='eyJhdXRocyI6e319'")
		})

		it("does NOT include secret mount on detector/restorer/builder", func() {
			df := mp.GenerateDockerfileMultiPlatform(opts)
			lines := splitLines(df)
			for _, line := range lines {
				if contains(line, "/cnb/lifecycle/detector") || contains(line, "/cnb/lifecycle/builder") {
					h.AssertNotContains(t, line, "secret")
				}
			}
		})

		when("ClearCache is true", func() {
			it.Before(func() {
				opts.ClearCache = true
			})

			it("omits cache mount", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				h.AssertNotContains(t, df, "--mount=type=cache")
			})

			it("removes -cache-dir from restorer and exporter", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				h.AssertNotContains(t, df, "-cache-dir")
			})
		})

		when("OrderToml is provided", func() {
			it.Before(func() {
				opts.OrderToml = "[[order]]\n  [[order.group]]\n    id = \"my-bp\"\n    version = \"1.0\"\n"
			})

			it("writes order.toml", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				h.AssertContains(t, df, "cat > /cnb/order.toml")
				h.AssertContains(t, df, "id = \"my-bp\"")
			})
		})

		when("LifecycleImage is specified (non-local)", func() {
			it.Before(func() {
				opts.LifecycleImage = "buildpacksio/lifecycle:0.19"
			})

			it("adds lifecycle FROM stage", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				h.AssertContains(t, df, "FROM buildpacksio/lifecycle:0.19 AS lifecycle")
				h.AssertContains(t, df, "COPY --from=lifecycle /cnb/lifecycle /cnb/lifecycle")
			})
		})

		when("LifecycleImage is pack.local", func() {
			it.Before(func() {
				opts.LifecycleImage = "pack.local/builder/abc123:latest"
			})

			it("skips lifecycle FROM stage", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				h.AssertNotContains(t, df, "FROM pack.local")
				h.AssertNotContains(t, df, "COPY --from=lifecycle")
			})
		})

		when("BuildpackImages are specified", func() {
			it.Before(func() {
				opts.BuildpackImages = []string{"docker.io/paketo-buildpacks/go:4.19"}
			})

			it("adds buildpack FROM stages", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				h.AssertContains(t, df, "FROM docker.io/paketo-buildpacks/go:4.19 AS buildpack-0")
				h.AssertContains(t, df, "COPY --from=buildpack-0 /cnb/buildpacks /cnb/buildpacks")
			})
		})

		when("ExportMode is oci-layout", func() {
			it.Before(func() {
				opts.ExportMode = mp.ExportOCILayout
			})

			it("sets CNB_EXPERIMENTAL_MODE", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				h.AssertContains(t, df, "ENV CNB_EXPERIMENTAL_MODE=warn")
			})

			it("does not include secret mount on exporter", func() {
				df := mp.GenerateDockerfileMultiPlatform(opts)
				lines := splitLines(df)
				for _, line := range lines {
					if contains(line, "/cnb/lifecycle/exporter") {
						h.AssertNotContains(t, line, "secret")
					}
				}
			})
		})
	})
}

func splitLines(s string) []string {
	result := []string{}
	current := ""
	for _, c := range s {
		if c == '\n' {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
