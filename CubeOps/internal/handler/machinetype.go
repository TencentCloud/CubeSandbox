// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	nmhandler "github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

func (h *ClusterHandler) registerMachineTypes(r *gin.RouterGroup) {
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

func (h *ClusterHandler) ListMachineTypes(c *gin.Context) {
	views, err := h.svc.ListMachineTypes(c.Request.Context())
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: list machine types failed: %v", err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, views)
}

func (h *ClusterHandler) CreateMachineType(c *gin.Context) {
	var req model.CreateMachineTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	view, err := h.svc.CreateMachineType(c.Request.Context(), &req, auth.UsernameFromContext(c.Request.Context()))
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: create machine type failed: name=%s: %v", req.Name, err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, view)
}

func (h *ClusterHandler) GetMachineType(c *gin.Context) {
	view, err := h.svc.GetMachineType(c.Request.Context(), c.Param("name"))
	if err != nil {
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, view)
}

func (h *ClusterHandler) UpdateMachineType(c *gin.Context) {
	var req model.UpdateMachineTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	view, err := h.svc.UpdateMachineType(c.Request.Context(), c.Param("name"), &req, auth.UsernameFromContext(c.Request.Context()))
	if err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: update machine type failed: name=%s: %v", c.Param("name"), err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, view)
}

func (h *ClusterHandler) DeleteMachineType(c *gin.Context) {
	unbind := strings.EqualFold(c.Query("unbind"), "true")
	if err := h.svc.DeleteMachineType(c.Request.Context(), c.Param("name"), unbind, auth.UsernameFromContext(c.Request.Context())); err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: delete machine type failed: name=%s: %v", c.Param("name"), err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteNoContent(c)
}

func (h *ClusterHandler) ReplaceMachineTypeNodes(c *gin.Context) {
	h.writeMachineTypeNodes(c, "replace")
}

func (h *ClusterHandler) BindMachineTypeNodes(c *gin.Context) {
	h.writeMachineTypeNodes(c, "bind")
}

func (h *ClusterHandler) UnbindMachineTypeNodes(c *gin.Context) {
	h.writeMachineTypeNodes(c, "unbind")
}

func (h *ClusterHandler) writeMachineTypeNodes(c *gin.Context, op string) {
	var req model.MachineTypeNodesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	name := c.Param("name")
	operator := auth.UsernameFromContext(c.Request.Context())
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
		logging.G(c.Request.Context()).Errorf("cluster: %s machine type nodes failed: name=%s: %v", op, name, err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, result)
}

func (h *ClusterHandler) SetNodeMachineType(c *gin.Context) {
	var req model.SetNodeMachineTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.svc.SetNodeMachineType(c.Request.Context(), c.Param("nodeID"), req.MachineType, auth.UsernameFromContext(c.Request.Context())); err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: set node machine type failed: node=%s: %v", c.Param("nodeID"), err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteNoContent(c)
}

func (h *ClusterHandler) ClearNodeMachineType(c *gin.Context) {
	if err := h.svc.ClearNodeMachineType(c.Request.Context(), c.Param("nodeID"), auth.UsernameFromContext(c.Request.Context())); err != nil {
		logging.G(c.Request.Context()).Errorf("cluster: clear node machine type failed: node=%s: %v", c.Param("nodeID"), err)
		nmhandler.MapNodeError(c, err)
		return
	}
	httputil.WriteNoContent(c)
}
