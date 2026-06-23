// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var deleteTemplateFn = templatecenter.DeleteTemplate
var getTemplateInfoFn = templatecenter.GetTemplateInfo
var getTemplateRequestFn = templatecenter.GetTemplateRequest
var lookupTemplateIDByDisplayName = templatecenter.FindTemplateIDByDisplayName
var lookupTemplateIDByDisplayNameFresh = templatecenter.FindTemplateIDByDisplayNameFresh

type templateResponse struct {
	*types.Res
	TemplateID                 string                         `json:"template_id,omitempty"`
	DisplayName                string                         `json:"display_name,omitempty"`
	InstanceType               string                         `json:"instance_type,omitempty"`
	Version                    string                         `json:"version,omitempty"`
	Status                     string                         `json:"status,omitempty"`
	LastError                  string                         `json:"last_error,omitempty"`
	CreatedAt                  string                         `json:"created_at,omitempty"`
	ImageInfo                  string                         `json:"image_info,omitempty"`
	JobID                      string                         `json:"job_id,omitempty"`
	Replicas                   []templatecenter.ReplicaStatus `json:"replicas,omitempty"`
	CreateRequest              *types.CreateCubeSandboxReq    `json:"create_request,omitempty"`
	CubeEgressCABaked          bool                           `json:"cube_egress_ca_baked,omitempty"`
	CubeEgressCAFingerprint    string                         `json:"cube_egress_ca_fingerprint,omitempty"`
	CubeEgressCATargetsWritten int                            `json:"cube_egress_ca_targets_written,omitempty"`
}

type templateListResponse struct {
	*types.Res
	Data []templateSummary `json:"data,omitempty"`
}

type templateSummary struct {
	TemplateID   string `json:"template_id,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	Version      string `json:"version,omitempty"`
	Status       string `json:"status,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	ImageInfo    string `json:"image_info,omitempty"`
	JobID        string `json:"job_id,omitempty"`
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
			code = int(errorcode.ErrorCode_Conflict)
		case errors.Is(err, templatecenter.ErrTemplateAttemptInProgress):
			code = int(errorcode.ErrorCode_Conflict)
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
		TemplateID:                 info.TemplateID,
		DisplayName:                info.DisplayName,
		InstanceType:               info.InstanceType,
		Version:                    info.Version,
		Status:                     info.Status,
		LastError:                  info.LastError,
		JobID:                      info.JobID,
		Replicas:                   info.Replicas,
		CubeEgressCABaked:          info.CubeEgressCABaked,
		CubeEgressCAFingerprint:    info.CubeEgressCAFingerprint,
		CubeEgressCATargetsWritten: info.CubeEgressCATargetsWritten,
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
		TemplateID:                 info.TemplateID,
		DisplayName:                info.DisplayName,
		InstanceType:               info.InstanceType,
		Version:                    info.Version,
		Status:                     info.Status,
		LastError:                  info.LastError,
		CreatedAt:                  info.CreatedAt,
		ImageInfo:                  info.ImageInfo,
		JobID:                      info.JobID,
		Replicas:                   info.Replicas,
		CreateRequest:              createReq,
		CubeEgressCABaked:          info.CubeEgressCABaked,
		CubeEgressCAFingerprint:    info.CubeEgressCAFingerprint,
		CubeEgressCATargetsWritten: info.CubeEgressCATargetsWritten,
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
			DisplayName:  info.DisplayName,
			InstanceType: info.InstanceType,
			Version:      info.Version,
			Status:       info.Status,
			LastError:    info.LastError,
			CreatedAt:    info.CreatedAt,
			ImageInfo:    info.ImageInfo,
			JobID:        info.JobID,
		})
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return rsp
}

// templateLookupResponse carries the templateID resolved from a display name.
type templateLookupResponse struct {
	*types.Res
	TemplateID  string `json:"template_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type updateTemplateDisplayNameRequest struct {
	RequestID   string `json:"RequestID,omitempty"`
	TemplateID  string `json:"template_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// handleTemplateLookupAction resolves a template display name to its templateID.
// This is distinct from internal "template resolve" (node/locality binding); it
// is a name-to-ID lookup backed by the unique display_name_key index.
func handleTemplateLookupAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	if r.Method != http.MethodGet {
		rt.RetCode = -1
		return &templateLookupResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: -1,
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			}},
		}
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &templateLookupResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "name is required",
			}},
		}
	}
	ctx := log.WithLogger(r.Context(), log.G(r.Context()))
	fresh := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("fresh")), "true") ||
		strings.TrimSpace(r.URL.Query().Get("fresh")) == "1"
	var templateID string
	var err error
	if fresh {
		templateID, err = lookupTemplateIDByDisplayNameFresh(ctx, name)
	} else {
		templateID, err = lookupTemplateIDByDisplayName(ctx, name)
	}
	if err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		retMsg := "internal error"
		switch {
		case errors.Is(err, templatecenter.ErrTemplateNameNotFound),
			errors.Is(err, templatecenter.ErrTemplateNameAmbiguous):
			code = int(errorcode.ErrorCode_NotFound)
			retMsg = "template name not found"
		case errors.Is(err, templatecenter.ErrTemplateNameInvalid):
			code = int(errorcode.ErrorCode_MasterParamsError)
			retMsg = err.Error()
		default:
			log.G(ctx).Errorf("template lookup failed: %v", err)
		}
		rt.RetCode = int64(code)
		return &templateLookupResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: code,
				RetMsg:  retMsg,
			}},
		}
	}
	normalized, normErr := templatecenter.NormalizeTemplateDisplayName(name)
	if normErr != nil {
		log.G(ctx).Warnf("template lookup display_name normalization failed: %v", normErr)
		normalized = strings.TrimSpace(name)
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &templateLookupResponse{
		Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		}},
		TemplateID:  templateID,
		DisplayName: normalized,
	}
}

func handleTemplateDisplayNameAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	if r.Method != http.MethodPost {
		rt.RetCode = -1
		return &types.Res{Ret: &types.Ret{
			RetCode: -1,
			RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
		}}
	}
	req := &updateTemplateDisplayNameRequest{}
	if err := utils.DecodeHttpBody(r.Body, req); err != nil {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  "请求体解析失败",
		}}
	}
	if req.TemplateID == "" {
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  "template_id is required",
		}}
	}
	rt.RequestID = req.RequestID
	ctx := CubeLog.WithRequestTrace(r.Context(), rt)
	if err := templatecenter.UpdateDefinitionDisplayName(ctx, req.TemplateID, req.DisplayName); err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		retMsg := "internal error"
		switch {
		case errors.Is(err, templatecenter.ErrTemplateNotFound):
			code = int(errorcode.ErrorCode_NotFound)
			retMsg = err.Error()
		case errors.Is(err, templatecenter.ErrTemplateNameInUse):
			code = int(errorcode.ErrorCode_Conflict)
			retMsg = err.Error()
		case errors.Is(err, templatecenter.ErrTemplateNameInvalid):
			code = int(errorcode.ErrorCode_MasterParamsError)
			retMsg = err.Error()
		default:
			log.G(ctx).Errorf("update template display name failed: %v", err)
		}
		rt.RetCode = int64(code)
		return &types.Res{Ret: &types.Ret{
			RetCode: code,
			RetMsg:  retMsg,
		}}
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return &types.Res{Ret: &types.Ret{
		RetCode: int(errorcode.ErrorCode_Success),
		RetMsg:  "success",
	}}
}
