// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	nmhandler "github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

// logClusterStep logs a sub-step failure.
func logClusterStep(ctx context.Context, step string, err error) {
	logging.G(ctx).Warnf("cluster step %q failed: %v", step, err)
}

// ClusterHandler handles cluster-related HTTP requests.
type ClusterHandler struct {
	cm  CubeMasterClient
	svc nmhandler.NodeService
}

func NewClusterHandler(cm CubeMasterClient) *ClusterHandler {
	return &ClusterHandler{cm: cm}
}

func (h *ClusterHandler) WithNodeService(svc nmhandler.NodeService) *ClusterHandler {
	h.svc = svc
	return h
}

func (h *ClusterHandler) Register(r *gin.RouterGroup) {
	r.GET("/cluster/overview", h.Overview)
	r.GET("/cluster/versions", h.Versions)
	r.GET("/nodes", h.ListNodes)
	r.GET("/nodes/:nodeID", h.GetNode)
	r.PATCH("/nodes/:nodeID/labels", h.UpdateLabels)
	r.DELETE("/nodes/:nodeID/labels/:key", h.DeleteLabel)
	r.PUT("/nodes/:nodeID/isolation", h.Isolate)
	r.DELETE("/nodes/:nodeID/isolation", h.Unisolate)
	r.GET("/nodes/:nodeID/operations", h.ListOperations)
}

func (h *ClusterHandler) Overview(c *gin.Context) {
	nodes, err := h.svc.ListNodes(c.Request.Context())
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: overview list nodes failed: %v", err)
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	used := h.fetchUsedResources(c.Request.Context())
	httputil.WriteJSON(c, http.StatusOK, nmhandler.BuildOverview(nodes, used))
}

func (h *ClusterHandler) ListNodes(c *gin.Context) {
	nodes, err := h.svc.ListNodes(c.Request.Context())
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: list nodes failed: %v", err)
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	used := h.fetchUsedResources(c.Request.Context())
	views := make([]nmhandler.NodeView, 0, len(nodes))
	for _, n := range nodes {
		views = append(views, nmhandler.ToNodeView(n, used))
	}
	httputil.WriteJSON(c, http.StatusOK, views)
}

func (h *ClusterHandler) GetNode(c *gin.Context) {
	nodeID := c.Param("nodeID")
	if nodeID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "nodeID is required")
		return
	}
	n, err := h.svc.GetNode(c.Request.Context(), nodeID)
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: get node failed: node=%s: %v", nodeID, err)
		httputil.WriteError(c, http.StatusNotFound, err.Error())
		return
	}
	used := h.fetchUsedResources(c.Request.Context())
	httputil.WriteJSON(c, http.StatusOK, nmhandler.ToNodeView(n, used))
}

func (h *ClusterHandler) Versions(c *gin.Context) {
	matrix, err := h.svc.GetVersionMatrix(c.Request.Context())
	if err != nil {
		httputil.WriteJSON(c, http.StatusOK, nmhandler.EmptyVersionMatrix())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, matrix)
}

func (h *ClusterHandler) UpdateLabels(c *gin.Context) {
	nodeID := c.Param("nodeID")
	var req model.UpdateNodeLabelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: update labels invalid body: node=%s: %v", nodeID, err)
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.svc.UpdateNodeLabels(c.Request.Context(), nodeID, req.Labels, auth.UsernameFromContext(c.Request.Context())); err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: update labels failed: node=%s: %v", nodeID, err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteNoContent(c)
}

func (h *ClusterHandler) DeleteLabel(c *gin.Context) {
	if err := h.svc.DeleteNodeLabel(c.Request.Context(), c.Param("nodeID"), c.Param("key"), auth.UsernameFromContext(c.Request.Context())); err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: delete label failed: node=%s key=%s: %v", c.Param("nodeID"), c.Param("key"), err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteNoContent(c)
}

func (h *ClusterHandler) Isolate(c *gin.Context) {
	h.writeIsolation(c, true)
}

func (h *ClusterHandler) Unisolate(c *gin.Context) {
	h.writeIsolation(c, false)
}

func (h *ClusterHandler) writeIsolation(c *gin.Context, disabled bool) {
	var req struct {
		Detail string `json:"detail"`
	}
	_ = c.ShouldBindJSON(&req)
	snap, err := h.svc.SetNodeSchedulingDisabled(c.Request.Context(), c.Param("nodeID"), disabled, auth.UsernameFromContext(c.Request.Context()), req.Detail)
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: isolation toggle failed: node=%s disabled=%t: %v", c.Param("nodeID"), disabled, err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}

func (h *ClusterHandler) ListOperations(c *gin.Context) {
	ops, err := h.svc.ListOperations(c.Request.Context(), c.Param("nodeID"), 50)
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: list operations failed: node=%s: %v", c.Param("nodeID"), err)
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(c, http.StatusOK, ops)
}

// fetchUsedResources queries CubeMaster for running sandboxes to compute
// per-node CPU/memory usage.
func (h *ClusterHandler) fetchUsedResources(ctx context.Context) map[string]struct {
	CPUMilli int64
	MemoryMB int64
} {
	data, err := h.cm.ListSandboxes(ctx)
	if err != nil {
		logClusterStep(ctx, "list sandboxes", err)
		return map[string]struct {
			CPUMilli int64
			MemoryMB int64
		}{}
	}
	var resp cmSandboxListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		logClusterStep(ctx, "parse sandbox list", err)
		return map[string]struct {
			CPUMilli int64
			MemoryMB int64
		}{}
	}
	used := map[string]struct {
		CPUMilli int64
		MemoryMB int64
	}{}
	for _, sb := range resp.Data {
		if sb.Status != 1 {
			continue
		}
		entry := used[sb.HostIP]
		entry.CPUMilli += int64(sb.CPUCount) * 1000
		entry.MemoryMB += int64(sb.MemoryMB)
		used[sb.HostIP] = entry
	}
	return used
}

type cmSandboxItem struct {
	HostIP   string `json:"host_ip"`
	Status   int    `json:"status"`
	CPUCount int    `json:"cpu_count"`
	MemoryMB int    `json:"memory_mb"`
}

type cmSandboxListResponse struct {
	Data []cmSandboxItem `json:"data"`
}
