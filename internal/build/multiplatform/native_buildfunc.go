package multiplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	ggcrname "github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/buildpacks/lifecycle/api"
	"github.com/buildpacks/lifecycle/phase/emit"
	"github.com/buildpacks/lifecycle/platform/files"
)

// nativeBuildFunc is the IN-PROCESS BuildKit gateway BuildFunc for the
// buildkit-native (Option A) backend. Pack drives it via bkClient.Build (NOT a
// separate published frontend — the cnbfrontend package/image are retired). It:
//   - runs the lifecycle phases + exporter EMIT-MODE as LLB RUNs,
//   - reads the emit plan (whose NEW layers carry filesystem SOURCE refs) +
//     build-metadata.json,
//   - assembles FROM run-image by llb.Copy-ing each layer from its source (no tar
//     extraction, no run-image shell/tar, no large materialization),
//   - sets the image config + the io.buildpacks.lifecycle.prepared-metadata
//     label via the gateway result (ExporterImageConfigKey),
//   - returns per-platform refs so BuildKit exports ONE (multi-platform) image.
//
// A lifecycle FINALIZE step (called by NativeBackend after the push) then authors
// the real io.buildpacks.lifecycle.metadata from the produced diffIDs.
type nativeBuildInputs struct {
	builderImage       string
	runImage           string
	lifecycleImage     string
	platformAPI        string
	uid, gid           int
	orderTOML          string
	registryAuth       string
	imageName          string
	insecureRegistries []string
	platforms          []ocispecs.Platform
	// stackID + target distro are advertised to the lifecycle/buildpacks so postal
	// dependency resolution picks stack/target-specific PREBUILT dependencies rather
	// than falling back to a wildcard-stack SOURCE build. Read from the builder image.
	stackID             string
	targetDistroName    string
	targetDistroVersion string
	// buildEnv is the user-supplied build-time env (pack --env / --env-file +
	// project.toml [[build.env]]). Written as files under /platform/env/<NAME> — the
	// CNB platform contract the lifecycle reads to expose them to buildpacks (this is
	// how BP_* configuration reaches buildpacks). Standard pack writes these into the
	// builder's env layer; the buildkit path must do the same.
	buildEnv map[string]string
	// experimentalMode, when non-empty, is passed as CNB_EXPERIMENTAL_MODE (needed
	// for extensions and some lifecycle features), matching standard pack.
	experimentalMode string
	// sourceDateEpoch, when non-empty, is passed as SOURCE_DATE_EPOCH for reproducible
	// image timestamps, matching standard pack's --creation-time handling.
	sourceDateEpoch string
	// httpProxy/httpsProxy/noProxy are propagated (both UPPER and lower case) to the
	// lifecycle phases so buildpacks that fetch dependencies work behind a proxy,
	// matching standard pack's WithLifecycleProxy.
	httpProxy  string
	httpsProxy string
	noProxy    string
	// defaultProcessType is passed to the exporter as -process-type so the built
	// image's default entrypoint is the requested process (pack --default-process-type),
	// matching standard pack.
	defaultProcessType string
	// additionalTags are extra tags (pack --tag). The exporter records them, but in
	// emit-mode the RecordingImage does not push; the backend applies them when it
	// publishes the assembled image (see exporterImageAttrs) and finalize updates
	// each tag.
	additionalTags []string
	// sbomDestDir / reportDestDir are host directories (pack --sbom-output-dir /
	// --report-output-dir). When set, the backend reads /layers/sbom and
	// /layers/report.toml out of the built LLB state and writes them here — the
	// buildkit analog of the daemon backend's CopyOutTo. For multi-arch builds the
	// output is namespaced per platform (<dest>/<os>-<arch>/...).
	sbomDestDir   string
	reportDestDir string
	// bindings are CNB service bindings (pack --binding). Each is synced in as an
	// llb.Local (keyed bindingLocalPrefix+Name) and MOUNTED read-only at
	// /platform/bindings/<Name> on the detector + builder RUNs — mounted, not copied,
	// so binding secrets never land in a layer.
	bindings []BindingMount
	// workspace is the app dir mount path inside the build (pack --workspace),
	// default /workspace. The app source is copied here and every phase runs with
	// -app <workspace>.
	workspace string
	// hasExtraBuildpacks indicates the user supplied additional buildpack modules
	// (--buildpack / --pre-buildpack / --post-buildpack). When true, buildEmitLLB
	// syncs the staged modules (llb.Local(extraBuildpacksLocalName), a tree rooted at
	// /cnb/buildpacks/...) and copies them over the builder's /cnb/buildpacks before
	// detect, so added buildpacks participate and same-id/version modules override the
	// builder's copy. The staging dir is provided by the backend as a local mount.
	hasExtraBuildpacks bool
	// execEnv is the CNB execution environment (pack --exec-env, e.g. production/
	// test/development). Passed as CNB_EXEC_ENV when the platform API is >= 0.15.
	execEnv string
}

