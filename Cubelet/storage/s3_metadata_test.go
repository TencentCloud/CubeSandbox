// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

type memS3MetadataKV struct {
	mu     sync.Mutex
	data   []byte
	setErr error
}

func (m *memS3MetadataKV) Get() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.data) == 0 {
		return nil, nil
	}
	out := make([]byte, len(m.data))
	copy(out, m.data)
	return out, nil
}

func (m *memS3MetadataKV) Set(body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	m.data = append([]byte(nil), body...)
	return nil
}

func stubS3MetadataMounts(t *testing.T) {
	t.Helper()
	mounted := map[string]string{}
	origFormat := formatS3MetadataBaseDevice
	origPack := packS3MetadataDevice
	origExecPack := execS3MetadataPack
	origProbe := checkMkfsExt4PopulateSupport
	origMount := mountS3MetadataDevice
	origUmount := unmountS3MetadataPath
	origIsMounted := s3MetadataIsMounted
	origMountSource := s3MetadataMountSource
	origKV := testS3MetadataKV
	kv := &memS3MetadataKV{}
	testS3MetadataKV = kv
	mkfsExt4PopulateSupported.Store(false)
	formatS3MetadataBaseDevice = func(string) error {
		return nil
	}
	packS3MetadataDevice = func(string, string) error {
		return nil
	}
	checkMkfsExt4PopulateSupport = func() error {
		return nil
	}
	mountS3MetadataDevice = func(devicePath, mountPath string) error {
		mounted[mountPath] = devicePath
		return nil
	}
	unmountS3MetadataPath = func(mountPath string) error {
		delete(mounted, mountPath)
		return nil
	}
	s3MetadataIsMounted = func(mountPath string) bool {
		_, ok := mounted[mountPath]
		return ok
	}
	s3MetadataMountSource = func(mountPath string) string { return mounted[mountPath] }
	t.Cleanup(func() {
		formatS3MetadataBaseDevice = origFormat
		packS3MetadataDevice = origPack
		execS3MetadataPack = origExecPack
		checkMkfsExt4PopulateSupport = origProbe
		mountS3MetadataDevice = origMount
		unmountS3MetadataPath = origUmount
		s3MetadataIsMounted = origIsMounted
		s3MetadataMountSource = origMountSource
		testS3MetadataKV = origKV
		mkfsExt4PopulateSupported.Store(false)
		s3MetadataMu.Lock()
		s3MetadataMounts = map[string]string{}
		s3MetadataMu.Unlock()
	})
}

func writeValidS3PackageMetadata(t *testing.T, metaDir string, withMemory, withPauseSpec bool) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(metaDir, "snapshot"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(metaDir, "metadata.json"), []byte(`{"version":"1"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(metaDir, "snapshot", "config.json"), []byte(`{"ok":true}`), 0o644))
	if withMemory {
		require.NoError(t, os.WriteFile(filepath.Join(metaDir, "snapshot", "state.json"), []byte(`{"ok":true}`), 0o644))
	}
	if withPauseSpec {
		require.NoError(t, os.WriteFile(filepath.Join(metaDir, "sandbox_spec.json"), []byte(`{"sandbox":"sb"}`), 0o644))
	}
}

func TestS3MetadataMountAliasDoesNotRemount(t *testing.T) {
	stubS3MetadataMounts(t)
	root := t.TempDir()
	real := filepath.Join(root, "real data", "metadata")
	require.NoError(t, os.MkdirAll(real, 0o755))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(filepath.Dir(real), alias))
	aliasMount := filepath.Join(alias, "metadata")
	// The kernel escapes spaces and records the real path, not the alias.
	fixture := fmt.Sprintf("42 1 259:1 / %s rw - ext4 /dev/nvme42n1 rw\n", strings.ReplaceAll(real, " ", `\040`))
	s3MetadataMountSource = func(path string) string {
		return s3MetadataMountSourceFrom(strings.NewReader(fixture), path)
	}
	s3MetadataIsMounted = s3MetadataIsMountedImpl
	mounts, unmounts := 0, 0
	mountS3MetadataDevice = func(string, string) error { mounts++; return nil }
	unmountS3MetadataPath = func(string) error { unmounts++; return nil }
	s3MetadataMounts["tpl-alias"] = aliasMount
	require.True(t, s3MetadataIsMounted(aliasMount))
	require.Equal(t, "/dev/nvme42n1", s3MetadataMountSource(aliasMount))
	require.NoError(t, mountS3MetadataLocked("tpl-alias", "/dev/nvme42n1", real))
	require.NoError(t, mountS3MetadataLocked("tpl-alias", "/dev/nvme42n1", aliasMount))
	require.Zero(t, mounts)
	require.Zero(t, unmounts)
}

func TestS3MetadataUnknownSourceDoesNotUnmount(t *testing.T) {
	stubS3MetadataMounts(t)
	s3MetadataIsMounted = func(string) bool { return true }
	s3MetadataMountSource = func(string) string { return "" }
	unmountS3MetadataPath = func(string) error { t.Fatal("must preserve an unidentified mounted disk"); return nil }
	require.ErrorContains(t, mountS3MetadataLocked("tpl-unknown", "/dev/nvme42n1", t.TempDir()), "refusing to unmount")
}

func TestS3MetadataRecoverySkipsActivePackageWriter(t *testing.T) {
	stubS3MetadataMounts(t)
	id := "tpl-writing"
	engine := &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		S3MetadataVolumeName(id): {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/meta-writing"},
	}}
	useTestCowStorage(t, engine)
	_, release, err := BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	t.Cleanup(release)
	require.True(t, S3PackageOperationInProgress(id))
	_, _, err = BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	require.Error(t, err)
	dir := t.TempDir()
	require.NoError(t, MountS3MetadataForRecovery(context.Background(), cow.BackendS3, id, dir))
	require.Empty(t, engine.activatedVolumes)
	require.False(t, s3MetadataIsMounted(dir))
	release()
	release()
	require.False(t, S3PackageOperationInProgress(id))
	require.NoError(t, MountS3MetadataForRecovery(context.Background(), cow.BackendS3, id, dir))
	require.True(t, s3MetadataIsMounted(dir))
}

func TestValidateS3PackageMetadata(t *testing.T) {
	dir := t.TempDir()
	writeValidS3PackageMetadata(t, dir, true, false)
	// Plain staged directories pass validation without being mounted.
	require.NoError(t, ValidateS3PackageMetadata(cow.BackendS3, dir))
	require.NoError(t, ValidateS3PackageMetadataFor(cow.BackendS3, dir, false))

	// Invalid JSON and non-object JSON values (such as null or arrays) are rejected.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot", "config.json"), []byte(`not-json`), 0o644))
	require.ErrorContains(t, ValidateS3PackageMetadata(cow.BackendS3, dir), "invalid JSON")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot", "config.json"), []byte(`null`), 0o644))
	require.ErrorContains(t, ValidateS3PackageMetadata(cow.BackendS3, dir), "expected JSON object")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot", "config.json"), []byte(`[]`), 0o644))
	require.ErrorContains(t, ValidateS3PackageMetadata(cow.BackendS3, dir), "invalid JSON")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "snapshot", "config.json"), []byte(`{}`), 0o644))

	// Missing metadata.json is rejected for both full and fs-only snapshots.
	require.NoError(t, os.Remove(filepath.Join(dir, "metadata.json")))
	require.ErrorContains(t, ValidateS3PackageMetadata(cow.BackendS3, dir), "metadata.json")
	require.ErrorContains(t, ValidateS3PackageMetadataFor(cow.BackendS3, dir, false), "metadata.json")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{}`), 0o644))

	// Missing state.json fails full validation but passes fs-only validation.
	require.NoError(t, os.Remove(filepath.Join(dir, "snapshot", "state.json")))
	require.ErrorContains(t, ValidateS3PackageMetadata(cow.BackendS3, dir), "state.json")
	require.NoError(t, ValidateS3PackageMetadataFor(cow.BackendS3, dir, false))

	// Symlink files are rejected.
	target := filepath.Join(t.TempDir(), "real-state.json")
	require.NoError(t, os.WriteFile(target, []byte(`{}`), 0o644))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "snapshot", "state.json")))
	require.ErrorContains(t, ValidateS3PackageMetadata(cow.BackendS3, dir), "symlink")

	// Non-S3 backends are no-op.
	require.NoError(t, ValidateS3PackageMetadata(cow.BackendXFS, dir))
}

