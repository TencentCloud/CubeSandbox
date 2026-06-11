// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func makeTestConfig(t *testing.T) *Config {
	testDir := t.TempDir()

	return &Config{
		RootPath:                  filepath.Join(testDir, "root"),
		DataPath:                  filepath.Join(testDir, "data"),
		PoolType:                  cp_type,
		DiskSize:                  "10Mi",
		WarningPercent:            200,
		PoolDefaultFormatSizeList: []string{"1Mi"},
		PoolSize:                  4,
		PoolWorkers:               2,
		PoolTriggerIntervalInMs:   1,
	}
}

func TestParam(t *testing.T) {
	cfg := makeTestConfig(t)

	s := &local{}
	s.config = cfg
	s.config.PoolWorkers = -1
	s.config.PoolTriggerIntervalInMs = -1
	s.config.WarningPercent = -1
	s.config.PoolWorkers = -1
	s.config.PoolDefaultFormatSizeList = nil
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	s.config.DiskSize = "kk"
	assert.Error(t, s.init(&plugin.InitContext{Context: context.Background()}))

	s.config.PoolDefaultFormatSizeList = []string{"kk"}
	assert.Error(t, s.init(&plugin.InitContext{Context: context.Background()}))

}

func TestCreateDestroy(t *testing.T) {
	cfg := makeTestConfig(t)

	s := &local{}
	s.config = cfg
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{
			{
				Name: "test",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						SizeLimit: "1Mi",
					},
				},
			},
		},
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test",
		},
		ReqInfo: req,
	}

	err := s.Create(ctx, opts)
	assert.NoError(t, err)
	require.NotNil(t, opts.StorageInfo)
	res := opts.StorageInfo.(*StorageInfo)

	require.Len(t, res.Volumes, 1)
	filePath := res.Volumes["test"].FilePath

	exist, _ := utils.DenExist(filePath)
	assert.True(t, exist)
	assert.Equal(t, 1024*1024+diskSizeExtendInBytes, int(res.Volumes["test"].FSQuota))

	dOpts := &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test",
		},
	}
	assert.NoError(t, s.Destroy(ctx, dOpts))

	exist, _ = utils.DenExist(filePath)
	assert.False(t, exist)

	assert.NoError(t, s.Destroy(ctx, dOpts))

	assert.Error(t, s.Destroy(ctx, nil))
}
func TestCreateDestroyInvalidVolume(t *testing.T) {
	cfg := makeTestConfig(t)

	s := &local{}
	s.config = cfg
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)

	dOpts := &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: ".",
		},
	}
	assert.NoError(t, s.Destroy(ctx, dOpts))
}

type noopPool struct{}

func (noopPool) Get(context.Context, int64) (*devInfo, error)     { return nil, nil }
func (noopPool) GetSync(context.Context, int64) (*devInfo, error) { return nil, nil }
func (noopPool) Close()                                           {}
func (noopPool) InitBaseFile(context.Context) error               { return nil }

type fakeCowCreateDefaultCall struct {
	sandboxID  string
	volumeName string
	sizeBytes  uint64
}

type fakeCowCreateSnapshotCall struct {
	sandboxID        string
	templateID       string
	gen              uint32
	desiredSizeBytes uint64
}

type fakeCowCreateBuildRootfsCall struct {
	templateID string
	sizeBytes  uint64
}

type fakeCowCommitTemplateRootfsCall struct {
	sourceName string
	templateID string
}

type fakeCowCreateMemoryCall struct {
	templateID string
	sizeBytes  uint64
}

type fakeCowCommitMemoryCall struct {
	sourceName string
	templateID string
	sizeBytes  uint64
}

type fakeCowResolveCall struct {
	name string
	kind string
}

type fakeCowDeleteCall struct {
	name string
	kind string
}

type fakeCowVolumeManager struct {
	mu                  sync.Mutex
	resolvePaths        map[string]string
	resolveErrs         map[string]error
	deleteErrs          map[string]error
	deactivateErrs      map[string]error
	createDefaultErr    error
	createSnapshotErr   error
	resolveDelay        time.Duration
	createDefaultCalls  []fakeCowCreateDefaultCall
	createSnapshotCalls []fakeCowCreateSnapshotCall
	createBuildCalls    []fakeCowCreateBuildRootfsCall
	commitRootfsCalls   []fakeCowCommitTemplateRootfsCall
	createMemoryCalls   []fakeCowCreateMemoryCall
	commitMemoryCalls   []fakeCowCommitMemoryCall
	commitMemoryErr     error
	resolveCalls        []fakeCowResolveCall
	deleteCalls         []fakeCowDeleteCall
	deactivateCalls     []fakeCowDeleteCall
	sizeBytes           map[string]uint64
	metrics             map[string]uint64
}

func (m *fakeCowVolumeManager) CreateDefaultMediumVolume(ctx context.Context, sandboxID, volumeName string, sizeBytes uint64) (*cowVolume, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createDefaultCalls = append(m.createDefaultCalls, fakeCowCreateDefaultCall{sandboxID: sandboxID, volumeName: volumeName, sizeBytes: sizeBytes})
	if m.createDefaultErr != nil {
		return nil, m.createDefaultErr
	}
	name := fmt.Sprintf("sb-%s-%s", sandboxID, volumeName)
	path := fmt.Sprintf("/dev/mapper/%s", name)
	if m.resolvePaths != nil {
		if v, ok := m.resolvePaths[name]; ok {
			path = v
		}
	}
	return &cowVolume{VolumeName: name, Kind: cowKindVolume, Gen: 0, FilePath: path}, nil
}

func (m *fakeCowVolumeManager) CreateSandboxRootfsFromTemplate(ctx context.Context, sandboxID, templateID string, gen uint32, desiredSizeBytes uint64) (*cowVolume, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createSnapshotCalls = append(m.createSnapshotCalls, fakeCowCreateSnapshotCall{sandboxID: sandboxID, templateID: templateID, gen: gen, desiredSizeBytes: desiredSizeBytes})
	if m.createSnapshotErr != nil {
		return nil, m.createSnapshotErr
	}
	name := fmt.Sprintf("sb-%s-rootfs-gen%d", sandboxID, gen)
	path := fmt.Sprintf("/dev/mapper/%s", name)
	if m.resolvePaths != nil {
		if v, ok := m.resolvePaths[name]; ok {
			path = v
		}
	}
	return &cowVolume{VolumeName: name, Kind: cowKindSnapshot, Gen: gen, FilePath: path}, nil
}

func (m *fakeCowVolumeManager) RollbackDeriveNewGen(ctx context.Context, sandboxID, snapshotRootfsVol string, gen uint32, desiredSizeBytes uint64) (*cowVolume, error) {
	_ = snapshotRootfsVol
	return m.CreateSandboxRootfsFromTemplate(ctx, sandboxID, "", gen, desiredSizeBytes)
}

