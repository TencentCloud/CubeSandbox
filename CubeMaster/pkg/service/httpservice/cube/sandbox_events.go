// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxevent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// SandboxEventsReq is the request for GET/POST /cube/sandbox/events.
type SandboxEventsReq struct {
	types.Request
	SandboxID string `json:"sandboxID"`
	// Cursor is the ID of the last event already read; 0 starts at the beginning.
	Cursor uint64 `json:"cursor,omitempty"`
	// Limit is the maximum number of events to return (default 100, max 500).
	Limit int `json:"limit,omitempty"`
}

// SandboxLifecycleEvent is one lifecycle history entry.
type SandboxLifecycleEvent struct {
	ID         uint64 `json:"id"`
	Event      string `json:"event"`
	Timestamp  string `json:"timestamp"`
	TemplateID string `json:"templateID,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// SandboxEventsRes is the response for /cube/sandbox/events.
type SandboxEventsRes struct {
	*types.Res
	Events     []SandboxLifecycleEvent `json:"events"`
	NextCursor uint64                  `json:"nextCursor,omitempty"`
	HasMore    bool                    `json:"hasMore"`
}

// handleSandboxEventsAction lists a sandbox's lifecycle history oldest first.
// The ID is matched as given and not resolved against live sandboxes, so the
// history of a deleted sandbox stays readable. An unknown ID returns an empty
// list.
func handleSandboxEventsAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	setRetCode := func(code int64) {
		if rt != nil {
			rt.RetCode = code
		}
	}
	req := &SandboxEventsReq{}
	if err := utils.DecodeHttpBody(c.Request.Body, req); err != nil {
		req.SandboxID = c.Query("sandbox_id")
		if req.SandboxID == "" {
			req.SandboxID = c.Query("sandboxID")
		}
		if cursor := c.Query("cursor"); cursor != "" {
			req.Cursor, _ = strconv.ParseUint(cursor, 10, 64)
		}
		if l := c.Query("limit"); l != "" {
			req.Limit, _ = strconv.Atoi(l)
		}
	}

	if req.SandboxID == "" {
		setRetCode(int64(errorcode.ErrorCode_MasterParamsError))
		common.WriteAPI(c, &SandboxEventsRes{Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  "sandboxID is required",
		}}})
		return
	}

	rows, hasMore, err := sandboxevent.List(c.Request.Context(), req.SandboxID, req.Cursor, req.Limit)
	if err != nil {
		CubeLog.Errorf("sandboxevent.List sandboxID=%s err=%v", req.SandboxID, err)
		setRetCode(int64(errorcode.ErrorCode_DBError))
		common.WriteAPI(c, &SandboxEventsRes{Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_DBError),
			RetMsg:  "failed to list sandbox events",
		}}})
		return
	}

	events := make([]SandboxLifecycleEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, SandboxLifecycleEvent{
			ID:         row.ID,
			Event:      row.Event,
			Timestamp:  row.CreatedAt.UTC().Format(time.RFC3339Nano),
			TemplateID: row.TemplateID,
			Reason:     row.Reason,
		})
	}
	setRetCode(0)
	res := &SandboxEventsRes{
		Res:     &types.Res{Ret: &types.Ret{RetCode: 0, RetMsg: ""}},
		Events:  events,
		HasMore: hasMore,
	}
	if hasMore {
		res.NextCursor = rows[len(rows)-1].ID
	}
	common.WriteAPI(c, res)
}
