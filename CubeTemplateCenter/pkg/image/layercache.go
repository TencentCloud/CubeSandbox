// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// Layer cache: avoid re-downloading the same layer across builds.
//
// Container images share layers aggressively (base images, common deps), so
// building many templates from related images re-downloads the same blobs.
// This cache stores downloaded layer tarballs keyed by digest on local disk,
// with LRU eviction and a disk-quota cap so it cannot grow unbounded.
//
// Layout:
//
//	<cacheDir>/blobs/sha256/<digest>            layer tarball (content-addressed)
//	<cacheDir>/blobs/sha256/<digest>.meta.json  {size, last_access, digest}
//	<cacheDir>/lock                             process-wide cleanup mutex
//
// Concurrency: a cache hit is a read-only open, safe across processes. A miss
// writes to <digest>.tmp-<pid> then renames, so concurrent writers of the same
// digest produce one winner (the other's rename fails and it re-opens the
// winner's file). Eviction takes the per-cache-dir file lock to avoid two
// processes evicting the same blob simultaneously.
//
// Disabled by default; enable with CUBE_TEMPLATECENTER_LAYER_CACHE_DIR.

const (
	// layerCacheDirEnv enables the cache and points it at a directory.
	layerCacheDirEnv = "CUBE_TEMPLATECENTER_LAYER_CACHE_DIR"
	// layerCacheQuotaEnv caps total cache size in bytes. Default 20GiB.
	layerCacheQuotaEnv       = "CUBE_TEMPLATECENTER_LAYER_CACHE_QUOTA_BYTES"
	defaultLayerCacheQuota   = 20 << 30 // 20 GiB
	layerCacheMetaSuffix     = ".meta.json"
	layerCacheTmpSuffix      = ".tmp"
	layerCacheAccessInterval = 30 * time.Second // touch mtime at most this often
)

// layerCache is a content-addressed, LRU-evicted, quota-capped on-disk cache
// for layer tarballs. The zero value is unusable; construct via newLayerCache.
type layerCache struct {
	dir   string
	quota int64
	// mu serializes meta updates and eviction within this process. Cross-process
	// eviction is serialized by a lock file.
	mu sync.Mutex
}

var (
	sharedLayerCache     *layerCache
	sharedLayerCacheOnce sync.Once
)

// getLayerCache returns the process-wide cache, or nil when disabled. The
// cache directory is created on first use; an unwritable dir disables the
// cache (builds fall back to always-download).
func getLayerCache(ctx context.Context) *layerCache {
	sharedLayerCacheOnce.Do(func() {
		dir := strings.TrimSpace(os.Getenv(layerCacheDirEnv))
		if dir == "" {
			return
		}
		quota := int64(defaultLayerCacheQuota)
		if v := strings.TrimSpace(os.Getenv(layerCacheQuotaEnv)); v != "" {
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
				quota = parsed
			}
		}
		blobsDir := filepath.Join(dir, "blobs", "sha256")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			log.G(ctx).Warnf("layer cache disabled: cannot create %s: %v", blobsDir, err)
			return
		}
		sharedLayerCache = &layerCache{dir: dir, quota: quota}
		log.G(ctx).Infof("layer cache enabled: dir=%s quota=%d", dir, quota)
	})
	return sharedLayerCache
}

// blobPath returns the on-disk path for a digest's blob.
func (c *layerCache) blobPath(digest string) string {
	// digest is "sha256:<hex>"; store under blobs/sha256/<hex>.
	hexPart := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(c.dir, "blobs", "sha256", hexPart)
}

func (c *layerCache) metaPath(digest string) string {
	return c.blobPath(digest) + layerCacheMetaSuffix
}

// layerCacheMeta is the sidecar metadata for a cached blob.
type layerCacheMeta struct {
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	LastAccess int64  `json:"last_access"` // unix seconds
}

// Get returns an open ReadCloser for the cached blob, or (nil, nil) on a miss.
// A hit updates the blob's last-access time (throttled) for LRU.
func (c *layerCache) Get(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
	if c == nil {
		return nil, 0, nil
	}
	path := c.blobPath(digest)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil // miss
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	// Verify the blob is complete (size matches meta). A partial write from a
	// crashed process would have a stale meta; treat as miss and overwrite.
	meta, merr := c.readMeta(digest)
	if merr != nil || meta.Size != st.Size() {
		_ = f.Close()
		// Incomplete or corrupt: remove so the caller re-downloads cleanly.
		_ = os.Remove(path)
		_ = os.Remove(c.metaPath(digest))
		return nil, 0, nil
	}
	c.touch(digest, st.Size())
	return f, st.Size(), nil
}