func (m *fakeCowVolumeManager) CreateTemplateBuildRootfs(ctx context.Context, templateID string, sizeBytes uint64) (*cowVolume, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createBuildCalls = append(m.createBuildCalls, fakeCowCreateBuildRootfsCall{templateID: templateID, sizeBytes: sizeBytes})
	name := fmt.Sprintf("tpl-%s-build-rootfs", templateID)
	path := fmt.Sprintf("/dev/mapper/%s", name)
	if m.resolvePaths != nil {
		if v, ok := m.resolvePaths[name]; ok {
			path = v
		}
	}
	return &cowVolume{VolumeName: name, Kind: cowKindVolume, Gen: 0, FilePath: path}, nil
}

func (m *fakeCowVolumeManager) CommitTemplateRootfs(ctx context.Context, sourceName, templateID string) (*cowVolume, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commitRootfsCalls = append(m.commitRootfsCalls, fakeCowCommitTemplateRootfsCall{sourceName: sourceName, templateID: templateID})
	name := fmt.Sprintf("tpl-%s-rootfs", templateID)
	path := fmt.Sprintf("/dev/mapper/%s", name)
	if m.resolvePaths != nil {
		if v, ok := m.resolvePaths[name]; ok {
			path = v
		}
	}
	return &cowVolume{VolumeName: name, Kind: cowKindSnapshot, Gen: 0, FilePath: path}, nil
}

func (m *fakeCowVolumeManager) CreateMemoryVolume(ctx context.Context, templateID string, sizeBytes uint64) (*cowVolume, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createMemoryCalls = append(m.createMemoryCalls, fakeCowCreateMemoryCall{templateID: templateID, sizeBytes: sizeBytes})
	name := fmt.Sprintf("tpl-%s-memory", templateID)
	path := fmt.Sprintf("/dev/mapper/%s", name)
	if m.resolvePaths != nil {
		if v, ok := m.resolvePaths[name]; ok {
			path = v
		}
	}
	return &cowVolume{VolumeName: name, Kind: cowKindVolume, Gen: 0, FilePath: path}, nil
}

func (m *fakeCowVolumeManager) CommitTemplateMemory(ctx context.Context, sourceName, templateID string, sizeBytes uint64) (*cowVolume, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commitMemoryCalls = append(m.commitMemoryCalls, fakeCowCommitMemoryCall{sourceName: sourceName, templateID: templateID, sizeBytes: sizeBytes})
	if m.commitMemoryErr != nil {
		return nil, m.commitMemoryErr
	}
	name := fmt.Sprintf("tpl-%s-memory", templateID)
	path := fmt.Sprintf("/dev/mapper/%s", name)
	if m.resolvePaths != nil {
		if v, ok := m.resolvePaths[name]; ok {
			path = v
		}
	}
	return &cowVolume{VolumeName: name, Kind: cowKindSnapshot, Gen: 0, FilePath: path}, nil
}

func (m *fakeCowVolumeManager) DeleteByKind(ctx context.Context, name, kind string) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, fakeCowDeleteCall{name: name, kind: kind})
	if m.deleteErrs == nil {
		return nil
	}
	err := m.deleteErrs[name+"|"+kind]
	if err == nil || isCowSemantic(err, cubecow.SemNotFound) {
		return nil
	}
	return err
}

func (m *fakeCowVolumeManager) DeactivateByKind(ctx context.Context, name, kind string) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deactivateCalls = append(m.deactivateCalls, fakeCowDeleteCall{name: name, kind: kind})
	if m.deactivateErrs == nil {
		return nil
	}
	return m.deactivateErrs[name+"|"+kind]
}

func (m *fakeCowVolumeManager) ResolveDevPath(ctx context.Context, name, kind string) (string, error) {
	_ = ctx
	m.mu.Lock()
	m.resolveCalls = append(m.resolveCalls, fakeCowResolveCall{name: name, kind: kind})
	delay := m.resolveDelay
	var resolveErr error
	if m.resolveErrs != nil {
		if err, ok := m.resolveErrs[name]; ok {
			resolveErr = err
		}
	}
	path := fmt.Sprintf("/dev/mapper/%s", name)
	if m.resolvePaths != nil {
		if configuredPath, ok := m.resolvePaths[name]; ok {
			path = configuredPath
		}
	}
	m.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if resolveErr != nil {
		return "", resolveErr
	}
	return path, nil
}

func (m *fakeCowVolumeManager) GetSizeBytes(ctx context.Context, name string) (uint64, error) {
	_ = ctx
	if m.sizeBytes != nil {
		return m.sizeBytes[name], nil
	}
	return 1024 * 1024, nil
}

func (m *fakeCowVolumeManager) GetVolumeInfo(ctx context.Context, name string) (*cubecow.Volume, error) {
	_ = ctx
	if m.resolveErrs != nil {
		if err, ok := m.resolveErrs[name]; ok {
			return nil, err
		}
	}
	path, err := m.ResolveDevPath(ctx, name, "")
	if err != nil {
		return nil, err
	}
	size, err := m.GetSizeBytes(ctx, name)
	if err != nil {
		return nil, err
	}
	return &cubecow.Volume{Name: name, DevicePath: path, SizeBytes: size}, nil
}

func (m *fakeCowVolumeManager) GetMetrics(ctx context.Context) (map[string]uint64, error) {
	_ = ctx
	if m.metrics != nil {
		return m.metrics, nil
	}
	return map[string]uint64{}, nil
}

func TestCleanupTemplateLocalDataIsIdempotent(t *testing.T) {
	cfg := makeTestConfig(t)

	s := &local{}
	s.config = cfg
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	previousLocalStorage := localStorage
	localStorage = s
	t.Cleanup(func() {
		localStorage = previousLocalStorage
	})

	templateID := "tpl-cleanup-" + uuid.NewString()
	snapshotPath := filepath.Join(s.config.RootPath, "snapshots", templateID)
	templatePath := filepath.Join(s.cubeboxTemplateFormatPath, templateID)
	pooledTemplatePath := filepath.Join(s.config.RootPath, "base-block-storage", "templates", "1Mi", templateID)

	require.NoError(t, os.MkdirAll(snapshotPath, 0o755))
	require.NoError(t, os.MkdirAll(templatePath, 0o755))
	require.NoError(t, os.MkdirAll(pooledTemplatePath, 0o755))

	s.tmpPoolFormat.Store(templateID, noopPool{})
	s.poolFormat.Store(templateID, noopPool{})
	derivedPoolKey := filepath.Join("1Mi", templateID, "derived")
	s.poolFormat.Store(derivedPoolKey, noopPool{})

	require.NoError(t, CleanupTemplateLocalData(context.Background(), templateID, snapshotPath))

	_, err := os.Stat(snapshotPath)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(templatePath)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(pooledTemplatePath)
	assert.True(t, os.IsNotExist(err))
	if _, ok := s.tmpPoolFormat.Load(templateID); ok {
		t.Fatal("tmp pool entry should be removed")
	}
	if _, ok := s.poolFormat.Load(templateID); ok {
		t.Fatal("template pool entry should be removed")
	}
	if _, ok := s.poolFormat.Load(derivedPoolKey); ok {
		t.Fatal("derived pool entry should be removed")
	}

	require.NoError(t, CleanupTemplateLocalData(context.Background(), templateID, snapshotPath))
}

