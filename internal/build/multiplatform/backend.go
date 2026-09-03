// Package multiplatform provides abstractions for building container images
// across multiple architectures. Today the sole backend is BuildKit (the Go SDK
// / LLB, buildkit-native-export); the BuildBackend abstraction is retained so an
// alternative (e.g. a buildah backend) can be added later.
package multiplatform

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/buildpacks/pack/pkg/logging"
)

// BackendType identifies which build backend to use.
type BackendType string

const (
	// BackendDockerDaemon is the standard pack build path: the lifecycle runs in
	// containers against the local Docker daemon, producing a single-architecture
	// image. It is the DEFAULT backend (and what "auto" resolves to). It is an
	// official backend value so that every build flows through the same
	// backend+capabilities model, even though its execution is still routed
	// through the existing daemon lifecycle executor (not BuildBackend.Build) for
	// now.
	BackendDockerDaemon BackendType = "docker-daemon"

	// BackendBuildkit uses the BuildKit Go SDK to run detector + builder +
	// the lifecycle's prepare-image-metadata mode as RUN steps (producing the
	// metadata contract inside BuildKit), then assembles the final CNB app image
	// natively in BuildKit (FROM run-image + add the produced layers + apply the
	// prepared config) and exports it via BuildKit's native multi-platform image
	// export. No layer-data egress to the host. This is the sole native backend
	// today; the abstraction is retained for a future buildah backend.
	BackendBuildkit BackendType = "buildkit"

	// BackendAuto auto-detects the best available backend (resolves to
	// docker-daemon, the standard single-arch path).
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
	return ParsePlatformList(strings.Split(s, ","))
}

