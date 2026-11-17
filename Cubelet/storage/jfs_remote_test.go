// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// useTestJFSStorage mounts a fake shared JFS store and cache in temp dirs.
func useTestJFSStorage(t *testing.T, engine *fakeCowEngine) (root, cache string) {
	t.Helper()
	mount := t.TempDir()
	cache = t.TempDir()
	cfg := &Config{StorageBackend: "cubecow", RootPath: t.TempDir()}
	cfg.Cow.JFS.Enable = true
	cfg.Cow.JFS.MountPath = mount
	cfg.Cow.JFS.CacheDir = cache
	require.NoError(t, os.MkdirAll(cfg.jfsStorageRoot(), 0o755))
	previous := localStorage
	localStorage = &local{
		config:        cfg,
		cowManager:    &XfsCow{engine: engine},
		jfsCowManager: &XfsCow{engine: engine},
	}
	t.Cleanup(func() { localStorage = previous })
	return cfg.jfsStorageRoot(), cache
}

type fakeJFSControl struct {
	request  bytes.Buffer
	response []byte
	offset   int
	maxRead  int
	closed   bool
}

func (f *fakeJFSControl) Write(p []byte) (int, error) {
	return f.request.Write(p)
}

func (f *fakeJFSControl) Read(p []byte) (int, error) {
	if f.offset == len(f.response) {
		return 0, io.EOF
	}
	n := len(p)
	if f.maxRead > 0 && n > f.maxRead {
		n = f.maxRead
	}
	if left := len(f.response) - f.offset; n > left {
		n = left
	}
	copy(p, f.response[f.offset:f.offset+n])
	f.offset += n
	return n, nil
}

func (f *fakeJFSControl) Close() error {
	f.closed = true
	return nil
}

func TestQueryJFSControlObjectKeys(t *testing.T) {
	target := filepath.Join(t.TempDir(), "rootfs")
	require.NoError(t, os.WriteFile(target, []byte("rootfs"), 0o644))
	info, err := os.Stat(target)
	require.NoError(t, err)
	inode := info.Sys().(*syscall.Stat_t).Ino

	payload := []byte(`{"Ino":1,"Failed":false,"Objects":[` +
		`{"ChunkIndex":0,"Key":"cube/chunks/0/385/385229_0_4194304"},` +
		`{"ChunkIndex":0,"Key":"cube/chunks/0/385/385229_1_1234"}]}`)
	response := make([]byte, 17+5+len(payload))
	response[0] = jfsControlProgress
	response[17] = jfsControlData
	binary.BigEndian.PutUint32(response[18:22], uint32(len(payload)))
	copy(response[22:], payload)
	control := &fakeJFSControl{response: response, maxRead: 3}

	previous := jfsOpenControl
	jfsOpenControl = func(path string) (jfsControlFile, error) {
		require.Equal(t, target, path)
		return control, nil
	}
	t.Cleanup(func() { jfsOpenControl = previous })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	keys, err := queryJFSControlObjectKeys(ctx, target)
	require.NoError(t, err)
	require.Equal(t, []string{
		"chunks/0/385/385229_0_4194304",
		"chunks/0/385/385229_1_1234",
	}, keys)
	require.True(t, control.closed)

	request := control.request.Bytes()
	require.Len(t, request, 19)
	require.Equal(t, jfsControlInfoV2, binary.BigEndian.Uint32(request[0:4]))
	require.Equal(t, jfsControlInfoBodySize, binary.BigEndian.Uint32(request[4:8]))
	require.Equal(t, inode, binary.BigEndian.Uint64(request[8:16]))
	require.Equal(t, []byte{0, 0, 0}, request[16:19])
}

func TestReadJFSControlDataRejectsErrorsAndOversize(t *testing.T) {
	_, err := readJFSControlData(context.Background(), bytes.NewReader([]byte{byte(syscall.EIO)}))
	require.ErrorContains(t, err, "input/output error")

	header := make([]byte, 5)
	header[0] = jfsControlData
	binary.BigEndian.PutUint32(header[1:5], jfsControlMaxResponseBytes+1)
	_, err = readJFSControlData(context.Background(), bytes.NewReader(header))
	require.ErrorContains(t, err, "too large")
}

