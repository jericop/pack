package multiplatform

import (
	"bytes"
	"context"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestExecutorInternal(t *testing.T) {
	spec.Run(t, "executor_internal", testExecutorInternal, spec.Report(report.Terminal{}))
}

// fakeBackend is a stub BuildBackend used to drive the executor without a live
// BuildKit daemon. Its capabilities control the executor's skip-vs-assemble
// branch; it records whether Build ran.
type fakeBackend struct {
	name        string
	caps        BackendCapabilities
	buildCalled bool
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Capabilities() BackendCapabilities { return f.caps }

func (f *fakeBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	f.buildCalled = true
	return PlatformBuildResult{Platform: opts.Platform, ImageRef: opts.ImageName}, nil
}

// nativeMultiBackend additionally implements MultiPlatformBuilder so the executor
// takes the single-invocation multi-platform path (the buildkit backend's path).
type nativeMultiBackend struct {
	fakeBackend
	multiPlatform bool
}

func (f *nativeMultiBackend) BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	f.multiPlatform = true
	results := make([]PlatformBuildResult, len(platforms))
	for i, p := range platforms {
		results[i] = PlatformBuildResult{Platform: p, ImageRef: opts.ImageName}
	}
	return results, nil
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
			h.AssertEq(t, skipManifestAssembly(BackendCapabilities{PushesNatively: false}), false)
		})

		it("ignores other capabilities (only PushesNatively drives the decision)", func() {
			caps := BackendCapabilities{
				SupportsParallelArch: true,
				PushesNatively:       false,
			}
			h.AssertEq(t, skipManifestAssembly(caps), false)
		})
	})

	when("#Execute", func() {
		when("the backend pushes natively", func() {
			it("builds via the multi-platform path and skips executor-side assembly", func() {
				backend := &nativeMultiBackend{fakeBackend: fakeBackend{
					name: "buildkit",
					caps: BackendCapabilities{SupportsParallelArch: true, PushesNatively: true},
				}}
				e := NewExecutor(backend, logger)
				results, err := e.Execute(context.Background(), MultiPlatformBuildOpts{
					Platforms:        platforms,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/app:latest"},
					Logger:           logger,
					ManifestListName: "registry.example.com/app:latest",
					Publish:          true,
				})
				h.AssertNil(t, err)
				h.AssertEq(t, backend.multiPlatform, true)
				h.AssertEq(t, len(results), len(platforms))
			})
		})

		when("not publishing", func() {
			it("does not attempt any manifest assembly", func() {
				backend := &nativeMultiBackend{fakeBackend: fakeBackend{
					name: "buildkit",
					caps: BackendCapabilities{SupportsParallelArch: true, PushesNatively: true},
				}}
				e := NewExecutor(backend, logger)
				results, err := e.Execute(context.Background(), MultiPlatformBuildOpts{
					Platforms:        platforms,
					BuildOpts:        PlatformBuildOpts{ImageName: "app:latest"},
					Logger:           logger,
					ManifestListName: "app:latest",
					Publish:          false,
				})
				h.AssertNil(t, err)
				h.AssertEq(t, len(results), len(platforms))
			})
		})

		when("the backend does not push natively", func() {
			it("returns a clear error rather than mis-assembling", func() {
				backend := &nativeMultiBackend{fakeBackend: fakeBackend{
					name: "future-backend",
					caps: BackendCapabilities{SupportsParallelArch: true, PushesNatively: false},
				}}
				e := NewExecutor(backend, logger)
				_, err := e.Execute(context.Background(), MultiPlatformBuildOpts{
					Platforms:        platforms,
					BuildOpts:        PlatformBuildOpts{ImageName: "registry.example.com/app:latest"},
					Logger:           logger,
					ManifestListName: "registry.example.com/app:latest",
					Publish:          true,
				})
				h.AssertError(t, err, "does not push natively")
			})
		})
	})

	when("fakes satisfy interfaces", func() {
		it("fakeBackend is a BuildBackend and nativeMultiBackend is a MultiPlatformBuilder", func() {
			var _ BuildBackend = &fakeBackend{}
			var _ BuildBackend = &nativeMultiBackend{}
			var _ MultiPlatformBuilder = &nativeMultiBackend{}
		})
	})
}
