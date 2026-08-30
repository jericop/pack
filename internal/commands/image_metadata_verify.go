package commands

import (
	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/buildpacks/pack/internal/build/multiplatform"
	"github.com/buildpacks/pack/pkg/logging"
)

// ImageMetadataVerify is the pass/fail counterpart to `inspect`: it performs the
// same READ-ONLY check but exits NON-ZERO unless the image is finalized, so it can
// gate CI (e.g. `pack image-metadata verify $IMG && deploy`). It never modifies
// the image.
func ImageMetadataVerify(logger logging.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "verify <image>",
		Args:    cobra.ExactArgs(1),
		Short:   "Check a pushed image's CNB metadata is finalized (exit code = pass/fail)",
		Example: "pack image-metadata verify registry.example.com/my-app:latest",
		Long: `Read-only pass/fail check of a pushed image's CNB metadata, for use as a CI gate.

Exits zero only when the image is finalized (has a valid io.buildpacks.lifecycle.metadata and no leftover prepared-metadata label). Exits non-zero when the image still needs the apply step (run 'pack image-metadata fix <image>') or is not a buildkit-native CNB image. Use 'pack image-metadata inspect' if you just want to print the state without a failing exit code.`,
		RunE: logError(logger, func(cmd *cobra.Command, args []string) error {
			imageName := args[0]
			if imageName == "" {
				return errors.New("'<image>' is required")
			}
			state, err := multiplatform.VerifyImageMetadata(cmd.Context(), logger, imageName)
			if err != nil {
				return err
			}
			switch state {
			case multiplatform.MetadataFinalized:
				return nil
			case multiplatform.MetadataNeedsApply:
				return errors.Errorf("image %s is not finalized (needs-apply): run 'pack image-metadata fix %s'", imageName, imageName)
			default:
				return errors.Errorf("image %s is not a buildkit-native CNB image (not-cnb-native)", imageName)
			}
		}),
	}

	AddHelpFlag(cmd, "verify")
	return cmd
}
