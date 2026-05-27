// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
)

type fakeCowEngine struct {
	createVolumePath          string
	createVolumeErr           error
	createVolumes             []string
	createVolumeSizes         map[string]uint64
	createSnapshots           [][2]string
	createSnapshotActivations []bool
	createSnapshotPath        string
	createSnapshotErr         error
	activatePaths             map[string]string
	activatedVolumes          []string
	deactivatedVolumes        []string
	activateErr               error
	deactivateErr             error
	volumeInfos               map[string]*cubecow.Volume
	listSnapshots             map[string][]cubecow.Snapshot
	listSnapshotsErr          error
	deletedVolumes            []string
	deletedSnapshots          []string
	deleteVolumeErr           error
	deleteSnapshotErr         error
	resizeErr                 error
	resizedVolumes            map[string]uint64
	metrics                   map[string]uint64
	metricsErr                error
}

func (f *fakeCowEngine) CreateVolume(name string, sizeBytes uint64) (string, error) {
	f.createVolumes = append(f.createVolumes, name)
	if f.createVolumeSizes == nil {
		f.createVolumeSizes = map[string]uint64{}
	}
	f.createVolumeSizes[name] = sizeBytes
	if f.volumeInfos == nil {
		f.volumeInfos = map[string]*cubecow.Volume{}
	}
	if f.createVolumeErr == nil {
		if _, ok := f.volumeInfos[name]; !ok {
			f.volumeInfos[name] = &cubecow.Volume{DevicePath: f.createVolumePath, SizeBytes: sizeBytes}
		}
	}
	return f.createVolumePath, f.createVolumeErr
}

func (f *fakeCowEngine) CreateSnapshot(sourceName, snapshotName string, activate bool) (string, error) {
	f.createSnapshots = append(f.createSnapshots, [2]string{sourceName, snapshotName})
	f.createSnapshotActivations = append(f.createSnapshotActivations, activate)
	return f.createSnapshotPath, f.createSnapshotErr
}

func (f *fakeCowEngine) ActivateVolume(name string) (string, error) {
	f.activatedVolumes = append(f.activatedVolumes, name)
	if f.activateErr != nil {
		return "", f.activateErr
	}
	path := "/dev/mapper/" + name
	if f.activatePaths != nil && f.activatePaths[name] != "" {
		path = f.activatePaths[name]
	}
	if f.volumeInfos == nil {
		f.volumeInfos = map[string]*cubecow.Volume{}
	}
	info := f.volumeInfos[name]
	if info == nil {
		info = &cubecow.Volume{}
		f.volumeInfos[name] = info
	}
	info.DevicePath = path
	return path, nil
}

func (f *fakeCowEngine) DeactivateVolume(name string) error {
	f.deactivatedVolumes = append(f.deactivatedVolumes, name)
	if f.deactivateErr != nil {
		return f.deactivateErr
	}
	if f.volumeInfos != nil && f.volumeInfos[name] != nil {
		f.volumeInfos[name].DevicePath = ""
	}
	return nil
}

func (f *fakeCowEngine) DeleteVolume(name string) error {
	f.deletedVolumes = append(f.deletedVolumes, name)
	return f.deleteVolumeErr
}

func (f *fakeCowEngine) DeleteSnapshot(name string) error {
	f.deletedSnapshots = append(f.deletedSnapshots, name)
	return f.deleteSnapshotErr
}

func (f *fakeCowEngine) ResizeVolume(name string, newSizeBytes uint64) (uint64, uint64, error) {
	if f.resizeErr != nil {
		return 0, 0, f.resizeErr
	}
	if f.resizedVolumes == nil {
		f.resizedVolumes = map[string]uint64{}
	}
	f.resizedVolumes[name] = newSizeBytes
	if f.volumeInfos == nil {
		f.volumeInfos = map[string]*cubecow.Volume{}
	}
	info := f.volumeInfos[name]
	if info == nil {
		info = &cubecow.Volume{DevicePath: "/dev/mapper/" + name}
		f.volumeInfos[name] = info
	}
	oldSize := info.SizeBytes
	info.SizeBytes = newSizeBytes
	return oldSize, newSizeBytes, nil
}

func (f *fakeCowEngine) GetVolumeInfo(name string) (*cubecow.Volume, error) {
	if f.volumeInfos == nil {
		return nil, nil
	}
	return f.volumeInfos[name], nil
}

func (f *fakeCowEngine) ListSnapshots(volumeName string, pageSize uint64, pageToken string) (*cubecow.ListSnapshotsResult, error) {
	_ = pageSize
	_ = pageToken
	if f.listSnapshotsErr != nil {
		return nil, f.listSnapshotsErr
	}
	return &cubecow.ListSnapshotsResult{Snapshots: f.listSnapshots[volumeName]}, nil
}

