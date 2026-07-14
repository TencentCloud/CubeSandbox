// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"net/http"
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

func handleTemplateFromImageAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	var res interface{}
	switch c.Request.Method {
	case http.MethodPost:
		res = createTemplateFromImage(c.Request, rt)
	case http.MethodGet:
		res = getTemplateFromImage(c.Request, rt)
	default:
		res = &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}
	common.WriteResponse(c.Writer, http.StatusOK, res)
}

func handleRedoTemplateAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	if c.Request.Method != http.MethodPost {
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		})
		return
	}
	req := &types.RedoTemplateFromImageReq{}
	if err := common.GetBodyReq(c.Request, req); err != nil {
		common.WriteResponse(c.Writer, http.StatusOK, &types.CreateTemplateFromImageRes{
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
		common.WriteResponse(c.Writer, http.StatusOK, &types.CreateTemplateFromImageRes{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteResponse(c.Writer, http.StatusOK, &types.CreateTemplateFromImageRes{
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

func handleTemplateArtifactDownloadAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		})
		return
	}
	artifactID := strings.TrimSpace(c.Request.URL.Query().Get("artifact_id"))
	token := strings.TrimSpace(c.Request.URL.Query().Get("token"))
	record, file, err := templatecenter.OpenRootfsArtifact(c.Request.Context(), artifactID, token)
	if err != nil {
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_NotFound),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterInternalError),
				RetMsg:  err.Error(),
			},
		})
		return
	}
	c.Writer.Header().Set("Content-Type", "application/octet-stream")
	c.Writer.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	c.Writer.Header().Set("ETag", record.Ext4SHA256)
	c.Writer.Header().Set("X-Cube-Artifact-Id", record.ArtifactID)
	if c.Request.Method == http.MethodHead {
		rt.RetCode = int64(errorcode.ErrorCode_Success)
		return
	}
	http.ServeContent(c.Writer, c.Request, filepath.Base(record.Ext4Path), stat.ModTime(), file)
	rt.RetCode = int64(errorcode.ErrorCode_Success)
}

func handleRootfsArtifactAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	if c.Request.Method != http.MethodGet {
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		})
		return
	}
	artifactID := strings.TrimSpace(c.Request.URL.Query().Get("artifact_id"))
	if artifactID == "" {
		common.WriteResponse(c.Writer, http.StatusOK, &types.CreateTemplateFromImageRes{
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
		common.WriteResponse(c.Writer, http.StatusOK, &types.CreateTemplateFromImageRes{
			Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			},
		})
		return
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	common.WriteResponse(c.Writer, http.StatusOK, &types.CreateTemplateFromImageRes{
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
