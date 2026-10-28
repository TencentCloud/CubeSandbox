// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// useTestJFSStorage mounts a fake jfs store whose shared volume is a temp
// dir, as seen by node-b. It returns the nodes root holding every node tree.
func useTestJFSStorage(t *testing.T, engine *fakeCowEngine) (nodes, cache string) {
	t.Helper()
	mount := t.TempDir()
	cache = t.TempDir()
	cfg := &Config{StorageBackend: "cubecow", RootPath: t.TempDir()}
	cfg.Cow.JFS.Enable = true
	cfg.Cow.JFS.MountPath = mount
	cfg.Cow.JFS.NodeName = "node-b"
	cfg.Cow.JFS.CacheDir = cache
	require.NoError(t, os.MkdirAll(cfg.jfsNodesRoot(), 0o755))
	previous := localStorage
	localStorage = &local{config: cfg, cowManager: &XfsCow{engine: engine}, jfsCowManager: &XfsCow{engine: engine}}
	t.Cleanup(func() { localStorage = previous })
	return cfg.jfsNodesRoot(), cache
}

func jfsCrossNodeAnnotations(nodes string) map[string]string {
	origin := filepath.Join(nodes, "node-a")
	return map[string]string{
		constants.MasterAnnotationSnapshotCrossNode: "true",
		constants.MasterAnnotationStorageBackend:    cow.BackendJFS,
		constants.MasterAnnotationDesiredSandboxID:  "sb-1",
		constants.MasterAnnotationSnapshotRemoteUUIDs: (&cow.RemoteUUIDs{
			Rootfs:   filepath.Join(origin, "snapshots", "tpl-snap-x-rootfs"),
			Memory:   filepath.Join(origin, "objects", "tpl-snap-x-memory"),
			Metadata: filepath.Join(origin, "pause-snapshots", "snap-x"),
		}).JSON(),
	}
}

func TestJFSSandboxImportClonesOriginFilesUnderSandboxNames(t *testing.T) {
	engine := &fakeCowEngine{}
	nodes, _ := useTestJFSStorage(t, engine)
	ann := jfsCrossNodeAnnotations(nodes)
	uuids := cow.ParseRemoteUUIDs(ann[constants.MasterAnnotationSnapshotRemoteUUIDs])
	require.NoError(t, os.MkdirAll(filepath.Dir(uuids.Rootfs), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(uuids.Memory), 0o755))
	require.NoError(t, os.WriteFile(uuids.Rootfs, nil, 0o644))
	require.NoError(t, os.WriteFile(uuids.Memory, nil, 0o644))
	imp := CrossNodeSandboxImport(ann)
	require.NotNil(t, imp)
	require.Equal(t, cow.BackendJFS, imp.Backend)

	rootfs, _, err := imp.Rootfs(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, "sb-sb-1-rootfs-gen0", rootfs)
	memory, _, err := imp.Memory(context.Background())
	require.NoError(t, err)
	require.Equal(t, "sb-sb-1-memory", memory)

	origin := filepath.Join(nodes, "node-a")
	require.Equal(t, [][2]string{
		{"sb-sb-1-rootfs-gen0", filepath.Join(origin, "snapshots", "tpl-snap-x-rootfs")},
		{"sb-sb-1-memory", filepath.Join(origin, "objects", "tpl-snap-x-memory")},
	}, engine.importedLvols)
}

// The paths come from Master; one outside the shared nodes tree must never
// be cloned into a sandbox disk.
func TestJFSSandboxImportRejectsPathsOutsideTheNodesTree(t *testing.T) {
	engine := &fakeCowEngine{}
	nodes, _ := useTestJFSStorage(t, engine)
	for _, src := range []string{"/etc/shadow", nodes, filepath.Join(nodes, "..", "x"), "relative/path"} {
		ann := jfsCrossNodeAnnotations(nodes)
		ann[constants.MasterAnnotationSnapshotRemoteUUIDs] = (&cow.RemoteUUIDs{Rootfs: src, Metadata: src}).JSON()
		imp := CrossNodeSandboxImport(ann)
		require.NotNil(t, imp)
		_, _, err := imp.Rootfs(context.Background(), 0)
		require.ErrorContains(t, err, "outside", src)
		_, err = imp.EnsureMetadata(context.Background())
		require.ErrorContains(t, err, "outside", src)
	}
	require.Empty(t, engine.importedLvols)

	escape := filepath.Join(nodes, "node-a", "escape")
	require.NoError(t, os.MkdirAll(filepath.Dir(escape), 0o755))
	require.NoError(t, os.Symlink("/etc/passwd", escape))
	ann := jfsCrossNodeAnnotations(nodes)
	ann[constants.MasterAnnotationSnapshotRemoteUUIDs] = (&cow.RemoteUUIDs{Rootfs: escape, Metadata: escape}).JSON()
	imp := CrossNodeSandboxImport(ann)
	require.NotNil(t, imp)
	_, _, err := imp.Rootfs(context.Background(), 0)
	require.ErrorContains(t, err, "outside")
}