// Put writes data for digest into the cache atomically (tmp + rename), then
// evicts if over quota. Concurrent puts of the same digest produce one winner;
// the loser re-opens the winner's file (rename is atomic and idempotent).
//
// The caller owns data; Put consumes it fully. Returns the blob path for the
// caller to stream from, or an error.
func (c *layerCache) Put(ctx context.Context, digest string, data io.Reader, sizeHint int64) (string, error) {
	if c == nil {
		return "", fmt.Errorf("layer cache disabled")
	}
	finalPath := c.blobPath(digest)
	tmpPath := finalPath + layerCacheTmpSuffix + "-" + strconv.Itoa(os.Getpid())

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("create tmp blob: %w", err)
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	n, err := io.Copy(w, data)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write blob: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close blob: %w", err)
	}
	// Verify content matches the claimed digest (defense against a buggy or
	// malicious registry returning wrong bytes for a digest-addressed blob).
	gotHex := hex.EncodeToString(h.Sum(nil))
	wantHex := strings.TrimPrefix(digest, "sha256:")
	if gotHex != wantHex {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("digest mismatch: got sha256:%s, want %s", gotHex, digest)
	}
	// Atomic publish. If a peer already published, rename fails on some
	// filesystems; in that case just drop our tmp and use theirs.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		if _, statErr := os.Stat(finalPath); statErr != nil {
			return "", fmt.Errorf("publish blob: %w", err)
		}
	}
	c.writeMeta(digest, n)
	// Evict over-quota blobs (best-effort; failure leaves cache temporarily
	// over quota, next put retries).
	if err := c.evictIfNeeded(ctx); err != nil {
		log.G(ctx).Warnf("layer cache eviction fail: %v", err)
	}
	return finalPath, nil
}

// touch updates last-access, throttled to avoid a metadata write on every hit.
func (c *layerCache) touch(digest string, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta, err := c.readMetaLocked(digest)
	if err == nil && time.Since(time.Unix(meta.LastAccess, 0)) < layerCacheAccessInterval {
		return
	}
	c.writeMetaLocked(digest, size)
}

func (c *layerCache) readMeta(digest string) (*layerCacheMeta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readMetaLocked(digest)
}

func (c *layerCache) readMetaLocked(digest string) (*layerCacheMeta, error) {
	data, err := os.ReadFile(c.metaPath(digest))
	if err != nil {
		return nil, err
	}
	var m layerCacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (c *layerCache) writeMeta(digest string, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeMetaLocked(digest, size)
}

func (c *layerCache) writeMetaLocked(digest string, size int64) {
	m := layerCacheMeta{
		Digest:     digest,
		Size:       size,
		LastAccess: time.Now().Unix(),
	}
	data, _ := json.Marshal(m)
	// Best-effort; a lost meta update just means less-accurate LRU.
	_ = os.WriteFile(c.metaPath(digest), data, 0o644)
}

// evictIfNeeded removes least-recently-used blobs until total size is under
// quota. Serialized cross-process by a lock file; loser skips eviction.
func (c *layerCache) evictIfNeeded(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	lockPath := filepath.Join(c.dir, "evict.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	// Non-blocking advisory lock; if a peer is evicting, skip.
	if err := lockFileExclusiveNonblock(lockFile); err != nil {
		return nil
	}
	defer unlockFile(lockFile)

	total, entries, err := c.scanBlobs()
	if err != nil {
		return err
	}
	if total <= c.quota {
		return nil
	}
	// Sort oldest-first by last access.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastAccess < entries[j].lastAccess
	})
	evicted := int64(0)
	for _, e := range entries {
		if total-evicted <= c.quota {
			break
		}
		if err := os.Remove(e.blobPath); err != nil && !os.IsNotExist(err) {
			log.G(ctx).Warnf("layer cache evict %s fail: %v", e.blobPath, err)
			continue
		}
		_ = os.Remove(e.blobPath + layerCacheMetaSuffix)
		evicted += e.size
	}
	if evicted > 0 {
		log.G(ctx).Infof("layer cache evicted %d bytes (total was %d, quota %d)", evicted, total, c.quota)
	}
	return nil
}

