// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"

	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/api/resource"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	cubelabels "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/labels"
	cristore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	sandboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/sandbox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/capability"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/cgroup"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/command"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/env"
	localnetfile "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/rlimit"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/rootfs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/seccomp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/sysctl"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/tmpfs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/uid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/taskio"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
	cgroupp "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/images"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	cubeSharedBindRootPath = "/run/cube-bind-share"

	K8sEmptyDirPath = "kubernetes.io~empty-dir"
)

func init() {
	typeurl.Register(&cubeboxstore.CubeBox{},
		"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox", "CubeBox")
}

func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	realReq := opts.ReqInfo
	if len(realReq.Containers) == 0 {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "should provide containers param")
	}

	startTime := time.Now()
	defer func() {
		if CubeLog.GetLevel() == CubeLog.DEBUG {
			log.G(ctx).WithFields(CubeLog.Fields{
				"req":      log.WithJsonValue(realReq),
				"duration": time.Since(startTime).String(),
			}).Debugf("create sandbox %s", opts.SandboxID)
		}
	}()

	if opts.IsCreateSnapshot() {
		sb, err := l.cubeboxManger.Get(ctx, opts.GetSandboxID())
		if err == nil && sb.SandboxID == opts.GetSandboxID() {
			return ret.Err(errorcode.ErrorCode_PreConditionFailed, "already exists")
		}
	}
	opts.CubeBoxCreated = true
	if err := l.createContainers(ctx, opts); err != nil {
		return err
	}
	cgInfo, ok := opts.CgroupInfo.(*cgroupp.Info)
	if ok {
		if !config.GetCommon().DisableHostCgroup && !constants.GetDisableHostCgroup(ctx) {
			startTime := time.Now()
			err := cgroupp.SetCubeboxCgroupLimit(ctx, cgInfo.CgroupID, cgInfo.ResourceQuantity.HostCpuQ,
				cgInfo.ResourceQuantity.HostMemQ, cgInfo.UsePoolV2)
			if err != nil {
				err = ret.Errorf(errorcode.ErrorCode_SetCubeboxCgroupLimitFailed,
					"set cubebox cgroup limit error: %s", err)
				workflow.RecordCreateMetric(ctx, err, constants.CubeRunCgroupId, time.Since(startTime))
				return err
			}
			workflow.RecordCreateMetric(ctx, nil, constants.CubeRunCgroupId, time.Since(startTime))
			log.G(ctx).Debugf("set cubebox cgroup %s limit: mem %s, cpu %s", cgInfo.CgroupID,
				cgInfo.ResourceQuantity.HostMemQ.String(), cgInfo.ResourceQuantity.HostCpuQ.String())
		}
	}

	return nil
}

type createContainerParam struct {
	ci      *cubeboxstore.Container
	cOpts   []containerd.NewContainerOpts
	ctxTmp  context.Context
	cntrReq *cubebox.ContainerConfig
}

func (l *local) createContainers(ctx context.Context, flowOpts *workflow.CreateContext) error {
	realReq := flowOpts.ReqInfo

	ociRuntime, err := l.getSandboxRuntime(realReq)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}

	sandBox := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			ID:           flowOpts.SandboxID,
			SandboxID:    flowOpts.SandboxID,
			Labels:       deepCopyStringMap(realReq.GetLabels()),
			Annotations:  realReq.GetAnnotations(),
			CreatedAt:    time.Now().UnixNano(),
			InstanceType: flowOpts.GetInstanceType(),
		},
		IP:               getSandboxIp(flowOpts),
		PortMappings:     getAllocatedPort(flowOpts),
		NumaNode:         0,
		Queues:           0,
		OciRuntime:       &ociRuntime,
		Version:          cubeboxstore.CurrentCubeboxVersion,
		RequestSource:    getUserAgent(ctx),
		LocalRunTemplate: flowOpts.LocalRunTemplate,
	}
	if sandBox.Metadata.Labels == nil {
		sandBox.Metadata.Labels = make(map[string]string)
	}

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	sandBox.Namespace = ns
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = cubeboxstore.WithCubeBox(ctx, sandBox)

	log.G(ctx).Infof("create sandbox with namespace %s", sandBox.Namespace)

	if flowOpts.UserData != nil && flowOpts.UserData.K8sPod != nil {
		sandBox.GetOrCreatePodConfig().SetK8sPod(ctx, flowOpts.UserData.K8sPod)
	}

	l.storeNumaQueues(ctx, sandBox, flowOpts)

	cgInfo, cgSet := flowOpts.CgroupInfo.(*cgroupp.Info)
	if cgSet {
		sandBox.ResourceWithOverHead = &cgInfo.ResourceQuantity
		sandBox.CGroupPath = cgInfo.CgroupID
	}

	sandBox.Metadata.AddLabels(l.genListFilterLabels(ctx, realReq, sandBox))
	if err = l.createCubeboxContainer(ctx, flowOpts, realReq, sandBox); err != nil {
		return err
	}

	additionalSandboxOpt, err := l.genSandboxOptions(ctx, realReq, sandBox, flowOpts)
	if err != nil {
		return err
	}

	var (
		params    []createContainerParam
		sanboxlog = log.G(ctx).WithFields(CubeLog.Fields{
			"sandboxID": sandBox.ID,
		})
		containerNameArray []*cubebox.KeyValue
	)
	for _, c := range realReq.Containers {
		containerNameArray = append(containerNameArray, &cubebox.KeyValue{
			Key:   constants.MakeContainerIDEnvKey(c.Name),
			Value: c.Id,
		})
	}
	for i, cntrReq := range realReq.Containers {
		ci, err := sandBox.Get(cntrReq.Id)
		if err != nil {
			return ret.Err(errorcode.ErrorCode_CreateContainerFailed, fmt.Sprintf("get container info failed.%s", err.Error()))
		}
		var (
			additionalOpt []oci.SpecOpts
			containerLog  = sanboxlog.WithField("containerID", ci.ID)
		)
		ctxTmp := context.WithValue(ctx, constants.KCubeIndexContext, fmt.Sprint(i))
		ctxTmp = constants.WithImageSpec(ctxTmp, cntrReq.GetImage())
		ctxTmp = constants.WithFuncType(ctxTmp, ci.InstanceType)

		cntrReq.Envs = append(cntrReq.Envs, containerNameArray...)

		start := time.Now()

		if isImageStorageMediaType(cntrReq, cubeimages.ImageStorageMediaType_ext4) {

			ctxTmp = constants.WithAppImageID(ctxTmp, cntrReq.GetImage().GetImage())
		}

		if oopt, err := l.containerOciSpec(ctxTmp, cntrReq, flowOpts, ci, sandBox); err == nil {
			additionalOpt = append(additionalOpt, oopt...)

			if ci.IsPod {
				additionalOpt = append(additionalOpt, additionalSandboxOpt...)
			}
		} else {
			containerLog.Errorf("create container oci spec failed.%s", err.Error())
			return err
		}

		cOpts, err := l.containerSpec(ctxTmp, sandBox, cntrReq, flowOpts, ci, additionalOpt)
		cOpts = append(cOpts,
			containerd.WithImageName(ci.Config.Image.Image),
		)
		if !ci.IsPod {
			cOpts = append(cOpts, containerd.WithSandbox(ci.SandboxID))
		}

		if ociRuntime.Type != "" {
			cOpts = append(cOpts, containerd.WithRuntime(ociRuntime.Type, &runtimeoptions.Options{}))
		}
		workflow.RecordCreateMetric(ctxTmp, err, constants.CubeContainerSpecId, time.Since(start))
		if err != nil {
			containerLog.Errorf("create container Spec failed.%s", err.Error())
			return err
		}
		params = append(params, createContainerParam{
			ci:      ci,
			cOpts:   cOpts,
			ctxTmp:  ctxTmp,
			cntrReq: cntrReq,
		})
	}

	sandBox.Lock()
	defer func() {
		if err := l.cubeboxManger.Save(ctx, sandBox); err != nil {
			log.G(ctx).Warnf("saveSandBoxInfo failed.%s", err.Error())
		}
		sandBox.Unlock()
	}()

	for _, param := range params {
		ci := param.ci
		containerLog := sanboxlog.WithFields(CubeLog.Fields{
			"containerID": ci.ID,
			"isPod":       ci.IsPod,
		})
		err = func() (retE error) {
			containerLog := log.G(ctx).WithField("container-id", ci.ID)
			retE = l.runContainer(param.ctxTmp, sandBox, param.ci, param.cOpts, ociRuntime)
			withOciSpec := log.IsDebug() || retE != nil
			if ci.Container != nil && withOciSpec {
				info, err := ci.Container.Info(ctx, containerd.WithoutRefreshedMetadata)
				if err == nil {
					v, err := typeurl.UnmarshalAny(info.Spec)
					if err != nil {
						return fmt.Errorf("failed to unmarshal container spec with url %s: %w", info.Spec.GetTypeUrl(), err)
					}
					jsonstr := log.WithJsonValue(struct {
						containers.Container
						Spec interface{} `json:"Spec,omitempty"`
					}{
						Container: info,
						Spec:      v,
					})
					containerLog.Debugf("container-oci-spec: %s", jsonstr)
				}
			}
			if retE != nil {
				containerLog.Errorf("run container failed.%s", retE.Error())
			} else {
				containerLog.Debug("run container success")
			}
			return retE
		}()
		if err != nil {
			return fmt.Errorf("failed to run container %s: %w", param.ci.ID, err)
		}
		if err := l.doProbe(param.ctxTmp, param.cntrReq, param.ci); err != nil {
			return err
		}
		err = l.cbriManager.PostCreateContainer(ctx, sandBox, param.ci)
		if err != nil {
			containerLog.Errorf("post create container failed, err: %v", err)
		}
	}

	pid := sandBox.Endpoint.Pid

	if cgSet {
		go func() {
			setCgroup(ctx, pid, cgInfo.CgroupID)
			sanboxlog.Debugf("set cgroup for sandbox success. shim pid: %d", pid)
		}()
	}

	if !config.GetCommon().DisableVmCgroup && !constants.GetDisableVMCgroup(ctx) {
		for _, ci := range sandBox.AllContainers() {
			if err := updateCgroup(ctx, ci); err != nil {
				sanboxlog.Errorf("failed to update container %v cgroup: %v", ci.ID, err)
			}
		}
	}

	return nil
}

