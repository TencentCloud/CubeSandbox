// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	opsAgentPusher   OpsAgentPusher
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

	// Preserve the previous physical/ratio on re-register: an old cubelet
	// reports 0/nil ("unknown"), which must not clobber a known value.
	prevReg, err := svc.store.GetRegistration(ctx, req.NodeID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	var prevFacts *model.HostFacts
	if prevReg != nil {
		prevFacts = unmarshalHostFacts(prevReg.HostFactsJSON)
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
		PausedReleaseRatio:  req.PausedReleaseRatio,
	}
	// Merge once and reuse for both the DB row and the returned snapshot so a
	// zero physical from an old cubelet cannot diverge the two (see snapshot).
	mergedFacts := mergeIncomingHostFacts(prevFacts, req.HostFacts)
	applyHostFactsToRegistration(reg, mergedFacts)
	if req.PausedReleaseRatio == nil && prevReg != nil {
		reg.PausedReleaseRatio = prevReg.PausedReleaseRatio
	}

	if err := svc.store.UpsertRegistration(ctx, reg); err != nil {
		logging.G(ctx).Errorf("nodemgmt: register upsert failed: node=%s: %v", req.NodeID, err)
		return nil, err
	}
	mergedLabels, _, err := svc.store.MutateLabels(ctx, req.NodeID, func(current map[string]string) (map[string]string, bool, error) {
		// Copy first: the merge writes into its existing map, and comparing
		// that map with itself would always look unchanged.
		merged := StripAndPreserveSchedulingLabel(cloneStringMap(current), req.Labels)
		if err := ValidateLabelsSkippingReserved(merged); err != nil {
			logging.G(ctx).Warnf("nodemgmt: register labels invalid: node=%s: %v", req.NodeID, err)
			return nil, false, fmt.Errorf("register labels invalid: %w", err)
		}
		if CountUserLabels(merged) > maxLabelsPerNode {
			logging.G(ctx).Warnf("nodemgmt: register label count exceeded: node=%s got=%d max=%d", req.NodeID, CountUserLabels(merged), maxLabelsPerNode)
			return nil, false, fmt.Errorf("%w: a node cannot have more than %d labels, got %d after merge",
				ErrInvalidLabel, maxLabelsPerNode, CountUserLabels(merged))
		}
		if labelsEqual(current, merged) {
			return current, false, nil
		}
		return merged, true, nil
	})
	if err != nil {
		if errors.Is(err, store.ErrCorruptLabelsJSON) {
			logging.G(ctx).Errorf("nodemgmt: register labels corrupt: node=%s: %v", req.NodeID, err)
			return nil, fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
		}
		logging.G(ctx).Errorf("nodemgmt: register update labels failed: node=%s: %v", req.NodeID, err)
		return nil, err
	}

	now := time.Now()
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
		// Use the persisted value so the response matches the DB, preserving the previous ratio for nil or non-reporting cubelets.
		PausedReleaseRatio:  reg.PausedReleaseRatio,
		HostFacts:           cloneHostFacts(mergedFacts),
		HeartbeatTime:       now,
		HeartbeatReceivedAt: now,
	}
	applyCurrentHealth(snap, now)
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)

	svc.ensureClusterRow(ctx, req.NodeID)
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
	receivedAt := time.Now()

	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		return nil, err
	}

	reportedReady := ReadyConditionTrue(req.Conditions)
	status := &store.NodeStatus{
		NodeID:                nodeID,
		ConditionsJSON:        model.MustJSON(req.Conditions),
		ImagesJSON:            model.MustJSON(req.Images),
		LocalTemplatesJSON:    model.MustJSON(req.LocalTemplates),
		HeartbeatUnix:         req.HeartbeatTime.Unix(),
		HeartbeatReceivedUnix: receivedAt.Unix(),
		Healthy:               reportedReady,
	}
	if err := svc.store.UpsertStatus(ctx, status); err != nil {
		logging.G(ctx).Errorf("nodemgmt: heartbeat upsert failed: node=%s: %v", nodeID, err)
		return nil, err
	}

	snap, err := svc.getNodeFromRedisOrDB(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	applyRegistrationFields(snap, reg)
	snap.Conditions = append([]model.NodeCondition(nil), req.Conditions...)
	snap.Images = append([]model.ContainerImage(nil), req.Images...)
	snap.LocalTemplates = append([]model.LocalTemplate(nil), req.LocalTemplates...)
	snap.HeartbeatTime = req.HeartbeatTime
	snap.HeartbeatReceivedAt = receivedAt
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
		prev := snap.HostFacts
		merged := mergeIncomingHostFacts(prev, req.HostFacts)
		snap.HostFacts = merged
		// Diff-then-write on the persisted form.
		if marshalHostFacts(merged) != marshalHostFacts(prev) {
			reg := &store.NodeRegistration{NodeID: nodeID}
			applyHostFactsToRegistration(reg, merged)
			if err := svc.store.UpdateHostFacts(ctx, nodeID, reg.HostFactsJSON, reg.CPUIDHash, reg.HostKernelRelease, reg.CPUCount, reg.MemTotalMB); err != nil {
				logging.G(ctx).Warnf("nodemgmt: persist host facts failed: node=%s: %v", nodeID, err)
			}
		}
	}

	if req.Quota != nil && !req.Quota.IsZero() {
		svc.applyHeartbeatQuota(ctx, nodeID, req.Quota, snap)
	}

	svc.persistVersions(ctx, nodeID, req.Versions, req.InventoryIncomplete, snap)
	nodemetric.WriteNodeSnapshot(snap)
	return cloneSnapshot(snap), nil
}