// bindingLocalName returns the llb.Local key (and SolveOpt.LocalMounts key) for a
// binding, namespaced so it can't collide with the app context local.
func bindingLocalName(name string) string { return "cnb-binding-" + name }

// contextLocalName is the llb.Local key under which pack provides the app source.
const contextLocalName = "context"

// extraBuildpacksLocalName is the llb.Local key (and SolveOpt.LocalMounts key) under
// which pack provides the staged extra buildpack modules (tree rooted at
// /cnb/buildpacks/...). Copied over the builder's /cnb/buildpacks before detect.
const extraBuildpacksLocalName = "cnb-extra-buildpacks"

const emitDirNBF = "/emit"

// reportTOMLPathNBF is where the exporter writes report.toml inside the build; the
// backend reads it out for --report-output-dir. sbomDirNBF is the launch SBOM tree
// the backend reads out for --sbom-output-dir.
const reportTOMLPathNBF = "/layers/report.toml"
const sbomDirNBF = "/layers/sbom"

// makeNativeBuildFunc returns a gateway BuildFunc bound to the given inputs.
func makeNativeBuildFunc(in nativeBuildInputs) client.BuildFunc {
	return func(ctx context.Context, c client.Client) (*client.Result, error) {
		multiPlatform := len(in.platforms) > 1
		res := client.NewResult()
		expPlatforms := &exptypes.Platforms{Platforms: make([]exptypes.Platform, len(in.platforms))}

		eg, ctx := errgroup.WithContext(ctx)
		for i, p := range in.platforms {
			i, p := i, p
			eg.Go(func() error {
				ref, cfg, err := nativeBuildPlatform(ctx, c, in, p)
				if err != nil {
					return errors.Wrapf(err, "building %s/%s", p.OS, p.Architecture)
				}
				if !multiPlatform {
					res.AddMeta(exptypes.ExporterImageConfigKey, cfg)
					res.SetRef(ref)
					return nil
				}
				k := fmt.Sprintf("%s/%s", p.OS, p.Architecture)
				res.AddMeta(fmt.Sprintf("%s/%s", exptypes.ExporterImageConfigKey, k), cfg)
				res.AddRef(k, ref)
				expPlatforms.Platforms[i] = exptypes.Platform{ID: k, Platform: p}
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
		if multiPlatform {
			dt, err := json.Marshal(expPlatforms)
			if err != nil {
				return nil, errors.Wrap(err, "marshal platforms")
			}
			res.AddMeta(exptypes.ExporterPlatformsKey, dt)
		}
		return res, nil
	}
}

// nativeBuildPlatform builds one platform: solve the emit graph, read the plan +
// build-metadata, assemble FROM run-image via llb.Copy from sources, and return the
// assembled ref + the marshaled image config (runtime config + build-metadata label).
func nativeBuildPlatform(ctx context.Context, c client.Client, in nativeBuildInputs, p ocispecs.Platform) (client.Reference, []byte, error) {
	built := buildEmitLLB(in, p)
	builtRef, err := solveLLB(ctx, c, built, p)
	if err != nil {
		return nil, nil, errors.Wrap(err, "solve emit graph")
	}

	plan, err := readPlan(ctx, builtRef)
	if err != nil {
		return nil, nil, err
	}
	bmJSON, err := readBuildMetadataJSON(ctx, builtRef)
	if err != nil {
		return nil, nil, err
	}
	cfgJSON, err := readEmitConfigJSON(ctx, builtRef)
	if err != nil {
		return nil, nil, err
	}
	var emitCfg emit.ImageConfig
	if err := json.Unmarshal(cfgJSON, &emitCfg); err != nil {
		return nil, nil, errors.Wrap(err, "parse emit config")
	}

	// Extract report.toml + the launch SBOM tree from the built state to the host
	// (pack --report-output-dir / --sbom-output-dir), the buildkit analog of the
	// daemon backend's CopyOutTo. Namespaced per-platform so multi-arch builds don't
	// clobber each other. Best-effort: a missing report/sbom is not fatal.
	if err := extractBuildArtifactsNBF(ctx, builtRef, in, p); err != nil {
		return nil, nil, errors.Wrap(err, "extracting sbom/report output")
	}

	runRef, err := resolvedRunImageRefNBF(ctx, builtRef, in.runImage)
	if err != nil {
		return nil, nil, err
	}
	baseImg, err := resolveImageConfigNBF(ctx, c, runRef, p)
	if err != nil {
		return nil, nil, errors.Wrap(err, "resolve run image config")
	}

	assembled := assembleFromRunImage(runRef, built, plan, p)
	assembledRef, err := solveLLB(ctx, c, assembled, p)
	if err != nil {
		return nil, nil, errors.Wrap(err, "solve assembled image")
	}

	img := *baseImg
	applyRuntimeConfigNBF(&img, &emitCfg)
	if img.Config.Labels == nil {
		img.Config.Labels = map[string]string{}
	}
	img.Config.Labels[emit.BuildMetadataLabel] = bmJSON

	// --creation-time: set the image config's `created` timestamp from
	// SOURCE_DATE_EPOCH. On the daemon backend the exporter does this via
	// WithCreatedAt, but the buildkit path runs the exporter in emit-mode (a
	// RecordingImage that never pushes), so the timestamp would otherwise be lost —
	// finalize only rewrites labels and preserves whatever `created` we set here.
	// Gated on Platform API >= 0.9, matching the daemon backend's AtLeast("0.9").
	if in.sourceDateEpoch != "" && platformAPIAtLeastNBF(in.platformAPI, "0.9") {
		if secs, perr := strconv.ParseInt(in.sourceDateEpoch, 10, 64); perr == nil {
			t := time.Unix(secs, 0).UTC()
			img.Created = &t
		}
	}

	cfgOut, err := json.Marshal(img)
	if err != nil {
		return nil, nil, errors.Wrap(err, "marshal image config")
	}
	return assembledRef, cfgOut, nil
}

// assembleFromRunImage builds the final image state: FROM run-image + one llb.Copy
// per NEW layer, sourced from the layer's emitted filesystem source in the built
// state (buildpack/app/launcher) or from the small persisted tree (synthesized
// layers like process-types). Reused run-image layers are already in the base.
func assembleFromRunImage(runRef string, built llb.State, plan emit.Plan, p ocispecs.Platform) llb.State {
	state := llb.Image(runRef, llb.Platform(p))
	plat := platformLabel(p)
	for _, layer := range plan.Layers {
		if layer.Reused {
			continue
		}
		switch {
		case layer.Source != nil && (layer.Source.Dir != "" || layer.Source.File != ""):
			state = copyFromSource(state, built, layer, plat)
		case layer.TarPath != "":
			// Synthesized layer (e.g. process-types): copy from the small persisted
			// tree. The emit step persisted a tar; for the MVP the tree copy path
			// reads the extracted tree the emit step also lays down next to it.
			// (Process-types is symlinks-only; a tar copy of a tiny tree.)
			state = copyFromTree(state, built, layer, plat)
		}
	}
	return state
}

// copyFromSource adds one layer by copying the layer's files from the built state's
// source path onto the assembly base, applying the emitted uid/gid (and mode/dest).
func copyFromSource(state, built llb.State, layer emit.LayerOp, plat string) llb.State {
	src := layer.Source
	chown := &llb.ChownOpt{User: &llb.UserOpt{UID: src.UID}, Group: &llb.UserOpt{UID: src.GID}}
	info := &llb.CopyInfo{
		CreateDestPath: true,
		ChownOpt:       chown,
	}
	from := src.Dir
	dest := src.Dir
	if src.File != "" {
		from = src.File
		dest = src.Dest
		if dest == "" {
			dest = src.File
		}
		if src.Mode != 0 {
			info.Mode = &llb.ChmodOpt{Mode: os.FileMode(src.Mode)}
		}
	} else {
		// Directory copy. For app slices, restrict to the exact files this slice
		// contains; otherwise copy the whole dir's contents.
		info.CopyDirContentsOnly = true
		if len(src.Include) > 0 {
			info.IncludePatterns = src.Include
		}
		if src.Dest != "" {
			dest = src.Dest
		}
	}
	return state.File(
		llb.Copy(built, from, dest, info),
		llb.WithCustomNamef("[%s] assemble layer (copy): %s", plat, layer.ID),
	)
}

// copyFromTree adds one synthesized layer by copying its small persisted tree.
// The emit step persisted the layer as a tar under <emitDir>/buildkit/layers/;
// for a copy-based assembly we read the extracted tree the emit step lays down at
// the same path with a ".d" suffix. (Process-types etc. are tiny.)
func copyFromTree(state, built llb.State, layer emit.LayerOp, plat string) llb.State {
	// layer.TarPath is relative to the emit output root (e.g.
	// "buildkit/layers/NNN-name.tar"); the corresponding extracted tree is at
	// "<same>.d". Both live under emitDirNBF in the built state.
	treeRel := strings.TrimSuffix(layer.TarPath, ".tar") + ".d"
	src := path.Join(emitDirNBF, treeRel)
	return state.File(
		llb.Copy(built, src, "/", &llb.CopyInfo{CreateDestPath: true, CopyDirContentsOnly: true, AllowWildcard: true}),
		llb.WithCustomNamef("[%s] assemble layer (tree): %s", plat, layer.ID),
	)
}

// --- reads from the solved emit state (gateway Reference) ---

func readPlan(ctx context.Context, ref client.Reference) (emit.Plan, error) {
	p := path.Join(emitDirNBF, emit.RecorderDir, emit.PlanFileName)
	data, err := ref.ReadFile(ctx, client.ReadRequest{Filename: p})
	if err != nil {
		return emit.Plan{}, errors.Wrapf(err, "read %s", p)
	}
	var plan emit.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return emit.Plan{}, errors.Wrapf(err, "parse %s", p)
	}
	if plan.Schema != emit.Schema {
		return emit.Plan{}, fmt.Errorf("emit plan schema %q unsupported (want %q)", plan.Schema, emit.Schema)
	}
	if len(plan.Layers) == 0 {
		return emit.Plan{}, fmt.Errorf("emit plan has no layers")
	}
	return plan, nil
}

func readBuildMetadataJSON(ctx context.Context, ref client.Reference) (string, error) {
	p := path.Join(emitDirNBF, emit.RecorderDir, emit.BuildMetadataFileName)
	data, err := ref.ReadFile(ctx, client.ReadRequest{Filename: p})
	if err != nil {
		return "", errors.Wrapf(err, "read %s", p)
	}
	if _, err := emit.ParseBuildMetadata(string(data)); err != nil {
		return "", err
	}
	return string(data), nil
}

func readEmitConfigJSON(ctx context.Context, ref client.Reference) ([]byte, error) {
	p := path.Join(emitDirNBF, emit.RecorderDir, emit.ConfigFileName)
	data, err := ref.ReadFile(ctx, client.ReadRequest{Filename: p})
	if err != nil {
		return nil, errors.Wrapf(err, "read %s", p)
	}
	return data, nil
}

// resolvedRunImageRefNBF prefers the analyzer-resolved (digest-pinned) run image
// from /layers/analyzed.toml; falls back to normalizing the raw option.
func resolvedRunImageRefNBF(ctx context.Context, builtRef client.Reference, rawRunImage string) (string, error) {
	data, err := builtRef.ReadFile(ctx, client.ReadRequest{Filename: "/layers/analyzed.toml"})
	if err == nil {
		var analyzed files.Analyzed
		if _, derr := toml.Decode(string(data), &analyzed); derr == nil {
			if ref := analyzed.RunImageRef(); ref != "" {
				return normalizeImageRefNBF(ref), nil
			}
		}
	}
	if rawRunImage == "" {
		return "", fmt.Errorf("no run image reference available (analyzed.toml missing and no run-image option)")
	}
	return normalizeImageRefNBF(rawRunImage), nil
}

func normalizeImageRefNBF(ref string) string {
	named, err := ggcrname.ParseReference(ref, ggcrname.WeakValidation)
	if err != nil {
		return ref
	}
	return named.Name()
}

func resolveImageConfigNBF(ctx context.Context, c client.Client, ref string, p ocispecs.Platform) (*dockerspec.DockerOCIImage, error) {
	_, _, data, err := c.ResolveImageConfig(ctx, ref, sourceresolver.Opt{
		ImageOpt: &sourceresolver.ResolveImageOpt{Platform: &p},
	})
	if err != nil {
		return nil, err
	}
	var img dockerspec.DockerOCIImage
	if err := json.Unmarshal(data, &img); err != nil {
		return nil, err
	}
	return &img, nil
}

func solveLLB(ctx context.Context, c client.Client, st llb.State, p ocispecs.Platform) (client.Reference, error) {
	def, err := st.Marshal(ctx, llb.Platform(p))
	if err != nil {
		return nil, errors.Wrap(err, "marshal state")
	}
	r, err := c.Solve(ctx, client.SolveRequest{Definition: def.ToPB()})
	if err != nil {
		return nil, errors.Wrap(err, "solve")
	}
	return r.SingleRef()
}

// applyRuntimeConfigNBF overlays ONLY the emitted runtime config (entrypoint/cmd/
// workingdir/env) onto the image config. The CNB metadata labels are authored by
// finalize; the build phase carries them in the build-metadata label instead.
func applyRuntimeConfigNBF(img *dockerspec.DockerOCIImage, ic *emit.ImageConfig) {
	img.Config.Entrypoint = ic.Entrypoint
	img.Config.Cmd = ic.Cmd
	img.Config.WorkingDir = ic.WorkingDir
	img.Config.Env = mergeEnvNBF(img.Config.Env, ic.Env)
}

func mergeEnvNBF(base []string, overlay map[string]string) []string {
	out := make([]string, 0, len(base)+len(overlay))
	seen := map[string]bool{}
	for k, v := range overlay {
		out = append(out, k+"="+v)
		seen[k] = true
	}
	for _, e := range base {
		k := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			k = e[:i]
		}
		if !seen[k] {
			out = append(out, e)
		}
	}
	return out
}

// buildEmitLLB constructs the LLB that runs the lifecycle phases + exporter
// emit-mode, producing /emit/buildkit/{plan.json,config.json,build-metadata.json}
// and the per-layer sources under /layers + /workspace in the built state.
// platformLabel renders a platform as "os/arch[/variant]" for use as a per-vertex
// progress-name prefix, so multi-platform solves show which architecture each
// operation runs on (e.g. "[linux/arm64] lifecycle: analyzer").
func platformLabel(p ocispecs.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func buildEmitLLB(in nativeBuildInputs, p ocispecs.Platform) llb.State {
	base := llb.Image(in.builderImage, llb.Platform(p))
	plat := platformLabel(p) // e.g. "linux/arm64" — prefixed onto every vertex name below

	if in.lifecycleImage != "" {
		lc := llb.Image(in.lifecycleImage, llb.Platform(p))
		base = base.Run(
			llb.Args([]string{"/bin/sh", "-c", "rm -rf /cnb/lifecycle"}),
			llb.WithCustomNamef("[%s] remove existing lifecycle", plat),
		).Root()
		base = base.File(
			llb.Copy(lc, "/cnb/lifecycle", "/cnb/lifecycle", &llb.CopyInfo{CreateDestPath: true}),
			llb.WithCustomNamef("[%s] overlay emit-capable lifecycle", plat),
		)
	}

	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "mkdir -p /cache /layers /platform " + emitDirNBF + " && chmod -R 777 /cache /layers " + emitDirNBF}),
		llb.WithCustomNamef("[%s] setup directories", plat),
	).Root()

	if in.orderTOML != "" {
		orderCmd := fmt.Sprintf("cat > /cnb/order.toml << 'TOML'\n%s\nTOML", in.orderTOML)
		base = base.Run(
			llb.Args([]string{"/bin/bash", "-c", orderCmd}),
			llb.WithCustomNamef("[%s] write order.toml", plat),
			llb.User("0:0"),
		).Root()
	}

	// Extra buildpack modules (--buildpack / --pre-buildpack / --post-buildpack):
	// the staged tree is rooted at /cnb/buildpacks/... so copying it over the
	// builder's /cnb/buildpacks adds new modules and OVERRIDES same-id/version ones
	// (the "test a local buildpack change" use case). This runs before detect, and
	// only mutates the transient builder state — the final image is assembled FROM
	// the run image, so these modules never enter the output. Copying (vs mounting)
	// is intentional: the modules must persist through the detect/build RUNs.
	if in.hasExtraBuildpacks {
		bpSrc := llb.Local(extraBuildpacksLocalName)
		base = base.File(
			// CopyDirContentsOnly is REQUIRED: /cnb/buildpacks already exists in the
			// builder, and BuildKit's Copy otherwise nests the source dir UNDER the
			// destination (yielding /cnb/buildpacks/buildpacks/...). Copying contents
			// merges the staged {id}/{version}/* trees INTO the builder's existing
			// /cnb/buildpacks so the order.toml references resolve.
			llb.Copy(bpSrc, "/cnb/buildpacks", "/cnb/buildpacks", &llb.CopyInfo{
				CreateDestPath:      true,
				AllowWildcard:       true,
				AllowEmptyWildcard:  true,
				CopyDirContentsOnly: true,
			}),
			llb.WithCustomNamef("[%s] add user buildpacks", plat),
		)
	}

	// Write the user-supplied build-time env vars as files under /platform/env/<NAME>
	// (CNB platform contract). The lifecycle build phase reads these and exposes them
	// to buildpacks, so BP_* configuration (e.g. BP_CPYTHON_VERSION, BP_JVM_VERSION)
	// works exactly as it does with standard pack. File contents are the raw value;
	// default modifier is overwrite/append per the CNB spec. Deterministic key order
	// keeps the LLB stable for cache reuse.
	if len(in.buildEnv) > 0 {
		keys := make([]string, 0, len(in.buildEnv))
		for k := range in.buildEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			base = base.File(
				llb.Mkfile(path.Join("/platform/env", k), 0644, []byte(in.buildEnv[k])),
				llb.WithCustomNamef("[%s] platform env: %s", plat, k),
			)
		}
	}

	appSrc := llb.Local(contextLocalName)
	base = base.File(
		llb.Copy(appSrc, "/", in.workspace, &llb.CopyInfo{CreateDestPath: true, AllowWildcard: true, AllowEmptyWildcard: true}),
		llb.WithCustomNamef("[%s] copy app source", plat),
	)
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod -R 777 " + in.workspace}),
		llb.WithCustomNamef("[%s] fix workspace permissions", plat),
	).Root()

	cacheMount := llb.AddMount("/cache", llb.Scratch(), llb.AsPersistentCacheDir("cnb-buildpacks-cache-"+p.Architecture, llb.CacheMountShared))

	cnbUser := fmt.Sprintf("%d:%d", in.uid, in.gid)
	env := []llb.RunOption{
		llb.AddEnv("CNB_PLATFORM_API", in.platformAPI),
		llb.AddEnv("CNB_USER_ID", fmt.Sprintf("%d", in.uid)),
		llb.AddEnv("CNB_GROUP_ID", fmt.Sprintf("%d", in.gid)),
		// Advertise the TARGET os/arch to the lifecycle + buildpacks. Without these,
		// packit's postal dependency resolver (used by buildpacks like cpython) falls
		// back to runtime.GOOS/GOARCH, and — more importantly — combined with a missing
		// CNB_STACK_ID it filters out stack/target-specific PREBUILT dependencies and
		// selects the wildcard-stack SOURCE dependency instead, forcing a
		// compile-from-source (e.g. CPython built from source, ~90s) instead of
		// extracting a prebuilt binary. Set both the modern target vars and the stack id.
		llb.AddEnv("CNB_TARGET_OS", p.OS),
		llb.AddEnv("CNB_TARGET_ARCH", p.Architecture),
		llb.User(cnbUser),
		llb.Network(pb.NetMode_HOST),
	}
	if p.Variant != "" {
		env = append(env, llb.AddEnv("CNB_TARGET_ARCH_VARIANT", p.Variant))
	}
	if in.stackID != "" {
		env = append(env, llb.AddEnv("CNB_STACK_ID", in.stackID))
	}
	if in.targetDistroName != "" {
		env = append(env, llb.AddEnv("CNB_TARGET_DISTRO_NAME", in.targetDistroName))
	}
	if in.targetDistroVersion != "" {
		env = append(env, llb.AddEnv("CNB_TARGET_DISTRO_VERSION", in.targetDistroVersion))
	}
	if in.experimentalMode != "" {
		env = append(env, llb.AddEnv("CNB_EXPERIMENTAL_MODE", in.experimentalMode))
	}
	if in.sourceDateEpoch != "" {
		env = append(env, llb.AddEnv("SOURCE_DATE_EPOCH", in.sourceDateEpoch))
	}
	// CNB_EXEC_ENV (pack --exec-env) is a Platform API >= 0.15 feature: the lifecycle
	// exposes it to detect/build so buildpacks can behave per-environment and
	// detection can filter by a buildpack's declared exec-env list. Only set it on a
	// qualifying platform API, matching the daemon backend's If(AtLeast("0.15")) gate.
	if in.execEnv != "" && platformAPIAtLeastNBF(in.platformAPI, "0.15") {
		env = append(env, llb.AddEnv("CNB_EXEC_ENV", in.execEnv))
	}
	// Proxy vars (both UPPER and lower case), matching standard pack's
	// WithLifecycleProxy, so buildpacks that download dependencies work behind a proxy.
	if in.httpProxy != "" {
		env = append(env, llb.AddEnv("HTTP_PROXY", in.httpProxy), llb.AddEnv("http_proxy", in.httpProxy))
	}
	if in.httpsProxy != "" {
		env = append(env, llb.AddEnv("HTTPS_PROXY", in.httpsProxy), llb.AddEnv("https_proxy", in.httpsProxy))
	}
	if in.noProxy != "" {
		env = append(env, llb.AddEnv("NO_PROXY", in.noProxy), llb.AddEnv("no_proxy", in.noProxy))
	}
	if in.registryAuth != "" {
		env = append(env, llb.AddEnv("CNB_REGISTRY_AUTH", in.registryAuth))
	}

	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod 777 /cache"}),
		llb.WithCustomNamef("[%s] fix cache mount permissions", plat), cacheMount, llb.IgnoreCache,
	).Root()

	skipChown := []string{"-skip-chown", "-uid", fmt.Sprintf("%d", in.uid), "-gid", fmt.Sprintf("%d", in.gid)}
	insecure := insecureRegistryArgsNBF(in.insecureRegistries)

	// CNB service bindings: mount each host dir READ-ONLY at /platform/bindings/<name>
	// on the phases that read them (detector + builder). Mounted, not copied, so the
	// binding data (incl. secrets) is present during the RUN but never captured in a
	// layer or the assembled image.
	var bindingMounts []llb.RunOption
	for _, b := range in.bindings {
		bindingMounts = append(bindingMounts, llb.AddMount(
			path.Join("/platform/bindings", b.Name),
			llb.Local(bindingLocalName(b.Name)),
			llb.Readonly,
		))
	}

	analyzerArgs := append([]string{"/cnb/lifecycle/analyzer"}, skipChown...)
	analyzerArgs = append(analyzerArgs, insecure...)
	analyzerArgs = append(analyzerArgs, "-run-image", in.runImage, "-layers", "/layers", in.imageName)
	base = base.Run(append([]llb.RunOption{llb.Args(analyzerArgs), llb.WithCustomNamef("[%s] lifecycle: analyzer", plat), cacheMount}, env...)...).Root()

	detectorOpts := append([]llb.RunOption{llb.Args([]string{"/cnb/lifecycle/detector", "-app", in.workspace, "-layers", "/layers"}), llb.WithCustomNamef("[%s] lifecycle: detector", plat)}, env...)
	detectorOpts = append(detectorOpts, bindingMounts...)
	base = base.Run(detectorOpts...).Root()

	restorerArgs := append([]string{"/cnb/lifecycle/restorer"}, skipChown...)
	restorerArgs = append(restorerArgs, "-layers", "/layers")
	base = base.Run(append([]llb.RunOption{llb.Args(restorerArgs), llb.WithCustomNamef("[%s] lifecycle: restorer", plat), cacheMount}, env...)...).Root()

	builderOpts := append([]llb.RunOption{llb.Args([]string{"/cnb/lifecycle/builder", "-app", in.workspace, "-layers", "/layers"}), llb.WithCustomNamef("[%s] lifecycle: builder", plat)}, env...)
	builderOpts = append(builderOpts, bindingMounts...)
	base = base.Run(builderOpts...).Root()

	exporterArgs := append([]string{"/cnb/lifecycle/exporter"}, skipChown...)
	exporterArgs = append(exporterArgs, insecure...)
	if in.defaultProcessType != "" {
		exporterArgs = append(exporterArgs, "-process-type", in.defaultProcessType)
	}
	// Pin the report path so the backend can extract it (pack --report-output-dir).
	// The exporter writes report.toml here even in emit-mode.
	exporterArgs = append(exporterArgs, "-report", reportTOMLPathNBF)
	exporterArgs = append(exporterArgs, "-layers", "/layers", "-app", in.workspace, "-emit-export-plan", emitDirNBF, in.imageName)
	// Additional tags: the exporter takes args[1:] as extra names to record. In
	// emit-mode the RecordingImage ignores them (nothing to push), so they carry no
	// effect here beyond being recorded; the backend applies them at publish time via
	// exporterImageAttrs + finalize. Passing them keeps the exporter's report/plan
	// consistent with the daemon path.
	exporterArgs = append(exporterArgs, in.additionalTags...)
	base = base.Run(append([]llb.RunOption{llb.Args(exporterArgs), llb.WithCustomNamef("[%s] lifecycle: exporter (emit-mode)", plat), cacheMount}, env...)...).Root()

	return base
}

