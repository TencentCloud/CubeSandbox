// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cbri"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/api/resource"
)

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.PluginCBRI,
		ID:     "cubebox",
		Config: defaultConfig(),
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			config := ic.Config.(*cubeboxInstancePluginConfig)
			return &cubeboxInstancePlugin{
				config: config,
			}, nil
		},
	})
}

type cubeboxInstancePluginConfig struct {
	BasePath         string `toml:"base_path,omitempty"`
	ImageBasePath    string `toml:"image_base_path,omitempty"`
	KernelBasePath   string `toml:"kernel_base_path,omitempty"`
	SnapShotBasePath string `toml:"snapshot_base_path,omitempty"`
	instanceType     string
}

func defaultConfig() *cubeboxInstancePluginConfig {
	cfg := &cubeboxInstancePluginConfig{
		BasePath: "/usr/local/services/cubetoolbox",
	}
	cfg.ImageBasePath = filepath.Join(cfg.BasePath, "cubebox_os_image")
	cfg.KernelBasePath = filepath.Join(cfg.BasePath, "cubebox_os_image")
	cfg.SnapShotBasePath = filepath.Join(cfg.BasePath, "cube-snapshot")
	cfg.instanceType = cubebox.InstanceType_cubebox.String()
	return cfg
}

type snapshotConfig struct {
	Payload struct {
		Kernel string `json:"kernel"`
	} `json:"payload"`
	Pmem []struct {
		File string `json:"file"`
		ID   string `json:"id"`
	} `json:"pmem"`
}

type snapshotPaths struct {
	Base string
	Spec string
}

type cubeboxInstancePlugin struct {
	config *cubeboxInstancePluginConfig
}

func (e *cubeboxInstancePlugin) PostCreateContainer(ctx context.Context, cb *cubeboxstore.CubeBox, container *cubeboxstore.Container) error {
	_ = cb
	_ = container
	return nil
}

func (e *cubeboxInstancePlugin) GetPassthroughMounts(ctx context.Context, flowOpts *workflow.CreateContext) ([]specs.Mount, error) {
	return nil, nil
}