// applyHeartbeatQuota persists a heartbeat-reported quota change (diff-then-write).
func (svc *NodeService) applyHeartbeatQuota(ctx context.Context, nodeID string, q *model.QuotaReport, snap *model.NodeSnapshot) {
	// A 0 cpu/mem/mvm is a degenerate read (e.g. /proc/meminfo failure), not a
	// valid value; keep the last good one. create_concurrent_num=0 stays valid.
	if q.MilliCPU == 0 {
		q.MilliCPU = snap.QuotaCPU
	}
	if q.MemMB == 0 {
		q.MemMB = snap.QuotaMemMB
	}
	if q.MaxMvmNum == 0 {
		q.MaxMvmNum = snap.MaxMvmNum
	}
	ratioChanged := q.PausedReleaseRatio != nil &&
		(snap.PausedReleaseRatio == nil || *snap.PausedReleaseRatio != *q.PausedReleaseRatio)
	// Capacity/Allocatable must mirror the effective quota (cubelet's
	// applyHostQuota keeps them equal); a prior drift also forces a write.
	capacityDrifted := snap.Capacity.MilliCPU != q.MilliCPU || snap.Capacity.MemoryMB != q.MemMB ||
		snap.Allocatable.MilliCPU != q.MilliCPU || snap.Allocatable.MemoryMB != q.MemMB
	if snap.QuotaCPU == q.MilliCPU && snap.QuotaMemMB == q.MemMB &&
		snap.MaxMvmNum == q.MaxMvmNum && snap.CreateConcurrentNum == q.CreateConcurrentNum &&
		!ratioChanged && !capacityDrifted {
		return
	}
	if err := svc.store.UpdateQuota(ctx, nodeID, *q); err != nil {
		logging.G(ctx).Errorf("nodemgmt: update quota failed: node=%s: %v", nodeID, err)
		return
	}
	logging.G(ctx).Infof("nodemgmt: node quota updated by heartbeat: node=%s cpu=%d memMB=%d mvm=%d createConcurrent=%d ratio=%s",
		nodeID, q.MilliCPU, q.MemMB, q.MaxMvmNum, q.CreateConcurrentNum, formatRatioPtr(q.PausedReleaseRatio))
	snap.QuotaCPU = q.MilliCPU
	snap.QuotaMemMB = q.MemMB
	snap.MaxMvmNum = q.MaxMvmNum
	snap.CreateConcurrentNum = q.CreateConcurrentNum
	// Keep the in-memory snapshot's Capacity/Allocatable consistent with
	// what was just persisted, so the scheduler view never shows two numbers.
	snap.Capacity = model.ResourceSnapshot{MilliCPU: q.MilliCPU, MemoryMB: q.MemMB}
	snap.Allocatable = model.ResourceSnapshot{MilliCPU: q.MilliCPU, MemoryMB: q.MemMB}
	if q.PausedReleaseRatio != nil {
		snap.PausedReleaseRatio = q.PausedReleaseRatio
	}
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
	// Redis is a status cache. The registration row is the label source of
	// truth, including a node whose snapshot write has not landed yet.
	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	applyRegistrationFields(snap, reg)
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)
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
	regByID := make(map[string]*store.NodeRegistration, len(regs))
	for i := range regs {
		regByID[regs[i].NodeID] = &regs[i]
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
		reg := regByID[s.NodeID]
		if reg == nil {
			continue // stale Redis key for a node no longer registered
		}
		applyRegistrationFields(s, reg)
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
		// A scan miss can still GET a stale snapshot; the registration row
		// remains the label source.
		applyRegistrationFields(snap, &regs[i])
		snap.SchedulingDisabled = snapSchedulingDisabled(snap)
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

// mutateNodeLabels is the single write path for node labels: read, apply
// mutate, validate, persist, refresh the Redis snapshot and record the audit
// entry. Control-plane-owned keys are never part of mutate's input or output;
// they are restored from the persisted row afterwards.
func (svc *NodeService) mutateNodeLabels(
	ctx context.Context,
	nodeID, operator, opType string,
	protected map[string]struct{},
	mutate func(mutable map[string]string) (map[string]string, error),
) error {
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	var beforeUser, afterUser map[string]string
	_, changed, err := svc.store.MutateLabels(ctx, nodeID, func(current map[string]string) (map[string]string, bool, error) {
		mutable := mutableLabels(current, protected)
		beforeUser = cloneStringMap(mutable)
		// Hand the callback its own copy so an in-place edit cannot alias the
		// map we compare against.
		nextUser, err := mutate(cloneStringMap(mutable))
		if err != nil {
			return nil, false, err
		}
		if err := ValidateLabels(nextUser); err != nil {
			return nil, false, err
		}
		kept := protectedLabels(current, protected)
		next := make(map[string]string, len(nextUser)+len(kept))
		for k, v := range nextUser {
			next[k] = v
		}
		for k, v := range kept {
			next[k] = v
		}
		if CountUserLabels(next) > maxLabelsPerNode {
			return nil, false, fmt.Errorf("%w: a node cannot have more than %d labels, got %d",
				ErrInvalidLabel, maxLabelsPerNode, CountUserLabels(next))
		}
		afterUser = cloneStringMap(nextUser)
		if labelsEqual(current, next) {
			return current, false, nil
		}
		return next, true, nil
	})
	if err != nil {
		if errors.Is(err, store.ErrCorruptLabelsJSON) {
			logging.G(ctx).Errorf("nodemgmt: %s labels corrupt: node=%s: %v", opType, nodeID, err)
			return fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
		}
		if errors.Is(err, store.ErrNotFound) {
			logging.G(ctx).Errorf("nodemgmt: %s get registration failed: node=%s: %v", opType, nodeID, err)
		}
		return err
	}
	if !changed {
		return nil
	}
	svc.syncRegistrationLabelsToRedis(ctx, nodeID)
	detail := labelChangeDetail(beforeUser, afterUser)
	if err := svc.recordOperation(ctx, nodeID, opType, operator, truncateOperationDetail(detail)); err != nil {
		logging.G(ctx).Warnf("nodemgmt: %s record operation failed: node=%s: %v", opType, nodeID, err)
	}
	return nil
}

// syncRegistrationLabelsToRedis copies the committed registration labels onto
// the Redis snapshot. Reading after the transaction commits means a later
// writer is not overwritten by an earlier in-memory map.
func (svc *NodeService) syncRegistrationLabelsToRedis(ctx context.Context, nodeID string) {
	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		logging.G(ctx).Warnf("nodemgmt: sync labels read failed: node=%s: %v", nodeID, err)
		return
	}
	svc.updateSnapshotInRedis(ctx, nodeID, func(snap *model.NodeSnapshot) {
		applyRegistrationFields(snap, reg)
		snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	})
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// UpdateNodeLabels merges labels into the node's user labels. Keys that are not
// present in labels are preserved. Control-plane-owned keys cannot be modified.
func (svc *NodeService) UpdateNodeLabels(ctx context.Context, nodeID string, labels map[string]string, operator string) error {
	// A nil map merges nothing (historical 200 no-op; only replace distinguishes
	// "clear everything" from "no body").
	if labels == nil {
		labels = map[string]string{}
	}
	// Reserved-key policy is checked before syntax so a control-plane key maps
	// to 403 instead of 400.
	if _, ok := labels[model.LabelSchedulingDisabled]; ok {
		return fmt.Errorf("%w: cannot modify reserved label %q via label API", ErrReservedLabel, model.LabelSchedulingDisabled)
	}
	if _, ok := labels[model.LabelMachineType]; ok {
		return fmt.Errorf("%w: machine-type is managed by the machine type API", ErrReservedLabel)
	}
	if err := ValidateLabels(labels); err != nil {
		return err
	}
	next := make(map[string]string, len(labels))
	for k, v := range labels {
		next[k] = v
	}
	// machine-type is reserved and must stay out of the mutable set, or the
	// merge would re-validate the stored value and reject it.
	err := svc.mutateNodeLabels(ctx, nodeID, operator, model.OpSetLabels, replaceProtectedLabelKeys,
		func(user map[string]string) (map[string]string, error) {
			return mergeLabels(user, next), nil
		})
	if err != nil {
		return err
	}
	logging.G(ctx).Infof("nodemgmt: labels updated: node=%s operator=%s keys=%d", nodeID, operator, len(labels))
	return nil
}

// ReplaceNodeLabels replaces the node's complete user-label set. Control-plane
// keys are preserved; user keys not present in labels are removed.
func (svc *NodeService) ReplaceNodeLabels(ctx context.Context, nodeID string, labels map[string]string, operator string) error {
	if labels == nil {
		return fmt.Errorf("%w: labels must not be empty; send {} to clear user labels", ErrInvalidLabel)
	}
	if _, ok := labels[model.LabelSchedulingDisabled]; ok {
		return fmt.Errorf("%w: cannot modify reserved label %q via label API", ErrReservedLabel, model.LabelSchedulingDisabled)
	}
	if _, ok := labels[model.LabelMachineType]; ok {
		return fmt.Errorf("%w: machine-type is managed by the machine type API", ErrReservedLabel)
	}
	if err := ValidateLabels(labels); err != nil {
		return err
	}
	next := make(map[string]string, len(labels))
	for k, v := range labels {
		next[k] = v
	}
	// mutateNodeLabels re-attaches every control-plane and data-plane key from
	// a fresh read, so the replacement set is exactly the caller's user labels.
	err := svc.mutateNodeLabels(ctx, nodeID, operator, model.OpReplaceLabels, replaceProtectedLabelKeys,
		func(map[string]string) (map[string]string, error) {
			return next, nil
		})
	if err != nil {
		return err
	}
	logging.G(ctx).Infof("nodemgmt: labels replaced: node=%s operator=%s keys=%d", nodeID, operator, len(labels))
	return nil
}

// DeleteNodeLabel removes one user label. Control-plane keys cannot be deleted
// through this API.
func (svc *NodeService) DeleteNodeLabel(ctx context.Context, nodeID, key, operator string) error {
	if key == "" {
		return fmt.Errorf("%w: label key must not be empty", ErrInvalidLabel)
	}
	// DeleteLabelByQuery deletes a label key that may contain '/'.
	// ValidateLabelKey rejects reserved keys (403); malformed keys map to 400.
	if err := ValidateLabelKey(key); err != nil {
		return err
	}
	if err := svc.mutateNodeLabels(ctx, nodeID, operator, model.OpDelLabel, replaceProtectedLabelKeys,
		func(user map[string]string) (map[string]string, error) {
			delete(user, key)
			return user, nil
		}); err != nil {
		return err
	}
	logging.G(ctx).Infof("nodemgmt: label deleted: node=%s key=%s operator=%s", nodeID, key, operator)
	return nil
}

// truncateOperationDetail keeps audit detail within the column budget.
func truncateOperationDetail(detail string) string {
	if utf8.RuneCountInString(detail) <= maxOperationDetailLen {
		return detail
	}
	runes := []rune(detail)
	return string(runes[:maxOperationDetailLen])
}

// labelChangeDetail summarizes an audit entry from the operator-label sets
// before and after the write. Label names are included (sorted, capped).
func labelChangeDetail(before, after map[string]string) string {
	added, changed, removed := 0, 0, 0
	var names []string
	for k, v := range after {
		old, ok := before[k]
		switch {
		case !ok:
			added++
			names = append(names, "+"+k)
		case old != v:
			changed++
			names = append(names, "~"+k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed++
			names = append(names, "-"+k)
		}
	}
	if len(names) == 0 {
		return "no label change"
	}
	sort.Strings(names)
	const maxNames = 8
	shown := names
	if len(shown) > maxNames {
		shown = shown[:maxNames]
	}
	detail := fmt.Sprintf("added=%d changed=%d removed=%d keys=%s",
		added, changed, removed, strings.Join(shown, ","))
	if len(names) > maxNames {
		detail += fmt.Sprintf(" (+%d more)", len(names)-maxNames)
	}
	return detail
}

func (svc *NodeService) SetNodeSchedulingDisabled(ctx context.Context, nodeID string, disabled bool, operator, detail string) (*model.NodeSnapshot, error) {
	if nodeID == "" {
		return nil, ErrNodeIDRequired
	}
	if utf8.RuneCountInString(detail) > maxOperationDetailLen {
		return nil, fmt.Errorf("%w: detail must be at most %d characters, got %d", ErrDetailTooLong, maxOperationDetailLen, utf8.RuneCountInString(detail))
	}

	_, changed, err := svc.store.MutateLabels(ctx, nodeID, func(current map[string]string) (map[string]string, bool, error) {
		_, has := current[model.LabelSchedulingDisabled]
		switch {
		case disabled && (!has || current[model.LabelSchedulingDisabled] != model.LabelSchedulingDisabledValue):
			current[model.LabelSchedulingDisabled] = model.LabelSchedulingDisabledValue
			return current, true, nil
		case !disabled && has:
			delete(current, model.LabelSchedulingDisabled)
			return current, true, nil
		default:
			return current, false, nil
		}
	})
	if err != nil {
		if errors.Is(err, store.ErrCorruptLabelsJSON) {
			logging.G(ctx).Errorf("nodemgmt: isolation labels corrupt: node=%s: %v", nodeID, err)
			return nil, fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
		}
		logging.G(ctx).Errorf("nodemgmt: isolation get registration failed: node=%s: %v", nodeID, err)
		return nil, err
	}
	if changed {
		svc.syncRegistrationLabelsToRedis(ctx, nodeID)
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
	snap, err := svc.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
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
	return operationsToModel(rows), nil
}

func (svc *NodeService) ListOperationsByTypes(ctx context.Context, nodeID string, types []string, limit int) ([]model.NodeOperation, error) {
	rows, err := svc.store.ListOperationsByTypes(ctx, nodeID, types, limit)
	if err != nil {
		return nil, err
	}
	return operationsToModel(rows), nil
}

func operationsToModel(rows []store.NodeOperation) []model.NodeOperation {
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
	return out
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
