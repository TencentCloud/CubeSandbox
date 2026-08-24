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
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
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
	Base   string
	Spec   string
	ResDir string
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
	if appImageID == "" {
		annotations[constants.AnnotationsVMOSImagePath] = filepath.Join(e.config.BasePath, "cube-image", "cube-guest-image-cpu.img")
		annotations[constants.AnnotationsVMAgentPath] = filepath.Join(e.config.BasePath, "cube-agent", "cube-agent.ext4")
	}

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
			paths, err := e.resolveSnapshotPaths(templateID, snapshotRestoreRawPath(ctx, flowOpts), flowOpts.ReqInfo)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
			}
			if err := storage.EnsureShimSpecDirLink(paths.Base, paths.ResDir); err != nil {
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
			}
			backend := pauseResumeCatalogBackend(flowOpts)
			if flowOpts.IsPauseResume() {
				// Resume: clone sealed metadata snap → RW volume, then mount
				// (MountS3MetadataAt never activates the package snap for IO).
				metaMount := filepath.Join(paths.Base, storage.SnapshotMetadataDir)
				mounted, err := mountRestoreMetadata(ctx, flowOpts, backend, templateID, metaMount)
				if err != nil {
					return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
				}
				snapBasePath, snapSpecPath = restorePathsFromMetadataMount(mounted, paths)
			} else {
				// Create from template／snapshot: private metadata volume
				// mounted under the sandbox-owned package path — never on
				// the parent template metadata/ mount.
				childID := strings.TrimSpace(flowOpts.GetSandboxID())
				if childID == "" {
					childID = templateID
				}
				snapBasePath = paths.Base
				snapSpecPath = paths.Spec
				if isS3CatalogCreateBackend(backend) {
					sandboxHome := storage.SnapshotHome(backend, storage.SnapshotKindNormal, childID)
					metaMount := filepath.Join(sandboxHome, storage.SnapshotMetadataDir)
					mounted, err := mountRestoreMetadata(ctx, flowOpts, backend, templateID, metaMount)
					if err != nil {
						return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
					}
					snapBasePath, snapSpecPath = restorePathsFromMetadataMount(mounted, paths)
					if err := storage.EnsureShimSpecDirLink(snapBasePath, paths.ResDir); err != nil {
						return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
					}
				} else {
					metaMount := filepath.Join(paths.Base, storage.SnapshotMetadataDir)
					if err := storage.CloneS3MetadataFromParent(ctx, backend, templateID, childID, metaMount); err != nil {
						return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
					}
				}
			}

			kernelPath, imagePath, err := e.resolveSnapshotRuntimeArtifacts(snapSpecPath, flowOpts.LocalRunTemplate)
			if err != nil {
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, err.Error())
			}
			annotations[constants.AnnotationsVMKernelPath] = kernelPath
			annotations[constants.AnnotationsVMOSImagePath] = imagePath
			if agentPath := resolveTemplateComponentPath(flowOpts.LocalRunTemplate, templatetypes.CubeComponentCubeAgent); agentPath != "" {
				annotations[constants.AnnotationsVMAgentPath] = agentPath
			}
		} else {

			snapBasePath = filepath.Join(e.getSnapShotFilePath(templateID), "")
			if exists, err := utils.DenExist(snapBasePath); err != nil || !exists {
				logEntry.Errorf("check snapshot path %s failed: %v", snapBasePath, err)
				return nil, ret.Err(errorcode.ErrorCode_AppSnapshotNotExist, "snapshot not exist")
			}
		}

		annotations[constants.AnnotationVMSnapshotPath] = snapBasePath
		memoryVolURL := snapshotRestoreMemoryVolURLFromStorageInfo(flowOpts)
		if memoryVolURL != "" {
			annotations[constants.AnnotationVMSnapshotMemoryVolURL] = memoryVolURL
			logEntry.WithField("memory_vol_url", memoryVolURL).Warnf("resolved snapshot restore memory volume")
		} else {
			// v4: physical refs live in cubelet's snapshot catalog keyed by
			// logical id (runtime snapshot id / appsnapshot template id).
			// If neither annotation is present, the request is not a snapshot
			// restore at all; otherwise the catalog entry for the logical id
			// is missing/empty and Cubelet has already returned a fail-fast
			// error upstream of this point.
			logEntry.WithFields(CubeLog.Fields{
				"runtime_snapshot_id": realReq.GetAnnotations()[constants.MasterAnnotationRuntimeSnapshotID],
				"appsnapshot_tpl_id":  realReq.GetAnnotations()[constants.MasterAnnotationAppSnapshotTemplateID],
			}).Warnf("missing snapshot restore memory volume")
		}

		annotations[constants.AnnotationAppSnapshotRestore] = "true"

		annotations[constants.AnnotationAppSnapshotContainerID] = snapshotRestoreContainerID(templateID, snapSpecPath)

		// Pause / FromSnap: guest virtiofs + container binds are already live
		// in restored memory. Stamp pause.snapshot.id on the sandbox OCI spec
		// so shim skips replaying virtio-fs storages to the agent (same signal
		// Pause uses). Do not emit PropagationExecMounts. Still emit
		// AnnotationVirtiofs so the hypervisor reconnects host paths.
		if flowOpts.IsGuestMountRestore() {
			pauseID := strings.TrimSpace(realReq.GetAnnotations()[constants.MasterAnnotationPauseSnapshotID])
			if pauseID == "" {
				pauseID = strings.TrimSpace(realReq.GetAnnotations()[constants.MasterAnnotationRuntimeSnapshotID])
			}
			if pauseID != "" {
				annotations[constants.MasterAnnotationPauseSnapshotID] = pauseID
			}
		}

		sandbox := cubeboxstore.GetCubeBox(ctx)
		if sandbox != nil && sandbox.FirstContainer() != nil {
			if !flowOpts.IsGuestMountRestore() {
				opts, err := generateRestoreVirtiofsOpt(ctx, flowOpts, sandbox.FirstContainer().Config)
				if err != nil {
					return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
				}
				specOpts = append(specOpts, opts...)
			}
			opts, err := generateSandboxVirtiofsOpt(ctx, flowOpts, false)
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
			// Pause / FromSnap keep existing container mounts; only create-from-
			// template needs propagation exec.mount annotations for do_exec_mount.
			if !flowOpts.IsGuestMountRestore() {
				opts, err := generateRestoreVirtiofsOpt(ctx, flowOpts, c.Config)
				if err != nil {
					return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
				}
				specOpts = append(specOpts, opts...)
			}
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
	return pmem.RefreshKernelFile(
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
		kernelPath = resolveTemplateComponentPath(localTemplate, templatetypes.CubeComponentCubeKernel)
		imagePath = resolveTemplateComponentPath(localTemplate, templatetypes.CubeComponentCubeImage)
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

func resolveTemplateComponentPath(localTemplate *templatetypes.LocalRunTemplate, name templatetypes.CubeComponent) string {
	if localTemplate == nil || localTemplate.Componts == nil {
		return ""
	}
	component, ok := localTemplate.Componts[name]
	if !ok {
		return ""
	}
	return strings.TrimSpace(component.Component.Path)
}

// snapshotRestoreRawPath is the hypervisor snapshot package Cubelet hands
// the shim. Pause resume must use the pause catalog metadata home even
// though LocalRunTemplate is the original tpl-* (needed for kernel／image
// EnsureCubeRunTemplate). Using the template 2C2000M path with pause
// memory hangs shim Task.Create.
func snapshotRestoreRawPath(ctx context.Context, flowOpts *workflow.CreateContext) string {
	raw := ""
	if flowOpts != nil && flowOpts.LocalRunTemplate != nil {
		raw = strings.TrimSpace(flowOpts.LocalRunTemplate.Snapshot.Snapshot.Path)
	}
	if flowOpts == nil || !flowOpts.IsPauseResume() {
		return raw
	}
	// Cross-node there is no package to look up, and the run template was
	// recovered from the sandbox's own imported metadata, so raw already
	// points at it.
	if storage.CrossNodeSandboxImport(flowOpts.ReqInfo.GetAnnotations()) != nil {
		return raw
	}
	snapID, ok := flowOpts.GetSnapshotTemplateID()
	if !ok {
		return raw
	}
	if meta := pauseCatalogMetaDir(ctx, snapID, pauseResumeCatalogBackend(flowOpts)); meta != "" {
		return meta
	}
	return raw
}

func pauseResumeCatalogBackend(flowOpts *workflow.CreateContext) string {
	if flowOpts == nil || flowOpts.ReqInfo == nil {
		return cow.BackendXFS
	}
	if raw := strings.TrimSpace(flowOpts.ReqInfo.GetBackend()); raw != "" {
		if b, err := cow.NormalizeBackend(raw); err == nil {
			return b
		}
	}
	raw := strings.TrimSpace(flowOpts.ReqInfo.GetAnnotations()[constants.MasterAnnotationStorageBackend])
	if raw == "" {
		return cow.BackendXFS
	}
	b, err := cow.NormalizeBackend(raw)
	if err != nil {
		return cow.BackendXFS
	}
	return b
}

func isS3CatalogCreateBackend(backend string) bool {
	normalized, err := cow.NormalizeBackend(backend)
	return err == nil && normalized == cow.BackendS3
}

// mountRestoreMetadata puts the metadata disk this sandbox reads its
// description from in place, and reports where it landed.
//
// Cross-node it is the sandbox's own imported copy, mounted under the sandbox
// home by whichever step first knew the sandbox id — the Create entry for a
// Resume, this call for a create from a template or snapshot. Same-node a
// Resume mounts the pause package's sealed disk, and a create clones the
// parent's into a private volume so a child never writes on a parent's
// metadata.
// restorePathsFromMetadataMount turns the directory mountRestoreMetadata
// returned (the metadata mount) into the snapshot home + spec paths the
// hypervisor annotations use. Cross-node that directory is the sandbox's
// imported copy, not the template package.
func restorePathsFromMetadataMount(mounted string, paths *snapshotPaths) (base, spec string) {
	if paths == nil {
		return "", ""
	}
	mounted = strings.TrimSpace(mounted)
	if mounted == "" {
		return paths.Base, paths.Spec
	}
	base = filepath.Clean(filepath.Dir(mounted))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return paths.Base, paths.Spec
	}
	spec = paths.Spec
	if paths.ResDir != "" {
		spec = filepath.Join(base, paths.ResDir)
	}
	return base, spec
}

func mountRestoreMetadata(ctx context.Context, flowOpts *workflow.CreateContext,
	backend, packageID, mountPath string) (string, error) {
	if imp := storage.CrossNodeSandboxImport(flowOpts.ReqInfo.GetAnnotations()); imp != nil {
		return imp.EnsureMetadata(ctx)
	}
	if flowOpts.IsPauseResume() {
		return mountPath, storage.MountS3MetadataAt(ctx, backend, packageID, mountPath)
	}
	sandboxID := strings.TrimSpace(flowOpts.GetSandboxID())
	if sandboxID == "" {
		sandboxID = packageID
	}
	return mountPath, storage.CloneS3MetadataFromParent(ctx, backend, packageID, sandboxID, mountPath)
}

func pauseCatalogMetaDir(ctx context.Context, snapID, preferred string) string {
	seen := map[string]struct{}{}
	for _, backend := range []string{preferred, cow.BackendXFS, cow.BackendS3} {
		backend = strings.TrimSpace(backend)
		if backend == "" {
			continue
		}
		if _, ok := seen[backend]; ok {
			continue
		}
		seen[backend] = struct{}{}
		entry, err := storage.GetLocalSnapshotFor(ctx, backend, snapID)
		if err != nil || entry == nil {
			continue
		}
		if meta := strings.TrimSpace(entry.MetaDir); meta != "" {
			return meta
		}
		if home := strings.TrimSpace(entry.SnapshotPath); home != "" {
			return filepath.Join(home, storage.SnapshotMetadataDir)
		}
	}
	return ""
}

func (e *cubeboxInstancePlugin) resolveSnapshotPaths(templateID, rawPath string, req *cubebox.RunCubeSandboxRequest) (*snapshotPaths, error) {
	resDir, err := inferSnapshotResDirFromRequest(req)
	if err != nil {
		return nil, err
	}

	path := normalizeSnapshotRestorePath(strings.TrimSpace(rawPath))
	if path == "" {
		base := e.getSnapShotFilePath(templateID)
		return &snapshotPaths{
			Base:   base,
			Spec:   filepath.Join(base, resDir),
			ResDir: resDir,
		}, nil
	}

	if looksLikeSnapshotSpecPath(path) || looksLikeSnapshotSpecDir(path) {
		return &snapshotPaths{
			Base:   filepath.Dir(path),
			Spec:   path,
			ResDir: resDir,
		}, nil
	}

	return &snapshotPaths{
		Base:   path,
		Spec:   filepath.Join(path, resDir),
		ResDir: resDir,
	}, nil
}

func normalizeSnapshotRestorePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if !strings.HasSuffix(base, ".tmp") {
		return clean
	}
	return filepath.Join(filepath.Dir(clean), strings.TrimSuffix(base, ".tmp"))
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

func snapshotRestoreMemoryVolURLFromStorageInfo(flowOpts *workflow.CreateContext) string {
	if flowOpts == nil || flowOpts.StorageInfo == nil {
		return ""
	}
	info, ok := flowOpts.StorageInfo.(*storage.StorageInfo)
	if !ok || info == nil {
		return ""
	}
	return strings.TrimSpace(info.RestoreMemoryVolURL)
}

type snapshotMetadata struct {
	AppSnapshotContainerID string `json:"app_snapshot_container_id,omitempty"`
}

func snapshotRestoreContainerID(templateID, snapshotSpecPath string) string {
	fallback := strings.TrimSpace(templateID) + "_0"
	metadataPath := filepath.Join(filepath.Clean(snapshotSpecPath), "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return fallback
	}
	var metadata snapshotMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fallback
	}
	if value := strings.TrimSpace(metadata.AppSnapshotContainerID); value != "" {
		return value
	}
	return fallback
}

func looksLikeSnapshotSpecPath(path string) bool {
	if !looksLikeSnapshotSpecDir(path) {
		return false
	}
	_, err := os.Stat(filepath.Join(path, "metadata.json"))
	return err == nil
}

func looksLikeSnapshotSpecDir(path string) bool {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if strings.HasSuffix(base, "M") && strings.Contains(base, "C") {
		return true
	}
	if _, err := os.Stat(filepath.Join(clean, "snapshot", "config.json")); err == nil {
		return true
	}
	return false
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
