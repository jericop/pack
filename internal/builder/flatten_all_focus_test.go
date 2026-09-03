package builder_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildpacks/imgutil/fakes"
	"github.com/buildpacks/lifecycle/api"
	"github.com/golang/mock/gomock"

	"github.com/buildpacks/pack/internal/builder"
	"github.com/buildpacks/pack/internal/builder/testmocks"
	ifakes "github.com/buildpacks/pack/internal/fakes"
	"github.com/buildpacks/pack/pkg/archive"
	"github.com/buildpacks/pack/pkg/buildpack"
	"github.com/buildpacks/pack/pkg/dist"
	"github.com/buildpacks/pack/pkg/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

// TestFlattenAllModulesFocus is a fast, standalone regression test for FR-7 / AC-2.
// It does NOT use the (very large, parallel) testBuilder spec suite so it can be run
// on its own quickly.
func TestFlattenAllModulesFocus(t *testing.T) {
	var outBuf bytes.Buffer
	logger := logging.NewLogWithWriters(&outBuf, &outBuf)

	baseImage := fakes.NewImage("base/image", "", nil)
	defer baseImage.Cleanup()

	h.AssertNil(t, baseImage.SetEnv("CNB_USER_ID", "1234"))
	h.AssertNil(t, baseImage.SetEnv("CNB_GROUP_ID", "4321"))
	h.AssertNil(t, baseImage.SetLabel("io.buildpacks.stack.id", "some.stack.id"))
	h.AssertNil(t, baseImage.SetLabel("io.buildpacks.stack.mixins", `["mixinX", "mixinY", "build:mixinA"]`))

	mockController := gomock.NewController(t)
	defer mockController.Finish()

	lifecycleTarReader := archive.ReadDirAsTar(
		filepath.Join("testdata", "lifecycle", "platform-0.4"),
		".", 0, 0, 0755, true, false, nil,
	)
	descriptorContents, err := os.ReadFile(filepath.Join("testdata", "lifecycle", "platform-0.4", "lifecycle.toml"))
	h.AssertNil(t, err)
	lifecycleDescriptor, err := builder.ParseDescriptor(string(descriptorContents))
	h.AssertNil(t, err)
	mockLifecycle := testmocks.NewMockLifecycle(mockController)
	mockLifecycle.EXPECT().Open().Return(lifecycleTarReader, nil).AnyTimes()
	mockLifecycle.EXPECT().Descriptor().Return(builder.CompatDescriptor(lifecycleDescriptor)).AnyTimes()

	newBP := func(id, version string) buildpack.BuildModule {
		bp, err := ifakes.NewFakeBuildpack(dist.BuildpackDescriptor{
			WithAPI:  api.MustParse("0.2"),
			WithInfo: dist.ModuleInfo{ID: id, Version: version},
			WithStacks: []dist.Stack{{
				ID:     "some.stack.id",
				Mixins: []string{"mixinX", "mixinY", "build:mixinA", "run:mixinB"},
			}},
		}, 0644)
		h.AssertNil(t, err)
		return bp
	}

	bp1v1 := newBP("buildpack-1-id", "buildpack-1-version-1")
	bp1v2 := newBP("buildpack-1-id", "buildpack-1-version-2")
	bp2v1 := newBP("buildpack-2-id", "buildpack-2-version-1")

	fakeLayerImage := &h.FakeAddedLayerImage{Image: baseImage}
	bldr, err := builder.New(fakeLayerImage, "some-builder", builder.WithFlattenAllModules())
	h.AssertNil(t, err)
	bldr.SetLifecycle(mockLifecycle)

	// Add 3 distinct modules.
	bldr.AddBuildpacks(bp1v1, []buildpack.BuildModule{bp2v1, bp1v2})

	h.AssertEq(t, len(bldr.FlattenedModules(buildpack.KindBuildpack)), 1)
	h.AssertEq(t, len(bldr.FlattenedModules(buildpack.KindBuildpack)[0]), 3)
	h.AssertTrue(t, bldr.ShouldFlatten(bp1v1))
	h.AssertTrue(t, bldr.ShouldFlatten(bp2v1))
	h.AssertTrue(t, bldr.ShouldFlatten(bp1v2))

	h.AssertNil(t, bldr.Save(logger, builder.CreatorMetadata{}))
	h.AssertEq(t, baseImage.IsSaved(), true)

	// 3 modules -> exactly ONE added module layer (O(1)).
	h.AssertEq(t, len(fakeLayerImage.AddedLayersOrder()), 1)
}