func (l *local) genSandboxOptions(ctx context.Context, realReq *cubebox.RunCubeSandboxRequest, sandBox *cubeboxstore.CubeBox, flowOpts *workflow.CreateContext) ([]oci.SpecOpts, error) {
	var (
		additionalSandboxOpt []oci.SpecOpts
		err                  error
	)

	if !flowOpts.IsRetoreSnapshot() {
		additionalSandboxOpt, err = WithCubeFsAnnotation(ctx, realReq, sandBox)
		if err != nil {
			return nil, fmt.Errorf("failed to set cube fs annotation opt: %w", err)
		}
	}

	additionalSandboxOpt = append(additionalSandboxOpt, prepareVolumePmems(flowOpts))

	sOpts, err := l.genStorageMediumDefaultAnnotationOpt(ctx, flowOpts, sandBox)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "generate storage medium annotation failed: %v", err)
	}
	additionalSandboxOpt = append(additionalSandboxOpt, sOpts...)
	dnsServers, err := sandboxDNSServersFromContainers(realReq)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "generate sandbox dns annotation failed: %v", err)
	}
	if len(dnsServers) > 0 {
		data, _ := json.Marshal(dnsServers)
		additionalSandboxOpt = append(additionalSandboxOpt, oci.WithAnnotations(map[string]string{
			constants.AnnotationsSandboxDNS: string(data),
		}))
	}
	err = l.genImageReferenceForCubebox(ctx, flowOpts, sandBox)
	if err != nil {
		return nil, fmt.Errorf("failed to generate image reference for cubebox: %w", err)
	}
	return additionalSandboxOpt, nil
}

func sandboxDNSServersFromContainers(realReq *cubebox.RunCubeSandboxRequest) ([]string, error) {
	return localnetfile.ResolveEffectiveDNSServers(realReq)
}

func (l *local) genImageReferenceForCubebox(ctx context.Context, flowOpts *workflow.CreateContext, sandBox *cubeboxstore.CubeBox) error {
	if sandBox.ImageReferences == nil {
		sandBox.ImageReferences = make(map[string]cubeboxstore.ImageReference)
	}

	if flowOpts.StorageInfo != nil {
		_, _ = flowOpts.StorageInfo.(*storage.StorageInfo)
	}
	return nil
}

