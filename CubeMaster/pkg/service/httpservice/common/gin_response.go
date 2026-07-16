// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package common

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// WriteAPI writes a standard API response onto a gin.Context: HTTP 200 with the
// JSON envelope serialized via FastestJsoniter (NOT gin's encoding/json). It is
// the single write path for success/error responses so handlers stop hand-rolling
// c.Writer + Content-Type + status. The request trace (rt.RetCode) is still set
// by the caller, since some handlers record a sentinel value for metrics.
func WriteAPI(c *gin.Context, data interface{}) {
	WriteResponse(c.Writer, http.StatusOK, data)
}

// WriteListAPI is the list-endpoint variant, routing through the bufferpool-backed
// list encoder (WriteListResponse).
func WriteListAPI(c *gin.Context, data interface{}) {
	WriteListResponse(c.Writer, http.StatusOK, data)
}

// WriteErr writes an HTTP 200 response carrying a business error (ret_code/ret_msg)
// in the standard envelope, for the common case where a handler just needs to
// surface an error code + message.
func WriteErr(c *gin.Context, code int, msg string) {
	WriteResponse(c.Writer, http.StatusOK, &types.Res{
		Ret: &types.Ret{
			RetCode: code,
			RetMsg:  msg,
		},
	})
}
