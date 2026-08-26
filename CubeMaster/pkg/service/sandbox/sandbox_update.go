// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func Update(ctx context.Context, req *types.UpdateRequest) (rsp *types.Res) {
	rsp = &types.Res{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	defer func() {
		logger := log.G(ctx).WithFields(map[string]interface{}{
			"RequestId": req.RequestID,
			"RetCode":   int64(rsp.Ret.RetCode),
		})
		logger.Infof("Update:%+v", utils.InterfaceToString(req))
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			logger.Errorf("Update fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	if req.SandboxID == "" || req.InstanceType == "" || req.Action == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "should provide InstanceType,SandboxID,Action"
		return
	}
	if req.Action != "pause" && req.Action != "resume" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "action should be pause or resume"
		return
	}
	if ret := normalizeSandboxIDInReq(ctx, &req.SandboxID); ret != nil {
		rsp.Ret = ret
		return
	}

	// Covers API pause/resume and CLM auto-pause/auto-resume (both hit Update).
	// Hold until pause/resume reaches a terminal Master outcome; concurrent
	// delete/resume/pause on the same sandboxID must wait or Conflict.
	lockOpts := sandboxlock.Options{Value: req.Action}
	switch req.Action {
	case "pause":
		lockOpts.TTL = sandboxlock.PauseTTL
	case "resume":
		lockOpts.TTL = sandboxlock.ResumeTTL
	}
	err := sandboxlock.WithLock(ctx, req.SandboxID, lockOpts, func(ctx context.Context) error {
		// Client/HTTP cancel must not abort bookkeeping or release the lock
		// while SAVE/Create/Destroy is still in flight on Cubelet.
		ctx = context.WithoutCancel(ctx)
		var hostIP string
		if v := localcache.GetSandboxCache(req.SandboxID); v != nil {
			hostIP = v.HostIP
		} else if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID); ok {
			hostIP = proxyMap.HostIP
		} else if ip, ok := resolvePauseHostIP(ctx, req.SandboxID); ok {
			hostIP = ip
		} else {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = "sandbox not found"
			return nil
		}
		if config.GetConfig().Common.MockUpdateAction {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_Success)
			rsp.Ret.RetMsg = "mock update action success"
			return nil
		}

		switch req.Action {
		case "pause":
			*rsp = *pauseSandbox(ctx, req, hostIP)
		case "resume":
			*rsp = *resumeFromPauseSnapshot(ctx, req, hostIP)
		}
		return nil
	})
	if err != nil {
		applyLifecycleLockError(rsp, err)
	}
	return
}

func applyLifecycleLockError(rsp *types.Res, err error) {
	if rsp == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, sandboxlock.ErrLockNotAcquired):
		rsp.Ret.RetCode = int(errorcode.ErrorCode_Conflict)
		rsp.Ret.RetMsg = "sandbox lifecycle operation in progress; retry later"
	case errors.Is(err, sandboxlock.ErrRedisUnavailable):
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = err.Error()
	default:
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = err.Error()
	}
}

// UpdateNetwork implements POST /cube/sandbox/network: it replaces a running
// sandbox's egress policy in place.
//
// Held under the same per-sandbox lifecycle lock as pause/resume/delete, so a
// policy update can neither interleave with a pause that is packaging the
// sandbox nor with a delete that is tearing it down.
//
// The node is updated before sandboxspec because sandboxspec is what a later
// clone or resume replays: recording a policy the node rejected would hand that
// policy to every descendant of this sandbox. A failed spec write is logged and
// not surfaced — the update itself did take effect — but it does mean a clone
// taken before the next successful write inherits the older policy.
func UpdateNetwork(ctx context.Context, req *types.UpdateNetworkRequest) (rsp *types.UpdateNetworkRes) {
	rsp = &types.UpdateNetworkRes{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	if req.SandboxID == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "should provide SandboxID"
		return
	}
	if req.CubeNetworkConfig == nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "should provide cube_network_config"
		return
	}
	if err := validateCubeNetworkConfig(req.CubeNetworkConfig); err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	if r := normalizeSandboxIDInReq(ctx, &req.SandboxID); r != nil {
		rsp.Ret = r
		rsp.SandboxID = req.SandboxID
		return
	}
	rsp.SandboxID = req.SandboxID

	lockErr := sandboxlock.WithLock(ctx, req.SandboxID, sandboxlock.Options{
		Value: "network",
		TTL:   sandboxlock.LifecycleTTL,
	}, func(ctx context.Context) error {
		// A client disconnect must not abandon the update between the node and
		// sandboxspec, which would leave the two disagreeing until the next write.
		ctx = context.WithoutCancel(ctx)
		hostIP, ok := resolveSandboxHostIP(ctx, req.SandboxID)
		if !ok {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = "sandbox not found"
			return nil
		}
		*rsp.Ret = *updateSandboxNetworkOnNode(ctx, req, hostIP)
		return nil
	})
	if lockErr != nil {
		applyLifecycleLockError(&types.Res{Ret: rsp.Ret}, lockErr)
	}
	return
}

