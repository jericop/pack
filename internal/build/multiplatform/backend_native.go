package multiplatform

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/tonistiigi/fsutil"

	"github.com/buildpacks/lifecycle/buildkit/cnbfrontend"
	"github.com/buildpacks/lifecycle/phase/emit"

	"github.com/buildpacks/pack/pkg/logging"
)

// emitOutputDir is the directory inside the BuildKit build where the lifecycle's
// emit-mode writes the emit contract. The BuildKit-native recorder writes its
// files under <emitOutputDir>/<emit.RecorderDir>/ (i.e. /emit/buildkit/plan.json
// and config.json). This path is passed to the exporter's -emit-export-plan flag
// and is where pack reads the contract back from.
const emitOutputDir = "/emit"

// NativeBackend implements BuildBackend for the EXPERIMENTAL "buildkit-native"
// approach (Option C: buildkit-native-export). It runs the lifecycle
// detector/builder phases as RUN steps and then runs the exporter in EMIT-MODE
// (-emit-export-plan) so the lifecycle records the layer plan + image config as a
// small contract (plan.json + config.json) INSIDE BuildKit instead of assembling
// and pushing an image. Pack then assembles the final CNB app image natively in
// BuildKit (FROM run-image + add the emitted layers by diffID + apply the emitted
// config) and exports it via BuildKit's native multi-platform image export.
//
// The key property: layer DATA never leaves BuildKit. Only the small
// plan/config metadata crosses to the host (to drive the assembly graph).
//
// This backend shares the low-level BuildKit plumbing (daemon connection,
// progress display, cache import/export, auth) with the LLB backend by holding an
// *LLBBackend; it implements its own build graph and assembly.
type NativeBackend struct {
	logger       logging.Logger
	buildkitOpts BuildkitOpts

	// llb provides the shared BuildKit plumbing (connectToBuildkit,
	// startProgressDisplay, parseCacheImports/Exports). The native backend reuses
	// these rather than duplicating them.
	llb *LLBBackend
}

// NewNativeBackend creates a new BuildKit-native build backend.
func NewNativeBackend(logger logging.Logger, buildkitOpts BuildkitOpts) *NativeBackend {
	return &NativeBackend{
		logger:       logger,
		buildkitOpts: buildkitOpts,
		llb:          NewLLBBackend(logger, buildkitOpts),
	}
}

func (b *NativeBackend) Name() string {
	return "buildkit-native"
}

func (b *NativeBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{
		SupportsLLB:          true,
		SupportsCacheMounts:  true,
		SupportsParallelArch: true,
		SupportsOCILayout:    false,
		SupportsSecretMounts: true,
		// The native backend assembles + pushes the final image (and, for
		// multi-arch, the manifest list) itself via BuildKit's native
		// multi-platform image export, so the executor MUST NOT run its own
		// manifest assembly/push (mirrors the LLB backend's OCI-layout behavior).
		PushesNatively: true,
	}
}

