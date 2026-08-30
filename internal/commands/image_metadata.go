package commands

import (
	"github.com/spf13/cobra"

	"github.com/buildpacks/pack/pkg/logging"
)

// NewImageMetadataCommand is the parent of the `pack image-metadata` subcommands
// (`verify`, `fix`). They inspect and repair the CNB metadata on an image that a
// buildkit build already pushed, WITHOUT rebuilding — the standalone counterpart
// to the build-time `--buildkit-fix-image-metadata` self-healing flag.
func NewImageMetadataCommand(logger logging.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image-metadata",
		Short: "Verify or fix CNB metadata on a pushed image (no rebuild)",
		Long: `Inspect and repair the Cloud Native Buildpacks metadata on an image that a BuildKit build already pushed, without rebuilding it.

The buildkit build backend builds and pushes an image, then a post-push step ("finalize"/"apply") authors the final 'io.buildpacks.lifecycle.metadata' from the image's actual layers, consuming the build-phase 'io.buildpacks.lifecycle.prepared-metadata' label. If that step was interrupted, the pushed image is runnable but not yet rebuildable/rebaseable.

'pack image-metadata inspect <image>' prints whether an image is already finalized, still needs the apply step, or is not a buildkit-native CNB image (always exits zero). 'pack image-metadata verify <image>' does the same read-only check but exits non-zero unless the image is finalized (a CI gate). 'pack image-metadata fix <image>' runs the apply step against the pushed image (config+manifest only, no layer re-upload) and is idempotent.

These commands are experimental.`,
		RunE: nil,
	}

	cmd.AddCommand(ImageMetadataInspect(logger))
	cmd.AddCommand(ImageMetadataVerify(logger))
	cmd.AddCommand(ImageMetadataFix(logger))

	AddHelpFlag(cmd, "image-metadata")
	return cmd
}