// The origin deletes its package after the restore, so the sandbox gets its
// own copy of the home; the catalog stays behind since the copy is not a
// package of this node.
func TestJFSSandboxImportCopiesThePackageHome(t *testing.T) {
	nodes, _ := useTestJFSStorage(t, &fakeCowEngine{})
	src := filepath.Join(nodes, "node-a", "pause-snapshots", "snap-x")
	meta := filepath.Join(src, SnapshotMetadataDir)
	require.NoError(t, os.MkdirAll(filepath.Join(meta, "snapshot"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(meta, "snapshot", "config.json"), []byte("cfg"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(meta, snapshotCatalogFileName), []byte("{}"), 0o644))
	require.NoError(t, os.Symlink(SnapshotMetadataDir, filepath.Join(src, "2C2000M")))

	imp := CrossNodeSandboxImport(jfsCrossNodeAnnotations(nodes))
	require.NotNil(t, imp)
	dir, err := imp.EnsureMetadata(context.Background())
	require.NoError(t, err)
	require.Equal(t, SnapshotMetaDir(cow.BackendJFS, SnapshotKindNormal, "sb-1"), dir)
	require.Equal(t, filepath.Join(nodes, "node-b", "snapshots", "sb-1", "metadata"), dir)

	data, err := os.ReadFile(filepath.Join(dir, "snapshot", "config.json"))
	require.NoError(t, err)
	require.Equal(t, "cfg", string(data))
	require.NoFileExists(t, filepath.Join(dir, snapshotCatalogFileName))
	link, err := os.Readlink(filepath.Join(filepath.Dir(dir), "2C2000M"))
	require.NoError(t, err)
	require.Equal(t, SnapshotMetadataDir, link)

	// The create workflow asks again; the copy is reused.
	require.NoError(t, os.RemoveAll(src))
	again, err := imp.EnsureMetadata(context.Background())
	require.NoError(t, err)
	require.Equal(t, dir, again)
}

func TestJFSRuntimeSnapshotUsesSharedMetadataAndMemory(t *testing.T) {
	engine := &fakeCowEngine{}
	nodes, _ := useTestJFSStorage(t, engine)
	ann := jfsCrossNodeAnnotations(nodes)
	ann[constants.MasterAnnotationRuntimeSnapshotID] = "snap-x"

	origin := filepath.Join(nodes, "node-a")
	home := filepath.Join(origin, SnapshotKindNormal, "snap-x")
	meta := filepath.Join(home, SnapshotMetadataDir)
	require.NoError(t, os.MkdirAll(meta, 0o755))
	objects := filepath.Join(origin, SnapshotObjectsDir, "volumes", "base")
	rootfs := filepath.Join(objects, "snap-x-rootfs")
	memory := filepath.Join(objects, "snap-x-memory")
	require.NoError(t, os.MkdirAll(filepath.Dir(memory), 0o755))
	require.NoError(t, os.WriteFile(rootfs, []byte("rootfs"), 0o644))
	require.NoError(t, os.WriteFile(memory, []byte("memory"), 0o644))
	ann[constants.MasterAnnotationSnapshotRemoteUUIDs] = (&cow.RemoteUUIDs{
		Rootfs: rootfs, Memory: memory, Metadata: home,
	}).JSON()

	imp := CrossNodeSandboxImport(ann)
	require.NotNil(t, imp)
	require.True(t, imp.SharedJFSRuntimeSnapshot)

	ctx, err := WithRegisteredSharedJFSRuntimeSnapshot(context.Background(), imp)
	require.NoError(t, err)

	gotMeta, err := imp.EnsureMetadata(ctx)
	require.NoError(t, err)
	require.Equal(t, meta, gotMeta)
	gotMemory, err := imp.SharedJFSMemoryPath(ctx)
	require.NoError(t, err)
	require.Equal(t, memory, gotMemory)
	require.NoDirExists(t, SnapshotHome(cow.BackendJFS, SnapshotKindNormal, "sb-1"))

	// The writable rootfs remains private to the new sandbox.
	gotRootfs, _, err := imp.Rootfs(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, "sb-sb-1-rootfs-gen0", gotRootfs)
	require.Equal(t, [][2]string{{
		"sb-sb-1-rootfs-gen0",
		rootfs,
	}}, engine.importedLvols)
}

func TestJFSRuntimeSnapshotUsesRegisteredPathsWithoutCatalog(t *testing.T) {
	nodes, _ := useTestJFSStorage(t, &fakeCowEngine{})
	ann := jfsCrossNodeAnnotations(nodes)
	ann[constants.MasterAnnotationRuntimeSnapshotID] = "snap-x"

	origin := filepath.Join(nodes, "node-a")
	home := filepath.Join(origin, SnapshotKindNormal, "snap-x")
	objects := filepath.Join(origin, SnapshotObjectsDir, "volumes", "base")
	rootfs := filepath.Join(objects, "snap-x-rootfs")
	memory := filepath.Join(objects, "snap-x-memory")
	ann[constants.MasterAnnotationSnapshotRemoteUUIDs] = (&cow.RemoteUUIDs{
		Rootfs: rootfs, Memory: memory, Metadata: home,
	}).JSON()

	imp := CrossNodeSandboxImport(ann)
	require.NotNil(t, imp)
	ctx, err := WithRegisteredSharedJFSRuntimeSnapshot(context.Background(), imp)
	require.NoError(t, err)
	gotHome, ok := RegisteredJFSRuntimeSnapshotHome(ctx, "snap-x")
	require.True(t, ok)
	require.Equal(t, home, gotHome)
	gotMemory, err := imp.SharedJFSMemoryPath(ctx)
	require.NoError(t, err)
	require.Equal(t, memory, gotMemory)
}

func TestJFSPauseSnapshotStillImportsPrivateMetadataAndMemory(t *testing.T) {
	nodes, _ := useTestJFSStorage(t, &fakeCowEngine{})
	ann := jfsCrossNodeAnnotations(nodes)
	ann[constants.MasterAnnotationRuntimeSnapshotID] = "runtime-parent"
	ann[constants.MasterAnnotationPauseSnapshotID] = "pause-x"

	imp := CrossNodeSandboxImport(ann)
	require.NotNil(t, imp)
	require.False(t, imp.SharedJFSRuntimeSnapshot)
}

func TestCommitMemoryFromSharedJFSBaseCreatesLocalWritableDestination(t *testing.T) {
	engine := &fakeCowEngine{}
	nodes, _ := useTestJFSStorage(t, engine)
	source := filepath.Join(nodes, "node-a", SnapshotObjectsDir, "volumes", "base", "snap-memory")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, nil, 0o644))

	obj, err := CommitMemoryFromBaseFor(context.Background(), cow.BackendJFS, &CowSnapshotObject{
		Name:         "snap-memory",
		Kind:         cowKindSnapshot,
		DevPath:      source,
		SharedSource: true,
	}, "next", 1<<30)
	require.NoError(t, err)
	require.Equal(t, "tpl-next-memory", obj.Name)
	require.Equal(t, cowKindVolume, obj.Kind)
	require.Equal(t, uint64(1<<30), obj.SizeBytes)
	require.Equal(t, [][2]string{{"tpl-next-memory", source}}, engine.importedLvols)
}

