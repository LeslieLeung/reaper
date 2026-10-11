package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

const testGitlinkPath = "runtime/gpu/tensorrt_fastertransformer/FasterTransformer"

func TestManagedRecoveryPreservesMismatchedSubmodule(t *testing.T) {
	for _, moduleName := range []string{"transformer", testGitlinkPath} {
		t.Run(moduleName, func(t *testing.T) {
			_, local, worktree, child := newMismatchedSubmoduleClone(t, moduleName, false)
			before, err := local.Head()
			require.NoError(t, err)
			childBefore, err := child.Head()
			require.NoError(t, err)
			cfgBefore, err := local.Config()
			require.NoError(t, err)
			status, err := worktree.Status()
			require.NoError(t, err)
			require.Equal(t, git.Modified, status.File(testGitlinkPath).Worktree)

			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, checkoutManagedWorktree(local, worktree, &git.CheckoutOptions{Branch: before.Name(), Force: true}))
				writeBillyFile(t, worktree.Filesystem, "tracked.txt", []byte("parent dirt"), 0o644)
				require.NoError(t, restoreManagedWorktree(local, worktree))
				require.Equal(t, "original", string(readBillyFile(t, worktree.Filesystem, "tracked.txt")))
			}
			after, err := local.Head()
			require.NoError(t, err)
			require.Equal(t, before, after)
			childAfter, err := child.Head()
			require.NoError(t, err)
			require.Equal(t, childBefore, childAfter)
			require.Equal(t, "local edits", string(readBillyFile(t, worktree.Filesystem, testGitlinkPath+"/tracked.txt")))
			require.Equal(t, "keep me", string(readBillyFile(t, worktree.Filesystem, testGitlinkPath+"/local.txt")))
			cfgAfter, err := local.Config()
			require.NoError(t, err)
			require.Equal(t, cfgBefore, cfgAfter, "the view must not persist configuration changes")
		})
	}
}

func TestManagedPullWithMismatchedSubmodule(t *testing.T) {
	for _, scenario := range []string{"update", "up to date", "ordinary dirt", "missing remote", "cancelled", "diverged"} {
		t.Run(scenario, func(t *testing.T) {
			remoteTree, local, worktree, child := newMismatchedSubmoduleClone(t, "transformer", false)
			before, err := local.Head()
			require.NoError(t, err)
			childBefore, err := child.Head()
			require.NoError(t, err)
			cfgBefore, err := local.Config()
			require.NoError(t, err)
			opts := &git.PullOptions{RemoteName: "origin"}
			ctx := context.Background()
			want := git.NoErrAlreadyUpToDate
			expected := plumbing.NewHash("f04cc0a2a167c2552318ff73b9a3b4fbad60e9c7")
			switch scenario {
			case "update", "ordinary dirt":
				remote, err := git.PlainOpen(remoteTree.Filesystem.Root())
				require.NoError(t, err)
				idx, err := remote.Storer.Index()
				require.NoError(t, err)
				entry, err := idx.Entry(testGitlinkPath)
				require.NoError(t, err)
				expected = plumbing.NewHash("1111111111111111111111111111111111111111")
				entry.Hash = expected
				require.NoError(t, remote.Storer.SetIndex(idx))
				commitTestFile(t, remoteTree, "tracked.txt", "upstream update")
				want = nil
				if scenario == "ordinary dirt" {
					writeBillyFile(t, worktree.Filesystem, "tracked.txt", []byte("parent dirt"), 0o644)
					want = git.ErrUnstagedChanges
				}
			case "missing remote":
				opts.RemoteName = "missing"
				want = git.ErrRemoteNotFound
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "diverged":
				commitTestFile(t, remoteTree, "remote.txt", "upstream history")
				commitTestFile(t, worktree, "parent.txt", "local history")
				before, err = local.Head()
				require.NoError(t, err)
				want = git.ErrNonFastForwardUpdate
			}
			err = pullManagedWorktree(ctx, local, worktree, opts)
			if scenario == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
			} else if want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, want)
			}
			if scenario == "update" {
				require.Equal(t, "upstream update", string(readBillyFile(t, worktree.Filesystem, "tracked.txt")))
				require.NoError(t, restoreManagedWorktree(local, worktree))
			} else if scenario == "ordinary dirt" {
				require.Equal(t, "parent dirt", string(readBillyFile(t, worktree.Filesystem, "tracked.txt")))
			} else if scenario != "ordinary dirt" {
				after, err := local.Head()
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			if scenario != "ordinary dirt" {
				idx, err := local.Storer.Index()
				require.NoError(t, err)
				entry, err := idx.Entry(testGitlinkPath)
				require.NoError(t, err)
				require.Equal(t, filemode.Submodule, entry.Mode)
				require.Equal(t, expected, entry.Hash)
			}
			_, err = local.CommitObject(before.Hash())
			require.NoError(t, err, "old parent history must remain recoverable")
			childAfter, err := child.Head()
			require.NoError(t, err)
			require.Equal(t, childBefore, childAfter)
			require.Equal(t, "local edits", string(readBillyFile(t, worktree.Filesystem, testGitlinkPath+"/tracked.txt")))
			require.Equal(t, "keep me", string(readBillyFile(t, worktree.Filesystem, testGitlinkPath+"/local.txt")))
			cfgAfter, err := local.Config()
			require.NoError(t, err)
			require.Equal(t, cfgBefore, cfgAfter)
		})
	}
}

