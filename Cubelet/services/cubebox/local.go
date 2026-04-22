// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	v2 "github.com/containerd/containerd/v2/core/runtime/v2"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/plugins/services"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/google/uuid"
	"github.com/hashicorp/go-multierror"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cbri"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	protobuf "google.golang.org/protobuf/proto"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/rootfs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/runc"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/taskio"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	cubeShimPluginName = "io.containerd.sandbox.controller.v1.cube-shim"
)

type CubeConfig struct {
	RootPath  string `toml:"root_path"`
	StatePath string `toml:"state_path"`

	CubeHostSharedDir  string `toml:"cube_host_shared_dir"`
	CubeHyperVisorPath string `toml:"cube_hypervisor_path"`
	CubeShimPath       string `toml:"cube_shim_path"`
	NetfilePath        string `toml:"-"`

	DisableHostCgroup bool `toml:"disable_host_cgroup"`

	DisableVmCgroup bool `toml:"disable_vm_cgroup"`

	DefaultRuntimeName string `toml:"default_runtime_name"`

	Runtimes map[string]cubeconfig.Runtime `toml:"runtimes"`

	CubeboxGc CubeboxGc `toml:"cubebox_gc"`
}

type CubeboxGc struct {
	Disabled   bool          `toml:"disabled"`
	GcInterval time.Duration `toml:"gc_interval"`
	GcTimeout  time.Duration `toml:"gc_timeout"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type: constants.InternalPlugin,
		ID:   constants.CubeboxID.ID(),
		Requires: []plugin.Type{
			plugins.ShimPlugin,
			plugins.EventPlugin,
			plugins.ServicePlugin,
			plugins.SandboxStorePlugin,
			plugins.SandboxControllerPlugin,
			plugins.CRIServicePlugin,
			constants.PluginCBRIManager,
			constants.CubeStorePlugin,
		},
		Config: defaultConfig(),
		InitFn: func(ic *plugin.InitContext) (_ interface{}, err error) {
			defer func() {
				if err != nil {
					stdlog.Fatalf("plugin %s init fail:%v", constants.CubeboxID, err.Error())
				}
			}()
			config := ic.Config.(*CubeConfig)
			if config.RootPath == "" {
				config.RootPath = ic.Properties[plugins.PropertyRootDir]
			}
			if config.StatePath == "" {
				config.StatePath = ic.Properties[plugins.PropertyStateDir]
			}

			if config.CubeHostSharedDir == "" {
				config.CubeHostSharedDir = defaultCubeHostSharedDir
			}
			if config.CubeHyperVisorPath == "" {
				config.CubeHyperVisorPath = defaultChPath
			}
			if config.CubeShimPath == "" {
				config.CubeShimPath = defaultShimPath
			}
			if err := os.MkdirAll(path.Clean(config.RootPath), os.ModeDir|0755); err != nil {
				return nil, fmt.Errorf("init RootPath dir failed: %w", err)
			}
			if err := os.MkdirAll(path.Clean(config.StatePath), os.ModeDir|0755); err != nil {
				return nil, fmt.Errorf("init StatePath dir failed: %w", err)
			}

			config.NetfilePath = path.Join(config.StatePath, "netfile")

			if config.DefaultRuntimeName == "" {
				return nil, fmt.Errorf("`default_runtime_name` is empty")
			}
			if _, ok := config.Runtimes[config.DefaultRuntimeName]; !ok {
				return nil, fmt.Errorf("no corresponding runtime configured in `runtimes` for `default_runtime_name = \"%s\"", config.DefaultRuntimeName)
			}

			for k, r := range config.Runtimes {
				if !r.PrivilegedWithoutHostDevices && r.PrivilegedWithoutHostDevicesAllDevicesAllowed {
					return nil, errors.New("`privileged_without_host_devices_all_devices_allowed` requires `privileged_without_host_devices` to be enabled")
				}

				if len(r.Sandboxer) == 0 {
					r.Sandboxer = constants.PluginSandboxControllerCubeShim
					config.Runtimes[k] = r
				}

				if len(r.IOType) == 0 {
					r.IOType = cubeconfig.IOTypeFifo
				}
				if r.IOType != cubeconfig.IOTypeStreaming && r.IOType != cubeconfig.IOTypeFifo {
					return nil, errors.New("`io_type` can only be `streaming` or `named_pipe`")
				}
			}

			CubeLog.Infof("%v init config:%+v", fmt.Sprintf("%v.%v", constants.InternalPlugin,
				constants.CubeboxID), config)

			client, err := containerd.New(
				"",
				containerd.WithDefaultPlatform(platforms.Default()),
				containerd.WithInMemoryServices(ic),
			)
			if err != nil {
				return nil, fmt.Errorf("init containerd connect failed.%s", err)
			}

			svcs, err := ic.GetByType(plugins.ServicePlugin)
			if err != nil {
				return nil, fmt.Errorf("failed to get services: %w", err)
			}
			i, ok := svcs[services.TasksService]
			if !ok {
				return nil, errors.New("tasks service not found")
			}

			obj, err := ic.GetByID(plugins.CRIServicePlugin, "images")
			if err != nil {
				return nil, fmt.Errorf("failed to get cri images service: %w", err)
			}

			cbriObj, err := ic.GetByID(constants.PluginCBRIManager, constants.PluginManager)
			if err != nil {
				return nil, fmt.Errorf("failed to get cbri manager: %w", err)
			}
			cbriManager, ok := cbriObj.(cbri.APIManager)
			if !ok {
				return nil, errors.New("cbri manager is not a cbri plugin")
			}
			cubeboxAPIObj, err := ic.GetByID(constants.CubeStorePlugin, constants.CubeboxID.ID())
			if err != nil {
				return nil, fmt.Errorf("get cubebox api client fail:%v", err)
			}
			if setter, ok := cubeboxAPIObj.(cubes.ContainerdClientSetter); ok {
				setter.SetContainerdClient(client)
			}

			shimPlugin, err := ic.GetSingle(plugins.ShimPlugin)
			if err != nil {
				return nil, fmt.Errorf("get shim plugin fail:%v", err)
			}

			l := &local{
				client:        client,
				localTask:     i.(tasks.TasksClient),
				config:        config,
				criImage:      obj.(*cubeimages.CubeImageService),
				cbriManager:   cbriManager,
				cubeboxManger: cubeboxAPIObj.(cubes.CubeboxAPI),
				shims:         shimPlugin.(*v2.ShimManager),
			}

			CubeLog.Info("Start recovering state")

			cbriManager.SetCubeRuntimeImplementation(l)
			afterRecover := func(tmpCtx context.Context, cb *cubeboxstore.CubeBox) error {
				return nil
			}

			if rev, ok := l.cubeboxManger.(cubes.RocoverCubebox); ok {
				if err := rev.RecoverAllCubebox(namespaces.WithNamespace(context.Background(), namespaces.Default), afterRecover); err != nil {
					return nil, fmt.Errorf("recover cubebox: %v", err)
				}
			} else {
				return nil, fmt.Errorf("cubebox manager not support recover")
			}

			runc.Init(config.RootPath)
			taskio.Init(taskio.FIFODir(config.StatePath))
			rootfs.Init(config.RootPath)
			virtiofs.Init(config.StatePath)
			return l, nil
		},
	})
}

