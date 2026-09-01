// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type memObject struct {
	data         []byte
	sha256       string
	lastModified time.Time
	contentType  string
}

func (o memObject) info(key string) ObjectInfo {
	return ObjectInfo{
		Key:          key,
		Size:         int64(len(o.data)),
		SHA256:       o.sha256,
		LastModified: o.lastModified,
	}
}

// MemBlobStore is an in-memory BlobStore for tests.
type MemBlobStore struct {
	mu          sync.Mutex
	objects     map[string]memObject
	incomplete  []IncompleteUpload
	putPartSize int64
	now         func() time.Time
}

func NewMemBlobStore() *MemBlobStore {
	return &MemBlobStore{
		objects:     map[string]memObject{},
		putPartSize: PutPartSize,
		now:         time.Now,
	}
}

func (m *MemBlobStore) PutPartSize() int64 { return m.putPartSize }

func (m *MemBlobStore) SetLastModified(key string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return
	}
	obj.lastModified = t
	m.objects[key] = obj
}

func (m *MemBlobStore) InjectIncomplete(u IncompleteUpload) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incomplete = append(m.incomplete, u)
}

func (m *MemBlobStore) Put(_ context.Context, key string, r io.Reader, contentType string) (ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return ObjectInfo{}, err
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasPrefix(key, blobsPrefix) {
		if existing, ok := m.objects[key]; ok {
			return existing.info(key), nil
		}
	}
	obj := memObject{
		data:         data,
		sha256:       hexSum,
		lastModified: now,
		contentType:  contentType,
	}
	m.objects[key] = obj
	return obj.info(key), nil
}

func (m *MemBlobStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, objectNotFoundError{key: key}
	}
	return io.NopCloser(bytes.NewReader(obj.data)), nil
}

func (m *MemBlobStore) Stat(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, objectNotFoundError{key: key}
	}
	return obj.info(key), nil
}

func (m *MemBlobStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MemBlobStore) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; !ok {
		return "", objectNotFoundError{key: key}
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return fmt.Sprintf("mem://%s?expires=%d", key, int(ttl.Seconds())), nil
}

func (m *MemBlobStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ObjectInfo
	for k, obj := range m.objects {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		out = append(out, obj.info(k))
	}
	return out, nil
}

func (m *MemBlobStore) ListIncompleteUploads(_ context.Context, prefix string) ([]IncompleteUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []IncompleteUpload
	for _, u := range m.incomplete {
		if prefix != "" && !strings.HasPrefix(u.Key, prefix) {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func (m *MemBlobStore) AbortMultipartUpload(_ context.Context, key, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.incomplete[:0]
	for _, u := range m.incomplete {
		if u.Key == key && u.UploadID == uploadID {
			continue
		}
		kept = append(kept, u)
	}
	m.incomplete = kept
	return nil
}

func (m *MemBlobStore) EnsureBucket(context.Context) error    { return nil }
func (m *MemBlobStore) EnsureLifecycle(context.Context) error { return nil }
