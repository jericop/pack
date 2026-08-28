package multiplatform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/containerd/containerd/v2/core/content"
	contentlocal "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/docker/cli/cli/config"
	"github.com/moby/buildkit/client"
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer" // register docker-container:// scheme
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"

	"github.com/buildpacks/pack/pkg/logging"
)

// applayoutStoreID is the store identifier that wires the Phase 1 content store
// into the Phase 2 solve. It MUST be identical in two places (design.md "Tier 1:
// Store wiring: OCIStores key matches the llb.OCIStore storeID"):
//   - the SolveOpt.OCIStores map key, and
//   - the storeID argument to llb.OCIStore("", storeID) used to build the import
//     source (llb.OCILayout).
//
// Defining it once and reusing it in both places guarantees they never drift.
const applayoutStoreID = "applayout"

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
		// The LLB backend pushes the final image / manifest list itself in OCI
		// layout mode (native ExporterImage push for single-arch;
		// assembleAndPushManifestList for multi-arch), so the executor MUST NOT
		// run its own manifest assembly/push for this backend (FR-5, Task 5).
		PushesNatively: true,
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

// multiPlatformPushPlan captures how a multi-platform LLB build routes its
// per-arch push and manifest-list assembly. It is the pure decision output of
// planMultiPlatformPush, extracted so the Task 4 flow-wiring guarantees are
// unit-testable without a live BuildKit daemon.
type multiPlatformPushPlan struct {
	// PushPerArch is passed to solvePlatform: when true each platform runs Phase 2
	// and pushes natively (single-arch OCI layout, and — vacuously — registry
	// mode where Phase 2 is not used at all); when false Phase 2 is deferred so
	// the per-arch layouts can be assembled into one manifest list.
	PushPerArch bool

	// AssembleManifestList is true when BuildMultiPlatform must, after the
	// per-platform solves, assemble + push ONE manifest list from the per-arch OCI
	// layouts (multi-arch OCI layout mode only).
	AssembleManifestList bool
}

// planMultiPlatformPush decides the per-arch push / assembly routing for a
// multi-platform LLB build. It is a pure function of the export mode and the
// number of platforms so the routing (Task 4: "wire the two-phase flow") can be
// asserted directly in unit tests.
//
// In OCI layout mode the "no intermediate tags" guarantee (FR-5) depends on NOT
// pushing a per-arch image under a "<img>-build-<id>-<arch>" name when we are
// assembling a multi-arch manifest list. So:
//
//   - Single-arch OCI layout mode: the (only) platform does Phase 2, pushing the
//     imported layout natively under the final image name. No assembly step is
//     needed. (PushPerArch=true, AssembleManifestList=false.)
//   - Multi-arch OCI layout mode: platforms do Phase 1 ONLY (produce the per-arch
//     OCI layout in their own content store). No per-arch push. After the
//     errgroup, assembleAndPushManifestList reads those per-arch layouts and
//     pushes ONE manifest list under the final name (Task 3, Option b).
//     (PushPerArch=false, AssembleManifestList=true.)
//   - Registry mode (default): unchanged. solvePlatform never enters Phase 2 in
//     registry mode (the exporter pushes during Phase 1), so PushPerArch is
//     irrelevant there; we report it as true and AssembleManifestList=false so no
//     OCI-layout assembly is ever triggered (NFR-2 backward compatibility).
func planMultiPlatformPush(mode ExportMode, numPlatforms int) multiPlatformPushPlan {
	assemble := mode == ExportOCILayout && numPlatforms > 1
	return multiPlatformPushPlan{
		PushPerArch:          !assemble,
		AssembleManifestList: assemble,
	}
}

