package repository

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/lock"
	"github.com/wnarutou/gitrieve/internal/scm"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

func captureSyncLogs(t *testing.T) *recSink {
	t.Helper()
	sink := &recSink{}
	ui.SetSink(sink)
	unbind := ui.Bind("exec-sync", "test-repo")
	t.Cleanup(func() {
		unbind()
		ui.SetSink(nil)
	})
	return sink
}

func TestSyncReusesCachedRepository(t *testing.T) {
	for _, component := range []string{"code", "wiki"} {
		t.Run(component, func(t *testing.T) {
			// Keep both the cache and remote local so this exercises real git
			// operations without relying on GitHub or modifying a user's cache.
			root := t.TempDir()
			previousDir, err := os.Getwd()
			require.NoError(t, err)
			require.NoError(t, os.Chdir(root))
			t.Cleanup(func() { require.NoError(t, os.Chdir(previousDir)) })

			remoteDir := filepath.Join(root, "remote")
			remote, err := git.PlainInit(remoteDir, false)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(remoteDir, "Home.md"), []byte("wiki contents\n"), 0600))
			worktree, err := remote.Worktree()
			require.NoError(t, err)
			_, err = worktree.Add("Home.md")
			require.NoError(t, err)
			commit, err := worktree.Commit("initial contents", &git.CommitOptions{
				Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
			})
			require.NoError(t, err)

			gitDir := filepath.Join(root, ".gitrieve", "github.com", "test", "repo", component)
			cached, err := git.PlainClone(gitDir, false, &git.CloneOptions{URL: remoteDir})
			require.NoError(t, err)
			repo := typedef.Repository{URL: "github.com/test/repo", UseCache: true, AllBranches: true}
			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, Sync(context.Background(), repo, component == "wiki", nil))
				head, err := cached.Head()
				require.NoError(t, err)
				require.Equal(t, commit, head.Hash())
				contents, err := os.ReadFile(filepath.Join(gitDir, "Home.md"))
				require.NoError(t, err)
				require.Equal(t, "wiki contents\n", string(contents))
			}

			// An unavailable remote must not destroy previously cached history.
			require.NoError(t, cached.DeleteRemote("origin"))
			_, err = cached.CreateRemote(&gitconfig.RemoteConfig{
				Name: "origin", URLs: []string{filepath.Join(root, "missing-remote")},
			})
			require.NoError(t, err)
			require.Error(t, Sync(context.Background(), repo, component == "wiki", nil))
			preserved, err := git.PlainOpen(gitDir)
			require.NoError(t, err)
			_, err = preserved.CommitObject(commit)
			require.NoError(t, err, "cached history must survive a failed sync")
		})
	}
}

func TestSyncCancelledContextFailsPromptlyAndCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the sync starts

	repo := typedef.Repository{Name: "test-repo", URL: "github.com/test/repo", UseCache: true}

	// A cancelled context must fail the sync at the first network operation
	// instead of hanging forever.
	err := Sync(ctx, repo, false, nil)
	assert.Error(t, err, "a cancelled context must fail the sync")

	// The partial clone directory must be removed so the next sync retries a
	// clean clone instead of trying to open a broken .git.
	cwd, _ := os.Getwd()
	gitDir := path.Join(cwd, ".gitrieve", "github.com", "test", "repo", "code")
	_, statErr := os.Stat(path.Join(gitDir, ".git"))
	assert.True(t, os.IsNotExist(statErr), "partial clone .git must be cleaned up after a cancelled clone")

	// Clean up the .gitrieve cache dir created by the test.
	os.RemoveAll(path.Join(cwd, ".gitrieve"))
}

