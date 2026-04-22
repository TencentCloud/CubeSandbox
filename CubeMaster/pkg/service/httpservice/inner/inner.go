// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package inner provides the inner services of cube-master
package inner

import (
	"net/http"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	innerURI         = "/internal"
	NodeAction       = "/node"
	FakeCreateAction = "/fake_create"
	StateWs          = "/ws"
	StateQuery       = "/query"
)

func InnerURI() string {
	return innerURI
}

func actionURI(uri string) string {
	return filepath.Clean(filepath.Join(innerURI, uri))
}

func HttpHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rt := CubeLog.GetTraceInfo(ctx)
	var rsp interface{}
	switch r.URL.Path {
	case actionURI(NodeAction):
		req := &types.GetNodeReq{}
		queries := r.URL.Query()
		req.RequestID = queries.Get("requestID")
		req.HostID = queries.Get("host_id")
		ss := queries.Get("score_only")
		if ss != "" && ss == "true" {
			req.ScoreOnly = true
		}
		rt.RequestID = req.RequestID
		rsp = getNodeInfo(ctx, req)
	case actionURI(StateWs):
		handleWebsocket(w, r)
		return
	case actionURI(StateQuery):
		handleQuery(w, r)
		return
	default:
		rsp = &types.Res{
			Ret: &types.Ret{
				RetCode: -1,
				RetMsg:  http.StatusText(http.StatusNotFound),
			},
		}
	}
	common.WriteResponse(w, http.StatusOK, rsp)
}
