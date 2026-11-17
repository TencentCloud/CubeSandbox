// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

// OpsAgentHandler serves the ops-agent pull endpoint.
type OpsAgentHandler struct {
	svc NodeService
}

func NewOpsAgentHandler(svc NodeService) *OpsAgentHandler {
	return &OpsAgentHandler{svc: svc}
}

func (h *OpsAgentHandler) Register(r *gin.RouterGroup) {
	r.GET("/:nodeID/config/quota", h.GetSpec)
}

func (h *OpsAgentHandler) GetSpec(c *gin.Context) {
	resp, err := h.svc.GetOpsAgentSpec(c.Request.Context(), c.Param("nodeID"))
	if err != nil {
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, resp)
}
