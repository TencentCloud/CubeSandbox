// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var submitTemplateMigrateFn = templatecenter.SubmitTemplateMigrate
var getTemplateMigrateJobInfoFn = templatecenter.GetTemplateMigrateJobInfo

type migrateTemplateRequest struct {
	RequestID  string `json:"requestID,omitempty"`
	TemplateID string `json:"template_id,omitempty"`
}

type migrateTemplateResponse struct {
	*types.Res
	Job *types.TemplateImageJobInfo `json:"job,omitempty"`
}

func handleTemplateMigrateAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &migrateTemplateRequest{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		common.WriteAPI(c, &migrateTemplateResponse{
			Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterParamsError), RetMsg: err.Error()}},
		})
		return
	}
	if strings.TrimSpace(req.TemplateID) == "" {
		common.WriteAPI(c, &migrateTemplateResponse{
			Res: &types.Res{RequestID: req.RequestID, Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterParamsError), RetMsg: "template_id is required"}},
		})
		return
	}
	if rt != nil {
		rt.RequestID = req.RequestID
	}
	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId":  req.RequestID,
		"Action":     "SubmitTemplateMigrate",
		"TemplateID": req.TemplateID,
	}))
	resolvedTemplateID, err := resolveTemplateIdentifierFn(ctx, req.TemplateID)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, templatecenter.ErrTemplateNotFound) {
			code = int(errorcode.ErrorCode_NotFound)
		}
		if rt != nil {
			rt.RetCode = int64(code)
		}
		common.WriteAPI(c, &migrateTemplateResponse{
			Res: &types.Res{RequestID: req.RequestID, Ret: &types.Ret{RetCode: code, RetMsg: err.Error()}},
		})
		return
	}
	job, err := submitTemplateMigrateFn(ctx, resolvedTemplateID, req.RequestID)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterParamsError)
		switch {
		case errors.Is(err, templatecenter.ErrTemplateNotFound):
			code = int(errorcode.ErrorCode_NotFound)
		case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
			code = int(errorcode.ErrorCode_DBError)
		case errors.Is(err, templatecenter.ErrTemplateNotReady):
			code = int(errorcode.ErrorCode_Conflict)
		}
		if rt != nil {
			rt.RetCode = int64(code)
		}
		common.WriteAPI(c, &migrateTemplateResponse{
			Res: &types.Res{RequestID: req.RequestID, Ret: &types.Ret{RetCode: code, RetMsg: err.Error()}},
		})
		return
	}
	if rt != nil {
		rt.RetCode = int64(errorcode.ErrorCode_Success)
	}
	common.WriteAPI(c, &migrateTemplateResponse{
		Res: &types.Res{RequestID: req.RequestID, Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"}},
		Job: job,
	})
}

func getTemplateMigrateStatusAction(c *gin.Context) {
	jobID := strings.TrimSpace(c.Query("job_id"))
	if jobID == "" {
		common.WriteAPI(c, &migrateTemplateResponse{
			Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterParamsError), RetMsg: "job_id is required"}},
		})
		return
	}
	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"Action": "GetTemplateMigrateStatus",
		"JobID":  jobID,
	}))
	job, err := getTemplateMigrateJobInfoFn(ctx, jobID)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, templatecenter.ErrTemplateImageJobNotFound) {
			code = int(errorcode.ErrorCode_NotFound)
		}
		common.WriteAPI(c, &migrateTemplateResponse{
			Res: &types.Res{Ret: &types.Ret{RetCode: code, RetMsg: err.Error()}},
		})
		return
	}
	common.WriteAPI(c, &migrateTemplateResponse{
		Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"}},
		Job: job,
	})
}