func TestJFSStagedBlocksCountsOnlyStagedObjects(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	staged := filepath.Join(cache, "uuid-1", jfsStagingDir, "chunks", "0", "385", "385229_1_4194304")
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
	require.NoError(t, os.WriteFile(staged, nil, 0o644))

	previous := jfsInfoOutput
	jfsInfoOutput = func(context.Context, string) ([]byte, error) {
		return []byte(" objects:\n" +
			"| 0 | cube-jfs/chunks/0/385/385229_0_4194304 | 4194304 | 0 | 4194304 |\n" +
			"| 0 | cube-jfs/chunks/0/385/385229_1_4194304 | 4194304 | 0 | 4194304 |\n"), nil
	}
	t.Cleanup(func() { jfsInfoOutput = previous })

	n, err := jfsStagedBlocks(context.Background(), "/f")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.NoError(t, os.Remove(staged))
	n, err = jfsStagedBlocks(context.Background(), "/f")
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestJFSPackageUploadIncludesRestoreMetadata(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	home := t.TempDir()
	meta := filepath.Join(home, SnapshotMetadataDir)
	names := []string{"catalog.json", "metadata.json", "snapshot/config.json", "snapshot/state.json", "sandbox_spec.json"}
	for _, name := range names {
		p := filepath.Join(meta, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(`{"kind":"pause_snapshot"}`), 0o644))
	}
	files := &cow.RemoteUUIDs{Rootfs: "/rootfs", Memory: "/memory", Metadata: home}
	paths, err := jfsPackageUploadPaths(files)
	require.NoError(t, err)
	require.Len(t, paths, 7)
	staged := filepath.Join(cache, "uuid", jfsStagingDir, "chunks/0/0/1_0_1")
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
	previous := jfsInfoOutput
	t.Cleanup(func() { jfsInfoOutput = previous })
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(meta, name)
			jfsInfoOutput = func(_ context.Context, p string) ([]byte, error) {
				if p == target {
					return []byte("chunks/0/0/1_0_1"), nil
				}
				return nil, nil
			}
			require.NoError(t, os.WriteFile(staged, nil, 0o644))
			n, err := jfsStagedBlocks(context.Background(), paths...)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.NoError(t, os.Remove(staged))
			n, err = jfsStagedBlocks(context.Background(), paths...)
			require.NoError(t, err)
			require.Zero(t, n)
			require.NoError(t, os.Remove(target))
			_, err = jfsPackageUploadPaths(files)
			require.Error(t, err)
			require.NoError(t, os.WriteFile(target, []byte(`{"kind":"pause_snapshot"}`), 0o644))
		})
	}
	// Runtime snapshots do not require the pause-only request spec.
	require.NoError(t, os.Remove(filepath.Join(meta, "sandbox_spec.json")))
	require.NoError(t, os.WriteFile(filepath.Join(meta, "catalog.json"), []byte(`{"kind":"runtime_snapshot"}`), 0o644))
	_, err = jfsPackageUploadPaths(files)
	require.NoError(t, err)
}

