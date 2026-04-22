// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	proxytypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type createSandboxContext struct {
	selctx           *selctx.SelectorCtx
	directHost       bool
	selectHost       *node.Node
	startTime        time.Time
	endTime          time.Time
	cubeletStartTime time.Time
	cubeletEndTime   time.Time
	startHandleTime  time.Time

	retryCost time.Duration
	ctx       context.Context
	done      chan struct{}
	cancel    context.CancelFunc

	setMasterRspOnce sync.Once
	hasSetRet        bool
	masterRsp        *types.CreateCubeSandboxRes
	cubeletReq       *cubebox.RunCubeSandboxRequest
	cubeletRsp       *cubebox.RunCubeSandboxResponse

	cubeletRspPorts map[string]string

	retryTimes int64

	loopRetry bool
	delay     time.Duration

	reschedule bool
}

func CreateSandbox(ctx context.Context, req *types.CreateCubeSandboxReq) (rsp *types.CreateCubeSandboxRes) {
	rsp = &types.CreateCubeSandboxRes{
		RequestID: req.RequestID,
		ExtInfo:   map[string]string{},
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterInternalError),
			RetMsg:  errorcode.ErrorCode_MasterInternalError.String(),
		},
	}

	startTime := time.Now()
	createCtx := &createSandboxContext{
		ctx:        ctx,
		startTime:  startTime,
		masterRsp:  rsp,
		reschedule: true,
		retryCost:  time.Duration(0),
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("CreateSandbox:%s", safePrintCreateCubeSandboxReq(req))
	} else {
		log.G(ctx).Infof("CreateSandbox:%s", simplePrintCreateCubeSandboxReq(req))
	}

	defer func() {
		createCtx.setProbeInfo()
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			msg := ""
			if !filterErrMsg(rsp.Ret.RetCode) {
				msg = utils.InterfaceToString(rsp)
			}
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("CreateSandbox_rsp fail:%+v", msg)
		} else {
			log.G(ctx).Infof("CreateSandbox_rsp:%s", utils.InterfaceToString(rsp))
		}
	}()

	if config.GetConfig().Common.MockCreateDirect {
		createCtx.setMasterRsp(int(errorcode.ErrorCode_Success), "")
		time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)
		return
	}

	if err := createCtx.newContext(ctx, req); err != nil {
		err, _ := ret.FromError(err)
		createCtx.setMasterRsp(int(err.Code()), err.Message())
		return
	}

	if config.GetConfig().Common.MockCreateDirectHandle {
		createCtx.Handle()
	} else {

		scheduler.AddBufferTask(createCtx, req.InstanceType)
	}
	createCtx.Wait()
	createCtx.endTime = time.Now()

	go createCtx.dealMetric()
	return
}

func (c *createSandboxContext) Wait() {
	defer c.cancel()
	select {
	case <-c.done:
	case <-c.ctx.Done():

		code := int(errorcode.ErrorCode_ReqCubeAPIFailed)
		if errors.Is(c.ctx.Err(), context.Canceled) {
			code = int(errorcode.ErrorCode_ClientCancel)
		}
		c.setMasterRsp(code, fmt.Sprintf("%v", c.ctx.Err()))
	}
}

func (c *createSandboxContext) Handle() {
	c.startHandleTime = time.Now()
	defer func() {
		if r := recover(); r != nil {
			log.G(c.ctx).Fatalf("Handle panic:%+v", string(debug.Stack()))
			c.setMasterRsp(int(errorcode.ErrorCode_ReqCubeAPIFailed), "panic fatal error")
		} else {
			c.setMasterRsp(int(c.cubeletRsp.GetRet().GetRetCode()), c.cubeletRsp.GetRet().GetRetMsg())
		}

		close(c.done)

		c.failover()
	}()

	c.handleCubelet()
}

func (c *createSandboxContext) handleCubelet() {

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		if er := c.schedule(); er != nil {
			err, _ := ret.FromError(er)
			c.setMasterRsp(int(err.Code()), err.Message())
			return
		}
		if c.selectHost.HostIP() == "" {
			c.setMasterRsp(int(errorcode.ErrorCode_DBError), "HostIP is empty")
			return
		}

		if c.callCubelet() {
			c.retryCost += c.cubeletEndTime.Sub(c.cubeletStartTime)
			c.retryTimes++

			if c.cubeletRsp != nil && c.cubeletRsp.GetRet() != nil &&
				errorcode.IsCircuitBreakCode(errorcode.MasterCode(c.cubeletRsp.GetRet().GetRetCode())) {
				c.selctx.AddLastBadNode(c.selectHost)
			}
			continue
		}

		c.dealSuccResult()
		return
	}
}