// updateSandboxNetworkOnNode pushes the policy to Cubelet and, once the node has
// accepted it, records it as the sandbox's canonical spec.
func updateSandboxNetworkOnNode(ctx context.Context, req *types.UpdateNetworkRequest, hostIP string) *types.Ret {
	cubeletReq := &cubebox.UpdateCubeSandboxRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Annotations: map[string]string{
			constants.CubeAnnotationsUpdateAction: "network",
			constants.CubeAnnotationsInsType:      req.InstanceType,
		},
		CubeNetworkConfig: mapCubeNetworkConfig(req.CubeNetworkConfig),
	}
	cubeRsp, err := cubelet.Update(ctx, cubelet.GetCubeletAddr(hostIP), cubeletReq)
	if err != nil || cubeRsp.GetRet() == nil {
		msg := "cubelet update network response is nil"
		if err != nil {
			msg = err.Error()
		}
		return &types.Ret{RetCode: int(errorcode.ErrorCode_ReqCubeAPIFailed), RetMsg: msg}
	}
	result := &types.Ret{
		RetCode: int(cubeRsp.GetRet().GetRetCode()),
		RetMsg:  cubeRsp.GetRet().GetRetMsg(),
	}
	if result.RetCode != int(errorcode.ErrorCode_Success) {
		return result
	}
	persistSandboxNetworkSpec(ctx, req, hostIP)
	return result
}

// persistSandboxNetworkSpec rewrites the stored create spec so clone, snapshot
// and resume replay the new policy instead of the one the sandbox was born
// with. Best-effort by design: the node already enforces the new policy, and
// sandboxspec is recovery-friendly.
func persistSandboxNetworkSpec(ctx context.Context, req *types.UpdateNetworkRequest, hostIP string) {
	spec, err := sandboxspec.Get(ctx, req.SandboxID)
	if err != nil || spec == nil {
		log.G(ctx).Warnf("update network: load sandbox spec failed, clone will replay the old policy: sandbox=%s err=%v",
			req.SandboxID, err)
		return
	}
	spec.CubeNetworkConfig = req.CubeNetworkConfig.DeepCopy()
	opts := sandboxspec.PutOptions{HostIP: hostIP}
	if n, ok := localcache.GetNodesByIp(hostIP); ok {
		opts.HostID = n.ID()
	}
	if err := sandboxspec.Put(ctx, req.SandboxID, spec, opts); err != nil {
		log.G(ctx).Errorf("update network: persist sandbox spec failed, clone will replay the old policy: sandbox=%s err=%v",
			req.SandboxID, err)
	}
}

// resolveSandboxHostIP finds the node currently hosting the sandbox, trying the
// same sources as the pause/resume path.
func resolveSandboxHostIP(ctx context.Context, sandboxID string) (string, bool) {
	if v := localcache.GetSandboxCache(sandboxID); v != nil {
		return v.HostIP, true
	}
	if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok {
		return proxyMap.HostIP, true
	}
	return resolvePauseHostIP(ctx, sandboxID)
}
