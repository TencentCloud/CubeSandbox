// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package notify provides a notification service.
package notify

import (
	"context"
	"net/http"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	notifyURI              = "/notify"
	HostChangeNotifyAction = "/host"
	HealthCheckAction      = "/health"
)

func NotifyURI() string {
	return notifyURI
}

func actionURI(uri string) string {
	return filepath.Clean(filepath.Join(notifyURI, uri))
}

func HttpHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rt := CubeLog.GetTraceInfo(ctx)
	rsp := &types.Res{
		Ret: &types.Ret{
			RetCode: -1,
			RetMsg:  http.StatusText(http.StatusNotFound),
		},
	}
	switch r.RequestURI {
	case actionURI(HostChangeNotifyAction):
		req := &types.HostChangeEvent{}
		if err := common.GetBodyReq(r, req); err != nil {
			rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
			rsp.Ret.RetMsg = err.Error()
			break
		}
		rt.RequestID = req.RequestID
		ctx := log.WithLogger(r.Context(), log.G(r.Context()).WithFields(map[string]any{
			"RequestId": req.RequestID,
		}))
		rsp = hostChangeNotify(ctx, req)
	case actionURI(HealthCheckAction):
		rsp = healthCheck(w, r)
	default:
	}
	rt.RetCode = int64(rsp.Ret.RetCode)
	common.WriteResponse(w, http.StatusOK, rsp)
}

func hostChangeNotify(ctx context.Context, req *types.HostChangeEvent) (rsp *types.Res) {
	log.G(ctx).Debugf("%+v", utils.InterfaceToString(req))
	rsp = &types.Res{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	event := &localcache.Event{
		Type:   localcache.EventType(req.EventType),
		InsIDs: req.HostIDs,
	}
	if err := localcache.NotifyEvent(event); err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterInternalError)
		rsp.Ret.RetMsg = err.Error()
		log.G(ctx).Errorf("hostChangeNotify notify event failed, err: %v", err)
		return
	}
	return
}

func healthCheck(w http.ResponseWriter, r *http.Request) (rsp *types.Res) {
	log.G(r.Context()).Debug("healthCheck coming")
	rsp = &types.Res{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	return
}
