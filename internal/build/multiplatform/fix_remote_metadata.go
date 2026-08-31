package multiplatform

import (
	"context"
	"fmt"

	"github.com/buildpacks/lifecycle/phase/finalize"

	"github.com/buildpacks/pack/pkg/logging"
)

// FixRemoteImageMetadata is the SELF-HEALING entry point for the buildkit-native
// backend. Given an image reference that BuildKit already built and pushed while
// still carrying the io.buildpacks.lifecycle.prepared-metadata label (e.g. a
// build whose post-push finalize was interrupted, or an image intentionally left
// un-finalized), it re-runs the lifecycle finalize step against the REMOTE image
// WITHOUT rebuilding: it authors the correct io.buildpacks.lifecycle.metadata from
// the image's actual produced diffIDs + that label and re-pushes config+manifest
// (+ index) only. No layer blobs are read, added, or re-uploaded.
//
// It is idempotent: finalizing an already-finalized image (build-metadata label
// absent) is a no-op. It transparently handles a single image or a manifest list.
//
// This mirrors the in-process finalize that backend_native.go runs after a normal
// build; the difference is only that here there is no build — it operates purely
// on the remote reference. The same host-registry remap + insecure detection are
// applied so it works in local test setups (PACK_HOST_REGISTRY_REMAP) and against
// plain-HTTP local registries.
func FixRemoteImageMetadata(ctx context.Context, logger logging.Logger, imageName string, keepBuildMetadataLabel bool) error {
	ref := applyHostRegistryRemap(imageName)
	insecure := false
	if reg := registryHost(ref); reg != "" && isLikelyInsecureRegistry(reg) {
		insecure = true
	}
	logger.Infof("Fixing (finalizing) CNB metadata for existing image %s", ref)
	if err := finalize.Finalize(ctx, ref, finalize.Options{
		Insecure:               insecure,
		KeepBuildMetadataLabel: keepBuildMetadataLabel,
		Logger:                 logger,
	}); err != nil {
		return fmt.Errorf("fixing CNB metadata for %s: %w", ref, err)
	}
	return nil
}