func (c *createSandboxContext) callCubelet() bool {

	localcache.IncrNodeConcurrent(c.selectHost)
	defer func() {
		if r := recover(); r != nil {
			log.G(c.ctx).Fatalf("Handle panic:%+v", string(debug.Stack()))
			c.setMasterRsp(int(errorcode.ErrorCode_ReqCubeAPIFailed), "panic fatal error")
		}
		localcache.DecrNodeConcurrent(c.selectHost)
		c.cubeletEndTime = time.Now()
	}()

	var err error
	calleeEndpoint := cubelet.GetCubeletAddr(c.selectHost.HostIP())
	c.cubeletStartTime = time.Now()
	c.cubeletRsp, err = cubelet.Create(c.ctx, calleeEndpoint, c.cubeletReq)
	if err != nil {
		return c.errRetry(err)
	}
	return c.errorCodeRetry()
}

func (c *createSandboxContext) dealSuccResult() {
	if c.cubeletRsp.GetRet().GetRetCode() == cubeleterrorcode.ErrorCode_Success {
		c.masterRsp.SandboxID = c.cubeletRsp.GetSandboxID()
		c.masterRsp.SandboxIP = c.cubeletRsp.GetSandboxIP()
		c.masterRsp.HostIP = c.selectHost.HostIP()
		c.masterRsp.HostID = c.selectHost.ID()
		if c.cubeletRsp.GetExtInfo() != nil {
			if v, ok := c.cubeletRsp.GetExtInfo()[constants.CubeExtQueueKey]; ok {
				c.masterRsp.ExtInfo[constants.CubeExtQueueKey] = string(v)
			}
			if v, ok := c.cubeletRsp.GetExtInfo()[constants.CubeExtNumaKey]; ok {
				c.masterRsp.ExtInfo[constants.CubeExtNumaKey] = string(v)
			}
		}
		if config.GetConfig().CubeletConf.EnableExposedPort {
			if c.cubeletRsp.GetPortMappings() != nil {
				c.cubeletRspPorts = make(map[string]string)
				for _, m := range c.cubeletRsp.GetPortMappings() {
					c.cubeletRspPorts[strconv.FormatInt(int64(m.GetContainerPort()), 10)] = strconv.FormatInt(int64(m.GetHostPort()), 10)
				}
			}
			if len(c.cubeletRspPorts) == 0 {
				log.G(c.ctx).Warnf("no port mapping in response")
			}
		}
		if err := c.setProxyToRedis(); err != nil {
			c.setMasterRsp(int(errorcode.ErrorCode_DBError), fmt.Sprintf("setProxyToRedis fail:%s", err))
		}

		c.setMasterRsp(int(c.cubeletRsp.GetRet().GetRetCode()), c.cubeletRsp.GetRet().GetRetMsg())
	}
}

func (c *createSandboxContext) failover() {

	if c.masterRsp.Ret.RetCode != int(errorcode.ErrorCode_Success) && c.masterRsp.SandboxID != "" {
		rt := CubeLog.GetTraceInfo(c.ctx).DeepCopy()
		rt.Action = string(task.DestroySandbox)
		rt.CalleeAction = string(task.DestroySandbox)
		rt.CalleeEndpoint = cubelet.GetCubeletAddr(c.selectHost.HostIP())
		t := &task.Task{
			BaseInfo: task.BaseInfo{
				InstanceType: c.cubeletReq.GetInstanceType(),
				SandboxID:    c.cubeletRsp.GetSandboxID(),
			},
			Ctx:       CubeLog.WithRequestTrace(c.ctx, rt),
			RequestId: c.masterRsp.RequestID,
			Request: &cubebox.DestroyCubeSandboxRequest{
				RequestID: c.cubeletRsp.GetRequestID(),
				SandboxID: c.cubeletRsp.GetSandboxID(),
				Filter: &cubebox.CubeSandboxFilter{
					InstanceType: c.cubeletReq.GetInstanceType(),
				},
			},
			TaskType: task.DestroySandbox,
			CallEp:   rt.CalleeEndpoint,
		}

		if err := task.AddAsyncTask(t); err != nil {
			log.G(c.ctx).Fatalf("failover_DestroySandbox error:%+v", err.Error())
		}
	}
}

func (c *createSandboxContext) errRetry(err error) (retry bool) {
	if err == nil {
		return false
	}

	defer func() {
		c.backoffRetryDelay()
	}()
	c.cubeletRsp = &cubebox.RunCubeSandboxResponse{
		Ret: &cubeleterrorcode.Ret{
			RetCode: cubeleterrorcode.ErrorCode(errorcode.ErrorCode_ReqCubeAPIFailed),
			RetMsg:  err.Error(),
		},
	}
	c.reschedule = true
	log.G(c.ctx).Errorf("retries:%d,reschedule:%v,LoopRetry:%v,host:%s,errRetry:%v", c.retryTimes, c.reschedule,
		c.loopRetry, c.selectHost.HostIP(), err)
	return true
}

