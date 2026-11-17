// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/pkg/oci"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// generateSandboxVirtiofsOpt declares the devices that carry host-dir and
// plugin volumes. mountIntoSharedDir picks the shared path that memory restore
// relies on; otherwise each device gets its private path.
func generateSandboxVirtiofsOpt(ctx context.Context, flowOpts *workflow.CreateContext, mountIntoSharedDir bool) ([]oci.SpecOpts, error) {
	var specOpts []oci.SpecOpts
	if flowOpts.StorageInfo == nil {
		return specOpts, nil
	}
	storageInfo, ok := flowOpts.StorageInfo.(*storage.StorageInfo)
	if !ok || storageInfo == nil || len(storageInfo.HostDirBackendInfos) == 0 {
		return specOpts, nil
	}

	type channelKey struct {
		shareDir string
		readOnly bool
	}
	type channelVal struct {
		bindPaths []string
	}
	channels := make(map[channelKey]*channelVal)
	for key, info := range storageInfo.HostDirBackendInfos {
		if info == nil {
			return nil, fmt.Errorf("host-dir backend %q has no volume record", key)
		}
		k := channelKey{shareDir: info.ShareDir, readOnly: info.ReadOnly}
		if channels[k] == nil {
			channels[k] = &channelVal{}
		}
		channels[k].bindPaths = append(channels[k].bindPaths, info.BindPath)
	}

	var allVirtios []*virtiofs.VirtiofsConfig
	for k, v := range channels {
		cfg := &virtiofs.VirtiofsConfig{
			VirtioBackendFsConfig: virtiofs.VirtioBackendFsConfig{
				SharedDir:   k.shareDir,
				AllowedDirs: v.bindPaths,
				ReadOnly:    k.readOnly,
				Cache:       constants.VirtiofsCacheNone,
				RemapFilter: flowOpts.IsGuestMountRestore(),
			},
		}
		cfg.ID = virtiofs.Channel(k.readOnly)
		if mountIntoSharedDir {
			cfg.PropagationMountName = cfg.ID
		}

		limit, err := flowOpts.GetQos(constants.MasterAnnotationsFSQos)
		if err != nil {
			return nil, err
		}
		if limit != nil {
			cfg.RateLimiter = *limit
		}
		allVirtios = append(allVirtios, cfg)
		log.G(ctx).Infof("[hostdir] generateSandboxVirtiofsOpt: channel id=%s shareDir=%s allowed=%v mountIntoSharedDir=%v",
			cfg.ID, k.shareDir, v.bindPaths, mountIntoSharedDir)
	}

	if len(allVirtios) > 0 {
		vc, _ := jsoniter.Marshal(allVirtios)
		specOpts = append(specOpts, oci.WithAnnotations(map[string]string{
			constants.AnnotationVirtiofs: string(vc),
		}))
		log.G(ctx).Infof("[hostdir] %s annotation set: %s", constants.AnnotationVirtiofs, string(vc))
	}
	return specOpts, nil
}

func generateRestoreVirtiofsOpt(ctx context.Context, flowOpts *workflow.CreateContext, containerReq *cubebox.ContainerConfig) ([]oci.SpecOpts, error) {
	var specOpts []oci.SpecOpts
	// Pause / FromSnap: container /mnt/* binds are already in guest memory.
	// Re-emitting exec.mounts would re-enter do_exec_mount and fail (ENOENT on
	// /.container_rw/... or EBUSY on virtio_rw). Create-from-template still
	// needs these annotations for newly attached volumes.
	if flowOpts != nil && flowOpts.IsGuestMountRestore() {
		log.G(ctx).Infof("[hostdir] memory restore: skip propagation exec.mount / umount annotations")
		return specOpts, nil
	}
	if flowOpts == nil || flowOpts.StorageInfo == nil {
		return specOpts, nil
	}
	storageInfo, ok := flowOpts.StorageInfo.(*storage.StorageInfo)
	if !ok || storageInfo == nil {
		return specOpts, nil
	}
	plan, err := storageInfo.PlanHostDirMounts(ctx, containerReq.GetVolumeMounts())
	if err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return specOpts, nil
	}

	allMnts := make([]virtiofs.VirtioMounts, 0, len(plan))
	for _, mount := range plan {
		containerDir := constants.PropagationContainerDirRw
		if mount.ReadOnly {
			containerDir = constants.PropagationContainerDirRo
		}
		allMnts = append(allMnts, virtiofs.VirtioMounts{
			VirtiofsSource: containerDir + "/" + mount.SourceSubdir,
			Destination:    mount.ContainerPath,
		})
	}

	vc, err := jsoniter.Marshal(allMnts)
	if err != nil {
		return nil, fmt.Errorf("marshal restore virtiofs mounts: %w", err)
	}
	specOpts = append(specOpts, oci.WithAnnotations(map[string]string{
		constants.AnnotationPropagationExecMounts:       string(vc),
		constants.AnnotationPropagationContainerUmounts: virtiofs.GenPropagationContainerUmounts(),
	}))
	log.G(ctx).Infof("[hostdir] %s annotation set: %s", constants.AnnotationPropagationExecMounts, string(vc))
	log.G(ctx).Infof("[hostdir] %s annotation set: %s", constants.AnnotationPropagationContainerUmounts, virtiofs.GenPropagationContainerUmounts())
	return specOpts, nil
}

