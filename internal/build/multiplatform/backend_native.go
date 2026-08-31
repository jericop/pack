package multiplatform

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/entitlements"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"

	"github.com/buildpacks/lifecycle/phase/finalize"

	"github.com/buildpacks/pack/pkg/logging"
)

// BuildkitBackend implements BuildBackend for the buildkit approach
// (buildkit-native-export). It runs the lifecycle detector/builder phases as RUN
// steps and then runs the exporter in PREPARE-IMAGE-METADATA mode so the lifecycle
// records the layer plan + image config as a small contract INSIDE BuildKit
// instead of assembling and pushing an image. Pack then assembles the final CNB
// app image natively in BuildKit (FROM run-image + add the emitted layers by
// diffID + apply the emitted config) and exports it via BuildKit's native
// multi-platform image export.
//
// The key property: layer DATA never leaves BuildKit. Only the small
// plan/config metadata crosses to the host (to drive the assembly graph).
//
// It is the sole build backend on this branch. The BuildBackend abstraction is
// retained so an alternative implementation (e.g. buildah-podman) can be added
// later. The low-level BuildKit plumbing (daemon connection, progress display,
// cache import/export, auth) lives in buildkit_client.go.
type BuildkitBackend struct {
	logger       logging.Logger
	buildkitOpts BuildkitOpts
}

// NewBuildkitBackend creates a new buildkit build backend.
func NewBuildkitBackend(logger logging.Logger, buildkitOpts BuildkitOpts) *BuildkitBackend {
	return &BuildkitBackend{
		logger:       logger,
		buildkitOpts: buildkitOpts,
	}
}

func (b *BuildkitBackend) Name() string {
	return "buildkit"
}

func (b *BuildkitBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{
		MaxPlatforms:         0, // unlimited: builds any number of platforms in one invocation
		SupportsCacheMounts:  true,
		SupportsParallelArch: true,
		SupportsSecretMounts: true,
		// The buildkit backend assembles + pushes the final image (and, for
		// multi-arch, the manifest list) itself via BuildKit's native
		// multi-platform image export, so the executor MUST NOT run its own
		// manifest assembly/push.
		PushesNatively: true,
	}
}

