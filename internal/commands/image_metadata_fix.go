package commands

import (
	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/buildpacks/pack/internal/build/multiplatform"
	"github.com/buildpacks/pack/pkg/logging"
)

// ImageMetadataFixFlags are the flags for `pack image-metadata fix`.
type ImageMetadataFixFlags struct {
	KeepPreparedMetadataLabel bool
}

// ImageMetadataFix runs the apply/finalize step against an already-pushed image:
// it authors the correct io.buildpacks.lifecycle.metadata from the image's actual
// produced layers + the io.buildpacks.lifecycle.prepared-metadata label and
// re-pushes config+manifest (+ index) only — no layer blobs are re-uploaded. It is
// idempotent: finalizing an already-finalized image is a no-op.
//
// It repairs an image whose buildkit build pushed it but did not finalize (e.g. an
// interrupted build), using the prepared-metadata label the build left behind.
func ImageMetadataFix(logger logging.Logger) *cobra.Command {
	var flags ImageMetadataFixFlags
	cmd := &cobra.Command{
		Use:     "fix <image>",
		Args:    cobra.ExactArgs(1),
		Short:   "Apply/finalize CNB metadata on a pushed image (idempotent, no rebuild)",
		Example: "pack image-metadata fix registry.example.com/my-app:latest",
		Long: `Author the final CNB metadata on an image that a BuildKit build already pushed but did not finalize (e.g. an interrupted build).

Reads the image's actual produced layer diffIDs plus the io.buildpacks.lifecycle.prepared-metadata label, authors io.buildpacks.lifecycle.metadata, and re-pushes config+manifest (+ index for a manifest list) only — no layer blobs are read, added, or re-uploaded. Handles both a single image and a manifest list.

Idempotent: running it against an already-finalized image is a no-op.`,
		RunE: logError(logger, func(cmd *cobra.Command, args []string) error {
			imageName := args[0]
			if imageName == "" {
				return errors.New("'<image>' is required")
			}
			return multiplatform.FixRemoteImageMetadata(cmd.Context(), logger, imageName, flags.KeepPreparedMetadataLabel)
		}),
	}

	cmd.Flags().BoolVar(&flags.KeepPreparedMetadataLabel, "keep-prepared-metadata-label", false, "Keep the prepared-metadata label on the finalized image (enables later self-healing re-apply) instead of removing it.")
	AddHelpFlag(cmd, "fix")
	return cmd
}
