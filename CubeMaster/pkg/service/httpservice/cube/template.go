// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var deleteTemplateFn = templatecenter.DeleteTemplate
var getTemplateInfoFn = templatecenter.GetTemplateInfo
var getTemplateRequestFn = templatecenter.GetTemplateRequest
var resolveTemplateIdentifierFn = templatecenter.ResolveTemplateIdentifier
var setTemplateAliasFn = templatecenter.SetTemplateAlias

type templateResponse struct {
	*types.Res
	TemplateID                 string                         `json:"template_id,omitempty"`
	InstanceType               string                         `json:"instance_type,omitempty"`
	Version                    string                         `json:"version,omitempty"`
	Status                     string                         `json:"status,omitempty"`
	LastError                  string                         `json:"last_error,omitempty"`
	DisplayName                string                         `json:"display_name,omitempty"`
	StorageBackend             string                         `json:"storage_backend,omitempty"`
	Backend                    string                         `json:"backend,omitempty"`
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
	TemplateID     string `json:"template_id,omitempty"`
	InstanceType   string `json:"instance_type,omitempty"`
	Version        string `json:"version,omitempty"`
	Status         string `json:"status,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	StorageBackend string `json:"storage_backend,omitempty"`
	Backend        string `json:"backend,omitempty"`
	OriginNodeID   string `json:"origin_node_id,omitempty"`
	OriginNodeIP   string `json:"origin_node_ip,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	ImageInfo      string `json:"image_info,omitempty"`
	JobID          string `json:"job_id,omitempty"`
}

