// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

const (
	runtimeBackendSandboxID = "running-sandbox"
	runtimeBackendNamespace = "runtime-backend-test"
	runtimeBackendOldVolume = "work"
	runtimeBackendNewVolume = "dataset"
)

// The tagged integration fixture observes the production backend boundary.
func prepareRuntimeVolumeBackendForTest(ctx context.Context, storage *local, req *volume.AttachRequest, publish func()) error {
	if _, err := storage.prepareRuntimeVolumeBackend(ctx, *req); err != nil {
		return err
	}
	publish()
	return nil
}

type runtimeBackendTestPlugin struct {
	volume.VolumePlugin
	name       string
	attached   []string
	lastAttach volume.AttachRequest
	results    map[string]*volume.AttachResult
	detached   []volume.DetachRequest
	hostPath   string
	attachErr  error
	detachErr  error
	onAttach   func()
	onDetach   func(context.Context)
}

func (p *runtimeBackendTestPlugin) Name() string { return p.name }

func (*runtimeBackendTestPlugin) Init(context.Context, volume.PluginConfig) error { return nil }

func (*runtimeBackendTestPlugin) Close() error { return nil }

func (p *runtimeBackendTestPlugin) Attach(_ context.Context, req *volume.AttachRequest) (*volume.AttachResult, error) {
	p.attached = append(p.attached, req.VolumeID)
	p.lastAttach = *req
	if p.attachErr != nil {
		return nil, p.attachErr
	}
	hostPath := filepath.Join(req.VolumeBaseDir, req.VolumeID)
	if err := os.MkdirAll(hostPath, 0700); err != nil {
		return nil, err
	}
	if p.hostPath != "" {
		hostPath = p.hostPath
	}
	result := &volume.AttachResult{
		VolumeID: req.VolumeID,
		HostPath: hostPath,
		Metadata: map[string]string{
			"cleanup-token": uuid.NewString(),
			"source":        "opaque/source:" + req.VolumeID,
		},
	}
	p.results[req.VolumeID] = result
	if p.onAttach != nil {
		p.onAttach()
	}
	return result, nil
}

func (p *runtimeBackendTestPlugin) Detach(ctx context.Context, req *volume.DetachRequest) error {
	p.detached = append(p.detached, *req)
	if p.onDetach != nil {
		p.onDetach(ctx)
	}
	return p.detachErr
}

type runtimeBackendFixture struct {
	root     string
	storage  *local
	holderDB *bolt.DB
	holders  *refcount.Store
	driver   *runtimeBackendTestPlugin
	initial  *StorageInfo
}

func newRuntimeBackendFixture(t *testing.T) *runtimeBackendFixture {
	t.Helper()
	f := &runtimeBackendFixture{
		root: t.TempDir(),
		driver: &runtimeBackendTestPlugin{
			name:    "test-runtime-backend-" + uuid.NewString(),
			results: make(map[string]*volume.AttachResult),
		},
	}
	manager := volume.Global()
	previousStore := manager.RefCountStore()
	manager.Register(f.driver)
	t.Cleanup(func() {
		manager.SetRefCountStore(previousStore)
		if f.holderDB != nil {
			require.NoError(t, f.holderDB.Close())
		}
		if f.storage != nil {
			require.NoError(t, f.storage.db.Close())
		}
	})
	f.reopenStorage(t, &bolt.Options{})
	f.reopenHolders(t)
	require.NoError(t, os.MkdirAll(f.storage.failoverDir(), 0700))

	oldResult, err := manager.Attach(t.Context(), f.request(runtimeBackendOldVolume))
	require.NoError(t, err)
	f.initial = &StorageInfo{
		SandboxID:    runtimeBackendSandboxID,
		Namespace:    runtimeBackendNamespace,
		InstanceType: "test-instance",
		CreateAt:     time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		UpdateAt:     time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC),
		TemplateID:   "test-template",
		Volumes:      map[string]*BackendFileInfo{"root": {Name: "root", FilePath: filepath.Join(f.root, "disk"), SizeLimit: 5}},
		PluginVolumeBackendInfos: map[string]*PluginVolumeBackendInfo{
			runtimeBackendOldVolume: {
				VolumeID: runtimeBackendOldVolume,
				Driver:   f.driver.Name(),
				HostPath: oldResult.HostPath,
				Metadata: oldResult.Metadata,
			},
		},
	}
	require.NoError(t, f.storage.writeBackendFileInfo(t.Context(), runtimeBackendSandboxID, f.initial))
	return f
}

