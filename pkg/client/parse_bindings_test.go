package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/heroku/color"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestParseBindings(t *testing.T) {
	color.Disable(true)
	defer color.Disable(false)
	spec.Run(t, "ParseBindings", testParseBindings, spec.Parallel(), spec.Report(report.Terminal{}))
}

func testParseBindings(t *testing.T, when spec.G, it spec.S) {
	var tmp string

	it.Before(func() {
		var err error
		tmp, err = os.MkdirTemp("", "bindings")
		h.AssertNil(t, err)
		// two valid binding dirs
		for _, n := range []string{"my-ca", "other"} {
			h.AssertNil(t, os.MkdirAll(filepath.Join(tmp, n), 0755))
			h.AssertNil(t, os.WriteFile(filepath.Join(tmp, n, "type"), []byte("ca-certificates"), 0644))
		}
	})

	it.After(func() {
		_ = os.RemoveAll(tmp)
	})

	when("no bindings", func() {
		it("returns empty", func() {
			out, err := parseBindings(nil)
			h.AssertNil(t, err)
			h.AssertEq(t, len(out), 0)
		})
	})

	when("a bare host path", func() {
		it("uses the directory base name as the binding name", func() {
			out, err := parseBindings([]string{filepath.Join(tmp, "my-ca")})
			h.AssertNil(t, err)
			h.AssertEq(t, len(out), 1)
			h.AssertEq(t, out[0].Name, "my-ca")
			h.AssertEq(t, out[0].HostPath, filepath.Join(tmp, "my-ca"))
		})
	})

	when("an explicit name=path", func() {
		it("uses the provided name", func() {
			out, err := parseBindings([]string{"cacerts=" + filepath.Join(tmp, "my-ca")})
			h.AssertNil(t, err)
			h.AssertEq(t, len(out), 1)
			h.AssertEq(t, out[0].Name, "cacerts")
		})
	})

	when("the host path does not exist", func() {
		it("errors", func() {
			_, err := parseBindings([]string{filepath.Join(tmp, "missing")})
			h.AssertNotNil(t, err)
			h.AssertError(t, err, "invalid --binding")
		})
	})

	when("the host path is a file, not a directory", func() {
		it("errors", func() {
			f := filepath.Join(tmp, "afile")
			h.AssertNil(t, os.WriteFile(f, []byte("x"), 0644))
			_, err := parseBindings([]string{f})
			h.AssertNotNil(t, err)
			h.AssertError(t, err, "not a directory")
		})
	})

	when("the binding name contains a path separator", func() {
		it("errors", func() {
			_, err := parseBindings([]string{"bad/name=" + filepath.Join(tmp, "my-ca")})
			h.AssertNotNil(t, err)
			h.AssertError(t, err, "must not contain a path separator")
		})
	})

	when("two bindings resolve to the same name", func() {
		it("errors on the duplicate", func() {
			_, err := parseBindings([]string{
				"dup=" + filepath.Join(tmp, "my-ca"),
				"dup=" + filepath.Join(tmp, "other"),
			})
			h.AssertNotNil(t, err)
			h.AssertError(t, err, "duplicate binding name")
		})
	})
}
