package storage

import "time"

type Object struct {
	Path         string
	Content      []byte
	LastModified time.Time
}

type Storage interface {
	// ListObject returns a list of all objects in the storage backend.
	ListObject(prefix string) ([]Object, error)
	// GetObject returns the object identified by the given identifier.
	GetObject(identifier string) (Object, error)
	// PutObject stores the data in the storage backend identified by the given identifier.
	PutObject(identifier string, data []byte) error
	// DeleteObject deletes the object identified by the given identifier.
	DeleteObject(identifier string) error
}