func (e *cubeboxInstancePlugin) CreateSandbox(ctx context.Context, flowOpts *workflow.CreateContext) ([]oci.SpecOpts, error) {
	var (
		specOpts []oci.SpecOpts
		logEntry = log.G(ctx)
	)
	if flowOpts.GetInstanceType() != e.config.instanceType {
		return specOpts, nil
	}

	fileMode := os.FileMode(0o666)
	var uid uint32 = 0xffffffff
	specOpts = append(specOpts, oci.WithLinuxDevices([]specs.LinuxDevice{
		{
			Path:     "/dev/console",
			Type:     "c",
			Major:    5,
			Minor:    1,
			FileMode: &fileMode,
			UID:      &uid,
			GID:      &uid,
		},
		{
			Path:     "/dev/kmsg",
			Type:     "c",
			Major:    1,
			Minor:    11,
			FileMode: &fileMode,
			UID:      &uid,
			GID:      &uid,
		},
	}))

	specOpts = append(specOpts, oci.WithPrivileged)

	specOpts = append(specOpts, replaceDevMounts()...)
	realReq := flowOpts.ReqInfo

	for _, c := range realReq.Containers {
		if c.GetImage().GetStorageMedia() == cubeimages.ImageStorageMediaType_ext4.String() {
			specOpts = append(specOpts, e.genPmemOpt(ctx, c.GetImage().GetImage()))
		}
	}

	var (
		annotations = make(map[string]string)
		kernelPath  string
	)

	appImageID := constants.GetAppImageID(ctx)
	if appImageID == "" {
		kernelPath = filepath.Join(e.config.BasePath, "cube-kernel-scf", "vmlinux")
	} else {
		if flowOpts.IsCreateSnapshot() {
			if err := e.syncLatestKernelForImage(ctx, appImageID); err != nil {
				return nil, err
			}
		}
		kernelPath = e.getKernelFilePath(appImageID)
		rootfs := filepath.Join(e.config.ImageBasePath, appImageID)
		specOpts = append(specOpts, oci.WithRootFSPath(rootfs))
		logEntry = logEntry.WithField("rootfs", rootfs)
	}
	annotations[constants.AnnotationsVMKernelPath] = kernelPath
	annotations[constants.AnnotationsProduct] = e.config.instanceType

	if flowOpts.IsCreateSnapshot() {
		annotations[constants.AnnotationAppSnapshotCreate] = "true"

		virtiofsAnnotations, err := generateEmptyVirtiofsDevices(ctx)
		if err != nil {
			return nil, err
		}
		specOpts = append(specOpts, oci.WithAnnotations(virtiofsAnnotations))

	} else if templateID, ok := flowOpts.GetSnapshotTemplateID(); ok {

		var snapBasePath, snapSpecPath string

		if flowOpts.IsCubeboxV2() {
			if flowOpts.LocalRunTemplate == nil {
				logEntry.Errorf("check snapshot path failed: %s", "local run template is nil")
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, "local snapshot not exist")
			}
			paths, err := e.resolveSnapshotPaths(templateID, flowOpts.LocalRunTemplate.Snapshot.Snapshot.Path, flowOpts.ReqInfo)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
			}
			snapBasePath = paths.Base
			snapSpecPath = paths.Spec

			kernelPath, imagePath, err := e.resolveSnapshotRuntimeArtifacts(snapSpecPath, flowOpts.LocalRunTemplate)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
			}
			annotations[constants.AnnotationsVMKernelPath] = kernelPath
			annotations[constants.AnnotationsVMImagePath] = imagePath
		} else {

			snapBasePath = filepath.Join(e.getSnapShotFilePath(templateID), "")
			if exists, err := utils.DenExist(snapBasePath); err != nil || !exists {
				logEntry.Errorf("check snapshot path %s failed: %v", snapBasePath, err)
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, "snapshot not exist")
			}
		}

		annotations[constants.AnnotationVMSnapshotPath] = snapBasePath

		annotations[constants.AnnotationAppSnapshotRestore] = "true"

		annotations[constants.AnnotationAppSnapshotContainerID] = templateID + "_0"

		sandbox := cubeboxstore.GetCubeBox(ctx)
		if sandbox != nil && sandbox.FirstContainer() != nil {
			opts, err := generateRestoreVirtiofsOpt(ctx, flowOpts, sandbox.FirstContainer().Config)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
			}
			specOpts = append(specOpts, opts...)
			opts, err = generateSandboxVirtiofsOpt(ctx, flowOpts, false)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
			}
			specOpts = append(specOpts, opts...)

		}
	} else {

		annotations[constants.AnnotationSnapshotDisable] = "true"
		sandbox := cubeboxstore.GetCubeBox(ctx)
		if sandbox != nil && sandbox.FirstContainer() != nil {
			opts, err := generateSandboxVirtiofsOpt(ctx, flowOpts, true)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
			}
			specOpts = append(specOpts, opts...)
		}
	}

	if log.IsDebug() {
		logEntry.WithFields(CubeLog.Fields{
			"annotations": annotations,
		}).Debugf("create sandbox annotations")
	}
	specOpts = append(specOpts, oci.WithAnnotations(annotations))

	videoOpts, err := e.genVideoAnnotationOpt(ctx, flowOpts)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "gen video annotation failed: %v", err)
	}
	specOpts = append(specOpts, videoOpts...)
	return specOpts, nil
}

func (e *cubeboxInstancePlugin) CreateContainer(ctx context.Context, cubeBox *cubeboxstore.CubeBox, c *cubeboxstore.Container) ([]oci.SpecOpts, error) {
	var specOpts []oci.SpecOpts
	if cubeBox.InstanceType != e.config.instanceType {
		return specOpts, nil
	}
	specOpts = append(specOpts, replaceDevMounts()...)
	if constants.GetAppImageID(ctx) != "" {
		specOpts = append(specOpts, oci.WithRootFSPath(filepath.Join(e.config.ImageBasePath, constants.GetAppImageID(ctx))))
	}
	flowOpts := workflow.GetCreateContext(ctx)
	if flowOpts != nil {
		if flowOpts.IsCreateSnapshot() {
			specOpts = append(specOpts, oci.WithAnnotations(map[string]string{

				constants.AnnotationPropagationContainerMounts: virtiofs.GenPropagationContainerDirs(),
			}))
		} else if templateID, ok := flowOpts.GetSnapshotTemplateID(); ok {

			snapshotContainerID := templateID
			if innerIndex, ok := ctx.Value(constants.KCubeIndexContext).(string); ok {
				snapshotContainerID += "_" + innerIndex
			}
			specOpts = append(specOpts, oci.WithAnnotations(map[string]string{
				constants.AnnotationAppSnapshotContainerID: snapshotContainerID,
			}))
			opts, err := generateRestoreVirtiofsOpt(ctx, flowOpts, c.Config)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
			}
			specOpts = append(specOpts, opts...)
		}
	}

	if opts, err := e.generateCubeMsgOpt(ctx, c.Config); err != nil {
		return nil, err
	} else {
		specOpts = append(specOpts, opts...)
	}
	return specOpts, nil
}