// buildEmitGraph constructs the LLB state that runs the lifecycle through the
// builder phase and then runs the exporter in EMIT-MODE, producing the emit
// contract under emitOutputDir. The returned state's /emit/buildkit/ holds
// plan.json + config.json; /layers holds the built layer tars the plan
// references. Assembly (a later step) consumes both.
//
// This mirrors LLBBackend.buildLLBState through the builder phase, then diverges:
// instead of the exporter writing/pushing an image (or an OCI layout), it runs
// with -emit-export-plan so nothing is pushed and the contract is emitted.
func (b *NativeBackend) buildEmitGraph(opts PlatformBuildOpts, platform Platform, perArchTag string) llb.State {
	// Start from the builder image.
	base := llb.Image(opts.BuilderImage)

	// Optionally replace the lifecycle binaries from a lifecycle image (needed so
	// the builder bundles the emit-capable lifecycle).
	if opts.LifecycleImage != "" && !strings.HasPrefix(opts.LifecycleImage, "pack.local/") {
		lifecycleImg := llb.Image(opts.LifecycleImage)
		base = base.Run(
			llb.Args([]string{"/bin/sh", "-c", "rm -rf /cnb/lifecycle"}),
			llb.WithCustomName("remove existing lifecycle"),
		).Root()
		base = base.File(
			llb.Copy(lifecycleImg, "/cnb/lifecycle", "/cnb/lifecycle", &llb.CopyInfo{
				CreateDestPath: true,
			}),
			llb.WithCustomName("copy lifecycle from "+opts.LifecycleImage),
		)
	}

	// Setup dirs (world-writable; chown may not work in unprivileged buildkit).
	// Includes the /emit output dir for the emit contract.
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "mkdir -p /cache /layers /platform " + emitOutputDir + " && chmod -R 777 /cache /layers " + emitOutputDir}),
		llb.WithCustomName("setup directories"),
	).Root()

	// Custom order.toml if provided.
	if opts.OrderToml != "" {
		orderCmd := fmt.Sprintf("cat > /cnb/order.toml << 'TOML'\n%s\nTOML", opts.OrderToml)
		base = base.Run(
			llb.Args([]string{"/bin/bash", "-c", orderCmd}),
			llb.WithCustomName("write order.toml"),
			llb.User("0:0"),
		).Root()
	}

	// Copy app source and make it writable by the CNB user.
	appSource := llb.Local("context")
	base = base.File(
		llb.Copy(appSource, "/", "/workspace", &llb.CopyInfo{
			CreateDestPath:     true,
			AllowWildcard:      true,
			AllowEmptyWildcard: true,
		}),
		llb.WithCustomName("copy app source"),
	)
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod -R 777 /workspace"}),
		llb.WithCustomName("fix workspace permissions"),
	).Root()

	// Persistent buildpack cache mount.
	cacheID := fmt.Sprintf("%s-%s", opts.CacheID, platform.Arch)
	cacheMountOpt := llb.AddMount("/cache",
		llb.Scratch(),
		llb.AsPersistentCacheDir(cacheID, llb.CacheMountShared),
	)

	// Env + user for lifecycle phases (run as the CNB user).
	cnbUser := fmt.Sprintf("%d:%d", opts.BuilderUID, opts.BuilderGID)
	envOpts := []llb.RunOption{
		llb.AddEnv("CNB_PLATFORM_API", opts.PlatformAPI),
		llb.AddEnv("CNB_USER_ID", fmt.Sprintf("%d", opts.BuilderUID)),
		llb.AddEnv("CNB_GROUP_ID", fmt.Sprintf("%d", opts.BuilderGID)),
		llb.User(cnbUser),
	}
	if opts.RegistryAuth != "" {
		envOpts = append(envOpts, llb.AddEnv("CNB_REGISTRY_AUTH", opts.RegistryAuth))
	}

	// Ensure the cache mount is writable by the CNB user.
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod 777 /cache"}),
		llb.WithCustomName("fix cache mount permissions"),
		cacheMountOpt,
		llb.IgnoreCache,
	).Root()

	// Phase: Analyzer. In emit-mode the analyzer does NOT need -layout /
	// -pull-run-image: the run image is a native base in the assembly step, and
	// the exporter reads it there. The analyzer still resolves the run image into
	// analyzed.toml (which the emit exporter uses for the rebase boundary).
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(b.emitPhaseArgs(opts, "analyzer", perArchTag)),
			llb.WithCustomName("lifecycle: analyzer"),
			cacheMountOpt,
		}, envOpts...)...,
	).Root()

	// Phase: Detector.
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(buildPhaseArgs(opts, "detector", perArchTag)),
			llb.WithCustomName("lifecycle: detector"),
		}, envOpts...)...,
	).Root()

	// Phase: Restorer.
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(b.emitPhaseArgs(opts, "restorer", perArchTag)),
			llb.WithCustomName("lifecycle: restorer"),
			cacheMountOpt,
		}, envOpts...)...,
	).Root()

	// Phase: Builder.
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(buildPhaseArgs(opts, "builder", perArchTag)),
			llb.WithCustomName("lifecycle: builder"),
		}, envOpts...)...,
	).Root()

	// Phase: Exporter in EMIT-MODE. Instead of assembling/pushing an image, the
	// exporter records its operations and writes the emit contract to
	// <emitOutputDir>/buildkit/{plan.json,config.json}. Nothing is pushed.
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(b.emitPhaseArgs(opts, "exporter", perArchTag)),
			llb.WithCustomName("lifecycle: exporter (emit-mode)"),
			cacheMountOpt,
		}, envOpts...)...,
	).Root()

	return base
}

