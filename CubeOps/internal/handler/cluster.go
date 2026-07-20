// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

// ClusterHandler handles cluster-related HTTP requests.
type ClusterHandler struct {
	cm CubeMasterClient
}

// NewClusterHandler creates a new cluster handler.
func NewClusterHandler(cm CubeMasterClient) *ClusterHandler { return &ClusterHandler{cm: cm} }

// Register installs the cluster routes on the given router group.
func (h *ClusterHandler) Register(r *gin.RouterGroup) {
	r.GET("/cluster/overview", h.Overview)
	r.GET("/cluster/versions", h.Versions)
	r.GET("/nodes", h.ListNodes)
	r.GET("/nodes/:nodeID", h.GetNode)
}

// --- Response types matching the frontend's expected format ---

type nodeResourcesView struct {
	CpuMilli int64 `json:"cpuMilli"`
	MemoryMB int64 `json:"memoryMB"`
}

type nodeConditionView struct {
	Type              string  `json:"type"`
	Status            string  `json:"status"`
	LastHeartbeatTime *string `json:"lastHeartbeatTime"`
	Reason            string  `json:"reason"`
	Message           string  `json:"message"`
}

type componentVersionView struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	Source    string `json:"source"`
}

type nodeView struct {
	NodeID              string                 `json:"nodeID"`
	HostIP              string                 `json:"hostIP"`
	InstanceType        string                 `json:"instanceType"`
	Healthy             bool                   `json:"healthy"`
	Capacity            nodeResourcesView      `json:"capacity"`
	Allocatable         nodeResourcesView      `json:"allocatable"`
	CpuSaturation       float32                `json:"cpuSaturation"`
	MemorySaturation    float32                `json:"memorySaturation"`
	MaxMvmSlots         int                    `json:"maxMvmSlots"`
	QuotaCpu            int64                  `json:"quotaCpu"`
	QuotaMemMB          int64                  `json:"quotaMemMB"`
	CreateConcurrentNum int                    `json:"createConcurrentNum"`
	HeartbeatTime       *string                `json:"heartbeatTime"`
	Conditions          []nodeConditionView    `json:"conditions"`
	LocalTemplates      []string               `json:"localTemplates"`
	Versions            []componentVersionView `json:"versions"`
}

type clusterOverview struct {
	NodeCount           int   `json:"nodeCount"`
	HealthyNodes        int   `json:"healthyNodes"`
	TotalCpuMilli       int64 `json:"totalCpuMilli"`
	TotalMemoryMB       int64 `json:"totalMemoryMB"`
	AllocatableCpuMilli int64 `json:"allocatableCpuMilli"`
	AllocatableMemoryMB int64 `json:"allocatableMemoryMB"`
	MaxMvmSlots         int   `json:"maxMvmSlots"`
}

// --- CubeMaster raw response types (snake_case) ---

type cmNodeResources struct {
	MilliCPU int64 `json:"milli_cpu"`
	MemoryMB int64 `json:"memory_mb"`
}

type cmNodeCondition struct {
	Type              string  `json:"type"`
	Status            string  `json:"status"`
	LastHeartbeatTime *string `json:"lastHeartbeatTime"`
	Reason            string  `json:"reason"`
	Message           string  `json:"message"`
}

type cmComponentVersion struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Source    string `json:"source"`
}

type cmLocalTemplate struct {
	TemplateID string `json:"template_id"`
}

type cmNodeSnapshot struct {
	NodeID              string               `json:"node_id"`
	HostIP              string               `json:"host_ip"`
	InstanceType        string               `json:"instance_type"`
	Healthy             bool                 `json:"healthy"`
	Capacity            cmNodeResources      `json:"capacity"`
	Allocatable         cmNodeResources      `json:"allocatable"`
	MaxMvmNum           int                  `json:"max_mvm_num"`
	QuotaCPU            int64                `json:"quota_cpu"`
	QuotaMemMB          int64                `json:"quota_mem_mb"`
	CreateConcurrentNum int                  `json:"create_concurrent_num"`
	HeartbeatTime       *string              `json:"heartbeat_time"`
	Conditions          []cmNodeCondition    `json:"conditions"`
	LocalTemplates      []cmLocalTemplate    `json:"local_templates"`
	Versions            []cmComponentVersion `json:"versions"`
}

