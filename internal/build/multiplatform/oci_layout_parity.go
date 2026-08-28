package multiplatform

// Parity comparison for OCI layout mode (spec Task 8, design "Tier 2 parity
// check — the confidence check, no registry").
//
// This file implements the reusable, offline, deterministic parity check the
// design leads with: given two on-disk OCI layouts — one the registry-mode
// reference (Dockerfile MVP), one the LLB OCI-layout output — decide whether
// they are at PARITY per FR-7 (rebase compatibility) and FR-8 (parity
// verification against registry mode).
//
// It is intentionally decoupled from any BuildKit solve: it compares two
// already-produced OCILayoutInspection values (from InspectOCILayout), so it is
// a pure function of on-disk data. That keeps it unit-testable without a daemon
// or a registry and makes it usable both by the Deliverable B daemon-gated
// integration test and by any future caller.
//
// # What "must match" means, and why
//
// The design and the cnb-lifecycle steering `layer-order-and-rebase.md` are
// explicit that the KEY parity signal is the layer diff IDs recorded in the
// io.buildpacks.lifecycle.metadata label. Rebase reads that label to find the
// run-image boundary (runImage.topLayer) and to identify launcher / buildpack /
// app layers by diff ID; if those diff IDs match across modes, an image built
// one way rebases exactly like one built the other way (FR-7). So the parity
// check compares:
//
//  1. Lifecycle-metadata layer diff IDs (LifecycleMetadata.DiffIDs) — MUST match,
//     in order. This is the primary rebase/parity signal.
//  2. Config RootFS diff IDs (OCILayoutInspection.DiffIDs) — MUST match, in
//     order. Cross-checks that the actual image layers line up with what the
//     metadata records; a divergence here would mean the on-disk image and its
//     recorded metadata disagree.
//  3. Image config runtime fields — Entrypoint, Env, User, WorkingDir,
//     ExposedPorts — MUST match. A launchable image that changed its entrypoint
//     or user across modes would not be "equivalent" (FR-8).
//  4. Labels — MUST match, with ONE deliberate exception described below.
//
// # Why raw lifecycle-metadata JSON is NOT compared byte-for-byte
//
// The io.buildpacks.lifecycle.metadata label legitimately differs across modes
// in NON-layer fields: runImage.reference can be a different (equivalent) ref,
// and the label may carry build timestamps or other environmental data. Byte
// comparing the raw JSON would produce false mismatches for images that are
// genuinely rebase-equivalent. The steering doc is clear that the thing that
// must be identical is the recorded LAYER DIFF IDS, not the surrounding JSON.
// Therefore this check compares the diff IDs EXTRACTED from the label (parsed by
// InspectOCILayout into LifecycleMetadata.DiffIDs / SBOMDiffID) and EXCLUDES the
// raw lifecycle-metadata label from the label-equality comparison. Every OTHER
// label (e.g. io.buildpacks.stack.id, io.buildpacks.builder metadata,
// io.buildpacks.project.metadata) is compared for exact equality, because those
// describe the build inputs/identity and must be the same for equivalent images.
//
// SBOM parity is included as part of the lifecycle-metadata comparison: the
// recorded SBOM presence (HasSBOM) and its diff ID must match, since the SBOM
// layer is part of the rebasable layer set the metadata records.

import (
	"fmt"
	"sort"
	"strings"
)

// ParityReport is the structured result of comparing two on-disk OCI layouts for
// parity (Task 8, FR-7/FR-8). It is designed to give a caller actionable output
// rather than a bare bool: when Match is false, Differences lists each specific
// mismatch (which diff ID differs, which config field differs, which label
// differs) so a failing test points straight at the divergence.
//
// The zero value is not meaningful; always obtain a report from CompareParity.
type ParityReport struct {
	// ReferenceLayoutDir is the layout dir of the registry-mode reference image.
	ReferenceLayoutDir string

	// CandidateLayoutDir is the layout dir of the LLB OCI-layout output image.
	CandidateLayoutDir string

	// Differences lists every mismatch found, one human-readable entry per
	// divergence. Empty when the two layouts are at parity.
	Differences []string
}

// Match reports whether the two layouts are at parity: true exactly when no
// differences were recorded.
func (r ParityReport) Match() bool {
	return len(r.Differences) == 0
}

// Error returns a single formatted string describing all parity differences, or
// the empty string when the layouts match. It is convenient for logging or for
// building a test failure message.
func (r ParityReport) Error() string {
	if r.Match() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "OCI layout parity mismatch (reference=%s candidate=%s): %d difference(s):",
		r.ReferenceLayoutDir, r.CandidateLayoutDir, len(r.Differences))
	for _, d := range r.Differences {
		b.WriteString("\n  - ")
		b.WriteString(d)
	}
	return b.String()
}

