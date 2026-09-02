package storage

// Package storage abstracts attachment storage (local FS or S3) used by the
// collaboration module (blueprint §3.2 `platform/storage`).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrTooLarge indicates the upload exceeds the configured size cap.
var ErrTooLarge = errors.New("storage: object exceeds max size")

// ErrUnsupportedType indicates the MIME type is not in the allow-list.
var ErrUnsupportedType = errors.New("storage: mime type not allowed")

// Store abstracts binary object persistence.
type Store interface {
	// Put writes the object and returns its storage key (not the public URL).
	Put(ctx context.Context, key string, size int64, mimeType string, r io.Reader) error
	// Get returns a reader over the stored object.
	Get(ctx context.Context, key string) (io.ReadSeekCloser, error)
	// Delete removes the object (best-effort; missing keys are not errors).
	Delete(ctx context.Context, key string) error
}

// New builds a Store from config values; backend must be "local" or "s3".
func New(backend, localBasePath, s3Bucket, s3Region, s3AccessKey, s3SecretKey string) (Store, error) {
	switch strings.ToLower(backend) {
	case "local":
		return NewLocal(localBasePath)
	case "s3":
		return nil, fmt.Errorf("storage: s3 backend requires the AWS SDK dependency; enable via build tag 's3' (see docs/storage.md)")
	default:
		return nil, fmt.Errorf("storage: unknown backend %q", backend)
	}
}

// LocalStore persists objects on the local filesystem (Docker volume).
type LocalStore struct {
	basePath string
}

// NewLocal builds a LocalStore rooted at basePath, creating it if missing.
func NewLocal(basePath string) (*LocalStore, error) {
	if err := os.MkdirAll(basePath, 0o750); err != nil {
		return nil, fmt.Errorf("storage: mkdir %s: %w", basePath, err)
	}
	return &LocalStore{basePath: basePath}, nil
}

// Put writes the object atomically (temp file + rename).
func (s *LocalStore) Put(_ context.Context, key string, size int64, _ string, r io.Reader) error {
	dst := s.resolve(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("storage: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return fmt.Errorf("storage: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	n, err := io.Copy(tmp, r)
	if err != nil {
		return fmt.Errorf("storage: write: %w", err)
	}
	if size > 0 && n > size {
		return ErrTooLarge
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("storage: rename: %w", err)
	}
	return nil
}

// Get opens the stored object.
func (s *LocalStore) Get(_ context.Context, key string) (io.ReadSeekCloser, error) {
	return os.Open(s.resolve(key))
}

// Delete removes the stored object if it exists.
func (s *LocalStore) Delete(_ context.Context, key string) error {
	err := os.Remove(s.resolve(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// resolve guards against path traversal and returns the absolute path.
func (s *LocalStore) resolve(key string) string {
	clean := filepath.Clean("/" + strings.TrimPrefix(key, "/"))
	return filepath.Join(s.basePath, clean)
}

// RandomKey generates a storage key with entropy to avoid collisions:
// <yyyy>/<mm>/<uuid-hex>.
func RandomKey(originalName string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	ext := filepath.Ext(originalName)
	now := timeNowUTC()
	return fmt.Sprintf("%04d/%02d/%s%s", now.Year(), int(now.Month()), hex.EncodeToString(b), ext)
}
