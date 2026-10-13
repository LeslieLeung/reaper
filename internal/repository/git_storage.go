package repository

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

var errPackCleanup = errors.New("failed packfile cleanup")

// go-git v5.16.0's PackWriter.Close joins its parser but returns before closing
// its files when parsing fails. Keep the efficient pack storage and explicitly
// close those handles so EOF retries cannot leak files or block Windows cleanup.
type gitStorage struct {
	*filesystem.Storage
	packFS     *packTrackingFS
	cleanupErr error
}

// go-git can discard a writer's Close error when reading the transport already
// failed. Preserve cleanup failures separately so they still stop EOF retries.
func (s *gitStorage) operationError(err error) error {
	if s.cleanupErr == nil {
		return err
	}
	return errors.Join(err, s.cleanupErr)
}

func newGitStorage(gitDir string) *gitStorage {
	fs := &packTrackingFS{Filesystem: osfs.New(filepath.Join(gitDir, ".git"))}
	return &gitStorage{Storage: filesystem.NewStorage(fs, cache.NewObjectLRUDefault()), packFS: fs}
}

func (s *gitStorage) PackfileWriter() (io.WriteCloser, error) {
	// Sync's repository lock serializes writers. Capture only the handles
	// opened by this constructor; cached readers and other writers are untouched.
	s.packFS.opened = nil
	writer, err := s.Storage.PackfileWriter()
	files := s.packFS.opened
	s.packFS.opened = nil
	finish := func(original error) error {
		var cleanupErr error
		for _, file := range files {
			cleanupErr = errors.Join(cleanupErr, file.Close())
		}
		if original != nil && cleanupErr == nil {
			for _, file := range files {
				if err := s.packFS.Remove(file.Name()); err != nil && !os.IsNotExist(err) {
					cleanupErr = errors.Join(cleanupErr, err)
				}
			}
		}
		if cleanupErr != nil {
			s.cleanupErr = errors.Join(s.cleanupErr, fmt.Errorf("%w: %w", errPackCleanup, cleanupErr))
			return s.operationError(original)
		}
		return original
	}
	if err != nil {
		return nil, finish(err)
	}
	return &closingPackWriter{WriteCloser: writer, finish: finish}, nil
}

type closingPackWriter struct {
	io.WriteCloser
	finish func(error) error
}

func (w *closingPackWriter) Close() error {
	return w.finish(w.WriteCloser.Close())
}

type packTrackingFS struct {
	billy.Filesystem
	opened []*packFile
}

func (fs *packTrackingFS) track(file billy.File, err error) (billy.File, error) {
	if err != nil || !strings.HasPrefix(filepath.Base(file.Name()), "tmp_pack_") {
		return file, err
	}
	tracked := &packFile{File: file}
	fs.opened = append(fs.opened, tracked)
	return tracked, nil
}

func (fs *packTrackingFS) TempFile(dir, prefix string) (billy.File, error) {
	return fs.track(fs.Filesystem.TempFile(dir, prefix))
}

func (fs *packTrackingFS) Open(name string) (billy.File, error) {
	return fs.track(fs.Filesystem.Open(name))
}

type packFile struct {
	billy.File
	once     sync.Once
	closeErr error
}

func (f *packFile) Close() error {
	f.once.Do(func() { f.closeErr = f.File.Close() })
	return f.closeErr
}
