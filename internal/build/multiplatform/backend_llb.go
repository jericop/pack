package multiplatform

import (
	"context"
	"fmt"

	"github.com/buildpacks/pack/pkg/logging"
)

// LLBBackend implements BuildBackend using the BuildKit Go SDK (LLB API) directly.
// This provides programmatic control over the build graph, streaming progress output,
// and eliminates the need for a Dockerfile intermediate or docker CLI subprocess.
//
// Currently this is a skeleton that falls back to the Dockerfile backend.
// The full implementation will:
//   - Connect to the buildkit daemon via the buildx library's builder resolution
//   - Construct an LLB graph with llb.Image, llb.Run, llb.AsPersistentCacheDir, etc.
//   - Solve via client.Solve with progress streaming back to pack's logger
//   - Support direct image export to registry or OCI layout
type LLBBackend struct {
	logger       logging.Logger
	buildkitOpts BuildkitOpts

	// fallback is used until the LLB implementation is complete
	fallback *DockerfileBackend
}

// NewLLBBackend creates a new LLB-based build backend.
// Until the full LLB implementation is complete, it delegates to the Dockerfile backend.
func NewLLBBackend(logger logging.Logger, buildkitOpts BuildkitOpts) *LLBBackend {
	return &LLBBackend{
		logger:       logger,
		buildkitOpts: buildkitOpts,
		fallback:     NewDockerfileBackend(logger, buildkitOpts),
	}
}

func (b *LLBBackend) Name() string {
	return "buildkit-llb"
}

func (b *LLBBackend) Capabilities() BackendCapabilities {
	// When fully implemented, LLB will support parallel arch builds
	// since we can issue multiple Solve calls concurrently.
	return BackendCapabilities{
		SupportsLLB:          true,
		SupportsCacheMounts:  true,
		SupportsParallelArch: true,
		SupportsOCILayout:    true,
		SupportsSecretMounts: true,
	}
}

// Build constructs an LLB graph for the lifecycle phases and solves it via the buildkit daemon.
//
// TODO: Implement the full LLB flow:
//  1. Connect to buildkit daemon (resolve builder from buildkitOpts.Builder or use default)
//  2. Construct LLB state:
//     - base := llb.Image(opts.BuilderImage, llb.Platform(spec))
//     - If lifecycle image specified: lifecycle := llb.Image(opts.LifecycleImage)
//       base = base.File(llb.Copy(lifecycle, "/cnb/lifecycle", "/cnb/lifecycle"))
//     - appSource := llb.Local("app-source")
//     - withApp := base.File(llb.Copy(appSource, "/", "/workspace"))
//     - For each phase: state = state.Run(llb.Args(phase.Command()),
//         llb.AsPersistentCacheDir(opts.CacheID, llb.CacheMountShared),
//         llb.AddSecretMount(...)
//       )
//  3. Marshal and solve:
//     - def, _ := finalState.Marshal(ctx, llb.Platform(spec))
//     - client.Solve(ctx, def, client.SolveOpt{Exports: [...]}, statusChan)
//  4. Stream progress from statusChan to pack's logger
//  5. Return the image reference from the solve result
func (b *LLBBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	// For now, fall back to the Dockerfile backend with a notice
	b.logger.Debugf("LLB backend not yet fully implemented for %s, using Dockerfile fallback", opts.Platform.String())
	return b.fallback.Build(ctx, opts)
}

// connectToBuildkit resolves and connects to the appropriate buildkit daemon.
// This will use the buildx library to find the active builder.
//
// TODO: Implement using github.com/docker/buildx/builder and github.com/moby/buildkit/client
func (b *LLBBackend) connectToBuildkit(ctx context.Context) error {
	return fmt.Errorf("LLB backend: buildkit connection not yet implemented")
}

// buildLLBGraph constructs the LLB state graph from the platform build options.
//
// TODO: Implement using github.com/moby/buildkit/client/llb
// The graph structure mirrors the generated Dockerfile:
//   FROM builder-image
//   COPY --from=lifecycle /cnb/lifecycle /cnb/lifecycle  (if lifecycle image specified)
//   RUN mkdir -p /cache && chown ...
//   COPY . /workspace
//   RUN --mount=type=cache --mount=type=secret  /cnb/lifecycle/analyzer && ... && /cnb/lifecycle/exporter
func (b *LLBBackend) buildLLBGraph(opts PlatformBuildOpts) error {
	return fmt.Errorf("LLB backend: graph construction not yet implemented")
}