type cmResponse struct {
	Ret  *json.RawMessage `json:"ret,omitempty"`
	Data json.RawMessage  `json:"data"`
}

type cmNodesResponse struct {
	Data []cmNodeSnapshot `json:"data"`
}

type cmNodeResponse struct {
	Data *cmNodeSnapshot `json:"data"`
}

// --- CubeMaster sandbox list types (for actual used resources) ---

type cmSandboxItem struct {
	HostIP   string `json:"host_ip"`
	Status   int    `json:"status"`
	CPUCount int    `json:"cpu_count"`
	MemoryMB int    `json:"memory_mb"`
}

type cmSandboxListResponse struct {
	Data []cmSandboxItem `json:"data"`
}

// fetchUsedResources lists all running sandboxes and aggregates
// cpu_milli / memory_mb used per host IP. Matches old Rust fetch_used_resources.
// On any error the map is empty and saturation falls back to CubeMaster values.
func (h *ClusterHandler) fetchUsedResources(ctx context.Context) map[string]struct {
	CPUMilli int64
	MemoryMB int64
} {
	data, err := h.cm.ListSandboxes(ctx)
	if err != nil {
		return map[string]struct {
			CPUMilli int64
			MemoryMB int64
		}{}
	}
	var resp cmSandboxListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
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
		// status: 1 = running (CubeMaster integer status codes)
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

// --- Handlers ---

// Overview handles GET /cluster/overview.
func (h *ClusterHandler) Overview(c *gin.Context) {
	data, err := h.cm.GetNodes(c.Request.Context())
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to fetch cluster overview: "+err.Error())
		return
	}
	var resp cmNodesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to parse nodes response")
		return
	}
	used := h.fetchUsedResources(c.Request.Context())
	overview := buildOverview(resp.Data, used)
	httputil.WriteJSON(c, http.StatusOK, overview)
}

// ListNodes handles GET /nodes.
func (h *ClusterHandler) ListNodes(c *gin.Context) {
	data, err := h.cm.GetNodes(c.Request.Context())
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to fetch nodes: "+err.Error())
		return
	}
	var resp cmNodesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to parse nodes response")
		return
	}
	used := h.fetchUsedResources(c.Request.Context())
	views := make([]nodeView, 0, len(resp.Data))
	for _, s := range resp.Data {
		views = append(views, toNodeView(s, used))
	}
	httputil.WriteJSON(c, http.StatusOK, views)
}

// GetNode handles GET /nodes/{nodeID}.
func (h *ClusterHandler) GetNode(c *gin.Context) {
	nodeID := c.Param("nodeID")
	if nodeID == "" {
		httputil.WriteError(c, http.StatusBadRequest, "nodeID is required")
		return
	}
	data, err := h.cm.GetNode(c.Request.Context(), nodeID)
	if err != nil {
		httputil.WriteError(c, http.StatusBadGateway, "failed to fetch node: "+err.Error())
		return
	}
	var resp cmNodeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		httputil.WriteError(c, http.StatusInternalServerError, "failed to parse node response")
		return
	}
	if resp.Data == nil {
		httputil.WriteError(c, http.StatusNotFound, fmt.Sprintf("node %s not found", nodeID))
		return
	}
	used := h.fetchUsedResources(c.Request.Context())
	httputil.WriteJSON(c, http.StatusOK, toNodeView(*resp.Data, used))
}

// Versions handles GET /cluster/versions.
//
// Empty/missing CubeMaster data returns an empty shell for the UI. Otherwise
// keys are rewritten with camelCaseJSON (same path as writeSDKResponse).
func (h *ClusterHandler) Versions(c *gin.Context) {
	empty := map[string]interface{}{
		"controlPlane": map[string]string{},
		"components":   []interface{}{},
		"nodes":        []interface{}{},
	}
	data, err := h.cm.ClusterVersions(c.Request.Context())
	if err != nil {
		httputil.WriteJSON(c, http.StatusOK, empty)
		return
	}
	var resp cmResponse
	if err := json.Unmarshal(data, &resp); err != nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		httputil.WriteJSON(c, http.StatusOK, empty)
		return
	}
	httputil.WriteRawJSON(c, http.StatusOK, camelCaseJSON(resp.Data))
}

