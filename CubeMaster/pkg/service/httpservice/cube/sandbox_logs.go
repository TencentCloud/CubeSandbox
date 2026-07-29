// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeletErrorCode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

const (
	// defaultLogLimit is the internal default; CubeAPI's public v2 default is 1000.
	defaultLogLimit = 200

	// maxLogLimit caps a single request to avoid large responses.
	maxLogLimit = 2000
)

// SandboxLogsReq is the request body for POST /cube/sandbox/logs.
type SandboxLogsReq struct {
	types.Request
	SandboxID    string `json:"sandboxID"`
	InstanceType string `json:"instanceType,omitempty"`
	// Cursor is a Unix-ms timestamp; 0 reads from the beginning.
	Cursor int64 `json:"cursor,omitempty"`
	// Tail returns the newest limit entries instead of paginating.
	Tail bool `json:"tail,omitempty"`
	// Limit is the maximum number of entries to return (default 200, max 2000).
	Limit int `json:"limit,omitempty"`
}

// SandboxLogEntry is one log entry in the response.
type SandboxLogEntry struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Level     string `json:"level"`
	Module    string `json:"module,omitempty"`
}

// SandboxLogsRes is the response for POST /cube/sandbox/logs.
type SandboxLogsRes struct {
	*types.Res
	Logs       []SandboxLogEntry `json:"logs"`
	NextCursor int64             `json:"nextCursor,omitempty"`
	HasMore    bool              `json:"hasMore"`
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
		if cursor := c.Query("cursor"); cursor != "" {
			req.Cursor, _ = strconv.ParseInt(cursor, 10, 64)
		}
		if tail := c.Query("tail"); tail != "" {
			req.Tail, _ = strconv.ParseBool(tail)
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
	if req.Tail && req.Cursor > 0 {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		common.WriteAPI(c, &SandboxLogsRes{
			Res: &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  "tail and cursor are mutually exclusive",
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

	// The shim req log lives on the owning node; proxy the read via Cubelet gRPC.
	hostIP, fromPause := sandbox.ResolveEventHostIP(c.Request.Context(), req.SandboxID)
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
	if fromPause {
		log.G(c.Request.Context()).Warnf("GetSandboxEvents: sandboxID=%s routed via pause snapshot; sandbox may have migrated", req.SandboxID)
	}
	calleeEp := cubelet.GetCubeletAddr(hostIP)

	cmReq := &cubebox.GetSandboxEventsRequest{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Cursor:    req.Cursor,
		Tail:      req.Tail,
		Limit:     int32(limit),
	}
	rsp, err := cubelet.GetSandboxEvents(c.Request.Context(), calleeEp, cmReq)
	if err != nil {
		log.G(c.Request.Context()).Errorf("GetSandboxEvents sandboxID=%s ep=%s err=%v", req.SandboxID, calleeEp, err)
		retCode, retMsg := classifyCubeletEventError(err)
		rt.RetCode = int64(retCode)
		common.WriteAPI(c, &SandboxLogsRes{
			Res: &types.Res{
				Ret: &types.Ret{
					RetCode: int(retCode),
					RetMsg:  retMsg,
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
			Module:    e.GetModule(),
		})
	}

	rt.RetCode = 0
	common.WriteAPI(c, &SandboxLogsRes{
		Res: &types.Res{
			Ret: &types.Ret{RetCode: 0, RetMsg: ""},
		},
		Logs:       entries,
		NextCursor: rsp.GetNextCursor(),
		HasMore:    rsp.GetHasMore(),
	})
}

// classifyCubeletEventError maps a transport failure to a public code/message;
// wrapped ret errors keep their code, raw gRPC codes are translated.
func classifyCubeletEventError(err error) (errorcode.ErrorCode, string) {
	if st, ok := ret.FromError(err); ok && st != nil {
		return st.RetCode, "failed to fetch sandbox events from cubelet"
	}
	switch status.Code(err) {
	case codes.Unimplemented:
		return errorcode.ErrorCode_MasterInternalError,
			"cubelet does not support GetSandboxEvents; upgrade cubelet on the owning node first"
	case codes.Unavailable, codes.DeadlineExceeded:
		return errorcode.ErrorCode_ConnHostFailed, "cubelet unreachable"
	default:
		return errorcode.ErrorCode_MasterInternalError, "failed to fetch sandbox events from cubelet"
	}
}

func sandboxEventsPublicError(retCode int32) (errorcode.ErrorCode, string) {
	switch cubeletErrorCode.ErrorCode(retCode) {
	case cubeletErrorCode.ErrorCode_InvalidParamFormat:
		return errorcode.ErrorCode_MasterParamsError, "invalid sandbox event request"
	default:
		return errorcode.ErrorCode_MasterInternalError, "failed to read sandbox events from cubelet"
	}
}
