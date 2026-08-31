package builder_test

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	h "github.com/buildpacks/pack/testhelpers"

	"github.com/buildpacks/pack/internal/builder"
	"github.com/buildpacks/pack/pkg/dist"
)

func TestOrderTOML(t *testing.T) {
	t.Run("produces canonical order.toml matching builder's own serializer", func(t *testing.T) {
		order := dist.Order{
			{
				Group: []dist.ModuleRef{
					{ModuleInfo: dist.ModuleInfo{ID: "paketo-buildpacks/go", Version: "4.19.24"}},
					{ModuleInfo: dist.ModuleInfo{ID: "example/inject-marker", Version: "0.0.1"}, Optional: true},
				},
			},
			{
				Group: []dist.ModuleRef{
					{ModuleInfo: dist.ModuleInfo{ID: "paketo-buildpacks/procfile", Version: "5.13.6"}},
				},
			},
		}
		orderExt := dist.Order{
			{
				Group: []dist.ModuleRef{
					{ModuleInfo: dist.ModuleInfo{ID: "my/extension", Version: "1.0.0"}},
				},
			},
		}

		out, err := builder.OrderTOML(order, orderExt)
		h.AssertNil(t, err)

		// Verify it round-trips through TOML parsing.
		type groupEntry struct {
			ID       string `toml:"id"`
			Version  string `toml:"version"`
			Optional bool   `toml:"optional,omitempty"`
		}
		type orderEntry struct {
			Group []groupEntry `toml:"group"`
		}
		type parsed struct {
			Order    []orderEntry `toml:"order"`
			OrderExt []orderEntry `toml:"order-extensions"`
		}

		var p parsed
		_, err = toml.Decode(out, &p)
		h.AssertNil(t, err)

		// Order
		h.AssertEq(t, len(p.Order), 2)
		h.AssertEq(t, len(p.Order[0].Group), 2)
		h.AssertEq(t, p.Order[0].Group[0].ID, "paketo-buildpacks/go")
		h.AssertEq(t, p.Order[0].Group[0].Version, "4.19.24")
		h.AssertEq(t, p.Order[0].Group[1].ID, "example/inject-marker")
		h.AssertEq(t, p.Order[0].Group[1].Optional, true)
		h.AssertEq(t, len(p.Order[1].Group), 1)
		h.AssertEq(t, p.Order[1].Group[0].ID, "paketo-buildpacks/procfile")

		// OrderExt
		h.AssertEq(t, len(p.OrderExt), 1)
		h.AssertEq(t, p.OrderExt[0].Group[0].ID, "my/extension")
	})

	t.Run("empty order produces empty TOML", func(t *testing.T) {
		out, err := builder.OrderTOML(nil, nil)
		h.AssertNil(t, err)
		// Should be parseable but contain no order entries.
		h.AssertEq(t, true, !strings.Contains(out, "[[order]]"))
	})
}
