package multiplatform

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/docker/cli/cli/config"
	"github.com/moby/buildkit/client"
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer" // register docker-container:// scheme
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
)

// This file holds the low-level, builder-agnostic BuildKit plumbing shared by the
// buildkit backend: daemon connection, progress display, cache import/export
// parsing, and the Docker auth provider. It is intentionally separate from the
// backend build logic so a future backend (e.g. buildah-podman) can reuse or
// mirror it.

// connectToBuildkit connects to the configured buildx (docker-container) builder.
func (b *BuildkitBackend) connectToBuildkit(ctx context.Context) (*client.Client, error) {
	addr, err := b.resolveBuildkitAddr(ctx)
	if err != nil {
		return nil, err
	}
	b.logger.Debugf("Connecting to buildkit at %s", addr)
	return client.New(ctx, addr)
}

// resolveBuildkitAddr determines the buildkit daemon address for the configured
// docker-container driver builder, connecting via the docker-container:// scheme.
func (b *BuildkitBackend) resolveBuildkitAddr(ctx context.Context) (string, error) {
	builderName := b.buildkitOpts.Builder
	if builderName == "" {
		builderName = "pack-multiplatform"
	}
	containerName := fmt.Sprintf("buildx_buildkit_%s0", builderName)

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

// startProgressDisplay renders BuildKit solve status to stderr with numbered
// vertices, CACHED/DONE/ERROR markers, and durations. Returns the status channel
// to pass to client.Build/Solve.
func (b *BuildkitBackend) startProgressDisplay(prefix string) chan *client.SolveStatus {
	ch := make(chan *client.SolveStatus)
	vertexStartTimes := make(map[string]int64)
	vertexNumbers := make(map[string]int)
	// vertexArch maps a vertex digest -> its "[os/arch]" label (parsed from the
	// vertex name). Log lines reference their vertex only by number, so without
	// this they would print with no architecture and be impossible to attribute
	// in a multi-platform solve. We prepend the owning vertex's arch to every
	// log line so each line is self-describing.
	vertexArch := make(map[string]string)
	vertexCounter := 0
	// tag is " <prefix>" when a prefix is set, else "" — so an empty prefix does not
	// produce a double space. Each vertex name already carries a "[os/arch]" prefix.
	tag := ""
	if prefix != "" {
		tag = " " + prefix
	}
	go func() {
		for status := range ch {
			for _, v := range status.Vertexes {
				id := v.Digest.String()
				if v.Started != nil && vertexStartTimes[id] == 0 {
					vertexCounter++
					vertexStartTimes[id] = v.Started.UnixMilli()
					vertexNumbers[id] = vertexCounter
					vertexArch[id] = archLabelFromVertexName(v.Name)
					fmt.Fprintf(os.Stderr, "#%d%s %s\n", vertexCounter, tag, v.Name)
				}
				if v.Completed != nil {
					num := vertexNumbers[id]
					startMs := vertexStartTimes[id]
					var duration float64
					if startMs > 0 {
						duration = float64(v.Completed.UnixMilli()-startMs) / 1000.0
					}
					if v.Cached {
						fmt.Fprintf(os.Stderr, "#%d%s %s CACHED\n", num, tag, v.Name)
					} else if v.Error != "" {
						fmt.Fprintf(os.Stderr, "#%d%s %s ERROR: %s\n", num, tag, v.Name, v.Error)
					} else {
						fmt.Fprintf(os.Stderr, "#%d%s %s DONE %.1fs\n", num, tag, v.Name, duration)
					}
				}
			}
			for _, l := range status.Logs {
				id := l.Vertex.String()
				stepNum := vertexNumbers[id]
				// arch is the owning vertex's "[os/arch]" label; prepend it (with a
				// leading space) so every log line carries its architecture. When a
				// vertex has no arch label (single-platform / non-prefixed vertex),
				// this is empty and the line reads as before.
				arch := vertexArch[id]
				if arch != "" {
					arch = " " + arch
				}
				lines := strings.Split(string(l.Data), "\n")
				for _, line := range lines {
					if line != "" {
						fmt.Fprintf(os.Stderr, "#%d%s%s %s\n", stepNum, tag, arch, line)
					}
				}
			}
		}
	}()
	return ch
}

// archLabelFromVertexName extracts a leading "[os/arch]" token from a BuildKit
// vertex name (vertices are named with an "[os/arch] ..." prefix via
// platformLabel/WithCustomNamef). It returns the bracketed label including the
// brackets (e.g. "[linux/arm64]"), or "" when the name has no such prefix (for
// example internal vertices BuildKit names itself, or single-platform builds
// that don't prefix). Only a leading, well-formed "[...]" token is treated as an
// arch label so arbitrary bracketed text mid-name is not misread.
func archLabelFromVertexName(name string) string {
	if !strings.HasPrefix(name, "[") {
		return ""
	}
	end := strings.IndexByte(name, ']')
	if end <= 1 {
		return ""
	}
	inner := name[1:end]
	// A platform label always contains a "/" (os/arch[/variant]); this avoids
	// treating things like "[internal]" as an architecture.
	if !strings.Contains(inner, "/") {
		return ""
	}
	return name[:end+1]
}

// parseCacheImports converts BuildkitOpts.CacheFrom strings to CacheOptionsEntry.
func (b *BuildkitBackend) parseCacheImports() []client.CacheOptionsEntry {
	var imports []client.CacheOptionsEntry
	for _, cf := range b.buildkitOpts.CacheFrom {
		attrs := parseCacheAttrs(cf)
		imports = append(imports, client.CacheOptionsEntry{Type: attrs["type"], Attrs: attrs})
	}
	return imports
}

// parseCacheExports converts BuildkitOpts.CacheTo strings to CacheOptionsEntry.
func (b *BuildkitBackend) parseCacheExports() []client.CacheOptionsEntry {
	var exports []client.CacheOptionsEntry
	for _, ct := range b.buildkitOpts.CacheTo {
		attrs := parseCacheAttrs(ct)
		exports = append(exports, client.CacheOptionsEntry{Type: attrs["type"], Attrs: attrs})
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

// newDockerAuthProvider builds a BuildKit session auth provider seeded from the
// default Docker config, so solves authenticate with pack-resolved credentials.
func newDockerAuthProvider() session.Attachable {
	return authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: authprovider.LoadAuthConfig(config.LoadDefaultConfigFile(os.Stderr)),
	})
}
