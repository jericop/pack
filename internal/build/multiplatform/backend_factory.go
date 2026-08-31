package multiplatform

import (
	"context"
	"fmt"

	"github.com/buildpacks/pack/pkg/logging"
)

// NewBackend creates a BuildBackend for a NATIVE backend type. Only "buildkit" is
// a BuildBackend implementation today; the standard "docker-daemon" backend is not
// constructed here — the client routes it through the existing daemon lifecycle
// executor (A-lite). The switch is retained so a future native backend (e.g. a
// buildah backend) can be added without changing callers.
func NewBackend(ctx context.Context, backendType BackendType, logger logging.Logger, buildkitOpts BuildkitOpts) (BuildBackend, error) {
	switch backendType {
	case BackendBuildkit:
		return NewBuildkitBackend(logger, buildkitOpts), nil

	default:
		return nil, fmt.Errorf("build backend %q is not a native BuildBackend; valid native options: %s", backendType, BackendBuildkit)
	}
}
