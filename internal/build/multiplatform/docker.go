package multiplatform

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/buildpacks/pack/pkg/logging"
)

// runDockerCommand executes a docker CLI command and streams output to the logger.
// It also captures stderr for error reporting and stdout for output parsing.
func runDockerCommand(ctx context.Context, args []string, logger logging.Logger) error {
	cmd := exec.CommandContext(ctx, "docker", args...)

	// Stream stdout and stderr directly so the user sees progress
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	logger.Debugf("Running: docker %v", args)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %v failed: %w", args, err)
	}

	return nil
}

// runDockerCommandCapture executes a docker CLI command, streams to stdout/stderr,
// and also captures the combined output for parsing.
func runDockerCommandCapture(ctx context.Context, args []string, logger logging.Logger) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)

	// Use a multi-writer to both display and capture output
	var captured bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &captured)
	cmd.Stderr = io.MultiWriter(os.Stderr, &captured)

	logger.Debugf("Running: docker %v", args)

	if err := cmd.Run(); err != nil {
		return captured.String(), fmt.Errorf("docker %v failed: %w", args, err)
	}

	return captured.String(), nil
}

// runDockerCommandWithOutput executes a docker CLI command and returns stdout.
func runDockerCommandWithOutput(ctx context.Context, args []string, logger logging.Logger) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	logger.Debugf("Running: docker %v", args)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %v failed: %w\nstderr: %s", args, err, stderr.String())
	}

	return stdout.String(), nil
}

// dockerBinaryAvailable checks if the docker CLI is available in PATH.
func dockerBinaryAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// buildxAvailable checks if the docker buildx plugin is available.
func buildxAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "docker", "buildx", "version")
	return cmd.Run() == nil
}
