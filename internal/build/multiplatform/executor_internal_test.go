package multiplatform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestExecutorInternal(t *testing.T) {
	spec.Run(t, "executor_internal", testExecutorInternal, spec.Report(report.Terminal{}))
}

// fakeBackend is a stub BuildBackend used to drive the executor's manifest
// assembly decision without a live BuildKit daemon or docker binary. It records
// whether BuildMultiPlatform ran so tests can assert the build happened, and its
// capabilities control the executor's skip-vs-assemble branch.
type fakeBackend struct {
	name          string
	caps          BackendCapabilities
	buildCalled   bool
	multiPlatform bool
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Capabilities() BackendCapabilities { return f.caps }

func (f *fakeBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	f.buildCalled = true
	return PlatformBuildResult{Platform: opts.Platform, ImageRef: opts.ImageName}, nil
}

// nativeMultiBackend additionally implements MultiPlatformBuilder so the executor
// takes the single-invocation multi-platform path. It records the call and
// returns per-platform results without any real work.
type nativeMultiBackend struct {
	fakeBackend
}

func (f *nativeMultiBackend) BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	f.multiPlatform = true
	results := make([]PlatformBuildResult, len(platforms))
	for i, p := range platforms {
		results[i] = PlatformBuildResult{Platform: p, ImageRef: opts.ImageName}
	}
	return results, nil
}

// outputDirCapturingBackend records the OutputDir the executor passed down and
// can optionally return an error, so cleanup tests (FR-6/NFR-3) can (a) learn the
// exact temp dir the executor created and (b) exercise the failure path. It is a
// single-arch (Build-only) backend so the executor takes the sequential path.
type outputDirCapturingBackend struct {
	fakeBackend
	capturedOutputDir string
	buildErr          error
}

func (f *outputDirCapturingBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	f.buildCalled = true
	f.capturedOutputDir = opts.OutputDir
	if f.buildErr != nil {
		return PlatformBuildResult{}, f.buildErr
	}
	return PlatformBuildResult{Platform: opts.Platform, ImageRef: opts.ImageName}, nil
}

