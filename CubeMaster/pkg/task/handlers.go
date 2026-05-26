// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package task

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	imagesv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/retry"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapconcurrent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/instancecache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type taskHandler interface {
	HandleTask(ctx context.Context, t *Task) error
	SetMaxRetry(n int64)
	MaxRetry() int64
	SetLoopMaxRetry(n int64)
	LoopMaxRetry() int64

	SetLimiter(n int64)

	Acquire(ctx context.Context) error
	Release()
}

func (l *localTask) reportMetric() {
	var asyncTaskLenMax int64 = math.MinInt64
	recov.GoWithRecover(func() {
		ticker := time.NewTicker(config.GetConfig().Common.CollectMetricInterval)
		defer ticker.Stop()
		for range ticker.C {
			select {
			case <-l.stop:
				return
			case <-l.ctx.Done():
				return
			default:
			}

			recov.WithRecover(func() {
				v := int64(l.asyncTask.Len())
				if v > atomic.LoadInt64(&asyncTaskLenMax) {
					atomic.StoreInt64(&asyncTaskLenMax, v)
				}
			}, func(panicError interface{}) {
				CubeLog.WithContext(context.Background()).Fatalf("reportMetric panic:%v", panicError)
			})
		}
	}, func(panicError interface{}) {
		CubeLog.WithContext(context.Background()).Fatalf("reportMetric panic:%v", panicError)
	})

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	metricTrace := &CubeLog.RequestTrace{
		Caller: constants.CubeMasterServiceID,
		Callee: "metric",
	}
	for range ticker.C {
		if r := recover(); r != nil {
			CubeLog.Fatalf("reportMetric:%v", fmt.Errorf("panic: %v", r))
		}
		select {
		case <-l.ctx.Done():
			return
		case <-l.stop:
			return
		default:
		}

		if v := atomic.SwapInt64(&asyncTaskLenMax, 0); v > 0 {
			metricTrace.Action = "AsyncTask"
			metricTrace.RetCode = v
			CubeLog.Trace(metricTrace)
		}
	}
}

func (l *localTask) backoffRetryDelay(t *Task) {
	if t.delay == 0 {
		t.delay = l.initialDelay
	} else {
		maxDelay := time.Duration(config.GetConfig().CubeletConf.MaxDelayInSecond) * time.Second
		t.delay = time.Duration(float64(t.delay) * (1 + 0.8*rand.Float64()))
		if t.delay > maxDelay {
			t.delay = maxDelay
		}
	}
	if t.delay > 0 {
		time.Sleep(t.delay)
	}
	log.G(t.Ctx).Debugf("task_backoff_retry_delay: %v,loopRetry:%v,retries:%d", t.delay, t.loopRetry, t.retryTimes)
}

func (l *localTask) acquireTask(t *Task) error {
	handler, ok := l.handles[t.TaskType]
	if !ok {
		return errors.New("task type not found")
	}
	if err := handler.Acquire(t.Ctx); err != nil {

		l.backoffRetryDelay(t)
		AddAsyncTask(t)
	}
	return nil
}

func (l *localTask) workHandler(data interface{}) (err error) {
	t, ok := data.(*Task)
	if !ok {
		return errors.New("task data type error")
	}
	t.start = time.Now()
	if err := l.acquireTask(t); err != nil {
		return err
	}

	recov.GoWithRecover(func() {
		defer l.handles[t.TaskType].Release()
		_ = handleTask(t)
	})
	return nil
}

func reportMetric(t *Task, err error) {

	status, _ := ret.FromError(err)
	rt := CubeLog.GetTraceInfo(t.Ctx).DeepCopy()
	rt.Callee = constants.CubeLet
	rt.Cost = time.Since(t.start)
	rt.RetCode = int64(status.Code())
	rt.InstanceType = t.InsType()
	if rt.RetCode == int64(errorcode.ErrorCode_MasterRateLimitedError) ||
		rt.RetCode == int64(errorcode.ErrorCode_ConnHostFailed) {

		rt.RetCode = int64(errorcode.ErrorCode_ReqCubeAPIFailed)
	}
	rt.CalleeEndpoint = t.CallEp
	rt.CalleeAction = string(t.TaskType)
	CubeLog.Trace(rt)
}