func (f *fakeCowEngine) GetMetrics() (map[string]uint64, error) {
	if f.metricsErr != nil {
		return nil, f.metricsErr
	}
	if f.metrics == nil {
		return map[string]uint64{}, nil
	}
	return f.metrics, nil
}

func stubInitDefaultMediumDevice(t *testing.T, fn func(string) error) {
	t.Helper()
	previousInit := initDefaultMediumDevice
	initDefaultMediumDevice = fn
	t.Cleanup(func() {
		initDefaultMediumDevice = previousInit
	})
}

func TestCreateDefaultMediumVolumeFormatsNewVolume(t *testing.T) {
	engine := &fakeCowEngine{createVolumePath: "/dev/mapper/sb-sb1-data"}
	manager := &CowVolumeManager{engine: engine}

	var formatted []string
	stubInitDefaultMediumDevice(t, func(devicePath string) error {
		formatted = append(formatted, devicePath)
		return nil
	})

	volume, err := manager.CreateDefaultMediumVolume(context.Background(), "sb1", "data", 1024)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, []string{"/dev/mapper/sb-sb1-data"}, formatted)
	assert.Empty(t, engine.deletedVolumes)
	assert.Equal(t, "sb-sb1-data", volume.VolumeName)
	assert.Equal(t, cowKindVolume, volume.Kind)
}

func TestCreateDefaultMediumVolumeSkipsFormatForExistingVolume(t *testing.T) {
	engine := &fakeCowEngine{
		createVolumeErr: &cubecow.CowError{Code: cubecow.SemAlreadyExists, RawRC: int32(cubecow.SemAlreadyExists)},
		volumeInfos: map[string]*cubecow.Volume{
			"sb-sb1-data": {DevicePath: "/dev/mapper/existing"},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	var formatted []string
	stubInitDefaultMediumDevice(t, func(devicePath string) error {
		formatted = append(formatted, devicePath)
		return nil
	})

	volume, err := manager.CreateDefaultMediumVolume(context.Background(), "sb1", "data", 1024)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Empty(t, formatted)
	assert.Empty(t, engine.deletedVolumes)
	assert.Equal(t, "/dev/mapper/existing", volume.FilePath)
}

func TestCreateDefaultMediumVolumeCleansUpWhenFormatFails(t *testing.T) {
	engine := &fakeCowEngine{createVolumePath: "/dev/mapper/sb-sb1-data"}
	manager := &CowVolumeManager{engine: engine}

	stubInitDefaultMediumDevice(t, func(devicePath string) error {
		_ = devicePath
		return errors.New("format failed")
	})

	volume, err := manager.CreateDefaultMediumVolume(context.Background(), "sb1", "data", 1024)
	require.Nil(t, volume)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initialize cubecow default medium")
	assert.Equal(t, []string{"sb-sb1-data"}, engine.deletedVolumes)
}

func TestCreateTemplateBuildRootfsFormatsNewVolume(t *testing.T) {
	engine := &fakeCowEngine{createVolumePath: "/dev/mapper/tpl-tpl1-build-rootfs"}
	manager := &CowVolumeManager{engine: engine}

	var formatted []string
	stubInitDefaultMediumDevice(t, func(devicePath string) error {
		formatted = append(formatted, devicePath)
		return nil
	})

	volume, err := manager.CreateTemplateBuildRootfs(context.Background(), "tpl1", 1024)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, []string{"tpl-tpl1-build-rootfs"}, engine.createVolumes)
	assert.Equal(t, []string{"/dev/mapper/tpl-tpl1-build-rootfs"}, formatted)
	assert.Equal(t, "tpl-tpl1-build-rootfs", volume.VolumeName)
	assert.Equal(t, cowKindVolume, volume.Kind)
}

func TestCommitTemplateRootfsCreatesSnapshotKind(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-rootfs": {SizeBytes: 1024},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.CommitTemplateRootfs(context.Background(), "sb-sandbox-rootfs-gen3", "snap")
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, [][2]string{{"sb-sandbox-rootfs-gen3", "tpl-snap-rootfs"}}, engine.createSnapshots)
	assert.Equal(t, []bool{false}, engine.createSnapshotActivations)
	assert.Equal(t, "tpl-snap-rootfs", volume.VolumeName)
	assert.Equal(t, cowKindSnapshot, volume.Kind)
	assert.Empty(t, volume.FilePath)
}

