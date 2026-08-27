package multiplatform

import (
	"context"
	"fmt"
	"os"

	"github.com/buildpacks/pack/internal/style"
	"github.com/buildpacks/pack/pkg/logging"
)

// Executor orchestrates multi-platform builds by dispatching per-platform
// builds to a BuildBackend and then assembling the results into a manifest list.
type Executor struct {
	backend   BuildBackend
	assembler ManifestAssembler
	logger    logging.Logger
}

// MultiPlatformBuilder is an optional interface that backends can implement
// to handle all platforms in a single invocation (e.g., docker buildx build --platform a,b).
// When supported, buildkit builds all architectures in parallel internally and pushes
// the manifest list atomically — no intermediate per-arch tags are created.
type MultiPlatformBuilder interface {
	BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error)
}

// NewExecutor creates a new multi-platform build executor with the given backend
// and manifest assembler. The assembler uses pack's built-in manifest list support
// (imgutil + go-containerregistry) to create and push the final image index.
func NewExecutor(backend BuildBackend, assembler ManifestAssembler, logger logging.Logger) *Executor {
	return &Executor{
		backend:   backend,
		assembler: assembler,
		logger:    logger,
	}
}

// Execute runs the lifecycle for all specified platforms and assembles the results.
// If the backend supports it, a single multi-platform invocation is used and buildkit
// pushes the manifest list directly. Otherwise, builds run sequentially and the
// manifest list is assembled afterwards using pack's built-in manifest list functionality.
func (e *Executor) Execute(ctx context.Context, opts MultiPlatformBuildOpts) ([]PlatformBuildResult, error) {
	e.logger.Infof("Building for %d platform(s) using %s backend", len(opts.Platforms), e.backend.Name())

	for _, p := range opts.Platforms {
		e.logger.Infof("  - %s", p.String())
	}

	results, err := e.buildAllPlatforms(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Assemble and push the manifest list
	if opts.Publish && opts.ManifestListName != "" {
		e.logger.Info(style.Step("ASSEMBLING MANIFEST LIST"))

		if opts.ExportMode == ExportOCILayout {
			return nil, fmt.Errorf("oci-layout export mode is not yet fully implemented; use --buildkit-export-mode=registry")
		}

		// Registry mode: assemble from per-arch tags using pack's manifest list support
		perArchRefs := make([]string, len(opts.Platforms))
		for i, p := range opts.Platforms {
			perArchRefs[i] = fmt.Sprintf("%s-build-%s-%s", opts.ManifestListName, opts.BuildOpts.BuildID, p.Arch)
		}

		if err := e.assembler.AssembleAndPushManifest(ctx, opts.ManifestListName, perArchRefs); err != nil {
			return nil, fmt.Errorf("assembling manifest list: %w", err)
		}

		e.logger.Infof("Successfully created manifest list %s", style.Symbol(opts.ManifestListName))
	}

	return results, nil
}

// buildAllPlatforms executes the build for all platforms, either via a single multi-platform
// invocation (preferred — buildkit handles parallelism internally) or sequentially.
func (e *Executor) buildAllPlatforms(ctx context.Context, opts MultiPlatformBuildOpts) ([]PlatformBuildResult, error) {
	caps := e.backend.Capabilities()

	// For OCI layout mode, set up a temp output directory
	if opts.ExportMode == ExportOCILayout {
		if opts.BuildOpts.OutputDir == "" {
			tmpDir, err := os.MkdirTemp("", "pack-oci-layout-*")
			if err != nil {
				return nil, fmt.Errorf("creating temp output directory: %w", err)
			}
			opts.BuildOpts.OutputDir = tmpDir
			// Note: cleanup is handled by the caller after push
		}
	}

	// Prefer single-invocation multi-platform build — buildkit builds all arches
	// in parallel and pushes the manifest list atomically.
	if caps.SupportsParallelArch && len(opts.Platforms) > 1 {
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
	} else {
		buildOpts.OutputDir = fmt.Sprintf("%s/%s/%s", opts.BuildOpts.OutputDir, platform.OS, platform.Arch)
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
