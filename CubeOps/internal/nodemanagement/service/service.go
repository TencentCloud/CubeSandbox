// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

var (
	ErrNodeIDRequired          = errors.New("node_id is required")
	ErrSchedulingLabelRejected = errors.New("cubelet must not set scheduling-disabled label")
	ErrLabelsJSONCorrupt       = errors.New("node labels_json is corrupt")
	ErrDetailTooLong           = errors.New("operation detail too long")
)

const maxOperationDetailLen = 200

type DeclaredVersionInfo struct {
	Primary map[string]string
	Sets    map[string]map[string]struct{}
}

type NodeService struct {
	store               store.NodeStore
	hostMeta            store.HostMetaLoader
	declaredVersions    map[string]string
	declaredVersionSets map[string]map[string]struct{}

	sandboxCheckerFn SandboxInventoryChecker
}

func NewNodeService(s store.NodeStore, declared DeclaredVersionInfo) *NodeService {
	return NewNodeServiceWithHostMeta(s, nil, declared)
}

// NewNodeServiceWithHostMeta optionally injects a HostMetaLoader to recover
// the legacy static scheduler fields; when nil they stay empty.
func NewNodeServiceWithHostMeta(s store.NodeStore, hostMeta store.HostMetaLoader, declared DeclaredVersionInfo) *NodeService {
	if declared.Primary == nil {
		declared.Primary = map[string]string{}
	}
	if declared.Sets == nil {
		declared.Sets = map[string]map[string]struct{}{}
	}
	return &NodeService{
		store:               s,
		hostMeta:            hostMeta,
		declaredVersions:    declared.Primary,
		declaredVersionSets: declared.Sets,
	}
}

func (svc *NodeService) Init(ctx context.Context) error {
	return nil
}

func (svc *NodeService) RegisterNode(ctx context.Context, req *model.RegisterNodeRequest) (*model.NodeSnapshot, error) {
	if req == nil || req.NodeID == "" {
		return nil, ErrNodeIDRequired
	}
	if req.HostIP == "" {
		req.HostIP = req.NodeID
	}
	if _, ok := req.Labels[model.LabelSchedulingDisabled]; ok {
		logging.G(ctx).Warnf("nodemgmt: register rejected scheduling-disabled label: node=%s", req.NodeID)
		return nil, ErrSchedulingLabelRejected
	}

	reg := &store.NodeRegistration{
		NodeID:              req.NodeID,
		HostIP:              req.HostIP,
		GRPCPort:            req.GRPCPort,
		CapacityJSON:        model.MustJSON(req.Capacity),
		AllocatableJSON:     model.MustJSON(req.Allocatable),
		InstanceType:        req.InstanceType,
		ClusterLabel:        req.ClusterLabel,
		QuotaCPU:            req.QuotaCPU,
		QuotaMemMB:          req.QuotaMemMB,
		CreateConcurrentNum: req.CreateConcurrentNum,
		MaxMvmNum:           req.MaxMvmNum,
	}
	applyHostFactsToRegistration(reg, req.HostFacts)

	if err := svc.store.UpsertRegistration(ctx, reg); err != nil {
		logging.G(ctx).Errorf("nodemgmt: register upsert failed: node=%s: %v", req.NodeID, err)
		return nil, err
	}
	existing, err := svc.store.GetRegistration(ctx, req.NodeID)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: register re-fetch failed: node=%s: %v", req.NodeID, err)
		return nil, err
	}
	existingLabels, err := store.ParseLabelsJSON(existing.LabelsJSON)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: register labels corrupt: node=%s: %v", req.NodeID, err)
		return nil, fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}
	mergedLabels := StripAndPreserveSchedulingLabel(existingLabels, req.Labels)
	if err := ValidateLabelsSkippingReserved(mergedLabels); err != nil {
		logging.G(ctx).Warnf("nodemgmt: register labels invalid: node=%s: %v", req.NodeID, err)
		return nil, fmt.Errorf("register labels invalid: %w", err)
	}
	if CountUserLabels(mergedLabels) > maxLabelsPerNode {
		logging.G(ctx).Warnf("nodemgmt: register label count exceeded: node=%s got=%d max=%d", req.NodeID, CountUserLabels(mergedLabels), maxLabelsPerNode)
		return nil, fmt.Errorf("a node cannot have more than %d labels, got %d after merge", maxLabelsPerNode, CountUserLabels(mergedLabels))
	}
	if err := svc.store.UpdateLabels(ctx, req.NodeID, mergedLabels); err != nil {
		logging.G(ctx).Errorf("nodemgmt: register update labels failed: node=%s: %v", req.NodeID, err)
		return nil, err
	}

	snap := &model.NodeSnapshot{
		NodeID:              req.NodeID,
		HostIP:              req.HostIP,
		GRPCPort:            req.GRPCPort,
		Labels:              cloneStringMap(mergedLabels),
		Capacity:            req.Capacity,
		Allocatable:         req.Allocatable,
		InstanceType:        req.InstanceType,
		ClusterLabel:        req.ClusterLabel,
		QuotaCPU:            req.QuotaCPU,
		QuotaMemMB:          req.QuotaMemMB,
		CreateConcurrentNum: req.CreateConcurrentNum,
		MaxMvmNum:           req.MaxMvmNum,
		HostFacts:           cloneHostFacts(req.HostFacts),
		HeartbeatTime:       time.Now(),
	}
	applyCurrentHealth(snap, time.Now())
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)

	svc.persistVersions(ctx, req.NodeID, req.Versions, req.InventoryIncomplete, snap)
	nodemetric.WriteNodeSnapshot(snap)
	logging.G(ctx).Infof("nodemgmt: node registered: node=%s host=%s", req.NodeID, req.HostIP)
	return cloneSnapshot(snap), nil
}