// BuildMultiPlatform builds all platforms using the LLB API.
// Each platform is solved in parallel against the buildkit daemon.
func (b *LLBBackend) BuildMultiPlatform(ctx context.Context, platforms []Platform, opts PlatformBuildOpts) ([]PlatformBuildResult, error) {
	// Log the equivalent Dockerfile for debugging
	b.logger.Debugf("Equivalent Dockerfile (for reference):\n%s", GenerateDockerfileMultiPlatform(opts))

	// Connect to the buildkit daemon
	bkClient, err := b.connectToBuildkit(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to buildkit: %w", err)
	}
	defer bkClient.Close()

	// Decide whether each platform performs its own Phase 2 native push, and
	// whether a post-solve manifest-list assembly step is needed (see
	// planMultiPlatformPush for the full rationale).
	plan := planMultiPlatformPush(opts.ExportMode, len(platforms))
	assembleManifestList := plan.AssembleManifestList
	pushPerArch := plan.PushPerArch

	// Build all platforms in parallel
	results := make([]PlatformBuildResult, len(platforms))
	g, gCtx := errgroup.WithContext(ctx)

	for i, platform := range platforms {
		i, platform := i, platform
		g.Go(func() error {
			b.logger.Infof("Building for %s via LLB", platform.String())
			result, err := b.solvePlatform(gCtx, bkClient, platform, opts, pushPerArch)
			if err != nil {
				return fmt.Errorf("solving for %s: %w", platform.String(), err)
			}
			results[i] = result
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Task 3: assemble + push the multi-arch manifest list from the per-arch OCI
	// layouts produced by Phase 1. Only when we deferred the per-arch push above.
	if assembleManifestList {
		if err := b.assembleAndPushManifestList(ctx, opts, results); err != nil {
			return nil, err
		}
	}

	return results, nil
}

// assembleAndPushManifestList assembles a multi-arch manifest list from the
// per-arch OCI layouts produced by Phase 1 and pushes it atomically under the
// final image name, creating NO intermediate per-arch tags (Task 3, FR-5).
//
// Design decision (Option b, "combine per-arch results"): see the package doc in
// oci_layout_push.go. Because the LLB backend uses client.Solve with a separate
// per-arch content store per platform, there is no single-solve way to drive a
// multi-platform ExporterImage from N separate OCI-layout sources. We therefore
// use the go-containerregistry assembly path (the design's documented fallback),
// which reads the per-arch layouts and pushes only the final index — no per-arch
// tag is ever written to the registry.
func (b *LLBBackend) assembleAndPushManifestList(ctx context.Context, opts PlatformBuildOpts, results []PlatformBuildResult) error {
	b.logger.Infof("Assembling multi-arch manifest list for %s from %d per-arch OCI layout(s)", opts.ImageName, len(results))
	if err := PushPerArchLayoutsAsManifestList(ctx, opts.ImageName, results, b.logger); err != nil {
		return fmt.Errorf("assembling manifest list for %s: %w", opts.ImageName, err)
	}
	return nil
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
//
// pushPerArch controls Phase 2 in OCI layout mode: when true (single-arch), the
// platform imports its Phase 1 layout and pushes it natively under the final
// image name (Task 2). When false (multi-arch), Phase 2 is skipped — the result
// carries OCIStoreDir + OCILayoutDigest so BuildMultiPlatform can assemble the
// manifest list from all platforms afterward (Task 3). In registry mode
// pushPerArch is irrelevant (the exporter pushes during Phase 1).
func (b *LLBBackend) solvePlatform(ctx context.Context, bkClient *client.Client, platform Platform, opts PlatformBuildOpts, pushPerArch bool) (PlatformBuildResult, error) {
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

	// Set up progress display — format output similar to docker buildx.
	// Phase 1 gets its own channel; Phase 2 (the native push) gets a fresh one
	// via the same helper, since each solve consumes its own status stream.
	ch := b.startProgressDisplay(fmt.Sprintf("[%s]", platform.String()))

	// Create local FS for the app source directory
	appFS, err := fsutil.NewFS(opts.AppPath)
	if err != nil {
		return PlatformBuildResult{}, fmt.Errorf("creating local FS for app path %s: %w", opts.AppPath, err)
	}

	// A single session auth provider carries pack-resolved registry credentials
	// (loaded from the Docker config) for both solves: Phase 1's own registry
	// operations and Phase 2's native ExporterImage push (FR-5, design.md open
	// item 3). Building it once and sharing it keeps the credential source
	// identical across phases.
	authProvider := newDockerAuthProvider()

	solveOpt := client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			"context": appFS,
		},
		Session: []session.Attachable{
			authProvider,
		},
		CacheImports:        b.parseCacheImports(),
		CacheExports:        b.parseCacheExports(),
		FrontendAttrs:       map[string]string{},
		AllowedEntitlements: []string{},
	}

	// Phase 1 of the two-phase OCI layout solve (FR-4): export the isolated
	// /output OCI layout to a per-arch content store on disk. The store handle is
	// returned so Phase 2 (a separate task) can attach it via SolveOpt.OCIStores
	// and re-export the image via ExporterImage. Registry mode (the default) adds
	// no Exports here and keeps its existing push-during-exporter behavior.
	var phase1Store content.Store
	var phase1StoreDir string
	if opts.ExportMode == ExportOCILayout {
		phase1StoreDir = perArchStoreDir(opts, platform)
		if err := os.MkdirAll(phase1StoreDir, 0755); err != nil {
			return PlatformBuildResult{}, fmt.Errorf("creating per-arch content store dir %s: %w", phase1StoreDir, err)
		}
		// Use an explicit OutputStore (rather than OutputDir) so we hold the
		// content.Store handle for Phase 2's SolveOpt.OCIStores without re-opening
		// the directory. BuildKit would otherwise construct this same store from
		// OutputDir internally (see buildkit client/solve.go).
		phase1Store, err = contentlocal.NewStore(phase1StoreDir)
		if err != nil {
			return PlatformBuildResult{}, fmt.Errorf("creating content store at %s: %w", phase1StoreDir, err)
		}
		b.logger.Debugf("Phase 1: exporting OCI layout for %s to content store %s", platform.String(), phase1StoreDir)
		solveOpt.Exports = phase1ExportEntry(phase1Store, perArchTag)
	}

	// Solve
	solveResp, err := bkClient.Solve(ctx, def, solveOpt, ch)
	if err != nil {
		return PlatformBuildResult{}, fmt.Errorf("solving LLB for %s: %w", platform.String(), err)
	}

	result := PlatformBuildResult{
		Platform: platform,
		ImageRef: perArchTag,
	}
	if opts.ExportMode == ExportOCILayout {
		// Expose the on-disk store dir so Phase 2 / on-disk inspection can locate it.
		result.ImageRef = phase1StoreDir
		result.OCIStoreDir = phase1StoreDir

		// With tar=false the OCI exporter copies the image blobs into the content
		// store (blobs/sha256/...) but does NOT write the OCI layout marker files
		// (index.json / oci-layout) — those are only produced by the tar path. Our
		// Phase 2 import and on-disk inspection both read the dir as a standard OCI
		// layout (via go-containerregistry layout.FromPath), so synthesize the
		// index.json + oci-layout marker from the manifest descriptor the solve
		// returned. This turns the content store dir into a valid OCI layout.
		if err := writeOCILayoutIndexFromSolve(phase1StoreDir, solveResp); err != nil {
			return PlatformBuildResult{}, fmt.Errorf("writing OCI layout index for %s: %w", platform.String(), err)
		}

		// Phase 2 attaches this content.Store via SolveOpt.OCIStores. We reuse the
		// Phase 1 store handle when we still hold it (avoids re-opening the
		// directory); openPhase1Store exists for callers that only know the dir.
		phase2Store := phase1Store
		if phase2Store == nil {
			phase2Store, err = openPhase1Store(phase1StoreDir)
			if err != nil {
				return PlatformBuildResult{}, fmt.Errorf("opening phase 1 store for %s: %w", platform.String(), err)
			}
		}

		layoutDigest, err := readPhase1LayoutDigest(phase1StoreDir)
		if err != nil {
			return PlatformBuildResult{}, fmt.Errorf("reading phase 1 layout digest for %s: %w", platform.String(), err)
		}
		result.OCILayoutDigest = layoutDigest
		b.logger.Debugf("Phase 1 layout for %s ready for import: dir=%s digest=%s", platform.String(), phase1StoreDir, layoutDigest)

		if !pushPerArch {
			// Multi-arch (Task 3): skip the per-arch native push. The result now
			// carries OCIStoreDir + OCILayoutDigest; BuildMultiPlatform assembles
			// the manifest list from all platforms' layouts and pushes ONE index
			// under the final name, so no per-arch tag lands on the registry.
			// Keep the phase2Store handle from leaking an unused open store.
			_ = phase2Store
			b.logger.Debugf("Phase 2 push deferred for %s: manifest list will be assembled from per-arch layouts", platform.String())
			return result, nil
		}

		// Phase 2 (FR-4, FR-5): import the Phase 1 layout via llb.OCILayout and
		// push it natively via ExporterImage, using the shared auth provider so
		// the push authenticates with pack-resolved registry credentials.
		//
		// Single-arch scope (Task 2): the push name is the FINAL image name
		// (opts.ImageName), not the intermediate per-arch build tag, so a
		// single-arch OCI layout publish creates no intermediate tag. The import
		// ref uses perArchTag@digest because perArchTag is the per-arch image
		// target used consistently through the lifecycle phases and stored in the
		// layout; the digest pins the exact image Phase 1 produced.
		pushName := opts.ImageName
		importRef := buildImportRef(perArchTag, layoutDigest)
		if err := b.solvePhase2Push(ctx, bkClient, platformSpec, phase2Store, importRef, pushName, authProvider); err != nil {
			return PlatformBuildResult{}, fmt.Errorf("phase 2 import+push for %s: %w", platform.String(), err)
		}
		result.ImageRef = pushName
	}
	return result, nil
}

// buildImportRef builds the reference llb.OCILayout imports from the Phase 1
// store: "<ref>@<digest>". ref is the per-arch image target and digest is the
// Phase 1 manifest digest, so BuildKit imports the exact image Phase 1 produced.
func buildImportRef(ref, digest string) string {
	return fmt.Sprintf("%s@%s", ref, digest)
}

// buildImportLayoutState constructs the Phase 2 import source: an llb.OCILayout
// state that reads importRef from the content store identified by storeID. The
// storeID here MUST match the SolveOpt.OCIStores map key (see applayoutStoreID).
// The empty sessionID selects the store attached through OCIStores rather than a
// session-provided one.
func buildImportLayoutState(importRef, storeID string) llb.State {
	return llb.OCILayout(importRef, llb.OCIStore("", storeID))
}

// solvePhase2Push runs the Phase 2 solve: it imports the Phase 1 OCI layout
// (attached as the "applayout" store) and re-exports it via ExporterImage with
// push=true, assembling and pushing natively with no intermediate registry tag
// (FR-4, FR-5). The store is attached under applayoutStoreID, which matches the
// storeID llb.OCIStore reads from, so the import resolves against exactly this
// store.
//
// Phase 2 uses its own fresh progress channel (Phase 1 already consumed one);
// the display pattern mirrors Phase 1.
func (b *LLBBackend) solvePhase2Push(
	ctx context.Context,
	bkClient *client.Client,
	platformSpec ocispecs.Platform,
	store content.Store,
	importRef string,
	pushName string,
	authProvider session.Attachable,
) error {
	finalState := buildImportLayoutState(importRef, applayoutStoreID)

	finalDef, err := finalState.Marshal(ctx, llb.Platform(platformSpec))
	if err != nil {
		return fmt.Errorf("marshaling phase 2 import layout: %w", err)
	}

	ch := b.startProgressDisplay(fmt.Sprintf("[push %s]", pushName))

	_, err = bkClient.Solve(ctx, finalDef, client.SolveOpt{
		// The map key MUST equal the storeID passed to llb.OCIStore above
		// (applayoutStoreID) so the import source resolves to this store.
		OCIStores: map[string]content.Store{applayoutStoreID: store},
		Exports:   phase2ExportEntry(pushName),
		Session:   []session.Attachable{authProvider},
	}, ch)
	if err != nil {
		return fmt.Errorf("solving phase 2 push to %s: %w", pushName, err)
	}
	return nil
}

// phase1ExportEntry builds the Phase 1 export configuration: a single
// ExporterOCI entry that writes the isolated /output OCI layout to the per-arch
// content store (FR-4). This is invoked ONLY in OCI layout mode; registry mode
// (the default) leaves SolveOpt.Exports nil so the lifecycle exporter keeps its
// existing push-during-Phase-1 behavior unchanged (NFR-2).
//
// It exports to a local content store with NO push attr, so Phase 1 never writes
// to a registry — the sole registry write in OCI layout mode happens later
// (single-arch: phase2ExportEntry; multi-arch: the manifest-list assembly).
// Extracted so the export shape is unit-testable without a live BuildKit daemon.
func phase1ExportEntry(store content.Store, perArchTag string) []client.ExportEntry {
	return []client.ExportEntry{{
		Type:        client.ExporterOCI,
		OutputStore: store,
		// tar=false makes the daemon's OCI exporter write the layout to the
		// attached content store (OutputStore) instead of streaming a tarball to
		// the client over filesync. Without it the exporter defaults to tar=true
		// and fails with "method /moby.filesync.v1.FileSend/diffcopy not supported
		// by the client" because we register a content-store session, not a
		// filesync target.
		Attrs: map[string]string{"name": perArchTag, "tar": "false"},
	}}
}

// phase2ExportEntry builds the Phase 2 export configuration: a single
// ExporterImage entry that pushes the imported layout natively under pushName
// (FR-5). This is the ONLY registry interaction in OCI layout mode, and it is
// what guarantees "no intermediate tag":
//
//   - The lifecycle in OCI layout mode writes to /output (-layout -layout-dir
//     /output) and never pushes to a registry, so no per-arch build tag lands
//     on any registry.
//   - Phase 1 exports that /output layout to a local on-disk content store
//     (ExporterOCI + OutputStore), again with no registry push.
//   - Phase 2's single ExporterImage push here is the sole registry write, and
//     it uses the final target name (pushName) with push=true. There is no
//     "<img>-build-<id>-<arch>" intermediate tag pushed to a registry.
//
// Extracted as a standalone function so the export configuration is unit-testable
// without a live BuildKit daemon or registry (design.md Testing Strategy "Tier 1":
// the ExporterImage attrs and target name can be asserted directly). Returns a
// slice because SolveOpt.Exports is a slice; OCI layout mode uses exactly one
// entry.
func phase2ExportEntry(pushName string) []client.ExportEntry {
	return []client.ExportEntry{{
		Type:  client.ExporterImage,
		Attrs: map[string]string{"name": pushName, "push": "true"},
	}}
}

// startProgressDisplay creates a fresh SolveStatus channel and starts a
// goroutine that renders BuildKit progress to stderr in a docker-buildx-like
// format, prefixing each line with the given label (e.g. "[linux/amd64]" for a
// build solve or "[push <ref>]" for the Phase 2 push). Each solve needs its own
// channel, so callers get a new one per invocation. The goroutine exits when the
// solve closes the channel.
func (b *LLBBackend) startProgressDisplay(prefix string) chan *client.SolveStatus {
	ch := make(chan *client.SolveStatus)
	vertexStartTimes := make(map[string]int64)
	vertexNumbers := make(map[string]int)
	vertexCounter := 0
	go func() {
		for status := range ch {
			for _, v := range status.Vertexes {
				id := v.Digest.String()
				if v.Started != nil && vertexStartTimes[id] == 0 {
					vertexCounter++
					vertexStartTimes[id] = v.Started.UnixMilli()
					vertexNumbers[id] = vertexCounter
					fmt.Fprintf(os.Stderr, "#%d %s %s\n", vertexCounter, prefix, v.Name)
				}
				if v.Completed != nil {
					num := vertexNumbers[id]
					startMs := vertexStartTimes[id]
					var duration float64
					if startMs > 0 {
						duration = float64(v.Completed.UnixMilli()-startMs) / 1000.0
					}
					if v.Cached {
						fmt.Fprintf(os.Stderr, "#%d %s %s CACHED\n", num, prefix, v.Name)
					} else if v.Error != "" {
						fmt.Fprintf(os.Stderr, "#%d %s %s ERROR: %s\n", num, prefix, v.Name, v.Error)
					} else {
						fmt.Fprintf(os.Stderr, "#%d %s %s DONE %.1fs\n", num, prefix, v.Name, duration)
					}
				}
			}
			for _, l := range status.Logs {
				// Find the step number for this log's vertex
				stepNum := 0
				for id, num := range vertexNumbers {
					if id == l.Vertex.String() {
						stepNum = num
						break
					}
				}
				lines := strings.Split(string(l.Data), "\n")
				for _, line := range lines {
					if line != "" {
						fmt.Fprintf(os.Stderr, "#%d %s %s\n", stepNum, prefix, line)
					}
				}
			}
		}
	}()
	return ch
}

// newDockerAuthProvider builds a BuildKit session auth provider seeded from the
// default Docker config. It is shared by the Phase 1 and Phase 2 solves so both
// authenticate with the same pack-resolved registry credentials (FR-5).
func newDockerAuthProvider() session.Attachable {
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: authprovider.LoadAuthConfig(config.LoadDefaultConfigFile(os.Stderr)),
	})
}

