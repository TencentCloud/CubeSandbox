// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"encoding/json"


	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/disk"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func (l *local) genRuntimeCfgAnnotationOpt(ctx context.Context, ociRuntime *cubeconfig.Runtime,
	opts *workflow.CreateContext) []oci.SpecOpts {
	var tmpSpecs []oci.SpecOpts
	realReq := opts.ReqInfo
	if realReq.GetAnnotations() == nil {
		return tmpSpecs
	}

	runtimeAnno := make(map[string]string)

	cfgPath := ociRuntime.ConfigPath
	if path, ok := realReq.GetAnnotations()[constants.AnnotationsRuntimeCfgPath]; ok {
		ok, _ := utils.DenExist(path)
		if ok {
			cfgPath = path
		}
	}
	runtimeAnno[constants.AnnotationsRuntimeCfgPath] = cfgPath

	if path, ok := realReq.GetAnnotations()[constants.AnnotationsVMImagePath]; ok {
		ok, _ := utils.DenExist(path)
		if ok {
			runtimeAnno[constants.AnnotationsVMImagePath] = path
		}
	}

	if path, ok := realReq.GetAnnotations()[constants.AnnotationsVMKernelPath]; ok {
		ok, _ := utils.DenExist(path)
		if ok {
			runtimeAnno[constants.AnnotationsVMKernelPath] = path
		}
	}
	return append(tmpSpecs, oci.WithAnnotations(runtimeAnno))
}

func genContainerAnnotationReq(ctx context.Context, c *cubebox.ContainerConfig, ci *cubeboxstore.Container) oci.SpecOpts {
	anno := make(map[string]string)
	for k, v := range c.GetAnnotations() {
		anno[k] = v
	}

	if ci.IsPod {
		anno[constants.ContainerType] = constants.ContainerTypeSandBox
	} else {
		anno[constants.ContainerType] = constants.ContainerTypeContainer
	}
	anno[constants.SandboxID] = ci.SandboxID

	return oci.WithAnnotations(anno)
}

func (l *local) genListFilterLabels(ctx context.Context, req *cubebox.RunCubeSandboxRequest, sandBox *cubeboxstore.CubeBox) map[string]string {
	labels := make(map[string]string)
	labels[constants.AnnotationsProduct] = req.InstanceType
	labels[constants.LabelNumaNode] = fmt.Sprintf("%d", sandBox.NumaNode)
	return labels
}

func getDiskQos(ctx context.Context, realReq *cubebox.RunCubeSandboxRequest, key string) (*disk.RateLimiter, error) {
	if realReq.GetAnnotations() == nil {
		return nil, nil
	}
	data, ok := realReq.GetAnnotations()[key]
	if !ok || data == "" {
		return nil, nil
	}
	r := &disk.RateLimiter{}
	err := utils.Decode(data, r)
	if err != nil {
		log.G(ctx).Errorf("%v decode fail:%+v", constants.MasterAnnotationsBlkQos, data)
		return nil, fmt.Errorf("%v decode fail:%+v", constants.MasterAnnotationsBlkQos, data)
	}
	return r, nil
}

func (l *local) genVirtFsAnnotationOpt(ctx context.Context,
	opts *workflow.CreateContext,
	mountsConfig *virtiofs.CubeRootfsInfo) oci.SpecOpts {
	mc, _ := json.Marshal(mountsConfig)
	return oci.WithAnnotations(map[string]string{
		constants.AnnotationsRootfsKey: string(mc),
	})
}

func (l *local) genStorageMediumDefaultAnnotationOpt(ctx context.Context,
	opts *workflow.CreateContext,
	sandBox *cubeboxstore.CubeBox) ([]oci.SpecOpts, error) {
	var tmpSpecs []oci.SpecOpts
	var annotationInfo []disk.CubeDiskConfig
	limit, err := opts.GetQos(constants.MasterAnnotationsBlkQos)
	if err != nil {
		return tmpSpecs, err
	}
	if opts.StorageInfo != nil {
		tmpInfo, ok := opts.StorageInfo.(*storage.StorageInfo)
		if ok && len(tmpInfo.Volumes) > 0 {
			for _, v := range tmpInfo.Volumes {
				annotationInfo = append(annotationInfo, disk.CubeDiskConfig{
					HostPath:         v.FilePath,
					Type:             v.Type,
					SourcePath:       v.SourcePath,
					Size:             v.SizeLimit,
					FSQuota:          v.FSQuota,
					RateLimiter:      limit,
					VolumeSourceName: v.Name,
				})
			}
		}
	}
	for key := range sandBox.AllContainers() {
		ci, err := sandBox.Get(key)
		if err != nil {
			log.G(ctx).Errorf("get container fail:%+v", err)
			continue
		}
		if ci.HostImage != nil && ci.HostImage.ImageDevs != nil {
			annotationInfo = append(annotationInfo, disk.CubeDiskConfig{
				HostPath:    ci.HostImage.ImageDevs.HostDevicePath,
				Type:        ci.Snapshotter,
				RateLimiter: limit,
				Options:     []string{constants.MountOptReadOnly},
			})
		}
	}

	if len(annotationInfo) > 0 {
		b, _ := json.Marshal(annotationInfo)
		log.G(ctx).Debugf("%s genContainerTmpOpt %s: %s", opts.SandboxID, constants.AnnotationsMountListKey, string(b))
		tmpSpecs = append(tmpSpecs, oci.WithAnnotations(map[string]string{
			constants.AnnotationsMountListKey: string(b),
		}))
	}
	return tmpSpecs, nil
}