func (f *runtimeBackendFixture) request(id string) *volume.AttachRequest {
	return &volume.AttachRequest{
		SandboxID:     runtimeBackendSandboxID,
		Namespace:     runtimeBackendNamespace,
		VolumeID:      id,
		Driver:        f.driver.Name(),
		VolumeBaseDir: f.storage.volumePluginBaseDir(),
	}
}

func (f *runtimeBackendFixture) reopenStorage(t *testing.T, options *bolt.Options) {
	t.Helper()
	if f.storage != nil {
		require.NoError(t, f.storage.db.Close())
		f.storage = nil
	}
	db, err := utils.NewCubeStore(filepath.Join(f.root, "storage.db"), options)
	require.NoError(t, err)
	f.storage = &local{
		db: db,
		config: &Config{
			RootPath:            f.root,
			VolumePluginBaseDir: filepath.Join(f.root, "plugin-backends"),
		},
	}
}

func (f *runtimeBackendFixture) reopenHolders(t *testing.T) {
	t.Helper()
	manager := volume.Global()
	if f.holderDB != nil {
		manager.SetRefCountStore(nil)
		require.NoError(t, f.holderDB.Close())
		f.holderDB = nil
	}
	db, err := bolt.Open(filepath.Join(f.root, "holders.db"), 0600, &bolt.Options{})
	require.NoError(t, err)
	f.holderDB = db
	f.holders, err = refcount.New(db)
	require.NoError(t, err)
	manager.SetRefCountStore(f.holders)
}

func (f *runtimeBackendFixture) requireMergedInfo(t *testing.T, info *StorageInfo) {
	t.Helper()
	result := f.driver.results[runtimeBackendNewVolume]
	require.NotNil(t, result)
	expected := *f.initial
	expected.PluginVolumeBackendInfos = map[string]*PluginVolumeBackendInfo{
		runtimeBackendOldVolume: f.initial.PluginVolumeBackendInfos[runtimeBackendOldVolume],
		runtimeBackendNewVolume: {
			VolumeID: runtimeBackendNewVolume,
			Driver:   f.driver.Name(),
			HostPath: result.HostPath,
			Metadata: result.Metadata,
		},
	}
	require.Equal(t, &expected, info)
}