func TestManagedSubmoduleTransitionsPreserveLocalFiles(t *testing.T) {
	for _, transition := range []string{"removed", "replaced", "moved", "memory removal"} {
		t.Run(transition, func(t *testing.T) {
			remoteTree, local, worktree, child := newMismatchedSubmoduleClone(t, "transformer", transition == "memory removal")
			childBefore, err := child.Head()
			require.NoError(t, err)
			remote, err := git.PlainOpen(remoteTree.Filesystem.Root())
			require.NoError(t, err)
			idx, err := remote.Storer.Index()
			require.NoError(t, err)
			entry, err := idx.Remove(testGitlinkPath)
			require.NoError(t, err)
			if transition == "moved" {
				entry.Name = "moved-child"
				idx.Entries = append(idx.Entries, entry)
			}
			require.NoError(t, remote.Storer.SetIndex(idx))
			_, err = remoteTree.Remove(".gitmodules")
			require.NoError(t, err)
			if transition == "replaced" {
				writeBillyFile(t, remoteTree.Filesystem, testGitlinkPath+"/replacement.txt", []byte("upstream replacement"), 0o644)
				_, err = remoteTree.Add(testGitlinkPath + "/replacement.txt")
				require.NoError(t, err)
			}
			target, err := remoteTree.Commit("transition child", &git.CommitOptions{Author: testSignature()})
			require.NoError(t, err)
			require.Error(t, pullManagedWorktree(context.Background(), local, worktree, &git.PullOptions{RemoteName: "origin"}))
			// Pull may already advance the local HEAD. Neither recovery nor a
			// checkout retry may discard the now-unmapped child's local files.
			require.Error(t, restoreManagedWorktree(local, worktree))
			require.Error(t, checkoutManagedWorktree(local, worktree, &git.CheckoutOptions{Hash: target, Force: true}))
			require.Equal(t, "local edits", string(readBillyFile(t, worktree.Filesystem, testGitlinkPath+"/tracked.txt")))
			require.Equal(t, "keep me", string(readBillyFile(t, worktree.Filesystem, testGitlinkPath+"/local.txt")))
			childAfter, err := child.Head()
			require.NoError(t, err)
			require.Equal(t, childBefore, childAfter)
			idx, err = local.Storer.Index()
			require.NoError(t, err)
			_, err = idx.Entry(testGitlinkPath)
			require.NoError(t, err, "retain the gitlink for safe retries")
		})
	}
}