func defaultConfig() *CubeConfig {
	return &CubeConfig{
		CubeHostSharedDir:  defaultCubeHostSharedDir,
		CubeHyperVisorPath: defaultChPath,
		CubeShimPath:       defaultShimPath,
		CubeboxGc: CubeboxGc{
			GcInterval: 10 * time.Minute,
			GcTimeout:  10 * time.Minute,
		},
	}
}

type local struct {
	client    *containerd.Client
	localTask tasks.TasksClient
	config    *CubeConfig
	shims     *v2.ShimManager

	criImage      *cubeimages.CubeImageService
	cbriManager   cbri.APIManager
	cubeboxManger cubes.CubeboxAPI
}

const (
	defaultCubeHostSharedDir = "/run/cube-containers/shared/sandboxes"
	cmdTimeout               = time.Second * 3
	defaultChPath            = "/usr/local/services/cubetoolbox/cube-hypervisor/cube-hypervisor"
	defaultShimPath          = "/usr/local/services/cubetoolbox/cube-shim/bin/containerd-shim-cube-rs"
)

func (l *local) ID() string {
	return constants.CubeboxID.ID()
}

func (l *local) Init(ctx context.Context, opts *workflow.InitInfo) error {

	CubeLog.WithContext(ctx).Errorf("Init doing")
	defer CubeLog.WithContext(ctx).Errorf("Init end")

	nses, err := l.client.NamespaceService().List(ctx)
	if err != nil {
		return err
	}
	for _, ns := range nses {
		ctx = context.WithValue(ctx, CubeLog.KeyNamespace, ns)
		CubeLog.WithContext(ctx).Debugf("loading tasks in %v namespace", ns)
		ctx = namespaces.WithNamespace(ctx, ns)
		containers, err := l.client.Containers(ctx)
		if err != nil {
			CubeLog.WithContext(ctx).Warnf("loading tasks in %v namespace fail:%v", err)
			continue
		}
		for _, cnt := range containers {
			opts := &workflow.DestroyContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					SandboxID: cnt.ID(),
				},
			}
			err := l.Destroy(ctx, opts)
			if err != nil {
				CubeLog.WithContext(ctx).Errorf("Destroy %v fail:%v", cnt.ID(), err)
				continue
			}
		}
	}

	exist, _ := utils.DenExist(l.config.CubeHostSharedDir)
	if exist {
		sandBoxDirs, err := os.ReadDir(l.config.CubeHostSharedDir)
		if err != nil {
			return err
		}
		for _, sandBoxID := range sandBoxDirs {
			if !sandBoxID.IsDir() {
				continue
			}

			containerDirs, err := os.ReadDir(l.getMountPath(sandBoxID.Name()))
			if err != nil {
				mountPath := l.GetSharePath(sandBoxID.Name())
				if er := unmountNoFollow(mountPath); er != nil && er != syscall.ENOENT {
					CubeLog.WithContext(ctx).Errorf("unmount %v fail:%v", mountPath, er)
				}
				continue
			}
			for _, cnt := range containerDirs {
				if _, err := uuid.Parse(cnt.Name()); err == nil {

					mountPath := l.getMountPathRootfsPath(sandBoxID.Name(), cnt.Name())
					if er := unmountNoFollow(mountPath); er != nil && er != syscall.ENOENT {
						CubeLog.WithContext(ctx).Errorf("unmount %v fail:%v", mountPath, er)
					}
				} else {
					mountPath := filepath.Join(l.getMountPath(sandBoxID.Name()), cnt.Name())
					if er := unmountNoFollow(mountPath); er != nil && er != syscall.ENOENT {
						CubeLog.WithContext(ctx).Errorf("unmount %v fail:%v", mountPath, er)
					}
				}
				mountPath := l.GetSharePath(sandBoxID.Name())
				if er := unmountNoFollow(mountPath); er != nil && er != syscall.ENOENT {
					CubeLog.WithContext(ctx).Errorf("unmount %v fail:%v", mountPath, er)
				}
			}
		}
	}

	removeContainers(ctx, l.client)

	time.Sleep(time.Second)

	for _, d := range []string{l.config.RootPath, l.config.CubeHostSharedDir, l.config.NetfilePath} {
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("%v  RemoveAll failed:%v", d, err.Error())
		}
	}

	for _, d := range []string{l.config.RootPath, l.config.CubeHostSharedDir} {
		if err := os.MkdirAll(d, 0711); err != nil {
			return fmt.Errorf("%v  MkdirAll failed:%v", d, err.Error())
		}
	}

	err = l.cubeboxManger.Init(ctx)
	if err != nil {
		return fmt.Errorf("cubeboxManger Init failed:%v", err.Error())
	}

	taskio.Init(taskio.FIFODir(l.config.StatePath))
	return nil
}