// openPhase1Store opens the on-disk content store that Phase 1 wrote the
// isolated /output OCI layout to, returning a content.Store handle suitable for
// attaching to a Phase 2 solve via SolveOpt.OCIStores (FR-4, FR-5).
//
// Phase 1 already holds a content.Store handle (the one it exported to). When
// that handle is available, Phase 2 should reuse it directly rather than
// re-opening — see phase1Store in solvePlatform. This helper exists for the case
// where only the on-disk directory is known (e.g. a Phase 2 that runs from a
// path, or a test using a synthetic layout fixture): contentlocal.NewStore is
// idempotent over an existing store directory, so re-opening reads the same
// blobs Phase 1 wrote.
func openPhase1Store(storeDir string) (content.Store, error) {
	if storeDir == "" {
		return nil, fmt.Errorf("phase 1 content store directory is empty")
	}
	if info, err := os.Stat(storeDir); err != nil {
		return nil, fmt.Errorf("phase 1 content store dir %s: %w", storeDir, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("phase 1 content store path %s is not a directory", storeDir)
	}
	store, err := contentlocal.NewStore(storeDir)
	if err != nil {
		return nil, fmt.Errorf("opening content store at %s: %w", storeDir, err)
	}
	return store, nil
}

// writeOCILayoutIndexFromSolve turns a content-store directory into a valid OCI
// image layout by writing the two marker files the tar exporter would have
// written but the store (tar=false) path does not: the "oci-layout" version
// marker and an "index.json" referencing the image manifest.
//
// With tar=false BuildKit's OCI exporter copies the image blobs into the content
// store (blobs/sha256/...) and returns the top-level manifest descriptor in the
// solve response (exptypes.ExporterImageDescriptorKey, base64-encoded JSON; or
// ExporterImageDigestKey as a fallback). We decode that descriptor and write it
// as the sole entry of index.json, so the directory reads as a standard OCI
// layout for both Phase 2's llb.OCILayout import and the on-disk inspection.
func writeOCILayoutIndexFromSolve(storeDir string, resp *client.SolveResponse) error {
	if resp == nil {
		return fmt.Errorf("nil solve response")
	}

	desc, err := descriptorFromSolveResponse(resp)
	if err != nil {
		return err
	}

	index := ocispecs.Index{
		MediaType: ocispecs.MediaTypeImageIndex,
		Manifests: []ocispecs.Descriptor{desc},
	}
	index.SchemaVersion = 2

	indexJSON, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshaling index.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "index.json"), indexJSON, 0644); err != nil {
		return fmt.Errorf("writing index.json to %s: %w", storeDir, err)
	}

	// The "oci-layout" marker file declares the layout version (imageLayoutVersion).
	layoutMarker, err := json.Marshal(ocispecs.ImageLayout{Version: ocispecs.ImageLayoutVersion})
	if err != nil {
		return fmt.Errorf("marshaling oci-layout marker: %w", err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "oci-layout"), layoutMarker, 0644); err != nil {
		return fmt.Errorf("writing oci-layout marker to %s: %w", storeDir, err)
	}
	return nil
}

