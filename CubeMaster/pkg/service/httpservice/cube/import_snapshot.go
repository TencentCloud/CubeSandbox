// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

type snapshotImportRequest struct {
	RequestID        string                      `json:"request_id,omitempty"`
	LegacyRequestID  string                      `json:"requestID,omitempty"`
	HostID           string                      `json:"host_id,omitempty"`
	HostIP           string                      `json:"host_ip,omitempty"`
	RootfsSourcePath string                      `json:"rootfs_source_path,omitempty"`
	CreateSpec       *types.CreateCubeSandboxReq `json:"create_spec,omitempty"`
}

func importSnapshotGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	extendSnapshotWriteDeadline(c.Writer)
	common.WriteAPI(c, importSnapshot(c.Request, rt))
}

func importSnapshot(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req := &snapshotImportRequest{}
	if err := common.GetBodyReq(r, req); err != nil {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterParamsError), RetMsg: err.Error()}},
		}
	}
	requestID := firstNonEmptyTrimmed(req.RequestID, req.LegacyRequestID)
	if requestID == "" {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "request_id is required",
			}},
		}
	}
	if strings.TrimSpace(req.HostIP) == "" {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "host_ip is required",
			}},
		}
	}
	if strings.TrimSpace(req.RootfsSourcePath) == "" {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "rootfs_source_path is required",
			}},
		}
	}
	if req.CreateSpec == nil {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "create_spec is required",
			}},
		}
	}
	if it := strings.TrimSpace(req.CreateSpec.InstanceType); it != "" && it != cubebox.InstanceType_cubebox.String() {
		return &snapshotResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  fmt.Sprintf("instance type %s does not support snapshot import", it),
			}},
		}
	}

	ctx, cancel := snapshotExecutionContext(r.Context(), map[string]any{
		"RequestId": requestID,
		"Action":    "ImportSnapshot",
		"HostIP":    req.HostIP,
	})
	defer cancel()

	info, err := importSnapshotFn(ctx, requestID, req.HostID, req.HostIP, req.RootfsSourcePath, req.CreateSpec)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &snapshotResponse{
			Res: &types.Res{
				RequestID: requestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
		}
	}
	snapshotInfo, err := getSnapshotInfoFn(ctx, info.TemplateID, false)
	if err != nil {
		code := snapshotErrorCode(err)
		rt.RetCode = int64(code)
		return &snapshotResponse{
			Res: &types.Res{
				RequestID: requestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
		}
	}
	rt.RequestID = requestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &snapshotResponse{
		Res: &types.Res{
			RequestID: requestID,
			Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"},
		},
		Snapshot:  snapshotResourceFromInfo(snapshotInfo),
		Operation: operationResourceFromInfo(snapshotOperationInfoFromJob(info)),
	}
}
