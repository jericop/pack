package client

import (
	"context"
	"fmt"

	"github.com/buildpacks/pack/internal/style"
)

// AssembleAndPushManifest implements multiplatform.ManifestAssembler.
// It creates a manifest list from per-architecture image references already pushed
// to a registry, and pushes the assembled image index to the same registry at the
// given manifestListName tag.
//
// This uses pack's built-in manifest list functionality (imgutil + go-containerregistry)
// rather than shelling out to `docker buildx imagetools create`.
func (c *Client) AssembleAndPushManifest(ctx context.Context, manifestListName string, perArchRefs []string) error {
	c.logger.Debugf("Assembling manifest list %s from: %v", manifestListName, perArchRefs)

	// If a local manifest already exists from a previous build, remove it first
	if c.indexFactory.Exists(manifestListName) {
		c.logger.Debugf("Removing existing local manifest list %s", manifestListName)
		idx, err := c.indexFactory.LoadIndex(manifestListName)
		if err == nil {
			_ = idx.DeleteDir()
		}
	}

	// Create the manifest list and push it in one shot
	err := c.CreateManifest(ctx, CreateManifestOptions{
		IndexRepoName: manifestListName,
		RepoNames:     perArchRefs,
		Publish:       true,
	})
	if err != nil {
		return fmt.Errorf("failed to create and push manifest list %s: %w", style.Symbol(manifestListName), err)
	}

	return nil
}