func appendVirtiofsConfig(ctx context.Context, opts *workflow.CreateContext, allVirtios []*virtiofs.VirtiofsConfig,
	shared []string, readOnly bool, coldStart bool) ([]*virtiofs.VirtiofsConfig, error) {
	if len(shared) == 0 {
		return allVirtios, nil
	}
	virtiofsConfig, err := genVirtiofsConfig(ctx, opts, shared, readOnly)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	if coldStart {
		virtiofsConfig.PropagationMountName = ""
	}
	allVirtios = append(allVirtios, virtiofsConfig)
	return allVirtios, nil
}

func genVirtiofsConfig(ctx context.Context, opts *workflow.CreateContext, shared []string, readOnly bool) (*virtiofs.VirtiofsConfig, error) {
	_ = ctx
	virtiofsConfig, err := virtiofs.GenVirtiofsConfig(shared)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	virtiofsConfig.VirtioBackendFsConfig.Cache = constants.VirtiofsCacheNone
	virtiofsConfig.VirtioBackendFsConfig.ReadOnly = readOnly
	if readOnly {
		virtiofsConfig.PropagationMountName = constants.PropagationVirtioRo
		virtiofsConfig.ID = constants.PropagationVirtioRo
	} else {
		virtiofsConfig.PropagationMountName = constants.PropagationVirtioRw
		virtiofsConfig.ID = constants.PropagationVirtioRw
	}
	limit, err := opts.GetQos(constants.MasterAnnotationsFSQos)
	if err != nil {
		return nil, err
	}
	if limit != nil {
		virtiofsConfig.RateLimiter = *limit
	}
	return virtiofsConfig, nil
}

func generateEmptyVirtiofsDevices(ctx context.Context) (map[string]string, error) {
	var allVirtios []*virtiofs.VirtiofsConfig

	roVirtioCfg, err := virtiofs.GenEmptyVirtiofsConfig(true, constants.VirtiofsCacheNone)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	allVirtios = append(allVirtios, roVirtioCfg)

	rwVirtioCfg, err := virtiofs.GenEmptyVirtiofsConfig(false, constants.VirtiofsCacheNone)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	allVirtios = append(allVirtios, rwVirtioCfg)

	log.G(ctx).Debugf("%s: %+v", constants.AnnotationVirtiofs, allVirtios)
	vc, _ := jsoniter.Marshal(allVirtios)

	return map[string]string{
		constants.AnnotationPropagationMounts:          virtiofs.GenPropagationVirtioDirs(),
		constants.AnnotationPropagationContainerMounts: virtiofs.GenPropagationContainerDirs(),
		constants.AnnotationVirtiofs:                   string(vc),
	}, nil
}

