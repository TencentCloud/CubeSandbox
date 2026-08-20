// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

// AgentHandler serves /internal/v1/node-agent/node* routes for Cubelet.
type AgentHandler struct {
	svc NodeService
}

func NewAgentHandler(svc NodeService) *AgentHandler {
	return &AgentHandler{svc: svc}
}

func (h *AgentHandler) Register(r *gin.RouterGroup) {
	r.GET("/readyz", h.Readyz)
	r.POST("/nodes/register", h.RegisterNode)
	r.POST("/nodes/:nodeID/status", h.UpdateStatus)
}

func (h *AgentHandler) RegisterNode(c *gin.Context) {
	var req model.RegisterNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-agent: register invalid body: %v", err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	snap, err := h.svc.RegisterNode(c.Request.Context(), &req)
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-agent: register failed: node=%s: %v", req.NodeID, err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}

func (h *AgentHandler) UpdateStatus(c *gin.Context) {
	nodeID := c.Param("nodeID")
	if nodeID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "nodeID is required")
		return
	}
	var req model.UpdateNodeStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-agent: status invalid body: node=%s: %v", nodeID, err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	snap, err := h.svc.UpdateNodeStatus(c.Request.Context(), nodeID, &req)
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-agent: status update failed: node=%s: %v", nodeID, err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}

// Readyz uses the legacy CubeMaster envelope for existing cubelets.
func (h *AgentHandler) Readyz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"ret": gin.H{
			"ret_code": 200,
			"ret_msg":  "ok",
		},
	})
}
