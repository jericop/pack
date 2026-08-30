package commands

import (
	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/buildpacks/pack/internal/build/multiplatform"
	"github.com/buildpacks/pack/pkg/logging"
)

// ImageMetadataInspect reports whether a pushed image's CNB metadata is
// finalized, still needs the apply step, or the image is not a buildkit-native
// CNB image. It is READ-ONLY (fetches only the image config) and — consistent
// with pack's other `inspect` commands — prints the result and exits zero.
func ImageMetadataInspect(logger logging.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "inspect <image>",
		Args:    cobra.ExactArgs(1),
		Short:   "Show a pushed image's CNB metadata state",
		Example: "pack image-metadata inspect registry.example.com/my-app:latest",
		Long: `Inspect a pushed image's CNB metadata labels (read-only) and report one of:
  - finalized:      has a valid io.buildpacks.lifecycle.metadata and no leftover prepared-metadata label (nothing to do)
  - needs-apply:    still carries io.buildpacks.lifecycle.prepared-metadata; run 'pack image-metadata fix <image>'
  - not-cnb-native: has neither label; not a buildkit-native CNB image this command manages`,
		RunE: logError(logger, func(cmd *cobra.Command, args []string) error {
			imageName := args[0]
			if imageName == "" {
				return errors.New("'<image>' is required")
			}
			// VerifyImageMetadata logs the human-readable state; return it only as an
			// error when we truly could not inspect the image.
			_, err := multiplatform.VerifyImageMetadata(cmd.Context(), logger, imageName)
			return err
		}),
	}

	AddHelpFlag(cmd, "inspect")
	return cmd
}
