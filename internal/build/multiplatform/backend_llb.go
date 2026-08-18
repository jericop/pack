package multiplatform

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer" // register docker-container:// scheme
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"

	"github.com/buildpacks/pack/pkg/logging"
)

// LLBBackend implements BuildBackend using the BuildKit Go SDK (LLB API) directly.
// This provides programmatic control over the build graph, streaming progress output,
// and eliminates the need for a Dockerfile intermediate or docker CLI subprocess.
type LLBBackend struct {
	logger       logging.Logger
	buildkitOpts BuildkitOpts
}

// NewLLBBackend creates a new LLB-based build backend.
func NewLLBBackend(logger logging.Logger, buildkitOpts BuildkitOpts) *LLBBackend {
	return &LLBBackend{
		logger:       logger,
		buildkitOpts: buildkitOpts,
	}
}

func (b *LLBBackend) Name() string {
	return "buildkit-llb"
}

func (b *LLBBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{
		SupportsLLB:          true,
		SupportsCacheMounts:  true,
		SupportsParallelArch: true,
		SupportsOCILayout:    true,
		SupportsSecretMounts: true,
	}
}

// Build executes lifecycle phases for a single platform using the LLB API.
func (b *LLBBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	results, err := b.BuildMultiPlatform(ctx, []Platform{opts.Platform}, opts)
	if err != nil {
		return PlatformBuildResult{}, err
	}
	if len(results) == 0 {
		return PlatformBuildResult{}, fmt.Errorf("no results from LLB build")
	}
	return results[0], nil
}

// BuildMultiPlatform builds all platforms using the LLB API.
// Each platform is solved separately against the buildkit daemon.
func (b *LLBBackend) BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	// Connect to the buildkit daemon
	bkClient, err := b.connectToBuildkit(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to buildkit: %w", err)
	}
	defer bkClient.Close()

	// Build each platform
	results := make([]PlatformBuildResult, len(platforms))
	for i, platform := range platforms {
		b.logger.Infof("Building for %s via LLB", platform.String())

		result, err := b.solvePlatform(ctx, bkClient, platform, opts)
		if err != nil {
			return nil, fmt.Errorf("solving for %s: %w", platform.String(), err)
		}
		results[i] = result
	}

	return results, nil
}

// connectToBuildkit resolves and connects to the buildkit daemon.
func (b *LLBBackend) connectToBuildkit(ctx context.Context) (*client.Client, error) {
	addr, err := b.resolveBuildkitAddr(ctx)
	if err != nil {
		return nil, err
	}

	b.logger.Debugf("Connecting to buildkit at %s", addr)
	return client.New(ctx, addr)
}

// resolveBuildkitAddr determines the buildkit daemon address.
// For docker-container driver builders, connects via docker-container:// scheme.
func (b *LLBBackend) resolveBuildkitAddr(ctx context.Context) (string, error) {
	builderName := b.buildkitOpts.Builder
	if builderName == "" {
		builderName = "pack-multiplatform"
	}

	// For docker-container driver, the buildkit socket is inside the container.
	// The buildkit client supports connecting via "docker-container://<container-name>".
	containerName := fmt.Sprintf("buildx_buildkit_%s0", builderName)

	// Verify the container is running
	output, err := runDockerCommandWithOutput(ctx, []string{
		"inspect", containerName, "--format", "{{.State.Running}}",
	}, b.logger)
	if err != nil {
		return "", fmt.Errorf("builder container %s not found; ensure builder is running: docker buildx inspect --bootstrap %s", containerName, builderName)
	}

	if strings.TrimSpace(output) != "true" {
		return "", fmt.Errorf("builder container %s is not running; start it with: docker buildx inspect --bootstrap %s", containerName, builderName)
	}

	addr := fmt.Sprintf("docker-container://%s", containerName)
	b.logger.Debugf("Resolved buildkit address: %s", addr)
	return addr, nil
}

