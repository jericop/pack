package multiplatform

// Offline rebase-readiness check for OCI layout mode (spec Task 9, Deliverable A,
// design "Rebase tests", FR-7).
//
// This file implements a PRECONDITION check, not the rebase itself. The actual
// `pack rebase` operation swaps an image's run-image (base) layers for a new run
// image's layers WITHOUT re-running buildpacks; it needs the lifecycle/rebaser
// and real images (a daemon, and possibly a registry for remote layer mounting).
// None of that runs in a unit test. But the design's rebase-parity claim (FR-7)
// rests entirely on WHAT the built image RECORDS in its
// io.buildpacks.lifecycle.metadata label, and that is fully checkable offline
// from the on-disk OCI layout.
//
// # What "rebase-ready" means (per the CNB rebase contract)
//
// The rebaser (see cnb-lifecycle steering layer-order-and-rebase.md):
//  1. reads io.buildpacks.lifecycle.metadata from the app image,
//  2. finds the run-image boundary using runImage.topLayer (the diff ID of the
//     TOP run-image layer) and identifies the run image via runImage.reference,
//  3. replaces every layer up to and including that boundary with the new run
//     image's layers, keeping the app/launcher/buildpack layers above it.
//
// For that to succeed the built image MUST record a well-formed boundary and the
// layers must be coherent. This checker asserts, purely from on-disk data:
//
//   - lifecycle metadata is present and parsed (there is a rebase contract at all);
//   - runImage.topLayer is non-empty (the rebaser has a boundary to cut at);
//   - runImage.reference is non-empty (the rebaser can identify the old run image);
//   - the recorded topLayer diff ID actually appears among the image's config
//     RootFS diff IDs — i.e. the boundary layer really EXISTS in the image, so
//     the cut point is coherent (a strong, offline-checkable precondition);
//   - the lifecycle metadata records at least one non-run-image layer (app /
//     launcher / buildpack), so there is something ABOVE the boundary to preserve
//     — a "rebase" that preserved nothing would be meaningless.
//
// It deliberately does NOT try to fetch the new run image, mount layers, or
// mutate anything: that is the rebaser's job. A Ready=true result means "this
// image satisfies the recorded-metadata preconditions the rebaser relies on";
// the end-to-end rebase execution is exercised by the rebaser / acceptance suite
// (and, when a rebaser + new run image are wired in, by the daemon-gated
// integration test in oci_layout_rebase_integration_test.go).

import (
	"fmt"
	"sort"
	"strings"
)

// RebaseReadiness is the structured result of the offline rebase-precondition
// check. Like ParityReport it favors actionable output over a bare bool: when
// Ready is false, Reasons lists each unmet precondition so a caller/test can see
// exactly why the image would not be rebasable.
type RebaseReadiness struct {
	// LayoutDir is the on-disk OCI layout the readiness was computed from (empty
	// when the readiness was computed from an inspection with no LayoutDir).
	LayoutDir string

	// Ready is true exactly when no unmet preconditions were recorded.
	Ready bool

	// Reasons lists every unmet precondition, one human-readable entry each.
	// Empty when Ready is true.
	Reasons []string
}

// Error returns a single formatted string describing all unmet preconditions, or
// the empty string when Ready. Convenient for building a test failure message.
func (r RebaseReadiness) Error() string {
	if r.Ready {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "OCI layout at %s is not rebase-ready: %d unmet precondition(s):", r.LayoutDir, len(r.Reasons))
	for _, reason := range r.Reasons {
		b.WriteString("\n  - ")
		b.WriteString(reason)
	}
	return b.String()
}

// CheckRebaseReadiness evaluates the offline rebase preconditions (FR-7) for a
// single inspected OCI layout. It is a pure function of the OCILayoutInspection —
// no I/O — so it is unit-testable without a daemon or registry.
//
// This is a PRECONDITION check, not a rebase. See the file doc for the exact
// contract; a Ready=true result means the image records a coherent run-image
// boundary (runImage.topLayer + reference) that exists among the image layers,
// with at least one preserved layer above it.
func CheckRebaseReadiness(inspection OCILayoutInspection) RebaseReadiness {
	result := RebaseReadiness{LayoutDir: inspection.LayoutDir}

	// Rebase preconditions can only be evaluated over a complete, resolvable
	// image. An incomplete inspection means InspectOCILayout could not validate
	// the layout, so we surface that rather than reporting a misleading "ready".
	if !inspection.Complete {
		result.Reasons = append(result.Reasons, "layout is not a complete image (InspectOCILayout reported Complete=false)")
		return result
	}

	md := inspection.LifecycleMetadata

	// (1) There must be a lifecycle metadata contract to rebase against at all.
	if !md.Present {
		result.Reasons = append(result.Reasons, "io.buildpacks.lifecycle.metadata label is absent or unparseable; the rebaser has no metadata to read")
		// Without the label none of the boundary checks below are meaningful.
		return result
	}

	// (2) The rebaser needs a boundary diff ID to cut at.
	if md.RunImageTopLayer == "" {
		result.Reasons = append(result.Reasons, "lifecycle metadata runImage.topLayer is empty; the rebaser cannot locate the run-image boundary")
	}

	// (3) The rebaser needs a reference to identify the old run image.
	if md.RunImageReference == "" {
		result.Reasons = append(result.Reasons, "lifecycle metadata runImage.reference is empty; the rebaser cannot identify the run image being replaced")
	}

	// (4) The recorded boundary layer must actually exist among the image's
	// config RootFS diff IDs — otherwise the cut point is incoherent (the
	// metadata claims a boundary the image does not contain).
	if md.RunImageTopLayer != "" && !containsString(inspection.DiffIDs, md.RunImageTopLayer) {
		result.Reasons = append(result.Reasons, fmt.Sprintf(
			"recorded runImage.topLayer %q is not among the image config RootFS diff IDs (the run-image boundary layer is missing from the image)",
			md.RunImageTopLayer,
		))
	}

	// (5) There must be at least one non-run-image layer recorded (app / launcher
	// / buildpack) so that something is preserved ABOVE the boundary. The parsed
	// LifecycleMetadata.DiffIDs are exactly those non-run-image layers (app,
	// config, launcher, buildpack layers) — the run-image layers are NOT included
	// in that slice — so a non-empty slice proves there is a preserved set.
	if len(md.DiffIDs) == 0 {
		result.Reasons = append(result.Reasons, "lifecycle metadata records no app/launcher/buildpack layers above the run-image boundary; there is nothing to preserve on rebase")
	}

	result.Ready = len(result.Reasons) == 0
	return result
}

