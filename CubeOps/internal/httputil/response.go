// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package httputil contains small HTTP helpers shared by handlers.
package httputil

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/model"
)

// WriteJSON serialises v as JSON and writes it with the given status.
//
// We use gin's Render so charset + content-type are handled consistently
// with the rest of the framework.
func WriteJSON(c *gin.Context, status int, v interface{}) {
	c.JSON(status, v)
}

// WriteError writes a JSON error response in the shape the frontend
// already expects: {"error": "..."}.
func WriteError(c *gin.Context, status int, msg string) {
	c.JSON(status, model.APIError{Error: msg})
}

// WriteRawJSON writes a pre-encoded JSON body verbatim (used when proxying
// CubeMaster responses without re-marshalling).
func WriteRawJSON(c *gin.Context, status int, raw json.RawMessage) {
	c.Data(status, "application/json", raw)
}

// WriteNoContent writes a 204 with no body.
func WriteNoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}
