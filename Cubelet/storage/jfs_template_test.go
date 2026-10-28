// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// writeBuiltJFSTemplate lays out templateID as node-a's AppSnapshot leaves it.
func writeBuiltJFSTemplate(t *testing.T, nodes, templateID string) string {
	t.Helper()
	home := filepath.Join(nodes, "node-a", SnapshotKindNormal, templateID)
	meta := filepath.Join(home, SnapshotMetadataDir)
	require.NoError(t, os.MkdirAll(filepath.Join(meta, "snapshot"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(meta, "snapshot", "config.json"), []byte("cfg"), 0o644))
	require.NoError(t, os.Symlink(SnapshotMetadataDir, filepath.Join(home, "2C2000M")))
	require.NoError(t, WriteSnapshotCatalogFor(cow.BackendJFS, &SnapshotCatalogEntry{
		SnapshotID:        templateID,
		SpecDir:           "2C2000M",
		SnapshotPath:      home,
		RootfsVol:         "tpl-" + templateID + "-rootfs",
		RootfsKind:        cowKindSnapshot,
		MemoryVol:         "tpl-" + templateID + "-memory",
		MemoryKind:        cowKindVolume,
		BuildRootfsVol:    "tpl-" + templateID + "-build-rootfs",
		BuildRootfsKind:   cowKindVolume,
		ComponentVersions: map[string]string{"cube-shim": "v1"},
		Kind:              CatalogKindTemplate,
	}))
	objects := filepath.Join(nodes, "node-a", SnapshotObjectsDir, "volumes")
	rootfs := filepath.Join(objects, "tpl-"+templateID+"-build-rootfs", "tpl-"+templateID+"-rootfs")
	memory := filepath.Join(objects, "tpl-"+templateID+"-memory", "tpl-"+templateID+"-memory")
	require.NoError(t, os.MkdirAll(filepath.Dir(rootfs), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(memory), 0o755))
	require.NoError(t, os.WriteFile(rootfs, nil, 0o644))
	require.NoError(t, os.WriteFile(memory, nil, 0o644))
	// Writing node-a's catalog cached it as if it were this node's.
	DeleteSnapshotCatalogFor(cow.BackendJFS, templateID)
	t.Cleanup(func() { DeleteSnapshotCatalogFor(cow.BackendJFS, templateID) })
	return home
}

func TestAdoptJFSTemplateClonesTheBuilderPackage(t *testing.T) {
	engine := &fakeCowEngine{}
	nodes, _ := useTestJFSStorage(t, engine)
	const id = "tpl-x"
	writeBuiltJFSTemplate(t, nodes, id)

	adopted, err := AdoptJFSTemplate(context.Background(), id)
	require.NoError(t, err)
	require.True(t, adopted)

	objects := filepath.Join(nodes, "node-a", SnapshotObjectsDir, "volumes")
	require.Equal(t, [][2]string{
		{"tpl-tpl-x-rootfs", filepath.Join(objects, "tpl-tpl-x-build-rootfs", "tpl-tpl-x-rootfs")},
		{"tpl-tpl-x-memory", filepath.Join(objects, "tpl-tpl-x-memory", "tpl-tpl-x-memory")},
	}, engine.importedLvols)

	entry, err := GetLocalSnapshotFor(context.Background(), cow.BackendJFS, id)
	require.NoError(t, err)
	home := filepath.Join(nodes, "node-b", SnapshotKindNormal, id)
	require.Equal(t, home, entry.SnapshotPath)
	require.Equal(t, CowKindVolume, entry.RootfsKind)
	require.Equal(t, CowKindVolume, entry.MemoryKind)
	require.Empty(t, entry.BuildRootfsVol)
	require.Equal(t, "v1", entry.ComponentVersions["cube-shim"])
	data, err := os.ReadFile(filepath.Join(home, SnapshotMetadataDir, "snapshot", "config.json"))
	require.NoError(t, err)
	require.Equal(t, "cfg", string(data))
	require.FileExists(t, filepath.Join(home, SnapshotMetadataDir, snapshotCatalogFileName))

	adopted, err = AdoptJFSTemplate(context.Background(), id)
	require.NoError(t, err)
	require.False(t, adopted)
	require.Len(t, engine.importedLvols, 2)
}

func TestAdoptJFSTemplateIgnoresUnbuiltAndNonTemplatePackages(t *testing.T) {
	engine := &fakeCowEngine{}
	nodes, _ := useTestJFSStorage(t, engine)

	adopted, err := AdoptJFSTemplate(context.Background(), "tpl-none")
	require.NoError(t, err)
	require.False(t, adopted)

	home := writeBuiltJFSTemplate(t, nodes, "snap-y")
	entry, err := readSnapshotCatalogFile(filepath.Join(home, SnapshotMetadataDir, snapshotCatalogFileName))
	require.NoError(t, err)
	entry.Kind = CatalogKindRuntimeSnapshot
	require.NoError(t, WriteSnapshotCatalogFor(cow.BackendJFS, entry))
	DeleteSnapshotCatalogFor(cow.BackendJFS, "snap-y")
	adopted, err = AdoptJFSTemplate(context.Background(), "snap-y")
	require.NoError(t, err)
	require.False(t, adopted)
	require.Empty(t, engine.importedLvols)

	_, err = AdoptJFSTemplate(context.Background(), "../x")
	require.Error(t, err)
}

func TestFindJFSGlobalRuntimeSnapshotResolvesForeignObjects(t *testing.T) {
	nodes, _ := useTestJFSStorage(t, &fakeCowEngine{})
	const id = "snap-global"
	staleMeta := filepath.Join(nodes, "node-0", SnapshotKindNormal, id, SnapshotMetadataDir)
	require.NoError(t, os.MkdirAll(staleMeta, 0o755))
	staleData, err := json.Marshal(&SnapshotCatalogEntry{
		SnapshotID: id, RootfsVol: "missing-rootfs", RootfsKind: cowKindSnapshot,
		MemoryVol: "missing-memory", MemoryKind: cowKindSnapshot,
		Kind: CatalogKindRuntimeSnapshot, Backend: cow.BackendJFS,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staleMeta, snapshotCatalogFileName), staleData, 0o644))

	node := filepath.Join(nodes, "node-a")
	home := filepath.Join(node, SnapshotKindNormal, id)
	meta := filepath.Join(home, SnapshotMetadataDir)
	require.NoError(t, os.MkdirAll(meta, 0o755))
	rootfs := filepath.Join(node, SnapshotObjectsDir, "volumes", "live-rootfs", "snap-rootfs")
	memory := filepath.Join(node, SnapshotObjectsDir, "volumes", "live-memory", "snap-memory")
	require.NoError(t, os.MkdirAll(filepath.Dir(rootfs), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(memory), 0o755))
	require.NoError(t, os.WriteFile(rootfs, nil, 0o644))
	require.NoError(t, os.WriteFile(memory, nil, 0o644))

	entry := &SnapshotCatalogEntry{
		SnapshotID:   id,
		SnapshotPath: home,
		MetaDir:      meta,
		RootfsVol:    "snap-rootfs",
		RootfsKind:   cowKindSnapshot,
		MemoryVol:    "snap-memory",
		MemoryKind:   cowKindSnapshot,
		Kind:         CatalogKindRuntimeSnapshot,
		Backend:      cow.BackendJFS,
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(meta, snapshotCatalogFileName), data, 0o644))

	global, err := FindJFSGlobalRuntimeSnapshot(id)
	require.NoError(t, err)
	require.Equal(t, home, global.Home)
	require.Equal(t, rootfs, global.RootfsPath)
	require.Equal(t, memory, global.MemoryPath)
	require.Equal(t, id, global.Entry.SnapshotID)
}