func insecureRegistryArgsNBF(regs []string) []string {
	var out []string
	for _, r := range regs {
		out = append(out, "-insecure-registry", r)
	}
	return out
}

// platformAPIAtLeastNBF reports whether the given platform API string is >= min.
// A malformed/empty version is treated as "not at least" (conservative).
func platformAPIAtLeastNBF(platformAPI, min string) bool {
	v, err := api.NewVersion(platformAPI)
	if err != nil {
		return false
	}
	return v.AtLeast(min)
}

// extractBuildArtifactsNBF reads report.toml and the launch SBOM tree out of the
// built LLB state and writes them to the host destination dirs (pack
// --report-output-dir / --sbom-output-dir). It is the buildkit analog of the daemon
// backend's CopyOutTo. For multi-arch builds each platform's output is namespaced
// under <dest>/<os>-<arch>/ so platforms don't clobber each other; single-arch
// writes directly to <dest>. Missing artifacts are not fatal (a build may legitimately
// produce no SBOM), matching the daemon backend's best-effort copy-out.
func extractBuildArtifactsNBF(ctx context.Context, ref client.Reference, in nativeBuildInputs, p ocispecs.Platform) error {
	multi := len(in.platforms) > 1
	subdir := ""
	if multi {
		subdir = fmt.Sprintf("%s-%s", p.OS, p.Architecture)
	}

	if in.reportDestDir != "" {
		data, err := ref.ReadFile(ctx, client.ReadRequest{Filename: reportTOMLPathNBF})
		if err == nil {
			dest := filepath.Join(in.reportDestDir, subdir, "report.toml")
			if werr := writeHostFileNBF(dest, data, 0644); werr != nil {
				return werr
			}
		}
		// A missing report.toml is tolerated (best-effort, like the daemon copy-out).
	}

	if in.sbomDestDir != "" {
		destRoot := filepath.Join(in.sbomDestDir, subdir)
		if err := copyRefTreeToHostNBF(ctx, ref, sbomDirNBF, destRoot); err != nil {
			return err
		}
	}
	return nil
}

