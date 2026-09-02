package multiplatform

import (
	"context"
	gofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"
	"github.com/tonistiigi/fsutil"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestAppContextInternal(t *testing.T) {
	spec.Run(t, "appcontext_internal", testAppContextInternal, spec.Report(report.Terminal{}))
}

func testAppContextInternal(t *testing.T, when spec.G, it spec.S) {
	when("stageFilteredAppDir", func() {
		var srcDir string

		it.Before(func() {
			var err error
			srcDir, err = os.MkdirTemp("", "pack-appctx-src-")
			h.AssertNil(t, err)
			// A layout that reproduces the fsutil "changes out of order" failure: a
			// file at the app root (server.py) that sorts around a nested package dir
			// (src/pd_sample_python_app/...). See FOLLOWUPS #2.
			writeFile(t, srcDir, "server.py", "print('root')")
			writeFile(t, srcDir, "README.md", "# readme")
			writeFile(t, srcDir, "src/pd_sample_python_app/__init__.py", "")
			writeFile(t, srcDir, "src/pd_sample_python_app/server.py", "print('pkg')")
			writeFile(t, srcDir, "src/pd_sample_python_app/util/helpers.py", "x = 1")
		})

		it.After(func() {
			_ = os.RemoveAll(srcDir)
		})

		it("stages kept files and the resulting FS walks in an order fsutil accepts", func() {
			// keep everything
			stageDir, cleanup, err := stageFilteredAppDir(srcDir, func(string) bool { return true })
			defer cleanup()
			h.AssertNil(t, err)

			paths := walkStaged(t, stageDir)
			h.AssertSliceContains(t, paths, "server.py")
			h.AssertSliceContains(t, paths, "README.md")
			h.AssertSliceContains(t, paths, filepath.FromSlash("src/pd_sample_python_app/__init__.py"))
			h.AssertSliceContains(t, paths, filepath.FromSlash("src/pd_sample_python_app/server.py"))
			h.AssertSliceContains(t, paths, filepath.FromSlash("src/pd_sample_python_app/util/helpers.py"))

			// The regression: the staged dir must stream through fsutil's Validator
			// with NO "changes out of order" error (this is what BuildKit's receiver
			// enforces).
			assertFSUtilOrderValid(t, stageDir)
		})

		it("omits excluded files and still walks cleanly", func() {
			// exclude README.md and the nested __init__.py
			filter := func(rel string) bool {
				switch rel {
				case "README.md", filepath.FromSlash("src/pd_sample_python_app/__init__.py"):
					return false
				}
				return true
			}
			stageDir, cleanup, err := stageFilteredAppDir(srcDir, filter)
			defer cleanup()
			h.AssertNil(t, err)

			paths := walkStaged(t, stageDir)
			// excluded ones are gone
			for _, gone := range []string{"README.md", filepath.FromSlash("src/pd_sample_python_app/__init__.py")} {
				for _, p := range paths {
					if p == gone {
						t.Fatalf("expected %q to be excluded from staged dir, but it was present", gone)
					}
				}
			}
			// kept ones remain
			h.AssertSliceContains(t, paths, "server.py")
			h.AssertSliceContains(t, paths, filepath.FromSlash("src/pd_sample_python_app/server.py"))
			h.AssertSliceContains(t, paths, filepath.FromSlash("src/pd_sample_python_app/util/helpers.py"))

			assertFSUtilOrderValid(t, stageDir)
		})

		it("preserves symlinks as symlinks", func() {
			// add a symlink into the source tree
			h.AssertNil(t, os.Symlink("server.py", filepath.Join(srcDir, "link-to-server")))
			stageDir, cleanup, err := stageFilteredAppDir(srcDir, func(string) bool { return true })
			defer cleanup()
			h.AssertNil(t, err)

			fi, err := os.Lstat(filepath.Join(stageDir, "link-to-server"))
			h.AssertNil(t, err)
			if fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("expected link-to-server to be staged as a symlink")
			}
			target, err := os.Readlink(filepath.Join(stageDir, "link-to-server"))
			h.AssertNil(t, err)
			h.AssertEq(t, target, "server.py")
		})
	})
}

// writeFile writes content to dir/rel, creating parent dirs.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	h.AssertNil(t, os.MkdirAll(filepath.Dir(p), 0o755))
	h.AssertNil(t, os.WriteFile(p, []byte(content), 0o644))
}

// walkStaged returns the sorted relative file paths in a staged dir (files only).
func walkStaged(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, rel)
		return nil
	})
	h.AssertNil(t, err)
	sort.Strings(out)
	return out
}

// assertFSUtilOrderValid walks the dir through fsutil (the exact library BuildKit
// uses to stream a local context) and runs each change through fsutil.Validator,
// which enforces the same parents-before-children ordering BuildKit's receiver
// rejects with "changes out of order". A clean pass is the regression guarantee.
func assertFSUtilOrderValid(t *testing.T, dir string) {
	t.Helper()
	fsys, err := fsutil.NewFS(dir)
	h.AssertNil(t, err)
	var v fsutil.Validator
	err = fsys.Walk(context.Background(), "", func(p string, e gofs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		fi, ierr := e.Info()
		if ierr != nil {
			return ierr
		}
		return v.HandleChange(fsutil.ChangeKindAdd, p, fi, nil)
	})
	h.AssertNil(t, err)
}
