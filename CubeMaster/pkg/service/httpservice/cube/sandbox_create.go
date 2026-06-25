// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	createSandboxDealCubeboxCreateReqWithTemplateFn = dealCubeboxCreateReqWithTemplate
	createSandboxRunFn                              = sandbox.CreateSandbox
	createSandboxGetTemplateKindFn                  = templatecenter.GetTemplateKind
	createSandboxRegisterRuntimeRefFn               = templatecenter.RegisterSnapshotRuntimeRefForCreatedSandbox
	createSandboxRegisterRuntimeRefWithReplicaFn    = templatecenter.RegisterSnapshotRuntimeRefForCreatedSandboxWithReplica
)

func createSandbox(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	_ = w
	rt.RetCode = -1
	rsp := &types.Res{
		Ret: &types.Ret{
			RetCode: -1,
			RetMsg:  http.StatusText(http.StatusNotFound),
		},
	}

	req, err := constructCreateReq(r)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = err.Error()
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		return rsp
	}
	rsp.RequestID = req.RequestID
	rt.RequestID = req.RequestID
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
	}))
	resolveResult := &templateResolveResult{}
	ctx = withTemplateResolveResult(ctx, resolveResult)

	if err := createSandboxDealCubeboxCreateReqWithTemplateFn(ctx, req); err != nil {
		retCode := errorcode.ErrorCode_MasterParamsError
		if errors.Is(err, templatecenter.ErrTemplateNotFound) {
			retCode = errorcode.ErrorCode_NotFound
		} else if errors.Is(err, templatecenter.ErrTemplateStaleNeedsRedo) {
			retCode = errorcode.ErrorCode_Conflict
		}
		rsp.Ret.RetCode = int(retCode)
		rsp.Ret.RetMsg = err.Error()
		rt.RetCode = int64(retCode)
		log.G(ctx).Error(err)
		return rsp
	}

	ctx, err = runInsReq2Affinity(ctx, req)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = err.Error()
		rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		log.G(ctx).Error(err)
		return rsp
	}
	ret := createSandboxRunFn(ctx, req)
	if ret != nil && ret.Ret != nil && ret.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		if err := registerCreatedSandboxRuntimeRef(ctx, req, ret); err != nil {
			log.G(ctx).Warnf("register snapshot runtime ref after create failed: %v", err)
		}
		// Echo the envd version (propagated from the template annotation onto the
		// create request) back to the caller via the existing ext_info map, so
		// CubeAPI can surface it without an extra round-trip. Success branch only.
		if v := strings.TrimSpace(req.Annotations[constants.CubeAnnotationComponentEnvdVersion]); v != "" {
			if ret.ExtInfo == nil {
				ret.ExtInfo = make(map[string]string)
			}
			ret.ExtInfo[constants.CubeAnnotationComponentEnvdVersion] = v
		}
	}
	rt.RetCode = int64(ret.Ret.RetCode)
	return ret
}

func registerCreatedSandboxRuntimeRef(ctx context.Context, req *types.CreateCubeSandboxReq, ret *types.CreateCubeSandboxRes) error {
	if req == nil || ret == nil {
		return nil
	}
	templateID := strings.TrimSpace(req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	if templateID == "" {
		return nil
	}
	// Reuse the kind / chosen replica that the synchronous template-resolve
	// phase already discovered (see dealCubeboxCreateReqWithTemplateCenter
	// + bindSnapshotCreateReplica) so the post-create path doesn't re-issue
	// a GetDefinition + ListReplicas round-trip per create.
	resolved := templateResolveResultFromContext(ctx)
	kind := ""
	if resolved != nil && strings.EqualFold(strings.TrimSpace(resolved.TemplateID), templateID) {
		kind = resolved.Kind
	}
	if kind == "" {
		var err error
		kind, err = createSandboxGetTemplateKindFn(ctx, templateID)
		if err != nil {
			return err
		}
	}
	if !strings.EqualFold(strings.TrimSpace(kind), templatecenter.TemplateKindSnapshot) {
		return nil
	}
	if resolved != nil && resolved.HasChosenReplica &&
		strings.EqualFold(strings.TrimSpace(resolved.TemplateID), templateID) {
		return createSandboxRegisterRuntimeRefWithReplicaFn(
			ctx, templateID, ret.SandboxID, ret.HostID, ret.HostIP, resolved.ChosenReplica,
		)
	}
	return createSandboxRegisterRuntimeRefFn(ctx, templateID, ret.SandboxID, ret.HostID, ret.HostIP)
}

func constructCreateReq(r *http.Request) (*types.CreateCubeSandboxReq, error) {
	req := &types.CreateCubeSandboxReq{}
	if err := common.GetBodyReq(r, req); err != nil {
		return nil, err
	}

	if req.Request == nil {
		return nil, errors.New("requestID is nil")
	}

	if req.Labels == nil {
		req.Labels = map[string]string{}
	}
	if req.Annotations == nil {
		req.Annotations = map[string]string{}
	}
	constants.NormalizeAppSnapshotAnnotations(req.Annotations)
	if req.InstanceType == "" {
		if req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] != "" {
			req.InstanceType = cubebox.InstanceType_cubebox.String()
		} else {
			req.InstanceType = cubebox.InstanceType_cubebox.String()
		}
	}
	if req.NetworkType == "" {
		req.NetworkType = cubebox.NetworkType_tap.String()
	}
	if templateID := req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]; templateID != "" {
		req.Labels[constants.CubeAnnotationAppSnapshotTemplateID] = templateID
	}
	req.Labels[constants.Caller] = getCaller(r)
	req.Labels[constants.CubeAnnotationsInsType] = req.InstanceType
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	return req, nil
}

func dealAppWhitelistHook(ctx context.Context, req *types.CreateCubeSandboxReq) {
	_ = ctx
	_ = req
}
