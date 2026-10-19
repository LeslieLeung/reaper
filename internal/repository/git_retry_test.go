package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

// Keep real clone/fetch/list/pull and on-disk cache behavior, replacing only
// the remote transport with an embedded Git server and injected failures.
type eofTestTransport struct {
	transport.Transport
	calls             int
	fail              func(int) error
	truncateFirstPack bool
}

func (c *eofTestTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	c.calls++
	if err := c.fail(c.calls); err != nil {
		return nil, err
	}
	session, err := c.Transport.NewUploadPackSession(ep, auth)
	if err == nil && c.truncateFirstPack && c.calls == 1 {
		return &truncatedPackSession{session}, nil
	}
	return session, err
}

type truncatedPackSession struct{ transport.UploadPackSession }

func (s *truncatedPackSession) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	response, err := s.UploadPackSession.UploadPack(ctx, req)
	if err != nil {
		return nil, err
	}
	// Deliver the pack header and part of an object, then truncate the stream.
	partial := struct {
		io.Reader
		io.Closer
	}{io.LimitReader(response, 20), response}
	return packp.NewUploadPackResponseWithPackfile(req, partial), nil
}

func setupEOFSync(t *testing.T, wiki bool, fail func(int) error) (typedef.Repository, string, *eofTestTransport) {
	t.Helper()
	root := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	t.Cleanup(func() { require.NoError(t, os.Chdir(previousDir)) })

	fs := memfs.New()
	remote, err := git.Init(memory.NewStorage(), fs)
	require.NoError(t, err)
	f, err := fs.Create("Home.md")
	require.NoError(t, err)
	_, err = f.Write([]byte("archived contents\n"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	w, err := remote.Worktree()
	require.NoError(t, err)
	_, err = w.Add("Home.md")
	require.NoError(t, err)
	_, err = w.Commit("initial", &git.CommitOptions{Author: &object.Signature{
		Name: "Test", Email: "test@example.com", When: time.Now(),
	}})
	require.NoError(t, err)

	repo := typedef.Repository{URL: "github.com/test/eof-repo", UseCache: true, AllBranches: true}
	remoteURL, component := "https://"+repo.URL, "code"
	if wiki {
		remoteURL += ".wiki"
		component = "wiki"
	}
	c := &eofTestTransport{
		Transport: server.NewClient(server.MapLoader{remoteURL: remote.Storer}),
		fail:      fail,
	}
	previousTransport := client.Protocols["https"]
	client.InstallProtocol("https", c)
	t.Cleanup(func() { client.InstallProtocol("https", previousTransport) })
	return repo, filepath.Join(root, ".gitrieve", "github.com", "test", "eof-repo", component), c
}

func TestSyncRecoversFromEOFInEachGitPhase(t *testing.T) {
	for _, wiki := range []bool{false, true} {
		for index, phase := range []string{"clone", "fetch", "references", "pull"} {
			t.Run(fmt.Sprintf("wiki=%t/%s", wiki, phase), func(t *testing.T) {
				repo, cacheDir, remote := setupEOFSync(t, wiki, func(call int) error {
					if call == index+1 {
						return &url.Error{Op: "Get", URL: "https://github.com/test/info/refs", Err: io.ErrUnexpectedEOF}
					}
					return nil
				})
				require.NoError(t, Sync(context.Background(), repo, wiki, nil))
				require.Equal(t, 5, remote.calls, "four Git phases plus one failed attempt")
				contents, err := os.ReadFile(filepath.Join(cacheDir, "Home.md"))
				require.NoError(t, err)
				require.Equal(t, "archived contents\n", string(contents))
				cached, err := git.PlainOpen(cacheDir)
				require.NoError(t, err)
				head, err := cached.Head()
				require.NoError(t, err)
				_, err = cached.CommitObject(head.Hash())
				require.NoError(t, err)
			})
		}
	}
}

func TestSyncEOFExhaustionFailsAndPreservesCompletedClone(t *testing.T) {
	for _, wiki := range []bool{false, true} {
		for index, phase := range []string{"clone", "fetch", "references", "pull"} {
			t.Run(fmt.Sprintf("wiki=%t/%s", wiki, phase), func(t *testing.T) {
				repo, cacheDir, remote := setupEOFSync(t, wiki, func(call int) error {
					if call >= index+1 {
						return io.EOF
					}
					return nil
				})
				err := Sync(context.Background(), repo, wiki, nil)
				require.ErrorIs(t, err, io.EOF, "exhausted pull retries must not report success")
				require.Equal(t, index+4, remote.calls, "only three retries after the failed initial attempt")
				if phase == "clone" {
					_, err := os.Stat(cacheDir)
					require.True(t, os.IsNotExist(err), "failed initial clones must be cleaned up")
				} else {
					cached, err := git.PlainOpen(cacheDir)
					require.NoError(t, err, "a failed update must preserve existing history")
					head, err := cached.Head()
					require.NoError(t, err)
					_, err = cached.CommitObject(head.Hash())
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestSyncDoesNotRetryAuthenticationErrors(t *testing.T) {
	for _, wiki := range []bool{false, true} {
		t.Run(fmt.Sprintf("wiki=%t", wiki), func(t *testing.T) {
			repo, _, remote := setupEOFSync(t, wiki, func(int) error { return transport.ErrAuthenticationRequired })
			require.ErrorIs(t, Sync(context.Background(), repo, wiki, nil), transport.ErrAuthenticationRequired)
			require.Equal(t, 1, remote.calls)
		})
	}
}

func TestSyncEOFBackoffHonorsDeadline(t *testing.T) {
	repo, _, remote := setupEOFSync(t, false, func(int) error { return io.ErrUnexpectedEOF })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, Sync(ctx, repo, false, nil), context.DeadlineExceeded)
	require.Equal(t, 1, remote.calls, "deadline during backoff must prevent the next attempt")
}

func TestSyncRetriesTruncatedClonePack(t *testing.T) {
	for _, wiki := range []bool{false, true} {
		t.Run(fmt.Sprintf("wiki=%t", wiki), func(t *testing.T) {
			repo, cacheDir, remote := setupEOFSync(t, wiki, func(int) error { return nil })
			remote.truncateFirstPack = true
			require.NoError(t, Sync(context.Background(), repo, wiki, nil))
			require.Equal(t, 5, remote.calls)
			contents, err := os.ReadFile(filepath.Join(cacheDir, "Home.md"))
			require.NoError(t, err)
			require.Equal(t, "archived contents\n", string(contents))
		})
	}
}

func TestRetryGitEOFCanSucceedOnLastRetry(t *testing.T) {
	calls := 0
	err := retryGitEOF(context.Background(), "clone", func() error {
		calls++
		if calls < 4 {
			return fmt.Errorf("read pack: %w", io.ErrUnexpectedEOF)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 4, calls)
}

func TestRetryGitEOFDoesNotRetryTerminalErrors(t *testing.T) {
	for _, terminal := range []error{
		context.Canceled, context.DeadlineExceeded, transport.ErrAuthenticationRequired,
		transport.ErrAuthorizationFailed, errManagedIndexRecovery, errCloneCleanup, errManagedWorktreeRecovery, errPackCleanup,
	} {
		t.Run(terminal.Error(), func(t *testing.T) {
			calls := 0
			failure := errors.Join(io.ErrUnexpectedEOF, terminal)
			err := retryGitEOF(context.Background(), "pull", func() error {
				calls++
				return failure
			})
			require.ErrorIs(t, err, terminal)
			require.Equal(t, 1, calls)
		})
	}
}

func TestRetryGitEOFCancellationStopsRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := retryGitEOF(ctx, "fetch", func() error {
		calls++
		cancel()
		return io.EOF
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
}

func TestRetryGitEOFDoesNotRetryLocalWorktreeRecoveryEOF(t *testing.T) {
	pulls, recoveries := 0, 0
	err := retryGitEOF(context.Background(), "pull", func() error {
		return pullWithManagedWorktreeRecovery(func() error {
			pulls++
			return git.ErrUnstagedChanges
		}, func() error {
			recoveries++
			return io.ErrUnexpectedEOF
		})
	})
	require.ErrorIs(t, err, errManagedWorktreeRecovery)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 1, pulls)
	require.Equal(t, 1, recoveries)
}