func (svc *NodeService) UpdateNodeStatus(ctx context.Context, nodeID string, req *model.UpdateNodeStatusRequest) (*model.NodeSnapshot, error) {
	if nodeID == "" {
		return nil, ErrNodeIDRequired
	}
	if req == nil {
		req = &model.UpdateNodeStatusRequest{}
	}
	if req.HeartbeatTime.IsZero() {
		req.HeartbeatTime = time.Now()
	}

	if _, err := svc.store.GetRegistration(ctx, nodeID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		return nil, err
	}

	reportedReady := ReadyConditionTrue(req.Conditions)
	status := &store.NodeStatus{
		NodeID:             nodeID,
		ConditionsJSON:     model.MustJSON(req.Conditions),
		ImagesJSON:         model.MustJSON(req.Images),
		LocalTemplatesJSON: model.MustJSON(req.LocalTemplates),
		HeartbeatUnix:      req.HeartbeatTime.Unix(),
		Healthy:            reportedReady,
	}
	if err := svc.store.UpsertStatus(ctx, status); err != nil {
		logging.G(ctx).Errorf("nodemgmt: heartbeat upsert failed: node=%s: %v", nodeID, err)
		return nil, err
	}

	snap, err := svc.getNodeFromRedisOrDB(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	snap.Conditions = append([]model.NodeCondition(nil), req.Conditions...)
	snap.Images = append([]model.ContainerImage(nil), req.Images...)
	snap.LocalTemplates = append([]model.LocalTemplate(nil), req.LocalTemplates...)
	snap.HeartbeatTime = req.HeartbeatTime
	snap.ReportedReady = reportedReady
	applyCurrentHealth(snap, time.Now())
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)

	metricTime := req.MetricTime
	if metricTime.IsZero() {
		metricTime = req.HeartbeatTime
	}
	fanOutResourceMetric(ctx, nodeID, req, metricTime)

	if req.Allocated != nil || req.DiskUsage != nil {
		snap.MetricUpdate = metricTime
		snap.MetricLocalUpdateAt = time.Now()
		if a := req.Allocated; a != nil {
			snap.QuotaCpuUsage = a.MilliCPU
			snap.QuotaMemUsage = a.MemoryMB
			snap.MvmNum = a.MvmNum
			snap.NicQueues = a.NicQueues
		}
		if d := req.DiskUsage; d != nil {
			snap.DataDiskUsagePer = d.DataDiskUsagePer
			snap.StorageDiskUsagePer = d.StorageDiskUsagePer
			snap.SysDiskUsagePer = d.SysDiskUsagePer
		}
	}

	if req.HostFacts != nil && !req.HostFacts.IsZero() {
		merged := mergeIncomingHostFacts(snap.HostFacts, req.HostFacts)
		snap.HostFacts = merged
		reg := &store.NodeRegistration{NodeID: nodeID}
		applyHostFactsToRegistration(reg, merged)
		if err := svc.store.UpdateHostFacts(ctx, nodeID, reg.HostFactsJSON, reg.CPUIDHash, reg.HostKernelRelease); err != nil {
			logging.G(ctx).Warnf("nodemgmt: persist host facts failed: node=%s: %v", nodeID, err)
		}
	}

	svc.persistVersions(ctx, nodeID, req.Versions, req.InventoryIncomplete, snap)
	nodemetric.WriteNodeSnapshot(snap)
	return cloneSnapshot(snap), nil
}