// layerCacheTeeWriter streams a layer into the cache as it is downloaded.
// It writes to a tmp file; Commit verifies the digest and atomically renames
// into place, Abort discards the tmp file. This lets the download path write
// the prefetch file and the cache blob in a single pass (io.MultiWriter)
// without buffering the whole layer in memory.
type layerCacheTeeWriter struct {
	ctx    context.Context
	cache  *layerCache
	digest string
	tmp    *os.File
	tmpPath string
	hash   hash.Hash
	size   int64
	err    error
	closed bool
}

func newLayerCacheTeeWriter(ctx context.Context, c *layerCache, digest string) *layerCacheTeeWriter {
	w := &layerCacheTeeWriter{
		ctx:    ctx,
		cache:  c,
		digest: digest,
		hash:   sha256.New(),
	}
	tmpPath := c.blobPath(digest) + layerCacheTmpSuffix + "-" + strconv.Itoa(os.Getpid())
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		w.err = err
		return w
	}
	w.tmp = f
	w.tmpPath = tmpPath
	return w
}

// Write implements io.Writer. Errors are sticky: once the tmp write fails,
// subsequent writes are no-ops and Commit returns the stored error.
func (w *layerCacheTeeWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return len(p), nil // swallow: let the primary (prefetch) writer proceed
	}
	if w.tmp == nil {
		return len(p), nil
	}
	n, err := w.tmp.Write(p)
	if err != nil {
		w.err = err
		return n, err
	}
	w.hash.Write(p[:n])
	w.size += int64(n)
	return n, nil
}

// Commit finalizes the cache entry: close, verify digest, rename into place.
// Safe to call after a partial write only if Abort was not called; a digest
// mismatch removes the tmp file and returns an error.
func (w *layerCacheTeeWriter) Commit() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.tmp == nil {
		return w.err
	}
	if err := w.tmp.Close(); err != nil {
		_ = os.Remove(w.tmpPath)
		return err
	}
	if w.err != nil {
		_ = os.Remove(w.tmpPath)
		return w.err
	}
	gotHex := hex.EncodeToString(w.hash.Sum(nil))
	wantHex := strings.TrimPrefix(w.digest, "sha256:")
	if gotHex != wantHex {
		_ = os.Remove(w.tmpPath)
		return fmt.Errorf("digest mismatch: got sha256:%s, want %s", gotHex, w.digest)
	}
	finalPath := w.cache.blobPath(w.digest)
	if err := os.Rename(w.tmpPath, finalPath); err != nil {
		_ = os.Remove(w.tmpPath)
		if _, statErr := os.Stat(finalPath); statErr != nil {
			return fmt.Errorf("publish blob: %w", err)
		}
		// A peer already published it; treat as success.
	}
	w.cache.writeMeta(w.digest, w.size)
	if err := w.cache.evictIfNeeded(w.ctx); err != nil {
		log.G(w.ctx).Warnf("layer cache eviction fail: %v", err)
	}
	return nil
}

// Abort discards the tmp file without publishing. Called when the download
// fails partway.
func (w *layerCacheTeeWriter) Abort() {
	if w.closed {
		return
	}
	w.closed = true
	if w.tmp != nil {
		_ = w.tmp.Close()
	}
	if w.tmpPath != "" {
		_ = os.Remove(w.tmpPath)
	}
}

type blobEntry struct {
	blobPath   string
	size       int64
	lastAccess int64
}

func (c *layerCache) scanBlobs() (int64, []blobEntry, error) {
	blobsDir := filepath.Join(c.dir, "blobs", "sha256")
	files, err := os.ReadDir(blobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, err
	}
	var total int64
	var entries []blobEntry
	for _, fi := range files {
		name := fi.Name()
		if strings.HasSuffix(name, layerCacheMetaSuffix) || strings.HasSuffix(name, layerCacheTmpSuffix) {
			continue
		}
		info, err := fi.Info()
		if err != nil {
			continue
		}
		blobPath := filepath.Join(blobsDir, name)
		lastAccess := info.ModTime().Unix()
		// Prefer meta's last_access over file mtime (touch updates meta only).
		if data, err := os.ReadFile(blobPath + layerCacheMetaSuffix); err == nil {
			var m layerCacheMeta
			if json.Unmarshal(data, &m) == nil && m.LastAccess > 0 {
				lastAccess = m.LastAccess
			}
		}
		entries = append(entries, blobEntry{blobPath: blobPath, size: info.Size(), lastAccess: lastAccess})
		total += info.Size()
	}
	return total, entries, nil
}
