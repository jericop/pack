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
	}
}

// Build generates a Dockerfile from the phase commands, writes it to a temp directory,
// and executes `docker buildx build` for the target platform.
// For single-platform builds this is called once; for multi-platform the executor
// should use BuildMultiPlatform instead.
func (b *DockerfileBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	// Generate the Dockerfile content
	dockerfileContent := GenerateDockerfile(opts)

	// Create a temporary build context directory
	buildDir, err := os.MkdirTemp("", "pack-multiplatform-*")
	if err != nil {
		return PlatformBuildResult{}, fmt.Errorf("creating temp build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	// Write the Dockerfile
	dockerfilePath := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644); err != nil {
		return PlatformBuildResult{}, fmt.Errorf("writing Dockerfile: %w", err)
	}

	// Log the Dockerfile for debugging
	b.logger.Debugf("Generated Dockerfile for %s:\n%s", opts.Platform.String(), dockerfileContent)

	// Build the docker buildx build command
	args := b.buildBuildxArgs(opts, dockerfilePath, []Platform{opts.Platform})

	b.logger.Debugf("Executing: docker %v", args)

	// Run the build
	if err := runDockerCommand(ctx, args, b.logger); err != nil {
		return PlatformBuildResult{}, fmt.Errorf("docker buildx build for %s: %w", opts.Platform.String(), err)
	}

	result := PlatformBuildResult{
		Platform: opts.Platform,
		ImageRef: opts.ImageName,
	}

	return result, nil
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
		perArchTag := fmt.Sprintf("%s-%s", opts.ImageName, p.Arch)
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

	// Secret for registry credentials
	if opts.DockerConfigPath != "" {
		args = append(args, "--secret", fmt.Sprintf("id=docker-config,src=%s", opts.DockerConfigPath))
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

// SaveDockerfile writes the generated Dockerfile to a specified path for debugging.
// This can be used with a --dump-dockerfile flag.
func SaveDockerfile(opts PlatformBuildOpts, outputPath string) error {
	content := GenerateDockerfile(opts)
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating directory for Dockerfile: %w", err)
	}
	return os.WriteFile(outputPath, []byte(content), 0644)
}

// parseLifecycleDigests extracts image digests from lifecycle exporter output.
// The lifecycle prints lines like "*** Digest: sha256:abc123..." for each exported image.
func parseLifecycleDigests(output string) []string {
	var digests []string
	for _, line := range strings.Split(output, "\n") {
		// Look for the lifecycle's digest output pattern
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
