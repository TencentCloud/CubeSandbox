// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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
	if req.SandboxID != "" {
		if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
			rsp.Ret = ret
			return
		}
	}
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
	endpoint, rec, ok := checkValidAndGetReq(ctx, req, cubeletReq, rsp)
	if !ok {
		return
	}
	rt.CalleeEndpoint = endpoint
	if endpoint != "" {
		if err := doget(ctx, endpoint, cubeletReq, rsp); err != nil {
			setError(errorcode.ErrorCode_ReqCubeAPIFailed, rsp)
			return
		}
	}

	// Pause binding overlays status onto the node view. It does not replace
	// identity fields. A shimless binding whose node cannot be asked is
	// rendered from the persisted create spec.
	if applyPauseBindingToInfo(ctx, req, rsp, rec) {
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

// checkValidAndGetReq locates the cubelet that should answer an Info query.
//
// ok is false when rsp already carries the error. An empty endpoint with ok
// means the node cannot be asked, but a shimless pause binding can still be
// rendered from Master. rec is the pause binding read for this sandbox, or
// nil when there is none; callers must not read it again.
func checkValidAndGetReq(ctx context.Context, req *types.GetCubeSandboxReq, cubeletReq *cubebox.ListCubeSandboxRequest,
	rsp *types.GetCubeSandboxRes) (endpoint string, rec *pausesnap.Record, ok bool) {
	if req.SandboxID == "" && req.HostID == "" {
		setError(errorcode.ErrorCode_MasterParamsError, rsp)
		return "", nil, false
	}

	if req.SandboxID != "" {
		cubeletReq.Id = &req.SandboxID
		rec = lookupPauseBinding(ctx, req.SandboxID)
	}

	var (
		n      *node.Node
		exist  bool
		hostIP string
	)
	if req.HostID != "" {
		n, exist = localcache.GetNode(req.HostID)
		if !exist {
			setError(errorcode.ErrorCode_NotFound, rsp)
			return "", nil, false
		}
		hostIP = n.IP
	} else {
		hostIP = cachedSandboxHostIP(ctx, req.SandboxID)
		if hostIP == "" && rec != nil {
			hostIP = strings.TrimSpace(rec.NodeIP)
		}
		if hostIP == "" {
			setError(errorcode.ErrorCode_NotFound, rsp)
			return "", nil, false
		}
		n, exist = localcache.GetNodesByIp(hostIP)
	}

	if !exist || !n.Healthy {
		// A caller-pinned host keeps the old error. Only an unpinned Info of a
		// shimless pause can be answered without the node.
		if req.HostID == "" && rec != nil && isShimlessPauseStatus(rec.Status) {
			return "", rec, true
		}
		if !exist {
			setError(errorcode.ErrorCode_NotFound, rsp)
		} else {
			setError(errorcode.ErrorCode_CubeletUnHealthy, rsp)
		}
		return "", nil, false
	}
	return cubelet.GetCubeletAddr(hostIP), rec, true
}

// cachedSandboxHostIP is where the sandbox was last seen running. It wins
// over the pause binding's node: after a cross-node resume whose binding
// cleanup failed, the binding still names the origin node.
func cachedSandboxHostIP(ctx context.Context, sandboxID string) string {
	if v := localcache.GetSandboxCache(sandboxID); v != nil {
		return v.HostIP
	}
	if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok && proxyMap != nil {
		return proxyMap.HostIP
	}
	return ""
}

func lookupPauseBinding(ctx context.Context, sandboxID string) *pausesnap.Record {
	rec, err := pausesnap.GetBySandbox(ctx, sandboxID)
	if err != nil {
		if !errors.Is(err, pausesnap.ErrNotFound) && !errors.Is(err, pausesnap.ErrNotReady) {
			log.G(ctx).Warnf("GetSandboxInfo: read pause binding %s: %v", sandboxID, err)
		}
		return nil
	}
	if rec == nil || strings.TrimSpace(rec.SnapshotID) == "" {
		return nil
	}
	return rec
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
				CpuMilli:    parseCPUMilli(container.GetResources().GetCpu()),
				MemoryMiB:   parseMemoryMiB(container.GetResources().GetMem()),
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
		one.VolumeMounts = volumeMountsToContainerInfo(collectVolumeMountsFromContainers(sandbox.GetContainers()))
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

// applyPauseBindingToInfo lets the pause binding decide the reported status
// of an Info view without replacing its identity. It returns true when it
// has produced the final response.
func applyPauseBindingToInfo(ctx context.Context, req *types.GetCubeSandboxReq,
	rsp *types.GetCubeSandboxRes, rec *pausesnap.Record) bool {
	if rec == nil || req == nil || rsp == nil || req.SandboxID == "" {
		return false
	}
	item := findSandboxData(rsp.Data, req.SandboxID)
	var observed int32
	if item != nil {
		observed = item.Status
	}
	view := decidePauseView(rec, observed, item != nil)
	switch {
	case view.stale:
		recordStalePauseBinding(ctx, "info", req.SandboxID, rec)
		return false
	case !view.override:
		// No observation and a binding status we do not synthesize (unknown
		// status): leave the response alone so Info can report not found.
		if item == nil {
			return false
		}
		item.Annotations = overlayPauseAnnotations(item.Annotations, rec, "")
		return false
	}
	if item == nil {
		// The caller named a host that does not hold this binding. An empty
		// list from that host means the sandbox is not there; do not invent a
		// paused sandbox and stamp the caller's host on it. An unpinned Info,
		// or a pin that matches the binding's node, still synthesizes: that is
		// how CREATING/FAILED stay visible when the node has not reported a row.
		if req.HostID != "" && !pauseBindingOnHost(rec, req.HostID) {
			return false
		}
		item = sandboxDataFromSpec(req.SandboxID, loadSandboxSpec(ctx, req.SandboxID))
		item.EndAt = LookupSandboxEndAt(ctx, req.SandboxID)
	}
	setSandboxDataStatus(item, view.status)
	item.Annotations = overlayPauseAnnotations(item.Annotations, rec, view.pauseErr)
	setPauseLocation(ctx, req, item, rec)
	rsp.Data = []*types.SandboxData{item}
	if rsp.Ret == nil {
		rsp.Ret = &types.Ret{}
	}
	rsp.Ret.RetCode = int(errorcode.ErrorCode_Success)
	rsp.Ret.RetMsg = errorcode.ErrorCode_Success.String()
	return true
}

func pauseBindingOnHost(rec *pausesnap.Record, hostID string) bool {
	hostID = strings.TrimSpace(hostID)
	if rec == nil || hostID == "" {
		return false
	}
	return rec.NodeID == hostID || rec.NodeIP == hostID
}

func findSandboxData(items []*types.SandboxData, sandboxID string) *types.SandboxData {
	for _, item := range items {
		if item != nil && item.SandboxID == sandboxID {
			return item
		}
	}
	return nil
}

func loadSandboxSpec(ctx context.Context, sandboxID string) *types.CreateCubeSandboxReq {
	spec, err := sandboxspec.Get(ctx, sandboxID)
	if err != nil {
		if !errors.Is(err, sandboxspec.ErrSandboxSpecNotFound) &&
			!errors.Is(err, sandboxspec.ErrSandboxSpecStoreNotReady) {
			log.G(ctx).Warnf("GetSandboxInfo: read sandbox spec %s: %v", sandboxID, err)
		}
		return nil
	}
	return spec
}

// setPauseLocation reports where the sandbox lives. A shimless binding names
// the node holding the pause package; otherwise the proxy map does.
func setPauseLocation(ctx context.Context, req *types.GetCubeSandboxReq, item *types.SandboxData, rec *pausesnap.Record) {
	if item == nil || rec == nil {
		return
	}
	proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID)
	hostIP := ""
	if isShimlessPauseStatus(rec.Status) {
		hostIP = strings.TrimSpace(rec.NodeIP)
	}
	if hostIP == "" && ok && proxyMap != nil {
		hostIP = proxyMap.HostIP
	}
	if ok && proxyMap != nil {
		item.SandboxIP = proxyMap.SandboxIP
	}
	item.HostIP = hostIP
	if n, exist := localcache.GetNodesByIp(hostIP); exist && n != nil {
		item.HostID = n.ID()
	} else if isShimlessPauseStatus(rec.Status) {
		item.HostID = rec.NodeID
	}
	if req.HostID != "" {
		item.HostID = req.HostID
	}
}
