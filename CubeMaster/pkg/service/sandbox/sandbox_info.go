// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func SandboxInfo(ctx context.Context, req *types.GetCubeSandboxReq) (rsp *types.GetCubeSandboxRes) {
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}
	rsp = &types.GetCubeSandboxRes{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	log.G(ctx).Infof("GetSandboxInfo:%+v", utils.InterfaceToString(req))
	defer func() {
		if log.IsDebug() {
			log.G(ctx).Debugf("GetSandboxInfo_rsp:%+v", utils.InterfaceToString(rsp))
		} else if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Warnf("GetSandboxInfo fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	start := time.Now()
	rt := CubeLog.GetTraceInfo(ctx).DeepCopy()
	rt.Callee = constants.CubeLet
	defer func() {
		rt.Cost = time.Since(start)
		rt.RetCode = int64(rsp.Ret.RetCode)
		rt.CalleeAction = "Info"
		CubeLog.Trace(rt)
	}()

	cubeletReq := &cubebox.ListCubeSandboxRequest{}
	rt.CalleeEndpoint = checkValidAndGetReq(ctx, req, cubeletReq, rsp)
	if rt.CalleeEndpoint == "" {
		return
	}
	err := doget(ctx, rt.CalleeEndpoint, cubeletReq, rsp)
	if err != nil {
		setError(errorcode.ErrorCode_ReqCubeAPIFailed, rsp)
		return
	}

	if len(rsp.Data) == 0 {
		setError(errorcode.ErrorCode_NotFoundAtCubelet, rsp)
		return
	}
	if err := decorateSandboxInfo(ctx, req, rsp); err != nil {
		setError(errorcode.ErrorCode_MasterParamsError, rsp)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	return
}

func setError(code errorcode.ErrorCode, rsp *types.GetCubeSandboxRes) string {
	rsp.Ret.RetCode = int(code)
	rsp.Ret.RetMsg = code.String()
	return ""
}
func checkValidAndGetReq(ctx context.Context, req *types.GetCubeSandboxReq, cubeletReq *cubebox.ListCubeSandboxRequest,
	rsp *types.GetCubeSandboxRes) string {
	var n *node.Node
	var exist bool
	var calleeEndpoint string

	if req.SandboxID != "" && req.HostID != "" {
		cubeletReq.Id = &req.SandboxID
		n, exist = localcache.GetNode(req.HostID)
		if !exist {
			return setError(errorcode.ErrorCode_NotFound, rsp)
		}
		if !n.Healthy {
			return setError(errorcode.ErrorCode_CubeletUnHealthy, rsp)
		}
		return cubelet.GetCubeletAddr(n.IP)
	}

	switch {
	case req.SandboxID != "":
		var hostIP string
		if v := localcache.GetSandboxCache(req.SandboxID); v != nil {
			hostIP = v.HostIP
		} else if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID); ok {
			hostIP = proxyMap.HostIP
		} else {
			return setError(errorcode.ErrorCode_NotFound, rsp)
		}
		cubeletReq.Id = &req.SandboxID
		calleeEndpoint = cubelet.GetCubeletAddr(hostIP)
		n, exist = localcache.GetNodesByIp(hostIP)
	case req.HostID != "":
		n, exist = localcache.GetNode(req.HostID)
		if !exist {
			return setError(errorcode.ErrorCode_NotFound, rsp)
		}
		if req.SandboxID != "" {
			cubeletReq.Id = &req.SandboxID
		}
		calleeEndpoint = cubelet.GetCubeletAddr(n.IP)
	default:
		return setError(errorcode.ErrorCode_MasterParamsError, rsp)
	}

	if !exist {
		return setError(errorcode.ErrorCode_NotFound, rsp)
	}
	if !n.Healthy {
		return setError(errorcode.ErrorCode_CubeletUnHealthy, rsp)
	}
	return calleeEndpoint
}

func doget(ctx context.Context, calleep string, cubeletReq *cubebox.ListCubeSandboxRequest,
	rsp *types.GetCubeSandboxRes) error {
	cubeRsp, err := cubelet.List(ctx, calleep, cubeletReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return err
	}

	for _, sandbox := range cubeRsp.GetItems() {
		one := &types.SandboxData{
			SandboxID: sandbox.GetId(),
			NameSpace: sandbox.GetNamespace(),
		}
		sandboxLabels := cloneStringMap(sandbox.GetLabels())

		for _, container := range sandbox.GetContainers() {
			if container.GetId() == sandbox.GetId() {
				one.Status = int32(container.GetState())
				sandboxLabels = sandboxViewLabels(sandboxLabels, container.GetLabels())
			}
			containerInfo := &types.ContainerInfo{
				Name:        getContainerName(container.GetLabels()),
				ContainerID: container.GetId(),
				Status:      int32(container.GetState()),
				Image:       container.GetImage(),
				CreateAt:    container.GetCreatedAt(),
				Cpu:         container.GetResources().GetCpu(),
				Mem:         container.GetResources().GetMem(),
				Type:        container.GetType(),
				PauseAt:     container.GetPausedAt(),
			}
			one.Containers = append(one.Containers, containerInfo)
		}
		templateID := templateIDFromLabels(sandboxLabels)
		one.TemplateID = templateID
		one.Annotations = buildAnnotationsFromLabels(sandboxLabels)
		one.Labels = sandboxLabels
		one.EndAt = LookupSandboxEndAt(ctx, sandbox.GetId())
		rsp.Data = append(rsp.Data, one)
	}
	return nil
}

func decorateSandboxInfo(ctx context.Context, req *types.GetCubeSandboxReq, rsp *types.GetCubeSandboxRes) error {
	if len(rsp.Data) == 0 {
		return nil
	}
	if req.HostID != "" {
		for _, item := range rsp.Data {
			item.HostID = req.HostID
		}
	}
	if req.SandboxID == "" {
		return nil
	}
	proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID)
	if !ok || proxyMap == nil {
		return nil
	}
	if n, exist := localcache.GetNodesByIp(proxyMap.HostIP); exist {
		for _, item := range rsp.Data {
			item.HostID = n.ID()
		}
	}
	for _, item := range rsp.Data {
		item.HostIP = proxyMap.HostIP
		item.SandboxIP = proxyMap.SandboxIP
	}
	if req.ContainerPort == 0 {
		return nil
	}
	endpoint, err := ResolveExposedPortEndpoint(constants.GetHostIP(ctx), proxyMap, req.ContainerPort)
	if err != nil {
		return err
	}
	for _, item := range rsp.Data {
		item.HostIP = endpoint.HostIP
		item.SandboxIP = endpoint.SandboxIP
		item.RequestedContainerPort = endpoint.ContainerPort
		item.ExposedPortEndpoint = endpoint.Address
		item.ExposedPortMode = endpoint.Mode
	}
	return nil
}

func getContainerName(label map[string]string) string {
	const containerNameKey = "io.kubernetes.cri.container-name"
	if name, ok := label[containerNameKey]; ok {
		return name
	}
	return ""
}
