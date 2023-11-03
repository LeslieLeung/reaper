package storage

import (
	"io"
	"os"
	"path"
)

var _ Storage = (*File)(nil)

type File struct {
}

func (f File) PutObject(identifier string, data []byte) error {
	dir := path.Dir(identifier)
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
	err = os.WriteFile(identifier, data, 0664)
	return err
}

func (f File) PutObjectFromPath(path string, identifier string) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(identifier)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}
