// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	corev1 "k8s.io/api/core/v1"
)

type ResourceSnapshot struct {
	MilliCPU int64 `json:"milli_cpu,omitempty"`
	MemoryMB int64 `json:"memory_mb,omitempty"`
}

type ContainerImage struct {
	Names     []string `json:"names,omitempty"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	MediaType string   `json:"media_type,omitempty"`
}

type LocalTemplate struct {
	TemplateID string `json:"template_id,omitempty"`
	ID         string `json:"id,omitempty"`
	Media      string `json:"media,omitempty"`
	Path       string `json:"path,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
}

type RegisterNodeRequest struct {
	RequestID           string            `json:"requestID,omitempty"`
	NodeID              string            `json:"node_id,omitempty"`
	HostIP              string            `json:"host_ip,omitempty"`
	GRPCPort            int               `json:"grpc_port,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	Capacity            ResourceSnapshot  `json:"capacity,omitempty"`
	Allocatable         ResourceSnapshot  `json:"allocatable,omitempty"`
	InstanceType        string            `json:"instance_type,omitempty"`
	ClusterLabel        string            `json:"cluster_label,omitempty"`
	QuotaCPU            int64             `json:"quota_cpu,omitempty"`
	QuotaMemMB          int64             `json:"quota_mem_mb,omitempty"`
	CreateConcurrentNum int64             `json:"create_concurrent_num,omitempty"`
	MaxMvmNum           int64             `json:"max_mvm_num,omitempty"`
}

type UpdateNodeStatusRequest struct {
	RequestID      string                 `json:"requestID,omitempty"`
	Conditions     []corev1.NodeCondition `json:"conditions,omitempty"`
	Images         []ContainerImage       `json:"images,omitempty"`
	LocalTemplates []LocalTemplate        `json:"local_templates,omitempty"`
	HeartbeatTime  time.Time              `json:"heartbeat_time,omitempty"`
}

type NodeSnapshot struct {
	NodeID              string                 `json:"node_id,omitempty"`
	HostIP              string                 `json:"host_ip,omitempty"`
	GRPCPort            int                    `json:"grpc_port,omitempty"`
	Labels              map[string]string      `json:"labels,omitempty"`
	Capacity            ResourceSnapshot       `json:"capacity,omitempty"`
	Allocatable         ResourceSnapshot       `json:"allocatable,omitempty"`
	InstanceType        string                 `json:"instance_type,omitempty"`
	ClusterLabel        string                 `json:"cluster_label,omitempty"`
	QuotaCPU            int64                  `json:"quota_cpu,omitempty"`
	QuotaMemMB          int64                  `json:"quota_mem_mb,omitempty"`
	CreateConcurrentNum int64                  `json:"create_concurrent_num,omitempty"`
	MaxMvmNum           int64                  `json:"max_mvm_num,omitempty"`
	Conditions          []corev1.NodeCondition `json:"conditions,omitempty"`
	Images              []ContainerImage       `json:"images,omitempty"`
	LocalTemplates      []LocalTemplate        `json:"local_templates,omitempty"`
	HeartbeatTime       time.Time              `json:"heartbeat_time,omitempty"`
	Healthy             bool                   `json:"healthy,omitempty"`
}

type service struct {
	db    *gorm.DB
	mu    sync.RWMutex
	ready bool
	nodes map[string]*NodeSnapshot
}

var global = &service{
	nodes: make(map[string]*NodeSnapshot),
}

func Init(ctx context.Context) error {
	_ = ctx
	// Schema is owned by pkg/base/dao/migrate and applied at startup
	// before any package Init runs.
	global.db = db.Init(config.GetDbConfig())
	if err := global.reload(); err != nil {
		return err
	}
	localcache.RegisterNodeLoader(ListSchedulerNodes)
	global.ready = true
	return nil
}

func Ready() bool {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.ready
}

func RegisterNode(ctx context.Context, req *RegisterNodeRequest) (*NodeSnapshot, error) {
	_ = ctx
	if req == nil || req.NodeID == "" {
		return nil, fmt.Errorf("node_id is required")
	}
	if req.HostIP == "" {
		req.HostIP = req.NodeID
	}
	reg := &models.NodeRegistration{
		NodeID:              req.NodeID,
		HostIP:              req.HostIP,
		GRPCPort:            req.GRPCPort,
		LabelsJSON:          mustJSON(req.Labels),
		CapacityJSON:        mustJSON(req.Capacity),
		AllocatableJSON:     mustJSON(req.Allocatable),
		InstanceType:        req.InstanceType,
		ClusterLabel:        req.ClusterLabel,
		QuotaCPU:            req.QuotaCPU,
		QuotaMemMB:          req.QuotaMemMB,
		CreateConcurrentNum: req.CreateConcurrentNum,
		MaxMvmNum:           req.MaxMvmNum,
	}
	if err := global.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"host_ip", "grpc_port", "labels_json", "capacity_json", "allocatable_json",
			"instance_type", "cluster_label", "quota_cpu", "quota_mem_mb",
			"create_concurrent_num", "max_mvm_num", "updated_at",
		}),
	}).Create(reg).Error; err != nil {
		return nil, err
	}

	snap := global.ensureNode(req.NodeID)
	global.mu.Lock()
	snap.NodeID = req.NodeID
	snap.HostIP = req.HostIP
	snap.GRPCPort = req.GRPCPort
	snap.Labels = cloneStringMap(req.Labels)
	snap.Capacity = req.Capacity
	snap.Allocatable = req.Allocatable
	snap.InstanceType = req.InstanceType
	snap.ClusterLabel = req.ClusterLabel
	snap.QuotaCPU = req.QuotaCPU
	snap.QuotaMemMB = req.QuotaMemMB
	snap.CreateConcurrentNum = req.CreateConcurrentNum
	snap.MaxMvmNum = req.MaxMvmNum
	global.mu.Unlock()
	syncLocalcache(snap)
	return cloneSnapshot(snap), nil
}

func UpdateNodeStatus(ctx context.Context, nodeID string, req *UpdateNodeStatusRequest) (*NodeSnapshot, error) {
	_ = ctx
	if nodeID == "" {
		return nil, fmt.Errorf("node_id is required")
	}
	if req == nil {
		req = &UpdateNodeStatusRequest{}
	}
	if req.HeartbeatTime.IsZero() {
		req.HeartbeatTime = time.Now()
	}
	status := &models.NodeStatus{
		NodeID:             nodeID,
		ConditionsJSON:     mustJSON(req.Conditions),
		ImagesJSON:         mustJSON(req.Images),
		LocalTemplatesJSON: mustJSON(req.LocalTemplates),
		HeartbeatUnix:      req.HeartbeatTime.Unix(),
		Healthy:            isHealthy(req.Conditions),
	}
	if err := global.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"conditions_json", "images_json", "local_templates_json",
			"heartbeat_unix", "healthy", "updated_at",
		}),
	}).Create(status).Error; err != nil {
		return nil, err
	}

	snap := global.ensureNode(nodeID)
	global.mu.Lock()
	snap.NodeID = nodeID
	snap.Conditions = append([]corev1.NodeCondition(nil), req.Conditions...)
	snap.Images = append([]ContainerImage(nil), req.Images...)
	snap.LocalTemplates = append([]LocalTemplate(nil), req.LocalTemplates...)
	snap.HeartbeatTime = req.HeartbeatTime
	snap.Healthy = status.Healthy
	global.mu.Unlock()
	syncLocalcache(snap)
	return cloneSnapshot(snap), nil
}

func GetNode(ctx context.Context, nodeID string) (*NodeSnapshot, error) {
	_ = ctx
	global.mu.RLock()
	defer global.mu.RUnlock()
	snap, ok := global.nodes[nodeID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return cloneSnapshot(snap), nil
}

func ListNodes(ctx context.Context) ([]*NodeSnapshot, error) {
	_ = ctx
	global.mu.RLock()
	defer global.mu.RUnlock()
	out := make([]*NodeSnapshot, 0, len(global.nodes))
	for _, snap := range global.nodes {
		out = append(out, cloneSnapshot(snap))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func ListSchedulerNodes(ctx context.Context) ([]*node.Node, error) {
	snaps, err := ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*node.Node, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, toSchedulerNode(snap))
	}
	return out, nil
}

func (s *service) ensureNode(nodeID string) *NodeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.nodes[nodeID]; ok {
		return snap
	}
	snap := &NodeSnapshot{NodeID: nodeID}
	s.nodes[nodeID] = snap
	return snap
}

func (s *service) reload() error {
	regs := make([]*models.NodeRegistration, 0)
	if err := s.db.Table(constants.NodeMetaRegistrationTable).Find(&regs).Error; err != nil {
		return err
	}
	statuses := make([]*models.NodeStatus, 0)
	if err := s.db.Table(constants.NodeMetaStatusTable).Find(&statuses).Error; err != nil {
		return err
	}
	next := make(map[string]*NodeSnapshot, len(regs))
	for _, reg := range regs {
		snap := &NodeSnapshot{
			NodeID:              reg.NodeID,
			HostIP:              reg.HostIP,
			GRPCPort:            reg.GRPCPort,
			Labels:              map[string]string{},
			Capacity:            ResourceSnapshot{},
			Allocatable:         ResourceSnapshot{},
			InstanceType:        reg.InstanceType,
			ClusterLabel:        reg.ClusterLabel,
			QuotaCPU:            reg.QuotaCPU,
			QuotaMemMB:          reg.QuotaMemMB,
			CreateConcurrentNum: reg.CreateConcurrentNum,
			MaxMvmNum:           reg.MaxMvmNum,
		}
		_ = json.Unmarshal([]byte(reg.LabelsJSON), &snap.Labels)
		_ = json.Unmarshal([]byte(reg.CapacityJSON), &snap.Capacity)
		_ = json.Unmarshal([]byte(reg.AllocatableJSON), &snap.Allocatable)
		next[reg.NodeID] = snap
	}
	for _, st := range statuses {
		snap, ok := next[st.NodeID]
		if !ok {
			snap = &NodeSnapshot{NodeID: st.NodeID}
			next[st.NodeID] = snap
		}
		_ = json.Unmarshal([]byte(st.ConditionsJSON), &snap.Conditions)
		_ = json.Unmarshal([]byte(st.ImagesJSON), &snap.Images)
		_ = json.Unmarshal([]byte(st.LocalTemplatesJSON), &snap.LocalTemplates)
		snap.HeartbeatTime = time.Unix(st.HeartbeatUnix, 0)
		snap.Healthy = st.Healthy
	}
	s.mu.Lock()
	s.nodes = next
	s.mu.Unlock()
	return nil
}

func isHealthy(conditions []corev1.NodeCondition) bool {
	for _, cond := range conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func toSchedulerNode(snap *NodeSnapshot) *node.Node {
	if snap == nil {
		return nil
	}
	quotaCPU := snap.QuotaCPU
	if quotaCPU == 0 {
		quotaCPU = snap.Allocatable.MilliCPU
	}
	quotaMem := snap.QuotaMemMB
	if quotaMem == 0 {
		quotaMem = snap.Allocatable.MemoryMB
	}
	hostIP := snap.HostIP
	if hostIP == "" {
		hostIP = snap.NodeID
	}
	instanceType := snap.InstanceType
	if instanceType == "" {
		instanceType = constants.DefaultInstanceTypeName
	}
	return &node.Node{
		InsID:               snap.NodeID,
		UUID:                snap.NodeID,
		IP:                  hostIP,
		CpuTotal:            int(snap.Capacity.MilliCPU / 1000),
		MemMBTotal:          snap.Capacity.MemoryMB,
		QuotaCpu:            quotaCPU,
		QuotaMem:            quotaMem,
		ClusterLabel:        snap.ClusterLabel,
		OssClusterLabel:     snap.ClusterLabel,
		InstanceType:        instanceType,
		HostStatus:          constants.HostStatusRunning,
		Healthy:             snap.Healthy,
		CreateConcurrentNum: snap.CreateConcurrentNum,
		MaxMvmLimit:         snap.MaxMvmNum,
		MetaDataUpdateAt:    snap.HeartbeatTime,
		MetricLocalUpdateAt: snap.HeartbeatTime,
		MetricUpdate:        snap.HeartbeatTime,
	}
}

func syncLocalcache(snap *NodeSnapshot) {
	localcache.UpsertNode(toSchedulerNode(snap))
	localcache.SyncNodeTemplates(snap.NodeID, templateIDsFromLocalTemplates(snap.LocalTemplates))
}

func templateIDsFromLocalTemplates(localTemplates []LocalTemplate) []string {
	if len(localTemplates) == 0 {
		return nil
	}
	templateIDs := make([]string, 0, len(localTemplates))
	for _, localTemplate := range localTemplates {
		if localTemplate.TemplateID == "" {
			continue
		}
		templateIDs = append(templateIDs, localTemplate.TemplateID)
	}
	return templateIDs
}

func cloneSnapshot(in *NodeSnapshot) *NodeSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Labels = cloneStringMap(in.Labels)
	out.Conditions = append([]corev1.NodeCondition(nil), in.Conditions...)
	out.Images = append([]ContainerImage(nil), in.Images...)
	out.LocalTemplates = append([]LocalTemplate(nil), in.LocalTemplates...)
	return &out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mustJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}
