package multiplatform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// This file resolves the current buildx builder from buildx's ON-DISK state
// (the same files the docker buildx CLI reads), so the buildkit backend can
// connect to the ACTUAL selected builder when the user did not pass an explicit
// --buildkit-builder. It intentionally reads the state directly rather than
// shelling out to `docker buildx inspect`, and does not import the heavyweight
// docker/buildx module.
//
// Layout (under $DOCKER_CONFIG, default ~/.docker):
//   buildx/current              -> {"Key":"<ctx-or-instance>","Name":"<instance>","Global":bool}
//   buildx/instances/<name>     -> {"Name":"<name>","Driver":"docker-container|remote|docker","Nodes":[...]}
// When current.Name is empty the selected builder is the docker-driver default
// for the docker context named by current.Key (which CANNOT serve multi-platform
// buildkit builds). When current.Name is set it names a buildx instance whose
// driver is read from instances/<name>.

// buildxCurrent mirrors buildx/current.
type buildxCurrent struct {
	Key    string `json:"Key"`
	Name   string `json:"Name"`
	Global bool   `json:"Global"`
}

// buildxInstance mirrors the fields we need from buildx/instances/<name>.
type buildxInstance struct {
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
}

// resolvedBuilder describes a buildx builder resolved from on-disk state.
type resolvedBuilder struct {
	Name   string // buildx instance name ("" when the default docker-driver builder is selected)
	Driver string // "docker-container", "remote", or "docker"
}

// dockerConfigDir returns $DOCKER_CONFIG or ~/.docker.
func dockerConfigDir() (string, error) {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home dir for docker config: %w", err)
	}
	return filepath.Join(home, ".docker"), nil
}

// resolveCurrentBuildxBuilder reads buildx's on-disk state to determine the
// currently selected builder and its driver. Resolution order matches buildx:
//  1. the BUILDX_BUILDER env var (an explicit instance name), else
//  2. buildx/current (the `docker buildx use`-selected builder).
//
// A builder with an empty instance name is the docker-context default, which uses
// the "docker" driver. Returns a resolvedBuilder; callers decide whether the
// driver can serve multi-platform buildkit.
func resolveCurrentBuildxBuilder() (resolvedBuilder, error) {
	cfgDir, err := dockerConfigDir()
	if err != nil {
		return resolvedBuilder{}, err
	}

	// 1. BUILDX_BUILDER wins (buildx honors it over the persisted current).
	if name := os.Getenv("BUILDX_BUILDER"); name != "" {
		return builderByName(cfgDir, name)
	}

	// 2. Fall back to the persisted current builder.
	currentPath := filepath.Join(cfgDir, "buildx", "current")
	data, err := os.ReadFile(currentPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No buildx state at all -> the default docker-driver builder is in use.
			return resolvedBuilder{Name: "", Driver: "docker"}, nil
		}
		return resolvedBuilder{}, fmt.Errorf("reading buildx current (%s): %w", currentPath, err)
	}
	var cur buildxCurrent
	if err := json.Unmarshal(data, &cur); err != nil {
		return resolvedBuilder{}, fmt.Errorf("parsing buildx current (%s): %w", currentPath, err)
	}
	if cur.Name == "" {
		// No explicit buildx instance selected: the current builder is the
		// docker-context default, which uses the "docker" driver.
		return resolvedBuilder{Name: "", Driver: "docker"}, nil
	}
	return builderByName(cfgDir, cur.Name)
}

// builderByName reads instances/<name> to get the driver for an explicit builder.
func builderByName(cfgDir, name string) (resolvedBuilder, error) {
	instPath := filepath.Join(cfgDir, "buildx", "instances", name)
	data, err := os.ReadFile(instPath)
	if err != nil {
		if os.IsNotExist(err) {
			return resolvedBuilder{}, fmt.Errorf("buildx builder %q not found (no instance file at %s); create it with: docker buildx create --driver docker-container --name %s", name, instPath, name)
		}
		return resolvedBuilder{}, fmt.Errorf("reading buildx instance %q (%s): %w", name, instPath, err)
	}
	var inst buildxInstance
	if err := json.Unmarshal(data, &inst); err != nil {
		return resolvedBuilder{}, fmt.Errorf("parsing buildx instance %q (%s): %w", name, instPath, err)
	}
	driver := inst.Driver
	if driver == "" {
		driver = "docker" // defensive: an instance with no driver behaves as docker
	}
	return resolvedBuilder{Name: name, Driver: driver}, nil
}

// driverSupportsMultiPlatform reports whether a buildx driver can serve a
// multi-platform buildkit build. Only container-backed drivers expose a
// buildkit daemon we can dial (buildx_buildkit_<name>0); the plain "docker"
// driver builds through the local dockerd and cannot.
func driverSupportsMultiPlatform(driver string) bool {
	switch driver {
	case "docker-container", "remote", "kubernetes":
		return true
	default:
		return false
	}
}
