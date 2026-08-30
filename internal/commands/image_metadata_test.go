package commands_test

import (
	"bytes"
	"testing"

	"github.com/heroku/color"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"
	"github.com/spf13/cobra"

	"github.com/buildpacks/pack/internal/commands"
	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestImageMetadataCommand(t *testing.T) {
	color.Disable(true)
	defer color.Disable(false)

	spec.Run(t, "ImageMetadataCommand", testImageMetadataCommand, spec.Random(), spec.Report(report.Terminal{}))
}

func testImageMetadataCommand(t *testing.T, when spec.G, it spec.S) {
	var (
		command *cobra.Command
		logger  *logging.LogWithWriters
		outBuf  bytes.Buffer
	)

	it.Before(func() {
		logger = logging.NewLogWithWriters(&outBuf, &outBuf)
		command = commands.NewImageMetadataCommand(logger)
		command.SetOut(logging.GetWriterForLevel(logger, logging.InfoLevel))
	})

	when("no subcommand", func() {
		it("prints help listing inspect, verify and fix", func() {
			command.SetArgs([]string{})
			err := command.Execute()
			h.AssertNilE(t, err)

			output := outBuf.String()
			h.AssertContains(t, output, "Usage:")
			for _, sub := range []string{"inspect", "verify", "fix"} {
				h.AssertContains(t, output, sub)
			}
		})
	})

	when("a subcommand is missing its image arg", func() {
		it("errors", func() {
			for _, sub := range []string{"inspect", "verify", "fix"} {
				outBuf.Reset()
				command.SetArgs([]string{sub})
				err := command.Execute()
				h.AssertNotNil(t, err)
			}
		})
	})

	when("fix exposes the keep-prepared-metadata-label flag", func() {
		it("has the flag", func() {
			fixCmd, _, err := command.Find([]string{"fix"})
			h.AssertNil(t, err)
			h.AssertNotNil(t, fixCmd.Flags().Lookup("keep-prepared-metadata-label"))
		})
	})
}