func TestEnsureS3MetadataBaseCreatesOnce(t *testing.T) {
	stubS3MetadataMounts(t)
	formatted := 0
	formatS3MetadataBaseDevice = func(string) error {
		formatted++
		return nil
	}
	engine := &fakeCowEngine{createVolumePath: "/dev/mapper/" + S3MetadataBaseVolumeName}
	useTestCowStorage(t, engine)

	require.NoError(t, EnsureS3MetadataBase(context.Background()))
	require.Equal(t, 1, formatted)
	require.Equal(t, []string{S3MetadataBaseVolumeName}, engine.createVolumes)
	require.Equal(t, uint64(s3MetadataBaseSizeBytes), engine.createVolumeSizes[S3MetadataBaseVolumeName])
	require.Contains(t, engine.deactivatedVolumes, S3MetadataBaseVolumeName)
	require.Empty(t, engine.volumeInfos[S3MetadataBaseVolumeName].DevicePath)

	require.NoError(t, EnsureS3MetadataBase(context.Background()))
	require.Equal(t, 1, formatted)
	require.Len(t, engine.createVolumes, 1)
	require.Empty(t, engine.volumeInfos[S3MetadataBaseVolumeName].DevicePath)
}

func TestEnsureS3MetadataBaseRecreatesWhenMissing(t *testing.T) {
	stubS3MetadataMounts(t)
	formatted := 0
	formatS3MetadataBaseDevice = func(string) error {
		formatted++
		return nil
	}
	engine := &fakeCowEngine{createVolumePath: "/dev/mapper/" + S3MetadataBaseVolumeName}
	useTestCowStorage(t, engine)

	require.NoError(t, EnsureS3MetadataBase(context.Background()))
	require.Equal(t, 1, formatted)
	delete(engine.volumeInfos, S3MetadataBaseVolumeName)

	require.NoError(t, EnsureS3MetadataBase(context.Background()))
	require.Equal(t, 2, formatted)
	require.Len(t, engine.createVolumes, 2)
}

func TestRollbackSnapshotMetadataNoopOnXFS(t *testing.T) {
	dir, err := MaterializeRollbackSnapshotMetadata(context.Background(), cow.BackendXFS, "snap-1", "sb-1")
	require.NoError(t, err)
	require.Empty(t, dir)
	require.NoError(t, ReleaseRollbackSnapshotMetadata(context.Background(), cow.BackendXFS, "sb-1"))
	require.NoError(t, ReleaseS3MetadataVolume(context.Background(), cow.BackendXFS, "snap-1"))
}

