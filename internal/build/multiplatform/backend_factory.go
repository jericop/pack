package multiplatform

import (
	"context"
	"fmt"

	"github.com/buildpacks/pack/pkg/logging"
)

// NewBackend creates the appropriate BuildBackend based on the requested type.
// Defaults to BackendBuildkitDockerfile. The LLB backend can be selected via --build-backend flag.
func NewBackend(ctx context.Context, backendType BackendType, logger logging.Logger, buildkitOpts BuildkitOpts) (BuildBackend, error) {
	switch backendType {
	case BackendBuildkitDockerfile, BackendAuto, "":
		return NewDockerfileBackend(logger, buildkitOpts), nil

	case BackendBuildkitLLB:
		return NewLLBBackend(logger, buildkitOpts), nil

	default:
		return nil, fmt.Errorf("unknown build backend %q; valid options: %s, %s",
			backendType, BackendBuildkitDockerfile, BackendBuildkitLLB)
	}
}
