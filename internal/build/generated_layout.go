package build

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// This file holds the SHARED discovery of the lifecycle "generated" output directory that
// the generation phase produces when image extensions participate in a build. Both the
// daemon path (hasExtensionsForBuild) and the buildkit multi-arch backend need to know
// which extensions produced a build.Dockerfile / run.Dockerfile, where the associated
// build context is, and what extend-config build args to pass. The daemon then applies the
// Dockerfiles with kaniko; the buildkit backend translates them to LLB. Only this DISCOVERY
// is shared — the application differs by construction. See spec buildkit-extension-support
// (NFR-2).

// GeneratedDockerfile describes one generated extension Dockerfile (build or run).
type GeneratedDockerfile struct {
	// ExtID is the extension id whose generate step produced this Dockerfile.
	ExtID string
	// Path is the absolute path to the build.Dockerfile / run.Dockerfile.
	Path string
	// ContextDir is the absolute path to the build context for this Dockerfile, resolved
	// per the CNB spec precedence (context.build / context.run before context). Empty
	// means "use the app dir" (the spec default when no context folder is present).
	ContextDir string
	// ExtendArgs are key/value build args from <generated>/<ext-id>/extend-config.toml
	// (provided to build.Dockerfile). Empty when there is no extend-config.toml.
	ExtendArgs map[string]string
}

// GeneratedLayout is the discovered set of generated Dockerfiles, split by kind, in
// extension id order (directory order, which the lifecycle writes in group order).
type GeneratedLayout struct {
	BuildDockerfiles []GeneratedDockerfile
	RunDockerfiles   []GeneratedDockerfile
}

// HasBuild reports whether any build.Dockerfile was generated.
func (g GeneratedLayout) HasBuild() bool { return len(g.BuildDockerfiles) > 0 }

// HasRun reports whether any run.Dockerfile was generated.
func (g GeneratedLayout) HasRun() bool { return len(g.RunDockerfiles) > 0 }

// extendConfigTOML mirrors <generated>/<ext-id>/extend-config.toml: build args passed to
// build.Dockerfile (the [build.args] table).
type extendConfigTOML struct {
	Build struct {
		Args map[string]string `toml:"args"`
	} `toml:"build"`
}

// DiscoverGeneratedLayout walks a lifecycle "generated" directory and returns the build
// and run Dockerfiles that extensions produced, with their context dirs and extend-config
// args. It handles BOTH layouts the lifecycle has used:
//
//   - newer (platform API >= 0.13): <generated>/<ext-id>/build.Dockerfile and
//     <generated>/<ext-id>/run.Dockerfile, with context folders context/ context.build/
//     context.run alongside;
//   - older: <generated>/build/<ext-id>/Dockerfile (build only).
//
// A missing generated dir is not an error — it returns an empty layout (the build simply
// has no generated Dockerfiles). Returning empty is how "no extensions participated" is
// represented.
func DiscoverGeneratedLayout(generatedDir string) (GeneratedLayout, error) {
	var layout GeneratedLayout

	// Newer layout: <generated>/<ext-id>/{build,run}.Dockerfile
	entries, err := os.ReadDir(generatedDir)
	if err != nil {
		if os.IsNotExist(err) {
			return layout, nil
		}
		return layout, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "build" || e.Name() == "run" {
			continue // "build"/"run" handled as the older layout below
		}
		extID := e.Name()
		extDir := filepath.Join(generatedDir, extID)

		if p := filepath.Join(extDir, "build.Dockerfile"); fileExists(p) {
			layout.BuildDockerfiles = append(layout.BuildDockerfiles, GeneratedDockerfile{
				ExtID:      extID,
				Path:       p,
				ContextDir: resolveContextDir(extDir, "context.build"),
				ExtendArgs: readExtendConfig(extDir),
			})
		}
		if p := filepath.Join(extDir, "run.Dockerfile"); fileExists(p) {
			layout.RunDockerfiles = append(layout.RunDockerfiles, GeneratedDockerfile{
				ExtID:      extID,
				Path:       p,
				ContextDir: resolveContextDir(extDir, "context.run"),
				ExtendArgs: readExtendConfig(extDir),
			})
		}
	}

	// Older layout (platform API < 0.13): <generated>/build/<ext-id>/Dockerfile. Match the
	// historical daemon semantics: the PRESENCE of an <ext-id> subdirectory under
	// <generated>/build indicates a build-image extension (the lifecycle populates the
	// Dockerfile inside), so record it on directory presence rather than requiring the
	// Dockerfile file to already exist — the daemon only checks HasBuild(). The Path points
	// at the conventional Dockerfile location for callers that do read it.
	oldBuild := filepath.Join(generatedDir, "build")
	if oldEntries, err := os.ReadDir(oldBuild); err == nil {
		for _, e := range oldEntries {
			if !e.IsDir() {
				continue
			}
			extID := e.Name()
			extDir := filepath.Join(oldBuild, extID)
			layout.BuildDockerfiles = append(layout.BuildDockerfiles, GeneratedDockerfile{
				ExtID:      extID,
				Path:       filepath.Join(extDir, "Dockerfile"),
				ContextDir: resolveContextDir(extDir, "context.build"),
				ExtendArgs: readExtendConfig(extDir),
			})
		}
	}

	return layout, nil
}

// resolveContextDir returns the context dir for a generated Dockerfile per the CNB
// precedence: the image-specific folder (context.build / context.run) if present, else the
// shared context/ folder, else "" (meaning the app dir default).
func resolveContextDir(extDir, specific string) string {
	if p := filepath.Join(extDir, specific); dirExists(p) {
		return p
	}
	if p := filepath.Join(extDir, "context"); dirExists(p) {
		return p
	}
	return ""
}

// readExtendConfig reads <extDir>/extend-config.toml build args, or nil if absent/unreadable.
func readExtendConfig(extDir string) map[string]string {
	p := filepath.Join(extDir, "extend-config.toml")
	if !fileExists(p) {
		return nil
	}
	var cfg extendConfigTOML
	if _, err := toml.DecodeFile(p, &cfg); err != nil {
		return nil
	}
	return cfg.Build.Args
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
