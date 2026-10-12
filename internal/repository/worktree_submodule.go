package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/storage"
)

var errManagedIndexRecovery = errors.New("managed index update failed")

// Parent sync does not recursively update submodules. Present them as
// uninitialized to go-git so status compares the parent's gitlink, rather than
// the child's HEAD. The view shares refs, objects, index and files, but never
// changes the stored configuration or the child's repository.
type parentWorktreeStorage struct {
	storage.Storer
	filesystem billy.Filesystem
}

func (s parentWorktreeStorage) Config() (*config.Config, error) {
	cfg, err := s.Storer.Config()
	if err != nil {
		return nil, err
	}
	view := *cfg
	view.Submodules = nil
	return &view, nil
}

func (s parentWorktreeStorage) Index() (*index.Index, error) {
	idx, err := s.Storer.Index()
	if err != nil {
		return nil, err
	}
	// Memory storage returns its live index pointer. Reset must build a copy
	// so SetIndex can check (and retain) the original before accepting writes.
	view := *idx
	view.Entries = make([]*index.Entry, len(idx.Entries))
	for i, entry := range idx.Entries {
		copy := *entry
		view.Entries[i] = &copy
	}
	return &view, nil
}

// Reset rebuilds the index before touching files, then recursively removes
// paths that disappear from the target tree. Reject dropping a populated
// gitlink at that boundary, retaining the old index for safe recovery/retries.
func (s parentWorktreeStorage) SetIndex(next *index.Index) error {
	previous, err := s.Storer.Index()
	if err != nil {
		return err
	}
	gitlinks := make(map[string]bool)
	for _, entry := range next.Entries {
		if entry.Mode == filemode.Submodule {
			gitlinks[entry.Name] = true
		}
	}
	for _, entry := range previous.Entries {
		if entry.Mode != filemode.Submodule || gitlinks[entry.Name] {
			continue
		}
		children, err := s.filesystem.ReadDir(entry.Name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect transitioning submodule %s: %w", entry.Name, err)
		}
		if len(children) != 0 {
			return fmt.Errorf("cannot remove or replace populated submodule %s during managed sync", entry.Name)
		}
	}
	return s.Storer.SetIndex(next)
}

func parentManagedWorktree(repo *git.Repository, worktree *git.Worktree) (*git.Worktree, error) {
	view, err := git.Open(parentWorktreeStorage{Storer: repo.Storer, filesystem: worktree.Filesystem}, worktree.Filesystem)
	if err != nil {
		return nil, fmt.Errorf("open managed parent worktree: %w", err)
	}
	parent, err := view.Worktree()
	if err != nil {
		return nil, fmt.Errorf("read managed parent worktree: %w", err)
	}
	parent.Excludes = worktree.Excludes
	return parent, nil
}

func checkoutManagedWorktree(repo *git.Repository, worktree *git.Worktree, opts *git.CheckoutOptions) error {
	if _, err := unmappedManagedGitlinks(repo, worktree); err != nil {
		return err
	}
	worktree, err := parentManagedWorktree(repo, worktree)
	if err != nil {
		return err
	}
	return worktree.Checkout(opts)
}

// Missing .gitmodules mappings are valid gitlinks to archive, but go-git
// treats their empty directories as deleted. Check before resetting: a reset
// could otherwise traverse an unmapped submodule and delete its contents.
func unmappedManagedGitlinks(repo *git.Repository, worktree *git.Worktree) (map[string]bool, error) {
	gitlinks, err := managedGitlinks(repo)
	if err != nil {
		return nil, err
	}
	modules, err := worktree.Submodules()
	if err != nil {
		return nil, fmt.Errorf("read managed submodule mappings: %w", err)
	}
	mapped := make(map[string]bool, len(modules))
	for _, module := range modules {
		mapped[module.Config().Path] = true
	}
	unmapped := make(map[string]bool)
	for name := range gitlinks {
		info, err := worktree.Filesystem.Lstat(name)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect submodule %s: %w", name, err)
		}
		if err == nil && !info.IsDir() {
			return nil, fmt.Errorf("submodule path %s is not a directory", name)
		}
		if mapped[name] {
			continue
		}
		if err == nil {
			children, err := worktree.Filesystem.ReadDir(name)
			if err != nil {
				return nil, fmt.Errorf("inspect unmapped submodule %s: %w", name, err)
			}
			if len(children) != 0 {
				return nil, fmt.Errorf("unmapped submodule %s contains local files", name)
			}
		}
		unmapped[name] = true
	}
	return unmapped, nil
}