func (e *cubeboxInstancePlugin) genVideoAnnotationOpt(ctx context.Context, opts *workflow.CreateContext) ([]oci.SpecOpts, error) {
	var tmpSpecs []oci.SpecOpts
	realReq := opts.ReqInfo
	if realReq.GetAnnotations() == nil {
		return tmpSpecs, nil
	}

	enableStr, ok := realReq.GetAnnotations()[constants.AnnotationVideoEnable]
	if !ok || enableStr != "true" {
		return tmpSpecs, nil
	}

	resolution, ok := realReq.GetAnnotations()[constants.AnnotationVideoResolution]
	if !ok || resolution == "" {
		return tmpSpecs, fmt.Errorf("%s is required when %s is true", constants.AnnotationVideoResolution, constants.AnnotationVideoEnable)
	}

	resolutionParts := strings.Split(resolution, "x")
	if len(resolutionParts) != 2 {
		return tmpSpecs, fmt.Errorf("invalid %s format: %s, expected format: widthxheight (e.g., 720x1280)", constants.AnnotationVideoResolution, resolution)
	}

	width, err := strconv.Atoi(resolutionParts[0])
	if err != nil || width <= 0 {
		return tmpSpecs, fmt.Errorf("invalid %s width: %s", constants.AnnotationVideoResolution, resolutionParts[0])
	}

	height, err := strconv.Atoi(resolutionParts[1])
	if err != nil || height <= 0 {
		return tmpSpecs, fmt.Errorf("invalid %s height: %s", constants.AnnotationVideoResolution, resolutionParts[1])
	}

	maxResolution := resolution
	userProvidedMaxResolution := false
	if maxResStr, ok := realReq.GetAnnotations()[constants.AnnotationVideoMaxResolution]; ok && maxResStr != "" {
		maxResolution = maxResStr
		userProvidedMaxResolution = true
	}

	maxResolutionParts := strings.Split(maxResolution, "x")
	if len(maxResolutionParts) != 2 {
		return tmpSpecs, fmt.Errorf("invalid %s format: %s, expected format: widthxheight (e.g., 720x1280)", constants.AnnotationVideoMaxResolution, maxResolution)
	}

	maxWidth, err := strconv.Atoi(maxResolutionParts[0])
	if err != nil || maxWidth <= 0 {
		return tmpSpecs, fmt.Errorf("invalid %s width: %s", constants.AnnotationVideoMaxResolution, maxResolutionParts[0])
	}

	maxHeight, err := strconv.Atoi(maxResolutionParts[1])
	if err != nil || maxHeight <= 0 {
		return tmpSpecs, fmt.Errorf("invalid %s height: %s", constants.AnnotationVideoMaxResolution, maxResolutionParts[1])
	}

	if userProvidedMaxResolution {
		maxArea := maxWidth * maxHeight
		area := width * height
		if maxArea < area {
			return tmpSpecs, fmt.Errorf("max-resolution area (%dx%d=%d) must be greater than or equal to resolution area (%dx%d=%d)",
				maxWidth, maxHeight, maxArea, width, height, area)
		}
	}

	fps := 60
	if fpsStr, ok := realReq.GetAnnotations()[constants.AnnotationVideoFPS]; ok && fpsStr != "" {
		fps, err = strconv.Atoi(fpsStr)
		if err != nil || fps <= 0 {
			return tmpSpecs, fmt.Errorf("invalid %s: %s", constants.AnnotationVideoFPS, fpsStr)
		}
	}

	videoMemorySize := int64(math.Ceil(float64(maxWidth) * float64(maxHeight) * 4 * 1.2))

	videoParam := fmt.Sprintf("video=vfb:enable,%dx%dM-32@%d", width, height, fps)
	videomemoryParam := fmt.Sprintf("vfb.videomemorysize=%d", videoMemorySize)

	cmdlineAppend := []string{videoParam, videomemoryParam}
	cmdlineAppendJSON, err := jsoniter.Marshal(cmdlineAppend)
	if err != nil {
		return tmpSpecs, fmt.Errorf("failed to marshal cmdline append: %v", err)
	}

	log.G(ctx).Debugf("%v genVideoAnnotationOpt: resolution=%s, max-resolution=%s, fps=%d, videomemorysize=%d, cmdline=%s",
		opts.SandboxID, resolution, maxResolution, fps, videoMemorySize, string(cmdlineAppendJSON))

	tmpSpecs = append(tmpSpecs, oci.WithAnnotations(map[string]string{
		constants.AnnotationVMKernelCmdlineAppend: string(cmdlineAppendJSON),
	}))

	return tmpSpecs, nil
}