func handleTask(t *Task) error {
	handler, ok := l.handles[t.TaskType]
	if !ok {
		return errors.New("task type not found")
	}

	var err error
	retry := false
	defer func() {
		if !retry {
			reportMetric(t, err)
			switch t.TaskType {
			case DestroySandbox:
			case UpdateSandbox:
				updateSandboxTaskResult(t, err)
			}
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			log.G(t.Ctx).Fatalf("HandleTask:%v,delay:%v,retries:%d,err:%v", t.TaskType, t.delay, t.retryTimes, err.Error())
		}
	}()

	if err = handler.HandleTask(t.Ctx, t); err != nil {
		if status, ok := ret.FromError(err); ok {
			if errorcode.IsRetryCode(status.Code()) {

				t.loopRetry = false
				t.retryTimes++
				if t.retryTimes <= handler.MaxRetry() {
					l.backoffRetryDelay(t)
					AddAsyncTask(t)
					retry = true
				} else {
					log.G(t.Ctx).Errorf("task_backoff_retry_delay: %v,loopRetry:%v,retries:%d,exceed:%v", t.delay, t.loopRetry,
						t.retryTimes, err.Error())
				}
				return err
			}

			if errorcode.IsLoopRetryCode(status.Code()) {

				t.loopRetry = true
				t.retryTimes++
				if t.retryTimes <= handler.LoopMaxRetry() {
					l.backoffRetryDelay(t)
					AddAsyncTask(t)
					retry = true
				} else {
					log.G(t.Ctx).Errorf("task_backoff_retry_delay: %v,loopRetry:%v,retries:%d,exceed:%v", t.delay, t.loopRetry,
						t.retryTimes, err.Error())
				}
				return err
			}
		}
	}
	return nil
}

type DestroySandboxTaskHandler struct {
	wrapconcurrent.ConcurrentHandle
}

func (h *DestroySandboxTaskHandler) HandleTask(ctx context.Context, t *Task) error {
	req, ok := t.Request.(*cubebox.DestroyCubeSandboxRequest)
	if !ok {
		return nil
	}

	hostIP := strings.Split(t.CallEp, ":")[0]
	_, ok = localcache.GetNodesByIp(hostIP)
	if ok {

		rsp, err := cubelet.Destroy(ctx, t.CallEp, req)
		defer func() {
			if log.IsDebug() {
				log.G(ctx).Debugf("Destroy_rsp:%+v", utils.InterfaceToString(rsp))
			}
		}()

		if err != nil {
			log.G(ctx).Errorf("Destroy fail:%+v", err)

			return ret.Errorf(errorcode.ErrorCode_MasterRateLimitedError, "%s", err.Error())
		}
		if rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_Success &&
			rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_OK {
			log.G(ctx).Errorf("Destroy error:%+v", rsp)
			return ret.Errorf(errorcode.MasterCode(rsp.GetRet().GetRetCode()), "%s", rsp.GetRet().GetRetMsg())
		}
	}

	if t.InsType() == cubebox.InstanceType_cubebox.String() {
		err := localcache.DeleteSandboxProxyMap(ctx, req.GetSandboxID())
		if err != nil {

			log.G(ctx).Errorf("DeleteSandboxProxyMap:%+v", err)
			return ret.Errorf(errorcode.ErrorCode_MasterRateLimitedError, "DeleteSandboxProxyMap failed: %s", err.Error())
		}
		localcache.DeleteSandboxCache(req.GetSandboxID())
		if err := runAfterDestroyTaskSuccessHook(ctx, req.GetSandboxID()); err != nil {
			log.G(ctx).Warnf("release snapshot runtime refs after destroy failed: %v", err)
		}
	}
	return nil
}

type CreateImageTaskHandler struct {
	wrapconcurrent.ConcurrentHandle
}

