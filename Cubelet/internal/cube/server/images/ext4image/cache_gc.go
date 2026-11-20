// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package ext4image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"golang.org/x/sys/unix"
)

// CacheGCConfig controls only recoverable, node-local cubebox .ext4 files.
type CacheGCConfig struct {
	Enabled        bool   `toml:"enabled"`
	DryRun         *bool  `toml:"dry_run"`
	Interval       string `toml:"interval"`
	HighPercent    int    `toml:"high_percent"`
	LowPercent     int    `toml:"low_percent"`
	IdleTime       string `toml:"idle_time"`
	MaxBytes       int64  `toml:"max_bytes_per_cycle"`
	SourceManifest string `toml:"source_manifest"`
}

// Omitted dry_run is safe: deletion requires an explicit false value.
func (c CacheGCConfig) dryRun() bool { return c.DryRun == nil || *c.DryRun }

type cacheDiskSpace struct{ total, available int64 }

func cacheSpace() (cacheDiskSpace, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(pmem.GetPmemBasePath("cubebox"), &stat); err != nil {
		return cacheDiskSpace{}, err
	}
	return cacheDiskSpace{int64(stat.Blocks) * int64(stat.Bsize), int64(stat.Bavail) * int64(stat.Bsize)}, nil
}

type CacheGC struct {
	config         CacheGCConfig
	interval, idle time.Duration
	inUse          ArtifactInUseFunc
	ready          func() bool
	space          func() (cacheDiskSpace, error)
	client         *http.Client
	now            func() time.Time
	sweepMu        sync.Mutex
	budgetMu       sync.Mutex
	reserved       int64
	manifestMod    time.Time
}

var activeCacheGC atomic.Pointer[CacheGC]

func NewCacheGC(c CacheGCConfig, inUse ArtifactInUseFunc, ready func() bool) (*CacheGC, error) {
	if inUse == nil || ready == nil {
		return nil, errors.New("rootfs cache GC requires runtime usage and recovery checks")
	}
	if c.Interval == "" {
		c.Interval = "1m"
	}
	if c.IdleTime == "" {
		c.IdleTime = "1h"
	}
	if c.HighPercent == 0 {
		c.HighPercent = 80
	}
	if c.LowPercent == 0 {
		c.LowPercent = 70
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 100 << 30
	}
	interval, err := time.ParseDuration(c.Interval)
	if err != nil || interval <= 0 {
		return nil, errors.New("invalid rootfs cache GC interval")
	}
	idle, err := time.ParseDuration(c.IdleTime)
	if err != nil || idle <= 0 {
		return nil, errors.New("invalid rootfs cache GC idle_time")
	}
	if c.LowPercent <= 0 || c.HighPercent >= 100 || c.LowPercent >= c.HighPercent || c.MaxBytes <= 0 {
		return nil, errors.New("invalid rootfs cache GC watermarks or byte limit")
	}
	if c.SourceManifest != "" && !filepath.IsAbs(c.SourceManifest) {
		return nil, errors.New("rootfs cache source_manifest must be absolute")
	}
	return &CacheGC{config: c, interval: interval, idle: idle, inUse: inUse, ready: ready,
		space: cacheSpace, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}, nil
}

// Activate enables metadata tracking before asynchronous GC starts.
// Capacity reservations remain disabled in dry-run mode.
func (g *CacheGC) Activate() {
	if g.config.Enabled {
		activeCacheGC.Store(g)
	}
}

