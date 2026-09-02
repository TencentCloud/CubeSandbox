// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"context"
	"io"
	"strings"
	"time"
)

const (
	// PutPartSize is the multipart part size. Must be explicit: minio-go
	// otherwise buffers 512MiB per concurrent unknown-length upload.
	PutPartSize = 64 << 20

	metaSHA256Key = "sha256"
)

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	SHA256       string
	LastModified time.Time
}

// IncompleteUpload is a leftover multipart upload.
type IncompleteUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// BlobStore is the object-storage backend for warehouse blobs.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, contentType string) (ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	ListIncompleteUploads(ctx context.Context, prefix string) ([]IncompleteUpload, error)
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
	EnsureBucket(ctx context.Context) error
	EnsureLifecycle(ctx context.Context) error
}

// ErrNotExist means the object (or bucket) is missing.
type objectNotFoundError struct{ key string }

func (e objectNotFoundError) Error() string { return "object not found: " + e.key }

func IsNotExist(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(objectNotFoundError); ok {
		return true
	}
	return isS3NotFound(err)
}

func formatChecksum(sum string) string {
	if sum != "" && !strings.HasPrefix(sum, "sha256:") {
		return "sha256:" + sum
	}
	return sum
}

func objectKeyOf(arch, component, version, stored string) string {
	if stored != "" {
		return stored
	}
	return ObjectKey(arch, component, version)
}