func removeContainers(ctx context.Context, client *containerd.Client) {
	nslist, err := client.NamespaceService().List(ctx)
	if err != nil {
		CubeLog.WithContext(ctx).Warnf("loading namespaces fail:%v", err)
		return
	}
	for _, ns := range nslist {
		ctx = namespaces.WithNamespace(ctx, ns)
		containers, err := client.Containers(ctx)
		if err != nil {
			CubeLog.WithContext(ctx).Warnf("loading container fail: %v", err)
			return
		}

		for _, cnt := range containers {
			if err = client.ContainerService().Delete(ctx, cnt.ID()); err != nil && !errdefs.IsNotFound(err) {
				CubeLog.WithContext(ctx).Errorf("remove container %q: %v", cnt.ID(), err)
			}
		}
	}
}

func (l *local) GetSharePath(id string) string {
	return filepath.Join(l.config.CubeHostSharedDir, id, "shared")
}

func (l *local) getMountPath(id string) string {
	return filepath.Join(l.config.CubeHostSharedDir, id, "mounts")
}

func (l *local) getSandboxPath(id string) string {
	return filepath.Join(l.config.CubeHostSharedDir, id)
}

func (l *local) getMountPathRootfsPath(sandBoxID, id string) string {
	return filepath.Join(l.config.CubeHostSharedDir, sandBoxID, "mounts", id, "rootfs")
}

