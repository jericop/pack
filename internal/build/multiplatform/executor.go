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

	// FR-6 / NFR-3: cleanup of the temporary per-arch content stores/directories.
	//
	// In OCI layout mode the per-arch content stores live UNDER opts.BuildOpts.OutputDir
	// (see backend_llb.go perArchStoreDir). When the caller did not supply an
	// OutputDir, the executor allocates a throwaway temp dir here as the base for
	// those stores; that temp dir is owned by the executor and MUST be released
	// promptly after the build (even on push failure). A caller/user-supplied
	// OutputDir is the user's requested artifact and MUST NOT be removed.
	//
	// ensureOCILayoutOutputDir returns whether it created the dir; we defer the
	// removal so it runs on every return path (success or error). Only the
	// executor-created temp dir is removed — never a caller-supplied one.
	tmpOutputDir, createdTmpOutputDir, err := e.ensureOCILayoutOutputDir(&opts)
	if err != nil {
		return nil, err
	}
	if createdTmpOutputDir {
		defer func() {
			e.logger.Debugf("Cleaning up temporary OCI layout output directory %s", tmpOutputDir)
			if rmErr := os.RemoveAll(tmpOutputDir); rmErr != nil {
				// Cleanup is best-effort: a failure to remove the temp dir must
				// not fail the build. Log at debug level (FR-6/NFR-3).
				e.logger.Debugf("Failed to remove temporary OCI layout output directory %s: %s", tmpOutputDir, rmErr)
			}
		}()
	}

	results, err := e.buildAllPlatforms(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Assemble and push the manifest list
	if opts.Publish && opts.ManifestListName != "" {
		caps := e.backend.Capabilities()

		// If the backend pushes natively, it has ALREADY assembled and pushed the
		// final image / manifest list itself (LLB OCI layout mode: native
		// ExporterImage push for single-arch, or assembleAndPushManifestList for
		// multi-arch — both with no intermediate tags, FR-5). The executor MUST
		// NOT assemble again, MUST NOT call PushOCILayoutAsManifestList, and MUST
		// NOT error. The decision is driven by the capability flag, not the export
		// mode, so the Dockerfile backend is unaffected.
		if skipManifestAssembly(caps) {
			e.logger.Infof("Manifest list pushed natively by the %s backend", e.backend.Name())
			return results, nil
		}

		// Non-native backend with OCI layout mode has no working executor-side
		// assembly path (only the LLB backend produces OCI layouts, and it pushes
		// natively). Fail clearly rather than silently mis-assembling from
		// per-arch tags that were never pushed. The factory only selects the LLB
		// backend for OCI layout mode, so this path is not reachable today; it
		// guards against a future non-native backend being wired to OCI layout.
		if opts.ExportMode == ExportOCILayout {
			return nil, fmt.Errorf("oci-layout export mode requires a backend that pushes natively; the %s backend does not", e.backend.Name())
		}

		e.logger.Info(style.Step("ASSEMBLING MANIFEST LIST"))

		// Registry mode: assemble from per-arch tags using imagetools
		perArchRefs := make([]PlatformBuildResult, len(opts.Platforms))
		for i, p := range opts.Platforms {
			perArchRefs[i] = PlatformBuildResult{
				Platform: p,
				ImageRef: fmt.Sprintf("%s-build-%s-%s", opts.ManifestListName, opts.BuildOpts.BuildID, p.Arch),
			}
		}

		if err := assembleManifestListViaDocker(ctx, opts.ManifestListName, perArchRefs, e.logger); err != nil {
			return nil, fmt.Errorf("assembling manifest list: %w", err)
		}

		e.logger.Infof("Successfully created manifest list %s", style.Symbol(opts.ManifestListName))
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

// ensureOCILayoutOutputDir makes sure opts.BuildOpts.OutputDir points at a
// directory that can hold the per-arch OCI layout content stores when building
// in OCI layout mode.
//
// Ownership rule (FR-6 / NFR-3): the executor cleans up ONLY the temp dir it
// itself creates. So this helper distinguishes two cases:
//
//   - Caller supplied an OutputDir (or mode is not OCI layout): nothing is
//     created. It returns created=false, and Execute must NOT remove it — a
//     user-supplied OutputDir (e.g. an OCI-layout-without-publish build writing
//     the user's artifact) is theirs to keep.
//   - OCI layout mode with an empty OutputDir (publish path / temp fallback):
//     it allocates a throwaway temp dir via os.MkdirTemp, sets it on opts, and
//     returns created=true so Execute defers os.RemoveAll on exactly that path.
//
// It mutates the passed opts (via pointer) so the created dir flows through to
// buildAllPlatforms and the backend, and returns the created path so the caller
// can defer its removal at the ownership boundary.
func (e *Executor) ensureOCILayoutOutputDir(opts *MultiPlatformBuildOpts) (dir string, created bool, err error) {
	if opts.ExportMode != ExportOCILayout {
		return "", false, nil
	}
	if opts.BuildOpts.OutputDir != "" {
		// Caller/user-supplied OutputDir: never owned or removed by the executor.
		return "", false, nil
	}

	tmpDir, err := os.MkdirTemp("", "pack-oci-layout-*")
	if err != nil {
		return "", false, fmt.Errorf("creating temp output directory: %w", err)
	}
	opts.BuildOpts.OutputDir = tmpDir
	return tmpDir, true, nil
}

// buildAllPlatforms executes the build for all platforms, either via a single multi-platform
// invocation (preferred — buildkit handles parallelism internally) or sequentially.
func (e *Executor) buildAllPlatforms(ctx context.Context, opts MultiPlatformBuildOpts) ([]PlatformBuildResult, error) {
	caps := e.backend.Capabilities()

	// The temp OutputDir for OCI layout mode is set up (and its cleanup deferred)
	// by Execute via ensureOCILayoutOutputDir, so opts already carries the right
	// OutputDir here. This keeps cleanup at the ownership boundary (Execute) so
	// the deferred removal runs even when a later step returns an error.

	// Prefer single-invocation multi-platform build — buildkit builds all arches
	// in parallel and pushes the manifest list atomically.
	//
	// Route through BuildMultiPlatform when there are multiple platforms. Also route
	// SINGLE-platform builds of the buildkit-native backend through it, so the image
	// is pushed under the FINAL manifest-list name (buildkit produces a single-arch
	// index at the final tag) rather than the per-arch-suffixed name the legacy
	// sequential path (platformOptsFor) uses — otherwise a native single-arch build
	// would leave the final tag uncreated. Scoped to the native backend by name to
	// avoid changing the LLB backend's established single-arch behavior.
	multi := len(opts.Platforms) > 1
	nativeSingle := e.backend.Name() == string(BackendBuildkitNative)
	if caps.SupportsParallelArch && (multi || nativeSingle) {
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
