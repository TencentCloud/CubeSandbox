// Copyright (c) 2026 Tencent Inc.
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

// SandboxMetrics resolves the Cubelet that owns the sandbox, requests one
// current metrics snapshot from that Cubelet, and applies optional start/end
// filtering before returning the CubeMaster HTTP response.
func SandboxMetrics(ctx context.Context, req *types.GetSandboxMetricsReq) (rsp *types.GetSandboxMetricsRes) {
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}
	rsp = &types.GetSandboxMetricsRes{
		RequestID: req.RequestID,
		Data:      []*types.SandboxMetricData{},
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	log.G(ctx).Infof("GetSandboxMetrics:%+v", utils.InterfaceToString(req))
	defer func() {
		if log.IsDebug() {
			log.G(ctx).Debugf("GetSandboxMetrics_rsp:%+v", utils.InterfaceToString(rsp))
		} else if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Warnf("GetSandboxMetrics fail:%+v", utils.InterfaceToString(rsp))
		}
	}()

	start := time.Now()
	rt := CubeLog.GetTraceInfo(ctx).DeepCopy()
	rt.Callee = constants.CubeLet
	defer func() {
		rt.Cost = time.Since(start)
		rt.RetCode = int64(rsp.Ret.RetCode)
		rt.CalleeAction = "GetSandboxMetrics"
		CubeLog.Trace(rt)
	}()

	if req.SandboxID == "" {
		setMetricsError(errorcode.ErrorCode_MasterParamsError, rsp, "sandbox_id is required")
		return rsp
	}
	if req.Start < 0 || req.End < 0 {
		setMetricsError(errorcode.ErrorCode_MasterParamsError, rsp, "start/end must be greater than or equal to 0")
		return rsp
	}
	if req.Start > 0 && req.End > 0 && req.Start > req.End {
		setMetricsError(errorcode.ErrorCode_MasterParamsError, rsp, "start must be less than or equal to end")
		return rsp
	}

	var calleeEndpoint string
	if req.HostID != "" {
		n, exist := localcache.GetNode(req.HostID)
		if !metricsNodeReady(n, exist, rsp) {
			return rsp
		}
		calleeEndpoint = cubelet.GetCubeletAddr(n.IP)
	} else {
		var hostIP string
		if v := localcache.GetSandboxCache(req.SandboxID); v != nil {
			hostIP = v.HostIP
		} else if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, req.SandboxID); ok {
			hostIP = proxyMap.HostIP
		} else {
			setMetricsError(errorcode.ErrorCode_NotFound, rsp, "sandbox "+req.SandboxID+" not found")
			return rsp
		}
		n, exist := localcache.GetNodesByIp(hostIP)
		if !metricsNodeReady(n, exist, rsp) {
			return rsp
		}
		calleeEndpoint = cubelet.GetCubeletAddr(n.IP)
	}
	rt.CalleeEndpoint = calleeEndpoint

	cubeRsp, err := cubelet.GetSandboxMetrics(ctx, calleeEndpoint, &cubebox.GetSandboxMetricsRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
	})
	if err != nil {
		setMetricsError(errorcode.ErrorCode_ReqCubeAPIFailed, rsp, err.Error())
		return rsp
	}
	if int(cubeRsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		rsp.Ret.RetCode = int(cubeRsp.GetRet().GetRetCode())
		rsp.Ret.RetMsg = cubeRsp.GetRet().GetRetMsg()
		return rsp
	}
	for _, metric := range cubeRsp.GetMetrics() {
		if !metricInRange(metric.GetTimestampUnixNano(), req.Start, req.End) {
			continue
		}
		rsp.Data = append(rsp.Data, &types.SandboxMetricData{
			TimestampUnixNano: metric.GetTimestampUnixNano(),
			CPUCount:          metric.GetCpuCount(),
			CPUUsedPct:        metric.GetCpuUsedPct(),
			MemUsed:           metric.GetMemUsed(),
			MemTotal:          metric.GetMemTotal(),
			MemCache:          metric.GetMemCache(),
			DiskUsed:          metric.GetDiskUsed(),
			DiskTotal:         metric.GetDiskTotal(),
		})
	}
	return rsp
}

func setMetricsError(code errorcode.ErrorCode, rsp *types.GetSandboxMetricsRes, msg string) {
	rsp.Ret.RetCode = int(code)
	if msg == "" {
		msg = code.String()
	}
	rsp.Ret.RetMsg = msg
}

func metricsNodeReady(n *node.Node, exist bool, rsp *types.GetSandboxMetricsRes) bool {
	if !exist {
		setMetricsError(errorcode.ErrorCode_NotFound, rsp, errorcode.ErrorCode_NotFound.String())
		return false
	}
	if !n.Healthy {
		setMetricsError(errorcode.ErrorCode_CubeletUnHealthy, rsp, errorcode.ErrorCode_CubeletUnHealthy.String())
		return false
	}
	return true
}

func metricInRange(timestampUnixNano, startUnix, endUnix int64) bool {
	timestampUnix := timestampUnixNano / int64(time.Second)
	if startUnix > 0 && timestampUnix < startUnix {
		return false
	}
	if endUnix > 0 && timestampUnix > endUnix {
		return false
	}
	return true
}
