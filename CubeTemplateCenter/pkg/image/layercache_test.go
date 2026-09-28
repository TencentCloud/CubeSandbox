// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDigest computes a sha256 digest string ("sha256:<hex>") for content.
func testDigest(content []byte) string {
	h := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(h[:])
}

func newTestCache(t *testing.T, quota int64) *layerCache {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	return &layerCache{dir: dir, quota: quota}
}

func TestLayerCachePutAndGet(t *testing.T) {
	c := newTestCache(t, 1<<20) // 1 MiB quota
	ctx := context.Background()
	content := []byte("hello layer cache world")
	digest := testDigest(content)

	// Miss before put.
	rc, _, err := c.Get(ctx, digest)
	if err != nil {
		t.Fatalf("Get miss: %v", err)
	}
	if rc != nil {
		t.Fatalf("expected miss, got hit")
	}

	// Put.
	if _, err := c.Put(ctx, digest, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Hit after put.
	rc, size, err := c.Get(ctx, digest)
	if err != nil {
		t.Fatalf("Get hit: %v", err)
	}
	if rc == nil {
		t.Fatalf("expected hit, got miss")
	}
	defer rc.Close()
	if size != int64(len(content)) {
		t.Fatalf("size=%d, want %d", size, len(content))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %q, want %q", got, content)
	}
}

func TestLayerCacheDigestMismatch(t *testing.T) {
	c := newTestCache(t, 1<<20)
	ctx := context.Background()
	content := []byte("actual content")
	wrongDigest := testDigest([]byte("different content"))

	_, err := c.Put(ctx, wrongDigest, bytes.NewReader(content), int64(len(content)))
	if err == nil {
		t.Fatalf("expected digest mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}
}

func TestLayerCacheEviction(t *testing.T) {
	// Small quota: 100 bytes. Each blob is ~50 bytes, so 2 fit, 3rd triggers eviction.
	c := newTestCache(t, 100)
	ctx := context.Background()

	digests := make([]string, 3)
	for i := 0; i < 3; i++ {
		content := bytes.Repeat([]byte{byte('a' + i)}, 50)
		digest := testDigest(content)
		digests[i] = digest
		if _, err := c.Put(ctx, digest, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		// Stagger last-access so eviction order is deterministic: blob 0 oldest.
		c.mu.Lock()
		meta, _ := c.readMetaLocked(digest)
		if meta != nil {
			meta.LastAccess = time.Now().Unix() - int64(100-i)
			data, _ := json.Marshal(meta)
			_ = os.WriteFile(c.metaPath(digest), data, 0o644)
		}
		c.mu.Unlock()
	}

	// After 3 puts with quota 100, total would be 150, so eviction should have
	// removed the oldest (blob 0) to get back under quota.
	rc, _, err := c.Get(ctx, digests[0])
	if err != nil {
		t.Fatalf("Get blob0: %v", err)
	}
	if rc != nil {
		rc.Close()
		t.Fatalf("expected blob0 evicted, but still present")
	}

	for i := 1; i < 3; i++ {
		rc, _, err := c.Get(ctx, digests[i])
		if err != nil {
			t.Fatalf("Get blob%d: %v", i, err)
		}
		if rc == nil {
			t.Fatalf("expected blob%d present, but evicted", i)
		}
		rc.Close()
	}
}

func TestLayerCacheConcurrentPut(t *testing.T) {
	c := newTestCache(t, 1<<20)
	ctx := context.Background()
	content := []byte("concurrent content")
	digest := testDigest(content)

	// Two goroutines put the same digest concurrently; both should succeed
	// (one wins the rename, the other falls back to the winner's file).
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := c.Put(ctx, digest, bytes.NewReader(content), int64(len(content)))
			done <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Put: %v", err)
		}
	}

	// Final state: blob exists and is correct.
	rc, _, err := c.Get(ctx, digest)
	if err != nil || rc == nil {
		t.Fatalf("expected blob after concurrent put, err=%v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch after concurrent put")
	}
}

func TestLayerCacheTeeWriter(t *testing.T) {
	c := newTestCache(t, 1<<20)
	ctx := context.Background()
	content := []byte("tee writer content for streaming")
	digest := testDigest(content)

	// Simulate the download path: tee into cache while writing to a primary file.
	primary := &bytes.Buffer{}
	tee := newLayerCacheTeeWriter(ctx, c, digest)

	mw := io.MultiWriter(primary, tee)
	if _, err := mw.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tee.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Primary got the content.
	if !bytes.Equal(primary.Bytes(), content) {
		t.Fatalf("primary content mismatch")
	}

	// Cache got the content.
	rc, _, err := c.Get(ctx, digest)
	if err != nil || rc == nil {
		t.Fatalf("expected cache hit after tee commit, err=%v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Fatalf("cache content mismatch")
	}
}

func TestLayerCacheTeeWriterDigestMismatch(t *testing.T) {
	c := newTestCache(t, 1<<20)
	ctx := context.Background()
	content := []byte("actual")
	wrongDigest := testDigest([]byte("expected"))

	tee := newLayerCacheTeeWriter(ctx, c, wrongDigest)
	if _, err := tee.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err := tee.Commit()
	if err == nil {
		t.Fatalf("expected digest mismatch on commit")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch, got %v", err)
	}

	// Blob must NOT be in cache.
	rc, _, _ := c.Get(ctx, wrongDigest)
	if rc != nil {
		rc.Close()
		t.Fatalf("expected no blob after digest mismatch")
	}
}

func TestLayerCacheTeeWriterAbort(t *testing.T) {
	c := newTestCache(t, 1<<20)
	ctx := context.Background()
	content := []byte("partial content")
	digest := testDigest(content)

	tee := newLayerCacheTeeWriter(ctx, c, digest)
	if _, err := tee.Write(content[:5]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tee.Abort()

	// Blob must NOT be in cache after abort.
	rc, _, _ := c.Get(ctx, digest)
	if rc != nil {
		rc.Close()
		t.Fatalf("expected no blob after abort")
	}
}

func TestLayerCacheDisabled(t *testing.T) {
	// getLayerCache with empty env returns nil (disabled).
	t.Setenv(layerCacheDirEnv, "")
	// Reset the singleton for this test. sync.Once cannot be copied, so we save
	// only the cache pointer and install a fresh Once; the deferred reset
	// installs another fresh Once so later tests re-initialize cleanly.
	oldCache := sharedLayerCache
	defer func() {
		sharedLayerCache = oldCache
		sharedLayerCacheOnce = sync.Once{}
	}()
	sharedLayerCache = nil
	sharedLayerCacheOnce = sync.Once{}

	c := getLayerCache(context.Background())
	if c != nil {
		t.Fatalf("expected nil cache when disabled, got %v", c)
	}
}

func TestLayerCacheQuotaEnforcement(t *testing.T) {
	c := newTestCache(t, 60) // 60 bytes quota
	ctx := context.Background()

	// Put two 50-byte blobs; total 100 > quota 60, so eviction must occur.
	for i := 0; i < 2; i++ {
		content := bytes.Repeat([]byte{byte('x' + i)}, 50)
		digest := testDigest(content)
		if _, err := c.Put(ctx, digest, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	// After eviction, total size should be <= quota.
	total, _, err := c.scanBlobs()
	if err != nil {
		t.Fatalf("scanBlobs: %v", err)
	}
	if total > c.quota {
		t.Fatalf("total=%d exceeds quota=%d after eviction", total, c.quota)
	}
}
