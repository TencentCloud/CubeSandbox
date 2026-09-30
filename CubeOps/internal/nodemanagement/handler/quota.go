// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

// QuotaHandler serves the authenticated node quota API.
type QuotaHandler struct {
	svc NodeService
}

func NewQuotaHandler(svc NodeService) *QuotaHandler {
	return &QuotaHandler{svc: svc}
}

func (h *QuotaHandler) Register(r *gin.RouterGroup) {
	r.GET("/nodes/:nodeID/config/quota", h.Get)
	r.PUT("/nodes/:nodeID/config/quota", h.Set)
	r.GET("/nodes/:nodeID/config/quota/history", h.History)
}

func (h *QuotaHandler) Get(c *gin.Context) {
	view, err := h.svc.GetNodeQuotaView(c.Request.Context(), c.Param("nodeID"))
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-quota: get view failed: %v", err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, view)
}

func (h *QuotaHandler) Set(c *gin.Context) {
	var spec model.QuotaSpec
	if err := c.ShouldBindJSON(&spec); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	operator := c.GetString("username")
	view, push, err := h.svc.SetNodeQuota(c.Request.Context(), c.Param("nodeID"), &spec, operator)
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-quota: set failed: node=%s operator=%s: %v", c.Param("nodeID"), operator, err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, gin.H{"view": view, "push": push})
}

func (h *QuotaHandler) History(c *gin.Context) {
	ops, err := h.svc.ListOperations(c.Request.Context(), c.Param("nodeID"), quotaHistoryLimit(c))
	if err != nil {
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, quotaHistoryFromOps(ops))
}

// quotaHistoryLimit parses the limit query param with shared bounds.
func quotaHistoryLimit(c *gin.Context) int {
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	return limit
}

// quotaHistoryFromOps filters quota operations (node- and cluster-level)
// into history entries.
func quotaHistoryFromOps(ops []model.NodeOperation) []model.QuotaHistoryEntry {
	out := make([]model.QuotaHistoryEntry, 0)
	for _, op := range ops {
		if op.Type != model.OpSetQuota && op.Type != model.OpSetClusterQuota {
			continue
		}
		out = append(out, model.QuotaHistoryEntry{
			Operator:  op.Operator,
			Detail:    op.Detail,
			CreatedAt: op.CreatedAt,
		})
	}
	return out
}