func TestMountS3MetadataAtClonesSealedSnap(t *testing.T) {
	stubS3MetadataMounts(t)
	id := "pause-1"
	snap := S3MetadataSnapshotName(id)
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			S3MetadataBaseVolumeName:   {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + S3MetadataBaseVolumeName},
			S3MetadataBaseSnapshotName: {SizeBytes: 8 << 20},
			snap:                       {SizeBytes: 8 << 20},
		},
	}
	useTestCowStorage(t, engine)

	mount := t.TempDir()
	require.NoError(t, MountS3MetadataAt(context.Background(), cow.BackendS3, id, mount))
	require.Equal(t, [][2]string{{snap, S3MetadataVolumeName(id)}}, engine.createVolumeFromSnapshots)
	require.True(t, s3MetadataIsMounted(mount))
	require.NotContains(t, engine.activatedVolumes, snap)

	require.NoError(t, ReleaseS3MetadataVolume(context.Background(), cow.BackendS3, id))
	require.Contains(t, engine.deletedVolumes, S3MetadataVolumeName(id))
	require.Contains(t, engine.deletedSnapshots, snap)
	require.NotContains(t, engine.deletedVolumes, S3MetadataBaseVolumeName)
	require.NotContains(t, engine.deletedSnapshots, S3MetadataBaseSnapshotName)
	require.False(t, s3MetadataIsMounted(mount))
}

func TestCloneS3MetadataFromParentUsesParentVolume(t *testing.T) {
	stubS3MetadataMounts(t)
	parentSnap := S3MetadataSnapshotName("tpl-1")
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			S3MetadataBaseVolumeName:   {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + S3MetadataBaseVolumeName},
			S3MetadataBaseSnapshotName: {SizeBytes: 8 << 20, DevicePath: ""},
			parentSnap:                 {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + parentSnap},
		},
	}
	useTestCowStorage(t, engine)

	mount := t.TempDir()
	require.NoError(t, CloneS3MetadataFromParent(context.Background(), cow.BackendS3, "tpl-1", "sb-1", mount))
	require.Equal(t, [][2]string{{parentSnap, S3MetadataVolumeName("sb-1")}}, engine.createVolumeFromSnapshots)
	require.True(t, s3MetadataIsMounted(mount))
}

func TestCloneS3MetadataFromParentFallsBackToBase(t *testing.T) {
	stubS3MetadataMounts(t)
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			S3MetadataBaseVolumeName: {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + S3MetadataBaseVolumeName},
		},
	}
	useTestCowStorage(t, engine)

	mount := t.TempDir()
	require.NoError(t, CloneS3MetadataFromParent(context.Background(), cow.BackendS3, "tpl-missing", "sb-2", mount))
	require.Contains(t, engine.createSnapshots, [2]string{S3MetadataBaseVolumeName, S3MetadataBaseSnapshotName})
	require.Equal(t, [][2]string{{S3MetadataBaseSnapshotName, S3MetadataVolumeName("sb-2")}}, engine.createVolumeFromSnapshots)
}

func TestUploadStatusUsesExportStatus(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {
				SizeBytes:    1 << 20,
				ExportUUID:   "uuid-root",
				ExportStatus: cow.ExportStatusDone,
			},
			"tpl-snap-1-memory": {
				SizeBytes:    64 << 20,
				ExportUUID:   "uuid-mem",
				ExportStatus: cow.ExportStatusInProgress,
			},
		},
	}
	useTestCowStorage(t, engine)

	st, err := SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateRunning, st.State)
	require.Equal(t, "uuid-root", st.RemoteUUIDs.Rootfs)
	require.Equal(t, "uuid-mem", st.RemoteUUIDs.Memory)

	engine.volumeInfos["tpl-snap-1-memory"].ExportStatus = cow.ExportStatusDone
	st, err = SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateReady, st.State)
}

// No uuid yet means the export has not been recorded anywhere, so an empty
// status is cubecow having nothing to report on rather than a lost export.
func TestUploadStatusEmptyExportStatusWithoutUUIDIsRunning(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {SizeBytes: 1 << 20},
			"tpl-snap-1-memory": {SizeBytes: 64 << 20},
		},
	}
	useTestCowStorage(t, engine)

	st, err := SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateRunning, st.State)
}

// A recorded uuid with empty export_status is not an s3lvol-confirmed
// failure. Status must stay non-terminal so Master keeps inprogress.
func TestUploadStatusEmptyExportStatusWithUUIDIsRunning(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {
				SizeBytes:    1 << 20,
				ExportUUID:   "uuid-root",
				ExportStatus: cow.ExportStatusDone,
			},
			"tpl-snap-1-memory": {SizeBytes: 64 << 20, ExportUUID: "uuid-mem"},
		},
	}
	useTestCowStorage(t, engine)

	st, err := SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateRunning, st.State)
}

func TestUploadStatusExportStatusFailedIsFailed(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs": {
				SizeBytes:    1 << 20,
				ExportUUID:   "uuid-root",
				ExportStatus: cow.ExportStatusDone,
			},
			"tpl-snap-1-memory": {
				SizeBytes:    64 << 20,
				ExportUUID:   "uuid-mem",
				ExportStatus: cow.ExportStatusFailed,
			},
		},
	}
	useTestCowStorage(t, engine)

	st, err := SnapshotUploadStatus(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.Equal(t, cow.RemoteStateFailed, st.State)
	require.Contains(t, st.Message, "tpl-snap-1-memory")
}

