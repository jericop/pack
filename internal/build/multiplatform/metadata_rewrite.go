package multiplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/buildpacks/lifecycle/buildkit/cnbfrontend"
	"github.com/google/go-containerregistry/pkg/authn"
	ggcrname "github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/buildpacks/pack/pkg/logging"
)

// lifecycleMetadataLabel is defined in oci_layout_inspect.go (same package).

// rewriteMetadataSHAs fixes the io.buildpacks.lifecycle.metadata label of a
// buildkit-native image AFTER it is pushed, so its per-layer SHAs match the
// image's ACTUAL layer diffIDs (which BuildKit recomputed during assembly).
//
// WHY: the buildkit-native frontend assembles the image by re-snapshotting the
// emitted layers, so BuildKit assigns NEW diffIDs. The lifecycle-metadata label
// (emitted by the lifecycle) still carries the ORIGINAL emitted diffIDs. Left
// unfixed, this breaks (a) the analyzer's previous-image restore on rebuilds
// (it looks up layers by the metadata SHA) and (b) buildpack-contributed-layer
// patching (matches layers by metadata SHA). This rewrite makes the metadata
// consistent with the image.
//
// HOW (no layer-data egress): the frontend recorded the ORDERED emitted diffIDs
// of the new layers in a temporary label (cnbfrontend.LayerOrderLabel). We read
// the pushed image's config, pair that ordered list positionally with the
// image's actual NEW-layer diffIDs (the diffIDs after the run-image base
// boundary), build an old->new map, replace every matching SHA in the
// lifecycle-metadata label, drop the temporary label, and re-push ONLY the
// updated config + manifest (layer blobs are unchanged, so nothing is
// re-uploaded).
//
// Handles both a single image and a multi-arch manifest list (each child image
// is rewritten). Insecure registries are handled by the caller-provided options.
func rewriteMetadataSHAs(ctx context.Context, imageName string, insecure bool, logger logging.Logger) error {
	// The image was pushed by BuildKit under a name reachable INSIDE the buildkit
	// network (e.g. a docker-network container name). The host-side rewrite must
	// reach the same registry by its HOST-reachable name. In local test setups
	// these differ (container-name vs localhost). PACK_HOST_REGISTRY_REMAP lets the
	// operator remap "src=dst" registry hosts for the host-side rewrite only; it is
	// unset (no-op) in production where one name works from both sides.
	hostImageName := applyHostRegistryRemap(imageName)
	if hostImageName != imageName {
		logger.Debugf("Metadata rewrite: using host-reachable ref %s (remapped from %s)", hostImageName, imageName)
	}
	ref, err := ggcrname.ParseReference(hostImageName, nameOpts(isLikelyInsecureRegistry(registryHost(hostImageName)))...)
	if err != nil {
		return fmt.Errorf("parse image ref %q: %w", hostImageName, err)
	}
	_ = insecure
	remoteOpts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	}

	desc, err := remote.Get(ref, remoteOpts...)
	if err != nil {
		return fmt.Errorf("fetch %q: %w", imageName, err)
	}

	if desc.MediaType.IsIndex() {
		return rewriteIndex(ref, desc, remoteOpts, logger)
	}
	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("resolve image %q: %w", imageName, err)
	}
	fixed, changed, err := rewriteImageMetadata(img, logger)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := remote.Write(ref, fixed, remoteOpts...); err != nil {
		return fmt.Errorf("push rewritten image %q: %w", imageName, err)
	}
	logger.Debugf("Rewrote lifecycle metadata SHAs for %s", imageName)
	return nil
}