func (l *local) createCubeboxContainer(ctx context.Context, flowOpts *workflow.CreateContext, realReq *cubebox.RunCubeSandboxRequest, sandBox *cubeboxstore.CubeBox) error {
	for i, cntrReq := range realReq.Containers {
		isPod := i == 0
		_, cid := l.generateContainerID(ctx, flowOpts, i)

		if len(cntrReq.Annotations) == 0 {
			cntrReq.Annotations = make(map[string]string)
		}

		if flowOpts.NetFile != nil {
			cntrReq.Envs = append(cntrReq.Envs, &cubebox.KeyValue{
				Key:   "HOSTNAME",
				Value: flowOpts.NetFile.Hostname,
			})
		}

		cntrReq.Id = cid
		createdAt := time.Now().UnixNano()
		ci := &cubeboxstore.Container{
			Metadata: cubeboxstore.Metadata{
				ID:           cntrReq.Id,
				Name:         cntrReq.Name,
				SandboxID:    sandBox.ID,
				Config:       makeContainerConfigToSave(cntrReq),
				CreatedAt:    createdAt,
				Namespace:    sandBox.Namespace,
				InstanceType: realReq.GetInstanceType(),
			},
			IP:     sandBox.IP,
			Status: cubeboxstore.StoreStatus(cubeboxstore.Status{CreatedAt: createdAt}),
			IsPod:  isPod,
		}

		if set := cntrReq.GetAnnotations()[constants.AnnotationsDebugStdout]; set == "true" {
			ci.IsDebugStdout = true
		}

		if isPod {
			sandBox.FirstContainerName = ci.ID
			sandBox.Config = makeContainerConfigToSave(cntrReq)
		}
		ci.AddAnnotations(ci.Metadata.Config.Annotations)
		sandBox.AddContainer(ci)
		err := l.prepareContainerFiles(ctx, sandBox, cntrReq, flowOpts, i)
		if err != nil {
			return fmt.Errorf("prepare container files failed: %w", err)
		}
	}

	if err := l.cubeboxManger.Save(ctx, sandBox, cubes.WithNoEvent); err != nil {
		log.G(ctx).Warnf("saveSandBoxInfo failed.%s", err.Error())
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}
	return nil
}

func (l *local) generateContainerID(ctx context.Context, flowOpts *workflow.CreateContext, index int) (context.Context, string) {
	var cid string
	ctxTmp := context.WithValue(ctx, constants.KCubeIndexContext, strconv.Itoa(index))
	if index == 0 {
		ctxTmp = context.WithValue(ctxTmp, CubeLog.KeyFunctionType, constants.ContainerTypeSandBox)
		cid = flowOpts.GetSandboxID()
	} else {
		ctxTmp = context.WithValue(ctxTmp, CubeLog.KeyFunctionType, constants.ContainerTypeContainer)
		cid = utils.GenerateID()

		if flowOpts.IsCreateSnapshot() {
			if templateID, ok := flowOpts.GetSnapshotTemplateID(); ok {

				cid = templateID + "_" + strconv.Itoa(index)
			}

		}
	}
	if cid == "" {
		cid = utils.GenerateID()
	}
	return ctxTmp, cid
}

func (l *local) prepareContainerFiles(ctx context.Context, sandBox *cubeboxstore.CubeBox, containerReq *cubebox.ContainerConfig, flowOpts *workflow.CreateContext, i int) error {
	ci, err := sandBox.Get(containerReq.Id)
	if err != nil {
		return err
	}
	ctxTmp := context.WithValue(ctx, constants.KCubeIndexContext, fmt.Sprint(i))
	ctxTmp = constants.WithImageSpec(ctxTmp, containerReq.GetImage())
	mountsConfig := &virtiofs.CubeRootfsInfo{
		Mounts: []virtiofs.CubeRootfsMount{},
	}
	if ci.IsPod {
		ctxTmp = context.WithValue(ctxTmp, CubeLog.KeyFunctionType, constants.ContainerTypeSandBox)
	} else {
		ctxTmp = context.WithValue(ctxTmp, CubeLog.KeyFunctionType, constants.ContainerTypeContainer)
	}

	if isImageStorageMediaType(containerReq, cubeimages.ImageStorageMediaType_ext4) {

		mountsConfig.PmemFile = pmem.GetRawImageFilePath(flowOpts.ReqInfo.GetInstanceType(), containerReq.GetImage().GetImage())
		appendExt4NetfileMounts(mountsConfig, flowOpts, containerReq.Name)
	} else {
		var ovlShare []virtiofs.ShareDirMapping

		if flowOpts.NetFile != nil {
			netfileMapping := flowOpts.NetFile.ContainerVirtiofsDirMaping(containerReq.Name)
			if netfileMapping != nil {
				ovlShare = append(ovlShare, *netfileMapping)
			}
		}

		pullImage := func() error {
			_, err := l.criImage.EnsureImage(ctxTmp, containerReq.GetImage().GetImage(),
				containerReq.GetImage().GetUsername(),
				containerReq.GetImage().GetToken(),
				&runtime.PodSandboxConfig{})
			if err != nil {

				return fmt.Errorf("failed to ensure image %s: %w", containerReq.GetImage().GetImage(), err)
			}
			localImage, err := l.criImage.LocalResolve(ctxTmp, containerReq.GetImage().GetImage())
			if err != nil {
				return fmt.Errorf("failed to get local image %s: %w", containerReq.GetImage().GetImage(), err)
			}
			ci.Snapshotter = sandBox.OciRuntime.Snapshotter
			ci.SnapshotKey = localImage.ChainID

			if localImage.MediaType == "erofs" {
				return fmt.Errorf("erofs image is not supported in the open source build")
			}
			if len(localImage.HostLayers) == 0 {
				return fmt.Errorf("no host layers with raw image")
			}

			var ovlDir []virtiofs.ShareDirMapping
			ovlDir, err = rootfs.GenImageSharedDirs(localImage.HostLayers)
			if err != nil {
				return err
			}
			ovlShare = append(ovlShare, ovlDir...)

			return nil
		}
		if err := pullImage(); err != nil {
			return ret.Errorf(errorcode.ErrorCode_PullImageFailed, "pull image failed: %v", err)
		}
		if len(ovlShare) > 0 {
			mountsConfig.Overlay = virtiofs.GenOverlayMountConfig(ovlShare)
		}
	}

	mounts, err := l.prepareExternalVolume(ctx, containerReq, flowOpts)
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_CreateVolumeFailed, "prepare external volume failed: %v", err)
	}

	cubeMounts, err := virtiofs.GenMountConfig(mounts)
	if err != nil {
		return err
	}
	mountsConfig.Mounts = append(mountsConfig.Mounts, cubeMounts...)

	ci.CubeRootfsInfo = mountsConfig
	return nil
}

func appendExt4NetfileMounts(
	mountsConfig *virtiofs.CubeRootfsInfo,
	flowOpts *workflow.CreateContext,
	containerName string,
) {
	if mountsConfig == nil || flowOpts == nil || flowOpts.NetFile == nil {
		return
	}

	netfileMounts := flowOpts.NetFile.ContainerVirtiofsMounts(containerName)
	if len(netfileMounts) == 0 {
		return
	}

	mountsConfig.Mounts = append(mountsConfig.Mounts, netfileMounts...)
}

func (l *local) getSandboxRuntime(req *cubebox.RunCubeSandboxRequest) (cubeconfig.Runtime, error) {
	runtimeHandler := req.GetRuntimeHandler()
	if req == nil || runtimeHandler == "" {
		runtimeHandler = l.config.DefaultRuntimeName
	}
	handler, ok := l.config.Runtimes[runtimeHandler]
	if !ok {
		return cubeconfig.Runtime{}, fmt.Errorf("no runtime for %q is configured", runtimeHandler)
	}
	return handler, nil
}

