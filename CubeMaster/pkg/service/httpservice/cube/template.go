// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"net/http"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var deleteTemplateFn = templatecenter.DeleteTemplate
var getTemplateInfoFn = templatecenter.GetTemplateInfo
var getTemplateRequestFn = templatecenter.GetTemplateRequest

type templateResponse struct {
	*types.Res
	TemplateID    string                         `json:"template_id,omitempty"`
	InstanceType  string                         `json:"instance_type,omitempty"`
	Version       string                         `json:"version,omitempty"`
	Status        string                         `json:"status,omitempty"`
	LastError     string                         `json:"last_error,omitempty"`
	Replicas      []templatecenter.ReplicaStatus `json:"replicas,omitempty"`
	CreateRequest *types.CreateCubeSandboxReq    `json:"create_request,omitempty"`
}

type templateListResponse struct {
	*types.Res
	Data []templateSummary `json:"data,omitempty"`
}

type templateSummary struct {
	TemplateID   string `json:"template_id,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	Version      string `json:"version,omitempty"`
	Status       string `json:"status,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	ImageInfo    string `json:"image_info,omitempty"`
}

type deleteTemplateRequest struct {
	RequestID    string `json:"RequestID,omitempty"`
	TemplateID   string `json:"template_id,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	Sync         bool   `json:"sync,omitempty"`
}

func handleTemplateAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	switch r.Method {
	case http.MethodPost:
		return createTemplate(w, r, rt)
	case http.MethodGet:
		return getTemplate(w, r, rt)
	case http.MethodDelete:
		return deleteTemplate(w, r, rt)
	default:
		return &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}
}

func deleteTemplate(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	req := &deleteTemplateRequest{}
	if err := common.GetBodyReq(r, req); err != nil {
		return &templateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			}},
		}
	}
	if req.TemplateID == "" {
		return &templateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "template_id is required",
			}},
		}
	}
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
		"Action":       "DeleteTemplate",
		"TemplateID":   req.TemplateID,
	}))
	err := deleteTemplateFn(ctx, req.TemplateID, req.InstanceType)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		switch {
		case errors.Is(err, templatecenter.ErrTemplateNotFound):
			code = int(errorcode.ErrorCode_NotFound)
		case errors.Is(err, templatecenter.ErrTemplateInUse):
			code = int(errorcode.ErrorCode_MasterParamsError)
		case errors.Is(err, templatecenter.ErrTemplateAttemptInProgress):
			code = int(errorcode.ErrorCode_MasterParamsError)
		case errors.Is(err, templatecenter.ErrTemplateCleanupLocatorMissing):
			code = int(errorcode.ErrorCode_NotFound)
		case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
			code = int(errorcode.ErrorCode_DBError)
		}
		rt.RetCode = int64(code)
		return &templateResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret: &types.Ret{
					RetCode: code,
					RetMsg:  err.Error(),
				},
			},
			TemplateID: req.TemplateID,
		}
	}
	rt.RequestID = req.RequestID
	rt.InstanceType = req.InstanceType
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &templateResponse{
		Res: &types.Res{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_Success),
				RetMsg:  "success",
			},
		},
		TemplateID: req.TemplateID,
	}
}

func createTemplate(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	req, err := constructCreateReq(r)
	if err != nil {
		return &templateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  err.Error(),
			}},
		}
	}
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
		"Action":       "CreateTemplate",
	}))
	info, err := templatecenter.CreateTemplate(ctx, req)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		switch {
		case errors.Is(err, templatecenter.ErrTemplateIDRequired),
			errors.Is(err, templatecenter.ErrDuplicateTemplate),
			errors.Is(err, templatecenter.ErrNoTemplateNodes):
			code = int(errorcode.ErrorCode_MasterParamsError)
		case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
			code = int(errorcode.ErrorCode_DBError)
		}
		rt.RetCode = int64(code)
		return &templateResponse{
			Res: &types.Res{
				RequestID: req.RequestID,
				Ret: &types.Ret{
					RetCode: code,
					RetMsg:  err.Error(),
				},
			},
		}
	}
	rt.RequestID = req.RequestID
	rt.InstanceType = req.InstanceType
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &templateResponse{
		Res: &types.Res{
			RequestID: req.RequestID,
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_Success),
				RetMsg:  "success",
			},
		},
		TemplateID:   info.TemplateID,
		InstanceType: info.InstanceType,
		Version:      info.Version,
		Status:       info.Status,
		LastError:    info.LastError,
		Replicas:     info.Replicas,
	}
}

func getTemplate(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	templateID := r.URL.Query().Get("template_id")
	includeRequest := r.URL.Query().Get("include_request") == "true" || r.URL.Query().Get("include_request") == "1"
	if templateID == "" {
		return listTemplates(r, rt)
	}
	info, err := getTemplateInfoFn(r.Context(), templateID)
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, templatecenter.ErrTemplateNotFound) {
			code = int(errorcode.ErrorCode_NotFound)
		}
		rt.RetCode = int64(code)
		return &templateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			}},
			TemplateID: templateID,
		}
	}
	var createReq *types.CreateCubeSandboxReq
	if includeRequest {
		createReq, err = getTemplateRequestFn(r.Context(), templateID)
		if err != nil {
			code := int(errorcode.ErrorCode_MasterInternalError)
			if errors.Is(err, templatecenter.ErrTemplateNotFound) {
				code = int(errorcode.ErrorCode_NotFound)
			}
			rt.RetCode = int64(code)
			return &templateResponse{
				Res: &types.Res{Ret: &types.Ret{
					RetCode: code,
					RetMsg:  err.Error(),
				}},
				TemplateID: templateID,
			}
		}
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &templateResponse{
		Res: &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_Success),
				RetMsg:  "success",
			},
		},
		TemplateID:    info.TemplateID,
		InstanceType:  info.InstanceType,
		Version:       info.Version,
		Status:        info.Status,
		LastError:     info.LastError,
		Replicas:      info.Replicas,
		CreateRequest: createReq,
	}
}

func listTemplates(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	infos, err := templatecenter.ListTemplates(r.Context())
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized) {
			code = int(errorcode.ErrorCode_DBError)
		}
		rt.RetCode = int64(code)
		return &templateListResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			}},
		}
	}
	rsp := &templateListResponse{
		Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		}},
		Data: make([]templateSummary, 0, len(infos)),
	}
	for _, info := range infos {
		rsp.Data = append(rsp.Data, templateSummary{
			TemplateID:   info.TemplateID,
			InstanceType: info.InstanceType,
			Version:      info.Version,
			Status:       info.Status,
			LastError:    info.LastError,
			CreatedAt:    info.CreatedAt,
			ImageInfo:    info.ImageInfo,
		})
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return rsp
}
