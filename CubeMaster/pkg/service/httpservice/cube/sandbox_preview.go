// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"

	"github.com/gin-gonic/gin"
	jsoniter "github.com/json-iterator/go"
	api "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

var previewConstructCubeletReqFn = sandbox.ConstructCubeletReq
var previewDealCubeboxCreateReqWithTemplateFn = dealCubeboxCreateReqWithTemplate

type sandboxPreviewResponse struct {
	*types.Res
	APIRequest     *types.CreateCubeSandboxReq `json:"api_request,omitempty"`
	MergedRequest  *types.CreateCubeSandboxReq `json:"merged_request,omitempty"`
	CubeletRequest *api.RunCubeSandboxRequest  `json:"cubelet_request,omitempty"`
}

func handleSandboxPreviewAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, previewSandbox(c.Request, rt))
}

func previewSandbox(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req, err := constructCreateReq(r)
	if err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &sandboxPreviewResponse{
			Res: &types.Res{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  err.Error(),
				},
			},
		}
	}

	rt.RequestID = req.RequestID
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
		"Action":       "PreviewSandbox",
	}))

	apiReq, err := cloneCreateReq(req)
	if err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterInternalError)
		return &sandboxPreviewResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterInternalError),
					RetMsg:  err.Error(),
				},
			},
		}
	}
	mergedReq, err := cloneCreateReq(req)
	if err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterInternalError)
		return &sandboxPreviewResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterInternalError),
					RetMsg:  err.Error(),
				},
			},
		}
	}
	if err = previewDealCubeboxCreateReqWithTemplateFn(ctx, mergedReq); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &sandboxPreviewResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  err.Error(),
				},
			},
			APIRequest: apiReq,
		}
	}

	cubeletReqInput, err := cloneCreateReq(mergedReq)
	if err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterInternalError)
		return &sandboxPreviewResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterInternalError),
					RetMsg:  err.Error(),
				},
			},
			APIRequest:    apiReq,
			MergedRequest: mergedReq,
		}
	}
	cubeletReq, err := previewConstructCubeletReqFn(ctx, cubeletReqInput)
	if err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &sandboxPreviewResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  err.Error(),
				},
			},
			APIRequest:    apiReq,
			MergedRequest: mergedReq,
		}
	}

	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &sandboxPreviewResponse{
		Res: &types.Res{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_Success),
				RetMsg:  "success",
			},
		},
		APIRequest:     apiReq,
		MergedRequest:  mergedReq,
		CubeletRequest: cubeletReq,
	}
}

func cloneCreateReq(req *types.CreateCubeSandboxReq) (*types.CreateCubeSandboxReq, error) {
	payload, err := jsoniter.Marshal(req)
	if err != nil {
		return nil, err
	}
	out := &types.CreateCubeSandboxReq{}
	if err = jsoniter.Unmarshal(payload, out); err != nil {
		return nil, err
	}
	return out, nil
}
