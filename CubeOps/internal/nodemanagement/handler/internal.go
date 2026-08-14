// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

// InternalHandler serves /internal/v1/node* routes for CubeMaster and CLI.
type InternalHandler struct {
	svc NodeService
}

func NewInternalHandler(svc NodeService) *InternalHandler {
	return &InternalHandler{svc: svc}
}

func (h *InternalHandler) Register(r *gin.RouterGroup) {
	r.GET("/nodes", h.ListNodes)
	r.GET("/nodes/:nodeID", h.GetNode)
	r.PUT("/nodes/:nodeID/isolation", h.Isolate)
	r.DELETE("/nodes/:nodeID/isolation", h.Unisolate)
	r.PUT("/nodes/:nodeID/labels", h.SetLabels)
	r.DELETE("/nodes/:nodeID/labels/:key", h.DeleteLabel)
	r.DELETE("/nodes/:nodeID", h.DeleteNode)
}

func (h *InternalHandler) ListNodes(c *gin.Context) {
	var nodes []*model.NodeSnapshot
	var err error
	if ids := c.Query("ids"); ids != "" {
		parts := strings.Split(ids, ",")
		nodes = make([]*model.NodeSnapshot, 0, len(parts))
		for _, id := range parts {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			snap, err := h.svc.GetNode(c.Request.Context(), id)
			if err != nil {
				continue
			}
			nodes = append(nodes, snap)
		}
	} else {
		nodes, err = h.svc.ListNodes(c.Request.Context())
		if err != nil {
			logging.G(c.Request.Context()).Errorf("nodemgmt-internal: list nodes failed: %v", err)
			httputil.WriteError(c, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if scoreOnly := c.Query("score_only"); scoreOnly == "true" {
		out := make([]*model.SchedulerNode, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, service.SchedulerNodeScoreView(n))
		}
		httputil.WriteJSON(c, http.StatusOK, out)
		return
	}

	out := make([]*model.SchedulerNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, service.ToSchedulerNode(n))
	}
	httputil.WriteJSON(c, http.StatusOK, out)
}

func (h *InternalHandler) GetNode(c *gin.Context) {
	nodeID := c.Param("nodeID")
	if nodeID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "nodeID is required")
		return
	}
	n, err := h.svc.GetNode(c.Request.Context(), nodeID)
	if err != nil {
		httputil.WriteError(c, http.StatusNotFound, err.Error())
		return
	}
	if c.Query("score_only") == "true" {
		httputil.WriteJSON(c, http.StatusOK, service.SchedulerNodeScoreView(n))
		return
	}
	httputil.WriteJSON(c, http.StatusOK, service.ToSchedulerNode(n))
}

func (h *InternalHandler) Isolate(c *gin.Context) {
	h.writeIsolation(c, true)
}

func (h *InternalHandler) Unisolate(c *gin.Context) {
	h.writeIsolation(c, false)
}

func (h *InternalHandler) writeIsolation(c *gin.Context, disabled bool) {
	nodeID := c.Param("nodeID")
	var req struct {
		Detail string `json:"detail"`
	}
	_ = c.ShouldBindJSON(&req)
	snap, err := h.svc.SetNodeSchedulingDisabled(c.Request.Context(), nodeID, disabled, "internal", req.Detail)
	if err != nil {
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}

func (h *InternalHandler) SetLabels(c *gin.Context) {
	nodeID := c.Param("nodeID")
	var req struct {
		Labels map[string]string `json:"labels"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: set-labels invalid body: node=%s: %v", nodeID, err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := h.svc.UpdateNodeLabels(c.Request.Context(), nodeID, req.Labels, "internal"); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: set-labels failed: node=%s: %v", nodeID, err)
		MapNodeError(c, err)
		return
	}
	snap, err := h.svc.GetNode(c.Request.Context(), nodeID)
	if err != nil {
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}

func (h *InternalHandler) DeleteLabel(c *gin.Context) {
	nodeID := c.Param("nodeID")
	key := c.Param("key")
	if err := h.svc.DeleteNodeLabel(c.Request.Context(), nodeID, key, "internal"); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: delete-label failed: node=%s key=%s: %v", nodeID, key, err)
		MapNodeError(c, err)
		return
	}
	snap, err := h.svc.GetNode(c.Request.Context(), nodeID)
	if err != nil {
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}

// DeleteNode handles DELETE /internal/v1/nodes/:nodeID. The node must be
// isolated and free of sandboxes unless ?force=true.
func (h *InternalHandler) DeleteNode(c *gin.Context) {
	nodeID := c.Param("nodeID")
	if nodeID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "nodeID is required")
		return
	}
	force := c.Query("force") == "true"
	snap, err := h.svc.DeleteNode(c.Request.Context(), nodeID, force)
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: delete failed: node=%s force=%t: %v", nodeID, force, err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}