func newMismatchedSubmoduleClone(t *testing.T, moduleName string, inMemory bool) (*git.Worktree, *git.Repository, *git.Worktree, *git.Repository) {
	t.Helper()
	remote, err := git.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	remoteTree, err := remote.Worktree()
	require.NoError(t, err)
	commitTestFile(t, remoteTree, "tracked.txt", "original")
	addTestGitlink(t, remote, remoteTree, true)
	if moduleName != "transformer" {
		commitTestFile(t, remoteTree, ".gitmodules", "[submodule \""+moduleName+"\"]\n\tpath = "+testGitlinkPath+"\n\turl = https://example.invalid/transformer.git\n")
	}
	opts := &git.CloneOptions{URL: remoteTree.Filesystem.Root()}
	var local *git.Repository
	if inMemory {
		local, err = git.Clone(memory.NewStorage(), memfs.New(), opts)
	} else {
		local, err = git.PlainClone(filepath.Join(t.TempDir(), "clone"), false, opts)
	}
	require.NoError(t, err)
	worktree, err := local.Worktree()
	require.NoError(t, err)
	module, err := worktree.Submodule(moduleName)
	require.NoError(t, err)
	require.NoError(t, module.Init())
	child, err := module.Repository()
	require.NoError(t, err)
	childTree, err := child.Worktree()
	require.NoError(t, err)
	commitTestFile(t, childTree, "tracked.txt", "local submodule history")
	writeBillyFile(t, childTree.Filesystem, "tracked.txt", []byte("local edits"), 0o644)
	writeBillyFile(t, childTree.Filesystem, "local.txt", []byte("keep me"), 0o644)
	return remoteTree, local, worktree, child
}

func TestPullWithUnmappedGitlink(t *testing.T) {
	remote, err := git.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	remoteTree, err := remote.Worktree()
	require.NoError(t, err)
	writeBillyFile(t, remoteTree.Filesystem, "tracked.txt", []byte("old\n"), 0o644)
	_, err = remoteTree.Add("tracked.txt")
	require.NoError(t, err)
	oldCommit := addTestGitlink(t, remote, remoteTree, false)
	local, err := git.PlainClone(filepath.Join(t.TempDir(), "clone"), false, &git.CloneOptions{URL: remoteTree.Filesystem.Root()})
	require.NoError(t, err)
	localTree, err := local.Worktree()
	require.NoError(t, err)
	require.NoError(t, restoreManagedWorktree(local, localTree))

	writeBillyFile(t, remoteTree.Filesystem, "tracked.txt", []byte("new\n"), 0o644)
	_, err = remoteTree.Add("tracked.txt")
	require.NoError(t, err)
	idx, err := remote.Storer.Index()
	require.NoError(t, err)
	entry, err := idx.Entry(testGitlinkPath)
	require.NoError(t, err)
	entry.Hash = plumbing.NewHash("1111111111111111111111111111111111111111")
	require.NoError(t, remote.Storer.SetIndex(idx))
	newCommit, err := remoteTree.Commit("update code and submodule", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)

	require.NoError(t, pullManagedWorktree(context.Background(), local, localTree, &git.PullOptions{RemoteName: "origin"}))
	require.Equal(t, "new\n", string(readBillyFile(t, localTree.Filesystem, "tracked.txt")))
	idx, err = local.Storer.Index()
	require.NoError(t, err)
	entry, err = idx.Entry(testGitlinkPath)
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash("1111111111111111111111111111111111111111"), entry.Hash)
	head, err := local.Head()
	require.NoError(t, err)
	require.Equal(t, newCommit, head.Hash())
	_, err = local.CommitObject(oldCommit)
	require.NoError(t, err)
	require.NoError(t, restoreManagedWorktree(local, localTree))
}

func TestRestoreManagedWorktreeRejectsPopulatedUnmappedGitlink(t *testing.T) {
	repo, worktree, filesystem := newMemoryRepository(t)
	commit := addTestGitlink(t, repo, worktree, false)
	writeBillyFile(t, filesystem, testGitlinkPath+"/keep.txt", []byte("local submodule data"), 0o644)
	err := restoreManagedWorktreeWithChmod(repo, worktree, func(string, os.FileMode) error { return nil })
	require.Error(t, err)
	require.Equal(t, "local submodule data", string(readBillyFile(t, filesystem, testGitlinkPath+"/keep.txt")))
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, commit, head.Hash())
}