func TestJFSStagedBlocksInspectionFailures(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	_, err := jfsStagedBlocks(context.Background(), "/f")
	require.Error(t, err) // Empty/misconfigured cache cannot establish readiness.
	root := filepath.Join(cache, "uuid", jfsStagingDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(root), 0o755))
	previous := jfsInfoOutput
	t.Cleanup(func() { jfsInfoOutput = previous })
	jfsInfoOutput = func(context.Context, string) ([]byte, error) { return []byte("chunks/0/0/1_0_1"), nil }
	n, err := jfsStagedBlocks(context.Background(), "/f")
	require.NoError(t, err)
	require.Zero(t, n) // Lazily-created staging is valid.
	require.NoError(t, os.MkdirAll(root, 0o755))
	// ENOTDIR must not be treated as an absent block.
	require.NoError(t, os.WriteFile(filepath.Join(root, "chunks"), nil, 0o644))
	_, err = jfsStagedBlocks(context.Background(), "/f")
	require.Error(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "chunks")))
	jfsInfoOutput = func(context.Context, string) ([]byte, error) { return nil, os.ErrPermission }
	_, err = jfsStagedBlocks(context.Background(), "/f")
	require.Error(t, err)
	require.NoError(t, os.RemoveAll(cache))
	_, err = jfsStagedBlocks(context.Background(), "/f")
	require.Error(t, err)
}

