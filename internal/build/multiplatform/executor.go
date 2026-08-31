package multiplatform

import (
	"context"
	"fmt"

	"github.com/buildpacks/pack/internal/style"
	"github.com/buildpacks/pack/pkg/logging"
)

// Executor orchestrates multi-platform builds by dispatching per-platform
// builds to a BuildBackend and then assembling the results into a manifest list.
type Executor struct {
	backend BuildBackend
	logger  logging.Logger
}

// MultiPlatformBuilder is an optional interface that backends can implement
// to handle all platforms in a single invocation (e.g., docker buildx build --platform a,b).
// When supported, buildkit builds all architectures in parallel internally and pushes
// the manifest list atomically — no intermediate per-arch tags are created.
type MultiPlatformBuilder interface {
	BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error)
}

// NewExecutor creates a new multi-platform build executor with the given backend.
func NewExecutor(backend BuildBackend, logger logging.Logger) *Executor {
	return &Executor{
		backend: backend,
		logger:  logger,
	}
}

// Execute runs the lifecycle for all specified platforms and assembles the results.
// If the backend supports it, a single multi-platform invocation is used and buildkit
// pushes the manifest list directly. Otherwise, builds run sequentially and the
// manifest list is assembled afterwards.
func (e *Executor) Execute(ctx context.Context, opts MultiPlatformBuildOpts) ([]PlatformBuildResult, error) {
	e.logger.Infof("Building for %d platform(s) using %s backend", len(opts.Platforms), e.backend.Name())

	for _, p := range opts.Platforms {
		e.logger.Infof("  - %s", p.String())
	}

	results, err := e.buildAllPlatforms(ctx, opts)
	if err != nil {
		return nil, err
	}

	// The buildkit backend assembles and pushes the final image / manifest list
	// itself via BuildKit's native multi-platform image export (one image/index,
	// no intermediate tags). The executor MUST NOT assemble or push again.
	if opts.Publish && opts.ManifestListName != "" {
		if skipManifestAssembly(e.backend.Capabilities()) {
			e.logger.Infof("Manifest list pushed natively by the %s backend", e.backend.Name())
			return results, nil
		}
		return nil, fmt.Errorf("the %s backend does not push natively and no executor-side assembly path exists", e.backend.Name())
	}

	return results, nil
}

// skipManifestAssembly reports whether the executor should skip its own
// manifest-list assembly / push because the backend already performed the final
// registry push itself (FR-5, Task 5).
//
// It is a pure function of the backend capabilities so the "skip vs assemble"
// branch decision is unit-testable without shelling out to docker. When it
// returns true the executor leaves assembly to the backend; when false the
// executor runs the registry-mode imagetools assembly (Dockerfile MVP path).
func skipManifestAssembly(caps BackendCapabilities) bool {
	return caps.PushesNatively
}

// buildAllPlatforms executes the build for all platforms via a single
// multi-platform invocation (buildkit builds all arches in parallel and pushes
// the manifest list atomically). Single-platform builds are also routed through
// BuildMultiPlatform so the image is pushed under the FINAL name (buildkit
// produces a single-arch index at the final tag).
func (e *Executor) buildAllPlatforms(ctx context.Context, opts MultiPlatformBuildOpts) ([]PlatformBuildResult, error) {
	caps := e.backend.Capabilities()
	if caps.SupportsParallelArch {
		if mp, ok := e.backend.(MultiPlatformBuilder); ok {
			return mp.BuildMultiPlatform(ctx, opts.Platforms, e.platformOptsForMulti(opts))
		}
	}
	return e.buildSequential(ctx, opts)
}

// buildSequential builds each platform one at a time (fallback for backends that
// don't support single-invocation multi-platform).
func (e *Executor) buildSequential(ctx context.Context, opts MultiPlatformBuildOpts) ([]PlatformBuildResult, error) {
	results := make([]PlatformBuildResult, 0, len(opts.Platforms))

	for _, platform := range opts.Platforms {
		e.logger.Infof(style.Step("BUILDING FOR %s"), platform.String())

		platformOpts := e.platformOptsFor(opts, platform)
		result, err := e.backend.Build(ctx, platformOpts)
		if err != nil {
			return nil, fmt.Errorf("building for %s: %w", platform.String(), err)
		}

		e.logger.Infof("Successfully built image for %s: %s", platform.String(), style.Symbol(result.ImageRef))
		results = append(results, result)
	}

	return results, nil
}

// platformOptsFor produces the per-platform build opts from the multi-platform config.
// Used for sequential builds where each platform gets its own invocation.
func (e *Executor) platformOptsFor(opts MultiPlatformBuildOpts, platform Platform) PlatformBuildOpts {
	buildOpts := opts.BuildOpts
	buildOpts.Platform = platform
	buildOpts.CacheID = fmt.Sprintf("%s-%s", opts.BuildOpts.CacheID, platform.Arch)

	if opts.Publish {
		buildOpts.ImageName = fmt.Sprintf("%s-%s", opts.BuildOpts.ImageName, platform.Arch)
	}

	return buildOpts
}

// platformOptsForMulti produces build opts for multi-platform single-invocation builds.
// The image name is used as-is — buildkit creates the manifest list at this tag directly.
func (e *Executor) platformOptsForMulti(opts MultiPlatformBuildOpts) PlatformBuildOpts {
	buildOpts := opts.BuildOpts
	buildOpts.ImageName = opts.ManifestListName
	return buildOpts
}