func TestCommitTemplateMemoryProducesActivatedSnapshot(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/tpl-snap-memory",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-memory": {DevicePath: "/dev/mapper/tpl-snap-memory", SizeBytes: 4096},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.CommitTemplateMemory(context.Background(), "tpl-base-memory", "snap", 4096)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, [][2]string{{"tpl-base-memory", "tpl-snap-memory"}}, engine.createSnapshots)
	// The cloned memory snapshot is the base of subsequent incremental
	// writes, so it must be activated (callers immediately stream pages
	// into the returned dev path).
	assert.Equal(t, []bool{true}, engine.createSnapshotActivations)
	assert.Equal(t, "tpl-snap-memory", volume.VolumeName)
	assert.Equal(t, cowKindSnapshot, volume.Kind)
	assert.Equal(t, "/dev/mapper/tpl-snap-memory", volume.FilePath)
}

func TestCommitTemplateMemoryRejectsAlreadyExists(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotErr: &cubecow.CowError{Code: cubecow.SemAlreadyExists, RawRC: int32(cubecow.SemAlreadyExists)},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.CommitTemplateMemory(context.Background(), "tpl-base-memory", "snap", 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCowObjectAlreadyExists)
	assert.Nil(t, volume)
	// The snapshot was never produced, so we must NOT issue a follow-up
	// DeleteSnapshot that would erase whatever the existing tpl-snap-memory
	// is actually backing.
	assert.Empty(t, engine.deletedSnapshots)
}

func TestCommitTemplateMemoryCleansUpWhenClonedSizeTooSmall(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/tpl-snap-memory",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-memory": {DevicePath: "/dev/mapper/tpl-snap-memory", SizeBytes: 512},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.CommitTemplateMemory(context.Background(), "tpl-base-memory", "snap", 4096)
	require.Error(t, err)
	assert.Nil(t, volume)
	assert.Contains(t, err.Error(), "is smaller than requested")
	// Hard-clean the clone so a retry doesn't trip the AlreadyExists guard
	// against a half-baked snapshot we just produced.
	assert.Equal(t, []string{"tpl-snap-memory"}, engine.deletedSnapshots)
}

func TestCreateMemoryVolumeUsesVolumeKind(t *testing.T) {
	engine := &fakeCowEngine{createVolumePath: "/dev/mapper/tpl-snap-memory"}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.CreateMemoryVolume(context.Background(), "snap", 1024)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, []string{"tpl-snap-memory"}, engine.createVolumes)
	assert.Equal(t, "tpl-snap-memory", volume.VolumeName)
	assert.Equal(t, cowKindVolume, volume.Kind)
	assert.Equal(t, "/dev/mapper/tpl-snap-memory", volume.FilePath)
}

func TestTemplateArtifactsRejectAlreadyExists(t *testing.T) {
	alreadyExists := &cubecow.CowError{Code: cubecow.SemAlreadyExists, RawRC: int32(cubecow.SemAlreadyExists)}

	t.Run("build rootfs", func(t *testing.T) {
		engine := &fakeCowEngine{createVolumeErr: alreadyExists}
		manager := &CowVolumeManager{engine: engine}

		volume, err := manager.CreateTemplateBuildRootfs(context.Background(), "snap", 1024)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrCowObjectAlreadyExists))
		assert.Nil(t, volume)
		assert.Empty(t, engine.deletedVolumes)
	})

	t.Run("rootfs snapshot", func(t *testing.T) {
		engine := &fakeCowEngine{createSnapshotErr: alreadyExists}
		manager := &CowVolumeManager{engine: engine}

		volume, err := manager.CommitTemplateRootfs(context.Background(), "sb-sandbox-rootfs-gen1", "snap")
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrCowObjectAlreadyExists))
		assert.Nil(t, volume)
		assert.Empty(t, engine.deletedSnapshots)
	})

	t.Run("memory volume", func(t *testing.T) {
		engine := &fakeCowEngine{createVolumeErr: alreadyExists}
		manager := &CowVolumeManager{engine: engine}

		volume, err := manager.CreateMemoryVolume(context.Background(), "snap", 1024)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrCowObjectAlreadyExists))
		assert.Nil(t, volume)
		assert.Empty(t, engine.deletedVolumes)
	})
}