func (c *createSandboxContext) errorCodeRetry() (retry bool) {
	if c.cubeletRsp.GetRet().GetRetCode() == cubeleterrorcode.ErrorCode_Success {
		return false
	}
	defer func() {
		if retry {
			log.G(c.ctx).Errorf("retries:%d,reschedule:%v,LoopRetry:%v,errorCodeRetry:%v,host:%s", c.retryTimes,
				c.reschedule, c.loopRetry, utils.InterfaceToString(c.cubeletRsp), c.selectHost.HostIP())
			if errorcode.IsBackoffRetryCode(errorcode.MasterCode(c.cubeletRsp.GetRet().GetRetCode())) {

				c.backoffRetryDelay()
			}
		}
	}()

	if errorcode.IsExcludesRetryCode(errorcode.MasterCode(c.cubeletRsp.GetRet().GetRetCode())) {
		c.loopRetry = false
		c.reschedule = false
		return false
	}

	if errorcode.IsReuseCode(errorcode.MasterCode(c.cubeletRsp.GetRet().GetRetCode())) {
		c.loopRetry = false
		c.reschedule = false
		if c.retryTimes <= l.CreateRetryConf.MaxRetry() {
			return true
		}
	}

	if errorcode.IsLoopRetryCode(errorcode.MasterCode(c.cubeletRsp.GetRet().GetRetCode())) {
		c.loopRetry = true
		c.reschedule = true
		if c.retryTimes <= l.CreateRetryConf.LoopMaxRetry() {
			return true
		}
	}
	return false
}

func (c *createSandboxContext) schedule() (err error) {
	if c.directHost || !c.reschedule {

		return nil
	}

	c.selectHost, err = scheduler.Select(c.selctx)
	if err != nil {
		return err
	}
	if c.selectHost == nil {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, scheduler.ErrNoRes.Error())
	}
	return nil
}

func (c *createSandboxContext) backoffRetryDelay() {
	if c.delay == 0 {
		c.delay = config.GetConfig().CubeletConf.BackoffRetryDelay
	} else {
		maxDelay := time.Duration(config.GetConfig().CubeletConf.MaxDelayInSecond) * time.Second
		c.delay = time.Duration(float64(c.delay) * (1 + 0.8*rand.Float64()))
		if c.delay > maxDelay {
			c.delay = maxDelay
		}
	}

	if c.delay > 0 {
		time.Sleep(c.delay)
	}
}

func (c *createSandboxContext) dealMetric() {
	if c == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.G(c.ctx).Fatalf("dealMetric:%+v", string(debug.Stack()))
		}
	}()

	baseRt := CubeLog.GetTraceInfo(c.ctx).DeepCopy()
	baseRt.Callee = constants.CubeLet
	baseRt.InstanceID = c.masterRsp.SandboxID
	baseRt.Action = "Create"
	baseRt.InstanceType = c.cubeletReq.GetInstanceType()

	if c.selectHost != nil {
		baseRt.CalleeEndpoint = cubelet.GetCubeletAddr(c.selectHost.HostIP())
		baseRt.CalleeCluster = c.selectHost.ClusterLabel
	}
	baseRt.RetCode = int64(c.masterRsp.Ret.RetCode)
	baseRt.ErrorCode = CubeLog.CodeSuccess

	cost := c.endTime.Sub(c.startTime)
	baseRt.CalleeAction = constants.ExtInfoCubeE2E
	baseRt.Cost = cost
	CubeLog.Trace(baseRt)

	baseRt.CalleeAction = constants.CubeMasterInnerId
	baseRt.Cost = cost - c.cubeletEndTime.Sub(c.cubeletStartTime)
	CubeLog.Trace(baseRt)

	if c.retryCost > 5*time.Millisecond {
		baseRt.CalleeAction = constants.CubeMasterInnerRetryID
		baseRt.Cost = c.retryCost
		CubeLog.Trace(baseRt)
	}

	baseRt.CalleeAction = constants.CubeMasterInnerHandleID
	baseRt.Cost = c.endTime.Sub(c.cubeletEndTime)
	CubeLog.Trace(baseRt)

	baseRt.Callee = constants.CubeMasterScheduleId
	baseRt.CalleeAction = constants.ActionBufferHandle
	baseRt.Cost = c.startHandleTime.Sub(c.startTime)
	CubeLog.Trace(baseRt)
}

func (c *createSandboxContext) setMasterRsp(code int, msg string) {
	c.setMasterRspOnce.Do(func() {
		c.hasSetRet = true
		c.masterRsp.Ret.RetCode = code
		c.masterRsp.Ret.RetMsg = msg
	})
}