func WithCubeFsAnnotation(ctx context.Context,
	realReq *cubebox.RunCubeSandboxRequest, sandBox *cubeboxstore.CubeBox) ([]oci.SpecOpts, error) {
	var opts []oci.SpecOpts

	var (
		shareDirs []string
		rootfsC   []*virtiofs.CubeRootfsInfo
	)
	for _, ci := range sandBox.AllContainers() {
		shareDirs = append(shareDirs, ci.CubeRootfsInfo.ShareDirs()...)
		rootfsC = append(rootfsC, ci.CubeRootfsInfo)
	}

	virtiofsConfig, err := virtiofs.GenVirtiofsConfig(shareDirs)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "generate virtiofs config failed: %v", err)
	}
	limit, err := workflow.GetQosFromReq(realReq, constants.MasterAnnotationsFSQos)
	if err != nil {
		return nil, err
	}
	if limit != nil {
		virtiofsConfig.RateLimiter = *limit
	}

	if sandBox.VirtiofsMap == nil {
		sandBox.VirtiofsMap = make(map[string]*virtiofs.VirtiofsConfig)
	}
	sandBox.VirtiofsMap[constants.CubeDefaultNamespace] = virtiofsConfig
	vc, _ := json.Marshal(virtiofsConfig)
	fsstr := string(vc)
	sandBox.FirstContainer().AddAnnotations(map[string]string{constants.AnnotationsFSKey: fsstr})
	opts = append(opts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		for k, v := range sandBox.FirstContainer().Annotations {
			s.Annotations[k] = v
		}
		log.G(ctx).WithFields(CubeLog.Fields{
			"method":  "WithCubeFsAnnotation",
			"cube.fs": fsstr,
		}).Debugf("with cube fs annotation")
		return nil
	})
	return opts, nil
}

func (l *local) containerOciSpec(ctx context.Context, containerReq *cubebox.ContainerConfig,
	flowOpts *workflow.CreateContext, ci *cubeboxstore.Container,
	sandBox *cubeboxstore.CubeBox) (specOpts []oci.SpecOpts, err error) {

	specOpts = append(specOpts, container.GenOpt(ctx, containerReq)...)

	specOpts = append(specOpts, l.genRuntimeCfgAnnotationOpt(ctx, sandBox.OciRuntime, flowOpts)...)

	if opt, err := l.genHooksOpts(ctx, flowOpts, containerReq); err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "gen hooks opts failed: %v", err)
	} else {
		specOpts = append(specOpts, opt)
	}

	imageSpecConfig := &imagespec.ImageConfig{}
	if !isImageStorageMediaType(containerReq, cubeimages.ImageStorageMediaType_ext4) {
		var image cristore.Image
		image, err = l.criImage.LocalResolve(ctx, containerReq.GetImage().GetImage())
		if err != nil {
			return nil, ret.Errorf(errorcode.ErrorCode_ResolveLocalSpecFailed,
				"local resolve image %q: %v", containerReq.GetImage().Image, err)
		}
		imageSpecConfig = &image.ImageSpec.Config

		opts, err := rootfs.GenRootfsOpt(ctx, containerReq, &image)
		if err != nil {
			return nil, ret.Errorf(errorcode.ErrorCode_CreateContainerFailed, "generate rootfs options failed: %v", err)
		}
		specOpts = append(specOpts, opts...)
	}

	opts, err := l.cbriBeforeCreateContainer(ctx, flowOpts, sandBox, ci)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "cbri before create container failed: %v", err)
	}
	specOpts = append(specOpts, opts...)

	specOpts = append(specOpts, genGeneralContainerSpecOpt(ctx, containerReq, ci, imageSpecConfig)...)

	opt, err := l.prepareWritableRootfs(ctx, flowOpts, containerReq)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "prepare writable rootfs failed: %v", err)
	}
	specOpts = append(specOpts, opt)

	cgroupOpts, err := cgroup.GenOpt(ctx, containerReq)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "failed to gen resource: %s", err.Error())
	}
	specOpts = append(specOpts, cgroupOpts...)

	specOpts = append(specOpts, l.genCgroupAnnotationOpt(ctx, containerReq, flowOpts)...)

	netOpts, err := l.genNetworkAnnotationOpt(ctx, containerReq, flowOpts, ci)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "generate network annotation failed: %v", err)
	}
	specOpts = append(specOpts, netOpts...)

	opt, err = l.prepareVolume(ctx, containerReq, flowOpts, ci)
	if err != nil {
		return nil, ret.WrapWithDefaultError(err, errorcode.ErrorCode_CreateVolumeFailed)
	}
	specOpts = append(specOpts, opt)

	specOpts = append(specOpts, l.genVirtFsAnnotationOpt(ctx, flowOpts, ci.CubeRootfsInfo))
	return specOpts, nil
}

func (l *local) containerSpec(ctx context.Context, sandBox *cubeboxstore.CubeBox, containerReq *cubebox.ContainerConfig,
	flowOpts *workflow.CreateContext, ci *cubeboxstore.Container, additionalOpts []oci.SpecOpts,
) ([]containerd.NewContainerOpts, error) {
	var (
		cOpts []containerd.NewContainerOpts
	)

	if !constants.IsCubeRuntime(ctx) {
		containerdImage, err := l.criImage.EnsureImage(ctx, containerReq.GetImage().GetImage(),
			containerReq.GetImage().GetUsername(),
			containerReq.GetImage().GetToken(),
			&runtime.PodSandboxConfig{})
		if err != nil {

			return nil, err
		}
		cOpts = append(cOpts,
			containerd.WithSnapshotter(sandBox.OciRuntime.Snapshotter),
			containerd.WithNewSnapshot(ci.ID, containerdImage,
				snapshots.WithLabels(snapshots.FilterInheritedLabels(flowOpts.ReqInfo.GetAnnotations()))))
	}
	var s specs.Spec
	cOpts = append(cOpts,

		containerd.WithSpec(&s, additionalOpts...),
	)

	var extendsLabels = map[string]string{
		constants.LabelContainerImageMedia: containerReq.GetImage().GetStorageMedia(),
	}
	if isImageStorageMediaType(containerReq, cubeimages.ImageStorageMediaType_ext4) {
		extendsLabels[constants.LabelContainerImagePem] = ci.Metadata.Config.GetImage().Image
		sandBox.AddImageReference(cubeboxstore.ImageReference{
			ID:     ci.Metadata.Config.GetImage().Image,
			Medium: cubeimages.ImageStorageMediaType_ext4,
		})
	} else {
		storedImage, err := l.criImage.LocalResolve(ctx, containerReq.GetImage().GetImage())
		if err != nil {
			return nil, fmt.Errorf("local resolve image %q: %v", containerReq.GetImage().GetImage(), err)
		}
		maps.Copy(extendsLabels, storedImage.ImageSpec.Config.Labels)
		extendsLabels[constants.LabelContainerImageMedia] = containerReq.GetImage().GetStorageMedia()
		sandBox.AddImageReference(cubeboxstore.ImageReference{
			ID:         storedImage.ID,
			References: storedImage.References,
			Medium:     cubeimages.ImageStorageMediaType(cubeimages.ImageStorageMediaType_value[storedImage.MediaType]),
		})
	}

	containerType := cubelabels.ContainerKindContainer
	if ci.IsPod {
		containerType = cubelabels.ContainerKindSandbox
	}
	containerLabels := util.BuildLabels(nil, extendsLabels)

	containerLabels[constants.ContainerType] = containerType
	containerLabels[constants.LabelCriContainerType] = containerType
	containerLabels[constants.LabelCriSandboxID] = sandBox.ID
	ci.AddLabels(containerLabels)
	cOpts = append(cOpts, containerd.WithContainerLabels(containerLabels))

	return cOpts, nil
}