func TestUploadSkipsMetadataBaseAndUploadsDerived(t *testing.T) {
	stubS3MetadataMounts(t)
	engine := &fakeCowEngine{
		createVolumePath: "/dev/mapper/" + S3MetadataVolumeName("snap-1"),
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-1-rootfs":            {SizeBytes: 1 << 20},
			"tpl-snap-1-memory":            {SizeBytes: 64 << 20},
			S3MetadataVolumeName("snap-1"): {SizeBytes: 8 << 20},
			S3MetadataBaseVolumeName:       {SizeBytes: 8 << 20},
		},
	}
	useTestCowStorageAt(t, engine)
	t.Cleanup(func() { DeleteSnapshotCatalogFor(cow.BackendS3, "snap-1") })
	home := SnapshotHome(cow.BackendS3, SnapshotKindNormal, "snap-1")
	require.NoError(t, EnsureSnapshotPackage(cow.BackendS3, home))
	metaDir := filepath.Join(home, SnapshotMetadataDir)
	writeValidS3PackageMetadata(t, metaDir, true, false)
	require.NoError(t, WriteSnapshotCatalogFor(cow.BackendS3, &SnapshotCatalogEntry{
		SnapshotID:   "snap-1",
		SnapshotPath: home,
		MetaDir:      metaDir,
		RootfsVol:    "tpl-snap-1-rootfs",
		RootfsKind:   cowKindSnapshot,
		MemoryVol:    "tpl-snap-1-memory",
		MemoryKind:   cowKindVolume,
		MetadataVol:  S3MetadataVolumeName("snap-1"),
		MetadataKind: cowKindVolume,
		Backend:      cow.BackendS3,
	}))
	res, err := FinalizeS3PackageSnapshots(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.NoError(t, res.CleanupErr)
	require.Contains(t, engine.deletedVolumes, S3MetadataVolumeName("snap-1"))
	require.Contains(t, engine.deactivatedVolumes, S3MetadataVolumeName("snap-1"))
	require.Contains(t, engine.deactivatedVolumes, S3MetadataSnapshotName("snap-1"))
	require.NotContains(t, engine.activatedVolumes, S3MetadataSnapshotName("snap-1"))

	uuids, err := UploadSnapshot(context.Background(), cow.BackendS3, "snap-1")
	require.NoError(t, err)
	require.NotEmpty(t, uuids.Rootfs)
	require.NotEmpty(t, uuids.Memory)
	require.NotEmpty(t, uuids.Metadata)
	require.NotEqual(t, uuids.Rootfs, uuids.Metadata)
	require.NotContains(t, engine.exportSnapshots, S3MetadataBaseVolumeName)
	require.Contains(t, engine.exportSnapshots, S3MetadataSnapshotName("snap-1"))
	require.NotContains(t, engine.exportSnapshots, S3MetadataVolumeName("snap-1"))

	_, err = (&S3Cow{engine: engine}).uploadOne(S3MetadataBaseVolumeName)
	require.Error(t, err)
	require.Contains(t, err.Error(), "node-local s3 metadata base")
}

func TestS3CowEmptyDevicePathAfterCreateFails(t *testing.T) {
	engine := &fakeCowEngine{createVolumePath: ""}
	store := &S3Cow{engine: engine}

	_, _, err := store.createOrResolveVolumePath(context.Background(), "vol-empty", 8<<20)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty device_path")
	require.Contains(t, err.Error(), "s3lvol/NVMe-oF")

	_, err = store.createTemplateVolumePath("tpl-empty", 8<<20)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty device_path")
}

func TestS3ResolveDevPathAlwaysActivatesForLatestPath(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-x-memory-snap": {DevicePath: "/dev/nvme17n1", SizeBytes: 64 << 20},
		},
		activatePaths: map[string]string{
			"tpl-x-memory-snap": "/dev/nvme3n1",
		},
	}
	store := &S3Cow{engine: engine}
	devPath, err := store.ResolveDevPath(context.Background(), "tpl-x-memory-snap", cowKindSnapshot)
	require.NoError(t, err)
	require.Equal(t, "/dev/nvme3n1", devPath)
	require.Equal(t, []string{"tpl-x-memory-snap"}, engine.activatedVolumes)
	require.Equal(t, "/dev/nvme3n1", engine.volumeInfos["tpl-x-memory-snap"].DevicePath)
}

func TestS3CowEmptyDevicePathAfterActivateFails(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"vol-1": {DevicePath: "", SizeBytes: 8 << 20},
		},
		activatePaths: map[string]string{
			"vol-1": "",
		},
	}
	store := &S3Cow{engine: engine}
	_, err := store.ResolveDevPath(context.Background(), "vol-1", cowKindVolume)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty device_path")
	require.Contains(t, err.Error(), "ActivateVolume")
	require.Contains(t, err.Error(), "refuse mkfs/mount")
}

