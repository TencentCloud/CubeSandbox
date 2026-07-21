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
	"github.com/tencentcloud/CubeSandbox/cubelog"
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
	req := &types.CreateTemplateFromImageReq{}
	if err := common.GetBodyReq(r, req); err != nil {
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
	job, err := templatecenter.SubmitTemplateFromImage(ctx, req, requestBaseURL(r))
	if err != nil {
		return &types.CreateTemplateFromImageRes{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		}
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
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized) {
			code = int(errorcode.ErrorCode_DBError)
		}
		return &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			},
		}
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &types.CreateTemplateFromImageRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		},
		Job: job,
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
	_, file, _, ok := openTemplateArtifactForDownload(c)
	if !ok {
		return
	}
	file.Close()
	rt.RetCode = int64(errorcode.ErrorCode_Success)
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
