package storage

import (
	"bytes"
	"context"
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var _ Storage = (*S3)(nil)

type S3 struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string

	client *minio.Client
}

func New(endpoint, bucket, region, accessKeyID, secretAccessKey string) (*S3, error) {
	s3 := &S3{
		Endpoint:        endpoint,
		Bucket:          bucket,
		Region:          region,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	}

	client, err := minio.New(s3.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s3.AccessKeyID, s3.SecretAccessKey, ""),
		Secure: true,
	})
	if err != nil {
		return nil, err
	}
	s3.client = client
	return s3, nil
}

func (s S3) ListObject(prefix string) ([]Object, error) {
	// TODO implement me
	panic("implement me")
}

func (s S3) GetObject(identifier string) (Object, error) {
	// TODO implement me
	panic("implement me")
}

func (s S3) PutObject(identifier string, data []byte) error {
	size := int64(len(data))
	info, err := s.client.PutObject(context.Background(), s.Bucket, identifier, bytes.NewReader(data), size, minio.PutObjectOptions{})
	if err != nil {
		return err
	}
	ui.Printf("info: %v", info)
	return nil
}

func (s S3) DeleteObject(identifier string) error {
	// TODO implement me
	panic("implement me")
}
