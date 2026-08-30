package multiplatform

import (
	"context"
	"fmt"

	ggcrname "github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/buildpacks/lifecycle/phase/emit"

	"github.com/buildpacks/pack/pkg/logging"
)

// preparedMetadataLabel is the build-phase label the buildkit backend attaches
// before finalize authors the final CNB metadata. It is owned by the lifecycle
// (emit.BuildMetadataLabel == "io.buildpacks.lifecycle.prepared-metadata").
const preparedMetadataLabel = emit.BuildMetadataLabel

// lifecycleMetadataLabel is the final CNB metadata label the finalize/apply step
// authors on a compliant image.
const lifecycleMetadataLabel = "io.buildpacks.lifecycle.metadata"

// MetadataState is the result of inspecting an image's CNB metadata labels.
type MetadataState int

const (
	// MetadataFinalized: the image carries a valid io.buildpacks.lifecycle.metadata
	// and no leftover prepared-metadata label — it is CNB-compliant (rebuildable +
	// rebaseable). Nothing to fix.
	MetadataFinalized MetadataState = iota
	// MetadataNeedsApply: the image still carries the prepared-metadata label,
	// meaning the build's post-push finalize/apply step has not run (or was
	// interrupted). It is runnable but not yet CNB-compliant; run `fix`.
	MetadataNeedsApply
	// MetadataNotCNBNative: the image has neither the prepared-metadata nor a
	// lifecycle.metadata label — it is not a buildkit-native CNB image this command
	// understands (e.g. a plain image, or a CNB image produced by another flow).
	MetadataNotCNBNative
)

func (s MetadataState) String() string {
	switch s {
	case MetadataFinalized:
		return "finalized"
	case MetadataNeedsApply:
		return "needs-apply"
	default:
		return "not-cnb-native"
	}
}

// VerifyImageMetadata inspects the CNB metadata labels of an already-pushed image
// (or manifest list — the first child image is inspected, as finalize applies the
// same authoring to every child) WITHOUT modifying it. It reports whether the
// image is finalized, still needs the apply/finalize step, or is not a
// buildkit-native CNB image. It is read-only: it fetches only the image config.
//
// The same host-registry remap + insecure detection used by the build/fix paths
// are applied so it works against local test registries.
func VerifyImageMetadata(ctx context.Context, logger logging.Logger, imageName string) (MetadataState, error) {
	ref := applyHostRegistryRemap(imageName)
	insecure := false
	if reg := registryHost(ref); reg != "" && isLikelyInsecureRegistry(reg) {
		insecure = true
	}

	parsed, err := ggcrname.ParseReference(ref, nameOptsFor(insecure)...)
	if err != nil {
		return MetadataNotCNBNative, fmt.Errorf("parse image ref %q: %w", ref, err)
	}
	desc, err := remote.Get(parsed, remote.WithContext(ctx))
	if err != nil {
		return MetadataNotCNBNative, fmt.Errorf("fetch %q: %w", ref, err)
	}

	labels, err := labelsFromDescriptor(desc)
	if err != nil {
		return MetadataNotCNBNative, err
	}

	_, hasPrepared := labels[preparedMetadataLabel]
	finalMeta, hasFinal := labels[lifecycleMetadataLabel]

	switch {
	case hasPrepared:
		logger.Infof("Image %s carries the prepared-metadata label (%s) — apply/finalize has NOT run.", ref, preparedMetadataLabel)
		return MetadataNeedsApply, nil
	case hasFinal && finalMeta != "" && finalMeta != "null":
		logger.Infof("Image %s is finalized: has a valid %s and no leftover %s.", ref, lifecycleMetadataLabel, preparedMetadataLabel)
		return MetadataFinalized, nil
	default:
		logger.Infof("Image %s has neither %s nor a valid %s — not a buildkit-native CNB image.", ref, preparedMetadataLabel, lifecycleMetadataLabel)
		return MetadataNotCNBNative, nil
	}
}

// labelsFromDescriptor returns the config labels of an image, resolving a
// manifest list to its first image child (finalize authors identical metadata on
// every child, so the first is representative for a read-only verify).
func labelsFromDescriptor(desc *remote.Descriptor) (map[string]string, error) {
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("resolve index: %w", err)
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("read index manifest: %w", err)
		}
		for _, m := range im.Manifests {
			if !m.MediaType.IsImage() {
				continue
			}
			child, err := idx.Image(m.Digest)
			if err != nil {
				return nil, fmt.Errorf("resolve child %s: %w", m.Digest, err)
			}
			return configLabels(child)
		}
		return map[string]string{}, nil
	}
	img, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("resolve image: %w", err)
	}
	return configLabels(img)
}

func configLabels(img v1.Image) (map[string]string, error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read image config: %w", err)
	}
	if cfg.Config.Labels == nil {
		return map[string]string{}, nil
	}
	return cfg.Config.Labels, nil
}

// nameOptsFor mirrors the finalize package's reference-parsing options so verify
// and fix parse refs identically.
func nameOptsFor(insecure bool) []ggcrname.Option {
	if insecure {
		return []ggcrname.Option{ggcrname.Insecure, ggcrname.WeakValidation}
	}
	return []ggcrname.Option{ggcrname.WeakValidation}
}