func (l *local) genContainerLabels(ci *cubeboxstore.Container, req *cubebox.RunCubeSandboxRequest) map[string]string {
	labels := make(map[string]string)
	if ci.IsPod {
		labels[constants.ContainerType] = constants.ContainerTypeSandBox
		for k, v := range req.GetLabels() {
			labels[k] = v
		}
	} else {
		labels[constants.ContainerType] = constants.ContainerTypeContainer
	}
	labels[constants.SandboxID] = ci.SandboxID
	return labels
}

func genGeneralContainerSpecOpt(ctx context.Context,
	containerReq *cubebox.ContainerConfig,
	ci *cubeboxstore.Container,
	imageSpecConfig *imagespec.ImageConfig,
) []oci.SpecOpts {
	var specOpts []oci.SpecOpts

	specOpts = append(specOpts, command.WithProcessArgs(containerReq, imageSpecConfig))

	specOpts = append(specOpts, uid.GenOpt(ctx, containerReq, imageSpecConfig)...)

	if containerReq.GetWorkingDir() != "" {
		specOpts = append(specOpts, oci.WithProcessCwd(containerReq.GetWorkingDir()))
	} else if wkDir := imageSpecConfig.WorkingDir; wkDir != "" {
		specOpts = append(specOpts, oci.WithProcessCwd(wkDir))
	}

	if containerReq.GetEnvs() != nil {
		specOpts = append(specOpts, env.GenOpt(ctx, containerReq, imageSpecConfig)...)
	}

	specOpts = append(specOpts, capability.GenOpt(ctx, containerReq)...)

	if containerReq.GetRLimit() != nil {
		specOpts = append(specOpts, rlimit.GenOpt(ctx, containerReq.GetRLimit().NoFile))
	}

	specOpts = append(specOpts, seccomp.GenOpt(ctx, containerReq.Syscalls))

	if containerReq.GetSysctls() != nil {
		specOpts = append(specOpts, sysctl.GenOpt(containerReq.GetSysctls()))
	}

	if ss := containerReq.GetSecurityContext(); ss != nil && ss.GetPrivileged() {
		specOpts = append(specOpts, oci.WithPrivileged)
	}

	specOpts = append(specOpts, genContainerAnnotationReq(ctx, containerReq, ci))

	if ci.IsDebugStdout {
		specOpts = append(specOpts, func() oci.SpecOpts {
			return func(ctx context.Context, client oci.Client, c *containers.Container, s *specs.Spec) error {
				return oci.WithTTY(ctx, client, c, s)
			}
		}())
		specOpts = append(specOpts, oci.WithAnnotations(map[string]string{
			constants.AnnotationsDebugStdout: "true",
		}))
	}

	if ss := containerReq.GetSecurityContext(); ss != nil && ss.GetNoNewPrivs() {
		specOpts = append(specOpts, oci.WithNoNewPrivileges)
	}

	return specOpts
}

func (l *local) cbriBeforeCreateContainer(ctx context.Context,
	flowOpts *workflow.CreateContext,
	cubeBox *cubeboxstore.CubeBox,
	ci *cubeboxstore.Container) ([]oci.SpecOpts, error) {
	if ci.IsPod {
		return l.cbriManager.CreateSandbox(ctx, flowOpts)
	}
	return l.cbriManager.CreateContainer(ctx, cubeBox, ci)
}

func (l *local) cubeMsgMountOpt(ctx context.Context, flowOpts *workflow.CreateContext, containerReq *cubebox.ContainerConfig,
	ci *cubeboxstore.Container) ([]oci.SpecOpts, error) {
	var Opts []oci.SpecOpts

	var mounts []specs.Mount
	if ci.IsPod {
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
				log.G(ctx).Debugf("req GetEmptyDir:%+v,vName:%s",
					v.GetVolumeSource().GetEmptyDir(), v.Name)
				cubeMsgVolumeName = v.Name
				break
			}
		}
		if cubeMsgVolumeName != "" {
			for _, v := range containerReq.VolumeMounts {
				if cubeMsgVolumeName == v.GetName() {
					mounts = append(mounts, specs.Mount{
						Type:        constants.MountTypeBind,
						Source:      constants.CubeMsgDevDefaultName,
						Destination: v.GetContainerPath(),
						Options: []string{
							constants.MountOptReadOnly, constants.MountPropagationRprivate, constants.MountOptBindRO,
						},
					})
					break
				}
			}
		}
	}

	if ci.IsDebugStdout {
		return append(Opts, oci.WithMounts(mounts)), nil
	}

	return append(Opts, oci.WithMounts(mounts)), nil
}

func prepareImagePmems(rootfsConfig []*virtiofs.CubeRootfsInfo) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, ctr *containers.Container, spec *oci.Spec) error {
		var tmpPmems []pmem.CubePmem
		for _, r := range rootfsConfig {
			if r.PmemFile != "" {
				fileInfo, err := os.Stat(r.PmemFile)
				if err != nil {
					return fmt.Errorf("prepareImagePmems failed to stat file %s: %v", r.PmemFile, err)
				}
				var fileSize int64
				if fileInfo != nil {
					fileSize = fileInfo.Size()
				}
				tmpPmems = append(tmpPmems, pmem.CubePmem{
					File:          r.PmemFile,
					DiscardWrites: true,
					SourceDir:     "/",
					FsType:        "ext4",
					Size:          fileSize,
					ID:            fmt.Sprintf("%s-%d", constants.AnnotationPmemContainerPrefix, len(tmpPmems)),
				})
			}
		}
		if len(tmpPmems) == 0 {
			return nil
		}

		if spec.Annotations == nil {
			spec.Annotations = make(map[string]string)
		}
		oldValues, ok := spec.Annotations[constants.AnnotationPmem]
		if ok {
			var pmemOpts []pmem.CubePmem
			if err := json.Unmarshal([]byte(oldValues), &pmemOpts); err != nil {
				return fmt.Errorf("failed to unmarshal pmem config: %v", err)
			}
			tmpPmems = append(tmpPmems, pmemOpts...)
		}

		pmemAnno, err := json.Marshal(tmpPmems)
		if err != nil {
			return fmt.Errorf("failed to marshal pmem config: %v", err)
		}

		spec.Annotations[constants.AnnotationPmem] = string(pmemAnno)
		log.G(ctx).Debugf("%s:%s", constants.AnnotationPmem, string(pmemAnno))
		return nil
	}
}

