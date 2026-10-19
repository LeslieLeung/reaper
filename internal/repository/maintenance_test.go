package repository

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/maintenance"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMaintenanceInterruptsGitFetchAndPreservesHistory(t *testing.T) {
	oldDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(oldDir)) })
	gitDir := filepath.Join(".gitrieve", "github.com", "test", "repo", "code")
	local, err := git.PlainInit(gitDir, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "history.txt"), []byte("irreplaceable history"), 0600))
	w, err := local.Worktree()
	require.NoError(t, err)
	_, err = w.Add("history.txt")
	require.NoError(t, err)
	hash, err := w.Commit("original history", &git.CommitOptions{Author: &object.Signature{Name: "archive", Email: "archive@example.test", When: time.Now()}})
	require.NoError(t, err)
	_, err = local.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/test/repo"}})
	require.NoError(t, err)
	out := t.TempDir()
	archive := filepath.Join(out, "github.com/test/repo/repo.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(archive), 0700))
	require.NoError(t, os.WriteFile(archive, []byte("previous archive"), 0600))
	started := make(chan struct{})
	original := client.Protocols["https"]
	client.InstallProtocol("https", githttp.NewClient(&http.Client{Transport: maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}))
	t.Cleanup(func() { client.InstallProtocol("https", original) })
	signal := maintenance.NewSignal()
	ctx, cancel := context.WithTimeout(maintenance.WithSignal(context.Background(), signal), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Sync(ctx, typedef.Repository{URL: "github.com/test/repo", UseCache: true}, false, []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fetch never started")
	}
	signal.Request()
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("maintenance failed to interrupt fetch")
	}
	require.ErrorIs(t, err, maintenance.ErrRequested)
	require.NoError(t, ctx.Err())
	commit, err := local.CommitObject(hash)
	require.NoError(t, err)
	require.Equal(t, "original history", commit.Message)
	head, err := local.Head()
	require.NoError(t, err)
	require.Equal(t, hash, head.Hash())
	data, err := os.ReadFile(archive)
	require.NoError(t, err)
	require.Equal(t, "previous archive", string(data))
}

func TestMaintenanceGitPreRequestedDoesNotStartNetwork(t *testing.T) {
	signal := maintenance.NewSignal()
	signal.Request()
	ctx := maintenance.WithSignal(context.Background(), signal)
	err := Sync(ctx, typedef.Repository{URL: "invalid URL"}, false, nil)
	require.ErrorIs(t, err, maintenance.ErrRequested, fmt.Sprint(err))
}

type maintenanceLogSink struct{ signal *maintenance.Signal }

func (s maintenanceLogSink) Log(_, _, _, message string) error {
	if strings.HasPrefix(message, "File ") && strings.HasSuffix(message, " stored") {
		s.signal.Request()
	}
	return nil
}

type stopAfterGitPullSink struct{ signal *maintenance.Signal }

func (s stopAfterGitPullSink) Log(_, _, _, message string) error {
	if strings.Contains(message, "has successed pull") {
		s.signal.Request()
	}
	return nil
}