// solvePlatform constructs and solves an LLB graph for a single platform.
func (b *LLBBackend) solvePlatform(ctx context.Context, bkClient *client.Client, platform Platform, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	platformSpec := ocispecs.Platform{
		OS:           platform.OS,
		Architecture: platform.Arch,
		Variant:      platform.Variant,
	}

	// Per-arch image tag
	perArchTag := fmt.Sprintf("%s-build-%s-%s", opts.ImageName, opts.BuildID, platform.Arch)

	// Construct the LLB graph
	state := b.buildLLBState(opts, platform, perArchTag)

	// Marshal the LLB definition
	def, err := state.Marshal(ctx, llb.Platform(platformSpec))
	if err != nil {
		return PlatformBuildResult{}, fmt.Errorf("marshaling LLB for %s: %w", platform.String(), err)
	}

	// Set up secrets session (for registry auth)
	var sessions []session.Attachable
	if opts.DockerConfigPath != "" {
		store, err := secretsprovider.NewStore([]secretsprovider.Source{
			{ID: "docker-config", FilePath: opts.DockerConfigPath},
		})
		if err == nil {
			sessions = append(sessions, secretsprovider.NewSecretProvider(store))
		}
	}

	// Set up progress display — prefix logs with platform for clarity
	ch := make(chan *client.SolveStatus)
	platformPrefix := fmt.Sprintf("[%s]", platform.String())
	go func() {
		for status := range ch {
			for _, v := range status.Vertexes {
				if v.Error != "" {
					fmt.Fprintf(os.Stderr, "%s %s: %s\n", platformPrefix, v.Name, v.Error)
				} else if v.Started != nil && v.Completed == nil {
					fmt.Fprintf(os.Stderr, "%s %s\n", platformPrefix, v.Name)
				}
			}
			for _, l := range status.Logs {
				// Prefix each line of output with the platform
				lines := strings.Split(string(l.Data), "\n")
				for _, line := range lines {
					if line != "" {
						fmt.Fprintf(os.Stderr, "%s %s\n", platformPrefix, line)
					}
				}
			}
		}
	}()

	// Create local FS for the app source directory
	appFS, err := fsutil.NewFS(opts.AppPath)
	if err != nil {
		return PlatformBuildResult{}, fmt.Errorf("creating local FS for app path %s: %w", opts.AppPath, err)
	}

	// Solve
	_, err = bkClient.Solve(ctx, def, client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			"context": appFS,
		},
		Session:             sessions,
		CacheImports:        b.parseCacheImports(),
		CacheExports:        b.parseCacheExports(),
		FrontendAttrs:       map[string]string{},
		AllowedEntitlements: []string{},
	}, ch)
	if err != nil {
		return PlatformBuildResult{}, fmt.Errorf("solving LLB for %s: %w", platform.String(), err)
	}

	return PlatformBuildResult{
		Platform: platform,
		ImageRef: perArchTag,
	}, nil
}