// emitPhaseArgs builds the lifecycle phase args for emit-mode. It starts from the
// base per-arch args, adds the unprivileged-BuildKit flags (-skip-chown/-uid/-gid)
// for the phases that touch layers/cache, and for the exporter adds
// -emit-export-plan=<emitOutputDir> so it runs in emit-mode (no push, no image
// assembly).
func (b *NativeBackend) emitPhaseArgs(opts PlatformBuildOpts, phaseName string, perArchTag string) []string {
	args := buildPhaseArgs(opts, phaseName, perArchTag)
	if len(args) == 0 {
		return args
	}

	switch phaseName {
	case "analyzer", "restorer", "exporter":
		args = insertAfterBinary(args,
			"-skip-chown",
			"-uid", fmt.Sprintf("%d", opts.BuilderUID),
			"-gid", fmt.Sprintf("%d", opts.BuilderGID),
		)
	}

	if phaseName == "exporter" {
		// Opt into emit-mode: record the plan + config to emitOutputDir instead of
		// assembling/pushing an image.
		args = insertAfterBinary(args, "-emit-export-plan", emitOutputDir)
	}

	return args
}

// Build executes the emit graph + native assembly for a single platform.
func (b *NativeBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	results, err := b.BuildMultiPlatform(ctx, []Platform{opts.Platform}, opts)
	if err != nil {
		return PlatformBuildResult{}, err
	}
	if len(results) == 0 {
		return PlatformBuildResult{}, fmt.Errorf("no results from buildkit-native build")
	}
	return results[0], nil
}

// BuildMultiPlatform builds all platforms with the buildkit-native approach:
// for each platform it (1) solves the emit graph to produce the emit contract +
// layer tars inside BuildKit, (2) reads back the small plan/config metadata,
// (3) assembles the final image natively (FROM run-image + add layers + apply
// config), and (4) exports via BuildKit's native image export. Multi-arch
// combines the per-arch images into one manifest list with no intermediate tags.
//
// NOTE: steps (3)-(4) — native assembly + multi-platform export — are implemented
// in assembleAndExport (Tasks 4-5). This method wires the emit-graph solve and
// contract read-back (Task 3) and delegates assembly/export.
func (b *NativeBackend) BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	bkClient, err := b.llb.connectToBuildkit(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to buildkit: %w", err)
	}
	defer bkClient.Close()

	return b.driveFrontend(ctx, bkClient, platforms, opts)
}