// descriptorFromSolveResponse extracts the top-level image manifest descriptor
// from a solve response. It prefers the full base64-encoded descriptor
// (ExporterImageDescriptorKey); if absent it falls back to reconstructing a
// minimal descriptor from the digest (ExporterImageDigestKey) with the OCI
// manifest media type.
func descriptorFromSolveResponse(resp *client.SolveResponse) (ocispecs.Descriptor, error) {
	if resp.ExporterResponse != nil {
		if encoded, ok := resp.ExporterResponse[exptypes.ExporterImageDescriptorKey]; ok && encoded != "" {
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return ocispecs.Descriptor{}, fmt.Errorf("decoding image descriptor: %w", err)
			}
			var desc ocispecs.Descriptor
			if err := json.Unmarshal(raw, &desc); err != nil {
				return ocispecs.Descriptor{}, fmt.Errorf("unmarshaling image descriptor: %w", err)
			}
			if desc.Digest != "" {
				return desc, nil
			}
		}
		if dgst, ok := resp.ExporterResponse[exptypes.ExporterImageDigestKey]; ok && dgst != "" {
			return ocispecs.Descriptor{
				MediaType: ocispecs.MediaTypeImageManifest,
				Digest:    digest.Digest(dgst),
			}, nil
		}
	}
	return ocispecs.Descriptor{}, fmt.Errorf("solve response has no image descriptor (%s) or digest (%s)",
		exptypes.ExporterImageDescriptorKey, exptypes.ExporterImageDigestKey)
}

