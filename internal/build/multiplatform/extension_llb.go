package multiplatform

import (
	"context"
	"fmt"
	"path"
	"sort"
	"time"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/gateway/client"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// This file implements the BuildKit-native CNB image extension support (generator phase +
// run/build image extend), kaniko-free, per spec buildkit-extension-support. The Dockerfile
// -> LLB translation lives in dockerfile_llb.go; here we wire the generator phase into the
// emit graph, stage /cnb/extensions per-arch, and (Stage 1) apply a generated run.Dockerfile
// to the run image at assembly time.

// generatedDirNBF is where the detector (given -generated) writes build.Dockerfile /
// run.Dockerfile per extension under extensions (CNB: <layers>/generated/<ext-id>/...).
// Generation runs INSIDE the detector phase — there is no standalone generator binary in
// the bundled lifecycle image — mirroring the daemon Detect(). It lives under /layers so it
// persists in the solved emit state and can be read back via the gateway Reference.
const generatedDirNBF = "/layers/generated"

// extensionsLocalName is the llb.Local key for PLATFORM-AGNOSTIC extra extensions
// (inline/local/single-manifest), staged once and copied to every leg's /cnb/extensions —
// mirroring extraBuildpacksLocalName for buildpacks.
const extensionsLocalName = "cnb-extra-extensions"

// stageExtensionsLLB copies extension modules onto the builder state's /cnb/extensions for
// one platform leg, mirroring the buildpack delivery in buildEmitLLB: platform-agnostic
// extensions from a single staged local (same tree on every leg), and multi-arch extension
// images pulled as their per-platform child. Returns the state with /cnb/extensions
// populated. A no-op (returns base unchanged) when there are no extensions.
func stageExtensionsLLB(base llb.State, in nativeBuildInputs, p ocispecs.Platform, plat string, extensionImageRefs []string) llb.State {
	if in.hasAgnosticExtensions {
		agSrc := llb.Local(extensionsLocalName)
		base = base.File(
			llb.Copy(agSrc, "/cnb/extensions", "/cnb/extensions", &llb.CopyInfo{
				CreateDestPath:      true,
				AllowWildcard:       true,
				AllowEmptyWildcard:  true,
				CopyDirContentsOnly: true,
			}),
			llb.WithCustomNamef("[%s] add platform-agnostic extensions", plat),
		)
	}
	for i, exRef := range extensionImageRefs {
		exSrc := llb.Image(exRef, llb.Platform(p))
		base = base.File(
			llb.Copy(exSrc, "/cnb/extensions", "/cnb/extensions", &llb.CopyInfo{
				CreateDestPath:      true,
				AllowWildcard:       true,
				AllowEmptyWildcard:  true,
				CopyDirContentsOnly: true,
			}),
			llb.WithCustomNamef("[%s] add user extension %d/%d", plat, i+1, len(extensionImageRefs)),
		)
	}
	return base
}

// extensionsEnabled reports whether the generation/extend phases should run: the build has
// extensions in the order AND the platform API supports extension generation (>= 0.13).
// Generation itself is driven by the detector's -generated flag (see native_buildfunc.go);
// there is no standalone generator vertex.
func extensionsEnabled(in nativeBuildInputs) bool {
	return in.hasExtensions && platformAPIAtLeastNBF(in.platformAPI, "0.13")
}

// discoverGeneratedFromRef enumerates generatedDirNBF in a SOLVED gateway reference and
// reads the generated run/build Dockerfiles + their content. It is the gateway-Reference
// analog of the host-filesystem build.DiscoverGeneratedLayout: the buildkit path has no
// host tmpDir mid-graph, so it reads the generated tree out of the solved emit state via
// ref.ReadDir / ref.ReadFile. Returns run and build Dockerfiles (ext-id, content, context
// subdir name) in extension-id order. Missing generated dir => empty (no error).
func discoverGeneratedFromRef(ctx context.Context, ref client.Reference) (run, build []refDockerfile, err error) {
	entries, err := ref.ReadDir(ctx, client.ReadDirRequest{Path: generatedDirNBF})
	if err != nil {
		// A build with extensions that produced nothing (or older layout) — treat as none.
		return nil, nil, nil
	}
	var extIDs []string
	for _, e := range entries {
		name := path.Base(e.GetPath())
		if name == "build" || name == "run" || name == "" {
			continue // older <generated>/build|run layout not produced on API >= 0.13
		}
		if e.IsDir() {
			extIDs = append(extIDs, name)
		}
	}
	sort.Strings(extIDs) // deterministic order (group order is not preserved by ReadDir)
	for _, extID := range extIDs {
		extDir := path.Join(generatedDirNBF, extID)
		if df, ok := readRefDockerfile(ctx, ref, extDir, extID, "run.Dockerfile", "context.run"); ok {
			run = append(run, df)
		}
		if df, ok := readRefDockerfile(ctx, ref, extDir, extID, "build.Dockerfile", "context.build"); ok {
			build = append(build, df)
		}
	}
	return run, build, nil
}

// refDockerfile is a generated Dockerfile read out of a solved gateway reference: its
// extension id, raw content, and the resolved in-state context directory (absolute path
// under generatedDirNBF, or "" to mean the app dir default).
type refDockerfile struct {
	extID      string
	content    []byte
	contextDir string
}

// readRefDockerfile reads <extDir>/<name> from the ref if present, resolving the context
// dir per CNB precedence (image-specific folder, else shared context, else "" = app).
func readRefDockerfile(ctx context.Context, ref client.Reference, extDir, extID, name, specificCtx string) (refDockerfile, bool) {
	content, err := ref.ReadFile(ctx, client.ReadRequest{Filename: path.Join(extDir, name)})
	if err != nil {
		return refDockerfile{}, false
	}
	return refDockerfile{
		extID:      extID,
		content:    content,
		contextDir: resolveRefContextDir(ctx, ref, extDir, specificCtx),
	}, true
}

// resolveRefContextDir returns the in-state context dir for a generated Dockerfile:
// <extDir>/<specificCtx> if present, else <extDir>/context, else "".
func resolveRefContextDir(ctx context.Context, ref client.Reference, extDir, specificCtx string) string {
	for _, candidate := range []string{specificCtx, "context"} {
		p := path.Join(extDir, candidate)
		if _, err := ref.StatFile(ctx, client.StatRequest{Path: p}); err == nil {
			return p
		}
	}
	return ""
}

// extendRunImageLLB applies the generated run.Dockerfiles (Stage 1: run-image extend) to the
// run image state for one platform leg, in extension-id order. Each Dockerfile is translated
// to LLB (dockerfile_llb.go) with base_image/build_id/user_id/group_id build args. Returns
// the extended run image state + merged image-config mutations (labels incl.
// io.buildpacks.rebasable, env, user, workdir). When there are no run Dockerfiles, returns
// the base unchanged and an empty config.
func extendRunImageLLB(runBase llb.State, runDockerfiles []refDockerfile, buildID, userID, groupID, plat string, appSrc llb.State) (llb.State, extImageConfig, error) {
	merged := extImageConfig{env: map[string]string{}, labels: map[string]string{}}
	state := runBase
	for _, df := range runDockerfiles {
		parsed, err := parseExtensionDockerfile(df.content)
		if err != nil {
			return state, merged, errors.Wrapf(err, "extension %q run.Dockerfile", df.extID)
		}
		ctxSrc := appSrc
		// A context folder would be sourced from the solved emit state; Stage 1 supports
		// the common no-context (app default) case. (Stage 2: mount df.contextDir from the
		// generate state as the COPY source.)
		out, cfg, err := parsed.toLLB(extendInput{
			base:       state,
			contextSrc: ctxSrc,
			buildArgs: map[string]string{
				"base_image": "run", // FROM ${base_image} is a no-op; base is `state`
				"build_id":   buildID,
				"user_id":    userID,
				"group_id":   groupID,
			},
			plat: plat,
		})
		if err != nil {
			return state, merged, errors.Wrapf(err, "extension %q run.Dockerfile -> LLB", df.extID)
		}
		state = out
		for k, v := range cfg.env {
			merged.env[k] = v
		}
		for k, v := range cfg.labels {
			merged.labels[k] = v
		}
		if cfg.user != "" {
			merged.user = cfg.user
		}
		if cfg.workdir != "" {
			merged.workdir = cfg.workdir
		}
	}
	return state, merged, nil
}

// extendBuildImageLLB applies the generated build.Dockerfiles (Stage 2: build-image extend,
// FR-3) to the builder image state for one platform leg, in extension-id order. It mirrors
// extendRunImageLLB: each Dockerfile is translated to LLB (dockerfile_llb.go) with
// base_image/build_id/user_id/group_id build args, and each FROM ${base_image} resolves to
// the intermediate produced by the previous one (the loop threads `state`). The returned
// state becomes the builder base the `builder` lifecycle phase runs on, so buildpacks see the
// extension's added OS packages. Kaniko-free — no volume build cache is required. When there
// are no build Dockerfiles, returns the base unchanged and an empty config.
//
// The returned extImageConfig (ENV/LABEL/USER/WORKDIR the build.Dockerfile set) is currently
// consumed only insofar as it shapes the builder state used for the `builder` phase; unlike
// the run-image extend, build-image config mutations do not flow to the FINAL image config
// (the final image is assembled FROM the run image), so the caller may ignore it.
func extendBuildImageLLB(builderBase llb.State, buildDockerfiles []refDockerfile, buildID, userID, groupID, plat string, contextSrc llb.State) (llb.State, extImageConfig, error) {
	merged := extImageConfig{env: map[string]string{}, labels: map[string]string{}}
	state := builderBase
	for _, df := range buildDockerfiles {
		parsed, err := parseExtensionDockerfile(df.content)
		if err != nil {
			return state, merged, errors.Wrapf(err, "extension %q build.Dockerfile", df.extID)
		}
		// A context folder would be sourced from the solved generate state; Stage 1 for
		// build-image extend supports the common no-context (app default) case, matching
		// extendRunImageLLB. (Stage 2: mount df.contextDir — context.build|context — from the
		// generate state as the COPY/ADD source.)
		out, cfg, err := parsed.toLLB(extendInput{
			base:       state,
			contextSrc: contextSrc,
			buildArgs: map[string]string{
				"base_image": "build", // FROM ${base_image} is a no-op; base is `state`
				"build_id":   buildID,
				"user_id":    userID,
				"group_id":   groupID,
			},
			plat: plat,
		})
		if err != nil {
			return state, merged, errors.Wrapf(err, "extension %q build.Dockerfile -> LLB", df.extID)
		}
		state = out
		for k, v := range cfg.env {
			merged.env[k] = v
		}
		for k, v := range cfg.labels {
			merged.labels[k] = v
		}
		if cfg.user != "" {
			merged.user = cfg.user
		}
		if cfg.workdir != "" {
			merged.workdir = cfg.workdir
		}
	}
	return state, merged, nil
}

// buildIDUUID returns a fresh build_id value for the CNB build_id build arg. It need not be
// a real UUID — any value that changes per build so $build_id-referencing RUNs rebuild.
func buildIDUUID() string {
	return fmt.Sprintf("build-%d", time.Now().UnixNano())
}

// resetRunImageExtendLLB flips `[run-image].extend = true` → `extend = false` in
// /layers/analyzed.toml AFTER the detector/generator has written it and BEFORE the restorer
// runs, so the bundled lifecycle RESTORER's run-image-extension branch (which does
// os.MkdirAll("/kaniko/...") — a root-owned path the -skip-chown -uid/-gid restorer cannot
// create) is skipped. On the buildkit path the run.Dockerfile is applied in LLB at run-image
// assembly time (extendRunImageLLB, FR-4), reading the generated tree directly — it does NOT
// depend on analyzed.toml's `extend` marker or on the restorer pulling the run image. And
// run-image RESOLUTION reads only `[run-image].reference` (resolvedRunImageRefNBF ->
// files.Analyzed.RunImageRef), never `.extend`, so clearing the marker is safe.
//
// The edit is a single targeted sed: analyzed.toml has exactly one `extend` key (under
// [run-image]), so matching the `extend = true` line is unambiguous. It no-ops when the file
// or the line is absent (`sed -i` on a matchless file changes nothing; the leading test guards
// a missing file). It runs as the detector's uid:gid so it can write the file the detector
// wrote under -skip-chown. Callers MUST guard on extensionsEnabled(in) so the no-extension
// detector→restorer graph is byte-for-byte unchanged (AC-5).
func resetRunImageExtendLLB(base llb.State, uid, gid int, plat string) llb.State {
	const script = `if [ -f /layers/analyzed.toml ]; then ` +
		`sed -i 's/^[[:space:]]*extend[[:space:]]*=[[:space:]]*true/extend = false/' /layers/analyzed.toml; fi`
	return base.Run(
		llb.Args([]string{"/bin/sh", "-c", script}),
		llb.WithCustomNamef("[%s] extensions: disable lifecycle run-image extend (applied in LLB)", plat),
		llb.User(fmt.Sprintf("%d:%d", uid, gid)),
	).Root()
}