func fanOutResourceMetric(ctx context.Context, nodeID string, req *model.UpdateNodeStatusRequest, metricTime time.Time) {
	if req == nil || (req.Allocated == nil && req.DiskUsage == nil) {
		return
	}
	if metricTime.IsZero() {
		metricTime = time.Now()
	}
	m := &nodemetric.NodeMetric{NodeID: nodeID, MetricTime: metricTime}
	if a := req.Allocated; a != nil {
		m.HasAllocated = true
		m.MilliCPUUsage = a.MilliCPU
		m.MemoryMBUsage = a.MemoryMB
		m.MvmNum = a.MvmNum
		m.NicQueues = a.NicQueues
	}
	if d := req.DiskUsage; d != nil {
		m.HasDisk = true
		m.DataDiskUsagePer = d.DataDiskUsagePer
		m.StorageDiskUsagePer = d.StorageDiskUsagePer
		m.SysDiskUsagePer = d.SysDiskUsagePer
	}
	if err := nodemetric.WriteNodeMetric(m); err != nil {
		logging.G(ctx).Warnf("nodemgmt: write node metric to redis failed: node=%s: %v", nodeID, err)
	}
}

func (svc *NodeService) GetNode(ctx context.Context, nodeID string) (*model.NodeSnapshot, error) {
	snap, err := svc.getNodeFromRedisOrDB(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	svc.applyHostMeta(ctx, []*model.NodeSnapshot{snap})
	return snap, nil
}

func (svc *NodeService) ListNodes(ctx context.Context) ([]*model.NodeSnapshot, error) {
	// DB is the authoritative node set; Redis snapshots are a fast-path
	// overlay that can go stale (TTL expiry), so the list must not depend
	// solely on what is currently in Redis.
	regs, err := svc.store.ListRegistrations(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]struct{}, len(regs))
	for i := range regs {
		byID[regs[i].NodeID] = struct{}{}
	}

	// Redis fast path for the status overlay.
	snaps, err := nodemetric.ScanNodeSnapshots()
	if err != nil {
		logging.G(ctx).Warnf("nodemgmt: scan redis snapshots failed, rebuilding from DB: %v", err)
	}
	now := time.Now()
	out := make([]*model.NodeSnapshot, 0, len(regs))
	seen := make(map[string]struct{}, len(snaps))
	for _, s := range snaps {
		if _, ok := byID[s.NodeID]; !ok {
			continue // stale Redis key for a node no longer registered
		}
		applyCurrentHealth(s, now)
		s.SchedulingDisabled = snapSchedulingDisabled(s)
		out = append(out, s)
		seen[s.NodeID] = struct{}{}
	}

	// Any DB-registered node missing from Redis is rebuilt from DB (which
	// also warms Redis), so the list always reflects the full node set.
	for i := range regs {
		if _, ok := seen[regs[i].NodeID]; ok {
			continue
		}
		snap, err := svc.getNodeFromRedisOrDB(ctx, regs[i].NodeID)
		if err != nil {
			logging.G(ctx).Warnf("nodemgmt: list nodes: skip node=%s: %v", regs[i].NodeID, err)
			continue
		}
		out = append(out, snap)
	}
	sortSnapshots(out)
	svc.applyHostMeta(ctx, out)
	return out, nil
}

