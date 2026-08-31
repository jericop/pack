package multiplatform

import (
	"os"
	"strings"
)

// The former host-side metadata-SHA REWRITE logic that lived here is SUPERSEDED by
// Option A (build-then-finalize): the lifecycle `phase/finalize` library now AUTHORS
// the correct io.buildpacks.lifecycle.metadata on the pushed image from its actual
// produced diffIDs + the io.buildpacks.lifecycle.prepared-metadata label. Pack
// consumes finalize.Finalize in-process (see backend_native.go), so pack no longer
// hand-rewrites CNB metadata. Only the test-env registry-remap shim remains here.

// applyHostRegistryRemap rewrites the registry host of imageName for host-side
// access, using the PACK_HOST_REGISTRY_REMAP env var (format "src=dst", e.g.
// "pack-local-registry:5000=localhost:5050"). This exists ONLY to bridge local
// test setups where the buildkit-reachable registry name differs from the
// host-reachable name (the finalize step runs host-side and must reach the same
// registry BuildKit pushed to). Unset in production => no-op.
func applyHostRegistryRemap(imageName string) string {
	remap := os.Getenv("PACK_HOST_REGISTRY_REMAP")
	if remap == "" {
		return imageName
	}
	parts := strings.SplitN(remap, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return imageName
	}
	src, dst := parts[0], parts[1]
	if strings.HasPrefix(imageName, src+"/") {
		return dst + strings.TrimPrefix(imageName, src)
	}
	return imageName
}