// rewriteIndex rewrites each child image of a manifest list and re-pushes the
// index (child blobs unchanged).
func rewriteIndex(ref ggcrname.Reference, desc *remote.Descriptor, remoteOpts []remote.Option, logger logging.Logger) error {
	idx, err := desc.ImageIndex()
	if err != nil {
		return fmt.Errorf("resolve index: %w", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("read index manifest: %w", err)
	}

	result := idx
	for _, m := range im.Manifests {
		if m.MediaType.IsImage() {
			child, err := idx.Image(m.Digest)
			if err != nil {
				return fmt.Errorf("resolve child %s: %w", m.Digest, err)
			}
			fixed, changed, err := rewriteImageMetadata(child, logger)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			// Replace the child in the index, preserving its platform descriptor.
			result = mutate.RemoveManifests(result, func(d v1.Descriptor) bool {
				return d.Digest == m.Digest
			})
			result = mutate.AppendManifests(result, mutate.IndexAddendum{
				Add:        fixed,
				Descriptor: v1.Descriptor{Platform: m.Platform},
			})
		}
	}
	if err := remote.WriteIndex(ref, result, remoteOpts...); err != nil {
		return fmt.Errorf("push rewritten index: %w", err)
	}
	logger.Debugf("Rewrote lifecycle metadata SHAs for manifest list %s", ref.Name())
	return nil
}

// rewriteImageMetadata rewrites a single image's lifecycle-metadata label using
// the temporary layer-order label + the image's actual diffIDs. Returns the
// updated image and whether a change was made.
func rewriteImageMetadata(img v1.Image, logger logging.Logger) (v1.Image, bool, error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, false, fmt.Errorf("read image config: %w", err)
	}
	labels := cfg.Config.Labels
	if labels == nil {
		return img, false, nil
	}
	orderJSON, ok := labels[cnbfrontend.LayerOrderLabel]
	if !ok {
		// No temp label -> nothing to rewrite (or already rewritten).
		return img, false, nil
	}
	metaJSON, ok := labels[lifecycleMetadataLabel]
	if !ok {
		return img, false, nil
	}

	var emittedOrder []string
	if err := json.Unmarshal([]byte(orderJSON), &emittedOrder); err != nil {
		return nil, false, fmt.Errorf("parse layer-order label: %w", err)
	}

	// The actual NEW-layer diffIDs are the trailing len(emittedOrder) diffIDs of
	// the image (the new layers were added on top of the run-image base, in the
	// same order as emittedOrder).
	actual := cfg.RootFS.DiffIDs
	if len(emittedOrder) > len(actual) {
		return nil, false, fmt.Errorf("layer-order has %d entries but image has %d layers", len(emittedOrder), len(actual))
	}
	newDiffIDs := actual[len(actual)-len(emittedOrder):]

	// Build old->new sha map.
	oldToNew := make(map[string]string, len(emittedOrder))
	for i, old := range emittedOrder {
		oldToNew[old] = newDiffIDs[i].String()
	}

	// Replace every matching SHA in the lifecycle-metadata label (string replace
	// is safe: SHAs are unique 71-char tokens). This covers app[], sbom, launcher,
	// config, process-types, and buildpacks[].layers[].sha uniformly.
	rewritten := metaJSON
	for old, nw := range oldToNew {
		rewritten = strings.ReplaceAll(rewritten, old, nw)
	}

	// Apply: set the fixed metadata label, drop the temporary layer-order label.
	newLabels := make(map[string]string, len(labels))
	for k, v := range labels {
		newLabels[k] = v
	}
	newLabels[lifecycleMetadataLabel] = rewritten
	delete(newLabels, cnbfrontend.LayerOrderLabel)

	newCfg := cfg.DeepCopy()
	newCfg.Config.Labels = newLabels
	out, err := mutate.ConfigFile(img, newCfg)
	if err != nil {
		return nil, false, fmt.Errorf("apply rewritten config: %w", err)
	}
	logger.Debugf("Rewrote %d layer SHA(s) in lifecycle metadata", len(oldToNew))
	return out, true, nil
}

func nameOpts(insecure bool) []ggcrname.Option {
	if insecure {
		return []ggcrname.Option{ggcrname.Insecure, ggcrname.WeakValidation}
	}
	return []ggcrname.Option{ggcrname.WeakValidation}
}

// applyHostRegistryRemap rewrites the registry host of imageName for host-side
// access, using the PACK_HOST_REGISTRY_REMAP env var (format "src=dst", e.g.
// "pack-local-registry:5000=localhost:5050"). This exists ONLY to bridge local
// test setups where the buildkit-reachable registry name differs from the
// host-reachable name. Unset in production => no-op.
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
