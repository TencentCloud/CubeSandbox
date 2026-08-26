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

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// SnapshotCatalogEntry captures everything cubelet needs in order to roll back
// to a snapshot, restore-from-snapshot for a fresh sandbox, or clean up the
// underlying cubecow artifacts, without requiring master to carry physical
// cubecow/path references in its tables.
//
// Persisted as catalog.json under <home>/metadata/. XFS and S3 both use
// that layout; the CoW backend is selected by the request, never by a
// field in this file.
//
// Written by CommitSandbox (Kind=KindRuntimeSnapshot), AppSnapshot
// (Kind=KindTemplate), and Pause (Kind=pause_snapshot). Dev paths are
// re-resolved from cubecow on demand because they can change across
// activations. Unknown JSON fields are tolerated and missing fields
// decode as zero values so old catalog files keep working after schema
// extensions.
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
	// MetadataVol/Kind is the S3 package metadata volume (cloned from the
	// node-local base snapshot or a parent package metadata volume).
	// Empty on XFS (plain directory). The node-local base volume/snapshot
	// itself is never recorded here and is never exported.
	MetadataVol  string `json:"metadata_vol,omitempty"`
	MetadataKind string `json:"metadata_kind,omitempty"`
	// BuildRootfsVol/Kind track the temporary writable working layer created
	// during template build (AppSnapshot path). They must be cleaned up at
	// template delete time. Empty for runtime snapshots (CommitSandbox), which
	// never produce a build artifact.
	BuildRootfsVol  string `json:"build_rootfs_vol,omitempty"`
	BuildRootfsKind string `json:"build_rootfs_kind,omitempty"`
	RootfsSizeBytes uint64 `json:"rootfs_size_bytes,omitempty"`
	// ComponentVersions: inventory dir name → version string (no absolute paths).
	ComponentVersions map[string]string `json:"component_versions,omitempty"`
	// Kind distinguishes the producer/semantics of this catalog entry so
	// CleanupTemplate/GetLocalSnapshot consumers can branch where needed.
	// Empty == legacy entry (pre-v4) and should be treated as a runtime
	// snapshot for backward compatibility.
	Kind      string `json:"kind,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	// Backend is xfs|s3. Empty on legacy catalog.json means xfs.
	Backend string `json:"backend,omitempty"`
	// No remote_uuids here. Export ids are node-local and are only known
	// after the package is sealed, when this catalog lives on a read-only
	// metadata snapshot: writing them would land a second, divergent copy
	// on the host under the unmounted mount point. cubecow records the id
	// on the object itself (GetVolumeInfo.ExportUUID), which is the one
	// place that survives both sealing and a restart.
}

// ExtInfoRemoteUUIDs is the Update/Pause ext_info key carrying remote_uuids JSON.
const ExtInfoRemoteUUIDs = "remote_uuids"

// Catalog entry kinds. See SnapshotCatalogEntry.Kind.
const (
	// CatalogKindRuntimeSnapshot is produced by CommitSandbox (taking a
	// snapshot of a running sandbox).
	CatalogKindRuntimeSnapshot = "runtime_snapshot"
	// CatalogKindTemplate is produced by AppSnapshot (building a template
	// from an image / one-shot sandbox).
	CatalogKindTemplate = "template"
	// CatalogKindPauseSnapshot is produced by Pause when the MicroVM is
	// snapshotted into CubeCow and the shim exits. Resume recreates the
	// sandbox from this catalog entry with the same sandboxID.
	CatalogKindPauseSnapshot = "pause_snapshot"
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

type snapshotCatalogNS struct {
	roots []string
	index map[string]*SnapshotCatalogEntry
}

var (
	snapshotCatalogMu sync.RWMutex
	// snapshotCatalogs keeps XFS and S3 catalogs in separate namespaces.
	// Backend is chosen by the request (or which root was registered), never
	// by a field inside catalog.json.
	snapshotCatalogs = map[string]*snapshotCatalogNS{
		cow.BackendXFS: {index: map[string]*SnapshotCatalogEntry{}},
		cow.BackendS3:  {index: map[string]*SnapshotCatalogEntry{}},
	}
)

func catalogBackendKey(backend string) string {
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil {
		return cow.BackendXFS
	}
	return normalized
}

func catalogNSLocked(backend string) *snapshotCatalogNS {
	key := catalogBackendKey(backend)
	ns := snapshotCatalogs[key]
	if ns == nil {
		ns = &snapshotCatalogNS{index: map[string]*SnapshotCatalogEntry{}}
		snapshotCatalogs[key] = ns
	}
	if ns.index == nil {
		ns.index = map[string]*SnapshotCatalogEntry{}
	}
	return ns
}

func uniqueCleanRoots(roots []string) []string {
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
	return cleaned
}

// SetSnapshotCatalogRoots replaces XFS snapshot catalog roots.
func SetSnapshotCatalogRoots(roots ...string) {
	SetSnapshotCatalogRootsFor(cow.BackendXFS, roots...)
}

// SetSnapshotCatalogRootsFor replaces catalog roots for one CoW backend.
func SetSnapshotCatalogRootsFor(backend string, roots ...string) {
	cleaned := uniqueCleanRoots(roots)
	snapshotCatalogMu.Lock()
	defer snapshotCatalogMu.Unlock()
	ns := catalogNSLocked(backend)
	ns.roots = cleaned
	ns.index = map[string]*SnapshotCatalogEntry{}
}

// AddSnapshotCatalogRoot adds an XFS snapshot root if not already present.
func AddSnapshotCatalogRoot(root string) {
	AddSnapshotCatalogRootFor(cow.BackendXFS, root)
}

// AddSnapshotCatalogRootFor adds a snapshot root to one CoW backend.
func AddSnapshotCatalogRootFor(backend, root string) {
	root = strings.TrimSpace(root)
	if root == "" {
		return
	}
	clean := filepath.Clean(root)
	snapshotCatalogMu.Lock()
	defer snapshotCatalogMu.Unlock()
	ns := catalogNSLocked(backend)
	for _, existing := range ns.roots {
		if existing == clean {
			return
		}
	}
	ns.roots = append(ns.roots, clean)
}

func snapshotCatalogRootsSnapshot(backend string) []string {
	snapshotCatalogMu.RLock()
	defer snapshotCatalogMu.RUnlock()
	ns := snapshotCatalogs[catalogBackendKey(backend)]
	if ns == nil {
		return nil
	}
	out := make([]string, len(ns.roots))
	copy(out, ns.roots)
	return out
}

// WriteSnapshotCatalog persists entry under <SnapshotPath>/catalog.json in the
// XFS catalog namespace.
func WriteSnapshotCatalog(entry *SnapshotCatalogEntry) error {
	return WriteSnapshotCatalogFor(cow.BackendXFS, entry)
}

// WriteSnapshotCatalogFor persists catalog.json in the given backend namespace.
func WriteSnapshotCatalogFor(backend string, entry *SnapshotCatalogEntry) error {
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
		entry.MetaDir = filepath.Join(entry.SnapshotPath, SnapshotMetadataDir)
	}
	catalogDir := entry.MetaDir
	if entry.CreatedAt == "" {
		entry.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(entry.Backend) == "" {
		entry.Backend = catalogBackendKey(backend)
	} else if normalized, err := cow.NormalizeBackend(entry.Backend); err == nil {
		entry.Backend = normalized
	}
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		return fmt.Errorf("ensure snapshot dir: %w", err)
	}
	path := filepath.Join(catalogDir, snapshotCatalogFileName)
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
	catalogNSLocked(backend).index[entry.SnapshotID] = cloneCatalogEntry(entry)
	snapshotCatalogMu.Unlock()
	return nil
}

// DeleteSnapshotCatalog removes the in-memory cache entry for snapshotID from
// every backend namespace.
func DeleteSnapshotCatalog(snapshotID string) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return
	}
	snapshotCatalogMu.Lock()
	for _, ns := range snapshotCatalogs {
		if ns != nil && ns.index != nil {
			delete(ns.index, snapshotID)
		}
	}
	snapshotCatalogMu.Unlock()
}

// DeleteSnapshotCatalogFor removes the in-memory cache entry from one backend.
func DeleteSnapshotCatalogFor(backend, snapshotID string) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return
	}
	snapshotCatalogMu.Lock()
	delete(catalogNSLocked(backend).index, snapshotID)
	snapshotCatalogMu.Unlock()
}

// GetLocalSnapshot looks up the catalog for snapshotID in the XFS namespace.
func GetLocalSnapshot(ctx context.Context, snapshotID string) (*SnapshotCatalogEntry, error) {
	return GetLocalSnapshotFor(ctx, cow.BackendXFS, snapshotID)
}

// GetLocalSnapshotFor looks up catalog.json in one CoW backend namespace.
func GetLocalSnapshotFor(ctx context.Context, backend, snapshotID string) (*SnapshotCatalogEntry, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, errors.New("snapshot_id is required")
	}
	key := catalogBackendKey(backend)
	snapshotCatalogMu.RLock()
	if ns := snapshotCatalogs[key]; ns != nil && ns.index != nil {
		if cached, ok := ns.index[snapshotID]; ok {
			out := cloneCatalogEntry(cached)
			snapshotCatalogMu.RUnlock()
			return out, nil
		}
	}
	snapshotCatalogMu.RUnlock()
	entry, err := findSnapshotCatalogOnDisk(key, snapshotID)
	if err != nil {
		if !errors.Is(err, ErrSnapshotCatalogNotFound) || !isS3CatalogBackend(key) {
			return nil, err
		}
		// A sealed S3 package keeps catalog.json on its unmounted metadata
		// disk, so the miss says nothing about whether the package is here.
		// Rebuild what the object names prove and do not cache it: the real
		// catalog carries spec_dir／component_versions and must win once it
		// is readable again.
		return deriveS3CatalogEntry(ctx, snapshotID)
	}
	snapshotCatalogMu.Lock()
	catalogNSLocked(key).index[snapshotID] = cloneCatalogEntry(entry)
	snapshotCatalogMu.Unlock()
	return entry, nil
}

// ListLocalSnapshots returns every XFS catalog entry.
func ListLocalSnapshots(ctx context.Context) ([]*SnapshotCatalogEntry, error) {
	return ListLocalSnapshotsFor(ctx, cow.BackendXFS)
}

// ListLocalSnapshotsFor returns catalog entries under one backend's roots.
func ListLocalSnapshotsFor(ctx context.Context, backend string) ([]*SnapshotCatalogEntry, error) {
	key := catalogBackendKey(backend)
	roots := snapshotCatalogRootsSnapshot(key)
	collected := map[string]*SnapshotCatalogEntry{}
	for _, root := range roots {
		entries, err := scanSnapshotCatalogsUnderRoot(key, root)
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
	ns := catalogNSLocked(key)
	for id, e := range collected {
		ns.index[id] = cloneCatalogEntry(e)
	}
	snapshotCatalogMu.Unlock()
	out := make([]*SnapshotCatalogEntry, 0, len(collected))
	for _, e := range collected {
		out = append(out, e)
	}
	return out, nil
}

// ResolveLocalSnapshot returns the XFS catalog entry plus re-resolved device paths.
func ResolveLocalSnapshot(ctx context.Context, snapshotID string) (*SnapshotCatalogEntryResolved, error) {
	return ResolveLocalSnapshotFor(ctx, cow.BackendXFS, snapshotID)
}

// ResolveLocalSnapshotFor resolves device paths on the Store for backend.
func ResolveLocalSnapshotFor(ctx context.Context, backend, snapshotID string) (*SnapshotCatalogEntryResolved, error) {
	entry, err := GetLocalSnapshotFor(ctx, backend, snapshotID)
	if err != nil {
		return nil, err
	}
	out := &SnapshotCatalogEntryResolved{SnapshotCatalogEntry: *entry}
	rootfsDev, err := ResolveObjectPathFor(ctx, backend, entry.RootfsVol, entry.RootfsKind)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs dev: %w", err)
	}
	memoryDev, err := ResolveObjectPathFor(ctx, backend, entry.MemoryVol, entry.MemoryKind)
	if err != nil {
		return nil, fmt.Errorf("resolve memory dev: %w", err)
	}
	out.RootfsDev = rootfsDev
	out.MemoryDev = memoryDev
	return out, nil
}

func catalogFilePatterns(backend, root, snapshotID string) []string {
	root = filepath.Clean(root)
	id := "*"
	if snapshotID != "" {
		id = snapshotID
	}
	patterns := []string{filepath.Join(root, id, SnapshotMetadataDir, snapshotCatalogFileName)}
	if !isS3CatalogBackend(backend) {
		// Legacy XFS: <root>/cubebox/<id>/<specDir>/catalog.json
		if snapshotID != "" {
			patterns = append(patterns, filepath.Join(root, "*", snapshotID, "*", snapshotCatalogFileName))
		} else {
			patterns = append(patterns, filepath.Join(root, "*", "*", "*", snapshotCatalogFileName))
		}
	}
	return patterns
}

func findSnapshotCatalogOnDisk(backend, snapshotID string) (*SnapshotCatalogEntry, error) {
	roots := snapshotCatalogRootsSnapshot(backend)
	for _, root := range roots {
		for _, pattern := range catalogFilePatterns(backend, root, snapshotID) {
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
	}
	return nil, ErrSnapshotCatalogNotFound
}

func scanSnapshotCatalogsUnderRoot(backend, root string) ([]*SnapshotCatalogEntry, error) {
	out := make([]*SnapshotCatalogEntry, 0)
	for _, pattern := range catalogFilePatterns(backend, root, "") {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			entry, err := readSnapshotCatalogFile(m)
			if err != nil {
				continue
			}
			out = append(out, entry)
		}
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
	if entry.SnapshotPath == "" || entry.MetaDir == "" {
		catalogDir := filepath.Dir(path)
		if filepath.Base(catalogDir) == S3SnapshotMetadataDir {
			home := filepath.Dir(catalogDir)
			if entry.SnapshotPath == "" {
				entry.SnapshotPath = home
			}
			if entry.MetaDir == "" {
				entry.MetaDir = catalogDir
			}
		} else {
			if entry.SnapshotPath == "" {
				entry.SnapshotPath = catalogDir
			}
			if entry.MetaDir == "" {
				entry.MetaDir = entry.SnapshotPath
			}
		}
	}
	return entry, nil
}

// ReadSnapshotCatalogAt loads catalog.json under snapshotPath when present.
// S3 packages store it in metadata/; this also looks there when the home
// directory is passed.
func ReadSnapshotCatalogAt(snapshotPath string) (*SnapshotCatalogEntry, error) {
	snapshotPath = strings.TrimSpace(snapshotPath)
	if snapshotPath == "" {
		return nil, ErrSnapshotCatalogNotFound
	}
	clean := filepath.Clean(snapshotPath)
	entry, err := readSnapshotCatalogFile(filepath.Join(clean, snapshotCatalogFileName))
	if err == nil {
		return entry, nil
	}
	return readSnapshotCatalogFile(filepath.Join(clean, S3SnapshotMetadataDir, snapshotCatalogFileName))
}

func cloneCatalogEntry(in *SnapshotCatalogEntry) *SnapshotCatalogEntry {
	if in == nil {
		return nil
	}
	out := *in
	if in.ComponentVersions != nil {
		out.ComponentVersions = make(map[string]string, len(in.ComponentVersions))
		for k, v := range in.ComponentVersions {
			out.ComponentVersions[k] = v
		}
	}
	return &out
}
