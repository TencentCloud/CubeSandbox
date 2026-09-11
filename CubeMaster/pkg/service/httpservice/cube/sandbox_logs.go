// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeletErrorCode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

const (
	// defaultLogLimit is the default number of log entries to return.
	defaultLogLimit = 200

	// maxLogLimit caps a single request to avoid large responses.
	maxLogLimit = 1000
)

// SandboxLogsReq is the request body for POST /cube/sandbox/logs.
type SandboxLogsReq struct {
	types.Request
	SandboxID    string `json:"sandboxID"`
	InstanceType string `json:"instanceType,omitempty"`
	// Limit is the maximum number of newest entries to return (default 200, max 1000).
	Limit int `json:"limit,omitempty"`
}

// SandboxLogEntry is one log entry in the response.
type SandboxLogEntry struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Level     string `json:"level"`
}

// SandboxLogsRes is the response for POST /cube/sandbox/logs.
type SandboxLogsRes struct {
	*types.Res
	Logs []SandboxLogEntry `json:"logs"`
}

func handleSandboxLogsAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &SandboxLogsReq{}
	if err := utils.DecodeHttpBody(c.Request.Body, req); err != nil {
		// Also support query params for GET-style calls.
		req.SandboxID = c.Query("sandbox_id")
		if req.SandboxID == "" {
			req.SandboxID = c.Query("sandboxID")
		}
		if l := c.Query("limit"); l != "" {
			req.Limit, _ = strconv.Atoi(l)
		}
	}

	if req.SandboxID == "" {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteAPI(c, &SandboxLogsRes{
			Res: &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  "sandboxID is required",
				},
			},
		})
		return
	}
	if resolved, ret := sandbox.NormalizeSandboxIDParam(c.Request.Context(), req.SandboxID); ret != nil {
		rt.RetCode = int64(ret.RetCode)
		common.WriteAPI(c, &SandboxLogsRes{Res: &types.Res{Ret: ret}})
		return
	} else {
		req.SandboxID = resolved
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}

	// Resolve the Cubelet endpoint that owns this sandbox. The shim req log
	// file lives on the compute node, not on the CubeMaster pod, so we must
	// proxy the read to Cubelet via gRPC.
	hostIP := resolveSandboxHostIP(c, req.SandboxID)
	if hostIP == "" {
		// The sandbox was confirmed to exist above (NormalizeSandboxIDParam), so a
		// missing host here is transient — retry instead of treating it as 404.
		log.G(c.Request.Context()).Warnf("GetSandboxEvents: cannot resolve host for sandboxID=%s", req.SandboxID)
		rt.RetCode = int64(errorcode.ErrorCode_ConnHostFailed)
		common.WriteAPI(c, &SandboxLogsRes{Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_ConnHostFailed),
			RetMsg:  "cannot resolve node for sandbox",
		}}})
		return
	}
	calleeEp := cubelet.GetCubeletAddr(hostIP)

	cmReq := &cubebox.GetSandboxEventsRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Limit:     int32(limit),
	}
	rsp, err := cubelet.GetSandboxEvents(c.Request.Context(), calleeEp, cmReq)
	if err != nil {
		log.G(c.Request.Context()).Errorf("GetSandboxEvents sandboxID=%s ep=%s err=%v", req.SandboxID, calleeEp, err)
		rt.RetCode = int64(errorcode.ErrorCode_MasterInternalError)
		common.WriteAPI(c, &SandboxLogsRes{
			Res: &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterInternalError),
					RetMsg:  "failed to fetch sandbox events from cubelet",
				},
			},
		})
		return
	}

	// Map Cubelet failures to safe public messages. The node-side response may
	// contain paths or transport details that must stay in internal logs.
	retCode := int32(0)
	if rsp.GetRet() != nil {
		retCode = int32(rsp.GetRet().GetRetCode())
	}
	if retCode != 0 && retCode != 200 {
		publicCode, publicMessage := sandboxEventsPublicError(retCode)
		rt.RetCode = int64(publicCode)
		common.WriteAPI(c, &SandboxLogsRes{Res: &types.Res{Ret: &types.Ret{
			RetCode: int(publicCode),
			RetMsg:  publicMessage,
		}}})
		return
	}

	entries := make([]SandboxLogEntry, 0, len(rsp.GetEvents()))
	for _, e := range rsp.GetEvents() {
		entries = append(entries, SandboxLogEntry{
			Timestamp: e.GetTimestamp(),
			Message:   e.GetMessage(),
			Level:     e.GetLevel(),
		})
	}

	rt.RetCode = 0
	common.WriteAPI(c, &SandboxLogsRes{
		Res: &types.Res{
			Ret: &types.Ret{RetCode: 0, RetMsg: ""},
		},
		Logs: entries,
	})
}

func sandboxEventsPublicError(retCode int32) (errorcode.ErrorCode, string) {
	switch cubeletErrorCode.ErrorCode(retCode) {
	case cubeletErrorCode.ErrorCode_InvalidParamFormat:
		return errorcode.ErrorCode_MasterParamsError, "invalid sandbox event request"
	default:
		return errorcode.ErrorCode_MasterInternalError, "failed to read sandbox events from cubelet"
	}
}

// resolveSandboxHostIP returns the node host IP, skipping entries with an empty HostIP.
func resolveSandboxHostIP(c *gin.Context, sandboxID string) string {
	ctx := c.Request.Context()
	if v := localcache.GetSandboxCache(sandboxID); v != nil && v.HostIP != "" {
		return v.HostIP
	}
	if proxyMap, ok := localcache.GetSandboxProxyMap(ctx, sandboxID); ok && proxyMap.HostIP != "" {
		return proxyMap.HostIP
	}
	if rec, err := pausesnap.GetBySandbox(ctx, sandboxID); err == nil && rec != nil && rec.NodeIP != "" {
		return rec.NodeIP
	}
	return ""
}
