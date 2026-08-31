package multiplatform_test

import (
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	mp "github.com/buildpacks/pack/internal/build/multiplatform"
	h "github.com/buildpacks/pack/testhelpers"
)

func TestMultiplatform(t *testing.T) {
	spec.Run(t, "multiplatform", testMultiplatform, spec.Report(report.Terminal{}))
}

func testMultiplatform(t *testing.T, when spec.G, it spec.S) {
	when("#ParsePlatform", func() {
		it("parses os/arch", func() {
			p, err := mp.ParsePlatform("linux/amd64")
			h.AssertNil(t, err)
			h.AssertEq(t, p.OS, "linux")
			h.AssertEq(t, p.Arch, "amd64")
			h.AssertEq(t, p.Variant, "")
		})

		it("parses os/arch/variant", func() {
			p, err := mp.ParsePlatform("linux/arm/v7")
			h.AssertNil(t, err)
			h.AssertEq(t, p.OS, "linux")
			h.AssertEq(t, p.Arch, "arm")
			h.AssertEq(t, p.Variant, "v7")
		})

		it("errors on invalid format", func() {
			_, err := mp.ParsePlatform("linux")
			h.AssertNotNil(t, err)
		})
	})

	when("#ParsePlatforms", func() {
		it("parses comma-separated platforms", func() {
			platforms, err := mp.ParsePlatforms("linux/amd64,linux/arm64")
			h.AssertNil(t, err)
			h.AssertEq(t, len(platforms), 2)
			h.AssertEq(t, platforms[0].Arch, "amd64")
			h.AssertEq(t, platforms[1].Arch, "arm64")
		})

		it("trims whitespace", func() {
			platforms, err := mp.ParsePlatforms(" linux/amd64 , linux/arm64 ")
			h.AssertNil(t, err)
			h.AssertEq(t, len(platforms), 2)
		})

		it("errors on empty string", func() {
			_, err := mp.ParsePlatforms("")
			h.AssertNotNil(t, err)
		})
	})

	when("#Platform.String", func() {
		it("formats without variant", func() {
			p := mp.Platform{OS: "linux", Arch: "amd64"}
			h.AssertEq(t, p.String(), "linux/amd64")
		})

		it("formats with variant", func() {
			p := mp.Platform{OS: "linux", Arch: "arm", Variant: "v7"}
			h.AssertEq(t, p.String(), "linux/arm/v7")
		})
	})
}