func TestEnsureS3MetadataBaseFailsOnEmptyDevicePath(t *testing.T) {
	stubS3MetadataMounts(t)
	engine := &fakeCowEngine{createVolumePath: ""}
	useTestCowStorage(t, engine)

	err := EnsureS3MetadataBase(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty device_path")
	require.NotContains(t, engine.createVolumes, "") // still attempted create
	require.Equal(t, []string{S3MetadataBaseVolumeName}, engine.createVolumes)
}

func TestIsS3MetadataBaseName(t *testing.T) {
	require.True(t, IsS3MetadataBaseName(S3MetadataBaseVolumeName))
	require.True(t, IsS3MetadataBaseName(S3MetadataBaseSnapshotName))
	require.False(t, IsS3MetadataBaseName(S3MetadataVolumeName("snap-1")))
	require.Equal(t, "s3-meta-snap-1", S3MetadataVolumeName("snap-1"))
	require.Equal(t, "", S3MetadataCatalogVol(cow.BackendXFS, "snap-1"))
	require.Equal(t, "s3-meta-snap-1", S3MetadataCatalogVol(cow.BackendS3, "snap-1"))
	require.Equal(t, cowKindVolume, S3MetadataCatalogKind(cow.BackendS3))
	require.Equal(t, "", S3MetadataCatalogKind(cow.BackendXFS))
}

func TestEnsureS3MetadataBaseDeactivatesAlreadyActiveLeftover(t *testing.T) {
	stubS3MetadataMounts(t)
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			S3MetadataBaseVolumeName:   {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + S3MetadataBaseVolumeName},
			S3MetadataBaseSnapshotName: {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + S3MetadataBaseSnapshotName},
		},
	}
	useTestCowStorage(t, engine)

	require.NoError(t, EnsureS3MetadataBase(context.Background()))
	require.Empty(t, engine.createVolumes)
	require.Contains(t, engine.deactivatedVolumes, S3MetadataBaseVolumeName)
	require.Contains(t, engine.deactivatedVolumes, S3MetadataBaseSnapshotName)
	require.Empty(t, engine.volumeInfos[S3MetadataBaseVolumeName].DevicePath)
	require.Empty(t, engine.volumeInfos[S3MetadataBaseSnapshotName].DevicePath)
}

func TestEnsureS3MetadataReadyLeavesPackageObjectsAlone(t *testing.T) {
	stubS3MetadataMounts(t)
	id := "tpl-7b70df45c3294788aa3dbbdb"
	vol := S3MetadataVolumeName(id)
	snap := S3MetadataSnapshotName(id)
	mem := cowTemplateMemoryName(id) + "-snap"
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			S3MetadataBaseVolumeName:   {SizeBytes: 8 << 20},
			S3MetadataBaseSnapshotName: {SizeBytes: 8 << 20},
			vol:                        {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + vol},
			snap:                       {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/" + snap},
			mem:                        {SizeBytes: 64 << 20, DevicePath: "/dev/mapper/" + mem},
		},
	}
	useTestCowStorage(t, engine)
	localStorage.config.DataPath = t.TempDir()

	home := SnapshotHome(cow.BackendS3, SnapshotKindNormal, id)
	require.NoError(t, EnsureSnapshotPackage(cow.BackendS3, home))
	s3MetadataMu.Lock()
	require.NoError(t, persistDerivedLocked(id, vol, filepath.Join(home, SnapshotMetadataDir)))
	s3MetadataMu.Unlock()

	// Startup only reconciles the node-local base. It must not clone, mount,
	// attach or detach a package disk: a resumed sandbox reads its memory snap
	// for the rest of its life, and a restarted Cubelet cannot tell that disk
	// from a leftover.
	require.NoError(t, EnsureS3MetadataReady(context.Background()))
	require.Empty(t, engine.createVolumeFromSnapshots)
	require.NotContains(t, engine.deactivatedVolumes, vol)
	require.NotContains(t, engine.deactivatedVolumes, snap)
	require.NotContains(t, engine.deactivatedVolumes, mem)
	require.NotEmpty(t, engine.volumeInfos[vol].DevicePath)
	require.NotEmpty(t, engine.volumeInfos[snap].DevicePath)
	require.NotEmpty(t, engine.volumeInfos[mem].DevicePath)
}

func newRollbackMaterializeFixture(
	t *testing.T,
	snapshotID, sandboxID string,
	populate func(t *testing.T, mountPath string),
) (*fakeCowEngine, string) {
	t.Helper()
	stubS3MetadataMounts(t)
	packageSnap := S3MetadataSnapshotName(snapshotID)
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			S3MetadataBaseVolumeName:   {SizeBytes: 8 << 20},
			S3MetadataBaseSnapshotName: {SizeBytes: 8 << 20},
			packageSnap:                {SizeBytes: 8 << 20},
		},
	}
	useTestCowStorageAt(t, engine)
	if populate != nil {
		origMount := mountS3MetadataDevice
		mountS3MetadataDevice = func(devicePath, mountPath string) error {
			if err := origMount(devicePath, mountPath); err != nil {
				return err
			}
			populate(t, mountPath)
			return nil
		}
	}
	return engine, packageSnap
}