func TestRuntimeVolumeBackendStorage(t *testing.T) {
	t.Run("publishes after metadata is readable", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		published := 0
		err := prepareRuntimeVolumeBackendForTest(t.Context(), f.storage, f.request(runtimeBackendNewVolume), func() {
			published++
			stored, err := f.storage.readBackendFileInfo(t.Context(), runtimeBackendSandboxID)
			require.NoError(t, err)
			f.requireMergedInfo(t, stored)
		})
		require.NoError(t, err)
		require.Equal(t, 1, published)
		require.Equal(t, []string{runtimeBackendOldVolume, runtimeBackendNewVolume}, f.driver.attached)
	})

	t.Run("fallback write leaves the old record visible", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			detachErr error
		}{
			{name: "successful cleanup"},
			{name: "unknown cleanup", detachErr: errors.New("backend detach failed")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newRuntimeBackendFixture(t)
				f.reopenStorage(t, &bolt.Options{ReadOnly: true})
				fallback := filepath.Join(f.storage.failoverDir(), runtimeBackendSandboxID)
				require.NoFileExists(t, fallback)
				oldBytes, err := f.storage.db.Get(bucketName, runtimeBackendSandboxID)
				require.NoError(t, err)
				require.ErrorIs(t, f.storage.db.Set(bucketName, runtimeBackendSandboxID, oldBytes), bolterrors.ErrDatabaseReadOnly)

				f.driver.detachErr = tc.detachErr
				f.driver.onDetach = func(ctx context.Context) {
					require.NoError(t, ctx.Err())
					backupBytes, err := os.ReadFile(fallback)
					require.NoError(t, err)
					var backup StorageInfo
					require.NoError(t, json.Unmarshal(backupBytes, &backup))
					f.requireMergedInfo(t, &backup)
				}
				published := 0
				err = prepareRuntimeVolumeBackendForTest(t.Context(), f.storage, f.request(runtimeBackendNewVolume), func() {
					published++
				})
				require.Zero(t, published)
				require.ErrorContains(t, err, "storage read-back did not confirm plugin volume \"dataset\"")
				require.Equal(t, []string{runtimeBackendOldVolume, runtimeBackendNewVolume}, f.driver.attached)
				require.Equal(t, []volume.DetachRequest{{
					SandboxID: runtimeBackendSandboxID, Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendNewVolume,
					Driver: f.driver.Name(), Metadata: f.driver.results[runtimeBackendNewVolume].Metadata, RequireRecordedHolder: true,
				}}, f.driver.detached)

				stored, err := f.storage.readBackendFileInfo(t.Context(), runtimeBackendSandboxID)
				require.NoError(t, err)
				require.Equal(t, f.initial, stored)
				scanned, err := f.storage.readAllFileInfo()
				require.NoError(t, err)
				holders, err := f.holders.ListForSandbox(runtimeBackendSandboxID)
				require.NoError(t, err)
				newHolder := refcount.SandboxRecord{Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendNewVolume, RefCount: 1}
				if tc.detachErr == nil {
					require.NoFileExists(t, fallback)
					require.Equal(t, oldBytes, scanned[runtimeBackendSandboxID])
					require.NotContains(t, holders, newHolder)
				} else {
					require.FileExists(t, fallback)
					var backup StorageInfo
					require.NoError(t, json.Unmarshal(scanned[runtimeBackendSandboxID], &backup))
					f.requireMergedInfo(t, &backup)
					require.Contains(t, holders, newHolder)
				}
			})
		}
	})

	t.Run("destroy preserves conflicting plugin cleanup records after reopen", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		rootFile := f.initial.Volumes["root"].FilePath
		require.NoError(t, os.WriteFile(rootFile, []byte("live rootfs"), 0600))
		f.reopenStorage(t, &bolt.Options{ReadOnly: true})
		f.driver.detachErr = errors.New("backend detach failed")
		_, err := f.storage.prepareRuntimeVolumeBackend(t.Context(), *f.request(runtimeBackendNewVolume))
		require.ErrorContains(t, err, "backend detach failed")

		f.reopenStorage(t, &bolt.Options{})
		f.reopenHolders(t)
		f.driver.detachErr = nil
		f.driver.detached = nil
		fallback := filepath.Join(f.storage.failoverDir(), runtimeBackendSandboxID)
		oldBolt, err := f.storage.db.Get(bucketName, runtimeBackendSandboxID)
		require.NoError(t, err)
		oldFallback, err := os.ReadFile(fallback)
		require.NoError(t, err)
		holders, err := f.holders.ListForSandbox(runtimeBackendSandboxID)
		require.NoError(t, err)
		require.Len(t, holders, 2)

		err = f.storage.Destroy(t.Context(), &workflow.DestroyContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: runtimeBackendSandboxID},
		})
		require.ErrorContains(t, err, "storage fallback contains plugin volume")
		rerr, _ := ret.FromError(err)
		require.Equal(t, errorcode.ErrorCode_DestroyStorageFailed, rerr.Code())
		require.Empty(t, f.driver.detached)
		require.FileExists(t, rootFile)
		gotBolt, err := f.storage.db.Get(bucketName, runtimeBackendSandboxID)
		require.NoError(t, err)
		require.Equal(t, oldBolt, gotBolt)
		gotFallback, err := os.ReadFile(fallback)
		require.NoError(t, err)
		require.Equal(t, oldFallback, gotFallback)
		gotHolders, err := f.holders.ListForSandbox(runtimeBackendSandboxID)
		require.NoError(t, err)
		require.ElementsMatch(t, holders, gotHolders)
	})

	t.Run("destroy uses newer Bolt cleanup metadata for the same volume", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		oldBolt, err := f.storage.db.Get(bucketName, runtimeBackendSandboxID)
		require.NoError(t, err)
		fallback := filepath.Join(f.storage.failoverDir(), runtimeBackendSandboxID)
		require.NoError(t, os.WriteFile(fallback, oldBolt, 0600))
		metadata := map[string]string{"cleanup-token": "updated"}
		f.initial.PluginVolumeBackendInfos[runtimeBackendOldVolume].Metadata = metadata
		require.NoError(t, f.storage.writeBackendFileInfo(t.Context(), runtimeBackendSandboxID, f.initial))

		require.NoError(t, f.storage.Destroy(t.Context(), &workflow.DestroyContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: runtimeBackendSandboxID},
		}))
		require.Len(t, f.driver.detached, 1)
		require.Equal(t, runtimeBackendOldVolume, f.driver.detached[0].VolumeID)
		require.Equal(t, f.driver.Name(), f.driver.detached[0].Driver)
		require.Equal(t, metadata, f.driver.detached[0].Metadata)
	})

	t.Run("destroy uses the persisted original metadata after reopen", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		require.NoError(t, prepareRuntimeVolumeBackendForTest(t.Context(), f.storage, f.request(runtimeBackendNewVolume), func() {}))
		f.reopenStorage(t, &bolt.Options{})
		f.reopenHolders(t)
		holders, err := f.holders.ListForSandbox(runtimeBackendSandboxID)
		require.NoError(t, err)
		require.ElementsMatch(t, []refcount.SandboxRecord{
			{Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendOldVolume, RefCount: 1},
			{Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendNewVolume, RefCount: 1},
		}, holders)

		require.NoError(t, f.storage.Destroy(t.Context(), &workflow.DestroyContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: runtimeBackendSandboxID},
		}))
		require.ElementsMatch(t, []volume.DetachRequest{
			{SandboxID: runtimeBackendSandboxID, Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendOldVolume,
				Driver: f.driver.Name(), Metadata: f.driver.results[runtimeBackendOldVolume].Metadata},
			{SandboxID: runtimeBackendSandboxID, Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendNewVolume,
				Driver: f.driver.Name(), Metadata: f.driver.results[runtimeBackendNewVolume].Metadata},
		}, f.driver.detached)
		require.Equal(t, []string{runtimeBackendOldVolume, runtimeBackendNewVolume}, f.driver.attached)
		holders, err = f.holders.ListForSandbox(runtimeBackendSandboxID)
		require.NoError(t, err)
		require.Empty(t, holders)
		for _, id := range []string{runtimeBackendOldVolume, runtimeBackendNewVolume} {
			count, err := f.holders.Get(runtimeBackendNamespace, id)
			require.NoError(t, err)
			require.Zero(t, count)
		}
		_, err = f.storage.readBackendFileInfoRaw(t.Context(), runtimeBackendSandboxID)
		require.ErrorIs(t, err, utils.ErrorKeyNotFound)
	})
}

