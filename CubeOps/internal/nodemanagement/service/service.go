// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	declaredVersions    map[string]string
	declaredVersionSets map[string]map[string]struct{}

	mu    sync.RWMutex
	nodes map[string]*model.NodeSnapshot
	ready bool

	versionWriteLocks sync.Map
	labelWriteLocks   sync.Map

	// sandboxCheckerFn is the per-service SandboxInventoryChecker (set via
	// SetSandboxInventoryChecker); nil falls back to the package default.
	sandboxCheckerFn SandboxInventoryChecker
}

func NewNodeService(s store.NodeStore, declared DeclaredVersionInfo) *NodeService {
	if declared.Primary == nil {
		declared.Primary = map[string]string{}
	}
	if declared.Sets == nil {
		declared.Sets = map[string]map[string]struct{}{}
	}
	return &NodeService{
		store:               s,
		declaredVersions:    declared.Primary,
		declaredVersionSets: declared.Sets,
		nodes:               map[string]*model.NodeSnapshot{},
	}
}

func (svc *NodeService) Ready() bool {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return svc.ready
}

func (svc *NodeService) Init(ctx context.Context) error {
	if err := svc.reload(ctx); err != nil {
		logging.G(ctx).Errorf("nodemgmt: init reload failed: %v", err)
		return err
	}
	svc.mu.Lock()
	count := len(svc.nodes)
	svc.ready = true
	svc.mu.Unlock()
	logging.G(ctx).Infof("nodemgmt: service ready, loaded %d node(s)", count)
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

	unlock := svc.lockNodeLabels(req.NodeID)
	defer unlock()

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
	if err := ValidateLabels(mergedLabels); err != nil {
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

	snap := svc.ensureNode(req.NodeID)
	svc.mu.Lock()
	snap.HostIP = req.HostIP
	snap.GRPCPort = req.GRPCPort
	snap.Labels = cloneStringMap(mergedLabels)
	snap.LabelsJSONCorrupt = false
	snap.Capacity = req.Capacity
	snap.Allocatable = req.Allocatable
	snap.InstanceType = req.InstanceType
	snap.ClusterLabel = req.ClusterLabel
	snap.QuotaCPU = req.QuotaCPU
	snap.QuotaMemMB = req.QuotaMemMB
	snap.CreateConcurrentNum = req.CreateConcurrentNum
	snap.MaxMvmNum = req.MaxMvmNum
	snap.HostFacts = cloneHostFacts(req.HostFacts)
	snap.HeartbeatTime = time.Now()
	applyCurrentHealth(snap, time.Now())
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	svc.mu.Unlock()

	svc.persistVersions(ctx, req.NodeID, req.Versions, req.InventoryIncomplete)
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
	// Hold the per-node lock so a concurrent DeleteNode cannot remove the
	// registration mid-heartbeat and let the status upsert revive the node.
	unlock := svc.lockNodeLabels(nodeID)
	defer unlock()

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

	snap := svc.ensureNode(nodeID)
	svc.mu.Lock()
	snap.Conditions = append([]model.NodeCondition(nil), req.Conditions...)
	snap.Images = append([]model.ContainerImage(nil), req.Images...)
	snap.LocalTemplates = append([]model.LocalTemplate(nil), req.LocalTemplates...)
	snap.HeartbeatTime = req.HeartbeatTime
	snap.ReportedReady = reportedReady
	applyCurrentHealth(snap, time.Now())
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	svc.mu.Unlock()

	metricTime := req.MetricTime
	if metricTime.IsZero() {
		metricTime = req.HeartbeatTime
	}
	fanOutResourceMetric(ctx, nodeID, req, metricTime)

	// Mirror metric freshness into the snapshot for score-only views.
	if req.Allocated != nil || req.DiskUsage != nil {
		svc.mu.Lock()
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
		svc.mu.Unlock()
	}

	// Merge incoming HostFacts against prev so a transient /sys/module read
	// gap (KVMModuleScanned=false) does not wipe the persisted module state.
	if req.HostFacts != nil && !req.HostFacts.IsZero() {
		svc.mu.Lock()
		merged := mergeIncomingHostFacts(snap.HostFacts, req.HostFacts)
		snap.HostFacts = merged
		svc.mu.Unlock()
		reg := &store.NodeRegistration{NodeID: nodeID}
		applyHostFactsToRegistration(reg, merged)
		if err := svc.store.UpdateHostFacts(ctx, nodeID, reg.HostFactsJSON, reg.CPUIDHash, reg.HostKernelRelease); err != nil {
			logging.G(ctx).Warnf("nodemgmt: persist host facts failed: node=%s: %v", nodeID, err)
		}
	}

	svc.persistVersions(ctx, nodeID, req.Versions, req.InventoryIncomplete)
	return cloneSnapshot(snap), nil
}

// fanOutResourceMetric writes the cubelet-reported resource metric to Redis.
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
	_ = ctx
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	snap, ok := svc.nodes[nodeID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneSnapshotWithCurrentHealth(snap), nil
}

func (svc *NodeService) ListNodes(ctx context.Context) ([]*model.NodeSnapshot, error) {
	_ = ctx
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	out := make([]*model.NodeSnapshot, 0, len(svc.nodes))
	now := time.Now()
	for _, snap := range svc.nodes {
		out = append(out, cloneSnapshotWithCurrentHealthAt(snap, now))
	}
	sortSnapshots(out)
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

	unlock := svc.lockNodeLabels(nodeID)
	defer unlock()

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

	snap := svc.ensureNode(nodeID)
	svc.mu.Lock()
	snap.Labels = cloneStringMap(existing)
	snap.LabelsJSONCorrupt = false
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	svc.mu.Unlock()

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

	unlock := svc.lockNodeLabels(nodeID)
	defer unlock()

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

	snap := svc.ensureNode(nodeID)
	svc.mu.Lock()
	snap.Labels = cloneStringMap(existing)
	snap.LabelsJSONCorrupt = false
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	svc.mu.Unlock()

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
	unlock := svc.lockNodeLabels(nodeID)
	defer unlock()

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

	snap := svc.ensureNode(nodeID)
	svc.mu.Lock()
	snap.Labels = cloneStringMap(existing)
	snap.LabelsJSONCorrupt = false
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	svc.mu.Unlock()

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

func (svc *NodeService) lockNodeLabels(nodeID string) func() {
	v, _ := svc.labelWriteLocks.LoadOrStore(nodeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (svc *NodeService) lockVersionWrite(nodeID string) func() {
	v, _ := svc.versionWriteLocks.LoadOrStore(nodeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (svc *NodeService) ensureNode(nodeID string) *model.NodeSnapshot {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if snap, ok := svc.nodes[nodeID]; ok {
		return snap
	}
	snap := &model.NodeSnapshot{NodeID: nodeID}
	svc.nodes[nodeID] = snap
	return snap
}

func (svc *NodeService) persistVersions(ctx context.Context, nodeID string, versions []model.ComponentVersion, inventoryIncomplete bool) {
	if len(versions) == 0 {
		return
	}
	unlock := svc.lockVersionWrite(nodeID)
	defer unlock()

	snap := svc.ensureNode(nodeID)
	svc.mu.RLock()
	prevVersions := append([]model.ComponentVersion(nil), snap.Versions...)
	prevHash := snap.VersionsHash
	prevCompat := CompatRelevantVersions(snap.Versions)
	svc.mu.RUnlock()

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
	svc.mu.Lock()
	if inventoryIncomplete {
		snap.Versions = merged
	} else {
		snap.Versions = append([]model.ComponentVersion(nil), versions...)
	}
	snap.VersionsHash = h
	newCompat := CompatRelevantVersions(snap.Versions)
	svc.mu.Unlock()
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

func (svc *NodeService) reload(ctx context.Context) error {
	regs, err := svc.store.ListRegistrations(ctx)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: reload list registrations failed: %v", err)
		return err
	}
	sts, err := svc.store.ListStatuses(ctx)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: reload list statuses failed: %v", err)
		return err
	}
	statusByNode := map[string]*store.NodeStatus{}
	for i := range sts {
		statusByNode[sts[i].NodeID] = &sts[i]
	}
	versions, err := svc.store.ListComponentVersions(ctx)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: reload list component versions failed: %v", err)
		return err
	}
	versionsByNode := map[string][]store.NodeComponentVersion{}
	for i := range versions {
		versionsByNode[versions[i].NodeID] = append(versionsByNode[versions[i].NodeID], versions[i])
	}

	next := make(map[string]*model.NodeSnapshot, len(regs))
	for i := range regs {
		snap := buildSnapshotFromStore(&regs[i], statusByNode[regs[i].NodeID], versionsByNode[regs[i].NodeID])
		next[regs[i].NodeID] = snap
	}
	for nodeID, st := range statusByNode {
		if _, ok := next[nodeID]; !ok {
			next[nodeID] = buildSnapshotFromStore(&store.NodeRegistration{NodeID: nodeID}, st, versionsByNode[nodeID])
		}
	}

	// Drop nodes gone from the DB since the last reload (e.g. deleted on
	// another replica) and clean their Redis metric (best-effort).
	svc.mu.Lock()
	evicted := make([]string, 0)
	for nodeID := range svc.nodes {
		if _, stillPresent := next[nodeID]; !stillPresent {
			evicted = append(evicted, nodeID)
		}
	}
	svc.nodes = next
	svc.mu.Unlock()
	for _, nodeID := range evicted {
		if err := nodemetric.DeleteNodeMetric(nodeID); err != nil {
			logging.G(ctx).Warnf("nodemgmt: reload evict metric failed: node=%s: %v", nodeID, err)
		}
	}
	return nil
}

func cloneSnapshotWithCurrentHealth(in *model.NodeSnapshot) *model.NodeSnapshot {
	return cloneSnapshotWithCurrentHealthAt(in, time.Now())
}

func cloneSnapshotWithCurrentHealthAt(in *model.NodeSnapshot, now time.Time) *model.NodeSnapshot {
	out := cloneSnapshot(in)
	applyCurrentHealth(out, now)
	out.SchedulingDisabled = snapSchedulingDisabled(out)
	return out
}

func (svc *NodeService) LoadDeclaredVersions(declared DeclaredVersionInfo) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if declared.Primary == nil {
		declared.Primary = map[string]string{}
	}
	if declared.Sets == nil {
		declared.Sets = map[string]map[string]struct{}{}
	}
	svc.declaredVersions = declared.Primary
	svc.declaredVersionSets = declared.Sets
}