func (e *cubeboxInstancePlugin) generateCubeMsgOpt(ctx context.Context, containerReq *cubebox.ContainerConfig) ([]oci.SpecOpts, error) {
	var specOpts []oci.SpecOpts
	flowOpts := workflow.GetCreateContext(ctx)
	if flowOpts == nil || containerReq == nil {
		return specOpts, nil
	}

	realReq := flowOpts.ReqInfo
	cubeMsgVolumeName := ""
	for _, v := range realReq.Volumes {
		if v.GetVolumeSource() == nil {
			continue
		}
		if v.GetVolumeSource().GetEmptyDir() == nil {
			continue
		}
		if v.GetVolumeSource().GetEmptyDir().GetMedium() == cubebox.StorageMedium_StorageMediumCubeMsg {
			if cubeMsgVolumeName != "" {
				return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, "only support one cube msg volume")
			}
			log.G(ctx).Debugf("req GetEmptyDir:%+v,vName:%s",
				v.GetVolumeSource().GetEmptyDir(), v.Name)
			cubeMsgVolumeName = v.Name
			break
		}
	}

	if cubeMsgVolumeName != "" {
		specOpts = append(specOpts, oci.WithAnnotations(map[string]string{
			constants.AnnotationsCubeMsgKey: constants.CubeMsgDevDefaultName,
		}))
	}
	return specOpts, nil
}

func (e *cubeboxInstancePlugin) DestroySandbox(ctx context.Context, sandboxID string) error {
	return nil
}

func (e *cubeboxInstancePlugin) genPmemOpt(ctx context.Context, imageID string) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, ctr *containers.Container, spec *oci.Spec) error {
		var oldPmems []pmem.CubePmem

		if spec.Annotations == nil {
			spec.Annotations = make(map[string]string)
		}
		oldValues, ok := spec.Annotations[constants.AnnotationPmem]
		if ok {
			if err := json.Unmarshal([]byte(oldValues), &oldPmems); err != nil {
				return fmt.Errorf("failed to unmarshal pmem config: %v", err)
			}
		}
		id := 0
		for _, pmem := range oldPmems {

			if strings.HasPrefix(pmem.ID, constants.AnnotationPmemCubeBoxImageIDPrefix) {
				id++
			}
		}

		filePath := e.getImageFilePath(imageID)
		var fileSize int64
		fileInfo, _ := os.Stat(filePath)
		if fileInfo != nil {
			fileSize = fileInfo.Size()
		}

		oldPmems = append(oldPmems, pmem.CubePmem{
			File:          filePath,
			DiscardWrites: true,
			SourceDir:     "/",
			FsType:        "ext4",
			Size:          fileSize,
			ID:            fmt.Sprintf("%s-%d", constants.AnnotationPmemCubeBoxImageIDPrefix, id),
		})

		pmemAnno, err := json.Marshal(oldPmems)
		if err != nil {
			return fmt.Errorf("failed to marshal pmem config: %v", err)
		}
		log.G(ctx).Debugf("%s:%s", constants.AnnotationPmem, string(pmemAnno))
		spec.Annotations[constants.AnnotationPmem] = string(pmemAnno)
		return nil
	}
}

func (e *cubeboxInstancePlugin) getImageFilePath(imageID string) string {
	return filepath.Join(e.config.ImageBasePath, imageID, imageID+".ext4")
}

func (e *cubeboxInstancePlugin) syncLatestKernelForImage(ctx context.Context, imageID string) error {
	return pmem.SyncKernelFile(
		ctx,
		filepath.Join(e.config.BasePath, "cube-kernel-scf", "vmlinux"),
		e.getKernelFilePath(imageID),
	)
}

func (e *cubeboxInstancePlugin) getKernelFilePath(imageID string) string {
	return filepath.Join(e.config.KernelBasePath, imageID, imageID+".vm")
}

func (e *cubeboxInstancePlugin) getSnapShotFilePath(templateID string) string {
	return filepath.Join(e.config.SnapShotBasePath, e.config.instanceType, templateID)
}