func TestMaintenanceAfterGitPullPublishesOnNextUnchangedSync(t *testing.T) {
	oldDir, err := os.Getwd()
	require.NoError(t, err)
	workspace := t.TempDir()
	require.NoError(t, os.Chdir(workspace))
	t.Cleanup(func() { require.NoError(t, os.Chdir(oldDir)) })
	upstreamDir := filepath.Join(workspace, "upstream")
	upstream, err := git.PlainInit(upstreamDir, false)
	require.NoError(t, err)
	commit := func(text string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(upstreamDir, "code.txt"), []byte(text), 0600))
		w, err := upstream.Worktree()
		require.NoError(t, err)
		_, err = w.Add("code.txt")
		require.NoError(t, err)
		_, err = w.Commit(text, &git.CommitOptions{Author: &object.Signature{Name: "archive", Email: "archive@example.test", When: time.Now()}})
		require.NoError(t, err)
	}
	commit("original")
	_, err = git.PlainClone(filepath.Join(workspace, ".gitrieve/github.com/test/repo/code"), false, &git.CloneOptions{URL: upstreamDir})
	require.NoError(t, err)
	commit("updated before maintenance")
	signal := maintenance.NewSignal()
	ui.SetSink(stopAfterGitPullSink{signal: signal})
	t.Cleanup(func() { ui.SetSink(nil) })
	unbind := ui.Bind("after-pull-test", "repo")
	defer unbind()
	out := t.TempDir()
	archivePath := filepath.Join(out, "github.com/test/repo/repo.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(archivePath), 0700))
	require.NoError(t, os.WriteFile(archivePath, []byte("previous archive"), 0600))
	repo := typedef.Repository{URL: "github.com/test/repo", UseCache: true, AllBranches: true}
	storages := []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}}
	err = Sync(maintenance.WithSignal(context.Background(), signal), repo, false, storages)
	require.ErrorIs(t, err, maintenance.ErrRequested)
	previous, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	require.Equal(t, "previous archive", string(previous), "soft stop before publication must preserve prior archive")
	ui.SetSink(nil)
	require.NoError(t, Sync(context.Background(), repo, false, storages))
	f, err := os.Open(archivePath)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err, "unchanged retry must publish cached changes")
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Name == "repo/code.txt" {
			data, err := io.ReadAll(tr)
			require.NoError(t, err)
			require.Equal(t, "updated before maintenance", string(data))
			found = true
		}
	}
	require.True(t, found, "retry archive must contain cached worktree")
}

func TestMaintenanceDuringGitPublicationFinishesAllStorages(t *testing.T) {
	oldDir, err := os.Getwd()
	require.NoError(t, err)
	workspace := t.TempDir()
	require.NoError(t, os.Chdir(workspace))
	t.Cleanup(func() { require.NoError(t, os.Chdir(oldDir)) })
	upstreamDir := filepath.Join(workspace, "upstream")
	upstream, err := git.PlainInit(upstreamDir, false)
	require.NoError(t, err)
	commit := func(text string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(upstreamDir, "code.txt"), []byte(text), 0600))
		w, err := upstream.Worktree()
		require.NoError(t, err)
		_, err = w.Add("code.txt")
		require.NoError(t, err)
		_, err = w.Commit(text, &git.CommitOptions{Author: &object.Signature{Name: "archive", Email: "archive@example.test", When: time.Now()}})
		require.NoError(t, err)
	}
	commit("original")
	_, err = git.PlainClone(filepath.Join(workspace, ".gitrieve/github.com/test/repo/code"), false, &git.CloneOptions{URL: upstreamDir})
	require.NoError(t, err)
	commit("updated")
	signal := maintenance.NewSignal()
	ui.SetSink(maintenanceLogSink{signal: signal})
	t.Cleanup(func() { ui.SetSink(nil) })
	unbind := ui.Bind("publication-test", "repo")
	defer unbind()
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)
	out, out2 := t.TempDir(), t.TempDir()
	ctx := maintenance.WithSignal(context.Background(), signal)
	err = Sync(ctx, typedef.Repository{URL: "github.com/test/repo", UseCache: true, AllBranches: true}, false, []typedef.MultiStorage{{Storage: typedef.Storage{Type: "file", Path: out}}, {Storage: typedef.Storage{Type: "file", Path: out2}}})
	require.ErrorIs(t, err, maintenance.ErrRequested)
	first, err := os.ReadFile(filepath.Join(out, "github.com/test/repo/repo.tar.gz"))
	require.NoError(t, err)
	second, err := os.ReadFile(filepath.Join(out2, "github.com/test/repo/repo.tar.gz"))
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.NotEmpty(t, first)
	require.NoError(t, ctx.Err())
}