func (svc *NodeService) ListSchedulerNodes(ctx context.Context) ([]*model.SchedulerNode, error) {
	snaps, err := svc.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*model.SchedulerNode, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, ToSchedulerNode(snap))
	}
	return out, nil
}

func (svc *NodeService) UpdateNodeLabels(ctx context.Context, nodeID string, labels map[string]string, operator string) error {
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	if err := ValidateLabels(labels); err != nil {
		return err
	}
	if _, ok := labels[model.LabelSchedulingDisabled]; ok {
		logging.G(ctx).Warnf("nodemgmt: set-labels rejected reserved label: node=%s operator=%s", nodeID, operator)
		return fmt.Errorf("cannot modify reserved label %q via label API", model.LabelSchedulingDisabled)
	}

	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: set-labels get registration failed: node=%s: %v", nodeID, err)
		return err
	}
	existing, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: set-labels labels corrupt: node=%s: %v", nodeID, err)
		return fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}
	for k, v := range labels {
		existing[k] = v
	}
	if CountUserLabels(existing) > maxLabelsPerNode {
		logging.G(ctx).Warnf("nodemgmt: set-labels count exceeded: node=%s got=%d max=%d", nodeID, CountUserLabels(existing), maxLabelsPerNode)
		return fmt.Errorf("a node cannot have more than %d labels, got %d after merge", maxLabelsPerNode, CountUserLabels(existing))
	}
	if err := svc.store.UpdateLabels(ctx, nodeID, existing); err != nil {
		logging.G(ctx).Errorf("nodemgmt: set-labels store failed: node=%s: %v", nodeID, err)
		return err
	}

	svc.updateSnapshotInRedis(ctx, nodeID, func(snap *model.NodeSnapshot) {
		snap.Labels = cloneStringMap(existing)
		snap.LabelsJSONCorrupt = false
		snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	})

	if err := svc.recordOperation(ctx, nodeID, model.OpSetLabels, operator, model.MustJSON(labels)); err != nil {
		logging.G(ctx).Warnf("nodemgmt: set-labels record operation failed: node=%s: %v", nodeID, err)
	}
	logging.G(ctx).Infof("nodemgmt: labels updated: node=%s operator=%s keys=%d", nodeID, operator, len(labels))
	return nil
}

func (svc *NodeService) DeleteNodeLabel(ctx context.Context, nodeID, key, operator string) error {
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	if err := ValidateLabelKey(key); err != nil {
		return err
	}
	if key == model.LabelSchedulingDisabled {
		logging.G(ctx).Warnf("nodemgmt: delete-label rejected reserved label: node=%s key=%s operator=%s", nodeID, key, operator)
		return fmt.Errorf("cannot delete reserved label %q via label API", model.LabelSchedulingDisabled)
	}

	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: delete-label get registration failed: node=%s: %v", nodeID, err)
		return err
	}
	existing, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: delete-label labels corrupt: node=%s: %v", nodeID, err)
		return fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}
	delete(existing, key)
	if err := svc.store.UpdateLabels(ctx, nodeID, existing); err != nil {
		logging.G(ctx).Errorf("nodemgmt: delete-label store failed: node=%s key=%s: %v", nodeID, key, err)
		return err
	}

	svc.updateSnapshotInRedis(ctx, nodeID, func(snap *model.NodeSnapshot) {
		snap.Labels = cloneStringMap(existing)
		snap.LabelsJSONCorrupt = false
		snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	})

	if err := svc.recordOperation(ctx, nodeID, model.OpDelLabel, operator, key); err != nil {
		logging.G(ctx).Warnf("nodemgmt: delete-label record operation failed: node=%s: %v", nodeID, err)
	}
	logging.G(ctx).Infof("nodemgmt: label deleted: node=%s key=%s operator=%s", nodeID, key, operator)
	return nil
}