const UmountNoFollow = 0x8

func unmountNoFollow(path string) error {

	return mount.UnmountAll(path, 0)
}

func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {

	if opts == nil {
		return nil
	}
	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"cubeboxID": opts.SandboxID,
		"step":      "cubeboxCleanUp",
	})
	stepLog.Errorf("CleanUp sandBox:%s", opts.SandboxID)
	sandBoxID := opts.SandboxID
	info, err := l.cubeboxManger.Get(ctx, sandBoxID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil
		}
		CubeLog.WithContext(ctx).Warnf("clean up getSandBoxInfo [%s] fail:%v", sandBoxID, err)
		return err
	}

	ns := info.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)

	err = l.cbriManager.DestroySandbox(ctx, info.SandboxID)
	if err != nil {
		log.G(ctx).Errorf("failed to destroy cbri sandbox %s", err.Error())
		return fmt.Errorf("failed to destroy cbri sandbox")
	}

	var ctrLists []*cubeboxstore.Container
	for id := range info.All() {
		ctr, err := info.Get(id)
		if err != nil {
			stepLog.Warnf("CleanUp get container %s fail: %v", sandBoxID, err)
			continue
		}
		ctrLists = append(ctrLists, ctr)
	}
	ctrLists = append(ctrLists, info.FirstContainer())
	for _, ctr := range ctrLists {
		err = l.stopTask(ctx, ctr.Container)
		if err != nil {
			stepLog.Warnf("CleanUp stopTask %s fail: %v", sandBoxID, err)
		}
	}
	if info.GetStatus() != nil &&
		info.GetStatus().Get().Pid != 0 &&
		utils.ProcessExists(ctx, int(info.GetStatus().Get().Pid)) {
		return fmt.Errorf("shim process still Exists [%s]", sandBoxID)
	}

	var (
		result *multierror.Error
	)

	umountFn := func(sandBoxID, id string) error {
		var (
			result *multierror.Error
		)

		containerDirs, err := os.ReadDir(l.getMountPath(sandBoxID))
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		for _, cnt := range containerDirs {
			if cnt.Name() == id {

				mountPath := l.getMountPathRootfsPath(sandBoxID, id)
				if er := unmountNoFollow(mountPath); er != nil && er != syscall.ENOENT {
					result = multierror.Append(result, fmt.Errorf("umount [%s] fail: %w", mountPath, er))
				}
			} else {
				mountPath := filepath.Join(l.getMountPath(sandBoxID), cnt.Name())
				if er := unmountNoFollow(mountPath); er != nil && er != syscall.ENOENT {
					result = multierror.Append(result, fmt.Errorf("umount [%s] fail: %w", mountPath, er))
				}
			}
		}
		return result.ErrorOrNil()
	}

	for id := range info.All() {
		if er := umountFn(sandBoxID, id); er != nil {
			result = multierror.Append(result, err)
		}
	}

	if er := umountFn(sandBoxID, sandBoxID); er != nil {
		result = multierror.Append(result, err)
	}

	mountPath := l.GetSharePath(sandBoxID)
	if er := unmountNoFollow(mountPath); er != nil && er != syscall.ENOENT {
		result = multierror.Append(result, fmt.Errorf("umount [%s] fail: %w", mountPath, er))
	}

	rmPath := l.getMountPath(sandBoxID)
	if er := os.RemoveAll(rmPath); er != nil {
		result = multierror.Append(result, fmt.Errorf("remove [%s] fail: %w", rmPath, er))
	}

	rmPath = filepath.Join("/run/vc/sbs", sandBoxID)
	if er := os.RemoveAll(rmPath); er != nil {
		result = multierror.Append(result, fmt.Errorf("remove [%s] fail: %w", rmPath, er))
	}

	cleanFn := func(id string) error {
		var (
			result *multierror.Error
		)

		cntr, er := l.client.LoadContainer(ctx, id)
		if er != nil {
			if !errdefs.IsNotFound(er) {
				result = multierror.Append(result, fmt.Errorf("LoadContainer [%s] fail: %w", id, er))
			}
		} else {
			if er := deleteContainer(ctx, cntr); er != nil {
				if !errdefs.IsNotFound(er) {
					result = multierror.Append(result, fmt.Errorf("deleteContainer [%s] fail: %w", id, er))
				}
			}
		}

		if er := netfile.Clean(ctx, id); er != nil {
			stepLog.Warnf("clean hostname file failed.%s", er)
		}

		if er := rootfs.CleanRootfs(ctx, id); er != nil {
			stepLog.Warnf("clean rootfs failed.%s", er)
		}

		if er := taskio.Clean(ctx, id); er != nil {
			result = multierror.Append(result, fmt.Errorf("clean up taskio [%s] fail: %w", rmPath, er))
		}
		return result.ErrorOrNil()
	}

	for id, _ := range info.All() {
		if er := cleanFn(id); er != nil {
			result = multierror.Append(result, fmt.Errorf("clean up [%s] fail: %w", id, er))
			stepLog.Warnf("clean up [%s] fail: %s", id, er)
		}
	}

	if er := cleanFn(sandBoxID); er != nil {
		result = multierror.Append(result, fmt.Errorf("clean up [%s] fail: %w", sandBoxID, er))
		stepLog.Warnf("clean up [%s] fail: %s", sandBoxID, er)
	}

	if er := result.ErrorOrNil(); er != nil {
		stepLog.Fatalf("cleanUp [%s] error: %s", constants.CubeboxID, er)
		return er
	}

	err = l.cubeboxManger.Delete(ctx, &cubes.DeleteOption{
		CubeboxID: sandBoxID,
	})
	if err != nil && err != utils.ErrorKeyNotFound {
		stepLog.Errorf("clean up deleteSandBoxInfo fail:%v", err)
		return err
	}
	return nil
}

