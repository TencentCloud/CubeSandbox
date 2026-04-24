// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"net/http"

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

	if err := createSandboxDealCubeboxCreateReqWithTemplateFn(ctx, req); err != nil {
		retCode := errorcode.ErrorCode_MasterParamsError
		if errors.Is(err, templatecenter.ErrTemplateNotFound) {
			retCode = errorcode.ErrorCode_NotFound
		}
		rsp.Ret.RetCode = int(retCode)
		rsp.Ret.RetMsg = err.Error()
		rt.RetCode = int64(retCode)
		log.G(ctx).Error(err)
		return rsp
	}

	ctx = runInsReq2Affinity(ctx, req)
	ret := createSandboxRunFn(ctx, req)
	rt.RetCode = int64(ret.Ret.RetCode)
	return ret
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