func TestSyncBlocksWhileCodeLockHeld(t *testing.T) {
	sink := captureSyncLogs(t)
	repo := typedef.Repository{Name: "test-repo", URL: "github.com/test/repo", UseCache: true}
	r, err := scm.NewRepository(repo.URL)
	require.NoError(t, err)
	release, err := lock.Acquire(context.Background(), r, "code")
	require.NoError(t, err)
	defer release()
	t.Cleanup(func() { os.RemoveAll(".gitrieve") })

	// If Sync respects the lock it blocks here and hits the ctx timeout instead
	// of reaching the network (github.com/test/repo does not exist).
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = Sync(ctx, repo, false, nil)
	require.Equal(t, context.DeadlineExceeded, err, "Sync must block on the held code lock")
	require.Contains(t, sink.snapshot(), "Sync phase: waiting for code lock")
	for _, message := range sink.snapshot() {
		require.NotContains(t, message, "code lock acquired", "the acquired message must only follow successful acquisition")
	}
}

func TestSyncLogsLockDeadlineAndClonePhase(t *testing.T) {
	sink := captureSyncLogs(t)
	t.Cleanup(func() { os.RemoveAll(".gitrieve") })

	// Loopback port 443 has no test server, so clone fails locally without
	// depending on DNS or an external Git host.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	expectedDeadline, ok := ctx.Deadline()
	require.True(t, ok)
	err := Sync(ctx, typedef.Repository{
		Name:     "test-repo",
		URL:      "127.0.0.1/test/repo",
		UseCache: true,
	}, false, nil)
	require.Error(t, err)

	messages := sink.snapshot()
	require.Contains(t, messages, "Sync phase: waiting for code lock")
	require.Contains(t, messages, "Sync phase: code lock acquired")
	require.Contains(t, messages, "Sync phase: Git clone started")
	deadlineLog, found := findLogWithPrefix(messages, "Sync deadline: ")
	require.True(t, found, "the effective deadline must be visible in logs")
	loggedDeadline, err := time.Parse(time.RFC3339, strings.TrimPrefix(deadlineLog, "Sync deadline: "))
	require.NoError(t, err, "the logged deadline must be RFC3339")
	require.Equal(t, expectedDeadline.UTC().Truncate(time.Second), loggedDeadline,
		"the log must show the effective caller deadline when it is shorter than 30 minutes")
}

func TestSyncStageLogsDoNotExposeInlineCredentials(t *testing.T) {
	sink := captureSyncLogs(t)
	t.Cleanup(func() { os.RemoveAll(".gitrieve") })

	const secret = "sentinel-inline-token"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = Sync(ctx, typedef.Repository{
		Name:     "test-repo",
		URL:      "user:" + secret + "@127.0.0.1/test/repo",
		UseCache: true,
	}, false, nil)

	stageLogs := 0
	for _, message := range sink.snapshot() {
		if strings.HasPrefix(message, "Sync phase: ") || strings.HasPrefix(message, "Sync deadline: ") {
			stageLogs++
			require.NotContains(t, message, secret, "diagnostic stage logs must not persist inline credentials")
		}
	}
	require.NotZero(t, stageLogs, "the credential check must exercise diagnostic stage logging")
}

func findLogWithPrefix(messages []string, prefix string) (string, bool) {
	for _, message := range messages {
		if strings.HasPrefix(message, prefix) {
			return message, true
		}
	}
	return "", false
}

func TestSyncBlocksWhileWikiLockHeld(t *testing.T) {
	repo := typedef.Repository{Name: "test-repo", URL: "github.com/test/repo", UseCache: true}
	r, err := scm.NewRepository(repo.URL)
	require.NoError(t, err)
	release, err := lock.Acquire(context.Background(), r, "wiki")
	require.NoError(t, err)
	defer release()
	t.Cleanup(func() { os.RemoveAll(".gitrieve") })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = Sync(ctx, repo, true, nil)
	require.Equal(t, context.DeadlineExceeded, err, "wiki Sync must block on the held wiki lock")
}

func TestWikiCloneRepositoryNotFoundIsReportedByWikiLayer(t *testing.T) {
	err := fmt.Errorf("%w: Repository not found", transport.ErrAuthenticationRequired)

	require.False(t, shouldLogCloneError(true, err))
	require.True(t, shouldLogCloneError(false, err))
	require.True(t, shouldLogCloneError(true, context.DeadlineExceeded))
}