// Build executes the buildkit build for a single platform.
func (b *BuildkitBackend) Build(ctx context.Context, opts PlatformBuildOpts) (PlatformBuildResult, error) {
	results, err := b.BuildMultiPlatform(ctx, []Platform{opts.Platform}, opts)
	if err != nil {
		return PlatformBuildResult{}, err
	}
	if len(results) == 0 {
		return PlatformBuildResult{}, fmt.Errorf("no results from buildkit build")
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
func (b *BuildkitBackend) BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	bkClient, err := b.connectToBuildkit(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to buildkit: %w", err)
	}
	defer bkClient.Close()

	return b.driveNative(ctx, bkClient, platforms, opts)
}

// driveNative drives pack's IN-PROCESS gateway BuildFunc (nativeBuildFunc) via
// bkClient.Build. The BuildFunc runs the lifecycle phases + exporter emit-mode,
// assembles the final image(s) FROM the run image via llb.Copy from the emitted
// layer sources, and returns per-platform refs + image config (incl. the
// build-metadata label). BuildKit then exports the (multi-platform) image via
// ExporterImage — ONE image/index, no intermediate tags, no host layer-data
// egress. NativeBackend then calls the lifecycle finalize library to author the
// final CNB metadata. No separate frontend package/image is involved.
//
// A single bkClient.Build handles ALL platforms (the BuildFunc builds each and
// returns per-platform refs).
func (b *BuildkitBackend) driveNative(ctx context.Context, bkClient *client.Client, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	if !opts.Publish {
		// MVP: the native path exports via BuildKit's image exporter with
		// push=true. Non-publish (local daemon/OCI) output is a future addition;
		// fail loudly rather than silently no-op.
		return nil, fmt.Errorf("buildkit-native backend currently requires --publish (registry export)")
	}

	// App source as the build context local.
	appFS, err := fsutil.NewFS(opts.AppPath)
	if err != nil {
		return nil, fmt.Errorf("creating local FS for app path %s: %w", opts.AppPath, err)
	}

	authProvider := newDockerAuthProvider()

	// Build the in-process BuildFunc inputs. There is NO separate frontend
	// package/image: pack drives the gateway API directly with its own BuildFunc
	// (nativeBuildFunc), which assembles FROM run-image via llb.Copy from the
	// emitted layer sources and sets the image config + build-metadata label.
	in := nativeBuildInputs{
		builderImage: opts.BuilderImage,
		runImage:     opts.RunImage,
		imageName:    opts.ImageName,
		platforms:    ocispecsPlatforms(platforms),
		platformAPI:  opts.PlatformAPI,
		uid:          opts.BuilderUID,
		gid:          opts.BuilderGID,
		orderTOML:           opts.OrderToml,
		registryAuth:        opts.RegistryAuth,
		stackID:             opts.StackID,
		targetDistroName:    opts.TargetDistroName,
		targetDistroVersion: opts.TargetDistroVersion,
		buildEnv:            opts.BuildEnv,
		experimentalMode:    opts.ExperimentalMode,
		sourceDateEpoch:     opts.SourceDateEpoch,
		httpProxy:           opts.HTTPProxy,
		httpsProxy:          opts.HTTPSProxy,
		noProxy:             opts.NoProxy,
		defaultProcessType:  opts.DefaultProcessType,
		additionalTags:      opts.AdditionalTags,
		sbomDestDir:         opts.SBOMDestinationDir,
		reportDestDir:       opts.ReportDestinationDir,
		bindings:            opts.Bindings,
	}
	if reg := registryHost(opts.ImageName); reg != "" && isLikelyInsecureRegistry(reg) {
		in.insecureRegistries = []string{reg}
	}
	if opts.LifecycleImage != "" && !strings.HasPrefix(opts.LifecycleImage, "pack.local/") {
		in.lifecycleImage = opts.LifecycleImage
	}

	// Local mounts: the app source context, plus one local per CNB service binding
	// (mounted read-only at /platform/bindings/<name> inside the lifecycle RUNs).
	localMounts := map[string]fsutil.FS{
		contextLocalName: appFS,
	}
	for _, b := range opts.Bindings {
		bindFS, ferr := fsutil.NewFS(b.HostPath)
		if ferr != nil {
			return nil, fmt.Errorf("creating local FS for binding %q (%s): %w", b.Name, b.HostPath, ferr)
		}
		localMounts[bindingLocalName(b.Name)] = bindFS
	}

	solveOpt := client.SolveOpt{
		LocalMounts: localMounts,
		Session: []session.Attachable{authProvider},
		// FrontendAttrs MUST be non-nil: BuildKit's client.solve does
		// `maps.Copy(maps.Clone(opt.FrontendAttrs), cacheOpt.frontendAttrs)`, and
		// when CacheImports is set (--buildkit-cache-from) it writes "cache-imports"
		// into that map. maps.Clone(nil) returns nil, so a nil FrontendAttrs panics
		// ("assignment to entry in nil map") on the cache-from path. An empty map
		// avoids the panic and is otherwise a no-op for our gateway build.
		FrontendAttrs: map[string]string{},
		CacheImports:  b.parseCacheImports(),
		CacheExports:  b.parseCacheExports(),
		// Request the network.host entitlement so the lifecycle phase RUNs (which
		// run on the builder's host network to reach registries the builder is
		// attached to) are permitted. The builder must also be started with
		// --allow-insecure-entitlement network.host. (MVP; revisit for production
		// network isolation.)
		AllowedEntitlements: []string{string(entitlements.EntitlementNetworkHost)},
		// BuildKit assembles + pushes the (multi-platform) image natively under
		// the final name — one image/index, no intermediate tags.
		Exports: []client.ExportEntry{{
			Type:  client.ExporterImage,
			Attrs: exporterImageAttrs(opts.ImageName, opts.AdditionalTags...),
		}},
	}

	// Empty progress prefix: each vertex name already carries a "[os/arch]" prefix
	// (see platformLabel in native_buildfunc.go), so vertex lines read
	// "#15 [linux/arm64] lifecycle: analyzer" without a redundant backend tag. The
	// display also prepends that same "[os/arch]" to each vertex's LOG lines, so
	// every streamed build line is attributable to an architecture in a
	// multi-platform solve.
	ch := b.startProgressDisplay("")
	b.logger.Infof("Building %s via buildkit (%d platform(s))", opts.ImageName, len(platforms))

	// product="" -> default. nativeBuildFunc is pack's in-process gateway BuildFunc.
	if _, err := bkClient.Build(ctx, solveOpt, "", makeNativeBuildFunc(in), ch); err != nil {
		return nil, fmt.Errorf("buildkit-native build: %w", err)
	}

	// Post-push FINALIZE (Option A): author the correct io.buildpacks.lifecycle.metadata
	// on the pushed image from its ACTUAL produced layer diffIDs + the
	// io.buildpacks.lifecycle.prepared-metadata label the build attached. This is
	// the lifecycle's finalize LIBRARY (consumed here like phase.Rebaser); it mutates
	// ONLY the image config+manifest (+ index) — no layer blobs are read, added, or
	// re-uploaded. The finalized image is rebuildable + rebaseable.
	//
	// The build pushed under a name reachable from BuildKit; the host-side finalize
	// must reach the same registry by its HOST-reachable name. In local test setups
	// these differ (container-name vs localhost) — PACK_HOST_REGISTRY_REMAP bridges
	// it (test-env only; no-op in prod where one name works from both sides).
	// Finalize the primary image name AND each additional tag (pack --tag). BuildKit
	// pushed the same image under every name; each tag independently points at the
	// pre-finalize manifest, so finalize must run per name to make them all
	// CNB-compliant. Finalize is idempotent and authors from each image's own
	// diffIDs + label, so per-tag finalize is correct (and cheap: config+manifest
	// only, no layer re-upload).
	finalizeNames := append([]string{opts.ImageName}, opts.AdditionalTags...)
	for _, name := range finalizeNames {
		finalizeRef := applyHostRegistryRemap(name)
		insecure := false
		if reg := registryHost(finalizeRef); reg != "" && isLikelyInsecureRegistry(reg) {
			insecure = true
		}
		b.logger.Infof("Finalizing CNB metadata for %s", finalizeRef)
		if err := finalize.Finalize(ctx, finalizeRef, finalize.Options{
			Insecure: insecure,
			Logger:   b.logger,
			// Authenticate the finalize fetch with pack's keychain (falls back to the
			// default keychain if nil), so it isn't subject to anonymous pull rate limits.
			Keychain: opts.Keychain,
		}); err != nil {
			return nil, fmt.Errorf("finalizing CNB metadata for %s: %w", finalizeRef, err)
		}
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
func exporterImageAttrs(imageName string, additionalTags ...string) map[string]string {
	names := append([]string{imageName}, additionalTags...)
	attrs := map[string]string{"name": strings.Join(names, ","), "push": "true"}
	if reg := registryHost(imageName); reg != "" && isLikelyInsecureRegistry(reg) {
		attrs["registry.insecure"] = "true"
	}
	return attrs
}

// ocispecsPlatforms converts pack Platforms to ocispecs.Platform for the BuildFunc.
func ocispecsPlatforms(platforms []Platform) []ocispecs.Platform {
	out := make([]ocispecs.Platform, len(platforms))
	for i, p := range platforms {
		out[i] = ocispecs.Platform{OS: p.OS, Architecture: p.Arch, Variant: p.Variant}
	}
	return out
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

// Ensure BuildkitBackend satisfies the backend interfaces.
var _ BuildBackend = (*BuildkitBackend)(nil)
var _ MultiPlatformBuilder = (*BuildkitBackend)(nil)