// buildLLBState constructs the LLB state graph for a lifecycle build.
func (b *LLBBackend) buildLLBState(opts PlatformBuildOpts, platform Platform, perArchTag string) llb.State {
	// Start from the builder image
	base := llb.Image(opts.BuilderImage)

	// Run setup — make directories world-writable since chown may not work in unprivileged buildkit
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "mkdir -p /cache /output /layers /platform && chmod -R 777 /cache /output /layers"}),
		llb.WithCustomName("setup directories"),
	).Root()

	// Write custom order.toml if provided
	if opts.OrderToml != "" {
		orderCmd := fmt.Sprintf("cat > /cnb/order.toml << 'TOML'\n%s\nTOML", opts.OrderToml)
		base = base.Run(
			llb.Args([]string{"/bin/bash", "-c", orderCmd}),
			llb.WithCustomName("write order.toml"),
			llb.User("0:0"),
		).Root()
	}

	// Copy app source
	appSource := llb.Local("context")
	base = base.File(
		llb.Copy(appSource, "/", "/workspace", &llb.CopyInfo{
			CreateDestPath:      true,
			AllowWildcard:       true,
			AllowEmptyWildcard:  true,
		}),
		llb.WithCustomName("copy app source"),
	)

	// Cache mount options
	// Note: buildkit cache mounts in LLB don't support uid/gid ownership settings.
	// The lifecycle restorer tries to chown the cache dir which fails in unprivileged buildkit.
	// We use a regular directory instead — lifecycle caching still works within a single build,
	// but doesn't persist across builds. Use --buildkit-cache-from/to for cross-build caching.
	cacheID := fmt.Sprintf("%s-%s", opts.CacheID, platform.Arch)
	_ = cacheID // reserved for future use when LLB supports uid on cache mounts

	// Secret mount for registry auth
	secretMountOpt := llb.AddSecret("/home/cnb/.docker/config.json",
		llb.SecretID("docker-config"),
	)

	// Environment and user for lifecycle phases
	// All phases run as the CNB user, matching the Dockerfile backend's USER directive.
	cnbUser := fmt.Sprintf("%d:%d", opts.BuilderUID, opts.BuilderGID)
	envOpts := []llb.RunOption{
		llb.AddEnv("CNB_PLATFORM_API", opts.PlatformAPI),
		llb.AddEnv("CNB_USER_ID", fmt.Sprintf("%d", opts.BuilderUID)),
		llb.AddEnv("CNB_GROUP_ID", fmt.Sprintf("%d", opts.BuilderGID)),
		llb.User(cnbUser),
	}

	// --- Lifecycle phases ---

	// Phase: Analyzer
	analyzerArgs := buildPhaseArgs(opts, "analyzer", perArchTag)
	analyzerArgs = append(analyzerArgs[:1], append([]string{"-uid", fmt.Sprintf("%d", opts.BuilderUID), "-gid", fmt.Sprintf("%d", opts.BuilderGID)}, analyzerArgs[1:]...)...)
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(analyzerArgs),
			llb.WithCustomName("lifecycle: analyzer"),
			
			secretMountOpt,
		}, envOpts...)...,
	).Root()

	// Phase: Detector
	detectorArgs := buildPhaseArgs(opts, "detector", perArchTag)
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(detectorArgs),
			llb.WithCustomName("lifecycle: detector"),
		}, envOpts...)...,
	).Root()

	// Phase: Restorer (needs cache)
	restorerArgs := buildPhaseArgs(opts, "restorer", perArchTag)
	restorerArgs = append(restorerArgs[:1], append([]string{"-uid", fmt.Sprintf("%d", opts.BuilderUID), "-gid", fmt.Sprintf("%d", opts.BuilderGID)}, restorerArgs[1:]...)...)
	if !opts.ClearCache {
		base = base.Run(
			append([]llb.RunOption{
				llb.Args(restorerArgs),
				llb.WithCustomName("lifecycle: restorer"),
				
			}, envOpts...)...,
		).Root()
	} else {
		restorerArgs = removeCacheArgs(restorerArgs)
		base = base.Run(
			append([]llb.RunOption{
				llb.Args(restorerArgs),
				llb.WithCustomName("lifecycle: restorer"),
			}, envOpts...)...,
		).Root()
	}

	// Phase: Builder
	builderArgs := buildPhaseArgs(opts, "builder", perArchTag)
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(builderArgs),
			llb.WithCustomName("lifecycle: builder"),
		}, envOpts...)...,
	).Root()

	// Phase: Exporter (needs registry auth + cache)
	exporterArgs := buildPhaseArgs(opts, "exporter", perArchTag)
	exporterArgs = append(exporterArgs[:1], append([]string{"-uid", fmt.Sprintf("%d", opts.BuilderUID), "-gid", fmt.Sprintf("%d", opts.BuilderGID)}, exporterArgs[1:]...)...)
	if opts.ClearCache {
		exporterArgs = removeCacheArgs(exporterArgs)
		base = base.Run(
			append([]llb.RunOption{
				llb.Args(exporterArgs),
				llb.WithCustomName("lifecycle: exporter"),
				secretMountOpt,
			}, envOpts...)...,
		).Root()
	} else {
		base = base.Run(
			append([]llb.RunOption{
				llb.Args(exporterArgs),
				llb.WithCustomName("lifecycle: exporter"),
				
				secretMountOpt,
			}, envOpts...)...,
		).Root()
	}

	return base
}

// buildPhaseArgs constructs the command args for a lifecycle phase.
func buildPhaseArgs(opts PlatformBuildOpts, phaseName string, perArchTag string) []string {
	for _, phase := range opts.Phases {
		if phase.Name == phaseName {
			args := phase.Command()
			// Replace image name with per-arch tag
			for i, arg := range args {
				if arg == opts.ImageName {
					args[i] = perArchTag
				}
			}
			return args
		}
	}
	return nil
}

// parseCacheImports converts BuildkitOpts.CacheFrom strings to client CacheOptionsEntry.
func (b *LLBBackend) parseCacheImports() []client.CacheOptionsEntry {
	var imports []client.CacheOptionsEntry
	for _, cf := range b.buildkitOpts.CacheFrom {
		attrs := parseCacheAttrs(cf)
		imports = append(imports, client.CacheOptionsEntry{
			Type:  attrs["type"],
			Attrs: attrs,
		})
	}
	return imports
}

// parseCacheExports converts BuildkitOpts.CacheTo strings to client CacheOptionsEntry.
func (b *LLBBackend) parseCacheExports() []client.CacheOptionsEntry {
	var exports []client.CacheOptionsEntry
	for _, ct := range b.buildkitOpts.CacheTo {
		attrs := parseCacheAttrs(ct)
		exports = append(exports, client.CacheOptionsEntry{
			Type:  attrs["type"],
			Attrs: attrs,
		})
	}
	return exports
}

// parseCacheAttrs parses a cache string like "type=registry,ref=myapp:cache" into attributes.
func parseCacheAttrs(s string) map[string]string {
	attrs := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			attrs[kv[0]] = kv[1]
		}
	}
	return attrs
}

// Ensure LLBBackend satisfies the interfaces.
var _ BuildBackend = (*LLBBackend)(nil)
var _ MultiPlatformBuilder = (*LLBBackend)(nil)
