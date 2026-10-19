package repository

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

func TestRepairWorktreeModesRestoresExecutableBitWithoutMovingHistory(t *testing.T) {
	repo, worktree, filesystem := newMemoryRepository(t)
	writeBillyFile(t, filesystem, "script.sh", []byte("#!/bin/sh\necho safe\n"), 0o755)
	_, err := worktree.Add("script.sh")
	require.NoError(t, err)
	commit, err := worktree.Commit("executable script", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)

	// Model a migration that preserved bytes but dropped the executable bit.
	rewriteBillyMode(t, filesystem, "script.sh", 0o644)
	status, err := worktree.Status()
	require.NoError(t, err)
	require.Equal(t, git.Modified, status.File("script.sh").Worktree)

	err = repairWorktreeModes(repo, worktree, func(name string, mode os.FileMode) error {
		rewriteBillyMode(t, filesystem, name, mode)
		return nil
	})
	require.NoError(t, err)

	status, err = worktree.Status()
	require.NoError(t, err)
	require.True(t, status.IsClean())
	info, err := filesystem.Stat("script.sh")
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o111)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, commit, head.Hash())
	_, err = repo.CommitObject(commit)
	require.NoError(t, err)
}

func TestRepairWorktreeModesRejectsFilesystemThatCannotPersistExecutableBit(t *testing.T) {
	repo, worktree, filesystem := newMemoryRepository(t)
	writeBillyFile(t, filesystem, "script.sh", []byte("#!/bin/sh\n"), 0o755)
	_, err := worktree.Add("script.sh")
	require.NoError(t, err)
	_, err = worktree.Commit("executable script", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)
	rewriteBillyMode(t, filesystem, "script.sh", 0o644)

	err = repairWorktreeModes(repo, worktree, func(string, os.FileMode) error {
		return nil // Simulate a mount that accepts chmod but keeps reporting 0644.
	})
	require.ErrorContains(t, err, "script.sh")
	require.ErrorContains(t, err, "expected 100755")
	require.ErrorContains(t, err, "actual 100644")
}

func TestRestoreManagedWorktreeDiscardsOnlyDerivedStateAndPreservesHistory(t *testing.T) {
	repo, worktree, filesystem := newMemoryRepository(t)
	writeBillyFile(t, filesystem, "tracked.txt", []byte("old\n"), 0o644)
	_, err := worktree.Add("tracked.txt")
	require.NoError(t, err)
	oldCommit, err := worktree.Commit("old history", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)

	writeBillyFile(t, filesystem, "tracked.txt", []byte("current\n"), 0o644)
	writeBillyFile(t, filesystem, "restore-me.txt", []byte("restored\n"), 0o644)
	_, err = worktree.Add("tracked.txt")
	require.NoError(t, err)
	_, err = worktree.Add("restore-me.txt")
	require.NoError(t, err)
	currentCommit, err := worktree.Commit("current history", &git.CommitOptions{Author: testSignature()})
	require.NoError(t, err)

	writeBillyFile(t, filesystem, "tracked.txt", []byte("tampered\n"), 0o644)
	require.NoError(t, filesystem.Remove("restore-me.txt"))
	require.NoError(t, filesystem.MkdirAll("untracked", 0o755))
	writeBillyFile(t, filesystem, filepath.Join("untracked", "leftover.txt"), []byte("discard me\n"), 0o644)

	err = restoreManagedWorktreeWithChmod(repo, worktree, func(string, os.FileMode) error {
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "current\n", string(readBillyFile(t, filesystem, "tracked.txt")))
	require.Equal(t, "restored\n", string(readBillyFile(t, filesystem, "restore-me.txt")))
	_, err = filesystem.Stat("untracked")
	require.True(t, os.IsNotExist(err))

	status, err := worktree.Status()
	require.NoError(t, err)
	require.True(t, status.IsClean())
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, currentCommit, head.Hash())
	_, err = repo.CommitObject(oldCommit)
	require.NoError(t, err, "previously pulled history must remain recoverable")
}

func TestPullWithManagedWorktreeRecoveryRetriesUnstagedChangesOnce(t *testing.T) {
	pulls := 0
	recoveries := 0
	err := pullWithManagedWorktreeRecovery(func() error {
		pulls++
		if pulls == 1 {
			return git.ErrUnstagedChanges
		}
		return git.NoErrAlreadyUpToDate
	}, func() error {
		recoveries++
		return nil
	})

	require.ErrorIs(t, err, git.NoErrAlreadyUpToDate)
	require.Equal(t, 2, pulls)
	require.Equal(t, 1, recoveries)
}

func TestPullWithManagedWorktreeRecoveryDoesNotRetryOtherErrors(t *testing.T) {
	want := errors.New("network failed")
	pulls := 0
	recoveries := 0
	err := pullWithManagedWorktreeRecovery(func() error {
		pulls++
		return want
	}, func() error {
		recoveries++
		return nil
	})

	require.ErrorIs(t, err, want)
	require.Equal(t, 1, pulls)
	require.Zero(t, recoveries)
}

func TestPullWithManagedWorktreeRecoveryStopsAfterOneRetry(t *testing.T) {
	pulls := 0
	recoveries := 0
	err := pullWithManagedWorktreeRecovery(func() error {
		pulls++
		return git.ErrUnstagedChanges
	}, func() error {
		recoveries++
		return nil
	})

	require.ErrorIs(t, err, git.ErrUnstagedChanges)
	require.Equal(t, 2, pulls)
	require.Equal(t, 1, recoveries)
}

func newMemoryRepository(t *testing.T) (*git.Repository, *git.Worktree, billy.Filesystem) {
	t.Helper()
	filesystem := memfs.New()
	repo, err := git.Init(memory.NewStorage(), filesystem)
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)
	return repo, worktree, filesystem
}

func writeBillyFile(t *testing.T, filesystem billy.Filesystem, name string, content []byte, mode os.FileMode) {
	t.Helper()
	file, err := filesystem.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	require.NoError(t, err)
	_, err = file.Write(content)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

func rewriteBillyMode(t *testing.T, filesystem billy.Filesystem, name string, mode os.FileMode) {
	t.Helper()
	file, err := filesystem.Open(name)
	require.NoError(t, err)
	content, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, filesystem.Remove(name))
	writeBillyFile(t, filesystem, name, content, mode)
}

func readBillyFile(t *testing.T, filesystem billy.Filesystem, name string) []byte {
	t.Helper()
	file, err := filesystem.Open(name)
	require.NoError(t, err)
	content, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	return content
}

func testSignature() *object.Signature {
	return &object.Signature{
		Name:  "Gitrieve Test",
		Email: "test@example.com",
		When:  time.Unix(1, 0),
	}
}