func (g *CacheGC) Run(ctx context.Context) {
	defer activeCacheGC.CompareAndSwap(g, nil)
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		cycleCtx, cancel := context.WithTimeout(ctx, min(g.interval, 30*time.Second))
		if err := g.importSources(cycleCtx); err != nil && ctx.Err() == nil {
			log.G(ctx).Warnf("rootfs cache source import failed: %v", err)
		}
		if _, err := g.collect(cycleCtx, 0); err != nil && ctx.Err() == nil {
			log.G(ctx).Warnf("rootfs cache GC skipped: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type cacheCandidate struct {
	path  string
	info  os.FileInfo
	entry *cacheEntry
}
type cacheGCResult struct {
	Files                    int
	PlannedBytes, FreedBytes int64
}

// Imports run outside sweepMu: an importer can wait for a download's file lock,
// while that download may itself need a pressure sweep to reserve capacity.
func (g *CacheGC) importSources(ctx context.Context) error {
	if !g.ready() {
		return errors.New("runtime recovery is incomplete")
	}
	manifest := g.config.SourceManifest
	if manifest == "" {
		return nil
	}
	info, err := os.Stat(manifest)
	if err != nil {
		return errors.New("cannot read rootfs cache source manifest")
	}
	if !info.ModTime().Equal(g.manifestMod) {
		if err := ImportCacheSources(ctx, manifest); err != nil {
			return err
		}
		g.manifestMod = info.ModTime()
	}
	return nil
}

// collect serializes eviction. required adds a pending download to the free-space
// goal; periodic sweeps use zero. Runtime and source failures always preserve data.
func (g *CacheGC) collect(ctx context.Context, required int64) (cacheGCResult, error) {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	var result cacheGCResult
	if !g.config.Enabled || !g.ready() {
		return result, errors.New("runtime recovery is incomplete or cache GC is disabled")
	}
	before, err := g.space()
	if err != nil {
		return result, err
	}
	if before.total <= 0 {
		return result, errors.New("invalid cache filesystem capacity")
	}
	g.budgetMu.Lock()
	reserved := g.reserved
	g.budgetMu.Unlock()
	highFree := before.total * int64(100-g.config.HighPercent) / 100
	if before.available-reserved-required >= highFree {
		return result, nil
	}
	goal := max(before.total*int64(100-g.config.LowPercent)/100, highFree+reserved+required)
	entries, err := os.ReadDir(pmem.GetPmemBasePath("cubebox"))
	if err != nil {
		return result, err
	}
	var candidates []cacheCandidate
	for _, dir := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !dir.IsDir() {
			continue
		}
		path, info, err := cacheFile(dir.Name())
		if err != nil {
			continue
		}
		entry, err := readCacheEntry(path)
		if err != nil || entry.SizeBytes != info.Size() || g.now().Sub(entry.LastUsed) < g.idle {
			continue
		}
		candidates = append(candidates, cacheCandidate{path, info, entry})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].entry.LastUsed.Before(candidates[j].entry.LastUsed) })
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		space, err := g.space()
		if err != nil {
			return result, err
		}
		available := space.available
		if g.config.dryRun() {
			available += result.PlannedBytes
		}
		if available >= goal {
			break
		}
		if candidate.info.Size() > g.config.MaxBytes-result.PlannedBytes {
			continue
		}
		deleted, err := g.evict(ctx, candidate)
		if err != nil {
			return result, err
		}
		if deleted {
			result.Files++
			result.PlannedBytes += candidate.info.Size()
		}
	}
	after, err := g.space()
	if err != nil {
		return result, err
	}
	if !g.config.dryRun() {
		result.FreedBytes = max(0, after.available-before.available)
	}
	log.G(ctx).Infof("rootfs cache GC dry_run=%t files=%d planned_bytes=%d freed_bytes=%d available_bytes=%d target_bytes=%d",
		g.config.dryRun(), result.Files, result.PlannedBytes, result.FreedBytes, after.available, goal)
	return result, nil
}

func (g *CacheGC) evict(ctx context.Context, c cacheCandidate) (bool, error) {
	// Network probes must not block a creator's shared artifact pin. Recheck
	// the file, source and usage under the exclusive lease after the probe.
	used, err := g.inUse(c.entry.ArtifactID)
	if err != nil || used || !g.ready() || !g.sourceReadable(ctx, c.entry.CacheSource) {
		return false, nil
	}
	release, ok := pmem.TryEvictArtifact("cubebox", c.entry.ArtifactID)
	if !ok {
		return false, nil
	}
	defer release()
	path, info, err := cacheFile(c.entry.ArtifactID)
	if err != nil || path != c.path || !os.SameFile(info, c.info) {
		return false, nil
	}
	entry, err := readCacheEntry(path)
	if err != nil || entry.CacheSource != c.entry.CacheSource || info.Size() != entry.SizeBytes || g.now().Sub(entry.LastUsed) < g.idle {
		return false, nil
	}
	// Keep the exclusive lease until unlink; creators wait and then rehydrate.
	used, err = g.inUse(entry.ArtifactID)
	if err != nil || used || !g.ready() {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !g.config.dryRun() {
		if err := os.Remove(path); err != nil {
			return false, err
		}
	}
	log.G(ctx).Infof("rootfs cache eviction artifact=%s bytes=%d dry_run=%t", entry.ArtifactID, info.Size(), g.config.dryRun())
	return true, nil
}

func (g *CacheGC) sourceReadable(ctx context.Context, source CacheSource) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.downloadURL(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := g.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("X-Cube-Artifact-Id"); id != "" && id != source.ArtifactID {
		return false
	}
	if resp.StatusCode == http.StatusPartialContent {
		if resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-0/%d", source.SizeBytes) {
			return false
		}
	} else if resp.StatusCode != http.StatusOK || resp.ContentLength != source.SizeBytes {
		return false
	}
	_, err = io.ReadFull(resp.Body, make([]byte, 1))
	return err == nil
}

// reserve counts simultaneous downloads, even when the filesystem has not yet
// allocated their blocks. It never holds the budget lock while evicting.
func (g *CacheGC) reserve(ctx context.Context, size int64) (func(), error) {
	if size <= 0 {
		return nil, errors.New("no space reservation possible without rootfs size")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.budgetMu.Lock()
		space, err := g.space()
		if err == nil && space.total > 0 && size <= space.available-g.reserved-space.total*int64(100-g.config.HighPercent)/100 {
			g.reserved += size
			g.budgetMu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.budgetMu.Lock(); g.reserved -= size; g.budgetMu.Unlock() }) }, nil
		}
		g.budgetMu.Unlock()
		if err != nil {
			return nil, err
		}
		if attempt == 0 {
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := g.collect(sweepCtx, size)
			cancel()
			if err != nil {
				return nil, err
			}
		}
	}
	return nil, errors.New("no space left for rootfs download within cache high watermark")
}

func reserveCacheDownload(ctx context.Context, size int64) (func(), error) {
	if g := activeCacheGC.Load(); g != nil && !g.config.dryRun() {
		return g.reserve(ctx, size)
	}
	return func() {}, nil
}