func makeContainerConfigToSave(cfg *cubebox.ContainerConfig) *cubebox.ContainerConfig {
	ret := &cubebox.ContainerConfig{
		Name:        cfg.GetName(),
		Annotations: maps.Clone(cfg.GetAnnotations()),
		Image:       cfg.GetImage(),
		Resources: &cubebox.Resource{
			Cpu:      cfg.GetResources().GetCpu(),
			CpuLimit: cfg.GetResources().GetCpuLimit(),
			Mem:      cfg.GetResources().GetMem(),
			MemLimit: cfg.GetResources().GetMemLimit(),
		},
		VolumeMounts: cfg.GetVolumeMounts(),
		OciConfig:    cfg.GetOciConfig(),
	}
	if ret.Image.Annotations != nil {

		if _, ok := ret.Image.Annotations[constants.MasterAnnotationsImageUserName]; ok {
			ret.Image.Annotations[constants.MasterAnnotationsImageUserName] = "*"
		}
		if _, ok1 := ret.Image.Annotations[constants.MasterAnnotationsImagetoken]; ok1 {
			ret.Image.Annotations[constants.MasterAnnotationsImagetoken] = "*"
		}
	}
	if cfg.GetPrestop() != nil && cfg.GetPrestop().GetLifecyleHandler() != nil {
		handler := cfg.GetPrestop().GetLifecyleHandler()
		if httpGet := handler.GetHttpGet(); httpGet != nil {
			ret.Prestop = &cubebox.PreStop{}
			inPreStop, _ := protobuf.Marshal(cfg.GetPrestop())
			_ = protobuf.Unmarshal(inPreStop, ret.Prestop)
		}
	}
	if cfg.GetPoststop() != nil && cfg.GetPoststop().GetLifecyleHandler() != nil {
		handler := cfg.GetPoststop().GetLifecyleHandler()
		if httpGet := handler.GetHttpGet(); httpGet != nil {
			ret.Poststop = &cubebox.PostStop{}
			inPostStop, _ := protobuf.Marshal(cfg.GetPoststop())
			_ = protobuf.Unmarshal(inPostStop, ret.Poststop)
		}
	}
	if ret.Annotations == nil {
		ret.Annotations = make(map[string]string)
	}
	return ret
}

func (l *local) CubeboxStore() cubes.CubeboxAPI {
	return l.cubeboxManger
}

func (l *local) GetImageService() *cubeimages.CubeImageService {
	return l.criImage
}

func (l *local) GetSnapshotter(snapshotter string) (snapshots.Snapshotter, error) {
	sn := l.client.SnapshotService(snapshotter)
	if sn == nil {
		return nil, fmt.Errorf("snapshotter %s not found: %w", snapshotter, errdefs.ErrNotFound)
	}
	return sn, nil
}

func (l *local) IsCubeboxExists(ctx context.Context, sandBoxID string) (bool, error) {
	_, err := l.cubeboxManger.Get(ctx, sandBoxID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