func (svc *NodeService) SetNodeSchedulingDisabled(ctx context.Context, nodeID string, disabled bool, operator, detail string) (*model.NodeSnapshot, error) {
	if nodeID == "" {
		return nil, ErrNodeIDRequired
	}
	if utf8.RuneCountInString(detail) > maxOperationDetailLen {
		return nil, fmt.Errorf("%w: detail must be at most %d characters, got %d", ErrDetailTooLong, maxOperationDetailLen, utf8.RuneCountInString(detail))
	}

	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: isolation get registration failed: node=%s: %v", nodeID, err)
		return nil, err
	}
	existing, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: isolation labels corrupt: node=%s: %v", nodeID, err)
		return nil, fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}

	var changed bool
	_, has := existing[model.LabelSchedulingDisabled]
	switch {
	case disabled && (!has || existing[model.LabelSchedulingDisabled] != model.LabelSchedulingDisabledValue):
		existing[model.LabelSchedulingDisabled] = model.LabelSchedulingDisabledValue
		changed = true
	case !disabled && has:
		delete(existing, model.LabelSchedulingDisabled)
		changed = true
	}
	if changed {
		if err := svc.store.UpdateLabels(ctx, nodeID, existing); err != nil {
			logging.G(ctx).Errorf("nodemgmt: isolation store failed: node=%s disabled=%t: %v", nodeID, disabled, err)
			return nil, err
		}
	}

	var snap *model.NodeSnapshot
	svc.updateSnapshotInRedis(ctx, nodeID, func(s *model.NodeSnapshot) {
		s.Labels = cloneStringMap(existing)
		s.LabelsJSONCorrupt = false
		s.SchedulingDisabled = snapSchedulingDisabled(s)
		snap = cloneSnapshotWithCurrentHealth(s)
	})

	if changed {
		op := model.OpIsolate
		if !disabled {
			op = model.OpUnisolate
		}
		if detail == "" {
			detail = fmt.Sprintf("scheduling_disabled=%t", disabled)
		}
		if err := svc.recordOperation(ctx, nodeID, op, operator, detail); err != nil {
			logging.G(ctx).Warnf("nodemgmt: isolation record operation failed: node=%s: %v", nodeID, err)
		}
		logging.G(ctx).Infof("nodemgmt: scheduling toggled: node=%s disabled=%t operator=%s", nodeID, disabled, operator)
	}
	if snap == nil {
		snap, err = svc.getNodeFromRedisOrDB(ctx, nodeID)
		if err != nil {
			return nil, err
		}
	}
	return cloneSnapshotWithCurrentHealth(snap), nil
}

func (svc *NodeService) GetVersionMatrix(ctx context.Context) (*model.VersionMatrix, error) {
	snaps, err := svc.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	return BuildVersionMatrix(svc.declaredVersions, svc.declaredVersionSets, snaps), nil
}

