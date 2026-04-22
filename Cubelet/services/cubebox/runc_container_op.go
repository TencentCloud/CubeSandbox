// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/containerd/containerd/api/types/runc/options"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	clabels "github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	osinterface "github.com/containerd/containerd/v2/pkg/os"
	"github.com/hashicorp/go-multierror"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	customopts "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/opts"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/runc"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type ociRuntime struct {
	cubeboxMgr *local
}

func (rr *ociRuntime) Destroy(ctx context.Context, opts *workflow.DestroyContext) (err error) {
	var (
		sb        *cubeboxstore.CubeBox
		sandBoxID = opts.SandboxID
		l         = rr.cubeboxMgr
		result    *multierror.Error
	)

	defer func() {

		if err == nil {
			l.cubeboxManger.Delete(ctx, &cubes.DeleteOption{CubeboxID: sandBoxID})
			if delerr := l.client.SandboxStore().Delete(ctx, sandBoxID); delerr != nil {
				log.G(ctx).Errorf("unable to update extensions for sandbox %q: %v", sandBoxID, delerr)
			}
		}
	}()

	sb, err = l.cubeboxManger.Get(ctx, sandBoxID)
	if err != nil {

		if errors.Is(err, utils.ErrorKeyNotFound) {
			return nil
		}
		return err
	}
	ctx = namespaces.WithNamespace(ctx, sb.Namespace)

	sb.GetStatus().Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.Removing = true
		return status, nil
	})

	var containers []*cubeboxstore.Container
	for _, ci := range sb.All() {
		containers = append(containers, ci)
	}

	containers = append(containers, sb.FirstContainer())
	for i, cntr := range containers {
		tmpCtx := log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{
			"ContainerId": cntr.ID,
		}))
		tmpCtx = context.WithValue(tmpCtx, constants.KCubeIndexContext, fmt.Sprint(i))
		tmpCtx = context.WithValue(tmpCtx, "sandboxID", sandBoxID)

		cntr.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
			status.Removing = true
			return status, nil
		})

		if er := l.destroyContainer(tmpCtx, cntr); er != nil {
			result = multierror.Append(result, fmt.Errorf("destroy container [%s] fail: %w", cntr.ID, er))
			log.G(tmpCtx).Warnf("Destroy container [%s] fail: %s", cntr.ID, er)
			continue
		}
		if er := l.cubeboxManger.Delete(ctx, &cubes.DeleteOption{CubeboxID: sb.ID, ContainerID: cntr.ID}); er != nil {
			log.G(tmpCtx).Warnf("Delete container [%s] in cubestore fail: %v", cntr.ID, er)
		}
	}
	if er := runc.Clean(ctx, opts.SandboxID); er != nil {
		result = multierror.Append(result, fmt.Errorf("destroy runc files [%s] fail: %w", sandBoxID, er))
	}

	if er := result.ErrorOrNil(); er != nil {
		err = ret.Errorf(errorcode.ErrorCode_RemoveContainerFailed, "%s", er.Error())
		return
	}

	return nil
}