// CompareParity compares a registry-mode reference OCI layout inspection against
// an LLB OCI-layout candidate inspection and reports whether they are at parity
// per FR-7/FR-8. It is pure, deterministic, and offline: it derives every
// finding from the two OCILayoutInspection values, adding no I/O of its own.
//
// The comparison covers exactly the things the design and rebase steering say
// MUST match (see the file doc comment for the full rationale):
//   - lifecycle-metadata layer diff IDs (the primary rebase signal),
//   - config RootFS diff IDs (cross-check of the actual layers),
//   - SBOM presence + diff ID recorded in the lifecycle metadata,
//   - image config runtime fields (entrypoint, env, user, workdir, ports),
//   - all labels EXCEPT the raw io.buildpacks.lifecycle.metadata JSON, whose
//     layer content is compared via the extracted diff IDs above.
//
// It intentionally does NOT compare manifest/config/layer blob DIGESTS: those
// are compression-dependent and are not the parity contract (the uncompressed
// diff IDs are). It also does not require the two images to have been built by
// the same backend — only that the parity-relevant recorded data agrees.
func CompareParity(reference, candidate OCILayoutInspection) ParityReport {
	report := ParityReport{
		ReferenceLayoutDir: reference.LayoutDir,
		CandidateLayoutDir: candidate.LayoutDir,
	}

	// A parity comparison is only meaningful over two complete images. An
	// incomplete inspection means InspectOCILayout could not resolve/validate the
	// image, so we surface that as a difference rather than silently "matching".
	if !reference.Complete {
		report.Differences = append(report.Differences, "reference layout is not a complete image (InspectOCILayout reported Complete=false)")
	}
	if !candidate.Complete {
		report.Differences = append(report.Differences, "candidate layout is not a complete image (InspectOCILayout reported Complete=false)")
	}
	if !reference.Complete || !candidate.Complete {
		// Comparing fields of an incomplete inspection would produce noisy,
		// misleading diffs; stop here with the completeness findings.
		return report
	}

	report.Differences = append(report.Differences, compareLifecycleMetadata(reference.LifecycleMetadata, candidate.LifecycleMetadata)...)
	report.Differences = append(report.Differences, compareConfigDiffIDs(reference.DiffIDs, candidate.DiffIDs)...)
	report.Differences = append(report.Differences, compareImageConfig(reference.Config, candidate.Config)...)
	report.Differences = append(report.Differences, compareLabels(reference.Labels, candidate.Labels)...)

	return report
}

// compareLifecycleMetadata compares the parity-relevant, parsed content of the
// io.buildpacks.lifecycle.metadata label: the recorded layer diff IDs (the
// primary rebase signal, FR-7) and the recorded SBOM layer. It compares the
// EXTRACTED diff IDs, never the raw JSON, because non-layer fields
// (runImage.reference, timestamps) legitimately differ across modes.
func compareLifecycleMetadata(reference, candidate LifecycleMetadata) []string {
	var diffs []string

	// Both images must actually carry a parseable lifecycle metadata label —
	// without it there is no rebase contract to compare, so its absence on either
	// side is itself a parity failure for lifecycle-produced images.
	switch {
	case reference.Present && !candidate.Present:
		diffs = append(diffs, "lifecycle metadata label: present on reference but absent/invalid on candidate")
		return diffs
	case !reference.Present && candidate.Present:
		diffs = append(diffs, "lifecycle metadata label: present on candidate but absent/invalid on reference")
		return diffs
	case !reference.Present && !candidate.Present:
		diffs = append(diffs, "lifecycle metadata label: absent/invalid on both reference and candidate (cannot verify rebase parity)")
		return diffs
	}

	// The KEY parity signal: the recorded layer diff IDs must match in order.
	if d := compareStringSlices("lifecycle-metadata layer diff ID", reference.DiffIDs, candidate.DiffIDs); len(d) > 0 {
		diffs = append(diffs, d...)
	}

	// SBOM layer parity: presence and (when present) the recorded diff ID.
	if reference.HasSBOM != candidate.HasSBOM {
		diffs = append(diffs, fmt.Sprintf("lifecycle-metadata SBOM presence differs: reference HasSBOM=%t, candidate HasSBOM=%t", reference.HasSBOM, candidate.HasSBOM))
	} else if reference.HasSBOM && reference.SBOMDiffID != candidate.SBOMDiffID {
		diffs = append(diffs, fmt.Sprintf("lifecycle-metadata SBOM diff ID differs: reference %q, candidate %q", reference.SBOMDiffID, candidate.SBOMDiffID))
	}

	return diffs
}

// compareConfigDiffIDs cross-checks the config RootFS diff IDs (the actual image
// layers, in order) across modes. These must match: if the lifecycle-metadata
// diff IDs agreed but the config diff IDs did not, the on-disk image would
// disagree with its own recorded rebase metadata.
func compareConfigDiffIDs(reference, candidate []string) []string {
	return compareStringSlices("config RootFS layer diff ID", reference, candidate)
}

