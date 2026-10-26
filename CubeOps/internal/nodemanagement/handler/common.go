// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

// NodeService is the subset of *service.NodeService used by handlers.
type NodeService interface {
	RegisterNode(ctx context.Context, req *model.RegisterNodeRequest) (*model.NodeSnapshot, error)
	UpdateNodeStatus(ctx context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error)
	GetNode(ctx context.Context, nodeID string) (*model.NodeSnapshot, error)
	ListNodes(ctx context.Context) ([]*model.NodeSnapshot, error)
	UpdateNodeLabels(ctx context.Context, nodeID string, labels map[string]string, operator string) error
	DeleteNodeLabel(ctx context.Context, nodeID, key, operator string) error
	SetNodeSchedulingDisabled(ctx context.Context, nodeID string, disabled bool, operator, detail string) (*model.NodeSnapshot, error)
	GetVersionMatrix(ctx context.Context) (*model.VersionMatrix, error)
	ListOperations(ctx context.Context, nodeID string, limit int) ([]model.NodeOperation, error)
	DeleteNode(ctx context.Context, nodeID string, force bool) (*model.NodeSnapshot, error)
}

var _ NodeService = (*service.NodeService)(nil)

func MapNodeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, service.ErrNodeNotFound):
		httputil.WriteError(c, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrNodeIDRequired):
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrSchedulingLabelRejected), errors.Is(err, service.ErrLabelsJSONCorrupt):
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrDetailTooLong):
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrNodeNotIsolated):
		// 409 Conflict: the node exists but is in the wrong state for deletion.
		httputil.WriteError(c, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrNodeHasSandboxes):
		// 409 Conflict: the node still holds workloads.
		httputil.WriteError(c, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrSandboxCheckFailed):
		// 502: CubeMaster unreachable; cannot verify the inventory safely.
		httputil.WriteError(c, http.StatusBadGateway, err.Error())
	case strings.Contains(err.Error(), "cannot have more than"):
		httputil.WriteError(c, http.StatusBadRequest, err.Error())
	case strings.Contains(err.Error(), "cannot modify reserved label"), strings.Contains(err.Error(), "cannot delete reserved label"):
		httputil.WriteError(c, http.StatusForbidden, err.Error())
	default:
		httputil.WriteError(c, http.StatusInternalServerError, err.Error())
	}
}

type NodeResourcesView struct {
	CpuMilli int64 `json:"cpuMilli"`
	MemoryMB int64 `json:"memoryMB"`
}

type NodeConditionView struct {
	Type              string  `json:"type"`
	Status            string  `json:"status"`
	LastHeartbeatTime *string `json:"lastHeartbeatTime"`
	Reason            string  `json:"reason"`
	Message           string  `json:"message"`
}

type ComponentVersionView struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	Source    string `json:"source"`
}

type NodeView struct {
	NodeID              string                 `json:"nodeID"`
	HostIP              string                 `json:"hostIP"`
	InstanceType        string                 `json:"instanceType"`
	Healthy             bool                   `json:"healthy"`
	UnhealthyReason     string                 `json:"unhealthyReason"`
	SchedulingDisabled  bool                   `json:"schedulingDisabled"`
	Capacity            NodeResourcesView      `json:"capacity"`
	Allocatable         NodeResourcesView      `json:"allocatable"`
	CpuSaturation       float32                `json:"cpuSaturation"`
	MemorySaturation    float32                `json:"memorySaturation"`
	MaxMvmSlots         int                    `json:"maxMvmSlots"`
	QuotaCpu            int64                  `json:"quotaCpu"`
	QuotaMemMB          int64                  `json:"quotaMemMB"`
	CreateConcurrentNum int                    `json:"createConcurrentNum"`
	HeartbeatTime       *string                `json:"heartbeatTime"`
	Conditions          []NodeConditionView    `json:"conditions"`
	LocalTemplates      []string               `json:"localTemplates"`
	Versions            []ComponentVersionView `json:"versions"`
}

type ClusterOverview struct {
	NodeCount           int   `json:"nodeCount"`
	HealthyNodes        int   `json:"healthyNodes"`
	TotalCpuMilli       int64 `json:"totalCpuMilli"`
	TotalMemoryMB       int64 `json:"totalMemoryMB"`
	AllocatableCpuMilli int64 `json:"allocatableCpuMilli"`
	AllocatableMemoryMB int64 `json:"allocatableMemoryMB"`
	MaxMvmSlots         int   `json:"maxMvmSlots"`
}

func ToNodeView(s *model.NodeSnapshot, usedMap map[string]struct{ CPUMilli, MemoryMB int64 }) NodeView {
	capCPU := s.Capacity.MilliCPU
	capMem := s.Capacity.MemoryMB

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

	conditions := make([]NodeConditionView, 0, len(s.Conditions))
	for _, c := range s.Conditions {
		conditions = append(conditions, NodeConditionView{
			Type:              c.Type,
			Status:            c.Status,
			LastHeartbeatTime: formatTimePtr(c.LastHeartbeatTime),
			Reason:            c.Reason,
			Message:           c.Message,
		})
	}

	localTemplates := make([]string, 0, len(s.LocalTemplates))
	for _, t := range s.LocalTemplates {
		if t.TemplateID != "" {
			localTemplates = append(localTemplates, t.TemplateID)
		}
	}

	versions := make([]ComponentVersionView, 0, len(s.Versions))
	for _, v := range s.Versions {
		versions = append(versions, ComponentVersionView{
			Component: v.Component,
			Version:   v.Version,
			Commit:    v.Commit,
			BuildTime: v.BuildTime,
			Source:    v.Source,
		})
	}

	return NodeView{
		NodeID:              s.NodeID,
		HostIP:              s.HostIP,
		InstanceType:        s.InstanceType,
		Healthy:             s.Healthy,
		UnhealthyReason:     s.UnhealthyReason,
		SchedulingDisabled:  s.SchedulingDisabled,
		Capacity:            NodeResourcesView{CpuMilli: capCPU, MemoryMB: capMem},
		Allocatable:         NodeResourcesView{CpuMilli: allocCPU, MemoryMB: allocMem},
		CpuSaturation:       saturationPct(capCPU, allocCPU),
		MemorySaturation:    saturationPct(capMem, allocMem),
		MaxMvmSlots:         int(s.MaxMvmNum),
		QuotaCpu:            s.QuotaCPU,
		QuotaMemMB:          s.QuotaMemMB,
		CreateConcurrentNum: int(s.CreateConcurrentNum),
		HeartbeatTime:       formatTimePtr(&s.HeartbeatTime),
		Conditions:          conditions,
		LocalTemplates:      localTemplates,
		Versions:            versions,
	}
}

func BuildOverview(nodes []*model.NodeSnapshot, usedMap map[string]struct{ CPUMilli, MemoryMB int64 }) ClusterOverview {
	o := ClusterOverview{NodeCount: len(nodes)}
	for _, n := range nodes {
		if n.Healthy {
			o.HealthyNodes++
		}
		o.TotalCpuMilli += n.Capacity.MilliCPU
		o.TotalMemoryMB += n.Capacity.MemoryMB
		o.MaxMvmSlots += int(n.MaxMvmNum)

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

func EmptyVersionMatrix() map[string]interface{} {
	return map[string]interface{}{
		"controlPlane": map[string]string{},
		"components":   []interface{}{},
		"nodes":        []interface{}{},
	}
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

func formatTimePtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
