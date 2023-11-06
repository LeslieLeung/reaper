package storage

import (
	"io"
	"os"
	"path"
)

var _ Storage = (*File)(nil)

type File struct {
}

// createPathIfNotExist creates the directory for the given file path if it does not exist.
func createPathIfNotExist(filePath string) error {
	dir := path.Dir(filePath)
	_, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			err := os.MkdirAll(dir, 0774)
			if err != nil {
				return err
			}
			// os.MkdirAll set the dir permissions before the umask
			// we need to use os.Chmod to ensure the permissions of the created directory are 774
			// because the default umask will prevent that and cause the permissions to be 755
			err = os.Chmod(dir, 0774)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (f File) PutObject(identifier string, data []byte) error {
	if err := createPathIfNotExist(identifier); err != nil {
		return err
	}
	return os.WriteFile(identifier, data, 0664)
}

func (f File) PutObjectFromPath(path string, identifier string) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()

	if err := createPathIfNotExist(identifier); err != nil {
		return err
	}
	destination, err := os.Create(identifier)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}
