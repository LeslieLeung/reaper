package repository

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

// Write Git objects directly: Git permits names that the host filesystem cannot
// create, as in block/buzz's agent-screenshots/klopez4212 branch.
func branchCommit(t *testing.T, repo *git.Repository, name, content string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	blob := repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	require.NoError(t, err)
	_, err = io.WriteString(w, content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	blobHash, err := repo.Storer.SetEncodedObject(blob)
	require.NoError(t, err)
	tree := &object.Tree{Entries: []object.TreeEntry{{Name: name, Mode: filemode.Regular, Hash: blobHash}}}
	encoded := repo.Storer.NewEncodedObject()
	require.NoError(t, tree.Encode(encoded))
	treeHash, err := repo.Storer.SetEncodedObject(encoded)
	require.NoError(t, err)
	sig := object.Signature{Name: "Test", Email: "test@example.com", When: time.Unix(1700000000, 0)}
	commit := &object.Commit{Author: sig, Committer: sig, Message: content, TreeHash: treeHash, ParentHashes: parents}
	encoded = repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(encoded))
	hash, err := repo.Storer.SetEncodedObject(encoded)
	require.NoError(t, err)
	return hash
}

func setBranch(t *testing.T, repo *git.Repository, name string, hash plumbing.Hash) {
	t.Helper()
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), hash)))
}

func TestUpdateArchivedBranchPreservesShallowHistory(t *testing.T) {
	repo, err := git.PlainInit(t.TempDir(), true)
	require.NoError(t, err)
	missingParent := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	boundary := branchCommit(t, repo, "file", "shallow boundary", missingParent)
	old := branchCommit(t, repo, "file", "local history", boundary)
	setBranch(t, repo, "screenshots", old)
	require.NoError(t, repo.Storer.SetShallow([]plumbing.Hash{boundary}))

	updated, err := updateArchivedBranch(repo, "screenshots", boundary)
	require.ErrorIs(t, err, git.ErrNonFastForwardUpdate)
	require.False(t, updated)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("screenshots"), true)
	require.NoError(t, err)
	require.Equal(t, old, ref.Hash())
}

func TestUpdateArchivedBranchRetriesWithExistingTrackingConfig(t *testing.T) {
	repo, err := git.PlainInit(t.TempDir(), true)
	require.NoError(t, err)
	hash := branchCommit(t, repo, "file", "content")
	require.NoError(t, repo.CreateBranch(&gitconfig.Branch{Name: "screenshots", Remote: "origin", Merge: plumbing.NewBranchReferenceName("screenshots")}))
	updated, err := updateArchivedBranch(repo, "screenshots", hash)
	require.NoError(t, err)
	require.True(t, updated)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("screenshots"), true)
	require.NoError(t, err)
	require.Equal(t, hash, ref.Hash())
}