func TestUnmountPackageMetadata(t *testing.T) {
	t.Run("non-s3 is a no-op", func(t *testing.T) {
		stubS3MetadataMounts(t)
		for _, backend := range []string{cow.BackendXFS, cow.BackendJFS} {
			require.NoError(t, UnmountPackageMetadata(backend, "tpl-x", "/nonexistent/metadata"))
		}
	})

	t.Run("empty id is a no-op and unsafe id is rejected", func(t *testing.T) {
		stubS3MetadataMounts(t)
		require.NoError(t, UnmountPackageMetadata(cow.BackendS3, "", "/nonexistent/metadata"))
		require.ErrorContains(t, UnmountPackageMetadata(cow.BackendS3, "../escape", "/nonexistent/metadata"), "invalid")
	})

	t.Run("unmounts the package metadata dir", func(t *testing.T) {
		stubS3MetadataMounts(t)
		id := "tpl-umount"
		metaDir := filepath.Join(t.TempDir(), SnapshotMetadataDir)
		require.NoError(t, os.MkdirAll(metaDir, 0o755))
		require.NoError(t, mountS3MetadataDevice("/dev/mapper/meta-umount", metaDir))
		require.True(t, s3MetadataIsMounted(metaDir))

		require.NoError(t, UnmountPackageMetadata(cow.BackendS3, id, metaDir))
		require.False(t, s3MetadataIsMounted(metaDir), "stale metadata mount must be gone")
	})

	t.Run("unmounts a recorded mount path", func(t *testing.T) {
		stubS3MetadataMounts(t)
		id := "tpl-umount-alias"
		alias := filepath.Join(t.TempDir(), "alias")
		require.NoError(t, os.MkdirAll(alias, 0o755))
		require.NoError(t, mountS3MetadataDevice("/dev/mapper/meta-alias", alias))
		s3MetadataMu.Lock()
		s3MetadataMounts[id] = alias
		s3MetadataMu.Unlock()

		require.NoError(t, UnmountPackageMetadata(cow.BackendS3, id, ""))
		require.False(t, s3MetadataIsMounted(alias), "recorded mount must be released")
	})

	t.Run("propagates unmount failure", func(t *testing.T) {
		stubS3MetadataMounts(t)
		id := "tpl-umount-busy"
		metaDir := filepath.Join(t.TempDir(), SnapshotMetadataDir)
		require.NoError(t, os.MkdirAll(metaDir, 0o755))
		require.NoError(t, mountS3MetadataDevice("/dev/mapper/meta-busy", metaDir))
		unmountS3MetadataPath = func(string) error { return errors.New("umount busy") }

		require.ErrorContains(t, UnmountPackageMetadata(cow.BackendS3, id, metaDir), "umount busy")
	})
}

func TestMaterializeRollbackMetadataReleaseFailureKeepsCopy(t *testing.T) {
	snapshotID := "snap-relfail"
	sandboxID := "sb-relfail"
	newRollbackMaterializeFixture(t, snapshotID, sandboxID, func(t *testing.T, mountPath string) {
		snapDir := filepath.Join(mountPath, "snapshot")
		require.NoError(t, os.MkdirAll(snapDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(snapDir, "config.json"), []byte(`{"vm":"cfg"}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(snapDir, "state.json"), []byte(`{"vm":"state"}`), 0o644))
	})
	unmountS3MetadataPath = func(string) error { return errors.New("umount busy") }

	// A clone-release failure must not fail the rollback.
	dir, err := MaterializeRollbackSnapshotMetadata(context.Background(), cow.BackendS3, snapshotID, sandboxID)
	require.NoError(t, err)
	require.NotEmpty(t, dir)
	require.FileExists(t, filepath.Join(dir, "snapshot", "config.json"))
	require.FileExists(t, filepath.Join(dir, "snapshot", "state.json"))
}

func TestMaterializeRollbackMetadataReturnsPlainDir(t *testing.T) {
	snapshotID := "snap-4c0c3e1f"
	sandboxID := "b1c1d1e1f1"
	engine, packageSnap := newRollbackMaterializeFixture(t, snapshotID, sandboxID, func(t *testing.T, mountPath string) {
		snapDir := filepath.Join(mountPath, "snapshot")
		require.NoError(t, os.MkdirAll(snapDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(snapDir, "config.json"), []byte(`{"vm":"cfg"}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(snapDir, "state.json"), []byte(`{"vm":"state"}`), 0o644))
	})
	origUmount := unmountS3MetadataPath
	unmountS3MetadataPath = func(mountPath string) error {
		if err := os.RemoveAll(filepath.Join(mountPath, "snapshot")); err != nil {
			return err
		}
		return origUmount(mountPath)
	}

	ctx := context.Background()
	dir, err := MaterializeRollbackSnapshotMetadata(ctx, cow.BackendS3, snapshotID, sandboxID)
	require.NoError(t, err)

	owned := S3MetadataVolumeName(RollbackMetadataOwnerID(sandboxID))
	mnt := dir + ".mnt"
	require.Equal(t, [][2]string{{packageSnap, owned}}, engine.createVolumeFromSnapshots)
	require.False(t, s3MetadataIsMounted(dir), "returned dir must be a plain directory, not a mount")
	require.False(t, s3MetadataIsMounted(mnt), "temporary mount must already be unmounted")
	require.NoDirExists(t, mnt, "temporary mount dir must be removed before returning")
	require.NotEmpty(t, engine.deletedVolumes)
	for _, name := range engine.deletedVolumes {
		require.Equal(t, owned, name)
	}
	require.NotContains(t, engine.deletedSnapshots, packageSnap)
	require.NotContains(t, engine.volumeInfos, S3MetadataVolumeName(sandboxID))

	gotCfg, err := os.ReadFile(filepath.Join(dir, "snapshot", "config.json"))
	require.NoError(t, err)
	require.Equal(t, `{"vm":"cfg"}`, string(gotCfg))
	gotState, err := os.ReadFile(filepath.Join(dir, "snapshot", "state.json"))
	require.NoError(t, err)
	require.Equal(t, `{"vm":"state"}`, string(gotState))

	require.NoError(t, ReleaseRollbackSnapshotMetadata(ctx, cow.BackendS3, sandboxID))
	require.NoDirExists(t, dir)
}

func TestMaterializeRollbackMetadataErrorsAndCleanup(t *testing.T) {
	cases := []struct {
		name      string
		cloneErr  error
		populate  func(t *testing.T, mountPath string)
		wantErr   string
		wantClone bool
	}{
		{
			name:     "clone failure cleans up dirs",
			cloneErr: errors.New("clone rpc timeout"),
			wantErr:  "clone rpc timeout",
		},
		{
			name:      "missing snapshot dir fails copy and cleans up",
			wantErr:   "no such file or directory",
			wantClone: true,
		},
		{
			name: "empty snapshot dir rejected before delete_vm",
			populate: func(t *testing.T, mountPath string) {
				require.NoError(t, os.MkdirAll(filepath.Join(mountPath, "snapshot"), 0o755))
			},
			wantErr:   "config.json",
			wantClone: true,
		},
		{
			name: "missing state.json rejected",
			populate: func(t *testing.T, mountPath string) {
				snapDir := filepath.Join(mountPath, "snapshot")
				require.NoError(t, os.MkdirAll(snapDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(snapDir, "config.json"), []byte(`{"ok":true}`), 0o644))
			},
			wantErr:   "state.json",
			wantClone: true,
		},
		{
			name: "null json payload rejected",
			populate: func(t *testing.T, mountPath string) {
				snapDir := filepath.Join(mountPath, "snapshot")
				require.NoError(t, os.MkdirAll(snapDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(snapDir, "config.json"), []byte(`null`), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(snapDir, "state.json"), []byte(`{"ok":true}`), 0o644))
			},
			wantErr:   "expected JSON object",
			wantClone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshotID := "snap-rb-err"
			sandboxID := "sb-rb-err"
			engine, packageSnap := newRollbackMaterializeFixture(t, snapshotID, sandboxID, tc.populate)
			engine.createVolumeFromSnapshotErr = tc.cloneErr

			dir, err := MaterializeRollbackSnapshotMetadata(context.Background(), cow.BackendS3, snapshotID, sandboxID)
			require.ErrorContains(t, err, tc.wantErr)
			require.Empty(t, dir)

			dst := rollbackMetadataMountPath(sandboxID)
			require.False(t, s3MetadataIsMounted(dst+".mnt"))
			require.NoDirExists(t, dst)
			require.NoDirExists(t, dst+".mnt")
			require.NotContains(t, engine.deletedSnapshots, packageSnap)
			if tc.wantClone {
				require.Contains(t, engine.deletedVolumes, S3MetadataVolumeName(RollbackMetadataOwnerID(sandboxID)))
			}
		})
	}

	t.Run("release unmounts and removes crash-leftover mnt", func(t *testing.T) {
		stubS3MetadataMounts(t)
		useTestCowStorageAt(t, &fakeCowEngine{})
		sandboxID := "sb-crash-leftover"
		dst := rollbackMetadataMountPath(sandboxID)
		mnt := dst + ".mnt"
		require.NoError(t, os.MkdirAll(mnt, 0o755))
		require.NoError(t, os.MkdirAll(dst, 0o755))
		require.NoError(t, mountS3MetadataDevice("/dev/nvme9n1", mnt))
		require.True(t, s3MetadataIsMounted(mnt))

		require.NoError(t, ReleaseRollbackSnapshotMetadata(context.Background(), cow.BackendS3, sandboxID))
		require.False(t, s3MetadataIsMounted(mnt))
		require.NoDirExists(t, mnt)
		require.NoDirExists(t, dst)
	})
}