func (c *createSandboxContext) setProxyToRedis() error {
	if c.hasSetRet &&
		c.masterRsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {

		return nil
	}
	switch c.cubeletReq.GetInstanceType() {
	case cubebox.InstanceType_cubebox.String():
		proxy := &proxytypes.SandboxProxyMap{
			HostIP:      c.selectHost.HostIP(),
			SandboxID:   c.masterRsp.SandboxID,
			SandboxIP:   c.masterRsp.SandboxIP,
			SandboxPort: "8080",
			CreatedAt:   strconv.FormatInt(time.Now().UnixNano(), 10),
		}

		if config.GetConfig().CubeletConf.EnableExposedPort {
			proxy.ContainerToHostPorts = c.cubeletRspPorts
		}

		return localcache.SetSandboxProxyMap(c.ctx, proxy)
	}
	return nil
}

func (c *createSandboxContext) newContext(ctx context.Context, req *types.CreateCubeSandboxReq) error {
	c.selctx = selctx.New(config.GetConfig().Scheduler.LeastSelectName)
	c.selctx.InstanceType = req.InstanceType

	c.constructAffanity(ctx, req)
	c.done = make(chan struct{})
	c.cubeletRsp = &cubebox.RunCubeSandboxResponse{
		Ret: &cubeleterrorcode.Ret{
			RetCode: cubeleterrorcode.ErrorCode_Unknown,
		},
	}

	cubeletReq, err := ConstructCubeletReq(ctx, req)
	if err != nil {
		return err
	}
	c.cubeletReq = cubeletReq

	reqResource, err := checkAndGetReqResource(req)
	if err != nil {
		return err
	}
	c.selctx.ReqRes = reqResource

	c.ctx, c.cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
	c.selctx.Ctx = c.ctx

	switch {
	case req.InsId != "":
		if !isDebug(req.Annotations) {
			return ret.Errorf(errorcode.ErrorCode_MasterParamsError, "invalid debug req:%s", req.InsId)
		}
		tmpResult, exist := localcache.GetNode(req.InsId)
		if !exist {
			return ret.Errorf(errorcode.ErrorCode_SelectNodesFailed, "no such insId:%s", req.InsId)
		}
		c.selectHost = tmpResult
		c.directHost = true
	case req.InsIp != "":
		if !isDebug(req.Annotations) {
			return ret.Errorf(errorcode.ErrorCode_MasterParamsError, "invalid debug req:%s", req.InsIp)
		}
		tmpResult, exist := localcache.GetNodesByIp(req.InsIp)
		if !exist {
			return ret.Errorf(errorcode.ErrorCode_SelectNodesFailed, "no such InsIp:%s", req.InsIp)
		}
		c.selectHost = tmpResult
		c.directHost = true
	default:

	}
	return nil
}
func (c *createSandboxContext) constructAffanity(ctx context.Context, req *types.CreateCubeSandboxReq) {

	if v := constants.GetNodeSelector(ctx); v != nil {
		nl, ok := v.(affinity.NodeSelector)
		if ok {
			c.selctx.Affinity.NodeSelector = nl
		}
	}
	if v := constants.GetBackoffNodeSelector(ctx); v != nil {
		nl, ok := v.(affinity.NodeSelector)
		if ok {
			c.selctx.Affinity.BackoffNodeSelector = nl
		}
	}
	if v := constants.GetPreferredSchedulingTerms(ctx); v != nil {
		np, ok := v.(affinity.PreferredSchedulingTerms)
		if ok {
			c.selctx.Affinity.NodePrefererd = np
		}
	}
}

func (c *createSandboxContext) setProbeInfo() {
	if c.masterRsp.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		if c.cubeletRsp != nil && c.cubeletRsp.GetExtInfo() != nil {
			if c.masterRsp.ExtInfo == nil {
				c.masterRsp.ExtInfo = make(map[string]string)
			}
			c.masterRsp.ExtInfo[constants.ExtInfoCubeE2E] = strconv.FormatInt(time.Since(c.startTime).Milliseconds(), 10)
			var probe int64
			for k, v := range c.cubeletRsp.GetExtInfo() {
				if strings.Contains(k, "-probe") {
					c.masterRsp.ExtInfo[k] = string(v)
					i, err := strconv.ParseInt(string(v), 10, 64)
					if err != nil {
						i = 0
					}
					probe += i
				}
			}
			c.masterRsp.ExtInfo["all-probe"] = strconv.FormatInt(probe, 10)
		}
	}
}

func isDebug(annotation map[string]string) bool {
	if annotation == nil {
		return false
	}
	v, ok := annotation[constants.AnnotationsDebug]
	if !ok {
		return false
	}
	return strings.ToLower(v) == "true"
}
