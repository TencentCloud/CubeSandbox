// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// A jfs template is built once, on one node, into that node's tree on the
// JuiceFS volume every node mounts. Any other node adopts it the first time
// it creates from it: the rootfs and memory become volumes of this node
// cloned from the builder's files, which copies metadata and no data, and
// the small package home is copied. From then on the template is an ordinary
// local one, so create, pause and CleanupTemplate need nothing jfs-specific,
// and the builder deleting its own copy does not affect this node.

var jfsTemplateLocks sync.Map

// AdoptJFSTemplate makes templateID usable on this node when another node
// built it. It reports whether it adopted anything: false means the template
// is already local, or no node has built it.
func AdoptJFSTemplate(ctx context.Context, templateID string) (bool, error) {
	id := strings.TrimSpace(templateID)
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return false, fmt.Errorf("invalid template id %q", templateID)
	}
	mu, _ := jfsTemplateLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	if _, err := GetLocalSnapshotFor(ctx, cow.BackendJFS, id); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrSnapshotCatalogNotFound) {
		return false, err
	}
	src, srcHome, err := findJFSTemplate(id)
	if err != nil || src == nil {
		return false, err
	}
	nodeDir := filepath.Dir(filepath.Dir(srcHome))
	rootfsFile, err := jfsObjectFile(nodeDir, src.RootfsVol, src.RootfsKind, src.BuildRootfsVol)
	if err != nil {
		return false, fmt.Errorf("template %s rootfs: %w", id, err)
	}
	memoryFile, err := jfsObjectFile(nodeDir, src.MemoryVol, src.MemoryKind, "")
	if err != nil {
		return false, fmt.Errorf("template %s memory: %w", id, err)
	}
	if _, err := importJFSVolume(ctx, src.RootfsVol, "rootfs", rootfsFile); err != nil {
		return false, err
	}
	if _, err := importJFSVolume(ctx, src.MemoryVol, "memory", memoryFile); err != nil {
		return false, err
	}
	home := SnapshotHome(cow.BackendJFS, SnapshotKindNormal, id)
	if err := copyPackageHome(srcHome, home); err != nil {
		return false, err
	}
	// The catalog goes last: its presence is what marks the adoption done.
	entry := cloneCatalogEntry(src)
	entry.SnapshotPath = home
	entry.MetaDir = filepath.Join(home, SnapshotMetadataDir)
	entry.RootfsKind = CowKindVolume
	entry.MemoryKind = CowKindVolume
	entry.BuildRootfsVol = ""
	entry.BuildRootfsKind = ""
	entry.CreatedAt = ""
	if err := WriteSnapshotCatalogFor(cow.BackendJFS, entry); err != nil {
		return false, fmt.Errorf("write catalog for adopted template %s: %w", id, err)
	}
	return true, nil
}

// findJFSTemplate returns the catalog and home of templateID as some other
// node built it, or nil when no node has.
func findJFSTemplate(id string) (*SnapshotCatalogEntry, string, error) {
	var cfg *Config
	if localStorage != nil {
		cfg = localStorage.config
	}
	own := cfg.jfsStorageRoot()
	pattern := filepath.Join(cfg.jfsNodesRoot(), "*", SnapshotKindNormal, id, SnapshotMetadataDir, snapshotCatalogFileName)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, "", err
	}
	for _, m := range matches {
		home := filepath.Dir(filepath.Dir(m))
		if filepath.Dir(filepath.Dir(home)) == own {
			continue
		}
		entry, err := readSnapshotCatalogFile(m)
		if err != nil || entry.SnapshotID != id || entry.Kind != CatalogKindTemplate {
			continue
		}
		return entry, home, nil
	}
	return nil, "", nil
}

// jfsObjectFile is where the reflink engine of the node rooted at nodeDir
// keeps an object: a volume at volumes/<name>/<name>, a snapshot next to
// the volume it was taken from.
func jfsObjectFile(nodeDir, name, kind, origin string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("no object recorded")
	}
	k, err := normalizeCowKind(kind)
	if err != nil {
		return "", err
	}
	dir := name
	if k == cowKindSnapshot {
		dir = strings.TrimSpace(origin)
		if dir == "" {
			return "", fmt.Errorf("snapshot %s has no recorded origin", name)
		}
	}
	return filepath.Join(nodeDir, SnapshotObjectsDir, "volumes", dir, name), nil
}