func TestMkfsExt4PopulateProbe(t *testing.T) {
	t.Run("fails fast before base volume creation when unsupported", func(t *testing.T) {
		stubS3MetadataMounts(t)
		checkMkfsExt4PopulateSupport = func() error {
			return errors.New("mkfs.ext4 -d is not supported")
		}
		engine := &fakeCowEngine{createVolumePath: "/dev/mapper/" + S3MetadataBaseVolumeName}
		useTestCowStorage(t, engine)

		err := EnsureS3MetadataReady(context.Background())
		require.ErrorContains(t, err, "mkfs.ext4 -d")
		require.Empty(t, engine.createVolumes)
	})

	t.Run("real mkfs.ext4 -n -d probe caches success", func(t *testing.T) {
		if _, err := exec.LookPath("mkfs.ext4"); err != nil {
			t.Skip(err.Error())
		}
		mkfsExt4PopulateSupported.Store(false)
		t.Cleanup(func() { mkfsExt4PopulateSupported.Store(false) })
		require.NoError(t, checkMkfsExt4PopulateSupportImpl())

		origExec := execS3MetadataPack
		t.Cleanup(func() { execS3MetadataPack = origExec })
		execS3MetadataPack = func([]string, time.Duration) (string, string, error) {
			return "", "", errors.New("cached probe must not invoke mkfs.ext4 again")
		}
		require.NoError(t, checkMkfsExt4PopulateSupportImpl())
	})
}

func TestPackS3MetadataDeviceImplBuildsValidExt4Image(t *testing.T) {
	for _, bin := range []string{"mkfs.ext4", "e2fsck", "debugfs"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(err.Error())
		}
	}
	srcDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "snapshot"), 0o755))
	wantCatalog := `{"snapshot_id":"snap-real","metadata_vol":"s3-meta-snap-real-snap"}`
	wantConfig := `{"vm":"real-cfg"}`
	wantState := `{"vm":"real-state"}`
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "catalog.json"), []byte(wantCatalog), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "snapshot", "config.json"), []byte(wantConfig), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "snapshot", "state.json"), []byte(wantState), 0o644))

	imgPath := filepath.Join(t.TempDir(), "metadata.ext4")
	f, err := os.Create(imgPath)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(s3MetadataBaseSizeBytes))
	require.NoError(t, f.Close())

	require.NoError(t, packS3MetadataDeviceImpl(srcDir, imgPath))

	out, err := exec.Command("e2fsck", "-fn", imgPath).CombinedOutput()
	require.NoError(t, err, "e2fsck failed: %s", string(out))

	readFromImage := func(imagePath string) string {
		t.Helper()
		cmd := exec.Command("debugfs", "-R", "cat "+imagePath, imgPath)
		body, err := cmd.Output()
		require.NoError(t, err)
		return string(body)
	}
	require.Equal(t, wantCatalog, readFromImage("/catalog.json"))
	require.Equal(t, wantConfig, readFromImage("/snapshot/config.json"))
	require.Equal(t, wantState, readFromImage("/snapshot/state.json"))
}

