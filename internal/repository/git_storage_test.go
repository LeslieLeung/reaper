package repository

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/stretchr/testify/require"
)

type failingPackFS struct {
	billy.Filesystem
	openErr  error
	closeErr error
}

func (fs *failingPackFS) Open(name string) (billy.File, error) {
	if fs.openErr != nil && strings.HasPrefix(filepath.Base(name), "tmp_pack_") {
		return nil, fs.openErr
	}
	return fs.Filesystem.Open(name)
}

func (fs *failingPackFS) TempFile(dir, prefix string) (billy.File, error) {
	f, err := fs.Filesystem.TempFile(dir, prefix)
	if err == nil && fs.closeErr != nil {
		return &failingClosePackFile{File: f, err: fs.closeErr}, nil
	}
	return f, err
}

type failingClosePackFile struct {
	billy.File
	err error
}

func (f *failingClosePackFile) Close() error {
	return errors.Join(f.File.Close(), f.err)
}

func TestPackWriterConstructorFailureClosesTemporaryFile(t *testing.T) {
	root := t.TempDir()
	store := newGitStorage(root)
	store.packFS.Filesystem = &failingPackFS{Filesystem: store.packFS.Filesystem, openErr: os.ErrPermission}
	require.NoError(t, store.Init())
	_, err := store.PackfileWriter()
	require.ErrorIs(t, err, os.ErrPermission)
	entries, err := os.ReadDir(filepath.Join(root, ".git", "objects", "pack"))
	require.NoError(t, err)
	require.Empty(t, entries, "failed constructors must close and remove their temporary pack")
}

type failedPackReader struct{}

func (failedPackReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestPackCleanupFailureSurvivesTransportEOF(t *testing.T) {
	store := newGitStorage(t.TempDir())
	store.packFS.Filesystem = &failingPackFS{Filesystem: store.packFS.Filesystem, closeErr: os.ErrPermission}
	require.NoError(t, store.Init())
	// A real pack header declaring one object, followed by a broken stream.
	reader := io.MultiReader(bytes.NewReader([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01")), failedPackReader{})
	err := packfile.WritePackfileToObjectStorage(store, reader)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	// go-git drops Close errors when the reader already failed. The operation
	// boundary must still report the cleanup failure and prevent another retry.
	err = store.operationError(err)
	require.ErrorIs(t, err, errPackCleanup)
	require.ErrorIs(t, err, os.ErrPermission)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