func TestSyncArchivesBranchesWithoutCheckingOutUnrepresentableNames(t *testing.T) {
	// Sync's cache is rooted at the process cwd. Isolate it from other test
	// processes as well as the developer's working tree; do not run in parallel.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(cwd)) })
	for _, tt := range []struct {
		name            string
		allBranches     bool
		recoverCheckout bool
	}{{"all_branches", true, false}, {"default_only", false, false}, {"failed_checkout", true, true}} {
		t.Run(tt.name, func(t *testing.T) {
			allBranches := tt.allBranches
			remoteDir := t.TempDir()
			remote, err := git.PlainInit(remoteDir, true)
			require.NoError(t, err)
			mainHash := branchCommit(t, remote, "README.md", "original main")
			setBranch(t, remote, "main", mainHash)
			require.NoError(t, remote.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))))

			cwd, err := os.Getwd()
			require.NoError(t, err)
			name := filepath.Base(t.TempDir())
			cacheRoot := filepath.Join(cwd, ".gitrieve", "test.local", "branches", name)
			gitDir := filepath.Join(cacheRoot, "code")
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(cacheRoot)) })
			local, err := git.PlainClone(gitDir, false, &git.CloneOptions{URL: remoteDir})
			require.NoError(t, err)

			longName := strings.Repeat("screenshot.png100644 blob ", 16) + "last.png"
			badHash := branchCommit(t, remote, longName, "image bytes", mainHash)
			setBranch(t, remote, "screenshots", badHash)
			newMain := branchCommit(t, remote, "README.md", "updated main", mainHash)
			if tt.recoverCheckout {
				newMain = mainHash
				require.NoError(t, local.Fetch(&git.FetchOptions{}))
				w, err := local.Worktree()
				require.NoError(t, err)
				// Reproduce the cache left by the old implementation after its
				// checkout creates the ref but cannot write the oversized name.
				require.Error(t, w.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("screenshots"), Hash: badHash, Create: true, Force: true}))
			}
			setBranch(t, remote, "main", newMain)
			storeDir := t.TempDir()
			repo := typedef.Repository{URL: "test.local/branches/" + name, UseCache: true, AllBranches: allBranches}
			storages := []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: storeDir}}}
			require.NoError(t, Sync(context.Background(), repo, false, storages))
			content, err := os.ReadFile(filepath.Join(gitDir, "README.md"))
			require.NoError(t, err)
			if tt.recoverCheckout {
				require.Equal(t, "original main", string(content))
			} else {
				require.Equal(t, "updated main", string(content))
			}
			if allBranches {
				ref, err := local.Reference(plumbing.NewBranchReferenceName("screenshots"), true)
				require.NoError(t, err)
				require.Equal(t, badHash, ref.Hash())
			} else {
				_, err := local.Reference(plumbing.NewBranchReferenceName("screenshots"), true)
				require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
			}

			// Restore the produced archive and verify the otherwise uncheckable
			// filename and its bytes survive in the archived Git object database.
			archivePath := filepath.Join(storeDir, "test.local", "branches", name, name+".tar.gz")
			f, err := os.Open(archivePath)
			require.NoError(t, err)
			defer f.Close()
			gz, err := gzip.NewReader(f)
			require.NoError(t, err)
			defer gz.Close()
			restoredDir := t.TempDir()
			tr := tar.NewReader(gz)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				require.True(t, filepath.IsLocal(h.Name))
				dest := filepath.Join(restoredDir, h.Name)
				if h.Typeflag == tar.TypeDir {
					require.NoError(t, os.MkdirAll(dest, 0755))
					continue
				}
				require.Equal(t, byte(tar.TypeReg), h.Typeflag)
				require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0755))
				data, err := io.ReadAll(tr)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(dest, data, 0644))
			}
			restored, err := git.PlainOpen(filepath.Join(restoredDir, name))
			require.NoError(t, err)
			commit, err := restored.CommitObject(badHash)
			require.NoError(t, err)
			file, err := commit.File(longName)
			require.NoError(t, err)
			contents, err := file.Contents()
			require.NoError(t, err)
			require.Equal(t, "image bytes", contents)

			if allBranches {
				// A subsequent sync fast-forwards the branch without checkout.
				next := branchCommit(t, remote, longName, "new image", badHash)
				setBranch(t, remote, "screenshots", next)
				require.NoError(t, Sync(context.Background(), repo, false, storages))
				ref, err := local.Reference(plumbing.NewBranchReferenceName("screenshots"), true)
				require.NoError(t, err)
				require.Equal(t, next, ref.Hash())

				// Upstream rewrites and deletes must not discard local history.
				setBranch(t, remote, "screenshots", branchCommit(t, remote, "replacement", "rewritten"))
				require.NoError(t, Sync(context.Background(), repo, false, storages))
				ref, err = local.Reference(plumbing.NewBranchReferenceName("screenshots"), true)
				require.NoError(t, err)
				require.Equal(t, next, ref.Hash())
				require.NoError(t, remote.Storer.RemoveReference(plumbing.NewBranchReferenceName("screenshots")))
				require.NoError(t, Sync(context.Background(), repo, false, storages))
				ref, err = local.Reference(plumbing.NewBranchReferenceName("screenshots"), true)
				require.NoError(t, err)
				require.Equal(t, next, ref.Hash())
			}
		})
	}
}
