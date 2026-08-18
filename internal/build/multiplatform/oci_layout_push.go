// Package multiplatform provides abstractions for building container images
// across multiple architectures using different build backends.
//
// NOTE: This file (oci_layout_push.go) contains code for the OCI layout export mode
// which is NOT YET FUNCTIONAL. The lifecycle's -layout flag requires the run image to
// be pre-populated in the layout directory before the analyzer runs, which is not yet
// solved in the buildkit execution context. Only the "registry" export mode works currently.
// This code is retained for future implementation.

package multiplatform

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/buildpacks/pack/pkg/logging"
)

// PushOCILayoutAsManifestList reads per-arch OCI layouts from disk, assembles them
// into a manifest list (image index), and pushes everything atomically to the registry.
// No intermediate tags are created — just the final manifest list at the target ref.
func PushOCILayoutAsManifestList(ctx context.Context, targetRef string, platforms []Platform, outputDir string, logger logging.Logger) error {
	ref, err := name.ParseReference(targetRef)
	if err != nil {
		return fmt.Errorf("parsing target reference %q: %w", targetRef, err)
	}

	// Read each per-arch OCI layout and build index addenda
	var addenda []mutate.IndexAddendum
	for _, p := range platforms {
		platformDir := filepath.Join(outputDir, p.OS, p.Arch)
		layoutPath, err := layout.FromPath(platformDir)
		if err != nil {
			return fmt.Errorf("reading OCI layout for %s from %s: %w", p.String(), platformDir, err)
		}

		// Get the image index from the layout — typically has one image
		idx, err := layoutPath.ImageIndex()
		if err != nil {
			return fmt.Errorf("reading image index for %s: %w", p.String(), err)
		}

		// Get the manifest from the index
		idxManifest, err := idx.IndexManifest()
		if err != nil {
			return fmt.Errorf("reading index manifest for %s: %w", p.String(), err)
		}

		if len(idxManifest.Manifests) == 0 {
			return fmt.Errorf("OCI layout for %s contains no images", p.String())
		}

		// Get the first (and typically only) image from the layout
		img, err := idx.Image(idxManifest.Manifests[0].Digest)
		if err != nil {
			return fmt.Errorf("reading image for %s: %w", p.String(), err)
		}

		addenda = append(addenda, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{
					OS:           p.OS,
					Architecture: p.Arch,
					Variant:      p.Variant,
				},
			},
		})

		logger.Debugf("Added %s image to manifest list from %s", p.String(), platformDir)
	}

	// Assemble the manifest list
	index := mutate.AppendManifests(empty.Index, addenda...)

	logger.Infof("Pushing manifest list to %s", targetRef)

	// Push atomically — all per-arch blobs + manifests + the index in one operation
	if err := remote.WriteIndex(ref, index, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(ctx)); err != nil {
		return fmt.Errorf("pushing manifest list to %s: %w", targetRef, err)
	}

	return nil
}