type deleteTemplateRequest struct {
	RequestID    string `json:"RequestID,omitempty"`
	TemplateID   string `json:"template_id,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	Sync         bool   `json:"sync,omitempty"`
}

func createTemplateGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, createTemplate(c.Request, rt))
}

func getTemplateGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, getTemplate(c.Request, rt))
}

func deleteTemplateGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, deleteTemplate(c.Request, rt))
}

func setTemplateAliasGinHandler(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	common.WriteAPI(c, setTemplateAlias(c.Request, rt, c.Param("template_id")))
}

// setAliasRequest is the body for PUT /cube/template/:template_id/alias.
// alias is optional; absent / null / "" means "clear" (design §3.1).
type setAliasRequest struct {
	Alias string `json:"alias,omitempty"`
}

// setTemplateAlias sets, transfers, or clears the alias of an existing
// template. The templateID path param may be a tpl- id or a current alias
// (resolved via resolveTemplateIdentifierFn, matching GET/DELETE).
//
// Error mapping (design §3.3):
//   - validateTemplateAlias failure → 400 MasterParamsError
//   - ErrTemplateNotFound (missing OR DELETING) → 404 NotFound
//   - ErrAliasNotApplicableToSnapshot → 400 MasterParamsError (a snapshot
//     exists but has no alias slot — distinct from a genuine 404)
//   - isDuplicateAliasError → 130409 Conflict (so CubeAPI maps to HTTP 409)
//   - ErrTemplateNotReady → 130409 Conflict (retry after the template is READY)
//   - other → 500 MasterInternalError
//
// templateResponseFromInfo builds the success templateResponse shared by
// getTemplate and setTemplateAlias so both endpoints always return the
// identical shape (avoids the two hand-written 15-field copies drifting).
// createReq is included only when non-nil (GET ?include_request=true).
func templateResponseFromInfo(info *templatecenter.TemplateInfo, createReq *types.CreateCubeSandboxReq) *templateResponse {
	return &templateResponse{
		Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  "success",
		}},
		TemplateID:                 info.TemplateID,
		InstanceType:               info.InstanceType,
		Version:                    info.Version,
		Status:                     info.Status,
		LastError:                  info.LastError,
		DisplayName:                info.DisplayName,
		StorageBackend:             info.StorageBackend,
		Backend:                    firstNonEmptyTrimmed(info.Backend, info.StorageBackend),
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

func setTemplateAlias(r *http.Request, rt *CubeLog.RequestTrace, rawTemplateID string) interface{} {
	if strings.TrimSpace(rawTemplateID) == "" {
		return &templateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  "template_id is required",
			}},
		}
	}
	// Body is optional; missing body = clear alias. GetBodyReq tolerates an
	// empty body only if the JSON is valid; treat empty body as alias="".
	bodyReq := &setAliasRequest{}
	if r.ContentLength != 0 {
		if err := common.GetBodyReq(r, bodyReq); err != nil {
			return &templateResponse{
				Res: &types.Res{Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_MasterParamsError),
					RetMsg:  err.Error(),
				}},
			}
		}
	}
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"Action":     "SetTemplateAlias",
		"TemplateID": rawTemplateID,
	}))
	resolvedTemplateID, err := resolveTemplateIdentifierFn(ctx, rawTemplateID)
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
			TemplateID: rawTemplateID,
		}
	}
	if err := setTemplateAliasFn(ctx, resolvedTemplateID, bodyReq.Alias); err != nil {
		code := int(errorcode.ErrorCode_MasterInternalError)
		switch {
		case errors.Is(err, templatecenter.ErrTemplateNotFound):
			code = int(errorcode.ErrorCode_NotFound)
		// A snapshot exists but aliases don't apply to it — map to 400 so
		// clients can distinguish "operation not supported on this resource"
		// from a genuine 404 "template not found".
		case errors.Is(err, templatecenter.ErrAliasNotApplicableToSnapshot):
			code = int(errorcode.ErrorCode_MasterParamsError)
		case errors.Is(err, templatecenter.ErrInvalidAlias):
			code = int(errorcode.ErrorCode_MasterParamsError)
		case errors.Is(err, templatecenter.ErrTemplateNotReady):
			code = int(errorcode.ErrorCode_Conflict)
		case templatecenter.IsDuplicateAliasError(err):
			code = int(errorcode.ErrorCode_Conflict)
		case errors.Is(err, templatecenter.ErrTemplateStoreNotInitialized):
			code = int(errorcode.ErrorCode_DBError)
		default:
			// Unknown errors (e.g. raw gorm DB failures) → 500.
			code = int(errorcode.ErrorCode_MasterInternalError)
		}
		rt.RetCode = int64(code)
		return &templateResponse{
			Res: &types.Res{Ret: &types.Ret{
				RetCode: code,
				RetMsg:  err.Error(),
			}},
			TemplateID: resolvedTemplateID,
		}
	}
	info, err := getTemplateInfoFn(ctx, resolvedTemplateID)
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
			TemplateID: resolvedTemplateID,
		}
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return templateResponseFromInfo(info, nil)
}

func deleteTemplate(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
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
	// Alias resolution: resolve human-readable aliases to template IDs,
	// matching the GET handler (see getTemplate).
	resolvedTemplateID, err := resolveTemplateIdentifierFn(ctx, req.TemplateID)
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
			TemplateID: req.TemplateID,
		}
	}
	err = deleteTemplateFn(ctx, resolvedTemplateID, req.InstanceType)
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

func createTemplate(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
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

func getTemplate(r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	templateID := r.URL.Query().Get("template_id")
	includeRequest := r.URL.Query().Get("include_request") == "true" || r.URL.Query().Get("include_request") == "1"
	if templateID == "" {
		return listTemplates(r, rt)
	}
	resolvedTemplateID, err := resolveTemplateIdentifierFn(r.Context(), templateID)
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
	info, err := getTemplateInfoFn(r.Context(), resolvedTemplateID)
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
		createReq, err = getTemplateRequestFn(r.Context(), resolvedTemplateID)
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
	return templateResponseFromInfo(info, createReq)
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
		originNodeID := strings.TrimSpace(info.OriginNodeID)
		originNodeIP := ""
		if originNodeID != "" {
			if n, ok := localcache.GetNode(originNodeID); ok && n != nil {
				originNodeIP = strings.TrimSpace(n.HostIP())
			}
		}
		// create-from-image templates often have empty origin_node_id; fall
		// back to any READY replica so list can still show an origin node.
		if originNodeIP == "" {
			originNodeID, originNodeIP = firstReadyReplicaOrigin(r.Context(), info.TemplateID, originNodeID)
		}
		rsp.Data = append(rsp.Data, templateSummary{
			TemplateID:     info.TemplateID,
			InstanceType:   info.InstanceType,
			Version:        info.Version,
			Status:         info.Status,
			LastError:      info.LastError,
			DisplayName:    info.DisplayName,
			StorageBackend: info.StorageBackend,
			Backend:        firstNonEmptyTrimmed(info.Backend, info.StorageBackend),
			OriginNodeID:   originNodeID,
			OriginNodeIP:   originNodeIP,
			CreatedAt:      info.CreatedAt,
			ImageInfo:      info.ImageInfo,
			JobID:          info.JobID,
		})
	}
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return rsp
}

func firstReadyReplicaOrigin(ctx context.Context, templateID, originNodeID string) (string, string) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return originNodeID, ""
	}
	replicas, err := templatecenter.ListReplicas(ctx, templateID)
	if err != nil {
		return originNodeID, ""
	}
	for _, replica := range replicas {
		if !strings.EqualFold(strings.TrimSpace(replica.Status), templatecenter.ReplicaStatusReady) {
			continue
		}
		nodeID := strings.TrimSpace(replica.NodeID)
		nodeIP := strings.TrimSpace(replica.NodeIP)
		if nodeIP == "" && nodeID != "" {
			if n, ok := localcache.GetNode(nodeID); ok && n != nil {
				nodeIP = strings.TrimSpace(n.HostIP())
			}
		}
		if nodeIP == "" {
			continue
		}
		if originNodeID == "" {
			originNodeID = nodeID
		}
		return originNodeID, nodeIP
	}
	return originNodeID, ""
}
