package storage

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/wnarutou/gitrieve/internal/typedef"
)

func TestGetStorageRejectsUnsupportedTypes(t *testing.T) {
	for _, kind := range []string{"s3", "ftp", ""} {
		t.Run(kind, func(t *testing.T) {
			backend, err := GetStorage(typedef.MultiStorage{Storage: typedef.Storage{Name: "archive", Type: kind}})
			require.ErrorContains(t, err, "only 'file' storage is supported")
			require.Nil(t, backend)
		})
	}
}

func TestFileStorageRoundTrip(t *testing.T) {
	backend, err := GetStorage(typedef.MultiStorage{Storage: typedef.Storage{Type: "file"}})
	require.NoError(t, err)
	filename := filepath.Join(t.TempDir(), "archive.tar.gz")
	require.NoError(t, backend.PutObject(filename, []byte("archive data")))
	object, err := backend.GetObject(filename)
	require.NoError(t, err)
	require.Equal(t, []byte("archive data"), object.Content)
}