// copyRefTreeToHostNBF recursively copies a directory tree from the built LLB state
// (srcDir) to a host directory (destDir), using the gateway ReadDir/ReadFile API. A
// missing srcDir is tolerated (no SBOM produced). Directory structure and file bytes
// are preserved; permissions are normalized to 0644/0755.
func copyRefTreeToHostNBF(ctx context.Context, ref client.Reference, srcDir, destDir string) error {
	entries, err := ref.ReadDir(ctx, client.ReadDirRequest{Path: srcDir})
	if err != nil {
		// srcDir absent -> nothing to extract.
		return nil
	}
	for _, e := range entries {
		name := path.Base(e.Path)
		srcPath := path.Join(srcDir, name)
		destPath := filepath.Join(destDir, name)
		if os.FileMode(e.Mode).IsDir() {
			if rerr := copyRefTreeToHostNBF(ctx, ref, srcPath, destPath); rerr != nil {
				return rerr
			}
			continue
		}
		data, rerr := ref.ReadFile(ctx, client.ReadRequest{Filename: srcPath})
		if rerr != nil {
			return errors.Wrapf(rerr, "reading %s from build", srcPath)
		}
		if werr := writeHostFileNBF(destPath, data, 0644); werr != nil {
			return werr
		}
	}
	return nil
}

// writeHostFileNBF writes data to a host path, creating parent dirs.
func writeHostFileNBF(dest string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return errors.Wrapf(err, "creating output dir for %s", dest)
	}
	if err := os.WriteFile(dest, data, mode); err != nil {
		return errors.Wrapf(err, "writing %s", dest)
	}
	return nil
}
