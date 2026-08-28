package multiplatform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildpacks/pack/pkg/logging"
)

// DockerfileBackend implements BuildBackend by generating a Dockerfile
// and executing it via `docker buildx build`.
//
// When building for multiple platforms, it uses a single buildx invocation
// with --platform linux/amd64,linux/arm64 which lets buildkit handle
// parallelism internally and push a manifest list atomically.
type DockerfileBackend struct {
	logger       logging.Logger
	buildkitOpts BuildkitOpts
}

// NewDockerfileBackend creates a new Dockerfile-based build backend.
func NewDockerfileBackend(logger logging.Logger, buildkitOpts BuildkitOpts) *DockerfileBackend {
	return &DockerfileBackend{
		logger:       logger,
		buildkitOpts: buildkitOpts,
	}
}

func (b *DockerfileBackend) Name() string {
	return "buildkit-dockerfile"
}

func (b *DockerfileBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{
		SupportsLLB:          false,
		SupportsCacheMounts:  true,
		SupportsParallelArch: true, // buildkit handles parallelism internally via single --platform invocation
		SupportsOCILayout:    true,
		SupportsSecretMounts: true,
		// PushesNatively is intentionally false: the Dockerfile MVP does NOT push
		// the manifest list itself. The lifecycle exporter pushes per-arch images
		// to intermediate registry tags and the executor assembles the final
		// manifest list from those tags via `docker buildx imagetools create`.
		PushesNatively: false,
	}
}

// Build satisfies the BuildBackend interface for single-platform builds.
// It delegates to BuildMultiPlatform with a single platform.
func (b *DockerfileBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	results, err := b.BuildMultiPlatform(ctx, []Platform{opts.Platform}, opts)
	if err != nil {
		return PlatformBuildResult{}, err
	}
	if len(results) == 0 {
		return PlatformBuildResult{}, fmt.Errorf("no results from build")
	}
	return results[0], nil
}

// BuildMultiPlatform generates a Dockerfile and runs a single `docker buildx build`
// with multiple --platform values. Buildkit handles parallelism internally.
// The lifecycle exporter pushes per-arch images to the registry. After the build,
// digests are parsed from the output and returned so the caller can assemble
// the manifest list.
func (b *DockerfileBackend) BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	// Generate the Dockerfile content (uses TARGETARCH for per-arch cache IDs)
	dockerfileContent := GenerateDockerfileMultiPlatform(opts)

	// Create a temporary build context directory
	buildDir, err := os.MkdirTemp("", "pack-multiplatform-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	// Write the Dockerfile
	dockerfilePath := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644); err != nil {
		return nil, fmt.Errorf("writing Dockerfile: %w", err)
	}

	// Log the Dockerfile for debugging
	b.logger.Debugf("Generated Dockerfile:\n%s", dockerfileContent)

	// Build the docker buildx build command with all platforms
	args := b.buildBuildxArgs(opts, dockerfilePath, platforms)

	b.logger.Debugf("Executing: docker %v", args)

	// Run the build and capture output to parse digests
	output, err := runDockerCommandCapture(ctx, args, b.logger)
	if err != nil {
		return nil, fmt.Errorf("docker buildx build: %w", err)
	}

	// Parse digests from lifecycle exporter output (lines like "*** Digest: sha256:...")
	digests := parseLifecycleDigests(output)

	// Construct results with the actual per-arch tag references
	results := make([]PlatformBuildResult, len(platforms))
	for i, p := range platforms {
		perArchTag := fmt.Sprintf("%s-build-%s-%s", opts.ImageName, opts.BuildID, p.Arch)
		results[i] = PlatformBuildResult{
			Platform: p,
			ImageRef: perArchTag,
			Digest:   safeIndex(digests, i),
		}
	}

	return results, nil
}

// buildBuildxArgs constructs the argument list for `docker buildx build`.
func (b *DockerfileBackend) buildBuildxArgs(opts PlatformBuildOpts, dockerfilePath string, platforms []Platform) []string {
	args := []string{"buildx", "build"}

	// Specify the builder if configured
	if b.buildkitOpts.Builder != "" {
		args = append(args, "--builder", b.buildkitOpts.Builder)
	}

	// Target platform(s)
	platformStrs := make([]string, len(platforms))
	for i, p := range platforms {
		platformStrs[i] = p.String()
	}
	args = append(args, "--platform", strings.Join(platformStrs, ","))

	// Dockerfile location
	args = append(args, "-f", dockerfilePath)

	// Image naming and output
	if opts.ExportMode == ExportOCILayout && opts.OutputDir != "" {
		// OCI layout mode: extract the /output directory from the build container.
		// Multi-platform builds with type=local create per-platform subdirectories.
		args = append(args, "--output", fmt.Sprintf("type=local,dest=%s", opts.OutputDir))
	} else if opts.Publish {
		// Registry mode: lifecycle pushes per-arch images directly.
		// Buildkit output is discarded.
		args = append(args, "--output", "type=cacheonly")
	} else {
		args = append(args, "--output", "type=cacheonly")
	}

	// External cache configuration
	for _, cacheFrom := range b.buildkitOpts.CacheFrom {
		args = append(args, "--cache-from", cacheFrom)
	}
	for _, cacheTo := range b.buildkitOpts.CacheTo {
		args = append(args, "--cache-to", cacheTo)
	}

	// Network mode
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}

	// Clear cache: skip all buildkit layer caching
	if opts.ClearCache {
		args = append(args, "--no-cache")
	}

	// Extra args passed through by the user
	args = append(args, b.buildkitOpts.ExtraArgs...)

	// Build context is the app source directory
	args = append(args, opts.AppPath)

	return args
}

// parseLifecycleDigests extracts image digests from lifecycle exporter output.
// The lifecycle prints lines like "*** Digest: sha256:abc123..." for each exported image.
func parseLifecycleDigests(output string) []string {
	var digests []string
	for _, line := range strings.Split(output, "\n") {
		if idx := strings.Index(line, "*** Digest: "); idx >= 0 {
			digest := strings.TrimSpace(line[idx+len("*** Digest: "):])
			if strings.HasPrefix(digest, "sha256:") {
				digests = append(digests, digest)
			}
		}
	}
	return digests
}

func safeIndex(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}