func TestRuntimeVolumeBackendFailures(t *testing.T) {
	t.Run("invalid path with confirmed cleanup", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		f.driver.hostPath = filepath.Join(f.root, "outside-base")
		attachCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.driver.onAttach = cancel
		f.driver.onDetach = func(ctx context.Context) { require.NoError(t, ctx.Err()) }
		_, err := f.storage.prepareRuntimeVolumeBackend(attachCtx, *f.request(runtimeBackendNewVolume))
		require.ErrorContains(t, err, "must be a subdirectory")
		require.Equal(t, []volume.DetachRequest{{
			SandboxID: runtimeBackendSandboxID, Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendNewVolume,
			Driver: f.driver.Name(), Metadata: f.driver.results[runtimeBackendNewVolume].Metadata, RequireRecordedHolder: true,
		}}, f.driver.detached)
		stored, err := f.storage.readBackendFileInfo(t.Context(), runtimeBackendSandboxID)
		require.NoError(t, err)
		require.Equal(t, f.initial, stored)
		_, held, err := f.holders.GetRecord(runtimeBackendNamespace, runtimeBackendNewVolume)
		require.NoError(t, err)
		require.False(t, held)
	})

	t.Run("invalid path with unknown cleanup", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		f.driver.hostPath = filepath.Join(f.root, "outside-base")
		f.driver.detachErr = errors.New("backend detach failed")
		_, err := f.storage.prepareRuntimeVolumeBackend(t.Context(), *f.request(runtimeBackendNewVolume))
		require.ErrorIs(t, err, f.driver.detachErr)
		stored, err := f.storage.readBackendFileInfo(t.Context(), runtimeBackendSandboxID)
		require.NoError(t, err)
		f.requireMergedInfo(t, stored)
		record, _, err := f.holders.GetRecord(runtimeBackendNamespace, runtimeBackendNewVolume)
		require.NoError(t, err)
		require.Contains(t, record.SandboxIDs, runtimeBackendSandboxID)
	})

	t.Run("failed Bolt and fallback writes with unknown cleanup", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		f.reopenStorage(t, &bolt.Options{ReadOnly: true})
		require.NoError(t, os.Remove(f.storage.failoverDir()))
		f.driver.detachErr = errors.New("backend detach failed")

		_, err := f.storage.prepareRuntimeVolumeBackend(t.Context(), *f.request(runtimeBackendNewVolume))
		require.ErrorIs(t, err, bolterrors.ErrDatabaseReadOnly)
		require.ErrorIs(t, err, f.driver.detachErr)
		require.Len(t, f.driver.detached, 1)
		require.NoFileExists(t, filepath.Join(f.storage.failoverDir(), runtimeBackendSandboxID))
		stored, err := f.storage.readBackendFileInfo(t.Context(), runtimeBackendSandboxID)
		require.NoError(t, err)
		require.Equal(t, f.initial, stored)
		record, _, err := f.holders.GetRecord(runtimeBackendNamespace, runtimeBackendNewVolume)
		require.NoError(t, err)
		require.Contains(t, record.SandboxIDs, runtimeBackendSandboxID)
	})

	t.Run("unknown plugin attach is not detached", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		f.driver.attachErr = errors.New("lost plugin reply")
		_, err := f.storage.prepareRuntimeVolumeBackend(t.Context(), *f.request(runtimeBackendNewVolume))
		require.ErrorIs(t, err, f.driver.attachErr)
		require.Equal(t, []string{runtimeBackendOldVolume, runtimeBackendNewVolume}, f.driver.attached)
		require.Empty(t, f.driver.detached)
		stored, err := f.storage.readBackendFileInfo(t.Context(), runtimeBackendSandboxID)
		require.NoError(t, err)
		require.Equal(t, f.initial, stored)
	})

	t.Run("existing fallback blocks side effects", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		fallback := filepath.Join(f.storage.failoverDir(), runtimeBackendSandboxID)
		previous := []byte("previous cleanup evidence")
		require.NoError(t, os.WriteFile(fallback, previous, 0600))
		_, err := f.storage.prepareRuntimeVolumeBackend(t.Context(), *f.request(runtimeBackendNewVolume))
		require.ErrorContains(t, err, "pending fallback")
		data, err := os.ReadFile(fallback)
		require.NoError(t, err)
		require.Equal(t, previous, data)
		require.Equal(t, []string{runtimeBackendOldVolume}, f.driver.attached)
	})

	t.Run("holder without a storage entry blocks side effects", func(t *testing.T) {
		f := newRuntimeBackendFixture(t)
		_, err := f.holders.Acquire(runtimeBackendNamespace, runtimeBackendNewVolume, runtimeBackendSandboxID, f.driver.Name())
		require.NoError(t, err)
		_, err = f.storage.prepareRuntimeVolumeBackend(t.Context(), *f.request(runtimeBackendNewVolume))
		require.ErrorContains(t, err, "already holds")
		require.Equal(t, []string{runtimeBackendOldVolume}, f.driver.attached)
		require.Empty(t, f.driver.detached)
	})
}

