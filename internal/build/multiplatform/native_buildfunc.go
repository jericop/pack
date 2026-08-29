package multiplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

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
//   - sets the image config + the io.buildpacks.buildkit.native.build-metadata
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
}

// contextLocalName is the llb.Local key under which pack provides the app source.
const contextLocalName = "context"

const emitDirNBF = "/emit"

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
	for _, layer := range plan.Layers {
		if layer.Reused {
			continue
		}
		switch {
		case layer.Source != nil && (layer.Source.Dir != "" || layer.Source.File != ""):
			state = copyFromSource(state, built, layer)
		case layer.TarPath != "":
			// Synthesized layer (e.g. process-types): copy from the small persisted
			// tree. The emit step persisted a tar; for the MVP the tree copy path
			// reads the extracted tree the emit step also lays down next to it.
			// (Process-types is symlinks-only; a tar copy of a tiny tree.)
			state = copyFromTree(state, built, layer)
		}
	}
	return state
}

// copyFromSource adds one layer by copying the layer's files from the built state's
// source path onto the assembly base, applying the emitted uid/gid (and mode/dest).
func copyFromSource(state, built llb.State, layer emit.LayerOp) llb.State {
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
		llb.WithCustomNamef("assemble layer (copy): %s", layer.ID),
	)
}

// copyFromTree adds one synthesized layer by copying its small persisted tree.
// The emit step persisted the layer as a tar under <emitDir>/buildkit/layers/;
// for a copy-based assembly we read the extracted tree the emit step lays down at
// the same path with a ".d" suffix. (Process-types etc. are tiny.)
func copyFromTree(state, built llb.State, layer emit.LayerOp) llb.State {
	// layer.TarPath is relative to the emit output root (e.g.
	// "buildkit/layers/NNN-name.tar"); the corresponding extracted tree is at
	// "<same>.d". Both live under emitDirNBF in the built state.
	treeRel := strings.TrimSuffix(layer.TarPath, ".tar") + ".d"
	src := path.Join(emitDirNBF, treeRel)
	return state.File(
		llb.Copy(built, src, "/", &llb.CopyInfo{CreateDestPath: true, CopyDirContentsOnly: true, AllowWildcard: true}),
		llb.WithCustomNamef("assemble layer (tree): %s", layer.ID),
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
func buildEmitLLB(in nativeBuildInputs, p ocispecs.Platform) llb.State {
	base := llb.Image(in.builderImage, llb.Platform(p))

	if in.lifecycleImage != "" {
		lc := llb.Image(in.lifecycleImage, llb.Platform(p))
		base = base.Run(
			llb.Args([]string{"/bin/sh", "-c", "rm -rf /cnb/lifecycle"}),
			llb.WithCustomName("remove existing lifecycle"),
		).Root()
		base = base.File(
			llb.Copy(lc, "/cnb/lifecycle", "/cnb/lifecycle", &llb.CopyInfo{CreateDestPath: true}),
			llb.WithCustomName("overlay emit-capable lifecycle"),
		)
	}

	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "mkdir -p /cache /layers /platform " + emitDirNBF + " && chmod -R 777 /cache /layers " + emitDirNBF}),
		llb.WithCustomName("setup directories"),
	).Root()

	if in.orderTOML != "" {
		orderCmd := fmt.Sprintf("cat > /cnb/order.toml << 'TOML'\n%s\nTOML", in.orderTOML)
		base = base.Run(
			llb.Args([]string{"/bin/bash", "-c", orderCmd}),
			llb.WithCustomName("write order.toml"),
			llb.User("0:0"),
		).Root()
	}

	appSrc := llb.Local(contextLocalName)
	base = base.File(
		llb.Copy(appSrc, "/", "/workspace", &llb.CopyInfo{CreateDestPath: true, AllowWildcard: true, AllowEmptyWildcard: true}),
		llb.WithCustomName("copy app source"),
	)
	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod -R 777 /workspace"}),
		llb.WithCustomName("fix workspace permissions"),
	).Root()

	cacheMount := llb.AddMount("/cache", llb.Scratch(), llb.AsPersistentCacheDir("cnb-buildpacks-cache-"+p.Architecture, llb.CacheMountShared))

	cnbUser := fmt.Sprintf("%d:%d", in.uid, in.gid)
	env := []llb.RunOption{
		llb.AddEnv("CNB_PLATFORM_API", in.platformAPI),
		llb.AddEnv("CNB_USER_ID", fmt.Sprintf("%d", in.uid)),
		llb.AddEnv("CNB_GROUP_ID", fmt.Sprintf("%d", in.gid)),
		llb.User(cnbUser),
		llb.Network(pb.NetMode_HOST),
	}
	if in.registryAuth != "" {
		env = append(env, llb.AddEnv("CNB_REGISTRY_AUTH", in.registryAuth))
	}

	base = base.Run(
		llb.Args([]string{"/bin/sh", "-c", "chmod 777 /cache"}),
		llb.WithCustomName("fix cache mount permissions"), cacheMount, llb.IgnoreCache,
	).Root()

	skipChown := []string{"-skip-chown", "-uid", fmt.Sprintf("%d", in.uid), "-gid", fmt.Sprintf("%d", in.gid)}
	insecure := insecureRegistryArgsNBF(in.insecureRegistries)

	analyzerArgs := append([]string{"/cnb/lifecycle/analyzer"}, skipChown...)
	analyzerArgs = append(analyzerArgs, insecure...)
	analyzerArgs = append(analyzerArgs, "-run-image", in.runImage, "-layers", "/layers", in.imageName)
	base = base.Run(append([]llb.RunOption{llb.Args(analyzerArgs), llb.WithCustomName("lifecycle: analyzer"), cacheMount}, env...)...).Root()

	base = base.Run(append([]llb.RunOption{llb.Args([]string{"/cnb/lifecycle/detector", "-app", "/workspace", "-layers", "/layers"}), llb.WithCustomName("lifecycle: detector")}, env...)...).Root()

	restorerArgs := append([]string{"/cnb/lifecycle/restorer"}, skipChown...)
	restorerArgs = append(restorerArgs, "-layers", "/layers")
	base = base.Run(append([]llb.RunOption{llb.Args(restorerArgs), llb.WithCustomName("lifecycle: restorer"), cacheMount}, env...)...).Root()

	base = base.Run(append([]llb.RunOption{llb.Args([]string{"/cnb/lifecycle/builder", "-app", "/workspace", "-layers", "/layers"}), llb.WithCustomName("lifecycle: builder")}, env...)...).Root()

	exporterArgs := append([]string{"/cnb/lifecycle/exporter"}, skipChown...)
	exporterArgs = append(exporterArgs, insecure...)
	exporterArgs = append(exporterArgs, "-layers", "/layers", "-app", "/workspace", "-emit-export-plan", emitDirNBF, in.imageName)
	base = base.Run(append([]llb.RunOption{llb.Args(exporterArgs), llb.WithCustomName("lifecycle: exporter (emit-mode)"), cacheMount}, env...)...).Root()

	return base
}

func insecureRegistryArgsNBF(regs []string) []string {
	var out []string
	for _, r := range regs {
		out = append(out, "-insecure-registry", r)
	}
	return out
}