func (l *local) genNetworkAnnotationOpt(ctx context.Context,
	containerReq *cubebox.ContainerConfig,
	opts *workflow.CreateContext,
	ci *cubeboxstore.Container,
) ([]oci.SpecOpts, error) {
	var specOpts []oci.SpecOpts
	if opts.NetworkInfo != nil {
		specOpts = append(specOpts, opts.NetworkInfo.OCISpecOpts())
	}
	if opts.NetFile != nil {
		if spec := opts.NetFile.OciContainerNetfileSpec(ctx, containerReq.Name); spec != nil {
			specOpts = append(specOpts, spec)
		}

		specOpts = append(specOpts, oci.WithHostname(opts.NetFile.Hostname))
	}

	if ci.IsPod {

		sOpts, err := l.genCubeVipsOpt(ctx, containerReq, opts)
		if err != nil {
			return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "generate vips options failed: %v", err)
		}
		specOpts = append(specOpts, sOpts...)
	}
	return specOpts, nil
}

func (l *local) genCgroupAnnotationOpt(ctx context.Context, c *cubebox.ContainerConfig,
	opts *workflow.CreateContext) []oci.SpecOpts {
	var tmpSpecs []oci.SpecOpts
	cgInfo, ok := opts.CgroupInfo.(*cgroup.Info)
	if !ok || opts.CgroupInfo == nil {
		return tmpSpecs
	}

	resJson, _ := json.Marshal(cgInfo.VmSnapshotSpec)
	log.G(ctx).Debugf("%v genCgroupAnnotationOpt:%+v,vmres:%v", opts.SandboxID,
		cgInfo.CgroupID, string(resJson))

	tmpSpecs = append(tmpSpecs, oci.WithAnnotations(map[string]string{

		constants.AnnotationsVMSpecKey: string(resJson),
	}))

	return tmpSpecs
}

func (l *local) genCubeVipsOpt(ctx context.Context, c *cubebox.ContainerConfig, opts *workflow.CreateContext) ([]oci.SpecOpts, error) {
	var tmpSpecs []oci.SpecOpts
	realReq := opts.ReqInfo
	if data, ok := realReq.GetAnnotations()[constants.MasterAnnotationsNetCubeVips]; ok && data != "" {
		ipList := strings.Split(data, ":")
		var goodList []string
		if len(ipList) > 0 {
			for _, ip := range ipList {
				if net.ParseIP(ip) == nil {
					return tmpSpecs, fmt.Errorf("invalid ip %s of config:%s", ip, constants.MasterAnnotationsNetCubeVips)
				}
				goodList = append(goodList, ip)
			}
		} else {

			return tmpSpecs, nil
		}
		value := strings.Join(goodList, ":")
		log.G(ctx).Debugf("%v genCubeVipsOpt:%s", opts.SandboxID, value)
		tmpSpecs = append(tmpSpecs, oci.WithAnnotations(map[string]string{
			constants.AnnotationsNetCubeVips: value,
		}))

		tmpSpecs = append(tmpSpecs, func() oci.SpecOpts {
			return func(ctx context.Context, client oci.Client, c *containers.Container, s *specs.Spec) error {
				if s.Linux == nil {
					s.Linux = &specs.Linux{}
				}
				if s.Linux != nil {
					if s.Linux.Resources == nil {
						s.Linux.Resources = &specs.LinuxResources{}
					}
				}

				if s.Linux.Resources.Network == nil {
					tmpClsID := uint32(0xfcfcfcfc)
					s.Linux.Resources.Network = &specs.LinuxNetwork{
						ClassID: &tmpClsID,
					}
				}
				return nil
			}
		}())
	}
	return tmpSpecs, nil
}

func WithRootfsPropagation(rootfsPropagation string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		s.Linux.RootfsPropagation = rootfsPropagation
		return nil
	}
}