func TestCreatePluginVolumeCleanupAfterPathRejection(t *testing.T) {
	f := newRuntimeBackendFixture(t)
	f.driver.hostPath = filepath.Join(f.root, "outside-base")
	ctx := namespaces.WithNamespace(t.Context(), runtimeBackendNamespace)
	id := runtimeBackendSandboxID + "-create"
	create := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: id},
		ReqInfo: &cubebox.RunCubeSandboxRequest{Annotations: map[string]string{
			cubebox.AnnotationPluginVolumeSources: fmt.Sprintf(`[{"name":%q,"driver":%q}]`, runtimeBackendNewVolume, f.driver.Name()),
		}},
	}
	info := &StorageInfo{SandboxID: id, Namespace: runtimeBackendNamespace}
	err := f.storage.attachPluginVolume(ctx, create, &cubebox.Volume{Name: runtimeBackendNewVolume}, info)
	require.ErrorContains(t, err, "must be a subdirectory")
	require.Equal(t, []string{runtimeBackendNewVolume}, create.PluginVolumeIDs)
	require.False(t, create.PluginVolumeAttachUncertain)
	require.Equal(t, f.driver.results[runtimeBackendNewVolume].Metadata, info.PluginVolumeBackendInfos[runtimeBackendNewVolume].Metadata)

	require.NoError(t, f.storage.cleanupCreateResult(ctx, info, create))
	require.Equal(t, []volume.DetachRequest{{
		SandboxID: id, Namespace: runtimeBackendNamespace, VolumeID: runtimeBackendNewVolume,
		Driver: f.driver.Name(), Metadata: f.driver.results[runtimeBackendNewVolume].Metadata, RequireRecordedHolder: true,
	}}, f.driver.detached)
	_, held, err := f.holders.GetRecord(runtimeBackendNamespace, runtimeBackendNewVolume)
	require.NoError(t, err)
	require.False(t, held)
}
