package multiplatform

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// This file stages the app build context for BuildKit when a project-descriptor
// file filter (pack --descriptor include/exclude) is in effect.
//
// Why not fsutil.NewFilterFS(Map)? Wrapping the app FS in a FilterFS with a Map
// callback can make fsutil's diff/send protocol deliver directory-walk entries in
// an order BuildKit's receiver rejects with "changes out of order" for certain
// nested layouts (e.g. a file at the app root plus a nested package dir like
// src/<pkg>/...). Instead we copy the KEPT files into a fresh temp dir and hand
// BuildKit a plain fsutil.NewFS over that dir, which walks in the standard sorted
// (parents-before-children) order the receiver accepts. This mirrors what the
// daemon backend does (it builds a filtered tar), and keeps the fix isolated from
// the vendored fsutil internals.

// stageFilteredAppDir copies the entries of srcDir that satisfy the keep-predicate
// filter into a new temporary directory, preserving relative layout, file modes,
// and symlinks. It returns the temp dir path and a cleanup func the caller MUST
// defer. filter receives paths RELATIVE to srcDir with no leading slash (matching
// the daemon backend's CopyDir filter and fsutil's path convention).
func stageFilteredAppDir(srcDir string, filter func(string) bool) (string, func(), error) {
	stageDir, err := os.MkdirTemp("", "pack-bk-appctx-")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating staged app context dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stageDir) }

	walkErr := filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(srcDir, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil // skip the root itself
		}
		// Directories are created on demand when a kept file needs them (below), so
		// an empty or fully-excluded directory is simply omitted — matching the
		// daemon backend, which only tars kept files.
		if info.IsDir() {
			return nil
		}
		if !filter(rel) {
			return nil
		}
		return copyStagedEntry(srcDir, stageDir, rel, info)
	})
	if walkErr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("staging filtered app context from %s: %w", srcDir, walkErr)
	}
	return stageDir, cleanup, nil
}

// copyStagedEntry copies one kept entry (file or symlink) from srcDir/rel into
// stageDir/rel, creating parent dirs as needed and preserving the file mode.
func copyStagedEntry(srcDir, stageDir, rel string, info os.FileInfo) error {
	srcPath := filepath.Join(srcDir, rel)
	destPath := filepath.Join(stageDir, rel)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("creating staged parent dir for %s: %w", rel, err)
	}

	// Preserve symlinks as symlinks (the daemon tar does too); do not follow them.
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(srcPath)
		if err != nil {
			return fmt.Errorf("reading symlink %s: %w", rel, err)
		}
		if err := os.Symlink(target, destPath); err != nil {
			return fmt.Errorf("staging symlink %s: %w", rel, err)
		}
		return nil
	}

	// Regular file: copy bytes + mode.
	if !info.Mode().IsRegular() {
		// Skip non-regular, non-symlink entries (sockets, devices, pipes); the app
		// context should not contain these, and BuildKit would reject them anyway.
		return nil
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", rel, err)
	}
	defer func() { _ = in.Close() }() // read-only; close error is not meaningful
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("creating staged file %s: %w", rel, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying %s: %w", rel, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing staged file %s: %w", rel, err)
	}
	return nil
}