func (e *cubeboxInstancePlugin) resolveSnapshotRuntimeArtifacts(
	snapshotPath string, localTemplate *templatetypes.LocalRunTemplate,
) (string, string, error) {
	var kernelPath string
	var imagePath string

	if localTemplate != nil {
		if component, ok := localTemplate.Componts[templatetypes.CubeComponentCubeKernel]; ok {
			kernelPath = strings.TrimSpace(component.Component.Path)
		}
		if component, ok := localTemplate.Componts[templatetypes.CubeComponentCubeImage]; ok {
			imagePath = strings.TrimSpace(component.Component.Path)
		}
	}
	if kernelPath != "" && imagePath != "" {
		return kernelPath, imagePath, nil
	}

	cfgPath := filepath.Join(snapshotPath, "snapshot", "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if kernelPath == "" {
			return "", "", fmt.Errorf("template have no kernel component: read %s failed: %w", cfgPath, err)
		}
		return "", "", fmt.Errorf("template have no os image component: read %s failed: %w", cfgPath, err)
	}

	var cfg snapshotConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", fmt.Errorf("parse snapshot config %s failed: %w", cfgPath, err)
	}

	if kernelPath == "" {
		kernelPath = strings.TrimSpace(cfg.Payload.Kernel)
	}
	if imagePath == "" {
		for _, p := range cfg.Pmem {
			if strings.HasPrefix(p.ID, constants.AnnotationPmemCubeBoxImageIDPrefix) && strings.TrimSpace(p.File) != "" {
				imagePath = strings.TrimSpace(p.File)
				break
			}
		}
	}

	if kernelPath == "" {
		return "", "", fmt.Errorf("template have no kernel component")
	}
	if imagePath == "" {
		return "", "", fmt.Errorf("template have no os image component")
	}
	return kernelPath, imagePath, nil
}

func (e *cubeboxInstancePlugin) resolveSnapshotPaths(templateID, rawPath string, req *cubebox.RunCubeSandboxRequest) (*snapshotPaths, error) {
	resDir, err := inferSnapshotResDirFromRequest(req)
	if err != nil {
		return nil, err
	}

	path := strings.TrimSpace(rawPath)
	if path == "" {
		base := e.getSnapShotFilePath(templateID)
		return &snapshotPaths{
			Base: base,
			Spec: filepath.Join(base, resDir),
		}, nil
	}

	if looksLikeSnapshotSpecPath(path) {
		return &snapshotPaths{
			Base: filepath.Dir(path),
			Spec: path,
		}, nil
	}

	return &snapshotPaths{
		Base: path,
		Spec: filepath.Join(path, resDir),
	}, nil
}

func inferSnapshotResDirFromRequest(req *cubebox.RunCubeSandboxRequest) (string, error) {
	if req == nil || len(req.Containers) == 0 || req.Containers[0].GetResources() == nil {
		return "", fmt.Errorf("local snapshot not exist")
	}

	resources := req.Containers[0].GetResources()

	cpuQ, err := resource.ParseQuantity(resources.GetCpu())
	if err != nil {
		return "", fmt.Errorf("parse snapshot cpu resource failed: %w", err)
	}
	memQ, err := resource.ParseQuantity(resources.GetMem())
	if err != nil {
		return "", fmt.Errorf("parse snapshot memory resource failed: %w", err)
	}

	cpu := int(math.Ceil(float64(cpuQ.MilliValue()) / 1000.0))
	mem := int(memQ.Value() / (1024 * 1024))
	if cpu <= 0 || mem <= 0 {
		return "", fmt.Errorf("local snapshot not exist")
	}

	return fmt.Sprintf("%dC%dM", cpu, mem), nil
}

func looksLikeSnapshotSpecPath(path string) bool {
	base := filepath.Base(filepath.Clean(path))
	if !strings.HasSuffix(base, "M") || !strings.Contains(base, "C") {
		return false
	}
	_, err := os.Stat(filepath.Join(path, "metadata.json"))
	return err == nil
}

var _ cbri.API = &cubeboxInstancePlugin{}

func replaceDevMounts() []oci.SpecOpts {
	var specOpts []oci.SpecOpts

	specOpts = append(specOpts, oci.WithoutMounts("/dev"))

	mounts := []specs.Mount{
		{

			Type:        constants.MountTypeCgroup,
			Source:      "cgroup",
			Destination: "/sys/fs/cgroup",
			Options: []string{
				constants.MountOptReadWrite, constants.MountOptNoDev,
				constants.MountOptNoSuid, constants.MountOptNoExec,
			},
		},
		{
			Type:        "debugfs",
			Source:      "none",
			Destination: "/sys/kernel/debug",
			Options: []string{
				constants.MountOptReadWrite, constants.MountOptNoDev,
				constants.MountOptNoSuid, constants.MountOptNoExec,
			},
		},
		{
			Destination: "/dev",
			Type:        "bind",
			Source:      "/dev",
			Options:     []string{constants.MountOptBind, constants.MountPropagationRShared},
		},

		{
			Destination: "/dev/console",
			Type:        "bind",
			Source:      "/dev/null",
			Options:     []string{constants.MountOptBind},
		},
		{
			Destination: "/dev/pts",
			Type:        "devpts",
			Source:      "devpts",
			Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
		},
		{
			Destination: "/dev/shm",
			Type:        "tmpfs",
			Source:      "shm",
			Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
		},
		{
			Destination: "/dev/mqueue",
			Type:        "mqueue",
			Source:      "mqueue",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
	}

	specOpts = append(specOpts, oci.WithMounts(mounts))
	return specOpts
}
