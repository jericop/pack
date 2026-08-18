package multiplatform

import (
	"context"
	"fmt"
	"strings"

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

	// Check if the target tag already exists and enforce the existing tag policy
	var previousDigest string
	if opts.Publish && opts.ManifestListName != "" {
		var err error
		previousDigest, err = e.checkExistingTag(ctx, opts)
		if err != nil {
			return nil, err
		}
	}

	results, err := e.buildAllPlatforms(ctx, opts)
	if err != nil {
		// If the build failed and we had a previous image, log it for manual recovery
		if previousDigest != "" {
			e.logger.Warnf("Build failed. The previous image at %s had digest %s",
				style.Symbol(opts.ManifestListName), previousDigest)
			e.logger.Warnf("To restore it: docker buildx imagetools create --tag %s %s@%s",
				opts.ManifestListName, opts.ManifestListName, previousDigest)
		}
		return nil, err
	}

	// The lifecycle exporter pushes per-arch images to arch-suffixed tags.
	// We assemble the manifest list from those tags, then clean up the temp tags.
	if opts.Publish && opts.ManifestListName != "" {
		e.logger.Info(style.Step("ASSEMBLING MANIFEST LIST"))

		// Build per-arch tag references
		perArchRefs := make([]PlatformBuildResult, len(opts.Platforms))
		for i, p := range opts.Platforms {
			perArchRefs[i] = PlatformBuildResult{
				Platform: p,
				ImageRef: fmt.Sprintf("%s-%s", opts.ManifestListName, p.Arch),
			}
		}

		if err := assembleManifestListViaDocker(ctx, opts.ManifestListName, perArchRefs, e.logger); err != nil {
			if previousDigest != "" {
				e.logger.Warnf("Manifest list assembly failed. Previous image digest: %s", previousDigest)
				e.logger.Warnf("To restore: docker buildx imagetools create --tag %s %s@%s",
					opts.ManifestListName, opts.ManifestListName, previousDigest)
			}
			return nil, fmt.Errorf("assembling manifest list: %w", err)
		}
		e.logger.Infof("Successfully created manifest list %s", style.Symbol(opts.ManifestListName))

		// Clean up per-arch tags (best effort — don't fail the build if cleanup fails)
		for _, ref := range perArchRefs {
			e.logger.Debugf("Cleaning up intermediate tag: %s", ref.ImageRef)
			// Note: most registries don't support tag deletion via docker CLI.
			// The tags will expire naturally (e.g., ttl.sh) or can be cleaned up
			// by registry garbage collection policies.
		}

		if previousDigest != "" {
			e.logger.Debugf("Previous image digest was: %s", previousDigest)
		}
	}

	return results, nil
}

// checkExistingTag verifies whether the target tag already exists and enforces the policy.
// Returns the existing digest (if any) for recovery purposes.
func (e *Executor) checkExistingTag(ctx context.Context, opts MultiPlatformBuildOpts) (string, error) {
	// Query the registry for the existing digest at this tag
	digest, err := getRemoteDigest(ctx, opts.ManifestListName, e.logger)
	if err != nil || digest == "" {
		// Tag doesn't exist or can't be reached — proceed normally
		return "", nil
	}

	policy := opts.ExistingTagPolicy
	if policy == "" {
		policy = ExistingTagWarn // default to warn
	}

	switch policy {
	case ExistingTagFail:
		return digest, fmt.Errorf(
			"tag %s already exists (digest: %s); use --existing-tag-policy=overwrite to replace it",
			style.Symbol(opts.ManifestListName), digest)
	case ExistingTagWarn:
		e.logger.Warnf("Tag %s already exists (digest: %s) and will be overwritten",
			style.Symbol(opts.ManifestListName), digest)
	case ExistingTagOverwrite:
		e.logger.Debugf("Tag %s exists (digest: %s), overwriting as requested",
			opts.ManifestListName, digest)
	}

	return digest, nil
}

// getRemoteDigest queries the registry for the digest of an image reference.
// Returns empty string if the image doesn't exist or can't be queried.
func getRemoteDigest(ctx context.Context, imageRef string, logger logging.Logger) (string, error) {
	output, err := runDockerCommandWithOutput(ctx, []string{"buildx", "imagetools", "inspect", imageRef, "--format", "{{.Digest}}"}, logger)
	if err != nil {
		// Image doesn't exist or registry is unreachable — not an error for our purposes
		return "", nil
	}
	digest := strings.TrimSpace(output)
	if strings.HasPrefix(digest, "sha256:") {
		return digest, nil
	}
	return "", nil
}

// buildAllPlatforms executes the build for all platforms, either via a single multi-platform
// invocation (preferred — buildkit handles parallelism internally) or sequentially.
func (e *Executor) buildAllPlatforms(ctx context.Context, opts MultiPlatformBuildOpts) ([]PlatformBuildResult, error) {
	caps := e.backend.Capabilities()

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

// assembleManifestList creates a manifest list from per-platform build results.
// Only needed for the sequential build path; single-invocation builds handle this internally.
func (e *Executor) assembleManifestList(ctx context.Context, opts MultiPlatformBuildOpts, results []PlatformBuildResult) error {
	if !opts.Publish {
		e.logger.Info("Manifest list assembly for local OCI layout is not yet implemented; per-arch images are available in the output directory")
		return nil
	}

	return assembleManifestListViaDocker(ctx, opts.ManifestListName, results, e.logger)
}

// assembleManifestListViaDocker creates a manifest list using `docker buildx imagetools create`.
func assembleManifestListViaDocker(ctx context.Context, manifestListName string, results []PlatformBuildResult, logger logging.Logger) error {
	sources := make([]string, 0, len(results))
	for _, r := range results {
		if r.ImageRef == "" {
			return fmt.Errorf("missing image reference for platform %s", r.Platform.String())
		}
		sources = append(sources, r.ImageRef)
	}

	logger.Debugf("Creating manifest list %s from: %v", manifestListName, sources)

	args := []string{"buildx", "imagetools", "create", "--tag", manifestListName}
	args = append(args, sources...)

	return runDockerCommand(ctx, args, logger)
}
