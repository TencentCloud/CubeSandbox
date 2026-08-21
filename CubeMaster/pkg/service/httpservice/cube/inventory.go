// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// inventoryRes is the inventory endpoint response. Unlike ListCubeSandboxRes,
// the Data field has no omitempty so an empty result serializes as "data":[]
// rather than being omitted. This lets callers distinguish "node has 0
// sandboxes" from "cubelet was unreachable" (which surfaces as a non-success
// ret_code via the fail-closed path in ListSandboxWithFailOnError).
type inventoryRes struct {
	RequestID string                    `json:"requestID,omitempty"`
	Ret       *types.Ret                `json:"ret,omitempty"`
	Size      int                       `json:"size,omitempty"`
	Total     int                       `json:"total,omitempty"`
	Data      []*types.SandboxBriefData `json:"data"`
}

// handleInventoryAction lists sandboxes on a single node for management use.
// Unlike /sandbox/list, cubelet list failures surface as a non-success
// ret_code so callers can fail-closed (e.g. refuse node deletion).
func handleInventoryAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	rsp := &types.ListCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	defer func() {
		rt.RetCode = int64(rsp.Ret.RetCode)
	}()

	hostID := c.Query("host_id")
	if hostID == "" {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = "host_id is required"
		common.WriteListAPI(c, toInventoryRes(rsp))
		return
	}

	req := &types.ListCubeSandboxReq{
		RequestID:    c.Query("requestID"),
		HostID:       hostID,
		InstanceType: cubebox.InstanceType_cubebox.String(),
		StartIdx:     1,
		Size:         1,
	}
	if req.RequestID == "" {
		req.RequestID = rt.RequestID
	}

	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId": req.RequestID,
		"HostID":    req.HostID,
	}))
	rsp = sandbox.ListSandboxWithFailOnError(ctx, req)
	common.WriteListAPI(c, toInventoryRes(rsp))
}

// toInventoryRes maps a ListCubeSandboxRes to inventoryRes, ensuring Data is
// non-nil so it always appears in JSON output as "data":[].
func toInventoryRes(rsp *types.ListCubeSandboxRes) *inventoryRes {
	out := &inventoryRes{
		RequestID: rsp.RequestID,
		Ret:       rsp.Ret,
		Size:      rsp.Size,
		Total:     rsp.Total,
		Data:      rsp.Data,
	}
	if out.Data == nil {
		out.Data = []*types.SandboxBriefData{}
	}
	return out
}