func TestCreateWithTimeoutCtx(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	s := &local{}
	s.config = makeTestConfig(t)
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{
			{
				Name: "test",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						SizeLimit: "1Mi",
					},
				},
			},
		},
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test",
		},
		ReqInfo: req,
	}
	assert.NoError(t, s.Create(ctx, opts))
	assert.Nil(t, opts.StorageInfo)
}

func TestCreateWithInvalidParam(t *testing.T) {
	ctx := context.Background()

	s := &local{}
	s.config = makeTestConfig(t)
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{
			{
				Name: "test",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						SizeLimit: "1Mi",
					},
				},
			},
		},
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test",
		},
		ReqInfo: req,
	}

	err := s.Create(ctx, nil)
	assert.Error(t, err)
	status, ok := ret.FromError(err)
	require.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, status.Code())
	assert.Nil(t, opts.StorageInfo)

	err = s.Create(context.Background(), opts)
	assert.Error(t, err)
	status, ok = ret.FromError(err)
	require.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, status.Code())
	assert.Nil(t, opts.StorageInfo)

	opts.ReqInfo = nil
	err = s.Create(ctx, opts)
	assert.Error(t, err)
	status, ok = ret.FromError(err)
	require.True(t, ok)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, status.Code())
	assert.Nil(t, opts.StorageInfo)

}

func TestMain(m *testing.M) {
	if os.Getenv("CI") != "" {
		fmt.Println("Skipping testing in CI environment")
		return
	}
	m.Run()
}

func TestPollImmediateInfiniteWithContext(t *testing.T) {
	timeout := 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan bool, 1)
	errChan := make(chan error, 1)
	interval := 10 * time.Millisecond
	expectedCnt := int(timeout/interval) + 1
	gotCnt := 0
	go func() {
		alreadyRun := false
		err := wait.PollImmediateInfiniteWithContext(ctx, interval, func(ctx context.Context) (bool, error) {
			if !alreadyRun {
				done <- false
				alreadyRun = true
			}
			gotCnt++
			return false, nil
		})
		errChan <- err
	}()

	select {
	case <-done:
	case <-time.After(11 * time.Millisecond):
		t.Fatal("PollImmediateInfiniteWithContext run immediately")
	}

	select {
	case <-ctx.Done():
		assert.Equal(t, expectedCnt, gotCnt)
	case err := <-errChan:
		assert.NoError(t, err)
	}
}

func TestSnapCreateCubebox(t *testing.T) {
	cfg := makeTestConfig(t)

	s := &local{}
	s.config = cfg
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "cube-box-template-id-" + uuid.NewString()
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{
			{
				Name: "test",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						SizeLimit: "1Mi",
					},
				},
			},
		},
		Annotations: map[string]string{
			constants.MasterAnnotationsAppSnapshotCreate:    "true",
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test",
		},
		ReqInfo: req,
	}

	err := s.Create(ctx, opts)
	assert.NoError(t, err)
	require.NotNil(t, opts.StorageInfo)
	res := opts.StorageInfo.(*StorageInfo)
	require.Len(t, res.Volumes, 1)
	filePath := res.Volumes["test"].FilePath

	exist, _ := utils.DenExist(filePath)
	assert.True(t, exist)
	assert.Equal(t, 1024*1024+diskSizeExtendInBytes, int(res.Volumes["test"].FSQuota))

	dOpts := &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test",
		},
	}
	assert.NoError(t, s.Destroy(ctx, dOpts))

	exist, _ = utils.DenExist(filePath)

	assert.True(t, exist)
	p, ok := s.poolFormat.Load(templateID)
	assert.True(t, ok)
	file, err := p.(Pool).Get(ctx, 0)
	assert.NoError(t, err)
	assert.NotNil(t, file)
	t.Logf("file: %v", file.FilePath)

	info, err := s.readBackendFileInfo(ctx, templateID)
	assert.NoError(t, err)
	assert.NotNil(t, info)
	assert.Equal(t, templateID, info.TemplateID)

	assert.NoError(t, s.Destroy(ctx, dOpts))

	assert.Error(t, s.Destroy(ctx, nil))
}

func TestCreateCubeboxBySnap(t *testing.T) {

	cfg := makeTestConfig(t)

	s := &local{}
	s.config = cfg
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "cube-box-template-id-" + uuid.NewString()
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{
			{
				Name: "test",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						SizeLimit: "1Mi",
					},
				},
			},
		},
		Annotations: map[string]string{
			constants.MasterAnnotationsAppSnapshotCreate:    "true",
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "test",
		},
		ReqInfo: req,
	}

	err := s.Create(ctx, opts)
	assert.NoError(t, err)
	require.NotNil(t, opts.StorageInfo)

	for i := 0; i < 10; i++ {
		reqSnap := &cubebox.RunCubeSandboxRequest{
			Volumes: []*cubebox.Volume{
				{
					Name: "test",
					VolumeSource: &cubebox.VolumeSource{
						EmptyDir: &cubebox.EmptyDirVolumeSource{
							SizeLimit: "1Mi",
						},
					},
				},
			},
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: templateID,
			},
			InstanceType: cubebox.InstanceType_cubebox.String(),
		}
		opts = &workflow.CreateContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test" + strconv.Itoa(i),
			},
			ReqInfo: reqSnap,
		}
		err = s.Create(ctx, opts)
		assert.NoError(t, err)
		require.NotNil(t, opts.StorageInfo)
		res := opts.StorageInfo.(*StorageInfo)
		require.Len(t, res.Volumes, 1)
		filePath := res.Volumes["test"].FilePath
		t.Logf("filePath: %s", filePath)
		exist, _ := utils.DenExist(filePath)
		assert.True(t, exist)
		assert.Equal(t, 1024*1024+diskSizeExtendInBytes, int(res.Volumes["test"].FSQuota))

		dOpts := &workflow.DestroyContext{
			BaseWorkflowInfo: workflow.BaseWorkflowInfo{
				SandboxID: "test" + strconv.Itoa(i),
			},
		}
		assert.NoError(t, s.Destroy(ctx, dOpts))

		exist, _ = utils.DenExist(filePath)
		assert.False(t, exist)

		assert.NoError(t, s.Destroy(ctx, dOpts))

		assert.Error(t, s.Destroy(ctx, nil))
	}
}

func TestInit(t *testing.T) {

	cfg := makeTestConfig(t)

	s := &local{}
	s.config = cfg
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))
	assert.NoError(t, s.Init(context.Background(), nil))
}

func TestInitSkipsPoolSetupWhenStorageBackendIsCow(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	cfg.DiskSize = "not-a-size"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))
	assert.Same(t, fakeManager, s.cowManager)

	emptyDirPath := filepath.Join(cfg.DataPath, emptyDir)
	_, err := os.Stat(emptyDirPath)
	assert.True(t, os.IsNotExist(err))

	poolCount := 0
	s.poolFormat.Range(func(_, _ any) bool {
		poolCount++
		return true
	})
	assert.Zero(t, poolCount)
}