// ParsePlatformList parses a slice of platform strings (each entry may itself be
// comma-separated). It is the []string counterpart of ParsePlatforms, used by the
// repeatable --platform flag.
func ParsePlatformList(in []string) ([]Platform, error) {
	var platforms []Platform
	for _, item := range in {
		for _, p := range strings.Split(item, ",") {
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

	// ExtraBuildpackImages are the registry image references of user-supplied extra
	// buildpacks (--buildpack / project.toml) that are MULTI-ARCH images. Each supports
	// every requested platform (pack verifies this). For each platform leg the backend
	// pulls the buildpack's PER-PLATFORM child image directly in LLB and COPYs its
	// /cnb/buildpacks over the builder's, so each arch gets its OWN arch-matching
	// buildpack binaries (PLATFORM-1662 FR-8b). Empty when none were requested.
	ExtraBuildpackImages []string

	// ExtraBuildpacksDir is a host directory (staged by pack) laid out as
	// /cnb/buildpacks/{id}/{version}/* containing the PLATFORM-AGNOSTIC extra buildpacks
	// (inline scripts, local dir/tarball, urn:cnb:registry, single-manifest images). When
	// set, the backend syncs it in as an llb.Local and COPYs it over the builder's
	// /cnb/buildpacks on EVERY platform leg (same arch-neutral content for all). Empty
	// when there are no agnostic extra buildpacks. Both ExtraBuildpackImages (per-arch)
	// and ExtraBuildpacksDir (agnostic) can be set together.
	//
	// These modules only exist in the transient builder state; the final image is
	// assembled FROM the run image, so they never leak into the output.
	ExtraBuildpacksDir string

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
	// StackID is the builder's CNB stack id (io.buildpacks.stack.id). It is advertised
	// to the lifecycle/buildpacks as CNB_STACK_ID so buildpack dependency resolution
	// (packit postal) can select stack-specific PREBUILT dependencies instead of
	// falling back to wildcard-stack SOURCE builds (e.g. compiling CPython).
	StackID string
	// TargetDistroName / TargetDistroVersion are the builder's OS distro (e.g. ubuntu
	// / 24.04), advertised as CNB_TARGET_DISTRO_NAME / CNB_TARGET_DISTRO_VERSION for
	// the same reason.
	TargetDistroName    string
	TargetDistroVersion string
	// BuildEnv is the user-supplied build-time environment (pack --env / --env-file +
	// project.toml [[build.env]]). Written to /platform/env/<NAME> so buildpacks read
	// it as BP_* configuration, matching standard pack.
	BuildEnv map[string]string
	// ExperimentalMode is passed as CNB_EXPERIMENTAL_MODE (e.g. "warn") when set.
	ExperimentalMode string
	// SourceDateEpoch is passed as SOURCE_DATE_EPOCH (Unix seconds) for reproducible
	// timestamps when a creation time is configured.
	SourceDateEpoch string
	// HTTPProxy / HTTPSProxy / NoProxy are propagated to the lifecycle phases (both
	// upper and lower case) so buildpacks that fetch dependencies work behind a proxy.
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string
	// Keychain is pack's resolved registry auth keychain. The host-side finalize step
	// uses it to fetch the just-pushed image config/manifest AUTHENTICATED; without it
	// finalize falls back to anonymous access and can hit registry pull rate limits
	// (e.g. Docker Hub TOOMANYREQUESTS).
	Keychain authn.Keychain
	// DefaultProcessType is the default process (pack --default-process-type). Passed
	// to the exporter as -process-type so the built image's default entrypoint matches
	// the daemon backend.
	DefaultProcessType string
	// AdditionalTags are extra tags to publish the image under (pack --tag). BuildKit
	// pushes the image under all of them; finalize is run per tag.
	AdditionalTags []string
	// SBOMDestinationDir / ReportDestinationDir are host directories (pack
	// --sbom-output-dir / --report-output-dir). When set, the backend extracts
	// /layers/sbom and /layers/report.toml from the built image to these dirs
	// (namespaced per platform for multi-arch), matching the daemon backend's
	// copy-out.
	SBOMDestinationDir   string
	ReportDestinationDir string
	// Bindings are CNB service bindings (pack --binding) to mount read-only at
	// /platform/bindings/<name> in the lifecycle RUNs. Each host dir is synced in as
	// an llb.Local and mounted (not copied) so binding secrets never land in a layer.
	Bindings []BindingMount
	// OverrideUID / OverrideGID are the user's --uid / --gid overrides. A value >= 0
	// overrides the builder's own UID/GID for the lifecycle (matching the daemon
	// backend's -uid/-gid); < 0 means "unset, use the builder's BuilderUID/BuilderGID".
	OverrideUID int
	OverrideGID int
	// Workspace is the app dir mount path inside the build (pack --workspace); empty
	// means the default /workspace.
	Workspace string
	// ExecutionEnv is the CNB execution environment (pack --exec-env). Passed as
	// CNB_EXEC_ENV when the platform API is >= 0.15.
	ExecutionEnv string
}

// BindingMount is a CNB service binding: a host directory exposed read-only under
// /platform/bindings/<Name> during the build.
type BindingMount struct {
	// Name is the binding name buildpacks see at /platform/bindings/<Name>.
	Name string
	// HostPath is the absolute host directory to sync in (contains type/provider/
	// secret files).
	HostPath string
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
	// MaxPlatforms is the number of target platforms the backend accepts in a
	// single build. 1 means single-architecture only (docker-daemon). 0 means
	// unlimited — the backend can build any number of platforms in one invocation
	// (buildkit). The CLI uses this to validate the number of --platform values,
	// so the rule lives with the backend rather than in per-backend CLI branches.
	MaxPlatforms int

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
	// UsesLifecycleCache indicates the backend uses the lifecycle's cache model —
	// the docker-daemon flags --cache / --cache-image / --clear-cache. The
	// docker-daemon backend sets this true. Registry-native backends (buildkit,
	// buildah) set it false: they use their own build-engine cache (for buildkit,
	// --buildkit-cache-from/--buildkit-cache-to) and never delete anything from a
	// registry, so --clear-cache is meaningless. The CLI uses this to reject the
	// lifecycle-cache flags on backends that don't use them (rather than silently
	// ignoring them), keeping backend-specific flag support grouped with the backend.
	UsesLifecycleCache bool
	// SupportsHostVolumes indicates the backend can mount arbitrary host paths into
	// the build (pack --volume). The docker-daemon backend sets this true (real Docker
	// bind mounts on the lifecycle phase containers). The buildkit backend sets it
	// false: it builds in a sandbox with no docker-run-style bind mount — read-write
	// host mounts are structurally impossible and read-only host data would be a
	// point-in-time llb.Local sync rather than a live bind. The CLI rejects --volume on
	// backends where this is false (read-only config/secret delivery is handled by the
	// dedicated --binding mechanism instead; see SupportsBindings).
	SupportsHostVolumes bool
	// SupportsBindings indicates the backend can deliver CNB service bindings
	// (read-only <name>/{type,provider,secret-files} trees at /platform/bindings) via
	// pack --binding. Both backends support it: the buildkit backend mounts each
	// binding read-only into the lifecycle RUNs; the docker-daemon backend translates
	// --binding into the equivalent read-only volume mount.
	SupportsBindings bool
	// SupportsPreviousImage indicates the backend honors --previous-image (the
	// analyzer reads a prior image's layer metadata + SBOM so the exporter can reuse
	// unchanged layers by reference). The docker-daemon backend sets this true. The
	// buildkit backend sets it false: its build-then-finalize model already gets
	// layer-blob reuse from BuildKit's content-addressed vertex cache (unchanged
	// layers keep their digest and are not re-pushed), and finalize authors metadata
	// from the ACTUAL produced layers rather than from a prior tag — so the only
	// distinct benefit (metadata/SBOM continuity across a retag) is out of scope. The
	// CLI rejects --previous-image on a backend where this is false rather than
	// silently ignoring it.
	SupportsPreviousImage bool
}

// Capabilities returns the declared capabilities for a backend type without
// constructing it. This lets the CLI validate platform counts (and resolve
// "auto") using the same single source of truth the backends report. "auto"
// resolves to docker-daemon.
func (t BackendType) Capabilities() BackendCapabilities {
	switch t {
	case BackendBuildkit:
		return BackendCapabilities{
			MaxPlatforms:         0, // unlimited
			SupportsCacheMounts:  true,
			SupportsParallelArch: true,
			SupportsSecretMounts: true,
			PushesNatively:        true,
			UsesLifecycleCache:    false, // uses BuildKit's own cache (--buildkit-cache-*)
			SupportsPreviousImage: false, // content-addressed cache covers layer reuse
			SupportsHostVolumes:   false, // sandboxed; no docker-run-style bind mount
			SupportsBindings:      true,  // --binding mounted read-only into the RUNs
		}
	case BackendDockerDaemon, BackendAuto, "":
		return BackendCapabilities{
			MaxPlatforms:          1, // single-arch only
			PushesNatively:        false,
			UsesLifecycleCache:    true, // --cache / --cache-image / --clear-cache
			SupportsPreviousImage: true, // --previous-image (analyzer reads prior image)
			SupportsHostVolumes:   true, // real Docker bind mounts on phase containers
			SupportsBindings:      true, // --binding translated to a read-only volume
		}
	default:
		// Unknown backend: be permissive on count (the factory will reject it).
		return BackendCapabilities{MaxPlatforms: 0}
	}
}

// Resolve maps "" and "auto" to the concrete default backend (docker-daemon).
func (t BackendType) Resolve() BackendType {
	if t == "" || t == BackendAuto {
		return BackendDockerDaemon
	}
	return t
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


