// Package multiplatform provides abstractions for building container images
// across multiple architectures using different build backends.
//
// This file (oci_layout_push.go) implements the multi-arch manifest-list
// assembly step for OCI layout mode (Task 3, FR-5).
//
// # Design decision: assemble-from-layouts (Option b), not native single-solve (Option a)
//
// Task 3 asked how to get BuildKit to assemble a multi-platform manifest list
// from per-arch images that originate from SEPARATE per-arch OCI layouts. Two
// options were considered:
//
//   - Option (a) "single multi-platform solve": one solve that produces all
//     platforms and lets BuildKit's ExporterImage assemble+push the manifest
//     list natively.
//   - Option (b) "combine per-arch results": keep per-arch native Phase 2
//     (each arch imported from its own store) and assemble the final manifest
//     list from the per-arch content-store images.
//
// We chose Option (b). Rationale:
//
//   - The LLB backend drives builds with the client.Solve API (see
//     backend_llb.go), NOT the gateway/frontend API. Each platform is solved
//     independently against its OWN per-arch content store (FR-4: "Each parallel
//     platform MUST use its own content store"). A single llb.OCILayout() source
//     resolves to exactly one platform's layout — there is no client.Solve knob
//     to feed N separate per-arch OCI-layout stores into one multi-platform
//     ExporterImage. Native cross-store assembly would require a gateway frontend
//     that returns a result carrying per-platform refs (exptypes.RefsKey /
//     exptypes.ExporterImageConfigKey + platforms), i.e. a materially different
//     execution model than the current code.
//   - design.md "Risk: Multi-platform manifest list assembly via ExporterImage"
//     documents exactly this fallback: "If native assembly across separate solves
//     is awkward, fall back to PushOCILayoutAsManifestList for the assembly step
//     while still using native per-arch push."
//   - Assembling from the per-arch layouts and pushing the index via
//     remote.WriteIndex creates NO intermediate tags — the registry sees only the
//     final manifest list appear atomically. This directly satisfies FR-5's
//     "no intermediate tags" for the assembly step.
//
// The functions below are split so the assembly (AssembleManifestList) is a pure,
// unit-testable operation over on-disk per-arch layouts (no network), while the
// push (pushIndex / PushPerArchLayoutsAsManifestList) is the only network step.

package multiplatform

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	ggcrname "github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/buildpacks/pack/pkg/logging"
)

// PerArchLayout identifies one platform's on-disk OCI layout for manifest-list
// assembly. It is produced from a per-arch Phase 1 content store: LayoutDir is
// the store directory (PlatformBuildResult.OCIStoreDir) and ManifestDigest is
// the digest of the single image manifest in that store
// (PlatformBuildResult.OCILayoutDigest). When ManifestDigest is empty the
// assembler resolves the sole image manifest from the layout's index.
type PerArchLayout struct {
	// Platform is the target OS/arch/variant for this layout. It becomes the
	// v1.Platform descriptor on the manifest-list entry.
	Platform Platform

	// LayoutDir is the on-disk OCI layout / content-store directory holding this
	// platform's image (the Phase 1 output dir, OCIStoreDir).
	LayoutDir string

	// ManifestDigest is the digest of the image manifest to select from the
	// layout (OCILayoutDigest from Phase 1). Optional: when empty, the assembler
	// falls back to resolving the single image manifest in the layout's index.
	ManifestDigest string
}

// perArchLayoutsFromResults derives the assembly inputs from per-arch build
// results. Each result carries the Phase 1 content-store dir (OCIStoreDir) and
// the manifest digest (OCILayoutDigest) that Phase 1 recorded, plus its
// Platform. This is the bridge from the LLB backend's per-arch results to the
// assembly step.
func perArchLayoutsFromResults(results []PlatformBuildResult) []PerArchLayout {
	layouts := make([]PerArchLayout, 0, len(results))
	for _, r := range results {
		layouts = append(layouts, PerArchLayout{
			Platform:       r.Platform,
			LayoutDir:      r.OCIStoreDir,
			ManifestDigest: r.OCILayoutDigest,
		})
	}
	return layouts
}

// AssembleManifestList builds an in-memory OCI image index (manifest list) from
// per-arch OCI layouts on disk. It is PURE and network-free: it reads image
// manifests/config/layers from the given per-arch layout directories and returns
// a v1.ImageIndex with exactly one entry per input layout, each carrying the
// correct os/arch/variant platform descriptor.
//
// This is the unit-testable core of the "no intermediate tags" assembly (Task 3
// verification): the resulting index references the per-arch images directly by
// content — no per-arch tag names are involved, and nothing is pushed here.
func AssembleManifestList(layouts []PerArchLayout) (v1.ImageIndex, error) {
	if len(layouts) == 0 {
		return nil, fmt.Errorf("no per-arch layouts provided for manifest list assembly")
	}

	addenda := make([]mutate.IndexAddendum, 0, len(layouts))
	for _, pl := range layouts {
		img, err := imageFromLayout(pl)
		if err != nil {
			return nil, err
		}
		addenda = append(addenda, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{
					OS:           pl.Platform.OS,
					Architecture: pl.Platform.Arch,
					Variant:      pl.Platform.Variant,
				},
			},
		})
	}

	return mutate.AppendManifests(empty.Index, addenda...), nil
}

