package multiplatform

import (
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestBuildkitClientInternal(t *testing.T) {
	spec.Run(t, "buildkit_client_internal", testBuildkitClientInternal, spec.Report(report.Terminal{}))
}

func testBuildkitClientInternal(t *testing.T, when spec.G, it spec.S) {
	when("archLabelFromVertexName", func() {
		it("extracts a leading [os/arch] label", func() {
			h.AssertEq(t, archLabelFromVertexName("[linux/arm64] lifecycle: analyzer"), "[linux/arm64]")
		})

		it("extracts a label with a variant", func() {
			h.AssertEq(t, archLabelFromVertexName("[linux/arm/v7] setup directories"), "[linux/arm/v7]")
		})

		it("returns empty when there is no bracketed prefix", func() {
			h.AssertEq(t, archLabelFromVertexName("resolve image config for docker.io/library/foo"), "")
		})

		it("returns empty for a bracketed token that is not a platform (no slash)", func() {
			h.AssertEq(t, archLabelFromVertexName("[internal] load metadata"), "")
		})

		it("returns empty for an empty bracket", func() {
			h.AssertEq(t, archLabelFromVertexName("[] weird"), "")
		})

		it("returns empty for an unterminated bracket", func() {
			h.AssertEq(t, archLabelFromVertexName("[linux/arm64 no close"), "")
		})

		it("only treats a LEADING bracket as the label", func() {
			// A bracket that appears mid-name must not be picked up.
			h.AssertEq(t, archLabelFromVertexName("exporting [linux/amd64] layers"), "")
		})
	})
}
