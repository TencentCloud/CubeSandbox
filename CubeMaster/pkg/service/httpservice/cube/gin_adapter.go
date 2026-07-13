// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

// Adapt wraps a handler returning interface{}, writing the response via
// common.WriteResponse. Streaming handlers return nil on success (they already
// wrote directly to the ResponseWriter); on error they return a *types.Res.
func Adapt(fn func(http.ResponseWriter, *http.Request, *CubeLog.RequestTrace) interface{}) gin.HandlerFunc {
	return func(c *gin.Context) {
		rt := CubeLog.GetTraceInfo(c.Request.Context())
		rsp := fn(c.Writer, c.Request, rt)
		if rsp == nil {
			return // streaming handler already wrote the response
		}
		common.WriteResponse(c.Writer, http.StatusOK, rsp)
	}
}

// AdaptList is identical to Adapt but uses common.WriteListResponse so that
// list endpoints go through the bufferpool path.
func AdaptList(fn func(http.ResponseWriter, *http.Request, *CubeLog.RequestTrace) interface{}) gin.HandlerFunc {
	return func(c *gin.Context) {
		rt := CubeLog.GetTraceInfo(c.Request.Context())
		rsp := fn(c.Writer, c.Request, rt)
		if rsp == nil {
			return
		}
		common.WriteListResponse(c.Writer, http.StatusOK, rsp)
	}
}
