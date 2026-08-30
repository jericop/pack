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
