package storage

type Storage interface {
	// PutObject stores the data in the storage backend identified by the given identifier.
	PutObject(identifier string, data []byte) error
	// PutObjectFromPath reads the file at the given path and stores the data in the storage backend identified by the given identifier.
	PutObjectFromPath(path string, identifier string) error
}