func TestCreateMemoryVolumeResizesSmallNewVolume(t *testing.T) {
	engine := &fakeCowEngine{
		createVolumePath: "/dev/mapper/tpl-snap-memory",
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-memory": {DevicePath: "/dev/mapper/tpl-snap-memory", SizeBytes: 512},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.CreateMemoryVolume(context.Background(), "snap", 1024)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, uint64(1024), engine.resizedVolumes["tpl-snap-memory"])
	assert.Empty(t, engine.deletedVolumes)
}

func TestCreateMemoryVolumeCleansUpWhenResizeFails(t *testing.T) {
	engine := &fakeCowEngine{
		createVolumePath: "/dev/mapper/tpl-snap-memory",
		resizeErr:        errors.New("resize failed"),
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-memory": {DevicePath: "/dev/mapper/tpl-snap-memory", SizeBytes: 512},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.CreateMemoryVolume(context.Background(), "snap", 1024)
	require.Error(t, err)
	assert.Nil(t, volume)
	assert.Contains(t, err.Error(), "resize cubecow volume")
	assert.Equal(t, []string{"tpl-snap-memory"}, engine.deletedVolumes)
}

func TestRollbackDeriveNewGenResizesSmallSnapshot(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotPath: "/dev/mapper/sb-sandbox-rootfs-gen2",
		volumeInfos: map[string]*cubecow.Volume{
			"sb-sandbox-rootfs-gen2": {DevicePath: "/dev/mapper/sb-sandbox-rootfs-gen2", SizeBytes: 1024},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.RollbackDeriveNewGen(context.Background(), "sandbox", "tpl-snap-rootfs", 2, 2048)
	require.NoError(t, err)
	require.NotNil(t, volume)

	assert.Equal(t, [][2]string{{"tpl-snap-rootfs", "sb-sandbox-rootfs-gen2"}}, engine.createSnapshots)
	assert.Equal(t, []bool{true}, engine.createSnapshotActivations)
	assert.Equal(t, uint64(2048), engine.resizedVolumes["sb-sandbox-rootfs-gen2"])
	assert.Equal(t, "sb-sandbox-rootfs-gen2", volume.VolumeName)
	assert.Equal(t, cowKindSnapshot, volume.Kind)
	assert.Equal(t, uint32(2), volume.Gen)
}

func TestResolveDevPathActivatesInactiveObject(t *testing.T) {
	engine := &fakeCowEngine{
		volumeInfos: map[string]*cubecow.Volume{
			"tpl-snap-memory": {DevicePath: ""},
		},
		activatePaths: map[string]string{
			"tpl-snap-memory": "/dev/mapper/tpl-snap-memory",
		},
	}
	manager := &CowVolumeManager{engine: engine}

	devPath, err := manager.ResolveDevPath(context.Background(), "tpl-snap-memory", cowKindVolume)
	require.NoError(t, err)
	assert.Equal(t, "/dev/mapper/tpl-snap-memory", devPath)
	assert.Equal(t, []string{"tpl-snap-memory"}, engine.activatedVolumes)
}

func TestDeactivateByKindCallsEngine(t *testing.T) {
	engine := &fakeCowEngine{}
	manager := &CowVolumeManager{engine: engine}

	require.NoError(t, manager.DeactivateByKind(context.Background(), "tpl-snap-memory", cowKindVolume))
	assert.Equal(t, []string{"tpl-snap-memory"}, engine.deactivatedVolumes)
}

func TestRollbackDeriveNewGenAcceptsExistingSameOrigin(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotErr: &cubecow.CowError{Code: cubecow.SemAlreadyExists, RawRC: int32(cubecow.SemAlreadyExists)},
		volumeInfos: map[string]*cubecow.Volume{
			"sb-sandbox-rootfs-gen2": {DevicePath: "/dev/mapper/existing", SizeBytes: 4096},
		},
		listSnapshots: map[string][]cubecow.Snapshot{
			"tpl-snap-rootfs": {
				{Name: "sb-sandbox-rootfs-gen2", OriginVolume: "tpl-snap-rootfs", SizeBytes: 4096, DevicePath: "/dev/mapper/existing"},
			},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.RollbackDeriveNewGen(context.Background(), "sandbox", "tpl-snap-rootfs", 2, 4096)
	require.NoError(t, err)
	require.NotNil(t, volume)
	assert.Equal(t, "/dev/mapper/existing", volume.FilePath)
}

func TestRollbackDeriveNewGenRejectsExistingDifferentOrigin(t *testing.T) {
	engine := &fakeCowEngine{
		createSnapshotErr: &cubecow.CowError{Code: cubecow.SemAlreadyExists, RawRC: int32(cubecow.SemAlreadyExists)},
		listSnapshots: map[string][]cubecow.Snapshot{
			"tpl-snap-rootfs": {
				{Name: "other", OriginVolume: "tpl-snap-rootfs"},
			},
		},
	}
	manager := &CowVolumeManager{engine: engine}

	volume, err := manager.RollbackDeriveNewGen(context.Background(), "sandbox", "tpl-snap-rootfs", 2, 4096)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCowObjectAlreadyExists)
	assert.Nil(t, volume)
}
