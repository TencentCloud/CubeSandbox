// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

var redoTemplateFromImageFn = templatecenter.SubmitRedoTemplateFromImage

// getRootfsArtifactForRedirectFn is the seam redirectToS3Artifact uses to look
// up the artifact row. Declared as a variable so tests can stub it without a
// database (mirrors redoTemplateFromImageFn above).
var getRootfsArtifactForRedirectFn = templatecenter.GetRootfsArtifactForRedirect

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
	job, err := templatecenter.SubmitTemplateFromImageWithoutBuild(ctx, req, requestBaseURL(r))
	if err != nil {
		return &types.CreateTemplateFromImageRes{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
	}
	go forwardBuildJobToTemplateCenter(job.JobID, req, requestBaseURL(r), envdPayload)
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

// openTemplateArtifactForDownload resolves, opens, and stats the template
// rootfs artifact identified by the artifact_id/token query params and writes
// the common response headers (Content-Type/Length, ETag, X-Cube-Artifact-Id).
// On error it writes the API error response and returns ok=false. On success
// the caller owns file (must Close).
func openTemplateArtifactForDownload(c *gin.Context) (name string, file *os.File, stat os.FileInfo, ok bool) {
	artifactID := strings.TrimSpace(c.Query("artifact_id"))
	token := strings.TrimSpace(c.Query("token"))
	record, f, err := templatecenter.OpenRootfsArtifact(c.Request.Context(), artifactID, token)
	if err != nil {
		common.WriteAPI(c, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_NotFound),
				RetMsg:  err.Error(),
			},
		})
		return "", nil, nil, false
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		common.WriteAPI(c, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterInternalError),
				RetMsg:  err.Error(),
			},
		})
		return "", nil, nil, false
	}
	c.Writer.Header().Set("Content-Type", "application/octet-stream")
	c.Writer.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	c.Writer.Header().Set("ETag", record.Ext4SHA256)
	c.Writer.Header().Set("X-Cube-Artifact-Id", record.ArtifactID)
	return filepath.Base(record.Ext4Path), f, st, true
}

func downloadTemplateArtifactGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())

	// S3-backed artifacts redirect to the presigned URL; local-disk artifacts
	// (legacy, or S3-disabled builds) are streamed from the store. The
	// artifact row's artifact_url is the discriminator: TC writes it after a
	// successful S3 upload, so its presence means the object lives in S3.
	if redirectToS3Artifact(c) {
		rt.RetCode = int64(errorcode.ErrorCode_Success)
		return
	}

	name, file, stat, ok := openTemplateArtifactForDownload(c)
	if !ok {
		return
	}
	defer file.Close()
	http.ServeContent(c.Writer, c.Request, name, stat.ModTime(), file)
	rt.RetCode = int64(errorcode.ErrorCode_Success)
}

func headTemplateArtifactGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())

	// Same S3-vs-local split as downloadTemplateArtifactGinHandler: a HEAD on
	// an S3-backed artifact redirects so the caller can probe the presigned
	// URL directly.
	if redirectToS3Artifact(c) {
		rt.RetCode = int64(errorcode.ErrorCode_Success)
		return
	}

	_, file, _, ok := openTemplateArtifactForDownload(c)
	if !ok {
		return
	}
	file.Close()
	rt.RetCode = int64(errorcode.ErrorCode_Success)
}

// redirectToS3Artifact issues a 302 to the artifact's presigned S3 URL when
// the artifact row has one. Returns true when a redirect was written (caller
// must not write anything else), false when the artifact is local-disk or the
// row/token is invalid (caller falls through to the local-file path, which
// produces the appropriate error response).
func redirectToS3Artifact(c *gin.Context) bool {
	artifactID := strings.TrimSpace(c.Query("artifact_id"))
	token := strings.TrimSpace(c.Query("token"))
	if artifactID == "" {
		return false
	}
	record, err := getRootfsArtifactForRedirectFn(c.Request.Context(), artifactID, token)
	if err != nil || record == nil {
		return false
	}
	if record.ArtifactURL == "" {
		return false
	}
	c.Writer.Header().Set("X-Cube-Artifact-Id", record.ArtifactID)
	c.Writer.Header().Set("ETag", record.Ext4SHA256)
	c.Redirect(http.StatusFound, record.ArtifactURL)
	return true
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

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