func TestJFSStagedBlocksCountsOnlyStagedObjects(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	staged := filepath.Join(cache, "uuid-1", jfsStagingDir, "chunks", "0", "385", "385229_1_4194304")
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
	require.NoError(t, os.WriteFile(staged, nil, 0o644))

	previous := jfsPathObjectKeys
	jfsPathObjectKeys = func(context.Context, string) ([]string, error) {
		return []string{
			"chunks/0/385/385229_0_4194304",
			"chunks/0/385/385229_1_4194304",
		}, nil
	}
	t.Cleanup(func() { jfsPathObjectKeys = previous })

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
	previous := jfsPathObjectKeys
	t.Cleanup(func() { jfsPathObjectKeys = previous })
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(meta, name)
			jfsPathObjectKeys = func(_ context.Context, p string) ([]string, error) {
				if p == target {
					return []string{"chunks/0/0/1_0_1"}, nil
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

// A filesystem-only package has no memory, but a cold boot still reads the
// guest runtime metadata and the VM config. Only the VM state file is
// mode-specific, so its absence must not excuse the other two.
func TestJFSPackageUploadRequiresGuestMetadataWithoutMemory(t *testing.T) {
	_, _ = useTestJFSStorage(t, &fakeCowEngine{})
	home := t.TempDir()
	meta := filepath.Join(home, SnapshotMetadataDir)
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(meta, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	write(snapshotCatalogFileName, `{"kind":"runtime_snapshot"}`)
	write("metadata.json", `{}`)
	write("snapshot/config.json", `{}`)

	// No memory: exactly the filesystem-only shape.
	files := &cow.RemoteUUIDs{Rootfs: "/rootfs", Metadata: home}
	_, err := jfsPackageUploadPaths(files)
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(meta, "metadata.json")))
	_, err = jfsPackageUploadPaths(files)
	require.Error(t, err)
}

func TestJFSStagedBlocksInspectionFailures(t *testing.T) {
	_, cache := useTestJFSStorage(t, &fakeCowEngine{})
	_, err := jfsStagedBlocks(context.Background(), "/f")
	require.Error(t, err) // Empty/misconfigured cache cannot establish readiness.
	root := filepath.Join(cache, "uuid", jfsStagingDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(root), 0o755))
	previous := jfsPathObjectKeys
	t.Cleanup(func() { jfsPathObjectKeys = previous })
	jfsPathObjectKeys = func(context.Context, string) ([]string, error) {
		return []string{"chunks/0/0/1_0_1"}, nil
	}
	n, err := jfsStagedBlocks(context.Background(), "/f")
	require.NoError(t, err)
	require.Zero(t, n) // Lazily-created staging is valid.
	require.NoError(t, os.MkdirAll(root, 0o755))
	// ENOTDIR must not be treated as an absent block.
	require.NoError(t, os.WriteFile(filepath.Join(root, "chunks"), nil, 0o644))
	_, err = jfsStagedBlocks(context.Background(), "/f")
	require.Error(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "chunks")))
	jfsPathObjectKeys = func(context.Context, string) ([]string, error) { return nil, os.ErrPermission }
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

	previous := jfsPathObjectKeys
	calls := 0
	jfsPathObjectKeys = func(context.Context, string) ([]string, error) {
		calls++
		return []string{key}, nil
	}
	t.Cleanup(func() { jfsPathObjectKeys = previous })

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

	previous := jfsPathObjectKeys
	jfsPathObjectKeys = func(context.Context, string) ([]string, error) {
		t.Fatal("global-zero fast path must not query JuiceFS control")
		return nil, nil
	}
	t.Cleanup(func() { jfsPathObjectKeys = previous })

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

	previous := jfsPathObjectKeys
	jfsPathObjectKeys = func(context.Context, string) ([]string, error) {
		t.Fatal("global staging grace period must not query JuiceFS control")
		return nil, nil
	}
	t.Cleanup(func() { jfsPathObjectKeys = previous })

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