func TestInitResetsCowStorageAndReinitializesEngine(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	// cubelet derives reflink root_dir from data_path, so put data_path
	// somewhere we can also pre-seed stale state in.
	rootDir := filepath.Join(cfg.DataPath, "cubecow-reflink")

	s := &local{config: cfg, cowEngine: &cubecow.Engine{}}
	require.NoError(t, os.MkdirAll(cfg.RootPath, 0o755))
	require.NoError(t, os.MkdirAll(cfg.DataPath, 0o755))
	require.NoError(t, s.initDb())

	oldEngine := s.cowEngine
	staleVolumes := filepath.Join(rootDir, "volumes")
	require.NoError(t, os.MkdirAll(staleVolumes, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staleVolumes, "stale"), []byte("stale"), 0o644))

	oldReset := cowResetNodeStorage
	oldInit := initCowEngine
	t.Cleanup(func() {
		cowResetNodeStorage = oldReset
		initCowEngine = oldInit
	})

	resetCalls := 0
	newEngine := &cubecow.Engine{}
	cowResetNodeStorage = func(engine *cubecow.Engine) error {
		resetCalls++
		assert.Same(t, oldEngine, engine)
		return nil
	}
	initCowEngine = func(got *Config) (*cubecow.Engine, string, error) {
		assert.Same(t, cfg, got)
		return newEngine, "test-config", nil
	}

	require.NoError(t, s.Init(context.Background(), nil))
	assert.Equal(t, 1, resetCalls)
	assert.Same(t, newEngine, s.cowEngine)
	assert.NotNil(t, s.cowManager)

	_, err := os.Stat(staleVolumes)
	assert.True(t, os.IsNotExist(err))
}

func TestCreateDestroyDefaultMediumWithCow(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "test"},
		ReqInfo:          req,
	}

	assert.NoError(t, s.Create(ctx, opts))
	require.NotNil(t, opts.StorageInfo)
	res := opts.StorageInfo.(*StorageInfo)
	vol := res.Volumes["test"]
	require.NotNil(t, vol)
	assert.Equal(t, "sb-test-test", vol.VolumeName)
	assert.Equal(t, cowKindVolume, vol.Kind)
	assert.Equal(t, uint32(0), vol.Gen)
	assert.Equal(t, "/dev/mapper/sb-test-test", vol.FilePath)
	assert.Equal(t, unifiedStorageSize.Value()+diskSizeOverheadInBytes, vol.SizeLimit)
	assert.Equal(t, int64(1024*1024+diskSizeExtendInBytes), vol.FSQuota)
	require.Len(t, fakeManager.createDefaultCalls, 1)
	assert.Equal(t, uint64(unifiedStorageSize.Value()), fakeManager.createDefaultCalls[0].sizeBytes)

	dOpts := &workflow.DestroyContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "test"}}
	assert.NoError(t, s.Destroy(ctx, dOpts))
	require.Len(t, fakeManager.deleteCalls, 1)
	assert.Equal(t, fakeCowDeleteCall{name: "sb-test-test", kind: cowKindVolume}, fakeManager.deleteCalls[0])
}

func TestRestoreFromTemplateWithCow(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	// Catalog entry exists but the template has no memory snapshot (rootfs-only
	// template). The create path should succeed without invoking memory
	// prefetch.
	seedTestSnapshotCatalog(t, templateID, "", "")
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations:  map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: templateID},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-sb"},
		ReqInfo:          req,
	}

	assert.NoError(t, s.Create(ctx, opts))
	require.NotNil(t, opts.StorageInfo)
	res := opts.StorageInfo.(*StorageInfo)
	vol := res.Volumes["test"]
	require.NotNil(t, vol)
	assert.Equal(t, "sb-restore-sb-rootfs-gen0", vol.VolumeName)
	assert.Equal(t, cowKindSnapshot, vol.Kind)
	assert.Equal(t, uint32(0), vol.Gen)
	assert.Equal(t, unifiedStorageSize.Value()+diskSizeOverheadInBytes, vol.SizeLimit)
	assert.Equal(t, int64(1024*1024+diskSizeExtendInBytes), vol.FSQuota)
	require.Len(t, fakeManager.createSnapshotCalls, 1)
	assert.Equal(t, fakeCowCreateSnapshotCall{sandboxID: "restore-sb", templateID: templateID, gen: 0, desiredSizeBytes: uint64(unifiedStorageSize.Value())}, fakeManager.createSnapshotCalls[0])
}

// seedTestSnapshotCatalog writes an in-memory catalog entry keyed by
// snapshotID/templateID so tests can exercise the v4 catalog-first
// restoreMemoryVolume path without setting up a real on-disk snapshot tree.
// The entry is removed in t.Cleanup so tests do not pollute each other.
func seedTestSnapshotCatalog(t *testing.T, id, memoryVol, memoryKind string) {
	t.Helper()
	snapDir := filepath.Join(t.TempDir(), "snap-"+id)
	entry := &SnapshotCatalogEntry{
		SnapshotID:   id,
		InstanceType: "cubebox",
		SpecDir:      "1C1M",
		SnapshotPath: snapDir,
		MetaDir:      snapDir,
		RootfsVol:    "tpl-" + id + "-rootfs",
		RootfsKind:   CowKindSnapshot,
		MemoryVol:    memoryVol,
		MemoryKind:   memoryKind,
		Kind:         CatalogKindTemplate,
	}
	require.NoError(t, WriteSnapshotCatalog(entry))
	t.Cleanup(func() { DeleteSnapshotCatalog(id) })
}

func TestRestoreFromTemplatePrefetchesMemoryVolURLWithCow(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolvePaths: map[string]string{"tpl-memory": "/dev/mapper/tpl-memory"},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	seedTestSnapshotCatalog(t, templateID, "tpl-memory", CowKindVolume)

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		// v4: master only sends the logical template id; physical
		// memory_vol/memory_kind are resolved from cubelet's local catalog.
		Annotations: map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-sb"},
		ReqInfo:          req,
	}

	require.NoError(t, s.Create(ctx, opts))
	require.NotNil(t, opts.StorageInfo)
	res := opts.StorageInfo.(*StorageInfo)
	assert.Equal(t, "file:///dev/mapper/tpl-memory", res.RestoreMemoryVolURL)
	require.Len(t, fakeManager.resolveCalls, 1)
	assert.Equal(t, fakeCowResolveCall{name: "tpl-memory", kind: CowKindVolume}, fakeManager.resolveCalls[0])
	require.Len(t, fakeManager.createSnapshotCalls, 1)
}