func TestCheckoutPreservesPopulatedUnmappedGitlink(t *testing.T) {
	repo, worktree, filesystem := newMemoryRepository(t)
	commit := addTestGitlink(t, repo, worktree, false)
	writeBillyFile(t, filesystem, testGitlinkPath+"/keep.txt", []byte("local data"), 0o644)
	err := checkoutManagedWorktree(repo, worktree, &git.CheckoutOptions{Hash: commit, Force: true})
	require.Error(t, err)
	require.Equal(t, "local data", string(readBillyFile(t, filesystem, testGitlinkPath+"/keep.txt")))
}

func TestPullRestoresIndexAfterFailedPreparation(t *testing.T) {
	repo, worktree, _ := newMemoryRepository(t)
	commit := addTestGitlink(t, repo, worktree, false)
	broken := &failIndexWriteStorage{Storer: repo.Storer, failAt: 1}
	repo.Storer = broken
	err := pullManagedWorktree(context.Background(), repo, worktree, &git.PullOptions{RemoteName: "missing"})
	require.ErrorIs(t, err, errManagedIndexRecovery)
	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	entry, err := idx.Entry(testGitlinkPath)
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash("f04cc0a2a167c2552318ff73b9a3b4fbad60e9c7"), entry.Hash)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, commit, head.Hash())
}

func TestPullDoesNotRetryFailedIndexRecovery(t *testing.T) {
	recovered := false
	err := pullWithManagedWorktreeRecovery(func() error {
		return errors.Join(git.ErrUnstagedChanges, errManagedIndexRecovery)
	}, func() error {
		recovered = true
		return nil
	})
	require.ErrorIs(t, err, errManagedIndexRecovery)
	require.False(t, recovered, "an index write failure must abort sync")
}

type failIndexWriteStorage struct {
	storage.Storer
	failAt int
	writes int
}

func (s *failIndexWriteStorage) SetIndex(idx *index.Index) error {
	s.writes++
	if s.writes == s.failAt {
		// Simulate a partial/truncated write, not an atomic rejected write.
		if err := s.Storer.SetIndex(&index.Index{Version: 2}); err != nil {
			return err
		}
		return errors.New("injected index write failure")
	}
	return s.Storer.SetIndex(idx)
}

func TestPullWithUnmappedGitlinkRestoresIndexOnEarlyExit(t *testing.T) {
	for _, scenario := range []string{"up to date", "missing remote", "cancelled", "diverged"} {
		t.Run(scenario, func(t *testing.T) {
			remoteTree, local, localTree := newGitlinkClone(t)
			opts := &git.PullOptions{RemoteName: "origin"}
			ctx := context.Background()
			want := git.NoErrAlreadyUpToDate
			switch scenario {
			case "missing remote":
				opts.RemoteName = "missing"
				want = git.ErrRemoteNotFound
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "diverged":
				commitTestFile(t, remoteTree, "remote.txt", "upstream change")
				commitTestFile(t, localTree, "local.txt", "retained local history")
				want = git.ErrNonFastForwardUpdate
			}
			before, err := local.Head()
			require.NoError(t, err)
			err = pullManagedWorktree(ctx, local, localTree, opts)
			if scenario == "cancelled" {
				require.Error(t, err)
			} else {
				require.ErrorIs(t, err, want)
				if scenario == "up to date" {
					require.Equal(t, git.NoErrAlreadyUpToDate, err, "preserve sentinel equality")
				}
			}
			after, err := local.Head()
			require.NoError(t, err)
			require.Equal(t, before, after)
			idx, err := local.Storer.Index()
			require.NoError(t, err)
			entry, err := idx.Entry(testGitlinkPath)
			require.NoError(t, err)
			require.Equal(t, filemode.Submodule, entry.Mode)
			require.Equal(t, plumbing.NewHash("f04cc0a2a167c2552318ff73b9a3b4fbad60e9c7"), entry.Hash)
			require.NoError(t, restoreManagedWorktree(local, localTree))
		})
	}
}

