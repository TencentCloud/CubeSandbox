// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SnapshotCatalogEntry captures everything cubelet needs in order to roll back
// to a snapshot, restore-from-snapshot for a fresh sandbox, or clean up the
// underlying cubecow artifacts, without requiring master to carry physical
// cubecow/path references in its tables.
//
// Persisted as <snapshot_path>/catalog.json by CommitSandbox (Kind=KindRuntimeSnapshot)
// and AppSnapshot (Kind=KindTemplate). Dev paths are re-resolved from cubecow on
// demand because they can change across activations. Unknown JSON fields are
// tolerated and missing fields decode as zero values so old catalog files keep
// working after schema extensions.
//
// Runtime artifact identity belongs here, not in the rootfs artifact cache. In
// particular, future "redo snapshot" checks should compare the node's active
// kernel artifact identity with the identity recorded in this catalog entry;
// a kernel mismatch requires rebuilding the snapshot/template replica, but does
// not by itself require rebuilding the rootfs ext4 artifact.
type SnapshotCatalogEntry struct {
	SnapshotID   string `json:"snapshot_id"`
	InstanceType string `json:"instance_type,omitempty"`
	SpecDir      string `json:"spec_dir,omitempty"`
	SnapshotPath string `json:"snapshot_path"`
	MetaDir      string `json:"meta_dir"`
	RootfsVol    string `json:"rootfs_vol"`
	RootfsKind   string `json:"rootfs_kind"`
	MemoryVol    string `json:"memory_vol"`
	MemoryKind   string `json:"memory_kind"`
	// BuildRootfsVol/Kind track the temporary writable working layer created
	// during template build (AppSnapshot path). They must be cleaned up at
	// template delete time. Empty for runtime snapshots (CommitSandbox), which
	// never produce a build artifact.
	BuildRootfsVol  string `json:"build_rootfs_vol,omitempty"`
	BuildRootfsKind string `json:"build_rootfs_kind,omitempty"`
	RootfsSizeBytes uint64 `json:"rootfs_size_bytes,omitempty"`
	// Kind distinguishes the producer/semantics of this catalog entry so
	// CleanupTemplate/GetLocalSnapshot consumers can branch where needed.
	// Empty == legacy entry (pre-v4) and should be treated as a runtime
	// snapshot for backward compatibility.
	Kind      string `json:"kind,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Catalog entry kinds. See SnapshotCatalogEntry.Kind.
const (
	// CatalogKindRuntimeSnapshot is produced by CommitSandbox (taking a
	// snapshot of a running sandbox).
	CatalogKindRuntimeSnapshot = "runtime_snapshot"
	// CatalogKindTemplate is produced by AppSnapshot (building a template
	// from an image / one-shot sandbox).
	CatalogKindTemplate = "template"
)

// SnapshotCatalogEntryResolved enriches a catalog entry with freshly re-resolved
// cubecow device paths.
type SnapshotCatalogEntryResolved struct {
	SnapshotCatalogEntry
	RootfsDev string `json:"rootfs_dev,omitempty"`
	MemoryDev string `json:"memory_dev,omitempty"`
}

const snapshotCatalogFileName = "catalog.json"

// ErrSnapshotCatalogNotFound is returned when no catalog can be located for the
// given snapshot id under any registered snapshot root.
var ErrSnapshotCatalogNotFound = errors.New("snapshot catalog not found")

var (
	snapshotCatalogMu sync.RWMutex
	// snapshotCatalogRoots is the list of snapshot directories cubelet should
	// scan/index for local snapshot catalogs. The cubelet boot path can extend
	// it; by default it contains DefaultSnapshotDir (registered lazily below).
	snapshotCatalogRoots = []string{}
	// snapshotCatalogIndex is a best-effort in-memory cache keyed by snapshotID.
	// Lookups go through the filesystem if the in-memory cache misses.
	snapshotCatalogIndex = map[string]*SnapshotCatalogEntry{}
)

// SetSnapshotCatalogRoots replaces the configured set of snapshot root
// directories. Each root must point at the same parent that CommitSandbox
// uses for snapshot output (e.g. DefaultSnapshotDir). Duplicates are removed
// while preserving order.
func SetSnapshotCatalogRoots(roots ...string) {
	cleaned := make([]string, 0, len(roots))
	seen := map[string]struct{}{}
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		clean := filepath.Clean(r)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		cleaned = append(cleaned, clean)
	}
	snapshotCatalogMu.Lock()
	snapshotCatalogRoots = cleaned
	snapshotCatalogIndex = map[string]*SnapshotCatalogEntry{}
	snapshotCatalogMu.Unlock()
}

// AddSnapshotCatalogRoot adds a snapshot root if not already present.
func AddSnapshotCatalogRoot(root string) {
	root = strings.TrimSpace(root)
	if root == "" {
		return
	}
	clean := filepath.Clean(root)
	snapshotCatalogMu.Lock()
	defer snapshotCatalogMu.Unlock()
	for _, existing := range snapshotCatalogRoots {
		if existing == clean {
			return
		}
	}
	snapshotCatalogRoots = append(snapshotCatalogRoots, clean)
}

func snapshotCatalogRootsSnapshot() []string {
	snapshotCatalogMu.RLock()
	defer snapshotCatalogMu.RUnlock()
	out := make([]string, len(snapshotCatalogRoots))
	copy(out, snapshotCatalogRoots)
	return out
}

// WriteSnapshotCatalog persists entry under <SnapshotPath>/catalog.json.
// Existing files are overwritten.
func WriteSnapshotCatalog(entry *SnapshotCatalogEntry) error {
	if entry == nil {
		return errors.New("nil snapshot catalog entry")
	}
	if entry.SnapshotID == "" {
		return errors.New("snapshot_id is required")
	}
	if entry.SnapshotPath == "" {
		return errors.New("snapshot_path is required")
	}
	if entry.MetaDir == "" {
		entry.MetaDir = entry.SnapshotPath
	}
	if entry.CreatedAt == "" {
		entry.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(entry.SnapshotPath, 0o755); err != nil {
		return fmt.Errorf("ensure snapshot dir: %w", err)
	}
	path := filepath.Join(entry.SnapshotPath, snapshotCatalogFileName)
	tmp := path + ".tmp"
	body, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	snapshotCatalogMu.Lock()
	snapshotCatalogIndex[entry.SnapshotID] = cloneCatalogEntry(entry)
	snapshotCatalogMu.Unlock()
	return nil
}

// DeleteSnapshotCatalog removes the in-memory cache entry for snapshotID.
// The on-disk file is cleared by CleanupTemplateLocalData which removes the
// entire snapshot directory; calling this directly is only useful when
// deletion happens out-of-band.
func DeleteSnapshotCatalog(snapshotID string) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return
	}
	snapshotCatalogMu.Lock()
	delete(snapshotCatalogIndex, snapshotID)
	snapshotCatalogMu.Unlock()
}

// GetLocalSnapshot looks up the catalog for snapshotID. The cache is consulted
// first; on miss the on-disk roots are scanned. Returns ErrSnapshotCatalogNotFound
// if no record exists.
func GetLocalSnapshot(ctx context.Context, snapshotID string) (*SnapshotCatalogEntry, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, errors.New("snapshot_id is required")
	}
	snapshotCatalogMu.RLock()
	if cached, ok := snapshotCatalogIndex[snapshotID]; ok {
		out := cloneCatalogEntry(cached)
		snapshotCatalogMu.RUnlock()
		return out, nil
	}
	snapshotCatalogMu.RUnlock()
	entry, err := findSnapshotCatalogOnDisk(snapshotID)
	if err != nil {
		return nil, err
	}
	snapshotCatalogMu.Lock()
	snapshotCatalogIndex[snapshotID] = cloneCatalogEntry(entry)
	snapshotCatalogMu.Unlock()
	return entry, nil
}

// ListLocalSnapshots returns every catalog entry discoverable under the
// configured roots. The in-memory cache is refreshed as a side effect.
func ListLocalSnapshots(ctx context.Context) ([]*SnapshotCatalogEntry, error) {
	roots := snapshotCatalogRootsSnapshot()
	collected := map[string]*SnapshotCatalogEntry{}
	for _, root := range roots {
		entries, err := scanSnapshotCatalogsUnderRoot(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if _, ok := collected[e.SnapshotID]; ok {
				continue
			}
			collected[e.SnapshotID] = e
		}
	}
	snapshotCatalogMu.Lock()
	for id, e := range collected {
		snapshotCatalogIndex[id] = cloneCatalogEntry(e)
	}
	snapshotCatalogMu.Unlock()
	out := make([]*SnapshotCatalogEntry, 0, len(collected))
	for _, e := range collected {
		out = append(out, e)
	}
	return out, nil
}

// ResolveLocalSnapshot returns the catalog entry plus freshly re-resolved
// device paths (rootfs/memory) via cubecow. Useful for callers that immediately
// need to feed the dev paths into a cubecow operation.
func ResolveLocalSnapshot(ctx context.Context, snapshotID string) (*SnapshotCatalogEntryResolved, error) {
	entry, err := GetLocalSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	out := &SnapshotCatalogEntryResolved{SnapshotCatalogEntry: *entry}
	rootfsDev, err := ResolveCowDevPath(ctx, entry.RootfsVol, entry.RootfsKind)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs dev: %w", err)
	}
	memoryDev, err := ResolveCowDevPath(ctx, entry.MemoryVol, entry.MemoryKind)
	if err != nil {
		return nil, fmt.Errorf("resolve memory dev: %w", err)
	}
	out.RootfsDev = rootfsDev
	out.MemoryDev = memoryDev
	return out, nil
}

func findSnapshotCatalogOnDisk(snapshotID string) (*SnapshotCatalogEntry, error) {
	roots := snapshotCatalogRootsSnapshot()
	for _, root := range roots {
		// Layout: <root>/<instance_type>/<snapshotID>/<specDir>/catalog.json
		pattern := filepath.Join(root, "*", snapshotID, "*", snapshotCatalogFileName)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			entry, err := readSnapshotCatalogFile(m)
			if err != nil {
				continue
			}
			if entry.SnapshotID == snapshotID {
				return entry, nil
			}
		}
	}
	return nil, ErrSnapshotCatalogNotFound
}

func scanSnapshotCatalogsUnderRoot(root string) ([]*SnapshotCatalogEntry, error) {
	pattern := filepath.Join(root, "*", "*", "*", snapshotCatalogFileName)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	out := make([]*SnapshotCatalogEntry, 0, len(matches))
	for _, m := range matches {
		entry, err := readSnapshotCatalogFile(m)
		if err != nil {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func readSnapshotCatalogFile(path string) (*SnapshotCatalogEntry, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrSnapshotCatalogNotFound
		}
		return nil, err
	}
	entry := &SnapshotCatalogEntry{}
	if err := json.Unmarshal(body, entry); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if entry.SnapshotPath == "" {
		entry.SnapshotPath = filepath.Dir(path)
	}
	if entry.MetaDir == "" {
		entry.MetaDir = entry.SnapshotPath
	}
	return entry, nil
}

func cloneCatalogEntry(in *SnapshotCatalogEntry) *SnapshotCatalogEntry {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