// TestRestoreFromRuntimeSnapshotPrefetchesMemoryVolURLFromCatalog verifies
// that the runtime-snapshot create-from path also resolves memory_vol via the
// local catalog when the master sends only the logical RuntimeSnapshotID.
func TestRestoreFromRuntimeSnapshotPrefetchesMemoryVolURLFromCatalog(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolvePaths: map[string]string{"snap-memory": "/dev/mapper/snap-memory"},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	snapshotID := "snap-" + uuid.NewString()
	seedTestSnapshotCatalog(t, snapshotID, "snap-memory", CowKindVolume)

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			// Runtime snapshot path: logical id only.
			constants.MasterAnnotationAppSnapshotTemplateID: snapshotID,
			constants.MasterAnnotationRuntimeSnapshotID:     snapshotID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-runtime-sb"},
		ReqInfo:          req,
	}

	require.NoError(t, s.Create(ctx, opts))
	res := opts.StorageInfo.(*StorageInfo)
	assert.Equal(t, "file:///dev/mapper/snap-memory", res.RestoreMemoryVolURL)
}

// TestRestoreFromTemplateFailsFastWhenCatalogMissing locks in the v4 contract
// that create-from-snapshot must fail fast when the logical id has no catalog
// entry, instead of silently degrading to a cold start.
func TestRestoreFromTemplateFailsFastWhenCatalogMissing(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			// Logical id present but no catalog entry seeded -> fail fast.
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-miss-sb"},
		ReqInfo:          req,
	}

	err := s.Create(ctx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), templateID)
	assert.Contains(t, err.Error(), "local catalog")
	assert.Empty(t, fakeManager.resolveCalls)
}

func TestRestoreFromTemplateSkipsMemoryPrefetchWithoutMemoryRef(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		// No template id / runtime snapshot id -> non-snapshot create path,
		// memory prefetch is skipped entirely.
		Annotations:  map[string]string{},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-sb"},
		ReqInfo:          req,
	}

	require.NoError(t, s.Create(ctx, opts))
	require.NotNil(t, opts.StorageInfo)
	res := opts.StorageInfo.(*StorageInfo)
	assert.Empty(t, res.RestoreMemoryVolURL)
	assert.Empty(t, fakeManager.resolveCalls)
	require.Len(t, fakeManager.createDefaultCalls, 1)
}

func TestRestoreFromTemplateKeepsExistingMemoryVolURL(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
			// Pre-resolved URL short-circuits the catalog lookup entirely;
			// no catalog seeding is required.
			constants.AnnotationVMSnapshotMemoryVolURL: "file:///dev/mapper/already",
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-sb"},
		ReqInfo:          req,
	}

	require.NoError(t, s.Create(ctx, opts))
	res := opts.StorageInfo.(*StorageInfo)
	assert.Equal(t, "file:///dev/mapper/already", res.RestoreMemoryVolURL)
	assert.Empty(t, fakeManager.resolveCalls)
	require.Len(t, fakeManager.createSnapshotCalls, 1)
}

func TestRestoreFromTemplateMemoryPrefetchErrorFailsCreate(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolveErrs:  map[string]error{"tpl-memory": errors.New("activate failed")},
		resolveDelay: 10 * time.Millisecond,
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	seedTestSnapshotCatalog(t, templateID, "tpl-memory", CowKindVolume)
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-sb"},
		ReqInfo:          req,
	}

	err := s.Create(ctx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve snapshot memory volume tpl-memory")
	assert.Nil(t, opts.StorageInfo)
	require.Len(t, fakeManager.createSnapshotCalls, 1)
	require.Len(t, fakeManager.deleteCalls, 1)
	assert.Equal(t, fakeCowDeleteCall{name: "sb-restore-sb-rootfs-gen0", kind: cowKindSnapshot}, fakeManager.deleteCalls[0])
	assert.Empty(t, fakeManager.deactivateCalls)
}

func TestRestoreFromTemplateMemoryPrefetchDoesNotDeleteTemplateMemoryOnRootfsFailure(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		createSnapshotErr: errors.New("create rootfs failed"),
		resolvePaths:      map[string]string{"tpl-memory": "/dev/mapper/tpl-memory"},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	seedTestSnapshotCatalog(t, templateID, "tpl-memory", CowKindVolume)
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-sb"},
		ReqInfo:          req,
	}

	err := s.Create(ctx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create rootfs failed")
	require.Len(t, fakeManager.resolveCalls, 1)
	assert.Equal(t, fakeCowResolveCall{name: "tpl-memory", kind: CowKindVolume}, fakeManager.resolveCalls[0])
	assert.Empty(t, fakeManager.deleteCalls)
	assert.Empty(t, fakeManager.deactivateCalls)
}

func TestCreateRollbackCleansCreatedCowVolumeWhenMetadataWriteFails(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))
	require.NoError(t, s.db.Close())
	s.config.RootPath = filepath.Join(t.TempDir(), "missing-root")

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "write-fail-sb"},
		ReqInfo:          req,
	}

	err := s.Create(ctx, opts)
	require.Error(t, err)
	require.Len(t, fakeManager.createDefaultCalls, 1)
	require.Len(t, fakeManager.deleteCalls, 1)
	assert.Equal(t, fakeCowDeleteCall{name: "sb-write-fail-sb-test", kind: cowKindVolume}, fakeManager.deleteCalls[0])
}

func TestCreateRollbackDeletesPersistedMetadataWhenPostPersistUpdateFails(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	require.NoError(t, s.db.Set(bucketName, templateID, []byte("{bad-json")))
	// Catalog seeded with empty memory_vol so the create proceeds past
	// memory prefetch and exercises the post-persist rollback path under
	// test.
	seedTestSnapshotCatalog(t, templateID, "", "")

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations:  map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: templateID},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "post-persist-fail-sb"},
		ReqInfo:          req,
	}

	err := s.Create(ctx, opts)
	require.Error(t, err)
	require.Len(t, fakeManager.deleteCalls, 1)
	assert.Equal(t, fakeCowDeleteCall{name: "sb-post-persist-fail-sb-rootfs-gen0", kind: cowKindSnapshot}, fakeManager.deleteCalls[0])
	_, err = s.readBackendFileInfoRaw(ctx, "post-persist-fail-sb")
	assert.ErrorIs(t, err, utils.ErrorKeyNotFound)
}

func TestCleanupCreateResultRemovesHostDirSandboxPath(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	s := &local{config: cfg, cowManager: &fakeCowVolumeManager{}}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = t.TempDir()
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })
	sandboxDir := filepath.Join(hostDirBasePath, "hostdir-sb", "vol")
	require.NoError(t, os.MkdirAll(sandboxDir, 0755))

	err := s.cleanupCreateResult(context.Background(), &StorageInfo{
		SandboxID: "hostdir-sb",
		Volumes:   map[string]*BackendFileInfo{},
	})
	require.NoError(t, err)
	require.NoDirExists(t, filepath.Join(hostDirBasePath, "hostdir-sb"))
}