// readPhase1LayoutDigest reads the manifest digest of the single image in the
// Phase 1 OCI layout at storeDir. Phase 2 needs this digest to construct the
// import reference for llb.OCILayout ("<ref>@<digest>"). It reuses
// InspectOCILayout (which resolves the index → image manifest and validates the
// layout is complete), so a malformed or incomplete Phase 1 store surfaces here
// rather than deep inside the Phase 2 solve.
func readPhase1LayoutDigest(storeDir string) (string, error) {
	inspection, err := InspectOCILayout(storeDir)
	if err != nil {
		return "", fmt.Errorf("reading manifest digest from phase 1 layout %s: %w", storeDir, err)
	}
	if inspection.ManifestDigest == "" {
		return "", fmt.Errorf("phase 1 layout %s has no manifest digest", storeDir)
	}
	return inspection.ManifestDigest, nil
}

// perArchStoreDir returns the on-disk directory for a platform's Phase 1 OCI
// layout content store. Each platform gets its own subdirectory so parallel
// solves never collide (FR-4: "Each parallel platform MUST use its own content
// store"). When opts.OutputDir is set (the executor allocates a temp dir for OCI
// layout mode) stores live under it; otherwise a process temp dir is used so the
// plumbing is still exercisable without the executor.
func perArchStoreDir(opts PlatformBuildOpts, platform Platform) string {
	base := opts.OutputDir
	if base == "" {
		base = os.TempDir()
	}
	// platform.Arch alone can collide across variants (e.g. arm/v6 vs arm/v7),
	// so include os, arch, and variant in the leaf path.
	leaf := platform.OS + "-" + platform.Arch
	if platform.Variant != "" {
		leaf += "-" + platform.Variant
	}
	return filepath.Join(base, "oci-store-"+leaf)
}

