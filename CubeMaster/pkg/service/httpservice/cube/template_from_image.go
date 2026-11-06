// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

var redoTemplateFromImageFn = templatecenter.SubmitRedoTemplateFromImage

func createTemplateFromImageGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, createTemplateFromImage(c.Request, rt))
}

func getTemplateFromImageGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, getTemplateFromImage(c.Request, rt))
}

func handleRedoTemplateAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	req := &types.RedoTemplateFromImageReq{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	rt.RequestID = req.RequestID
	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId":  req.RequestID,
		"Action":     "RedoTemplate",
		"TemplateID": req.TemplateID,
	}))
	// CubeMaster no longer builds templates in-process, including redo full
	// rebuilds. The redo job is persisted here and forwarded to
	// CubeTemplateCenter for the actual build work.
	job, err := redoTemplateFromImageFn(ctx, req, requestBaseURL(c.Request))
	if err != nil {
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	// Redo may be a full rebuild (needs TC) or a redistribution-only (no build).
	// SubmitRedoTemplateFromImage already decided: full-rebuild jobs stay
	// PENDING and must be forwarded to TC; redistribution-only jobs run locally.
	if job != nil && templatecenter.RedoNeedsFullRebuild(c.Request.Context(), job.JobID) {
		go forwardRedoBuildJobToTemplateCenter(job.JobID, requestBaseURL(c.Request))
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteAPI(c, &types.CreateTemplateFromImageRes{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: job,
	})
}

func createTemplateFromImage(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	req, envdPayload, err := parseCreateTemplateFromImageRequest(r)
	if err != nil {
		return &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
	}
	rt.RequestID = req.RequestID
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
		"Action":       "CreateTemplateFromImage",
		"TemplateID":   req.TemplateID,
	}))
	// CubeMaster no longer builds templates in-process. All template builds are
	// forwarded to the standalone CubeTemplateCenter process.
	//
	// Forward the NORMALIZED request (the exact object persisted into the
	// job's request_json snapshot), not the raw client request: TC binds the
	// submitted payload to that snapshot, and the raw request differs from it
	// (client requests carry no generated template_id / defaults), so
	// forwarding `req` here would be rejected as a payload mismatch.
	job, normalizedReq, err := templatecenter.SubmitTemplateFromImageWithoutBuild(ctx, req, requestBaseURL(r))
	if err != nil {
		return &types.CreateTemplateFromImageRes{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
	}
	// Only a job still awaiting a build (PENDING) needs forwarding to TC.
	// SubmitTemplateFromImageWithoutBuild can also return an existing job
	// that is already RUNNING (an identical in-flight request was reused) --
	// forwarding that again would resubmit a job TC already has. TC itself
	// only ever accepts PENDING/RUNNING build jobs it created, so a
	// non-PENDING job forwarded here would 404 and get wrongly marked FAILED.
	if job != nil && job.Status == templatecenter.JobStatusPending {
		go forwardBuildJobToTemplateCenter(job.JobID, normalizedReq, requestBaseURL(r), envdPayload)
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &types.CreateTemplateFromImageRes{
		RequestID: req.RequestID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: job,
	}
}

func getTemplateFromImage(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	jobID := strings.TrimSpace(r.URL.Query().Get("job_id"))
	if jobID == "" {
		return &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "job_id is required",
			},
		}
	}
	job, err := templatecenter.GetTemplateImageJobInfo(r.Context(), jobID)
	if err != nil {
		code := templateImageJobErrorCode(err)
		if rt != nil {
			rt.RetCode = int64(code)
		}
		return &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			},
		}
	}
	if rt != nil {
		rt.RetCode = int64(errorcode.ErrorCode_Success)
	}
	return &types.CreateTemplateFromImageRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: job,
	}
}

// templateImageJobErrorCode maps a build-job lookup error to a ret code.
//
// Shared by every handler that reads a job so they cannot disagree on what an
// absent job means. Anything unrecognised stays MasterInternalError: guessing
// a client-side code for an unknown failure would hide real server faults.
func templateImageJobErrorCode(err error) int {
	switch {
	case err == nil:
		return int(errorcode.ErrorCode_Success)
	case errors.Is(err, templatecenter.ErrTemplateImageJobNotFound):
		// "no such job" is a client-side fact, not a server fault. Returning
		// MasterInternalError here made every probe for a missing job look like
		// CubeMaster had broken.
		return int(errorcode.ErrorCode_NotFound)
	case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
		return int(errorcode.ErrorCode_DBError)
	default:
		return int(errorcode.ErrorCode_MasterInternalError)
	}
}

func handleRootfsArtifactAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	artifactID := strings.TrimSpace(c.Query("artifact_id"))
	if artifactID == "" {
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "artifact_id is required",
			},
		})
		return
	}
	info, err := templatecenter.GetRootfsArtifactInfo(c.Request.Context(), artifactID)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			code = int(errorcode.ErrorCode_NotFound)
		}
		common.WriteAPI(c, &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			},
		})
		return
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteAPI(c, &types.CreateTemplateFromImageRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: &types.TemplateImageJobInfo{
			ArtifactID:     info.ArtifactID,
			ArtifactStatus: info.Status,
			Artifact:       info,
		},
	})
}

// requestBaseURL returns the base URL other components must use to reach this
// CubeMaster.
//
// Prefer CUBE_MASTER_ADDR (or common.master_addr), otherwise fall back to the
// current request's Host. Every candidate uses the same node-facing rule: rewrite
// wildcard/loopback through CUBE_SANDBOX_NODE_IP when possible, otherwise skip it
// so Cubelets are never asked to download from 127.0.0.1 or 0.0.0.0.
func requestBaseURL(r *http.Request) string {
	resolve := func(raw string) string {
		if rewritten := templatecenter.RewriteLoopbackBaseURLWithSharedNodeIP(raw); rewritten != "" {
			return rewritten
		}
		if templatecenter.ExternallyUsableBaseURL(raw) {
			return templatecenter.NormalizeBaseURL(raw)
		}
		return ""
	}

	if addr := strings.TrimSpace(os.Getenv(config.EnvMasterAddr)); addr != "" {
		if resolved := resolve(addr); resolved != "" {
			return resolved
		}
	}
	if cfg := config.GetConfig(); cfg != nil && cfg.Common != nil {
		if addr := strings.TrimSpace(cfg.Common.MasterAddr); addr != "" {
			if resolved := resolve(addr); resolved != "" {
				return resolved
			}
		}
	}
	if r == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if host := strings.TrimSpace(r.Host); host != "" {
		return resolve(scheme + "://" + host)
	}
	return ""
}