// driveFrontend runs the CNB BuildKit gateway frontend (cnbfrontend.Build)
// IN-PROCESS via bkClient.Build. The frontend runs the lifecycle phases +
// emit-mode, assembles the final image(s) FROM the run image inside BuildKit, and
// returns per-platform refs + image config. BuildKit then exports the
// (multi-platform) image via ExporterImage — assembling one manifest list
// natively with no intermediate tags and no host layer-data egress.
//
// The frontend handles ALL platforms in a single Build (it builds each platform
// and returns per-platform refs), so there is one bkClient.Build call regardless
// of platform count.
func (b *NativeBackend) driveFrontend(ctx context.Context, bkClient *client.Client, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	if !opts.Publish {
		// MVP: the frontend path assembles + exports via BuildKit's image
		// exporter with push=true. Non-publish (local daemon/OCI) output is a
		// future addition; fail loudly rather than silently no-op.
		return nil, fmt.Errorf("buildkit-native backend currently requires --publish (registry export)")
	}

	// App source as the build context local.
	appFS, err := fsutil.NewFS(opts.AppPath)
	if err != nil {
		return nil, fmt.Errorf("creating local FS for app path %s: %w", opts.AppPath, err)
	}

	authProvider := newDockerAuthProvider()

	// Frontend options: the cnb-* keys the frontend reads via BuildOpts().Opts.
	frontendAttrs := map[string]string{
		cnbfrontend.OptBuilderImage: opts.BuilderImage,
		cnbfrontend.OptRunImage:     opts.RunImage,
		cnbfrontend.OptImageName:    opts.ImageName,
		cnbfrontend.OptPlatforms:    platformsCSV(platforms),
		cnbfrontend.OptPlatformAPI:  opts.PlatformAPI,
		cnbfrontend.OptUID:          fmt.Sprintf("%d", opts.BuilderUID),
		cnbfrontend.OptGID:          fmt.Sprintf("%d", opts.BuilderGID),
	}
	// Any insecure (plain-HTTP) registry the in-build lifecycle phases must reach
	// (e.g. a local dev registry). Derived from the target image's registry host.
	if reg := registryHost(opts.ImageName); reg != "" && isLikelyInsecureRegistry(reg) {
		frontendAttrs[cnbfrontend.OptInsecureReg] = reg
	}
	if opts.LifecycleImage != "" && !strings.HasPrefix(opts.LifecycleImage, "pack.local/") {
		frontendAttrs[cnbfrontend.OptLifecycleImage] = opts.LifecycleImage
	}
	if opts.OrderToml != "" {
		frontendAttrs[cnbfrontend.OptOrderTOML] = opts.OrderToml
	}
	if opts.RegistryAuth != "" {
		frontendAttrs[cnbfrontend.OptRegistryAuth] = opts.RegistryAuth
	}

	solveOpt := client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			cnbfrontend.ContextLocalName: appFS,
		},
		Session:       []session.Attachable{authProvider},
		CacheImports:  b.llb.parseCacheImports(),
		CacheExports:  b.llb.parseCacheExports(),
		FrontendAttrs: frontendAttrs,
		// Request the network.host entitlement so the frontend's lifecycle phase
		// RUNs (which run on the builder's host network to reach registries the
		// builder is attached to) are permitted. The builder must also be started
		// with --allow-insecure-entitlement network.host. (MVP; revisit for
		// production network isolation.)
		AllowedEntitlements: []string{string(entitlements.EntitlementNetworkHost)},
		// BuildKit assembles + pushes the (multi-platform) image natively under
		// the final name — one manifest list, no intermediate tags.
		Exports: []client.ExportEntry{{
			Type:  client.ExporterImage,
			Attrs: exporterImageAttrs(opts.ImageName),
		}},
	}

	ch := b.llb.startProgressDisplay("[buildkit-native]")
	b.logger.Infof("Building %s via buildkit-native frontend (%d platform(s))", opts.ImageName, len(platforms))

	// product="" -> default. cnbfrontend.Build is the in-process gateway BuildFunc.
	if _, err := bkClient.Build(ctx, solveOpt, "", cnbfrontend.Build, ch); err != nil {
		return nil, fmt.Errorf("buildkit-native frontend build: %w", err)
	}

	// One result per platform; all point at the pushed manifest-list name.
	results := make([]PlatformBuildResult, len(platforms))
	for i, p := range platforms {
		results[i] = PlatformBuildResult{Platform: p, ImageRef: opts.ImageName}
	}
	return results, nil
}

// exporterImageAttrs builds the ExporterImage attrs for the final push. It marks
// the registry insecure (plain HTTP) when the target is a local/dev registry, so
// BuildKit's image exporter does not attempt HTTPS against it.
func exporterImageAttrs(imageName string) map[string]string {
	attrs := map[string]string{"name": imageName, "push": "true"}
	if reg := registryHost(imageName); reg != "" && isLikelyInsecureRegistry(reg) {
		attrs["registry.insecure"] = "true"
	}
	return attrs
}

// platformsCSV renders the platforms as a comma-separated os/arch list for the
// frontend's cnb-platforms option.
func platformsCSV(platforms []Platform) string {
	parts := make([]string, len(platforms))
	for i, p := range platforms {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

// registryHost extracts the registry host (with port) from an image reference,
// e.g. "host.docker.internal:5050/foo/bar:tag" -> "host.docker.internal:5050".
// Returns "" if the ref has no explicit registry host.
func registryHost(imageRef string) string {
	slash := strings.IndexByte(imageRef, '/')
	if slash < 0 {
		return ""
	}
	first := imageRef[:slash]
	// A registry host has a "." or ":" (or is "localhost"); otherwise the first
	// path element is a Docker Hub namespace, not a registry host.
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return ""
}

// isLikelyInsecureRegistry reports whether the registry host is a local/dev
// registry that likely serves plain HTTP: localhost, host.docker.internal, or a
// bare hostname (no dot) such as a docker-network container name (e.g.
// "pack-local-registry:5000"). Hosts with a dot (real DNS names) are treated as
// secure. Conservative and MVP-oriented.
func isLikelyInsecureRegistry(host string) bool {
	h := host
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	switch h {
	case "localhost", "127.0.0.1", "host.docker.internal":
		return true
	}
	// A bare hostname with no dot is not a public DNS name — treat as a local
	// container-name registry (plain HTTP) for the MVP.
	return !strings.Contains(h, ".")
}

// schema is a compile-time reference to the imported emit contract version, so
// the native backend and the lifecycle stay pinned to the same schema.
var _ = emit.Schema