// buildLLBState constructs the LLB state graph for a lifecycle build.
func (b *LLBBackend) buildLLBState(opts PlatformBuildOpts, platform Platform, perArchTag string) llb.State {
	// Start from the builder image
	base := llb.Image(opts.BuilderImage)

	// If a lifecycle image is specified, replace the lifecycle binaries.
	// Use a RUN step to remove existing and copy from the lifecycle image.
	if opts.LifecycleImage != "" && !strings.HasPrefix(opts.LifecycleImage, "pack.local/") {
		lifecycleImg := llb.Image(opts.LifecycleImage)
		// First remove the existing lifecycle, then copy from the image
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

	// Copy app source and ensure writable by CNB user
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

	// Cache mount options
	// With the patched lifecycle (-skip-chown flag), we can use persistent cache mounts.
	// The lifecycle will skip the chown attempt that fails in unprivileged buildkit.
	cacheID := fmt.Sprintf("%s-%s", opts.CacheID, platform.Arch)
	cacheMountOpt := llb.AddMount("/cache",
		llb.Scratch(),
		llb.AsPersistentCacheDir(cacheID, llb.CacheMountShared),
	)

	// Secret mount for registry auth — no longer needed.
	// CNB_REGISTRY_AUTH env var is used instead (set in envOpts below).

	// Environment and user for lifecycle phases
	// All phases run as the CNB user, matching the Dockerfile backend's USER directive.
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
	// OCI layout export (-layout) is a lifecycle EXPERIMENTAL feature; without
	// this it aborts with "experimental features are disabled by
	// CNB_EXPERIMENTAL_MODE=error". The Dockerfile backend sets the same env
	// (see generateLifecycleRunMultiPlatform / dockerfile_generator.go).
	if opts.ExportMode == ExportOCILayout {
		envOpts = append(envOpts, llb.AddEnv("CNB_EXPERIMENTAL_MODE", "warn"))
	}

	// --- Lifecycle phases ---
	// All analyzer/restorer/exporter args include -skip-chown -uid -gid

	// Ensure cache mount is writable by CNB user (buildkit creates it as root:0755)
	// IgnoreCache ensures this runs even when importing from registry cache with a fresh builder
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod 777 /cache"}),
		llb.WithCustomName("fix cache mount permissions"),
		cacheMountOpt,
		llb.IgnoreCache,
	).Root()

	// Phase: Analyzer
	analyzerArgs := buildLifecyclePhaseArgs(opts, "analyzer", perArchTag)
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(analyzerArgs),
			llb.WithCustomName("lifecycle: analyzer"),
			cacheMountOpt,
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

	// Phase: Restorer
	restorerArgs := buildLifecyclePhaseArgs(opts, "restorer", perArchTag)
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(restorerArgs),
			llb.WithCustomName("lifecycle: restorer"),
			cacheMountOpt,
		}, envOpts...)...,
	).Root()

	// Phase: Builder
	builderArgs := buildPhaseArgs(opts, "builder", perArchTag)
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(builderArgs),
			llb.WithCustomName("lifecycle: builder"),
		}, envOpts...)...,
	).Root()

	// Phase: Exporter (needs registry auth + cache)
	exporterArgs := buildLifecyclePhaseArgs(opts, "exporter", perArchTag)
	base = base.Run(
		append([]llb.RunOption{
			llb.Args(exporterArgs),
			llb.WithCustomName("lifecycle: exporter"),
			cacheMountOpt,
		}, envOpts...)...,
	).Root()

	// In OCI layout mode, the exporter wrote a complete OCI image to /output.
	// Phase 1's export subject must be that layout, not the whole container root.
	// Isolate it by copying /output into a fresh scratch state so the marshaled
	// definition roots the OCI layout at "/" of the exported filesystem.
	if opts.ExportMode == ExportOCILayout {
		return isolateOutputLayout(base)
	}

	return base
}