// Keep go-git's fetch and fast-forward checks, but exclude empty, unmapped
// gitlinks from its unstaged-file check. They stay in commit trees and are
// rebuilt in the index by Pull's reset. Restore the index even if Pull exits
// early; use the current LOCAL HEAD, since Pull may already have advanced it.
func pullManagedWorktree(ctx context.Context, repo *git.Repository, worktree *git.Worktree, opts *git.PullOptions) (result error) {
	unmapped, err := unmappedManagedGitlinks(repo, worktree)
	if err != nil {
		return err
	}
	worktree, err = parentManagedWorktree(repo, worktree)
	if err != nil {
		return err
	}
	if len(unmapped) == 0 {
		return worktree.PullContext(ctx, opts)
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	filtered := *idx
	filtered.Entries = make([]*index.Entry, 0, len(idx.Entries))
	for _, entry := range idx.Entries {
		if !unmapped[entry.Name] {
			filtered.Entries = append(filtered.Entries, entry)
		}
	}
	if err := repo.Storer.SetIndex(&filtered); err != nil {
		// SetIndex can truncate the on-disk index before failing. Restore
		// from the saved copy and abort this sync even if restoration succeeds.
		restoreErr := repo.Storer.SetIndex(idx)
		return errors.Join(fmt.Errorf("%w: prepare pull: %w", errManagedIndexRecovery, err), restoreErr)
	}
	defer func() {
		if err := worktree.Reset(&git.ResetOptions{Mode: git.MixedReset}); err != nil {
			result = errors.Join(result, fmt.Errorf("%w: %w", errManagedIndexRecovery, err))
		}
	}()
	return worktree.PullContext(ctx, opts)
}

func managedGitlinks(repo *git.Repository) (map[string]bool, error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, fmt.Errorf("read managed gitlinks: %w", err)
	}
	gitlinks := make(map[string]bool)
	for _, entry := range idx.Entries {
		if entry.Mode == filemode.Submodule {
			gitlinks[entry.Name] = true
		}
	}
	return gitlinks, nil
}

// go-git's Clean(Dir: true) removes even tracked, empty gitlink directories
// and traverses populated submodules. Keep those subtrees opaque: this sync
// archives the parent repository without recursively fetching submodules.
func cleanManagedWorktree(worktree *git.Worktree, gitlinks map[string]bool) error {
	status, err := worktree.Status()
	if err != nil {
		return err
	}
	var clean func(string) error
	clean = func(dir string) error {
		children, err := worktree.Filesystem.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, child := range children {
			name := path.Join(dir, child.Name())
			if child.Name() == ".git" || gitlinks[name] {
				continue
			}
			if child.IsDir() {
				if err := clean(name); err != nil {
					return err
				}
			} else if status.IsUntracked(name) {
				if err := worktree.Filesystem.Remove(name); err != nil {
					return err
				}
			}
		}
		if dir != "" {
			remaining, err := worktree.Filesystem.ReadDir(dir)
			if err != nil {
				return err
			}
			if len(remaining) == 0 {
				return worktree.Filesystem.Remove(dir)
			}
		}
		return nil
	}
	if err := clean(""); err != nil {
		return err
	}
	for name := range gitlinks {
		if err := worktree.Filesystem.MkdirAll(name, 0o755); err != nil {
			return fmt.Errorf("restore submodule directory %s: %w", name, err)
		}
	}
	return nil
}