func TestS3MetadataInvalidMountPaths(t *testing.T) {
	stubS3MetadataMounts(t)
	s3MetadataIsMounted = func(string) bool { t.Fatal("invalid path reached mount lookup"); return true }
	s3MetadataMountSource = func(string) string { t.Fatal("invalid path reached source lookup"); return "/dev/root" }
	unmountS3MetadataPath = func(string) error { t.Fatal("invalid path reached unmount"); return nil }
	cwd, err := os.Getwd()
	require.NoError(t, err)
	fixture := fmt.Sprintf("42 1 259:1 / %s rw - ext4 /dev/root rw\n", strings.ReplaceAll(cwd, " ", `\040`))
	for _, path := range []string{"", " ", ".", "./", "child/.."} {
		require.NoError(t, unmountS3MetadataPathIfMounted(path), path)
		require.False(t, s3MetadataIsMountedImpl(path), path)
		require.Empty(t, s3MetadataMountSourceFrom(strings.NewReader(fixture), path), path)
	}
}

func TestS3MetadataCleanupExcludesWritersAndRecovery(t *testing.T) {
	stubS3MetadataMounts(t)
	id := "tpl-cleaning"
	engine := &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		S3MetadataVolumeName(id): {SizeBytes: 8 << 20, DevicePath: "/dev/mapper/meta-cleaning"},
	}}
	useTestCowStorage(t, engine)
	release, err := BeginS3PackageCleanup(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	t.Cleanup(release)
	_, _, err = BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	require.ErrorContains(t, err, "active cleanup")
	_, err = BeginS3PackageCleanup(context.Background(), cow.BackendS3, id)
	require.ErrorContains(t, err, "active cleanup")
	dir := t.TempDir()
	require.NoError(t, MountS3MetadataForRecovery(context.Background(), cow.BackendS3, id, dir))
	require.Empty(t, engine.activatedVolumes)
	require.False(t, s3MetadataIsMounted(dir))
	_, otherRelease, err := BeginS3PackageWrite(context.Background(), cow.BackendS3, "other-package")
	require.NoError(t, err)
	otherRelease()
	release()
	release()
	_, writerRelease, err := BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	t.Cleanup(writerRelease)
	_, err = BeginS3PackageCleanup(context.Background(), cow.BackendS3, id)
	require.ErrorContains(t, err, "active write")
	writerRelease()
	require.NoError(t, MountS3MetadataForRecovery(context.Background(), cow.BackendS3, id, dir))
	require.True(t, s3MetadataIsMounted(dir))
}

func TestS3MetadataCleanupRequiresCurrentWriterContext(t *testing.T) {
	id := "tpl-owned-cleanup"
	ctx, release, err := BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	t.Cleanup(release)
	cleanupRelease, err := BeginS3PackageCleanup(ctx, cow.BackendS3, id)
	require.NoError(t, err)
	cleanupRelease()
	require.True(t, S3PackageOperationInProgress(id), "nested cleanup must not release the writer")
	_, err = BeginS3PackageCleanup(context.Background(), cow.BackendS3, id)
	require.ErrorContains(t, err, "retry later")

	_, otherRelease, err := BeginS3PackageWrite(context.Background(), cow.BackendS3, "another-package")
	require.NoError(t, err)
	t.Cleanup(otherRelease)
	_, err = BeginS3PackageCleanup(ctx, cow.BackendS3, "another-package")
	require.ErrorContains(t, err, "active write")

	release()
	_, nextRelease, err := BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	t.Cleanup(nextRelease)
	_, err = BeginS3PackageCleanup(ctx, cow.BackendS3, id)
	require.ErrorContains(t, err, "active write", "a stale context must not own the next writer")
}

func TestRollbackMetadataRejectsUnsafeSandboxID(t *testing.T) {
	stubS3MetadataMounts(t)
	engine := &fakeCowEngine{}
	useTestCowStorageAt(t, engine)

	for _, unsafeID := range []string{"../escape", "a/b", `a\b`, ".."} {
		require.Empty(t, rollbackMetadataMountPath(unsafeID), "unsafe sandboxID %q must yield empty path", unsafeID)
		_, err := MaterializeRollbackSnapshotMetadata(context.Background(), cow.BackendS3, "snap-1", unsafeID)
		require.ErrorContains(t, err, "invalid rollback metadata sandbox_id")
	}
	require.Empty(t, engine.createVolumes)
	require.Empty(t, engine.createVolumeFromSnapshots)

	// Place a sentinel file where an unvalidated "../victim" join would point
	// and verify ReleaseRollbackSnapshotMetadata rejects it without deleting.
	normalRoot := filepath.Dir(SnapshotHome(cow.BackendS3, SnapshotKindNormal, "anchor"))
	victimDir := filepath.Join(filepath.Dir(normalRoot), "victim", rollbackMetadataDirName)
	require.NoError(t, os.MkdirAll(victimDir, 0o755))
	sentinel := filepath.Join(victimDir, "keep.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o644))

	err := ReleaseRollbackSnapshotMetadata(context.Background(), cow.BackendS3, "../victim")
	require.ErrorContains(t, err, "invalid rollback metadata sandbox_id")
	require.FileExists(t, sentinel)
	require.Empty(t, engine.deletedVolumes)
}