func (rr *ociRuntime) Create(ctx context.Context, flowOpts *workflow.CreateContext) error {
	l := rr.cubeboxMgr
	realReq := flowOpts.ReqInfo
	ociRuntime, err := l.getSandboxRuntime(realReq)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}

	sandBox := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			ID:           flowOpts.GetSandboxID(),
			Labels:       realReq.GetLabels(),
			Annotations:  realReq.GetAnnotations(),
			CreatedAt:    time.Now().UnixNano(),
			InstanceType: flowOpts.GetInstanceType(),
		},
		OciRuntime: &ociRuntime,
		IP:         CubeLog.LocalIP,
	}

	log := log.G(ctx).WithFields(CubeLog.Fields{
		"sandboxID": sandBox.ID,
		"runtime":   realReq.GetRuntimeHandler(),
	})
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "get namespace from context fail: %v", err)
	}
	sandBox.Namespace = ns
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = constants.WithRuntimeType(ctx, sandBox.OciRuntime.Type)

	rr.addSandboxContainer(realReq)

	failover := true
	defer func() {
		if failover && flowOpts.Failover {
			rr.Destroy(ctx, &workflow.DestroyContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					SandboxID: flowOpts.GetSandboxID(),
				},
			})
		}
	}()

	if err = l.createCubeboxContainer(ctx, flowOpts, realReq, sandBox); err != nil {
		return ret.Errorf(errorcode.ErrorCode_NewContainerMetaDataFailed, "create cubebox container fail: %v", err)
	}

	for i, realC := range realReq.Containers {
		ctxTmp := context.WithValue(ctx, constants.KCubeIndexContext, fmt.Sprint(i))
		ci, err := sandBox.Get(realC.Id)
		if err != nil {
			return fmt.Errorf("get container %q of sandbox %q fail", realC.Id, sandBox.ID)
		}
		copts, err := rr.createContainerOpts(ctxTmp, realC, sandBox, ci, flowOpts)
		if err != nil {
			log.WithError(err).Errorf("create container opts fail: %v", ci.ID)
			return fmt.Errorf("create container opts fail")
		}
		err = l.runContainer(ctxTmp, sandBox, ci, copts, ociRuntime)
		if err != nil {
			log.WithError(err).Errorf("run container fail: %v", ci.ID)
			return err
		}
		sandBox.AddContainer(ci)
		if err := l.cubeboxManger.Save(ctxTmp, sandBox); err != nil {
			log.Warnf("saveSandBoxInfo failed.%s", err.Error())
			return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
		}
		if err := l.doProbe(ctxTmp, realC, ci); err != nil {
			return err
		}
	}
	failover = false
	return nil
}

func (rr *ociRuntime) addSandboxContainer(realReq *cubebox.RunCubeSandboxRequest) {
	if id, ok := realReq.Annotations[constants.AnnotationCubeSandboxImageID]; ok {
		scontainer := &cubebox.ContainerConfig{
			Image: &images.ImageSpec{
				Image: id,
			},
			SecurityContext: &cubebox.ContainerSecurityContext{
				Privileged: true,
			},
		}
		old := realReq.Containers
		realReq.Containers = append([]*cubebox.ContainerConfig{}, scontainer)
		realReq.Containers = append(realReq.Containers, old...)
	}
}