// imageFromLayout opens one per-arch OCI layout directory and returns the single
// image to place in the manifest list. When pl.ManifestDigest is set (the Phase 1
// recorded digest) it selects that exact manifest; otherwise it resolves the sole
// image manifest via the shared InspectOCILayout resolution, keeping selection
// consistent with the on-disk inspector.
func imageFromLayout(pl PerArchLayout) (v1.Image, error) {
	if pl.LayoutDir == "" {
		return nil, fmt.Errorf("missing layout directory for platform %s", pl.Platform.String())
	}

	lp, err := layout.FromPath(pl.LayoutDir)
	if err != nil {
		return nil, fmt.Errorf("reading OCI layout for %s from %s: %w", pl.Platform.String(), pl.LayoutDir, err)
	}

	idx, err := lp.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("reading image index for %s from %s: %w", pl.Platform.String(), pl.LayoutDir, err)
	}

	digest := pl.ManifestDigest
	if digest == "" {
		// No recorded digest: resolve the single image manifest the same way the
		// on-disk inspector does, so ambiguous/multi-image layouts error clearly.
		idxManifest, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("reading index manifest for %s from %s: %w", pl.Platform.String(), pl.LayoutDir, err)
		}
		imgDesc, err := resolveImageDescriptor(pl.LayoutDir, idxManifest)
		if err != nil {
			return nil, err
		}
		digest = imgDesc.Digest.String()
	}

	hash, err := v1.NewHash(digest)
	if err != nil {
		return nil, fmt.Errorf("parsing manifest digest %q for %s: %w", digest, pl.Platform.String(), err)
	}

	img, err := idx.Image(hash)
	if err != nil {
		return nil, fmt.Errorf("reading image %s for %s from %s: %w", digest, pl.Platform.String(), pl.LayoutDir, err)
	}
	return img, nil
}

// PushPerArchLayoutsAsManifestList assembles a manifest list from the per-arch
// build results (each carrying its Phase 1 content-store dir + manifest digest +
// platform) and pushes it atomically to targetRef via go-containerregistry.
//
// This is the LLB OCI-layout-mode assembly entry point wired into the executor.
// It creates NO intermediate tags: the only registry write is the final
// remote.WriteIndex of the assembled index at targetRef (FR-5).
func PushPerArchLayoutsAsManifestList(ctx context.Context, targetRef string, results []PlatformBuildResult, logger logging.Logger) error {
	layouts := perArchLayoutsFromResults(results)
	index, err := AssembleManifestList(layouts)
	if err != nil {
		return fmt.Errorf("assembling manifest list for %s: %w", targetRef, err)
	}

	for _, pl := range layouts {
		logger.Debugf("Added %s image to manifest list from %s", pl.Platform.String(), pl.LayoutDir)
	}

	return pushIndex(ctx, targetRef, index, logger)
}

// pushIndex pushes an assembled image index to targetRef atomically. This is the
// sole network step of the assembly and the only place a registry write happens
// in OCI layout mode. remote.WriteIndex uploads all referenced per-arch blobs +
// manifests + the index in one operation, so the registry never sees a partial
// state and no intermediate per-arch tag is ever created.
func pushIndex(ctx context.Context, targetRef string, index v1.ImageIndex, logger logging.Logger) error {
	ref, err := ggcrname.ParseReference(targetRef)
	if err != nil {
		return fmt.Errorf("parsing target reference %q: %w", targetRef, err)
	}

	logger.Infof("Pushing manifest list to %s", targetRef)

	if err := remote.WriteIndex(ref, index, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(ctx)); err != nil {
		return fmt.Errorf("pushing manifest list to %s: %w", targetRef, err)
	}
	return nil
}

// PushOCILayoutAsManifestList is the legacy assembly path that reads per-arch OCI
// layouts from a shared output directory laid out as <outputDir>/<os>/<arch>. It
// is RETAINED as a fallback for callers that produce that directory structure
// (e.g. a future non-LLB path). The LLB OCI-layout backend uses
// PushPerArchLayoutsAsManifestList instead, which reads from the Phase 1 per-arch
// content-store dirs recorded on each PlatformBuildResult.
//
// Like the newer path, it creates NO intermediate tags — only the final index is
// written via remote.WriteIndex.
func PushOCILayoutAsManifestList(ctx context.Context, targetRef string, platforms []Platform, outputDir string, logger logging.Logger) error {
	layouts := make([]PerArchLayout, 0, len(platforms))
	for _, p := range platforms {
		layouts = append(layouts, PerArchLayout{
			Platform:  p,
			LayoutDir: filepath.Join(outputDir, p.OS, p.Arch),
		})
	}

	index, err := AssembleManifestList(layouts)
	if err != nil {
		return fmt.Errorf("assembling manifest list for %s: %w", targetRef, err)
	}

	for _, pl := range layouts {
		logger.Debugf("Added %s image to manifest list from %s", pl.Platform.String(), pl.LayoutDir)
	}

	return pushIndex(ctx, targetRef, index, logger)
}