// compareImageConfig compares the runtime image config fields FR-8 requires to
// match: entrypoint, env, user, working dir, and exposed ports. Order-sensitive
// fields (Entrypoint, Env) are compared in order; ExposedPorts is already sorted
// by the inspector so its comparison is order-independent by construction.
func compareImageConfig(reference, candidate OCIImageConfig) []string {
	var diffs []string

	diffs = append(diffs, compareStringSlices("config Entrypoint element", reference.Entrypoint, candidate.Entrypoint)...)
	diffs = append(diffs, compareStringSlices("config Env element", reference.Env, candidate.Env)...)
	diffs = append(diffs, compareStringSlices("config ExposedPorts element", reference.ExposedPorts, candidate.ExposedPorts)...)

	if reference.User != candidate.User {
		diffs = append(diffs, fmt.Sprintf("config User differs: reference %q, candidate %q", reference.User, candidate.User))
	}
	if reference.WorkingDir != candidate.WorkingDir {
		diffs = append(diffs, fmt.Sprintf("config WorkingDir differs: reference %q, candidate %q", reference.WorkingDir, candidate.WorkingDir))
	}

	return diffs
}

// compareLabels compares the image config labels across modes for exact
// equality, with ONE deliberate exclusion: the raw io.buildpacks.lifecycle.metadata
// label. That label's parity is asserted via its EXTRACTED diff IDs
// (compareLifecycleMetadata), because its raw JSON legitimately differs in
// non-layer fields (runImage.reference, timestamps) for images that are
// nonetheless rebase-equivalent. Every other label — io.buildpacks.stack.id,
// builder/project metadata, etc. — must be byte-equal, since those describe the
// build identity/inputs and must be the same for equivalent images.
func compareLabels(reference, candidate map[string]string) []string {
	var diffs []string

	// Collect the union of label keys (excluding the lifecycle-metadata label),
	// sorted for deterministic output.
	keys := make(map[string]struct{}, len(reference)+len(candidate))
	for k := range reference {
		if k == lifecycleMetadataLabel {
			continue
		}
		keys[k] = struct{}{}
	}
	for k := range candidate {
		if k == lifecycleMetadataLabel {
			continue
		}
		keys[k] = struct{}{}
	}

	sortedKeys := make([]string, 0, len(keys))
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	for _, k := range sortedKeys {
		refVal, refHas := reference[k]
		candVal, candHas := candidate[k]
		switch {
		case refHas && !candHas:
			diffs = append(diffs, fmt.Sprintf("label %q present on reference (%q) but missing on candidate", k, refVal))
		case !refHas && candHas:
			diffs = append(diffs, fmt.Sprintf("label %q present on candidate (%q) but missing on reference", k, candVal))
		case refVal != candVal:
			diffs = append(diffs, fmt.Sprintf("label %q differs: reference %q, candidate %q", k, refVal, candVal))
		}
	}

	return diffs
}

// compareStringSlices compares two ordered string slices element-by-element and
// returns a human-readable difference for each divergence, labeled with what.
// A length difference is reported once; per-index value differences are reported
// for each mismatched position up to the shorter length. This gives a caller a
// precise list ("<what> 2 differs: ...") rather than a single opaque failure.
func compareStringSlices(what string, reference, candidate []string) []string {
	var diffs []string

	if len(reference) != len(candidate) {
		diffs = append(diffs, fmt.Sprintf("%s count differs: reference has %d, candidate has %d", what, len(reference), len(candidate)))
	}

	min := len(reference)
	if len(candidate) < min {
		min = len(candidate)
	}
	for i := 0; i < min; i++ {
		if reference[i] != candidate[i] {
			diffs = append(diffs, fmt.Sprintf("%s %d differs: reference %q, candidate %q", what, i, reference[i], candidate[i]))
		}
	}

	return diffs
}

// CompareParityLayouts is a convenience wrapper that inspects two on-disk OCI
// layout directories and compares them for parity in one call. It is the
// directory-based entry point (Deliverable A "takes two on-disk layout
// directories"), for callers/tests that have paths rather than pre-built
// OCILayoutInspection values.
//
// If either directory fails to inspect (not a valid/complete layout), the
// returned report records that as a difference (Match()==false) and the error is
// returned as well so callers can distinguish an I/O/parse failure from a plain
// parity mismatch.
func CompareParityLayouts(referenceDir, candidateDir string) (ParityReport, error) {
	report := ParityReport{ReferenceLayoutDir: referenceDir, CandidateLayoutDir: candidateDir}

	reference, refErr := InspectOCILayout(referenceDir)
	candidate, candErr := InspectOCILayout(candidateDir)

	if refErr != nil {
		report.Differences = append(report.Differences, fmt.Sprintf("inspecting reference layout %s: %s", referenceDir, refErr))
	}
	if candErr != nil {
		report.Differences = append(report.Differences, fmt.Sprintf("inspecting candidate layout %s: %s", candidateDir, candErr))
	}
	if refErr != nil || candErr != nil {
		// Return the first inspection error so callers can surface it; the report
		// already records the failure as a non-match.
		if refErr != nil {
			return report, refErr
		}
		return report, candErr
	}

	return CompareParity(reference, candidate), nil
}
