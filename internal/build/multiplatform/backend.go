// Package multiplatform provides abstractions for building container images
// across multiple architectures. Today the sole backend is BuildKit (the Go SDK
// / LLB, buildkit-native-export); the BuildBackend abstraction is retained so an
// alternative (e.g. a buildah backend) can be added later.
package multiplatform

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildpacks/pack/pkg/logging"
)

// BackendType identifies which build backend to use.
type BackendType string

const (
	// BackendBuildkit uses the BuildKit Go SDK to run detector + builder +
	// the lifecycle's prepare-image-metadata mode as RUN steps (producing the
	// metadata contract inside BuildKit), then assembles the final CNB app image
	// natively in BuildKit (FROM run-image + add the produced layers + apply the
	// prepared config) and exports it via BuildKit's native multi-platform image
	// export. No layer-data egress to the host. This is the sole backend today;
	// the abstraction is retained for a future buildah backend.
	BackendBuildkit BackendType = "buildkit"

	// BackendAuto auto-detects the best available backend (currently buildkit).
	BackendAuto BackendType = "auto"
)

// Platform represents a target OS/architecture combination for a build.
type Platform struct {
	OS      string
	Arch    string
	Variant string
}

// String returns the platform in os/arch[/variant] format.
func (p Platform) String() string {
	s := p.OS + "/" + p.Arch
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// ParsePlatform parses a string like "linux/amd64" or "linux/arm64/v8" into a Platform.
func ParsePlatform(s string) (Platform, error) {
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return Platform{OS: parts[0], Arch: parts[1]}, nil
	case 3:
		return Platform{OS: parts[0], Arch: parts[1], Variant: parts[2]}, nil
	default:
		return Platform{}, fmt.Errorf("invalid platform format %q: expected os/arch or os/arch/variant", s)
	}
}

// ParsePlatforms parses a comma-separated list of platforms.
func ParsePlatforms(s string) ([]Platform, error) {
	var platforms []Platform
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		platform, err := ParsePlatform(p)
		if err != nil {
			return nil, err
		}
		platforms = append(platforms, platform)
	}
	if len(platforms) == 0 {
		return nil, fmt.Errorf("no platforms specified")
	}
	return platforms, nil
}

// PhaseCommand describes a single lifecycle phase to execute.
type PhaseCommand struct {
	// Name is the human-readable phase name (e.g., "analyzer", "detector").
	Name string

	// Binary is the absolute path to the lifecycle binary inside the build container.
	Binary string

	// Args are the command-line arguments for the lifecycle binary.
	Args []string

	// Env are additional environment variables to set when running the phase.
	Env map[string]string

	// NeedsCache indicates this phase requires the persistent buildpack cache directory.
	NeedsCache bool

	// NeedsRegistryAuth indicates this phase requires access to registry credentials.
	NeedsRegistryAuth bool
}

// Command returns the full command line as a string slice (binary + args).
func (p PhaseCommand) Command() []string {
	return append([]string{p.Binary}, p.Args...)
}

