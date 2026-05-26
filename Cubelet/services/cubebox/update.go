// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (s *service) Update(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	rt := &CubeLog.RequestTrace{
		Action:       "Update",
		RequestID:    req.RequestID,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "Update",
		InstanceID:   req.SandboxID,
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	log.G(ctx).Errorf("Update:%s", utils.InterfaceToString(req))
	start := time.Now()
	defer func() {
		if !ret.IsSuccessCode(rsp.Ret.RetCode) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("Update fail:%+v", rsp)
		}
		rt.Cost = time.Since(start)
		rt.RetCode = int64(rsp.Ret.RetCode)
		CubeLog.Trace(rt)
	}()

	if req.SandboxID == "" {
		rsp.Ret.RetMsg = "must provide container name"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	if req.Annotations == nil {
		rsp.Ret.RetMsg = "must provide Annotations"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	action := req.Annotations[constants.MasterAnnotationsUpdateAction]
	if action == "" {
		rsp.Ret.RetMsg = "must provide update action"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	rt.CalleeAction = action

	unlock := s.updateSandboxLocks.Lock(req.SandboxID)
	defer unlock()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Update panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = fmt.Sprintf("Update panic info:%s", panicError)
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.SandboxID)
	if err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	rt.CalleeAction = action
	switch action {
	case constants.UpdateActionAddDevice, constants.UpdateActionRemoveDevice:
		rsp.Ret.RetMsg = "cloud disk hotplug is not supported in the open source build"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	case constants.UpdateActionPause:
		return s.UpdateWithPause(ctx, req, sb)
	case constants.UpdateActionResume:
		return s.UpdateWithResume(ctx, req, sb)
	default:
		rsp.Ret.RetMsg = "invalid update action"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
}

func addPauseResumeMetaData(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest) context.Context {
	md, ok := ttrpc.GetMetadata(ctx)
	if !ok {
		md = ttrpc.MD{}
	}
	md.Append("pod_scope", req.SandboxID)
	ctx = ttrpc.WithMetadata(ctx, md)
	tmpmd, _ := ttrpc.GetMetadata(ctx)
	log.G(ctx).Debugf("metadata:%+v", tmpmd)
	return ctx
}

func (s *service) UpdateWithPause(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest, sb *cubeboxstore.CubeBox) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if sb.GetStatus().IsPaused() {
		rsp.Ret.RetMsg = "sandbox is already paused"
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
		return rsp, nil
	}
	if sb.GetStatus().IsTerminated() {
		// IsTerminated() covers both EXITED (FinishedAt!=0) and UNKNOWN
		// (Unknown=true). The legacy "sandbox is terminating" wording wrongly
		// implied a user-driven delete is in flight; use the same wording as
		// rollback.go's precheck so operators can tell the two states apart
		// from the message alone.
		rsp.Ret.RetMsg = "sandbox is not running"
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskStateInvalid
		return rsp, nil
	}

	ns := sb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = constants.WithPreStopType(ctx, constants.PreStopTypePause)
	task, err := sb.FirstContainer().Container.Task(ctx, nil)
	if err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskPauseFailed
		return rsp, nil
	}
	log.G(ctx).Infof("UpdateWithPause:%s", utils.InterfaceToString(req))
	ctx = addPauseResumeMetaData(ctx, req)
	defer func() {

		s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)
	}()
	defer utils.Recover()
	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausingAt = time.Now().UnixNano()
				return status, nil
			})
		}
	}

	for _, c := range sb.All() {
		doPreStop(ctx, c)
	}

	doPreStop(ctx, sb.FirstContainer())

	if err := task.Pause(ctx); err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskPauseFailed

		return rsp, nil
	}
	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausedAt = time.Now().UnixNano()
				status.PausingAt = 0
				return status, nil
			})
		}
	}
	return rsp, nil
}

func (s *service) UpdateWithResume(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest, sb *cubeboxstore.CubeBox) (*cubebox.UpdateCubeSandboxResponse, error) {
	rsp := &cubebox.UpdateCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if !sb.GetStatus().IsPaused() {
		rsp.Ret.RetMsg = "sandbox is not paused"
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskResumeFailed
		return rsp, nil
	}

	ns := sb.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	task, err := sb.FirstContainer().Container.Task(ctx, nil)
	if err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskResumeFailed
		return rsp, nil
	}
	log.G(ctx).Infof("UpdateWithResume:%s", utils.InterfaceToString(req))
	ctx = addPauseResumeMetaData(ctx, req)

	// 保证无论是否 panic，状态都会落盘
	defer func() {
		s.cubeboxMgr.cubeboxManger.SyncByID(ctx, sb.ID)
	}()
	defer utils.Recover()

	if err := task.Resume(ctx); err != nil {
		rsp.Ret.RetMsg = err.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_TaskResumeFailed
		return rsp, nil
	}
	for _, c := range sb.AllContainers() {
		if c.Status != nil {
			c.Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
				status.PausedAt = 0
				status.PausingAt = 0
				return status, nil
			})
		}
	}
	return rsp, nil
}