// isolateOutputLayout returns a scratch-rooted state containing only the OCI
// layout the lifecycle exporter wrote to /output.
//
// Design decision (design.md "Risk: Isolating /output for the OCI export",
// open item 5): the two candidate approaches were (a) exporting an
// ExporterOCI of the /output subtree, or (b) copying /output into llb.Scratch()
// before export. We use (b): copy /output into llb.Scratch(). This is the
// cleaner, more portable choice because:
//   - ExporterOCI exports the root ("/") of whatever state it is given; there is
//     no per-export "subpath" knob to point it at /output. So making /output the
//     export root still requires producing a state whose root IS /output.
//   - Copying /output into llb.Scratch() yields exactly that: a state whose "/"
//     is the OCI layout (index.json, oci-layout, blobs/). The subsequent
//     ExporterOCI then writes a clean layout with no container-root noise.
//   - It keeps the Phase 1 store minimal (only the layout bytes), which makes the
//     on-disk inspection (Task 7) and Phase 2 import (llb.OCILayout) straightforward.
func isolateOutputLayout(exported llb.State) llb.State {
	return llb.Scratch().File(
		llb.Copy(exported, "/output", "/", &llb.CopyInfo{
			CreateDestPath:     true,
			AllowWildcard:      true,
			AllowEmptyWildcard: true,
		}),
		llb.WithCustomName("isolate /output OCI layout"),
	)
}

