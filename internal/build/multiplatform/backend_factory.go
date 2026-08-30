package multiplatform

import (
	"context"
	"fmt"

	"github.com/buildpacks/pack/pkg/logging"
)

// NewBackend creates the appropriate BuildBackend based on the requested type.
// Today the only backend is BuildKit; "buildkit", "auto", and "" all resolve to
// it. The switch is retained so a future backend (e.g. a buildah backend) can be
// added without changing callers.
func NewBackend(ctx context.Context, backendType BackendType, logger logging.Logger, buildkitOpts BuildkitOpts) (BuildBackend, error) {
	switch backendType {
	case BackendBuildkit, BackendAuto, "":
		return NewBuildkitBackend(logger, buildkitOpts), nil

	default:
		return nil, fmt.Errorf("unknown build backend %q; valid options: %s", backendType, BackendBuildkit)
	}
}