func TestCleanupHostDirVolumesResolvesSymlinkedBasePath(t *testing.T) {
	// Simulate a deployment where an ancestor of hostDirBasePath is a symlink
	// (e.g. /data -> /mnt/ssd/data). The kernel records fully resolved
	// mountpoints in /proc/self/mountinfo, so cleanup must canonicalize the
	// path before walking, otherwise mounts would leak and os.RemoveAll could
	// wipe the real backing directory.
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	s := &local{config: cfg, cowManager: &fakeCowVolumeManager{}}
	require.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	tmp := t.TempDir()
	realBase := filepath.Join(tmp, "real", "hostdir")
	require.NoError(t, os.MkdirAll(realBase, 0755))

	symlinkBase := filepath.Join(tmp, "link")
	require.NoError(t, os.Symlink(filepath.Join(tmp, "real"), symlinkBase))

	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = filepath.Join(symlinkBase, "hostdir")
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	sandboxID := "symlink-sb"
	bindPath := filepath.Join(hostDirBasePath, sandboxID, "rw", "vol")
	require.NoError(t, os.MkdirAll(bindPath, 0755))

	err := s.cleanupHostDirVolumes(context.Background(), &StorageInfo{
		SandboxID: sandboxID,
		HostDirBackendInfos: map[string]*HostDirBackendInfo{
			"test/vol": {
				VolumeName: "test",
				ShareDir:   filepath.Join(hostDirBasePath, sandboxID, "rw"),
				BindPath:   bindPath,
			},
		},
	})
	require.NoError(t, err)
	// The sandbox dir must be gone whether referenced via the symlink path or
	// the resolved real path.
	require.NoDirExists(t, filepath.Join(hostDirBasePath, sandboxID))
	require.NoDirExists(t, filepath.Join(realBase, sandboxID))
}

func TestCleanupHostDirVolumesMissingSandboxDirIsNoop(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	s := &local{config: cfg, cowManager: &fakeCowVolumeManager{}}
	require.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = t.TempDir()
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	err := s.cleanupHostDirVolumes(context.Background(), &StorageInfo{
		SandboxID: "missing-sb",
		HostDirBackendInfos: map[string]*HostDirBackendInfo{
			"test/vol": {VolumeName: "test"},
		},
	})
	require.NoError(t, err)
}

func TestCleanupHostDirVolumesDoesNotFollowSymlinkedLeaf(t *testing.T) {
	// Safety: if the per-sandbox leaf directory is ever replaced by a symlink,
	// cleanup must only unlink the symlink itself and must NOT follow it into
	// the target, otherwise os.RemoveAll would delete unrelated data.
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	s := &local{config: cfg, cowManager: &fakeCowVolumeManager{}}
	require.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	tmp := t.TempDir()
	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = filepath.Join(tmp, "hostdir")
	require.NoError(t, os.MkdirAll(hostDirBasePath, 0755))
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })

	// A directory that must survive cleanup.
	secretDir := filepath.Join(tmp, "secret")
	require.NoError(t, os.MkdirAll(secretDir, 0755))
	secretFile := filepath.Join(secretDir, "keep.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("keep"), 0644))

	sandboxID := "leaf-symlink-sb"
	leaf := filepath.Join(hostDirBasePath, sandboxID)
	require.NoError(t, os.Symlink(secretDir, leaf))

	err := s.cleanupHostDirVolumes(context.Background(), &StorageInfo{
		SandboxID: sandboxID,
		HostDirBackendInfos: map[string]*HostDirBackendInfo{
			"test/vol": {VolumeName: "test"},
		},
	})
	require.NoError(t, err)
	// The symlink is gone, but the target directory and its contents survive.
	_, statErr := os.Lstat(leaf)
	require.True(t, os.IsNotExist(statErr))
	require.DirExists(t, secretDir)
	require.FileExists(t, secretFile)
}

func TestDestroyAfterRestoreCleansSandboxResourcesOnly(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolvePaths: map[string]string{"tpl-memory": "/dev/mapper/tpl-memory"},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	seedTestSnapshotCatalog(t, templateID, "tpl-memory", CowKindVolume)
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "test",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "destroy-restore-sb"},
		ReqInfo:          req,
	}

	require.NoError(t, s.Create(ctx, opts))
	rawInfo, err := s.readBackendFileInfoRaw(ctx, "destroy-restore-sb")
	require.NoError(t, err)
	assert.Empty(t, rawInfo.RestoreMemoryVolURL)

	err = s.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID:  "destroy-restore-sb",
			IsRollBack: true,
		},
	})
	require.NoError(t, err)
	require.Len(t, fakeManager.deleteCalls, 1)
	assert.Equal(t, fakeCowDeleteCall{name: "sb-destroy-restore-sb-rootfs-gen0", kind: cowKindSnapshot}, fakeManager.deleteCalls[0])
	assert.Empty(t, fakeManager.deactivateCalls)
}

func TestCreateSnapshotUsesTemplateBuildRootfsWithCow(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "cube_rootfs_rw",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationsAppSnapshotCreate:    "true",
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
			constants.MasterAnnotationAppSnapshotVersion:    "v2",
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: templateID + "_0"},
		ReqInfo:          req,
	}

	require.NoError(t, s.Create(ctx, opts))
	require.NotNil(t, opts.StorageInfo)
	res := opts.StorageInfo.(*StorageInfo)
	vol := res.Volumes["cube_rootfs_rw"]
	require.NotNil(t, vol)
	assert.Equal(t, fmt.Sprintf("tpl-%s-build-rootfs", templateID), vol.VolumeName)
	assert.Equal(t, cowKindVolume, vol.Kind)
	assert.Equal(t, unifiedStorageSize.Value()+diskSizeOverheadInBytes, vol.SizeLimit)
	assert.Equal(t, int64(1024*1024+diskSizeExtendInBytes), vol.FSQuota)
	require.Len(t, fakeManager.createBuildCalls, 1)
	assert.Equal(t, fakeCowCreateBuildRootfsCall{templateID: templateID, sizeBytes: uint64(unifiedStorageSize.Value())}, fakeManager.createBuildCalls[0])
}

func TestNormalizeRootfsSizes(t *testing.T) {
	t.Parallel()

	oneG := resource.MustParse("1G")
	oneGi := resource.MustParse("1Gi")
	twoGi := resource.MustParse("2Gi")

	tests := []struct {
		name             string
		requested        string
		wantBackendAlloc int64
		wantComparable   int64
		wantFSQuota      int64
	}{
		{
			name:             "decimal 1G rounds up to unified storage",
			requested:        "1G",
			wantBackendAlloc: unifiedStorageSize.Value(),
			wantComparable:   unifiedStorageSize.Value() + diskSizeOverheadInBytes,
			wantFSQuota:      oneG.Value() + diskSizeExtendInBytes,
		},
		{
			name:             "binary 1Gi preserves unified storage",
			requested:        "1Gi",
			wantBackendAlloc: oneGi.Value(),
			wantComparable:   oneGi.Value() + diskSizeOverheadInBytes,
			wantFSQuota:      oneGi.Value() + diskSizeExtendInBytes,
		},
		{
			name:             "larger request keeps larger allocation",
			requested:        "2Gi",
			wantBackendAlloc: twoGi.Value(),
			wantComparable:   twoGi.Value() + diskSizeOverheadInBytes,
			wantFSQuota:      twoGi.Value() + diskSizeExtendInBytes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requested := resource.MustParse(tt.requested)
			sizes := normalizeRootfsSizes(requested)
			assert.Equal(t, tt.wantBackendAlloc, sizes.backendAllocSize.Value())
			assert.Equal(t, tt.wantComparable, sizes.snapshotComparableSize.Value())
			assert.Equal(t, tt.wantFSQuota, sizes.fsQuotaSize.Value())
		})
	}
}