func prepareVolumePmems(flowOpts *workflow.CreateContext) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, ctr *containers.Container, spec *oci.Spec) error {
		var tmpPmems []pmem.CubePmem
		if flowOpts.VolumeInfo != nil {
			tmpInfo, ok := flowOpts.VolumeInfo.(*images.Info)
			if ok {
				for _, info := range tmpInfo.Volumes {
					if info.FileType == volumefile.FtLangExt4 {
						var fileSize int64
						fileInfo, err := os.Stat(info.FilePath)
						if err != nil {
							return fmt.Errorf("prepareVolumePmems failed to stat file %s: %v", info.FilePath, err)
						}
						if fileInfo != nil {
							fileSize = fileInfo.Size()
						}
						tmpPmems = append(tmpPmems, pmem.CubePmem{
							File:          info.FilePath,
							DiscardWrites: true,
							SourceDir:     "/",
							FsType:        "ext4",
							Size:          fileSize,
							ID:            fmt.Sprintf("%s-%d", constants.AnnotationPmemLangPrefix, len(tmpPmems)),
						})
					}
				}
			}
		}

		if len(tmpPmems) == 0 {
			return nil
		}

		if spec.Annotations == nil {
			spec.Annotations = make(map[string]string)
		}
		oldValues, ok := spec.Annotations[constants.AnnotationPmem]
		if ok {
			var pmemOpts []pmem.CubePmem
			if err := json.Unmarshal([]byte(oldValues), &pmemOpts); err != nil {
				return fmt.Errorf("failed to unmarshal pmem config: %v", err)
			}
			tmpPmems = append(tmpPmems, pmemOpts...)
		}

		pmemAnno, err := json.Marshal(tmpPmems)
		if err != nil {
			return fmt.Errorf("failed to marshal pmem config: %v", err)
		}
		log.G(ctx).Debugf("%s:%s", constants.AnnotationPmem, string(pmemAnno))
		spec.Annotations[constants.AnnotationPmem] = string(pmemAnno)
		return nil
	}
}

func (l *local) prepareVolumePmemsMounts(ctx context.Context, flowOpts *workflow.CreateContext,
	containerReq *cubebox.ContainerConfig) ([]oci.SpecOpts, error) {
	var mounts []specs.Mount
	if flowOpts.VolumeInfo != nil {
		tmpInfo, ok := flowOpts.VolumeInfo.(*images.Info)
		if ok && len(tmpInfo.Volumes) > 0 {
			for _, v := range containerReq.VolumeMounts {
				if file, ok := tmpInfo.Volumes[v.Name]; ok &&
					file.FileType == volumefile.FtLangExt4 {
					mounts = append(mounts, specs.Mount{
						Type:        constants.MountTypeBind,
						Source:      file.FilePath,
						Destination: v.ContainerPath,
						Options:     getMountOptions(v),
					})
				}
			}
		}
	}
	return append([]oci.SpecOpts{}, oci.WithMounts(mounts)), nil
}

func (l *local) prepareWritableRootfs(ctx context.Context, flowOpts *workflow.CreateContext, containerReq *cubebox.ContainerConfig,
) (oci.SpecOpts, error) {
	if containerReq.GetSecurityContext().GetReadonlyRootfs() {
		return func(ctx context.Context, c1 oci.Client, c2 *containers.Container, s *oci.Spec) error {
			return nil
		}, nil
	}

	found := false
	volumeName := ""
	for _, ctrReq := range containerReq.GetVolumeMounts() {
		if ctrReq.ContainerPath == "/" {
			found = true
			volumeName = ctrReq.GetName()
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("writable rootfs should provide Container.VolumeMounts.container_path=\"/\" " +
			"param and volumes.empty_dir")
	}

	foundBlk := false
	blkPath := ""
	if flowOpts.StorageInfo != nil {
		tmpInfo, ok := flowOpts.StorageInfo.(*storage.StorageInfo)
		if ok && len(tmpInfo.Volumes) > 0 {
			for _, v := range tmpInfo.Volumes {
				if volumeName == v.Name {
					foundBlk = true
					blkPath = v.FilePath
					break
				}
			}
		}
	}
	if !foundBlk {
		return nil, fmt.Errorf("writable rootfs should provide RunCubeSandboxRequest.volumes param")
	}

	annotations := make(map[string]string)
	annotations[constants.AnnotationsRootfsWritableKey] = blkPath
	annotations[constants.AnnotationsRootfsWlayerSubdir] = "disk/" + containerReq.GetId()
	log.G(ctx).Debugf("writable rootfs:%+v", blkPath)

	return oci.WithAnnotations(annotations), nil
}

func setCgroup(ctx context.Context, pid uint32, group string) {
	err := cgroupp.AddProc(group, uint64(pid))
	if err != nil {
		log.G(ctx).Errorf("add shim pid %d to cgroup %s failed: %v", pid, group, err)
	}
	log.G(ctx).Debugf("set cgroup proc success, pid: %d, cgroup: %s", pid, group)
}

func updateCgroup(ctx context.Context, ci *cubeboxstore.Container) error {
	cpuStr := ci.Config.GetResources().GetCpuLimit()
	if cpuStr == "" {
		cpuStr = ci.Config.GetResources().GetCpu()
	}
	if cpuStr != "" {
		cpuQ, err := resource.ParseQuantity(cpuStr)
		if err != nil {
			return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat,
				"cpu resource:%s", err.Error())
		}
		var (
			period = uint64(100000)
			quota  = cpuQ.MilliValue() * 100
		)
		res := &specs.LinuxResources{
			CPU: &specs.LinuxCPU{
				Quota:  &quota,
				Period: &period,
			},
		}
		go func(ctx context.Context) {
			namespace, err := namespaces.NamespaceRequired(ctx)
			if err != nil {
				log.G(ctx).Fatal(err.Error())
				return
			}
			ctxTmp := CubeLog.WithRequestTrace(context.Background(), CubeLog.GetTraceInfo(ctx))

			ctxTmp = namespaces.WithNamespace(ctxTmp, namespace)
			ctxTmp, cancel := context.WithTimeout(ctxTmp, 2*time.Second)
			defer cancel()
			defer recov.HandleCrash(func(panicError interface{}) {
				err = fmt.Errorf("update Resources panic :%v,stack:%s", panicError, string(debug.Stack()))
				log.G(ctxTmp).Fatalf("Update Resources error:%v", err)
			})
			t, err := ci.Container.Task(ctxTmp, nil)
			if err != nil && !errdefs.IsNotFound(err) {
				log.G(ctxTmp).Fatalf("Update Resources, load task %q:%v", ci.Container.ID(), err)
			}
			if err == nil {
				if err := t.Update(ctxTmp, containerd.WithResources(res)); err != nil {
					log.G(ctxTmp).Fatalf("Update Resources error:%v", err)
				}
			}
		}(ctx)
	}
	return nil
}