func (svc *NodeService) ListOperations(ctx context.Context, nodeID string, limit int) ([]model.NodeOperation, error) {
	rows, err := svc.store.ListOperations(ctx, nodeID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.NodeOperation, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.NodeOperation{
			ID:        int64(r.ID),
			NodeID:    r.NodeID,
			Type:      r.Type,
			Operator:  r.Operator,
			Detail:    r.Detail,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func (svc *NodeService) persistVersions(ctx context.Context, nodeID string, versions []model.ComponentVersion, inventoryIncomplete bool, snap *model.NodeSnapshot) {
	if len(versions) == 0 {
		return
	}
	prevVersions := append([]model.ComponentVersion(nil), snap.Versions...)
	prevHash := snap.VersionsHash
	prevCompat := CompatRelevantVersions(snap.Versions)

	var h string
	var merged []model.ComponentVersion
	if inventoryIncomplete {
		merged = MergeComponentVersions(prevVersions, versions)
		h = VersionsHash(merged) + incompleteVersionsHashTag
	} else {
		h = VersionsHash(versions)
	}
	if prevHash == h {
		return
	}
	if err := svc.store.WriteComponentVersions(ctx, nodeID, versions, inventoryIncomplete); err != nil {
		logging.G(ctx).Warnf("nodemgmt: persist versions failed: node=%s: %v", nodeID, err)
		return
	}
	if inventoryIncomplete {
		snap.Versions = merged
	} else {
		snap.Versions = append([]model.ComponentVersion(nil), versions...)
	}
	snap.VersionsHash = h
	newCompat := CompatRelevantVersions(snap.Versions)
	if CompatVersionsChanged(prevCompat, newCompat) {
		if fn := GuestAgentVersionChanged; fn != nil {
			go fn(nodeID)
		}
	}
}

// GuestAgentVersionChanged is a hook for template compatibility management.
var GuestAgentVersionChanged func(nodeID string)

func (svc *NodeService) recordOperation(ctx context.Context, nodeID, opType, operator, detail string) error {
	return svc.store.CreateOperation(ctx, &store.NodeOperation{
		NodeID:   nodeID,
		Type:     opType,
		Operator: operator,
		Detail:   detail,
	})
}

// getNodeFromRedisOrDB reads a node snapshot from Redis; on miss it rebuilds
// from DB and warms Redis.
func (svc *NodeService) getNodeFromRedisOrDB(ctx context.Context, nodeID string) (*model.NodeSnapshot, error) {
	snap, err := nodemetric.ReadNodeSnapshot(nodeID)
	if err != nil {
		logging.G(ctx).Warnf("nodemgmt: redis read snapshot failed: node=%s: %v", nodeID, err)
	}
	if snap != nil {
		applyCurrentHealth(snap, time.Now())
		snap.SchedulingDisabled = snapSchedulingDisabled(snap)
		return snap, nil
	}
	// Redis miss: rebuild from DB.
	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	st, _ := svc.store.GetStatus(ctx, nodeID)
	versions, _ := svc.store.ListComponentVersionsByNode(ctx, nodeID)
	snap = buildSnapshotFromStore(reg, st, versions)
	nodemetric.WriteNodeSnapshot(snap)
	return snap, nil
}

// updateSnapshotInRedis reads the current snapshot, applies fn, and writes it back.
func (svc *NodeService) updateSnapshotInRedis(ctx context.Context, nodeID string, fn func(*model.NodeSnapshot)) {
	snap, err := svc.getNodeFromRedisOrDB(ctx, nodeID)
	if err != nil {
		logging.G(ctx).Warnf("nodemgmt: update snapshot: read failed: node=%s: %v", nodeID, err)
		return
	}
	fn(snap)
	nodemetric.WriteNodeSnapshot(snap)
}

func (svc *NodeService) LoadDeclaredVersions(declared DeclaredVersionInfo) {
	if declared.Primary == nil {
		declared.Primary = map[string]string{}
	}
	if declared.Sets == nil {
		declared.Sets = map[string]map[string]struct{}{}
	}
	svc.declaredVersions = declared.Primary
	svc.declaredVersionSets = declared.Sets
}

// applyHostMeta overlays the legacy static scheduler fields onto snapshots
// (three SELECTs total). Read failures degrade to a warning.
func (svc *NodeService) applyHostMeta(ctx context.Context, snaps []*model.NodeSnapshot) {
	if svc.hostMeta == nil || len(snaps) == 0 {
		return
	}
	hosts, err := svc.hostMeta.LoadHostMetas(ctx)
	if err != nil {
		logging.G(ctx).Warnf("nodemgmt: load host_info for scheduling fields failed: %v", err)
		hosts = nil
	}
	hostTypes, err := svc.hostMeta.LoadHostTypes(ctx)
	if err != nil {
		logging.G(ctx).Warnf("nodemgmt: load host_type for scheduling fields failed: %v", err)
		hostTypes = nil
	}
	subHosts, err := svc.hostMeta.LoadSubHostMetas(ctx)
	if err != nil {
		logging.G(ctx).Warnf("nodemgmt: load sub_host_info for scheduling fields failed: %v", err)
		subHosts = nil
	}
	for _, s := range snaps {
		if s == nil {
			continue
		}
		applyHostMetaToSnapshot(s, hosts, hostTypes, subHosts)
	}
}
