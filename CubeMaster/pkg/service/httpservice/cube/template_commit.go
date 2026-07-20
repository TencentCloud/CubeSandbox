// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type commitTemplateRequest struct {
	RequestID     string                      `json:"requestID,omitempty"`
	SandboxID     string                      `json:"sandbox_id,omitempty"`
	TemplateID    string                      `json:"template_id,omitempty"`
	CreateRequest *types.CreateCubeSandboxReq `json:"create_request,omitempty"`
}

type commitTemplateResponse struct {
	*types.Res
	TemplateID string `json:"template_id,omitempty"`
	BuildID    string `json:"build_id,omitempty"`
}

type templateBuildStatusResponse struct {
	*types.Res
	BuildID      string `json:"build_id,omitempty"`
	TemplateID   string `json:"template_id,omitempty"`
	AttemptNo    int    `json:"attempt_no,omitempty"`
	RetryOfJobID string `json:"retry_of_job_id,omitempty"`
	Status       string `json:"status,omitempty"`
	Progress     int    `json:"progress,omitempty"`
	Message      string `json:"message,omitempty"`
}

func handleSandboxCommitAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &commitTemplateRequest{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		common.WriteAPI(c, &commitTemplateResponse{
			Res: &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_MasterParamsError), RetMsg: err.Error()}},
		})
		return
	}
	if strings.TrimSpace(req.SandboxID) == "" || req.CreateRequest == nil {
		common.WriteAPI(c, &commitTemplateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "sandbox_id and create_request are required",
			}},
		})
		return
	}
	if resolved, ret := sandbox.NormalizeSandboxIDParam(c.Request.Context(), req.SandboxID); ret != nil {
		common.WriteAPI(c, &commitTemplateResponse{Res: &types.Res{Ret: ret}})
		return
	} else {
		req.SandboxID = resolved
	}
	// Auto-generate the template ID before any later response can reference it.
	// Users are not allowed to set custom template IDs because the snapshot
	// system depends on the tpl- / snap- prefix convention.
	req.TemplateID = templatecenter.GenerateTemplateID()
	if strings.TrimSpace(req.RequestID) == "" {
		common.WriteAPI(c, &commitTemplateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "requestID is required for commit; retry should generate a new request id",
			}},
			TemplateID: req.TemplateID,
		})
		return
	}
	if req.CreateRequest.Request == nil {
		req.CreateRequest.Request = &types.Request{RequestID: req.RequestID}
	}
	if req.CreateRequest.RequestID == "" {
		req.CreateRequest.RequestID = req.RequestID
	}
	if req.CreateRequest.Annotations == nil {
		req.CreateRequest.Annotations = map[string]string{}
	}
	req.CreateRequest.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = req.TemplateID

	hostIP := ""
	if cache := localcache.GetSandboxCache(req.SandboxID); cache != nil {
		hostIP = cache.HostIP
	}
	hostID := ""
	if hostIP == "" {
		infoRsp := sandbox.SandboxInfo(c.Request.Context(), &types.GetCubeSandboxReq{
			RequestID: req.RequestID,
			SandboxID: req.SandboxID,
		})
		if infoRsp == nil || infoRsp.Ret == nil || infoRsp.Ret.RetCode != int(errorcode.ErrorCode_Success) || len(infoRsp.Data) == 0 {
			msg := "sandbox not found"
			if infoRsp != nil && infoRsp.Ret != nil && infoRsp.Ret.RetMsg != "" {
				msg = infoRsp.Ret.RetMsg
			}
			common.WriteAPI(c, &commitTemplateResponse{
				Res:        &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_NotFound), RetMsg: msg}},
				TemplateID: req.TemplateID,
			})
			return
		}
		hostIP = infoRsp.Data[0].HostIP
		hostID = infoRsp.Data[0].HostID
	}
	if hostID == "" && hostIP != "" {
		if n, ok := localcache.GetNodesByIp(hostIP); ok && n != nil {
			hostID = n.ID()
		}
	}
	if hostIP == "" || hostID == "" {
		common.WriteAPI(c, &commitTemplateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_NotFound),
				RetMsg:  "unable to resolve sandbox host",
			}},
			TemplateID: req.TemplateID,
		})
		return
	}

	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId":   req.RequestID,
		"Action":      "CommitTemplate",
		"TemplateID":  req.TemplateID,
		"SandboxID":   req.SandboxID,
		"SandboxHost": hostIP,
	}))
	job, err := templatecenter.SubmitTemplateCommit(ctx, req.SandboxID, hostID, hostIP, req.CreateRequest)
	if err != nil {
		code := commitTemplateErrorCode(err)
		rt.RetCode = int64(code)
		common.WriteAPI(c, &commitTemplateResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret:       &types.Ret{RetCode: code, RetMsg: err.Error()},
			},
			TemplateID: req.TemplateID,
		})
		return
	}
	rt.RequestID = req.RequestID
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteAPI(c, &commitTemplateResponse{
		Res: &types.Res{
			RequestID: req.RequestID,
			Ret:       &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"},
		},
		TemplateID: req.TemplateID,
		BuildID:    job.JobID,
	})
}

func commitTemplateErrorCode(err error) int {
	switch {
	case errors.Is(err, templatecenter.ErrTemplateIDRequired),
		errors.Is(err, templatecenter.ErrDuplicateTemplate),
		errors.Is(err, templatecenter.ErrTemplateAttemptInProgress):
		return int(errorcode.ErrorCode_MasterParamsError)
	case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
		return int(errorcode.ErrorCode_DBError)
	default:
		return int(errorcode.ErrorCode_MasterInternalError)
	}
}

func handleTemplateBuildStatusAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	buildID := strings.TrimSpace(c.Param("build_id"))
	if buildID == "" {
		common.WriteAPI(c, &templateBuildStatusResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "build_id is required",
			}},
		})
		return
	}
	job, err := templatecenter.GetTemplateImageJobInfo(c.Request.Context(), buildID)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized) {
			code = int(errorcode.ErrorCode_DBError)
		}
		common.WriteAPI(c, &templateBuildStatusResponse{
			Res:     &types.Res{Ret: &types.Ret{RetCode: code, RetMsg: err.Error()}},
			BuildID: buildID,
		})
		return
	}
	status := "building"
	switch strings.ToUpper(job.Status) {
	case templatecenter.JobStatusReady:
		status = "ready"
	case templatecenter.JobStatusFailed:
		status = "error"
	}
	message := job.Phase
	if job.ErrorMessage != "" {
		message = job.ErrorMessage
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteAPI(c, &templateBuildStatusResponse{
		Res:          &types.Res{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success), RetMsg: "success"}},
		BuildID:      buildID,
		TemplateID:   job.TemplateID,
		AttemptNo:    int(job.AttemptNo),
		RetryOfJobID: job.RetryOfJobID,
		Status:       strings.ToLower(status),
		Progress:     int(job.Progress),
		Message:      message,
	})
}