func (h *CreateImageTaskHandler) HandleTask(ctx context.Context, t *Task) error {
	req, ok := t.Request.(*imagesv1.CreateImageRequest)
	if !ok {
		return nil
	}

	rsp, err := cubelet.CreateImage(ctx, t.CallEp, req)
	defer func() {
		if log.IsDebug() {
			log.G(ctx).Debugf("CreateImage_rsp:%+v", utils.InterfaceToString(rsp))
		}
	}()
	if err != nil {
		log.G(ctx).Errorf("CreateImage fail:%+v", err)

		return ret.Errorf(errorcode.ErrorCode_ConnHostFailed, "%s", err.Error())
	}
	if rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_Success &&
		rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_OK {
		return ret.Errorf(errorcode.MasterCode(rsp.GetRet().GetRetCode()), "%s", rsp.GetRet().GetRetMsg())
	}
	return nil
}

type DeleteImageTaskHandler struct {
	wrapconcurrent.ConcurrentHandle
}

func (h *DeleteImageTaskHandler) HandleTask(ctx context.Context, t *Task) error {
	req, ok := t.Request.(*imagesv1.DestroyImageRequest)
	if !ok {
		return nil
	}

	rsp, err := cubelet.DeleteImage(ctx, t.CallEp, req)
	defer func() {
		if log.IsDebug() {
			log.G(ctx).Debugf("DeleteImage_rsp:%+v", utils.InterfaceToString(rsp))
		}
	}()
	if err != nil {
		log.G(ctx).Errorf("DeleteImage fail:%+v", err)

		return ret.Errorf(errorcode.ErrorCode_ConnHostFailed, "%s", err.Error())
	}

	if rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_Success &&
		rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_OK {
		return ret.Errorf(errorcode.MasterCode(rsp.GetRet().GetRetCode()), "%s", rsp.GetRet().GetRetMsg())
	}
	return nil
}

type UpdateSandboxTaskHandler struct {
	wrapconcurrent.ConcurrentHandle
}

func (h *UpdateSandboxTaskHandler) HandleTask(ctx context.Context, t *Task) error {
	req, ok := t.Request.(*cubebox.UpdateCubeSandboxRequest)
	if !ok {
		return nil
	}

	ins, err := instancecache.GetInstandesByInsID(ctx, t.InstanceID)
	if err != nil {
		if err == instancecache.ErrorKeyNotFound {
			return ret.Errorf(errorcode.ErrorCode_NotFound, "%s", err.Error())
		}
		return ret.Errorf(errorcode.ErrorCode_ConnHostFailed, "%s", err.Error())
	}

	action := req.GetAnnotations()[constants.CubeAnnotationsUpdateAction]
	switch action {
	case constants.UpdateActionAddDevice:

		if ins.InstanceState == constants.InstanceStatePending {

			return ret.Errorf(errorcode.ErrorCode_MasterRateLimitedError, "InstanceState is valid:[%s]", ins.InstanceState)
		}
	case constants.UpdateActionRemoveDevice:
	default:
		return ret.Errorf(errorcode.ErrorCode_Unknown, "action is valid:[%s]", action)
	}

	rsp, err := cubelet.Update(ctx, t.CallEp, req)
	defer func() {
		if log.IsDebug() {
			log.G(ctx).Errorf("Update_rsp:%+v", utils.InterfaceToString(rsp))
		}
	}()
	if err != nil {
		log.G(ctx).Errorf("Update fail,retry:%+v", err)

		return ret.Errorf(errorcode.ErrorCode_ConnHostFailed, "%s", err.Error())
	}

	if rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_Success &&
		rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_OK {
		return ret.Errorf(errorcode.MasterCode(rsp.GetRet().GetRetCode()), "%s", rsp.GetRet().GetRetMsg())
	}
	return nil
}

func updateSandboxTaskResult(t *Task, result error) error {
	taskInfo := &basetypes.DescribeTaskMap{
		TaskID:    t.DescribeTaskID,
		Status:    constants.TaskStatusSuccess,
		ErrorCode: 0,
	}

	if result != nil {

		taskInfo.Status = constants.TaskStatusFailed
		taskInfo.ErrorCode = -1
		taskInfo.ErrorMessage = result.Error()
	}
	if err := retry.DoWithBackoff(func() error {
		return localcache.SetDescribeTask(t.Ctx, taskInfo)
	}, nil); err != nil {
		log.G(t.Ctx).Fatalf("updateSandboxTaskResult fail:%s", err)
		return err
	}
	return nil
}
