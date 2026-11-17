// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var recoverSnapshotCreateReservationFn = templatecenter.RecoverSnapshotCreateReservation

func recoverSnapshotCreateReservationGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	var req templatecenter.SnapshotCreateRecovery
	code, message := int(errorcode.ErrorCode_Success), "success"
	if err := common.GetBodyReq(c.Request, &req); err != nil {
		code, message = int(errorcode.ErrorCode_MasterParamsError), err.Error()
	} else if err := recoverSnapshotCreateReservationFn(c.Request.Context(), c.Param("snapshot_id"), req); err != nil {
		code, message = snapshotErrorCode(err), err.Error()
	}
	rt.RetCode = int64(code)
	common.WriteAPI(c, &types.Res{RequestID: rt.RequestID, Ret: &types.Ret{RetCode: code, RetMsg: message}})
}