func (l *local) runContainer(
	ctx context.Context,
	cubebox *cubeboxstore.CubeBox,
	ci *cubeboxstore.Container,
	cOpts []containerd.NewContainerOpts,
	ociRuntime cubeconfig.Runtime) (err error) {

	start := time.Now()
	c, err := l.client.NewContainer(ctx, ci.ID, cOpts...)
	if err != nil {
		workflow.RecordCreateMetric(ctx, ret.Err(errorcode.ErrorCode_NewContainerMetaDataFailed, err.Error()),
			constants.CubeNewContainerId, time.Since(start))
		return ret.Err(errorcode.ErrorCode_NewContainerMetaDataFailed, fmt.Errorf("failed to create container [%s]: %w", ci.ID, err).Error())
	}
	workflow.RecordCreateMetric(ctx, err, constants.CubeNewContainerId, time.Since(start))

	ci.Container = c

	var ioCreater cio.Creator
	if ci.IsDebugStdout {
		ioCreater = taskio.New(false)
		debugStdout(ctx, ci.ID)
	} else {
		ioCreater = taskio.New(true)
	}

	var taskOpts []containerd.NewTaskOpts

	taskOpts = append(taskOpts, withRuntimePathOpt(cubebox))

	endpoint := cubebox.Endpoint
	if endpoint.IsValid() {
		taskOpts = append(taskOpts,
			containerd.WithTaskAPIEndpoint(endpoint.Address, endpoint.Version))
	}

	taskStart := time.Now()
	task, err := c.NewTask(ctx, ioCreater, taskOpts...)
	if err != nil {
		return transformError(err)
	}
	workflow.RecordCreateMetric(ctx, err,
		constants.CubeShimCreatetId,
		time.Since(taskStart))
	if ci.IsPod {
		shim, err := l.shims.Get(ctx, ci.ID)
		if err != nil {
			return ret.Err(errorcode.ErrorCode_ContainerNotFound, fmt.Sprintf("get shim %s failed, err: %v", ci.ID, err))
		}
		ep, v := shim.Endpoint()

		cubebox.Endpoint = sandboxstore.Endpoint{
			Address: ep,
			Version: uint32(v),
			Pid:     task.Pid(),
		}
	}

	exitCh, err := task.Wait(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_WaitTaskFailed, err.Error())
	}
	ci.ExitCh = exitCh

	if err := task.Start(ctx); err != nil {
		return ret.Err(errorcode.ErrorCode_StartTaskFailed, err.Error())
	}

	ci.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.Pid = task.Pid()
		status.StartedAt = time.Now().UnixNano()
		return status, nil
	})
	workflow.RecordCreateMetric(ctx, err,
		constants.CubeShimStartId,
		time.Since(taskStart))
	return nil
}

func getMountOptions(mount *cubebox.VolumeMounts) []string {
	mOptions := []string{constants.MountOptBindRO}
	switch mount.GetPropagation() {
	case cubebox.MountPropagation_PROPAGATION_PRIVATE:
		mOptions = append(mOptions, constants.MountPropagationRprivate)
	case cubebox.MountPropagation_PROPAGATION_BIDIRECTIONAL:

		mOptions = append(mOptions, constants.MountPropagationRShared)

	case cubebox.MountPropagation_PROPAGATION_HOST_TO_CONTAINER:

		mOptions = append(mOptions, constants.MountPropagationRSlave)
	default:
		mOptions = append(mOptions, constants.MountPropagationRprivate)
	}

	if mount.GetReadonly() {
		mOptions = append(mOptions, "ro")
	} else {
		mOptions = append(mOptions, "rw")
	}
	if mount.GetSubPath() != "" {
		subpath := mount.GetSubPath()
		subpath = filepath.Clean(subpath)

		subpath = strings.TrimPrefix(subpath, "/")
		subpath = fmt.Sprintf("blk-cube-source=%s", subpath)
		mOptions = append(mOptions, subpath)
	}
	return mOptions
}

func (l *local) prepareVolume(ctx context.Context,
	c *cubebox.ContainerConfig,
	opts *workflow.CreateContext,
	ci *cubeboxstore.Container,
) (oci.SpecOpts, error) {
	var specOpts []oci.SpecOpts
	var mounts []specs.Mount

	mounts = append(mounts, tmpfs.GenMount(ctx, c)...)

	realReq := opts.ReqInfo

	tmpfsMap := make(map[string]int64)
	for _, v := range realReq.Volumes {
		if v.GetVolumeSource() != nil && v.GetVolumeSource().GetEmptyDir() != nil {
			vDir := v.GetVolumeSource().GetEmptyDir()
			if vDir.GetMedium() == cubebox.StorageMedium_StorageMediumMemory {
				size, err := resource.ParseQuantity(vDir.SizeLimit)
				log.G(ctx).Debugf("req GetEmptyDir:%+v,vName:%s", vDir, v.Name)
				if err != nil {
					log.G(ctx).Errorf("valid EmptyDir SizeLimit")
					return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat,
						"invalid EmptyDir SizeLimit:%v", vDir.SizeLimit)
				}
				tmpfsMap[v.Name] = size.Value()
			}
		}
	}
	for _, v := range c.VolumeMounts {
		if size, ok := tmpfsMap[v.Name]; ok {
			mounts = append(mounts, tmpfs.GenSizeMountWithVolumeMount(ctx, v.ContainerPath, size, v)...)
		}
	}

	if constants.IsCubeRuntime(ctx) {
		if opts.StorageInfo != nil {
			tmpInfo, ok := opts.StorageInfo.(*storage.StorageInfo)
			if ok && len(tmpInfo.Volumes) > 0 {
				for _, v := range c.VolumeMounts {
					if v.ContainerPath == "/" {

						continue
					}
					if file, ok := tmpInfo.Volumes[v.Name]; ok {
						mounts = append(mounts, specs.Mount{
							Type:        constants.MountTypeBind,
							Source:      file.FilePath,
							Destination: v.ContainerPath,
							Options:     getMountOptions(v),
						})
					}
				}
			}
		}
	}

	mounts = append(mounts, l.prepareSandboxPathVolume(ctx, c, realReq)...)

	sOpts, err := l.prepareVolumeAnnotations(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("prepare cbs volume failed: %w", err)
	}
	specOpts = append(specOpts, sOpts...)

	sOpts, err = l.cubeMsgMountOpt(ctx, opts, c, ci)
	if err != nil {
		return nil, ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "prepare cubemsg mount failed: %v", err)
	}
	specOpts = append(specOpts, sOpts...)

	sOpts, err = l.prepareVolumePmemsMounts(ctx, opts, c)
	if err != nil {
		return nil, err
	}
	specOpts = append(specOpts, sOpts...)

	specOpts = append(specOpts, oci.WithMounts(mounts))

	if propagation, ok := c.Annotations[constants.AnnotationContaineRootfsPropagation]; ok {
		specOpts = append(specOpts, WithRootfsPropagation(propagation))
	}

	return oci.Compose(specOpts...), nil
}