func (rr *ociRuntime) createContainerOpts(
	ctx context.Context,
	containerReq *cubebox.ContainerConfig,
	cubeBox *cubeboxstore.CubeBox,
	ci *cubeboxstore.Container,
	flowOpts *workflow.CreateContext) ([]containerd.NewContainerOpts, error) {
	var (
		opts  []oci.SpecOpts
		cOpts []containerd.NewContainerOpts
	)

	image, err := rr.cubeboxMgr.criImage.EnsureImage(ctx, containerReq.GetImage().GetImage(),
		containerReq.GetImage().GetUsername(),
		containerReq.GetImage().GetToken(),
		&runtime.PodSandboxConfig{
			Annotations: map[string]string{
				constants.LabelContainerImageMedia:    containerReq.GetImage().GetStorageMedia(),
				constants.LabelContainerCubeImageSpec: utils.InterfaceToString(containerReq.GetImage()),
			},
		})
	if err != nil {

		return nil, fmt.Errorf("ensure image %v fail:%w", containerReq.GetImage().GetImage(), err)
	}
	imageSpec, err := image.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("get image %v spec fail:%w", containerReq.GetImage().GetImage(), err)
	}
	imageSpecConfig := &imageSpec.Config

	if !ci.IsPod {
		cOpts = append(cOpts, containerd.WithSandbox(cubeBox.ID))
	}

	opts = append(opts,
		oci.WithDefaultSpec(),
		oci.WithDefaultUnixDevices,
		oci.WithAnnotations(containerReq.Annotations),
	)
	opts = append(opts, container.GenOpt(ctx, containerReq)...)

	if containerReq.GetSecurityContext() != nil {
		if containerReq.GetSecurityContext().GetReadonlyRootfs() {
			opts = append(opts, oci.WithRootFSReadonly())
		}
		if containerReq.GetSecurityContext().GetPrivileged() {
			if !cubeBox.OciRuntime.PrivilegedWithoutHostDevices {
				opts = append(opts, oci.WithHostDevices, oci.WithAllDevicesAllowed)
			}
		}
		if caps := containerReq.GetSecurityContext().GetCapabilities(); caps != nil {
			opts = append(opts,
				oci.WithAddedCapabilities(caps.AddCapabilities),
				oci.WithDroppedCapabilities(caps.DropCapabilities),
				oci.WithAmbientCapabilities(caps.AddAmbientCapabilities))
		}
	}

	snapshotter := cubeBox.OciRuntime.Snapshotter
	labels := buildLabels(ctx, ci.Annotations, image.Labels())
	if ci.IsPod {
		labels[constants.ContainerType] = constants.ContainerTypeSandBox
	} else {
		labels[constants.ContainerType] = constants.ContainerTypeContainer
	}
	opts = append(opts, oci.WithImageConfig(image))
	cOpts = append(cOpts,
		containerd.WithImage(image),
		containerd.WithImageConfigLabels(image),
		containerd.WithAdditionalContainerLabels(labels),
		containerd.WithSnapshotter(snapshotter),
		containerd.WithNewSnapshot(ci.ID, image))

	{
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("get hostname: %w", err)
		}
		opts = append(opts,
			oci.WithHostNamespace(specs.NetworkNamespace),

			oci.WithHostHostsFile,
			oci.WithHostResolvconf,
			oci.WithEnv([]string{fmt.Sprintf("HOSTNAME=%s", hostname)}),
		)

	}

	if containerReq.GetResources() != nil {
		if cpuStr := containerReq.GetResources().GetCpu(); cpuStr != "" {
			cpuQ, err := resource.ParseQuantity(cpuStr)
			if err != nil {
				return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat,
					"cpu resource:%s", err.Error())
			}
			var (
				period = uint64(100000)
				quota  = cpuQ.MilliValue() * 100
			)
			opts = append(opts, oci.WithCPUCFS(quota, period))
		}
		if memStr := containerReq.GetResources().GetMem(); memStr != "" {
			memQ, err := resource.ParseQuantity(memStr)
			if err != nil {
				return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat,
					"mem resource:%s", err.Error())
			}
			opts = append(opts, oci.WithMemoryLimit(uint64(memQ.Value())))
		}
	}

	if containerReq.GetOciConfig() != nil {
		if containerReq.GetOciConfig().GetDevices() != nil {
			opts = append(opts, customopts.WithDevices(osinterface.RealOS{}, containerReq, false))
		}
	}

	opts = append(opts, genGeneralContainerSpecOpt(ctx, containerReq, ci, imageSpecConfig)...)
	smounts := rr.cubeboxMgr.prepareSandboxPathVolume(ctx, containerReq, flowOpts.ReqInfo)
	if len(smounts) > 0 {
		opts = append(opts, oci.WithMounts(smounts))
	}
	smounts, err = runc.GenMount(ctx, flowOpts)
	if err != nil {
		return nil, err
	}
	if len(smounts) > 0 {
		opts = append(opts, oci.WithMounts(smounts))
	}

	if propagation, ok := containerReq.Annotations[constants.AnnotationContainRootfsPropagation]; ok {
		opts = append(opts, WithRootfsPropagation(propagation))
	}
	var s specs.Spec
	cOpts = append(cOpts,
		containerd.WithSpec(&s, opts...),
		containerd.WithRuntime(cubeBox.OciRuntime.Type, &options.Options{}),
	)

	return cOpts, nil
}

func ociMounts(sr *cubebox.RunCubeSandboxRequest, cr *cubebox.ContainerConfig) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, container *containers.Container, s *specs.Spec) error {

		return nil
	}
}

func buildLabels(ctx context.Context, cmdLabels, imageLabels map[string]string) map[string]string {
	labels := make(map[string]string)
	for k, v := range imageLabels {
		if err := clabels.Validate(k, v); err == nil {
			labels[k] = v
		} else {

			log.G(ctx).WithError(err).Warnf("unable to add image label with key %s to the container", k)
		}
	}

	for k, v := range cmdLabels {
		labels[k] = v
	}
	return labels
}