func TestPullWithUnmappedGitlinkDoesNotHideOrdinaryChanges(t *testing.T) {
	remoteTree, local, localTree := newGitlinkClone(t)
	commitTestFile(t, remoteTree, "tracked.txt", "new upstream content")
	writeBillyFile(t, localTree.Filesystem, "tracked.txt", []byte("local dirt"), 0o644)
	err := pullManagedWorktree(context.Background(), local, localTree, &git.PullOptions{RemoteName: "origin"})
	require.ErrorIs(t, err, git.ErrUnstagedChanges)
	require.Equal(t, "local dirt", string(readBillyFile(t, localTree.Filesystem, "tracked.txt")))
	require.NoError(t, restoreManagedWorktree(local, localTree))
	err = pullManagedWorktree(context.Background(), local, localTree, &git.PullOptions{RemoteName: "origin"})
	require.Equal(t, git.NoErrAlreadyUpToDate, err)
	require.Equal(t, "new upstream content", string(readBillyFile(t, localTree.Filesystem, "tracked.txt")))
}

func TestPullWithUnmappedGitlinkHandlesUpstreamTransitions(t *testing.T) {
	for _, scenario := range []string{"removed", "mapping added"} {
		t.Run(scenario, func(t *testing.T) {
			remoteTree, local, localTree := newGitlinkClone(t)
			if scenario == "removed" {
				remote, err := git.PlainOpen(remoteTree.Filesystem.Root())
				require.NoError(t, err)
				idx, err := remote.Storer.Index()
				require.NoError(t, err)
				_, err = idx.Remove(testGitlinkPath)
				require.NoError(t, err)
				require.NoError(t, remote.Storer.SetIndex(idx))
			} else {
				writeBillyFile(t, remoteTree.Filesystem, ".gitmodules", []byte("[submodule \"transformer\"]\n\tpath = "+testGitlinkPath+"\n\turl = https://example.invalid/transformer.git\n"), 0o644)
				_, err := remoteTree.Add(".gitmodules")
				require.NoError(t, err)
			}
			_, err := remoteTree.Commit(scenario, &git.CommitOptions{Author: testSignature()})
			require.NoError(t, err)
			require.NoError(t, pullManagedWorktree(context.Background(), local, localTree, &git.PullOptions{RemoteName: "origin"}))
			require.NoError(t, restoreManagedWorktree(local, localTree))
			status, err := localTree.Status()
			require.NoError(t, err)
			require.True(t, status.IsClean(), status.String())
			idx, err := local.Storer.Index()
			require.NoError(t, err)
			_, err = idx.Entry(testGitlinkPath)
			if scenario == "removed" {
				require.ErrorIs(t, err, index.ErrEntryNotFound)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRestoreManagedWorktreePreservesInitializedSubmoduleFiles(t *testing.T) {
	repo, worktree, filesystem := newMemoryRepository(t)
	addTestGitlink(t, repo, worktree, true)
	module, err := worktree.Submodule("transformer")
	require.NoError(t, err)
	require.NoError(t, module.Init())
	child, err := module.Repository()
	require.NoError(t, err)
	childTree, err := child.Worktree()
	require.NoError(t, err)
	childCommit := commitTestFile(t, childTree, "tracked.txt", "submodule code")
	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	entry, err := idx.Entry(testGitlinkPath)
	require.NoError(t, err)
	entry.Hash = childCommit
	require.NoError(t, repo.Storer.SetIndex(idx))
	_, err = worktree.Commit("initialized submodule", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)
	writeBillyFile(t, childTree.Filesystem, "local.txt", []byte("local submodule data"), 0o644)
	require.NoError(t, restoreManagedWorktreeWithChmod(repo, worktree, func(string, os.FileMode) error { return nil }))
	require.Equal(t, "local submodule data", string(readBillyFile(t, filesystem, testGitlinkPath+"/local.txt")))
	require.Equal(t, "submodule code", string(readBillyFile(t, filesystem, testGitlinkPath+"/tracked.txt")))
}

func newGitlinkClone(t *testing.T) (*git.Worktree, *git.Repository, *git.Worktree) {
	t.Helper()
	remote, err := git.PlainInit(t.TempDir(), false)
	require.NoError(t, err)
	remoteTree, err := remote.Worktree()
	require.NoError(t, err)
	commitTestFile(t, remoteTree, "tracked.txt", "original")
	addTestGitlink(t, remote, remoteTree, false)
	local, err := git.PlainClone(filepath.Join(t.TempDir(), "clone"), false, &git.CloneOptions{URL: remoteTree.Filesystem.Root()})
	require.NoError(t, err)
	localTree, err := local.Worktree()
	require.NoError(t, err)
	require.NoError(t, restoreManagedWorktree(local, localTree))
	return remoteTree, local, localTree
}

func commitTestFile(t *testing.T, worktree *git.Worktree, name, content string) plumbing.Hash {
	t.Helper()
	writeBillyFile(t, worktree.Filesystem, name, []byte(content), 0o644)
	_, err := worktree.Add(name)
	require.NoError(t, err)
	commit, err := worktree.Commit("update "+name, &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)
	return commit
}

func TestRestoreManagedWorktreePreservesUninitializedSubmodules(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		name := "missing mapping"
		if mapped {
			name = "mapped"
		}
		t.Run(name, func(t *testing.T) {
			repo, worktree, filesystem := newMemoryRepository(t)
			writeBillyFile(t, filesystem, "tracked.txt", []byte("original\n"), 0o644)
			_, err := worktree.Add("tracked.txt")
			require.NoError(t, err)
			oldCommit, err := worktree.Commit("original", &git.CommitOptions{Author: testSignature()})
			require.NoError(t, err)
			commit := addTestGitlink(t, repo, worktree, mapped)
			writeBillyFile(t, filesystem, "tracked.txt", []byte("tampered\n"), 0o644)
			require.NoError(t, filesystem.MkdirAll("untracked", 0o755))
			writeBillyFile(t, filesystem, "untracked/junk.txt", []byte("junk"), 0o644)

			for attempt := 0; attempt < 2; attempt++ {
				err = restoreManagedWorktreeWithChmod(repo, worktree, func(string, os.FileMode) error { return nil })
				require.NoError(t, err)
				info, err := filesystem.Stat(testGitlinkPath)
				require.NoError(t, err)
				require.True(t, info.IsDir())
			}
			require.Equal(t, "original\n", string(readBillyFile(t, filesystem, "tracked.txt")))
			_, err = filesystem.Stat("untracked")
			require.True(t, os.IsNotExist(err))
			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, commit, head.Hash())
			_, err = repo.CommitObject(oldCommit)
			require.NoError(t, err)
			idx, err := repo.Storer.Index()
			require.NoError(t, err)
			entry, err := idx.Entry(testGitlinkPath)
			require.NoError(t, err)
			require.Equal(t, filemode.Submodule, entry.Mode)
			require.Equal(t, plumbing.NewHash("f04cc0a2a167c2552318ff73b9a3b4fbad60e9c7"), entry.Hash)
			if mapped {
				status, err := worktree.Status()
				require.NoError(t, err)
				require.True(t, status.IsClean(), status.String())
			} else {
				_, err := filesystem.Stat(".gitmodules")
				require.True(t, os.IsNotExist(err), "recovery must not fabricate repository content")
			}
		})
	}
}

func addTestGitlink(t *testing.T, repo *git.Repository, worktree *git.Worktree, mapped bool) plumbing.Hash {
	t.Helper()
	if mapped {
		writeBillyFile(t, worktree.Filesystem, ".gitmodules", []byte("[submodule \"transformer\"]\n\tpath = "+testGitlinkPath+"\n\turl = https://example.invalid/transformer.git\n"), 0o644)
		_, err := worktree.Add(".gitmodules")
		require.NoError(t, err)
	}
	idx, err := repo.Storer.Index()
	require.NoError(t, err)
	idx.Entries = append(idx.Entries, &index.Entry{
		Name: testGitlinkPath,
		Mode: filemode.Submodule,
		Hash: plumbing.NewHash("f04cc0a2a167c2552318ff73b9a3b4fbad60e9c7"),
	})
	require.NoError(t, repo.Storer.SetIndex(idx))
	commit, err := worktree.Commit("gitlink", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)
	require.NoError(t, worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: commit}))
	return commit
}
