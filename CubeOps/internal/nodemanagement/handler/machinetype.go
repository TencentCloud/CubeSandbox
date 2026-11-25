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
)

func (h *InternalHandler) registerMachineTypes(r *gin.RouterGroup) {
	r.GET("/machine-types", h.ListMachineTypes)
	r.POST("/machine-types", h.CreateMachineType)
	r.GET("/machine-types/:name", h.GetMachineType)
	r.PATCH("/machine-types/:name", h.UpdateMachineType)
	r.DELETE("/machine-types/:name", h.DeleteMachineType)
	r.PUT("/machine-types/:name/nodes", h.ReplaceMachineTypeNodes)
	r.POST("/machine-types/:name/nodes", h.BindMachineTypeNodes)
	r.DELETE("/machine-types/:name/nodes", h.UnbindMachineTypeNodes)
	r.PUT("/nodes/:nodeID/machine-type", h.SetNodeMachineType)
	r.DELETE("/nodes/:nodeID/machine-type", h.ClearNodeMachineType)
}

func (h *InternalHandler) ListMachineTypes(c *gin.Context) {
	views, err := h.svc.ListMachineTypes(c.Request.Context())
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: list machine types failed: %v", err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, views)
}

func (h *InternalHandler) CreateMachineType(c *gin.Context) {
	var req model.CreateMachineTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	view, err := h.svc.CreateMachineType(c.Request.Context(), &req, labelOperator(c))
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: create machine type failed: name=%s: %v", req.Name, err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, view)
}

func (h *InternalHandler) GetMachineType(c *gin.Context) {
	view, err := h.svc.GetMachineType(c.Request.Context(), c.Param("name"))
	if err != nil {
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, view)
}

func (h *InternalHandler) UpdateMachineType(c *gin.Context) {
	var req model.UpdateMachineTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	view, err := h.svc.UpdateMachineType(c.Request.Context(), c.Param("name"), &req, labelOperator(c))
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: update machine type failed: name=%s: %v", c.Param("name"), err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, view)
}

func (h *InternalHandler) DeleteMachineType(c *gin.Context) {
	unbind := strings.EqualFold(c.Query("unbind"), "true")
	if err := h.svc.DeleteMachineType(c.Request.Context(), c.Param("name"), unbind, labelOperator(c)); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: delete machine type failed: name=%s: %v", c.Param("name"), err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteNoContent(c)
}

func (h *InternalHandler) ReplaceMachineTypeNodes(c *gin.Context) {
	h.writeMachineTypeNodes(c, "replace")
}

func (h *InternalHandler) BindMachineTypeNodes(c *gin.Context) {
	h.writeMachineTypeNodes(c, "bind")
}

func (h *InternalHandler) UnbindMachineTypeNodes(c *gin.Context) {
	h.writeMachineTypeNodes(c, "unbind")
}

func (h *InternalHandler) writeMachineTypeNodes(c *gin.Context, op string) {
	var req model.MachineTypeNodesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	name := c.Param("name")
	operator := labelOperator(c)
	var (
		result *model.MachineTypeBindResult
		err    error
	)
	switch op {
	case "replace":
		if req.NodeIDs == nil {
			httputil.WriteError(c, http.StatusBadRequest, "nodeIDs is required; send [] to clear members")
			return
		}
		result, err = h.svc.ReplaceMachineTypeNodes(c.Request.Context(), name, req.NodeIDs, operator)
	case "unbind":
		result, err = h.svc.UnbindMachineTypeNodes(c.Request.Context(), name, req.NodeIDs, operator)
	default:
		result, err = h.svc.BindMachineTypeNodes(c.Request.Context(), name, req.NodeIDs, operator)
	}
	if err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: %s machine type nodes failed: name=%s: %v", op, name, err)
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, result)
}

func (h *InternalHandler) SetNodeMachineType(c *gin.Context) {
	var req model.SetNodeMachineTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.svc.SetNodeMachineType(c.Request.Context(), c.Param("nodeID"), req.MachineType, labelOperator(c)); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: set node machine type failed: node=%s: %v", c.Param("nodeID"), err)
		MapNodeError(c, err)
		return
	}
	snap, err := h.svc.GetNode(c.Request.Context(), c.Param("nodeID"))
	if err != nil {
		MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, snap)
}

func (h *InternalHandler) ClearNodeMachineType(c *gin.Context) {
	nodeID := c.Param("nodeID")
	if err := h.svc.ClearNodeMachineType(c.Request.Context(), nodeID, labelOperator(c)); err != nil {
		logging.G(c.Request.Context()).Errorf("nodemgmt-internal: clear node machine type failed: node=%s: %v", nodeID, err)
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