// --- Transformation helpers ---

func toNodeView(s cmNodeSnapshot, usedMap map[string]struct {
	CPUMilli int64
	MemoryMB int64
}) nodeView {
	capCPU := s.Capacity.MilliCPU
	capMem := s.Capacity.MemoryMB

	// Use sandbox-aggregated usage if available; fall back to CubeMaster allocatable diff.
	var usedCPU, usedMem int64
	if u, ok := usedMap[s.HostIP]; ok {
		usedCPU = u.CPUMilli
		usedMem = u.MemoryMB
	} else {
		usedCPU = capCPU - s.Allocatable.MilliCPU
		if usedCPU < 0 {
			usedCPU = 0
		}
		usedMem = capMem - s.Allocatable.MemoryMB
		if usedMem < 0 {
			usedMem = 0
		}
	}

	allocCPU := capCPU - usedCPU
	if allocCPU < 0 {
		allocCPU = 0
	}
	allocMem := capMem - usedMem
	if allocMem < 0 {
		allocMem = 0
	}

	conditions := make([]nodeConditionView, 0, len(s.Conditions))
	for _, c := range s.Conditions {
		conditions = append(conditions, nodeConditionView{
			Type:              c.Type,
			Status:            c.Status,
			LastHeartbeatTime: c.LastHeartbeatTime,
			Reason:            c.Reason,
			Message:           c.Message,
		})
	}

	localTemplates := make([]string, 0, len(s.LocalTemplates))
	for _, t := range s.LocalTemplates {
		localTemplates = append(localTemplates, t.TemplateID)
	}

	versions := make([]componentVersionView, 0, len(s.Versions))
	for _, v := range s.Versions {
		versions = append(versions, componentVersionView{
			Component: v.Component,
			Version:   v.Version,
			Commit:    v.Commit,
			BuildTime: v.BuildTime,
			Source:    v.Source,
		})
	}

	return nodeView{
		NodeID:              s.NodeID,
		HostIP:              s.HostIP,
		InstanceType:        s.InstanceType,
		Healthy:             s.Healthy,
		Capacity:            nodeResourcesView{CpuMilli: capCPU, MemoryMB: capMem},
		Allocatable:         nodeResourcesView{CpuMilli: allocCPU, MemoryMB: allocMem},
		CpuSaturation:       saturationPct(capCPU, allocCPU),
		MemorySaturation:    saturationPct(capMem, allocMem),
		MaxMvmSlots:         s.MaxMvmNum,
		QuotaCpu:            s.QuotaCPU,
		QuotaMemMB:          s.QuotaMemMB,
		CreateConcurrentNum: s.CreateConcurrentNum,
		HeartbeatTime:       s.HeartbeatTime,
		Conditions:          conditions,
		LocalTemplates:      localTemplates,
		Versions:            versions,
	}
}

func buildOverview(nodes []cmNodeSnapshot, usedMap map[string]struct {
	CPUMilli int64
	MemoryMB int64
}) clusterOverview {
	o := clusterOverview{NodeCount: len(nodes)}
	for _, n := range nodes {
		if n.Healthy {
			o.HealthyNodes++
		}
		o.TotalCpuMilli += n.Capacity.MilliCPU
		o.TotalMemoryMB += n.Capacity.MemoryMB
		o.MaxMvmSlots += n.MaxMvmNum

		// Use sandbox-aggregated used resources if available; fall back to CubeMaster allocatable.
		if u, ok := usedMap[n.HostIP]; ok {
			allocCPU := n.Capacity.MilliCPU - u.CPUMilli
			if allocCPU < 0 {
				allocCPU = 0
			}
			allocMem := n.Capacity.MemoryMB - u.MemoryMB
			if allocMem < 0 {
				allocMem = 0
			}
			o.AllocatableCpuMilli += allocCPU
			o.AllocatableMemoryMB += allocMem
		} else {
			o.AllocatableCpuMilli += n.Allocatable.MilliCPU
			o.AllocatableMemoryMB += n.Allocatable.MemoryMB
		}
	}
	return o
}

func saturationPct(total, allocatable int64) float32 {
	if total <= 0 {
		return 0
	}
	used := total - allocatable
	if used < 0 {
		used = 0
	}
	pct := float32(used) / float32(total) * 100.0
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
