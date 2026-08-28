package multiplatform

import (
	"encoding/json"
	"fmt"
	"sort"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// lifecycleMetadataLabel is the image config label the CNB lifecycle writes to
// record the exported image's layer structure (app/launcher/config/buildpack
// layers, run image, and SBOM). Its presence and contents are what the design's
// Tier 2 on-disk verification asserts for parity/rebase (FR-7). The label is
// JSON produced by the lifecycle; synthetic go-containerregistry fixtures do not
// have it, so the inspector treats it as informational (never required).
const lifecycleMetadataLabel = "io.buildpacks.lifecycle.metadata"

// OCILayoutInspection is the structured result of inspecting an on-disk OCI
// layout / content store. It reports whether the layout is a complete,
// self-contained image: an index that resolves to a manifest whose config and
// layer blobs all exist and are readable in the store.
//
// It is intentionally decoupled from any BuildKit solve so it can be reused by
// later on-disk verification tasks (Task 7 on-disk OCI layout test, Task 8
// parity check) against a synthetic fixture or a real Phase 1 store.
type OCILayoutInspection struct {
	// LayoutDir is the directory that was inspected.
	LayoutDir string

	// ManifestDigest is the digest of the resolved image manifest.
	ManifestDigest string

	// MediaType is the media type of the resolved image manifest.
	MediaType string

	// ConfigDigest is the digest of the image config blob referenced by the manifest.
	ConfigDigest string

	// LayerDigests are the digests of the layer blobs referenced by the manifest,
	// in manifest order.
	LayerDigests []string

	// DiffIDs are the uncompressed layer diff IDs read from the image config,
	// in config order. These are the values that must match across export modes
	// for parity/rebase (FR-7, FR-8).
	//
	// LayerDigests (manifest order) and DiffIDs (config order) are parallel: for a
	// well-formed image the i-th layer blob's uncompressed content has the i-th
	// diff ID. Callers verifying layer ORDER should assert len(LayerDigests) ==
	// len(DiffIDs); LayersMatchDiffIDs reports this.
	DiffIDs []string

	// Config holds the runtime image config fields the design's Tier 2 on-disk
	// test verifies (entrypoint, env, user, workdir, exposed ports). Read from the
	// image ConfigFile().Config. FR-7/FR-8 parity is asserted against these.
	Config OCIImageConfig

	// Labels is the image config Labels map (may be nil for a synthetic fixture
	// that set none). Exposed so callers can assert arbitrary labels are present,
	// including the CNB lifecycle metadata label.
	Labels map[string]string

	// LifecycleMetadata holds the parsed io.buildpacks.lifecycle.metadata label
	// when present. Present is false for images without the label (e.g. synthetic
	// fixtures) — a missing label is NOT an error (see InspectOCILayout).
	LifecycleMetadata LifecycleMetadata

	// Complete is true when the index resolved to a single image manifest and the
	// config blob plus every referenced layer blob exist and are readable.
	Complete bool
}

// OCIImageConfig holds the subset of runtime image config fields the on-disk
// verification (design "Tier 2: On-disk OCI layout tests") checks: entrypoint,
// env, user, working dir, and exposed ports. Values are copied out of
// v1.ConfigFile.Config so callers do not need to re-open the layout.
type OCIImageConfig struct {
	// Entrypoint is the image entrypoint (config Entrypoint), in order.
	Entrypoint []string

	// Env is the image environment (config Env), each entry "KEY=VALUE", in order.
	Env []string

	// User is the image user (config User).
	User string

	// WorkingDir is the image working directory (config WorkingDir).
	WorkingDir string

	// ExposedPorts are the exposed port keys (config ExposedPorts, e.g. "8080/tcp"),
	// sorted for deterministic comparison (the underlying config value is a set/map).
	ExposedPorts []string
}

// LifecycleMetadata is the parsed, informational view of the
// io.buildpacks.lifecycle.metadata label. It is intentionally partial: it
// surfaces the pieces the design's Tier 2 verification needs (the recorded layer
// diff IDs and SBOM presence) while ignoring fields it does not assert on, so the
// parse stays robust across lifecycle versions.
type LifecycleMetadata struct {
	// Present is true when the io.buildpacks.lifecycle.metadata label existed on
	// the image config AND parsed as JSON. When false, all other fields are zero
	// and callers should treat the metadata as unavailable (e.g. a synthetic
	// fixture). A missing/invalid label never fails InspectOCILayout.
	Present bool

	// Raw is the exact label value as stored on the image config (empty when the
	// label is absent). Exposed so callers can assert the raw JSON or re-parse it.
	Raw string

	// DiffIDs are the layer diff IDs recorded inside the lifecycle metadata, in
	// the order app → launcher (config/process types) → buildpack layers, as
	// carried by the label. These let callers cross-check the label's recorded
	// layers against the config RootFS diff IDs (FR-7). Empty when Present=false.
	DiffIDs []string

	// HasSBOM is true when the lifecycle metadata records an SBOM layer (the
	// "sbom" key with a diff ID). This is how SBOM presence is detected on disk
	// without unpacking layer contents (design "Tier 2 ... SBOM layer presence").
	// Meaningful only when Present=true.
	HasSBOM bool

	// SBOMDiffID is the diff ID of the SBOM layer recorded in the metadata (empty
	// when HasSBOM is false).
	SBOMDiffID string

	// RunImageReference is the recorded runImage.reference from the lifecycle
	// metadata — the run image the app was built FROM. Together with
	// RunImageTopLayer it defines the rebase boundary the CNB rebaser reads to
	// swap run-image layers without re-running buildpacks (FR-7; see the
	// cnb-lifecycle steering layer-order-and-rebase.md). Empty when absent.
	// Informational only: its absence never fails InspectOCILayout.
	RunImageReference string

	// RunImageTopLayer is the recorded runImage.topLayer from the lifecycle
	// metadata — the diff ID of the TOP run-image layer. The rebaser treats all
	// layers up to and including this diff ID as run-image layers (replaced on
	// rebase) and everything above it as app/launcher/buildpack layers
	// (preserved). This is the single most important field for rebase readiness
	// (FR-7). Empty when absent. Informational only.
	RunImageTopLayer string
}

// lifecycleLayerRef mirrors the shape the lifecycle uses for a single recorded
// layer inside io.buildpacks.lifecycle.metadata: an object carrying a "sha"
// (the layer diff ID). Only the fields we surface are decoded.
type lifecycleLayerRef struct {
	SHA string `json:"sha"`
}

// lifecycleRunImage mirrors the runImage object inside
// io.buildpacks.lifecycle.metadata. The rebaser reads topLayer + reference to
// locate the run-image boundary during rebase (see steering
// layer-order-and-rebase.md). Only the fields we surface are decoded.
type lifecycleRunImage struct {
	TopLayer  string `json:"topLayer"`
	Reference string `json:"reference"`
}

// lifecycleMetadataJSON is a partial decoding of the
// io.buildpacks.lifecycle.metadata label. Only the fields Tier 2 verification
// needs are declared; unknown fields are ignored so the parse is robust across
// lifecycle versions.
type lifecycleMetadataJSON struct {
	App        []lifecycleLayerRef `json:"app"`
	Config     *lifecycleLayerRef  `json:"config"`
	Launcher   *lifecycleLayerRef  `json:"launcher"`
	Buildpacks []struct {
		Layers map[string]lifecycleLayerRef `json:"layers"`
	} `json:"buildpacks"`
	SBOM     *lifecycleLayerRef `json:"sbom"`
	RunImage *lifecycleRunImage `json:"runImage"`
}

// InspectOCILayout opens an on-disk OCI layout / content store directory, reads
// the OCI index (index.json), resolves the image manifest, and confirms the
// image is complete: the config blob is present and readable, and every layer
// blob referenced by the manifest exists in the store.
//
// It returns a structured OCILayoutInspection on success. It returns an error
// (with Complete=false in the returned inspection) when the directory is not a
// valid OCI layout, when the index does not resolve to exactly one image, or
// when a referenced blob is missing/unreadable.
//
// The on-disk format written by BuildKit's ExporterOCI (containerd
// contentlocal store) is a standard OCI image layout — a directory containing
// an "oci-layout" marker, an "index.json", and a "blobs/<alg>/<digest>" tree.
// This is exactly what go-containerregistry's layout package reads, so we reuse
// it rather than re-implementing blob resolution.
func InspectOCILayout(layoutDir string) (OCILayoutInspection, error) {
	result := OCILayoutInspection{LayoutDir: layoutDir}

	// layout.FromPath validates the directory looks like an OCI layout: it reads
	// and parses index.json (the presence of the oci-layout marker is implied by
	// a well-formed layout produced by ExporterOCI).
	lp, err := layout.FromPath(layoutDir)
	if err != nil {
		return result, fmt.Errorf("opening OCI layout at %s: %w", layoutDir, err)
	}

	idx, err := lp.ImageIndex()
	if err != nil {
		return result, fmt.Errorf("reading image index at %s: %w", layoutDir, err)
	}

	idxManifest, err := idx.IndexManifest()
	if err != nil {
		return result, fmt.Errorf("reading index manifest at %s: %w", layoutDir, err)
	}

	imgDesc, err := resolveImageDescriptor(layoutDir, idxManifest)
	if err != nil {
		return result, err
	}
	result.ManifestDigest = imgDesc.Digest.String()

	img, err := idx.Image(imgDesc.Digest)
	if err != nil {
		return result, fmt.Errorf("resolving image %s at %s: %w", imgDesc.Digest, layoutDir, err)
	}

	mediaType, err := img.MediaType()
	if err != nil {
		return result, fmt.Errorf("reading media type for %s: %w", imgDesc.Digest, err)
	}
	result.MediaType = string(mediaType)

	manifest, err := img.Manifest()
	if err != nil {
		return result, fmt.Errorf("reading manifest for %s: %w", imgDesc.Digest, err)
	}
	result.ConfigDigest = manifest.Config.Digest.String()

	// Confirm the config blob is present and readable. RawConfigFile reads the
	// config blob bytes from the store; a missing/corrupt blob errors here.
	if _, err := img.RawConfigFile(); err != nil {
		return result, fmt.Errorf("reading config blob %s at %s: %w", manifest.Config.Digest, layoutDir, err)
	}

	// Capture diff IDs from the config (uncompressed layer identities). These are
	// the parity-relevant identifiers (FR-7/FR-8).
	configFile, err := img.ConfigFile()
	if err != nil {
		return result, fmt.Errorf("parsing config file %s at %s: %w", manifest.Config.Digest, layoutDir, err)
	}
	for _, d := range configFile.RootFS.DiffIDs {
		result.DiffIDs = append(result.DiffIDs, d.String())
	}

	// Capture the runtime image config fields the Tier 2 on-disk test verifies
	// (entrypoint, env, user, workdir, exposed ports) and the labels map.
	result.Config = imageConfigFields(configFile.Config)
	result.Labels = configFile.Config.Labels

	// Parse the CNB lifecycle metadata label when present. This is informational:
	// a missing or malformed label MUST NOT fail InspectOCILayout, because
	// synthetic go-containerregistry fixtures never carry it. Callers decide
	// whether its absence matters.
	result.LifecycleMetadata = parseLifecycleMetadata(configFile.Config.Labels)

	// Confirm every layer blob referenced by the manifest exists and is readable.
	layers, err := img.Layers()
	if err != nil {
		return result, fmt.Errorf("reading layers for %s at %s: %w", imgDesc.Digest, layoutDir, err)
	}
	for _, l := range layers {
		digest, err := l.Digest()
		if err != nil {
			return result, fmt.Errorf("reading layer digest at %s: %w", layoutDir, err)
		}
		result.LayerDigests = append(result.LayerDigests, digest.String())

		// Compressed() opens the layer blob from the store; if the blob is missing
		// from blobs/<alg>/<digest> this returns an error, catching an incomplete
		// layout. We close immediately — we only need existence/readability.
		rc, err := l.Compressed()
		if err != nil {
			return result, fmt.Errorf("layer blob %s missing or unreadable at %s: %w", digest, layoutDir, err)
		}
		_ = rc.Close()
	}

	result.Complete = true
	return result, nil
}

// resolveImageDescriptor selects the single image manifest from an OCI index.
//
// A Phase 1 store produced for one platform contains exactly one image manifest.
// We only treat image manifests (OCI/Docker manifest media types) as candidates
// so that a nested index entry does not get misinterpreted as the image. If the
// index does not contain exactly one image manifest we return an error, because
// the "is this a complete single image?" question is then ambiguous.
func resolveImageDescriptor(layoutDir string, idxManifest *v1.IndexManifest) (v1.Descriptor, error) {
	if idxManifest == nil || len(idxManifest.Manifests) == 0 {
		return v1.Descriptor{}, fmt.Errorf("OCI layout at %s contains no manifests", layoutDir)
	}

	var images []v1.Descriptor
	for _, m := range idxManifest.Manifests {
		if isImageManifest(m.MediaType) {
			images = append(images, m)
		}
	}

	switch len(images) {
	case 1:
		return images[0], nil
	case 0:
		return v1.Descriptor{}, fmt.Errorf("OCI layout at %s contains no image manifest (found %d non-image descriptor(s))", layoutDir, len(idxManifest.Manifests))
	default:
		return v1.Descriptor{}, fmt.Errorf("OCI layout at %s contains %d image manifests; expected exactly one for a per-arch layout", layoutDir, len(images))
	}
}

// LayersMatchDiffIDs reports whether the manifest-ordered layer digests and the
// config-ordered diff IDs are parallel (same length). A complete, well-formed
// image has exactly one diff ID per layer blob, so a caller verifying layer
// COUNT and ORDER can assert this alongside comparing the two slices index by
// index. It returns false on an incomplete inspection.
func (i OCILayoutInspection) LayersMatchDiffIDs() bool {
	return i.Complete && len(i.LayerDigests) == len(i.DiffIDs)
}

// imageConfigFields copies the runtime config fields the on-disk verification
// checks out of a v1.Config. ExposedPorts is a set (map) in the config, so its
// keys are sorted to give callers a deterministic, comparable slice.
func imageConfigFields(cfg v1.Config) OCIImageConfig {
	out := OCIImageConfig{
		Entrypoint: cfg.Entrypoint,
		Env:        cfg.Env,
		User:       cfg.User,
		WorkingDir: cfg.WorkingDir,
	}
	if len(cfg.ExposedPorts) > 0 {
		ports := make([]string, 0, len(cfg.ExposedPorts))
		for p := range cfg.ExposedPorts {
			ports = append(ports, p)
		}
		sort.Strings(ports)
		out.ExposedPorts = ports
	}
	return out
}

// parseLifecycleMetadata extracts the informational LifecycleMetadata from the
// image config labels. It NEVER errors: a missing label yields Present=false,
// and a present-but-unparseable label yields Present=false with Raw set to the
// original value so a caller can still inspect/report it. This keeps
// InspectOCILayout tolerant of synthetic fixtures (which lack the label) while
// surfacing everything a real lifecycle-produced layout carries.
//
// The recorded diff IDs are collected in a stable order — app layers, then
// config, then launcher, then buildpack layers (buildpack IDs sorted, then layer
// names sorted within each) — so the slice is deterministic for comparison.
// SBOM presence is derived from the metadata's "sbom" entry, the on-disk signal
// the design's Tier 2 test uses for SBOM-layer presence.
func parseLifecycleMetadata(labels map[string]string) LifecycleMetadata {
	raw, ok := labels[lifecycleMetadataLabel]
	if !ok || raw == "" {
		return LifecycleMetadata{Present: false}
	}

	var parsed lifecycleMetadataJSON
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Present on the image but not valid JSON we understand. Report the raw
		// value but treat the structured view as unavailable rather than erroring.
		return LifecycleMetadata{Present: false, Raw: raw}
	}

	md := LifecycleMetadata{Present: true, Raw: raw}

	for _, l := range parsed.App {
		if l.SHA != "" {
			md.DiffIDs = append(md.DiffIDs, l.SHA)
		}
	}
	if parsed.Config != nil && parsed.Config.SHA != "" {
		md.DiffIDs = append(md.DiffIDs, parsed.Config.SHA)
	}
	if parsed.Launcher != nil && parsed.Launcher.SHA != "" {
		md.DiffIDs = append(md.DiffIDs, parsed.Launcher.SHA)
	}
	for _, bp := range parsed.Buildpacks {
		names := make([]string, 0, len(bp.Layers))
		for name := range bp.Layers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if sha := bp.Layers[name].SHA; sha != "" {
				md.DiffIDs = append(md.DiffIDs, sha)
			}
		}
	}

	if parsed.SBOM != nil && parsed.SBOM.SHA != "" {
		md.HasSBOM = true
		md.SBOMDiffID = parsed.SBOM.SHA
	}

	// Surface the run-image rebase boundary (runImage.topLayer + reference). These
	// are what the CNB rebaser reads to find the boundary between run-image layers
	// (replaced on rebase) and app layers (preserved) — see FR-7 and the steering
	// doc. Kept informational: absent/empty values are fine (the offline rebase
	// readiness check in oci_layout_rebase.go decides whether their absence
	// matters), so a fixture without them still yields Present=true.
	if parsed.RunImage != nil {
		md.RunImageTopLayer = parsed.RunImage.TopLayer
		md.RunImageReference = parsed.RunImage.Reference
	}

	return md
}

// isImageManifest reports whether a media type denotes an image manifest (as
// opposed to an image index / manifest list). An empty media type is treated as
// an image manifest: some tools omit it on layout descriptors, and the OCI
// default for a manifest descriptor is the image manifest type.
func isImageManifest(mt types.MediaType) bool {
	switch mt {
	case types.OCIManifestSchema1, types.DockerManifestSchema2, "":
		return true
	default:
		return false
	}
}
