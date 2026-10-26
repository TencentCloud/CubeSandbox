// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package common

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// WriteAPI writes a standard API response onto a gin.Context: HTTP 200 with the JSON
// envelope serialized via FastestJsoniter (NOT gin's encoding/json), so handlers stop
// hand-rolling c.Writer + Content-Type + status. It is the success-path write helper
// the cube/inner/notify handlers (and meta's success paths) go through. This is not a
// unified single write path: cube error responses still build the envelope inline
// (ret_code/ret_msg under HTTP 200), and the meta package keeps its own writeErr for
// error responses, some of which return HTTP 400 on a malformed body. The request trace
// (rt.RetCode) is still set by the caller, since some handlers record a sentinel value
// for metrics.
func WriteAPI(c *gin.Context, data interface{}) {
	WriteResponse(c.Writer, http.StatusOK, data)
}

// WriteListAPI is the list-endpoint success-path variant, routing through the
// bufferpool-backed list encoder (WriteListResponse); same HTTP 200 + FastestJsoniter
// envelope contract as WriteAPI.
func WriteListAPI(c *gin.Context, data interface{}) {
	WriteListResponse(c.Writer, http.StatusOK, data)
}

// WriteErr is a convenience helper that writes an HTTP 200 response carrying a business
// error (ret_code/ret_msg) in the standard envelope, for the common case where a handler
// just needs to surface an error code + message. It is currently exercised by the common
// package's contract tests; production handlers build their error envelopes inline via
// WriteAPI (cube/inner/notify) or the meta package's own writeErr (which can return HTTP
// 400), so WriteErr is not yet on the hot path.
func WriteErr(c *gin.Context, code int, msg string) {
	WriteResponse(c.Writer, http.StatusOK, &types.Res{
		Ret: &types.Ret{
			RetCode: code,
			RetMsg:  msg,
		},
	})
}
