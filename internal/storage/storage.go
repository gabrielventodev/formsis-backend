// Package storage keeps uploaded documents in S3-compatible storage (MinIO, R2, S3) or on local disk for development.
// Files are always streamed through the API, so the browser never needs to reach the bucket directly.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrNotFound = errors.New("storage: object not found")

type Store interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

type Config struct {
	Driver    string // "s3" or "local"
	Dir       string // local driver
	Endpoint  string // host:port, e.g. "storage:9000"
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
}

func New(ctx context.Context, c Config) (Store, error) {
	switch c.Driver {
	case "", "local":
		return NewLocal(c.Dir)
	case "s3":
		return NewS3(ctx, c)
	}
	return nil, fmt.Errorf("storage: unknown driver %q", c.Driver)
}

// Local stores objects as files under a directory.
type Local struct{ dir string }

func NewLocal(dir string) (*Local, error) {
	if dir == "" {
		dir = "data/uploads"
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Local{dir: dir}, nil
}

func (l *Local) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(key, "..") {
		return "", fmt.Errorf("storage: invalid key %q", key)
	}
	return filepath.Join(l.dir, clean), nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	return f.Close()
}

func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// S3 stores objects in an S3-compatible bucket, creating it on startup if missing.
type S3 struct {
	client *minio.Client
	bucket string
}

func NewS3(ctx context.Context, c Config) (*S3, error) {
	client, err := minio.New(c.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: c.UseSSL,
		Region: c.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	exists, err := client.BucketExists(ctx, c.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage: check bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, c.Bucket, minio.MakeBucketOptions{Region: c.Region}); err != nil {
			return nil, fmt.Errorf("storage: create bucket: %w", err)
		}
	}
	return &S3{client: client, bucket: c.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
