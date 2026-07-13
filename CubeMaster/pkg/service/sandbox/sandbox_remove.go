// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func DestroySandbox(ctx context.Context, req *types.DeleteCubeSandboxReq) (rsp *types.DeleteCubeSandboxRes) {
	rsp = &types.DeleteCubeSandboxRes{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	destroyReq := &cubebox.DestroyCubeSandboxRequest{
		RequestID:   req.RequestID,
		SandboxID:   req.SandboxID,
		Annotations: req.Annotations,
	}

	if req.Annotations == nil {
		destroyReq.Annotations = make(map[string]string)
		destroyReq.Annotations[constants.CubeAnnotationsInsType] = req.InstanceType
	}
	reason := req.KillReason
	if reason == "" {
		reason = "request"
	}
	destroyReq.Annotations[constants.CubeAnnotationsKillReason] = reason
	collectMemoryOption(req, destroyReq)
	if config.GetConfig().Common.CubeDestroyCheckFilter {

		if req.Filter == nil || req.Filter.LabelSelector == nil {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = "should provide filter label"
			return
		}
		destroyReq.Filter = &cubebox.CubeSandboxFilter{
			LabelSelector: req.Filter.LabelSelector,
			InstanceType:  req.InstanceType,
		}
	}
	t := &task.Task{
		BaseInfo: task.BaseInfo{
			InstanceType: req.InstanceType,
			SandboxID:    req.SandboxID,
		},
		Ctx:       ctx,
		RequestId: req.RequestID,
		TaskType:  task.DestroySandbox,
		Request:   destroyReq,
	}
	log.G(ctx).Warnf("async DestroySandbox:%+v", utils.InterfaceToString(req))
	defer func() {
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			log.G(t.Ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("async DestroySandbox_rsp fail:%+v", utils.InterfaceToString(rsp))
		} else {
			log.G(t.Ctx).Warnf("async DestroySandbox_rsp:%+v", utils.InterfaceToString(rsp))
		}
	}()
	if config.GetConfig().Common.MockCreateDirect {
		return
	}
	if req.SandboxID == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "should provide sandbox id"
		return
	}

	switch req.InstanceType {
	case cubebox.InstanceType_cubebox.String():
		if !dealScfSandbox(ctx, req, t) {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_NotFound)
			rsp.Ret.RetMsg = "no such sandbox"
			return
		}
		if req.Sync {
			if err := callCubelet(ctx, t.CallEp, destroyReq); err != nil {
				setSyncDestroyFailure(rsp, err)
			}
			return
		}
	default:
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		return
	}
	t.Ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{"CalleeEndpoint": t.CallEp}))
	if err := task.AddAsyncTask(t); err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = err.Error()
	}
	return
}

func setSyncDestroyFailure(rsp *types.DeleteCubeSandboxRes, err error) {
	if status, ok := ret.FromError(err); ok && isDeleteAutoResumeBusinessCode(status.Code()) {
		rsp.Ret.RetCode = int(status.Code())
		rsp.Ret.RetMsg = status.Message()
		return
	}

	// Keep the existing sync-delete contract for every other failure, including
	// typed connection errors constructed by the Cubelet client wrapper.
	rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
	rsp.Ret.RetMsg = err.Error()
}

func isDeleteAutoResumeBusinessCode(code errorcode.ErrorCode) bool {
	switch code {
	case errorcode.ErrorCode_Conflict,
		errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskStateInvalid),
		errorcode.MasterCode(cubeleterrorcode.ErrorCode_TaskResumeFailed):
		return true
	default:
		return false
	}
}

func dealScfSandbox(ctx context.Context, req *types.DeleteCubeSandboxReq, t *task.Task) bool {
	var hostIP string
	if v := localcache.GetSandboxCache(req.SandboxID); v != nil {
		hostIP = v.HostIP
	} else {
		proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID)
		if !ok {
			return false
		}
		hostIP = proxyMap.HostIP
		if proxyMap.CreatedAt != "" {
			created, err := strconv.ParseInt(proxyMap.CreatedAt, 10, 64)
			if err == nil && created > 0 {
				rt := CubeLog.GetTraceInfo(ctx)
				rt.InstanceID = req.SandboxID

				tmpRt := rt.DeepCopy()
				tmpRt.RetCode = int64(errorcode.ErrorCode_Success)
				tmpRt.CalleeAction = "lifetime"
				tmpRt.Cost = time.Duration(time.Now().UnixNano()-created) * time.Nanosecond
				go CubeLog.Trace(tmpRt)
			}
		}
	}

	t.CallEp = cubelet.GetCubeletAddr(hostIP)
	return true
}

func callCubelet(ctx context.Context, callEp string, req *cubebox.DestroyCubeSandboxRequest) error {
	hostIP := strings.Split(callEp, ":")[0]
	_, ok := localcache.GetNodesByIp(hostIP)
	if ok {

		rsp, err := cubelet.Destroy(ctx, callEp, req)
		defer func() {
			if log.IsDebug() {
				log.G(ctx).Debugf("Destroy_rsp:%+v", utils.InterfaceToString(rsp))
			}
		}()

		if err != nil {
			log.G(ctx).Errorf("Destroy fail:%+v", err)
			return err
		}
		if rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_Success &&
			rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_OK {
			log.G(ctx).Errorf("Destroy error:%+v", rsp)
			return ret.Err(errorcode.MasterCode(rsp.GetRet().GetRetCode()), rsp.GetRet().GetRetMsg())
		}
	}

	err := localcache.DeleteSandboxProxyMap(ctx, req.GetSandboxID())
	if err != nil {
		log.G(ctx).Errorf("DeleteSandboxProxyMap:%+v", err)
		return ret.Errorf(errorcode.ErrorCode_MasterInternalError, "DeleteSandboxProxyMap failed: %s", err.Error())
	}
	localcache.DeleteSandboxCache(req.GetSandboxID())
	if err := runAfterDestroySandboxSuccessHook(ctx, req.GetSandboxID()); err != nil {
		log.G(ctx).Warnf("afterDestroySandboxSuccess hook failed: %v", err)
	}

	return nil
}
