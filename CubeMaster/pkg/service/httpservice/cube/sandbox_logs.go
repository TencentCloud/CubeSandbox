// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"bufio"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	// defaultShimLogPath is the path to the CubeShim request log file.
	// Each line is a JSON object with fields: Module, InstanceId, Timestamp, LogContent, FunctionType.
	defaultShimLogPath = "/data/log/CubeShim/cube-shim-req.log"

	// defaultLogLimit is the default number of log entries to return.
	defaultLogLimit = 200

	// maxLogLimit caps a single request to avoid large responses.
	maxLogLimit = 2000
)

// SandboxLogsReq is the request body for POST /cube/sandbox/logs.
type SandboxLogsReq struct {
	types.Request
	SandboxID    string `json:"sandboxID"`
	InstanceType string `json:"instanceType,omitempty"`
	// Cursor is a Unix millisecond timestamp; only return entries after this time.
	Cursor int64 `json:"cursor,omitempty"`
	// Limit is the maximum number of entries to return (default 200, max 2000).
	Limit int `json:"limit,omitempty"`
}

// ShimLogLine is one parsed line from cube-shim-req.log.
type ShimLogLine struct {
	Module       string `json:"Module"`
	InstanceID   string `json:"InstanceId"`
	ContainerID  string `json:"ContainerId"`
	Timestamp    string `json:"Timestamp"`
	LogContent   string `json:"LogContent"`
	FunctionType string `json:"FunctionType"`
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
	Logs       []SandboxLogEntry `json:"logs"`
	NextCursor int64             `json:"nextCursor,omitempty"`
	HasMore    bool              `json:"hasMore"`
}

var fastJSON = jsoniter.ConfigFastest

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

	entries, nextCursor, hasMore, err := readShimLogs(req.SandboxID, req.Cursor, limit)
	if err != nil {
		CubeLog.Errorf("readShimLogs sandboxID=%s err=%v", req.SandboxID, err)
		rt.RetCode = int64(errorcode.ErrorCode_MasterInternalError)
		common.WriteAPI(c, &SandboxLogsRes{
			Res: &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterInternalError),
					RetMsg:  err.Error(),
				},
			},
		})
		return
	}

	rt.RetCode = 0
	common.WriteAPI(c, &SandboxLogsRes{
		Res: &types.Res{
			Ret: &types.Ret{RetCode: 0, RetMsg: ""},
		},
		Logs:       entries,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	})
}

// readShimLogs scans the shim log file and returns entries matching sandboxID.
// cursor is a Unix millisecond timestamp; entries with Timestamp <= cursor are skipped.
// Returns at most limit entries, plus a nextCursor and hasMore flag for pagination.
func readShimLogs(sandboxID string, cursor int64, limit int) ([]SandboxLogEntry, int64, bool, error) {
	f, err := os.Open(defaultShimLogPath)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()

	var entries []SandboxLogEntry
	var nextCursor int64
	hasMore := false

	scanner := bufio.NewScanner(f)
	// Increase buffer for long lines (vmm log lines can be very large).
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry ShimLogLine
		if err := fastJSON.Unmarshal(line, &entry); err != nil {
			continue
		}

		if entry.InstanceID != sandboxID {
			continue
		}

		// Parse timestamp for cursor filtering.
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			continue
		}
		tsMs := ts.UnixMilli()

		// Skip entries at or before the cursor.
		if cursor > 0 && tsMs <= cursor {
			continue
		}

		if len(entries) >= limit {
			// We have more entries beyond the limit.
			hasMore = true
			break
		}

		entries = append(entries, SandboxLogEntry{
			Timestamp: entry.Timestamp,
			Message:   entry.LogContent,
			Level:     "info",
		})
		nextCursor = tsMs
	}

	if err := scanner.Err(); err != nil {
		return entries, nextCursor, hasMore, err
	}

	return entries, nextCursor, hasMore, nil
}
