package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnarutou/gitrieve/internal/ui"
)

// cleanupFileReleases only prunes direct version directories. A tag containing
// separators reserves its entire top-level directory so retained assets survive.
func cleanupFileReleases(ctx context.Context, root string, tags []string) error {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing release cleanup: %q is not a real directory", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	retained := make(map[string]bool, len(tags))
	var retainedDirs []os.FileInfo
	for _, tag := range tags {
		// Treat both separators conservatively, including on Unix.
		name := strings.SplitN(strings.ReplaceAll(tag, `\`, "/"), "/", 2)[0]
		retained[name] = true
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			retainedDirs = append(retainedDirs, info)
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if retained[entry.Name()] {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing release cleanup of symlink %q", filepath.Join(root, entry.Name()))
		}
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Filesystem identity also protects case aliases (e.g. V2 and v2 on
		// Windows), without conflating distinct directories on Unix.
		keep := false
		for _, retainedDir := range retainedDirs {
			if os.SameFile(info, retainedDir) {
				keep = true
				break
			}
		}
		if keep {
			continue
		}
		target := filepath.Join(root, entry.Name())
		if err := removeReleaseDirectory(root, target); err != nil {
			return err
		}
		ui.Printf("Deleted directory %s", target)
	}
	return nil
}

// removeReleaseDirectory deliberately does not change Storage.DeleteObject's
// single-object semantics. Validate both lexical and resolved boundaries before
// recursively removing a direct child; RemoveAll does not follow nested symlinks.
func removeReleaseDirectory(root, target string) error {
	if root == "" || target == "" {
		return fmt.Errorf("refusing release cleanup with an empty path")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.ContainsAny(rel, `/\`) {
		return fmt.Errorf("refusing release cleanup outside a direct child of %q: %q", root, target)
	}
	rootInfo, err := os.Lstat(absRoot)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing release cleanup through non-directory %q", root)
	}
	info, err := os.Lstat(absTarget)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing release cleanup of non-directory %q", target)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return err
	}
	resolvedTarget, err := filepath.EvalSymlinks(absTarget)
	if err != nil {
		return err
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil || resolvedRel != rel {
		return fmt.Errorf("refusing release cleanup through redirected path %q", target)
	}
	return os.RemoveAll(resolvedTarget)
}