func TestCowTemplateBuildAndRestoreShareComparableRootfsSize(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)

	buildReq := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "cube_rootfs_rw",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1G"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationsAppSnapshotCreate:    "true",
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
			constants.MasterAnnotationAppSnapshotVersion:    "v2",
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	buildOpts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: templateID + "_0"},
		ReqInfo:          buildReq,
	}
	require.NoError(t, s.Create(ctx, buildOpts))
	buildVol := buildOpts.StorageInfo.(*StorageInfo).Volumes["cube_rootfs_rw"]
	require.NotNil(t, buildVol)

	restoreReq := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "cube_rootfs_rw",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1G"}},
		}},
		Annotations:  map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: templateID},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	restoreOpts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "restore-sb"},
		ReqInfo:          restoreReq,
	}
	require.NoError(t, s.Create(ctx, restoreOpts))
	restoreVol := restoreOpts.StorageInfo.(*StorageInfo).Volumes["cube_rootfs_rw"]
	require.NotNil(t, restoreVol)

	assert.Equal(t, buildVol.SizeLimit, restoreVol.SizeLimit)
	assert.Equal(t, buildVol.FSQuota, restoreVol.FSQuota)
	require.Len(t, fakeManager.createBuildCalls, 1)
	require.Len(t, fakeManager.createSnapshotCalls, 1)
	assert.Equal(t, fakeManager.createBuildCalls[0].sizeBytes, fakeManager.createSnapshotCalls[0].desiredSizeBytes)
	assert.Equal(t, uint64(unifiedStorageSize.Value()), fakeManager.createBuildCalls[0].sizeBytes)
}

func TestPrepareDefaultMediumV2DerivesFromTemplateSnapshot(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	seedTestSnapshotCatalog(t, templateID, "tpl-memory", CowKindVolume)
	sandboxID := "v2-restore-sb"
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)

	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "cube_rootfs_rw",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
			constants.MasterAnnotationAppSnapshotVersion:    "v2",
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
		ReqInfo:          req,
	}

	require.NoError(t, s.Create(ctx, opts))
	require.NotNil(t, opts.StorageInfo)
	vol := opts.StorageInfo.(*StorageInfo).Volumes["cube_rootfs_rw"]
	require.NotNil(t, vol)

	assert.Equal(t, fmt.Sprintf("sb-%s-rootfs-gen0", sandboxID), vol.VolumeName)
	assert.Equal(t, cowKindSnapshot, vol.Kind)
	assert.Equal(t, uint32(0), vol.Gen)
	assert.Equal(t, unifiedStorageSize.Value()+diskSizeOverheadInBytes, vol.SizeLimit)
	assert.Equal(t, int64(1024*1024+diskSizeExtendInBytes), vol.FSQuota)

	require.Len(t, fakeManager.createSnapshotCalls, 1, "v2 restore must use template rootfs snapshot derivation")
	got := fakeManager.createSnapshotCalls[0]
	assert.Equal(t, sandboxID, got.sandboxID)
	assert.Equal(t, templateID, got.templateID)
	assert.Equal(t, uint32(0), got.gen)
	assert.Equal(t, uint64(unifiedStorageSize.Value()), got.desiredSizeBytes)

	assert.Empty(t, fakeManager.createDefaultCalls, "v2 restore must not run mkfs via CreateDefaultMediumVolume")
	assert.Empty(t, fakeManager.createBuildCalls, "v2 restore is not a build, must not allocate build-rootfs")
}

func TestPrepareDefaultMediumV2DestroyUsesDeleteSnapshot(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	templateID := "tpl-" + uuid.NewString()
	seedTestSnapshotCatalog(t, templateID, "tpl-memory", CowKindVolume)
	sandboxID := "v2-destroy-sb"
	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)

	req := &cubebox.RunCubeSandboxRequest{
		Volumes: []*cubebox.Volume{{
			Name:         "cube_rootfs_rw",
			VolumeSource: &cubebox.VolumeSource{EmptyDir: &cubebox.EmptyDirVolumeSource{SizeLimit: "1Mi"}},
		}},
		Annotations: map[string]string{
			constants.MasterAnnotationAppSnapshotTemplateID: templateID,
			constants.MasterAnnotationAppSnapshotVersion:    "v2",
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sandboxID},
		ReqInfo:          req,
	}
	require.NoError(t, s.Create(ctx, opts))

	require.NoError(t, s.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID:  sandboxID,
			IsRollBack: true,
		},
	}))

	require.Len(t, fakeManager.deleteCalls, 1)
	assert.Equal(t, fakeCowDeleteCall{
		name: fmt.Sprintf("sb-%s-rootfs-gen0", sandboxID),
		kind: cowKindSnapshot,
	}, fakeManager.deleteCalls[0])
	assert.Empty(t, fakeManager.deactivateCalls)
}

func TestGetSandboxRootfsForSnapshotUsesPreferredRootfs(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolvePaths: map[string]string{"sb-sandbox-rootfs-gen2": "/dev/mapper/refreshed-rootfs"},
		sizeBytes:    map[string]uint64{"sb-sandbox-rootfs-gen2": 2 * 1024 * 1024},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))
	previousLocalStorage := localStorage
	localStorage = s
	t.Cleanup(func() {
		localStorage = previousLocalStorage
	})

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	info := &StorageInfo{
		Namespace: "default",
		SandboxID: "sandbox",
		Volumes: map[string]*BackendFileInfo{
			"data": {Name: "data", VolumeName: "sb-sandbox-data", Kind: cowKindVolume},
			"root": {Name: "root", VolumeName: "sb-sandbox-rootfs-gen2", Kind: cowKindSnapshot, Gen: 2},
		},
	}
	require.NoError(t, s.writeBackendFileInfo(ctx, "sandbox", info))

	rootfs, err := GetSandboxRootfsForSnapshot(ctx, "sandbox", "root")
	require.NoError(t, err)
	assert.Equal(t, "sb-sandbox-rootfs-gen2", rootfs.Name)
	assert.Equal(t, cowKindSnapshot, rootfs.Kind)
	assert.Equal(t, uint32(2), rootfs.Gen)
	assert.Equal(t, "/dev/mapper/refreshed-rootfs", rootfs.DevPath)
	assert.Equal(t, uint64(2*1024*1024), rootfs.SizeBytes)
}

func TestReadBackendFileInfoRefreshesCowDevicePath(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{resolvePaths: map[string]string{"sb-refresh-test": "/dev/mapper/refreshed"}}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	info := &StorageInfo{
		Namespace: "default",
		SandboxID: "refresh-test",
		Volumes: map[string]*BackendFileInfo{
			"test": {
				Name:       "test",
				FilePath:   "/dev/mapper/stale",
				VolumeName: "sb-refresh-test",
				Kind:       cowKindVolume,
			},
		},
	}
	require.NoError(t, s.writeBackendFileInfo(ctx, "refresh-test", info))

	loaded, err := s.readBackendFileInfo(ctx, "refresh-test")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "/dev/mapper/refreshed", loaded.Volumes["test"].FilePath)
}