// PlatformBuildOpts contains all options needed to build for a single platform.
type PlatformBuildOpts struct {
	// Platform is the target OS/arch for this build.
	Platform Platform

	// BuilderImage is the multi-arch builder image reference.
	BuilderImage string

	// LifecycleImage is the lifecycle image to copy binaries from (for untrusted builders).
	LifecycleImage string

	// RunImage is the multi-arch run image reference.
	RunImage string

	// AppPath is the local path to the application source.
	AppPath string

	// Phases contains the ordered lifecycle phase commands to execute.
	Phases []PhaseCommand

	// CacheID is a unique identifier for this app+arch cache (used for cache mount IDs).
	CacheID string

	// BuildID is a short unique identifier for this build invocation.
	// Used to create ephemeral per-arch tags (e.g., image:build-<id>-<arch>).
	BuildID string

	// ImageName is the target image name. For registry push, this is the full reference.
	// For per-arch images, the backend may append an arch-specific tag.
	ImageName string

	// Publish indicates whether to push the image to a registry.
	Publish bool

	// DockerConfigPath is the path to Docker config.json for registry auth.
	DockerConfigPath string

	// BuilderUID is the UID of the CNB user in the builder image.
	BuilderUID int

	// BuilderGID is the GID of the CNB user in the builder image.
	BuilderGID int

	// PlatformAPI is the CNB Platform API version to use.
	PlatformAPI string

	// FileFilter is the optional file filter from project.toml.
	FileFilter func(string) bool

	// Network is the network mode for the build.
	Network string

	// BuildpackImages is the list of additional buildpack OCI image references to add to the builder.
	// These are COPYed from multi-stage FROM instructions in the generated Dockerfile,
	// allowing the remote buildkit builder to pull them directly without needing a local
	// ephemeral builder image.
	BuildpackImages []string

	// OrderToml is the custom order.toml content to write into the builder.
	// When additional buildpacks are specified, this defines the detection order.
	// If empty, the builder's default order is used.
	OrderToml string

	// ClearCache when true disables all caching: buildkit layer cache (--no-cache)
	// and lifecycle buildpack cache (skip the cache mount).
	ClearCache bool

	// RegistryAuth is the JSON value for CNB_REGISTRY_AUTH environment variable.
	// Contains pre-resolved auth headers for registries, eliminating the need for
	// docker config file mounts inside buildkit.
	RegistryAuth string
}

// PlatformBuildResult describes the outcome of building for a single platform.
type PlatformBuildResult struct {
	// ImageRef is the reference to the built image (registry digest ref or OCI layout path).
	ImageRef string

	// Digest is the image digest (if published to a registry).
	Digest string

	// Platform is the platform this result corresponds to.
	Platform Platform
}

// BackendCapabilities describes what a build backend supports.
type BackendCapabilities struct {
	// SupportsCacheMounts indicates the backend supports persistent cache mounts.
	SupportsCacheMounts bool

	// SupportsParallelArch indicates the backend can build multiple architectures in parallel.
	SupportsParallelArch bool

	// SupportsSecretMounts indicates the backend can mount secrets into build steps.
	SupportsSecretMounts bool

	// PushesNatively indicates the backend performs the final registry push /
	// manifest-list assembly itself, so the executor MUST skip its own
	// assembly/push. The buildkit backend sets this true: it exports the
	// (multi-platform) image via BuildKit's native image exporter — one
	// image/index, no intermediate tags.
	PushesNatively bool
}

// BuildBackend is the interface that all multi-platform build backends must implement.
type BuildBackend interface {
	// Name returns the human-readable name of this backend.
	Name() string

	// Build executes all lifecycle phases for a single platform, producing a per-architecture image.
	Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error)

	// Capabilities returns what this backend supports.
	Capabilities() BackendCapabilities
}

// BuildkitOpts holds configuration specific to BuildKit backends.
type BuildkitOpts struct {
	// Builder is the name of the buildx builder to use (empty = default).
	Builder string

	// CacheFrom is a list of external cache sources (e.g., "type=registry,ref=...").
	CacheFrom []string

	// CacheTo is a list of external cache destinations.
	CacheTo []string
}

// MultiPlatformBuildOpts contains all the options needed for a full multi-platform build.
type MultiPlatformBuildOpts struct {
	// Platforms is the list of target platforms to build for.
	Platforms []Platform

	// BuildOpts is the per-platform build configuration (same for all platforms except Platform field).
	BuildOpts PlatformBuildOpts

	// BuildkitOpts contains BuildKit-specific configuration.
	BuildkitOpts BuildkitOpts

	// Logger is the pack logger for output.
	Logger logging.Logger

	// ManifestListName is the name of the final manifest list (e.g., "registry.example.com/myapp:latest").
	ManifestListName string

	// Publish indicates whether to push the final manifest list to a registry.
	Publish bool
}


