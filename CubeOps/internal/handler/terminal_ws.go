// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/terminal"
)

// TerminalWSHandler is intentionally mounted outside the JWT middleware.
// Browser WebSocket APIs cannot set Authorization; the gateway consumes the
// target-bound one-time grant carried in Sec-WebSocket-Protocol instead.
type TerminalWSHandler struct {
	gateway *terminal.Gateway
}

func NewTerminalWSHandler(gateway *terminal.Gateway) *TerminalWSHandler {
	return &TerminalWSHandler{gateway: gateway}
}

func (h *TerminalWSHandler) Register(r *gin.RouterGroup) {
	r.GET("/terminal/ws", h.Serve)
}

func (h *TerminalWSHandler) Serve(c *gin.Context) {
	h.gateway.ServeHTTP(c.Writer, c.Request)
}