func TestJFSUploadTrackerCachesObjectKeysUntilStagingDrains(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	root := filepath.Join(cache, "uuid", jfsStagingDir)
	key := "chunks/0/0/1_0_1"
	staged := filepath.Join(root, key)
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
	require.NoError(t, os.WriteFile(staged, nil, 0o644))
	// Keep the global-zero shortcut disabled so this test exercises the
	// package-specific tracker.
	require.NoError(t, os.WriteFile(filepath.Join(localStorage.config.jfsMountPath(), ".stats"), []byte("juicefs_staging_blocks 1\n"), 0o644))

	previous := jfsInfoOutput
	calls := 0
	jfsInfoOutput = func(context.Context, string) ([]byte, error) {
		calls++
		return []byte(key), nil
	}
	t.Cleanup(func() { jfsInfoOutput = previous })

	tracker := &jfsUploadTracker{
		paths:          []string{"/rootfs", "/memory", "/metadata"},
		roots:          []string{root},
		globalBlocks:   1,
		globalSince:    time.Now().Add(-jfsTrackerGlobalMaxWait),
		globalProgress: time.Now().Add(-jfsTrackerGlobalIdleWait),
	}
	stagedCount, ready, err := tracker.check(context.Background())
	require.NoError(t, err)
	require.False(t, ready)
	require.Equal(t, 1, stagedCount)
	require.Equal(t, 3, calls)

	// A status request inside the backoff window is an O(1) cached read.
	stagedCount, ready, err = tracker.check(context.Background())
	require.NoError(t, err)
	require.False(t, ready)
	require.Equal(t, 1, stagedCount)
	require.Equal(t, 3, calls)

	require.NoError(t, os.Remove(staged))
	tracker.mu.Lock()
	tracker.nextCheck = time.Time{}
	tracker.mu.Unlock()
	stagedCount, ready, err = tracker.check(context.Background())
	require.NoError(t, err)
	require.True(t, ready)
	require.Zero(t, stagedCount)
	// One final discovery guards against a late object-key change.
	require.Equal(t, 6, calls)
}

func TestJFSUploadTrackerUsesGlobalZeroFastPath(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	root := filepath.Join(cache, "uuid", jfsStagingDir)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localStorage.config.jfsMountPath(), ".stats"), []byte("juicefs_staging_blocks 0\n"), 0o644))

	previous := jfsInfoOutput
	jfsInfoOutput = func(context.Context, string) ([]byte, error) {
		t.Fatal("global-zero fast path must not invoke juicefs info")
		return nil, nil
	}
	t.Cleanup(func() { jfsInfoOutput = previous })

	tracker := &jfsUploadTracker{paths: []string{"/rootfs", "/memory"}, roots: []string{root}}
	staged, ready, err := tracker.check(context.Background())
	require.NoError(t, err)
	require.True(t, ready)
	require.Zero(t, staged)
}

func TestJFSUploadTrackerWaitsForGlobalStagingBeforeMetadataLookup(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	root := filepath.Join(cache, "uuid", jfsStagingDir)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localStorage.config.jfsMountPath(), ".stats"), []byte("juicefs_staging_blocks 7\n"), 0o644))

	previous := jfsInfoOutput
	jfsInfoOutput = func(context.Context, string) ([]byte, error) {
		t.Fatal("global staging grace period must not invoke juicefs info")
		return nil, nil
	}
	t.Cleanup(func() { jfsInfoOutput = previous })

	tracker := &jfsUploadTracker{paths: []string{"/rootfs"}, roots: []string{root}}
	staged, ready, err := tracker.check(context.Background())
	require.NoError(t, err)
	require.False(t, ready)
	require.Equal(t, 7, staged)
}

func TestDeleteSnapshotCatalogForClearsOnlyJFSUploadTracker(t *testing.T) {
	const id = "tracker-delete-test"
	tracker := &jfsUploadTracker{}
	jfsUploadTrackers.Store(id, tracker)
	t.Cleanup(func() { jfsUploadTrackers.Delete(id) })

	DeleteSnapshotCatalogFor(cow.BackendS3, id)
	actual, ok := jfsUploadTrackers.Load(id)
	require.True(t, ok)
	require.Same(t, tracker, actual)

	DeleteSnapshotCatalogFor(cow.BackendJFS, id)
	_, ok = jfsUploadTrackers.Load(id)
	require.False(t, ok)
}