// insertAfterBinary returns args with the given flags inserted immediately after
// the binary (args[0]), preserving the trailing image reference and other args.
// If args is empty it returns args unchanged.
func insertAfterBinary(args []string, flags ...string) []string {
	if len(args) == 0 {
		return args
	}
	result := make([]string, 0, len(args)+len(flags))
	result = append(result, args[0])
	result = append(result, flags...)
	result = append(result, args[1:]...)
	return result
}

// buildPhaseArgs constructs the base command args for a lifecycle phase,
// substituting the per-arch tag for the target image name.
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

// buildLifecyclePhaseArgs constructs the full command args for a lifecycle phase
// as executed by the LLB backend, including the flags required by the unprivileged
// BuildKit environment and, in OCI layout export mode, the flags that make the
// lifecycle write a complete OCI image to /output instead of pushing to a registry.
//
// Flag rules (see FR-3):
//   - analyzer, restorer, exporter get -skip-chown -uid -gid (unprivileged BuildKit).
//   - In OCI layout mode:
//   - analyzer gets -layout -layout-dir /output -pull-run-image. Inside BuildKit
//     pack cannot pre-populate the run image into the layout dir, so
//     -pull-run-image tells the lifecycle to pull the run image (named by the
//     analyzer's -run-image arg) into /output itself, so the exporter can
//     resolve it. This requires a lifecycle that defines -pull-run-image
//     (jericop/lifecycle:buildkit-multi-arch-support and later); the older
//     skip-chown-poc lifecycle does NOT have it.
//   - exporter gets -layout -layout-dir /output so the exported image lands in
//     /output as the Phase 1 subject of the two-phase solve.
func buildLifecyclePhaseArgs(opts PlatformBuildOpts, phaseName string, perArchTag string) []string {
	args := buildPhaseArgs(opts, phaseName, perArchTag)
	if len(args) == 0 {
		return args
	}

	// analyzer, restorer, and exporter run against the layers/cache and must skip
	// the chown that fails in the unprivileged BuildKit environment.
	switch phaseName {
	case "analyzer", "restorer", "exporter":
		args = insertAfterBinary(args,
			"-skip-chown",
			"-uid", fmt.Sprintf("%d", opts.BuilderUID),
			"-gid", fmt.Sprintf("%d", opts.BuilderGID),
		)
	}

	if opts.ExportMode == ExportOCILayout {
		switch phaseName {
		case "analyzer":
			// Write to the /output layout AND pull the run image into it. In
			// BuildKit pack can't pre-populate the run image, so -pull-run-image
			// makes the lifecycle pull it (named by -run-image in the phase
			// command) into /output so the exporter can resolve it. Requires a
			// lifecycle that defines -pull-run-image.
			args = insertAfterBinary(args, "-layout", "-layout-dir", "/output", "-pull-run-image")
		case "exporter":
			// Write the complete OCI image to /output (Phase 1 export subject).
			args = insertAfterBinary(args, "-layout", "-layout-dir", "/output")
		}
	}

	return args
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