func (l *local) prepareExternalVolume(ctx context.Context, c *cubebox.ContainerConfig, opts *workflow.CreateContext,
) ([]specs.Mount, error) {
	var mounts []specs.Mount

	if opts.VolumeInfo != nil {
		tmpInfo, ok := opts.VolumeInfo.(*images.Info)
		if ok && len(tmpInfo.Volumes) > 0 {
			for _, v := range c.VolumeMounts {
				if file, ok := tmpInfo.Volumes[v.Name]; ok &&
					file.FileType != volumefile.FtLangExt4 {
					mounts = append(mounts, specs.Mount{
						Type:        constants.MountTypeBind,
						Source:      file.FilePath,
						Destination: v.ContainerPath,
						Options:     getMountOptions(v),
					})
				}
			}
		}
	}

	cmounts, err := l.cbriManager.GetPassthroughMounts(ctx, opts)
	if err != nil {
		return nil, err
	}

	return append(mounts, cmounts...), nil
}

func (l *local) prepareSandboxPathVolume(ctx context.Context, c *cubebox.ContainerConfig, realReq *cubebox.RunCubeSandboxRequest,
) []specs.Mount {
	var mounts []specs.Mount
	hostPathVolumeSource := make(map[string]*cubebox.SandboxPathVolumeSource)
	for _, v := range realReq.Volumes {
		if v.GetVolumeSource() != nil && v.GetVolumeSource().GetSandboxPath() != nil {
			hostPathVolumeSource[v.Name] = v.GetVolumeSource().GetSandboxPath()
		}
	}
	if len(hostPathVolumeSource) == 0 {
		return mounts
	}

	for _, v := range c.VolumeMounts {
		if sp, ok := hostPathVolumeSource[v.Name]; ok {
			switch sp.GetType() {
			case cubebox.SandboxPathType_Cgroup.String():
				mounts = append(mounts, specs.Mount{
					Type:        constants.MountTypeBind,
					Source:      filepath.Join("/sys/fs/cgroup", path.Clean(sp.GetPath())),
					Destination: v.ContainerPath,
					Options: []string{
						constants.MountOptBind,
						constants.MountOptReadOnly, constants.MountOptNoDev,
						constants.MountOptNoSuid, constants.MountOptNoExec,
					},
				})
			case cubebox.SandboxPathType_Directory.String():
				t := constants.MountTypeBind
				mounts = append(mounts, specs.Mount{
					Type:        t,
					Source:      path.Clean(sp.GetPath()),
					Destination: v.ContainerPath,
					Options:     getMountOptions(v),
				})
			case cubebox.SandboxPathType_SharedBindMount.String():
				t := constants.MountTypeBindShared
				mounts = append(mounts, specs.Mount{
					Type:        t,
					Source:      path.Clean(sp.GetPath()),
					Destination: v.ContainerPath,
					Options:     getMountOptions(v),
				})
			}
		}
	}

	return mounts
}

func (l *local) prepareVolumeAnnotations(ctx context.Context, opts *workflow.CreateContext,
) ([]oci.SpecOpts, error) {
	_ = ctx
	_ = opts
	return nil, nil
}

func isImageStorageMediaType(containerReq *cubebox.ContainerConfig, mediaType cubeimages.ImageStorageMediaType) bool {
	toCheck := containerReq.GetImage().GetStorageMedia()
	if toCheck == "" {
		toCheck = cubeimages.ImageStorageMediaType_docker.String()
	}

	return toCheck == mediaType.String()
}

func (l *local) storeNumaQueues(ctx context.Context, cubebox *cubeboxstore.CubeBox, opts *workflow.CreateContext) {
	_ = ctx
	cubebox.NumaNode = opts.GetNumaNode()
	if opts.StorageInfo != nil {
		tmpInfo, ok := opts.StorageInfo.(*storage.StorageInfo)
		if ok {
			cubebox.Queues += tmpInfo.GetNICQueues()
		}
	}
	if opts.NetworkInfo != nil {
		cubebox.Queues += opts.NetworkInfo.GetNICQueues()
	}
}

func withRuntimePathOpt(cubebox *cubeboxstore.CubeBox) containerd.NewTaskOpts {
	var runtimePath string
	if cubebox.OciRuntime != nil && cubebox.OciRuntime.Path != "" {
		runtimePath = cubebox.OciRuntime.Path
	}

	if cubebox.LocalRunTemplate != nil {
		if shim, ok := cubebox.LocalRunTemplate.Componts[templatetypes.CubeComponentCubeShim]; ok {
			if shim.Component.Path != "" {
				runtimePath = shim.Component.Path
			}
		}
	}
	if p, ok := cubebox.Annotations[constants.AnnotationCubeletInternalRuntimePath]; ok {
		runtimePath = p
	}
	return containerd.WithRuntimePath(runtimePath)
}

func (l *local) genHooksOpts(ctx context.Context, flowOpts *workflow.CreateContext, containerReq *cubebox.ContainerConfig) (oci.SpecOpts, error) {
	_ = ctx
	return func(ctx context.Context, client oci.Client, ctr *containers.Container, spec *oci.Spec) error {
		if containerReq.Hooks == nil {
			return nil
		}
		if containerReq.Hooks.Prestart != nil {
			if spec.Hooks == nil {
				spec.Hooks = &specs.Hooks{}
			}
			for _, hook := range containerReq.Hooks.Prestart {
				if hook.Path == "" {
					return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "hook path should be provided")
				}
				var timeout *int
				if hook.Timeout != nil {
					t := int(*hook.Timeout)
					timeout = &t
				}
				spec.Hooks.Prestart = append(spec.Hooks.Prestart, specs.Hook{
					Path:    hook.Path,
					Args:    hook.Args,
					Env:     hook.Env,
					Timeout: timeout,
				})
			}
		}
		return nil
	}, nil
}