func TestReadBackendFileInfoReturnsMissingErrorWhenCowObjectIsGone(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolveErrs: map[string]error{
			"sb-missing-test": &cubecow.CowError{Code: cubecow.SemNotFound, RawRC: int32(cubecow.SemNotFound)},
		},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	info := &StorageInfo{
		Namespace: "default",
		SandboxID: "missing-test",
		Volumes: map[string]*BackendFileInfo{
			"test": {
				Name:       "test",
				FilePath:   "/dev/mapper/stale",
				VolumeName: "sb-missing-test",
				Kind:       cowKindVolume,
			},
		},
	}
	require.NoError(t, s.writeBackendFileInfo(ctx, "missing-test", info))

	loaded, err := s.readBackendFileInfo(ctx, "missing-test")
	require.Nil(t, loaded)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCowObjectMissing)
	var missingErr *CowObjectMissingError
	require.True(t, errors.As(err, &missingErr))
	assert.Equal(t, "sb-missing-test", missingErr.VolumeName)
	assert.Equal(t, cowKindVolume, missingErr.Kind)
}

func TestRecoverStorageStateRefreshesAndPersistsCowPaths(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolvePaths: map[string]string{"sb-recover-test": "/dev/mapper/refreshed"},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	info := &StorageInfo{
		Namespace: "default",
		SandboxID: "recover-test",
		Volumes: map[string]*BackendFileInfo{
			"test": {
				Name:       "test",
				FilePath:   "/dev/mapper/stale",
				VolumeName: "sb-recover-test",
				Kind:       cowKindVolume,
			},
		},
	}
	require.NoError(t, s.writeBackendFileInfo(ctx, "recover-test", info))

	require.NoError(t, s.RecoverStorageState(ctx))

	loaded, err := s.readBackendFileInfoRaw(ctx, "recover-test")
	require.NoError(t, err)
	require.Equal(t, "/dev/mapper/refreshed", loaded.Volumes["test"].FilePath)
}

func TestRecoverSandboxStorageIgnoresMissingStorageInfo(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	s := &local{config: cfg, cowManager: &fakeCowVolumeManager{}}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	require.NoError(t, s.RecoverSandboxStorage(context.Background(), "missing-sandbox"))
}

func TestDestroyCowTreatsNotFoundAsSuccess(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolvePaths: map[string]string{"sb-test-test": "/dev/mapper/sb-test-test"},
		deleteErrs: map[string]error{
			"sb-test-test|volume": &cubecow.CowError{Code: cubecow.SemNotFound, RawRC: int32(cubecow.SemNotFound)},
		},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	info := &StorageInfo{
		Namespace: "default",
		SandboxID: "test",
		Volumes: map[string]*BackendFileInfo{
			"test": {
				Name:       "test",
				FilePath:   "/dev/mapper/stale",
				VolumeName: "sb-test-test",
				Kind:       cowKindVolume,
			},
		},
	}
	require.NoError(t, s.writeBackendFileInfo(ctx, "test", info))
	assert.NoError(t, s.Destroy(ctx, &workflow.DestroyContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "test"}}))

	_, err := s.readBackendFileInfo(ctx, "test")
	assert.True(t, errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound))
}

func TestDestroyCowSucceedsWhenReadDetectsMissingObject(t *testing.T) {
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	fakeManager := &fakeCowVolumeManager{
		resolveErrs: map[string]error{
			"sb-drift-test": &cubecow.CowError{Code: cubecow.SemNotFound, RawRC: int32(cubecow.SemNotFound)},
		},
	}

	s := &local{config: cfg, cowManager: fakeManager}
	assert.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))

	ctx := namespaces.WithNamespace(context.Background(), namespaces.Default)
	info := &StorageInfo{
		Namespace: "default",
		SandboxID: "drift-test",
		Volumes: map[string]*BackendFileInfo{
			"test": {
				Name:       "test",
				FilePath:   "/dev/mapper/stale",
				VolumeName: "sb-drift-test",
				Kind:       cowKindVolume,
			},
		},
	}
	require.NoError(t, s.writeBackendFileInfo(ctx, "drift-test", info))
	assert.NoError(t, s.Destroy(ctx, &workflow.DestroyContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "drift-test"}}))
	require.Len(t, fakeManager.deleteCalls, 1)
	assert.Equal(t, fakeCowDeleteCall{name: "sb-drift-test", kind: cowKindVolume}, fakeManager.deleteCalls[0])
}

func TestBuildCowInitJSONOnlyEmitsLogAndBackend(t *testing.T) {
	cfg := &Config{
		Cow: CowInlineConfig{
			Log: CowLogConfig{
				Level: stringPtr("debug"),
			},
			Backend: CowBackendConfig{
				Kind: cowBackendReflink,
				Reflink: CowReflinkBackendConfig{
					RootDir: stringPtr("/data/cubecow-reflink"),
				},
			},
		},
	}

	raw, err := cfg.BuildCowInitJSON()
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))

	logCfg, ok := payload["log"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "debug", logCfg["level"])

	backendCfg, ok := payload["backend"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, cowBackendReflink, backendCfg["kind"])
	reflinkCfg, ok := backendCfg["reflink"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/data/cubecow-reflink", reflinkCfg["root_dir"])

	_, hasStorage := payload["storage"]
	assert.False(t, hasStorage)
	_, hasDisk := payload["disk"]
	assert.False(t, hasDisk)
	_, hasRuntime := payload["cubecow"]
	assert.False(t, hasRuntime)
}

func TestValidateCowStartupDepsReportsMissingCommands(t *testing.T) {
	cfg := &Config{}

	origLookPath := cowLookPath
	cowLookPath = func(file string) (string, error) {
		switch file {
		case "losetup":
			return "", errors.New("not found")
		default:
			return "/usr/bin/" + file, nil
		}
	}
	t.Cleanup(func() {
		cowLookPath = origLookPath
	})

	err := cfg.validateCowStartupDeps()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "losetup")
}

func TestPrepareCowInlineConfigStampsBackendDefaults(t *testing.T) {
	cfg := &Config{
		DataPath: "/var/lib/cubelet/io.cubelet.internal.v1.storage",
	}
	require.NoError(t, cfg.PrepareCowInlineConfig())
	assert.Equal(t, cowBackendReflink, cfg.Cow.Backend.Kind)
	require.NotNil(t, cfg.Cow.Backend.Reflink.RootDir)
	assert.Equal(t, "/var/lib/cubelet/cubecow-reflink", *cfg.Cow.Backend.Reflink.RootDir)
}

func uint32Ptr(v uint32) *uint32 {
	return &v
}

func uint64Ptr(v uint64) *uint64 {
	return &v
}

func boolPtr(v bool) *bool {
	return &v
}

func stringPtr(v string) *string {
	return &v
}
