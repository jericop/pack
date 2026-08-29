package multiplatform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/buildpacks/lifecycle/phase/emit"
)

// EmitContract is the parsed output of the lifecycle's BuildKit-native emit-mode:
// the ordered layer Plan plus the final image Config. The pack-side buildkit-native
// backend consumes this to assemble the app image natively in BuildKit.
//
// The types (emit.Plan / emit.ImageConfig) are IMPORTED from the lifecycle so pack
// and the lifecycle share a single source of truth for the schema — the JSON files
// are only the transport across the BuildKit->host boundary (see the
// buildkit-native-export spec's "Transport model").
type EmitContract struct {
	Plan   emit.Plan
	Config emit.ImageConfig
}

// ReadEmitContract reads and validates the emit contract from an emit output
// directory. The lifecycle writes the BuildKit-native recorder's files under
// <emitDir>/<emit.RecorderDir>/ (i.e. <emitDir>/buildkit/plan.json and
// config.json). emitDir is the directory passed to the lifecycle's
// -emit-export-plan flag.
func ReadEmitContract(emitDir string) (*EmitContract, error) {
	dir := filepath.Join(emitDir, emit.RecorderDir)

	plan, err := readPlan(filepath.Join(dir, emit.PlanFileName))
	if err != nil {
		return nil, err
	}
	config, err := readConfig(filepath.Join(dir, emit.ConfigFileName))
	if err != nil {
		return nil, err
	}

	contract := &EmitContract{Plan: plan, Config: config}
	if err := contract.validate(); err != nil {
		return nil, err
	}
	return contract, nil
}

func readPlan(path string) (emit.Plan, error) {
	var plan emit.Plan
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return emit.Plan{}, fmt.Errorf("reading emit plan %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return emit.Plan{}, fmt.Errorf("parsing emit plan %s: %w", path, err)
	}
	return plan, nil
}

func readConfig(path string) (emit.ImageConfig, error) {
	var config emit.ImageConfig
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return emit.ImageConfig{}, fmt.Errorf("reading emit config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return emit.ImageConfig{}, fmt.Errorf("parsing emit config %s: %w", path, err)
	}
	return config, nil
}

// validate checks that the emit contract is well-formed and uses a schema this
// version of pack understands. It fails closed: any schema mismatch or structural
// problem is an error, so we never assemble an image from a contract we cannot
// faithfully interpret.
func (c *EmitContract) validate() error {
	if c.Plan.Schema != emit.Schema {
		return fmt.Errorf("emit plan schema %q is not supported (pack expects %q)", c.Plan.Schema, emit.Schema)
	}
	if c.Config.Schema != emit.Schema {
		return fmt.Errorf("emit config schema %q is not supported (pack expects %q)", c.Config.Schema, emit.Schema)
	}
	if c.Plan.RunImage.Reference == "" {
		return fmt.Errorf("emit plan is missing runImage.reference")
	}
	if c.Plan.RunImage.TopLayer == "" {
		return fmt.Errorf("emit plan is missing runImage.topLayer (rebase boundary)")
	}
	if len(c.Plan.Layers) == 0 {
		return fmt.Errorf("emit plan has no layers")
	}
	for i, layer := range c.Plan.Layers {
		if layer.DiffID == "" {
			return fmt.Errorf("emit plan layer %d (%q) is missing diffID", i, layer.ID)
		}
		// A non-reused (new) layer must carry the tar path to the actual built
		// layer; a reused layer is referenced by digest only and must NOT.
		if !layer.Reused && layer.TarPath == "" {
			return fmt.Errorf("emit plan layer %d (%q) is a new layer but has no tar path", i, layer.ID)
		}
	}
	if len(c.Config.Entrypoint) == 0 {
		return fmt.Errorf("emit config is missing entrypoint")
	}
	return nil
}

// NewLayers returns the plan entries that are new (build-produced) layers, in
// order. These are added to the assembled image by their diffID from the
// in-BuildKit layer tars.
func (c *EmitContract) NewLayers() []emit.LayerOp {
	var out []emit.LayerOp
	for _, l := range c.Plan.Layers {
		if !l.Reused {
			out = append(out, l)
		}
	}
	return out
}

// ReusedLayers returns the plan entries that are reused run-image/base layers, in
// order. These are referenced from the run image by digest (no tar).
func (c *EmitContract) ReusedLayers() []emit.LayerOp {
	var out []emit.LayerOp
	for _, l := range c.Plan.Layers {
		if l.Reused {
			out = append(out, l)
		}
	}
	return out
}