// CheckRebaseReadinessLayout is the directory-based convenience wrapper: it
// inspects an on-disk OCI layout and evaluates rebase readiness in one call, for
// callers/tests that have a path rather than a pre-built OCILayoutInspection.
//
// If the directory fails to inspect (not a valid/complete layout), the returned
// readiness records that as a reason (Ready==false) and the inspection error is
// returned as well so callers can distinguish an I/O/parse failure from an
// image that merely lacks the rebase metadata.
func CheckRebaseReadinessLayout(layoutDir string) (RebaseReadiness, error) {
	inspection, err := InspectOCILayout(layoutDir)
	if err != nil {
		return RebaseReadiness{
			LayoutDir: layoutDir,
			Ready:     false,
			Reasons:   []string{fmt.Sprintf("inspecting layout %s: %s", layoutDir, err)},
		}, err
	}
	return CheckRebaseReadiness(inspection), nil
}

// MultiArchRebaseReadiness is the aggregate rebase-readiness across all
// per-architecture layouts of a multi-arch build. It is the offline analogue of
// "multi-arch rebase → both platforms rebased" (Task 9): every platform image
// must independently record a coherent run-image boundary for a multi-arch
// rebase to succeed on every platform.
type MultiArchRebaseReadiness struct {
	// PerPlatform maps a platform string (os/arch[/variant]) to its readiness.
	PerPlatform map[string]RebaseReadiness

	// Ready is true exactly when EVERY platform is rebase-ready.
	Ready bool
}

// Error aggregates the not-ready platforms into a single message, or returns the
// empty string when every platform is rebase-ready.
func (m MultiArchRebaseReadiness) Error() string {
	if m.Ready {
		return ""
	}
	// Deterministic order for a stable message.
	platforms := make([]string, 0, len(m.PerPlatform))
	for p := range m.PerPlatform {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)

	var b strings.Builder
	b.WriteString("multi-arch rebase readiness failed: one or more platforms are not rebase-ready:")
	for _, p := range platforms {
		r := m.PerPlatform[p]
		if !r.Ready {
			fmt.Fprintf(&b, "\n[%s] %s", p, r.Error())
		}
	}
	return b.String()
}

// CheckMultiArchRebaseReadiness evaluates rebase readiness for every per-arch
// layout produced by a multi-arch build and reports whether ALL platforms are
// ready. It is the offline stand-in for verifying a multi-arch rebase would
// succeed on both platforms (Task 9): it inspects each result's on-disk
// OCIStoreDir and requires every one to satisfy the rebase preconditions.
//
// Results with an empty OCIStoreDir (e.g. a registry-mode result that produced
// no on-disk layout) are reported as not-ready with a clear reason rather than
// skipped, since a multi-arch OCI-layout rebase requires an inspectable layout
// per platform.
func CheckMultiArchRebaseReadiness(results []PlatformBuildResult) MultiArchRebaseReadiness {
	out := MultiArchRebaseReadiness{PerPlatform: make(map[string]RebaseReadiness, len(results))}

	allReady := len(results) > 0
	for _, res := range results {
		key := res.Platform.String()
		if res.OCIStoreDir == "" {
			out.PerPlatform[key] = RebaseReadiness{
				LayoutDir: "",
				Ready:     false,
				Reasons:   []string{"no on-disk OCI layout for this platform (OCIStoreDir is empty); cannot verify rebase readiness"},
			}
			allReady = false
			continue
		}
		readiness, _ := CheckRebaseReadinessLayout(res.OCIStoreDir)
		out.PerPlatform[key] = readiness
		if !readiness.Ready {
			allReady = false
		}
	}

	out.Ready = allReady
	return out
}

// containsString reports whether s is present in slice.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