func testExecutorInternal(t *testing.T, when spec.G, it spec.S) {
	var (
		outBuf    bytes.Buffer
		logger    logging.Logger
		platforms []Platform
	)

	it.Before(func() {
		outBuf.Reset()
		logger = logging.NewLogWithWriters(&outBuf, &outBuf)
		platforms = []Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
		}
	})

	when("#skipManifestAssembly", func() {
		it("returns true when the backend pushes natively", func() {
			h.AssertEq(t, skipManifestAssembly(BackendCapabilities{PushesNatively: true}), true)
		})

		it("returns false when the backend does not push natively", func() {
			// The Dockerfile MVP path: executor assembles via imagetools.
			h.AssertEq(t, skipManifestAssembly(BackendCapabilities{PushesNatively: false}), false)
		})

		it("ignores other capabilities (only PushesNatively drives the decision)", func() {
			caps := BackendCapabilities{
				SupportsLLB:          true,
				SupportsOCILayout:    true,
				SupportsParallelArch: true,
				PushesNatively:       false,
			}
			h.AssertEq(t, skipManifestAssembly(caps), false)
		})
	})

	when("#Execute manifest assembly decision", func() {
		when("backend pushes natively (LLB OCI layout mode)", func() {
			it("does not error and skips executor-side assembly for OCI layout mode", func() {
				backend := &nativeMultiBackend{fakeBackend: fakeBackend{
					name: "buildkit-llb",
					caps: BackendCapabilities{SupportsParallelArch: true, PushesNatively: true},
				}}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        platforms,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				results, err := e.Execute(context.Background(), opts)
				// No "not yet implemented" error for the native LLB OCI layout path.
				h.AssertNil(t, err)
				h.AssertEq(t, len(results), len(platforms))
				// The backend performed the multi-platform build (and thus the push).
				h.AssertEq(t, backend.multiPlatform, true)
				// The executor logged that the backend pushed natively.
				h.AssertContains(t, outBuf.String(), "pushed natively by the buildkit-llb backend")
			})

			it("no longer returns the 'not yet implemented' oci-layout error", func() {
				backend := &nativeMultiBackend{fakeBackend: fakeBackend{
					name: "buildkit-llb",
					caps: BackendCapabilities{SupportsParallelArch: true, PushesNatively: true},
				}}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        platforms,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				_, err := e.Execute(context.Background(), opts)
				h.AssertNil(t, err)
				h.AssertEq(t, strings.Contains(outBuf.String(), "not yet"), false)
			})

			it("skips assembly for a native single-arch OCI layout build", func() {
				backend := &fakeBackend{
					name: "buildkit-llb",
					caps: BackendCapabilities{PushesNatively: true},
				}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        []Platform{{OS: "linux", Arch: "amd64"}},
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				results, err := e.Execute(context.Background(), opts)
				h.AssertNil(t, err)
				h.AssertEq(t, len(results), 1)
				h.AssertContains(t, outBuf.String(), "pushed natively by the buildkit-llb backend")
			})
		})

		when("backend does not push natively with OCI layout mode (unreachable via factory)", func() {
			it("returns a clear error rather than mis-assembling", func() {
				// A non-native backend must NEVER hit the registry-mode imagetools
				// assembly for oci-layout mode (there are no per-arch tags to
				// assemble). Guarded with a clear error.
				backend := &nativeMultiBackend{fakeBackend: fakeBackend{
					name: "buildkit-dockerfile",
					caps: BackendCapabilities{SupportsParallelArch: true, PushesNatively: false},
				}}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        platforms,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				_, err := e.Execute(context.Background(), opts)
				h.AssertNotNil(t, err)
				h.AssertError(t, err, "requires a backend that pushes natively")
			})
		})

		when("not publishing", func() {
			it("does not attempt any manifest assembly regardless of capabilities", func() {
				backend := &nativeMultiBackend{fakeBackend: fakeBackend{
					name: "buildkit-dockerfile",
					caps: BackendCapabilities{SupportsParallelArch: true, PushesNatively: false},
				}}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        platforms,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          false,
					ExportMode:       ExportRegistry,
				}

				results, err := e.Execute(context.Background(), opts)
				h.AssertNil(t, err)
				h.AssertEq(t, len(results), len(platforms))
				// Registry-mode assembly (imagetools) is not triggered; nothing
				// about assembling a manifest list should be logged.
				h.AssertEq(t, strings.Contains(outBuf.String(), "ASSEMBLING MANIFEST LIST"), false)
			})
		})
	})

	when("backend capability defaults", func() {
		it("LLB backend reports PushesNatively=true", func() {
			b := NewLLBBackend(logger, BuildkitOpts{})
			h.AssertEq(t, b.Capabilities().PushesNatively, true)
		})

		it("Dockerfile backend reports PushesNatively=false", func() {
			b := NewDockerfileBackend(logger, BuildkitOpts{})
			h.AssertEq(t, b.Capabilities().PushesNatively, false)
		})
	})

	// Guard against accidental drift: the fakes satisfy the interfaces the
	// executor relies on.
	when("fakes satisfy interfaces", func() {
		it("fakeBackend is a BuildBackend and nativeMultiBackend is a MultiPlatformBuilder", func() {
			var _ BuildBackend = &fakeBackend{}
			var _ BuildBackend = &nativeMultiBackend{}
			var _ MultiPlatformBuilder = &nativeMultiBackend{}
		})
	})

	// FR-6 / NFR-3: the executor cleans up the temp per-arch content store
	// directory it creates, promptly and even on failure — but never a
	// caller-supplied OutputDir.
	when("#Execute temp OCI layout output cleanup", func() {
		singleArch := []Platform{{OS: "linux", Arch: "amd64"}}

		when("the executor creates the temp OutputDir (OutputDir empty + OCI layout mode)", func() {
			it("removes the created temp dir after a successful build", func() {
				backend := &outputDirCapturingBackend{fakeBackend: fakeBackend{
					name: "buildkit-llb",
					caps: BackendCapabilities{PushesNatively: true},
				}}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        singleArch,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				_, err := e.Execute(context.Background(), opts)
				h.AssertNil(t, err)

				// The executor allocated a temp dir and passed it to the backend.
				h.AssertNotEq(t, backend.capturedOutputDir, "")
				h.AssertContains(t, backend.capturedOutputDir, "pack-oci-layout-")
				// It must be removed after Execute returns.
				assertPathDoesNotExist(t, backend.capturedOutputDir)
			})

			it("removes the created temp dir even when the backend returns an error", func() {
				backend := &outputDirCapturingBackend{
					fakeBackend: fakeBackend{
						name: "buildkit-llb",
						caps: BackendCapabilities{PushesNatively: true},
					},
					buildErr: errors.New("boom: backend failed"),
				}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        singleArch,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				_, err := e.Execute(context.Background(), opts)
				// The build failed...
				h.AssertNotNil(t, err)
				// ...but the executor-created temp dir is still cleaned up (deferred).
				h.AssertNotEq(t, backend.capturedOutputDir, "")
				h.AssertContains(t, backend.capturedOutputDir, "pack-oci-layout-")
				assertPathDoesNotExist(t, backend.capturedOutputDir)
			})
		})

		when("the caller supplies OutputDir", func() {
			it("does NOT remove the caller-supplied OutputDir", func() {
				userDir := t.TempDir()

				backend := &outputDirCapturingBackend{fakeBackend: fakeBackend{
					name: "buildkit-llb",
					caps: BackendCapabilities{PushesNatively: true},
				}}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        singleArch,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123", OutputDir: userDir},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				_, err := e.Execute(context.Background(), opts)
				h.AssertNil(t, err)

				// The backend saw the user's dir unchanged...
				h.AssertEq(t, backend.capturedOutputDir, userDir)
				// ...and it still exists (the executor must not touch it).
				assertPathExists(t, userDir)
			})

			it("does NOT remove the caller-supplied OutputDir even on failure", func() {
				userDir := t.TempDir()

				backend := &outputDirCapturingBackend{
					fakeBackend: fakeBackend{
						name: "buildkit-llb",
						caps: BackendCapabilities{PushesNatively: true},
					},
					buildErr: errors.New("boom: backend failed"),
				}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        singleArch,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123", OutputDir: userDir},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          true,
					ExportMode:       ExportOCILayout,
				}

				_, err := e.Execute(context.Background(), opts)
				h.AssertNotNil(t, err)
				assertPathExists(t, userDir)
			})
		})

		when("registry export mode", func() {
			it("creates no temp dir and removes nothing", func() {
				backend := &outputDirCapturingBackend{fakeBackend: fakeBackend{
					name: "buildkit-dockerfile",
					caps: BackendCapabilities{},
				}}
				e := NewExecutor(backend, logger)

				opts := MultiPlatformBuildOpts{
					Platforms:        singleArch,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/myapp:latest", BuildID: "abc123"},
					Logger:           logger,
					ManifestListName: "registry.example.com/myapp:latest",
					Publish:          false,
					ExportMode:       ExportRegistry,
				}

				_, err := e.Execute(context.Background(), opts)
				h.AssertNil(t, err)
				// No pack-oci-layout temp dir was allocated for registry mode
				// (the executor only creates one in OCI layout mode). Registry
				// mode may still derive a per-platform OutputDir suffix, so we
				// assert on the absence of the temp-dir prefix rather than empty.
				h.AssertEq(t, strings.Contains(backend.capturedOutputDir, "pack-oci-layout-"), false)
			})
		})
	})

	when("#ensureOCILayoutOutputDir", func() {
		it("creates a temp dir when OCI layout mode and OutputDir empty", func() {
			e := NewExecutor(&fakeBackend{name: "buildkit-llb"}, logger)
			opts := MultiPlatformBuildOpts{ExportMode: ExportOCILayout}

			dir, created, err := e.ensureOCILayoutOutputDir(&opts)
			h.AssertNil(t, err)
			h.AssertEq(t, created, true)
			h.AssertNotEq(t, dir, "")
			h.AssertEq(t, opts.BuildOpts.OutputDir, dir)
			assertPathExists(t, dir)
			// Clean up the dir this unit test created directly.
			_ = os.RemoveAll(dir)
		})

		it("does not create a dir when the caller supplied OutputDir", func() {
			userDir := t.TempDir()
			e := NewExecutor(&fakeBackend{name: "buildkit-llb"}, logger)
			opts := MultiPlatformBuildOpts{
				ExportMode: ExportOCILayout,
				BuildOpts:  PlatformBuildOpts{OutputDir: userDir},
			}

			dir, created, err := e.ensureOCILayoutOutputDir(&opts)
			h.AssertNil(t, err)
			h.AssertEq(t, created, false)
			h.AssertEq(t, dir, "")
			h.AssertEq(t, opts.BuildOpts.OutputDir, userDir)
		})

		it("does not create a dir when not OCI layout mode", func() {
			e := NewExecutor(&fakeBackend{name: "buildkit-dockerfile"}, logger)
			opts := MultiPlatformBuildOpts{ExportMode: ExportRegistry}

			dir, created, err := e.ensureOCILayoutOutputDir(&opts)
			h.AssertNil(t, err)
			h.AssertEq(t, created, false)
			h.AssertEq(t, dir, "")
			h.AssertEq(t, opts.BuildOpts.OutputDir, "")
		})
	})
}

// assertPathExists fails the test if path does not exist.
func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected path %q to exist, but stat failed: %s", path, err)
	}
}

// assertPathDoesNotExist fails the test if path still exists.
func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected path %q to have been removed, but it still exists", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("expected path %q to be removed (IsNotExist), got stat error: %s", path, err)
	}
}
