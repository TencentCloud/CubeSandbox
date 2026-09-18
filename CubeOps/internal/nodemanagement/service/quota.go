// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

// Fixed overcommit ceilings: mcpu_limit may not exceed cpuTotal*1000*20,
// mem_limit not memTotal*3. Mirrors the ops-agent node-side guard.
const (
	maxCPUOvercommit = 20.0
	maxMemOvercommit = 3.0

	// quotaPushConcurrency bounds the fan-out pushes after a cluster
	// default change.
	quotaPushConcurrency = 10
)

var ErrQuotaRevisionConflict = errors.New("quota revision conflict")

// ErrQuotaValidation marks operator-input rejections (guard bounds, malformed
// quantities) so the HTTP layer can answer 400 instead of 500.
var ErrQuotaValidation = errors.New("quota validation failed")

// formatRatioPtr renders a *float64 for logs; nil → "nil".
func formatRatioPtr(r *float64) string {
	if r == nil {
		return "nil"
	}
	return fmt.Sprintf("%v", *r)
}

// resolvePausedRatio applies the inheritance chain: explicit node value >
// cluster default > 0.
func resolvePausedRatio(nodeRatio, clusterDefault *float64) (effective float64, inherited bool) {
	if nodeRatio != nil {
		return *nodeRatio, false
	}
	if clusterDefault != nil {
		return *clusterDefault, true
	}
	return 0, true
}

// clusterQuotaDefault reads the cluster default from the sentinel spec row.
// nil means unset.
func (svc *NodeService) clusterQuotaDefault(ctx context.Context) (*float64, error) {
	sentinel, err := svc.store.GetQuotaSpec(ctx, store.ClusterQuotaSentinelID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return sentinel.PausedReleaseRatio, nil
}

// ensureClusterRow registers a cluster-managed spec row for a node when a
// cluster default exists and the node has no row yet; pull reconcile then
// covers the node's ratio. Called on registration and cluster default writes.
func (svc *NodeService) ensureClusterRow(ctx context.Context, nodeID string) {
	defaultRatio, err := svc.clusterQuotaDefault(ctx)
	if err != nil || defaultRatio == nil {
		return
	}
	if _, err := svc.store.GetQuotaSpec(ctx, nodeID); err == nil {
		return // row exists (node-managed or cluster-managed)
	} else if !errors.Is(err, store.ErrNotFound) {
		return
	}
	row := &store.NodeQuotaSpec{NodeID: nodeID, Revision: 1, UpdatedBy: "cluster"}
	if err := svc.store.UpsertQuotaSpec(ctx, row); err != nil {
		logging.G(ctx).Warnf("nodemgmt: ensure cluster row failed: node=%s: %v", nodeID, err)
	}
}

// GetClusterQuotaDefaults returns the cluster-wide paused-release-ratio
// default; nil when unset.
func (svc *NodeService) GetClusterQuotaDefaults(ctx context.Context) (*model.ClusterQuotaDefaults, error) {
	ratio, err := svc.clusterQuotaDefault(ctx)
	if err != nil {
		return nil, err
	}
	return &model.ClusterQuotaDefaults{PausedReleaseRatio: ratio}, nil
}

// SetClusterQuotaDefaults stores the cluster default and fans it out to followers.
func (svc *NodeService) SetClusterQuotaDefaults(ctx context.Context, ratio *float64, operator string) (*model.ClusterQuotaDefaults, *model.QuotaPropagation, error) {
	if ratio != nil && (*ratio < 0 || *ratio > 1) {
		return nil, nil, fmt.Errorf("%w: paused_release_ratio must be in [0,1], got %v", ErrQuotaValidation, *ratio)
	}

	// Sentinel row: the cluster default plus a monotonic write counter.
	old, err := svc.store.GetQuotaSpec(ctx, store.ClusterQuotaSentinelID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}
	sentinel := &store.NodeQuotaSpec{
		NodeID:             store.ClusterQuotaSentinelID,
		PausedReleaseRatio: ratio,
		Revision:           1,
		UpdatedBy:          operator,
	}
	if err := svc.store.UpsertQuotaSpec(ctx, sentinel); err != nil {
		return nil, nil, err
	}
	logging.G(ctx).Infof("nodemgmt: cluster quota default saved: ratio=%s operator=%s", formatRatioPtr(ratio), operator)

	// Cover registered nodes without a spec row so every follower converges.
	regs, err := svc.store.ListRegistrations(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := svc.store.ListQuotaSpecs(ctx)
	if err != nil {
		return nil, nil, err
	}
	existing := make(map[string]struct{}, len(rows))
	for i := range rows {
		existing[rows[i].NodeID] = struct{}{}
	}
	for i := range regs {
		if _, ok := existing[regs[i].NodeID]; !ok {
			svc.ensureClusterRow(ctx, regs[i].NodeID)
		}
	}

	// Signal + fan out. Followers: cluster-managed rows plus node-managed
	// rows whose ratio is NULL. Explicit ratios are left untouched.
	bumped, err := svc.store.BumpClusterQuotaFollowers(ctx)
	if err != nil {
		return nil, nil, err
	}
	prop := &model.QuotaPropagation{Bumped: bumped}
	if bumped == 0 {
		svc.recordClusterQuotaAudit(ctx, operator, old, ratio, prop)
		return &model.ClusterQuotaDefaults{PausedReleaseRatio: ratio}, prop, nil
	}

	followers, err := svc.store.ListClusterQuotaFollowers(ctx)
	if err != nil {
		return nil, nil, err
	}

	// The fan-out must outlive the operator's request: the follower rows were
	// already bumped above, so a client disconnect (or the HTTP request timing
	// out) cancelling ctx would fail every remaining push and silently drop the
	// cluster onto the agents' 300s pull reconcile. Detach cancellation while
	// keeping the request's values (request id) for logging; each individual
	// push is still bounded by the ops-agent client's own HTTP timeout.
	pushCtx := context.WithoutCancel(ctx)

	sem := make(chan struct{}, quotaPushConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range followers {
		row := followers[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// The DB read and the HTTP push run outside the lock: mu only
			// guards the prop counters below, so quotaPushConcurrency
			// goroutines actually fan out in parallel instead of serialising
			// on a lock held across the network call.
			failed := false
			if reg, err := svc.store.GetRegistration(pushCtx, row.NodeID); err != nil {
				failed = true
			} else if reason := svc.pushQuotaRow(pushCtx, reg, &row, ratio); reason != "" {
				failed = true
				logging.G(pushCtx).Warnf("nodemgmt: cluster default push failed: node=%s: %s", row.NodeID, reason)
			}

			mu.Lock()
			defer mu.Unlock()
			if failed {
				prop.Failed++
				return
			}
			prop.Pushed++
		}()
	}
	wg.Wait()
	// Audit under pushCtx so the propagation outcome is still recorded even
	// after the caller's request has been cancelled.
	svc.recordClusterQuotaAudit(pushCtx, operator, old, ratio, prop)
	return &model.ClusterQuotaDefaults{PausedReleaseRatio: ratio}, prop, nil
}

// recordClusterQuotaAudit persists the cluster default change with its
// propagation outcome onto the sentinel row, retrievable through the shared
// history endpoint (node quota history <sentinel-id>).
func (svc *NodeService) recordClusterQuotaAudit(ctx context.Context, operator string, old *store.NodeQuotaSpec, ratio *float64, prop *model.QuotaPropagation) {
	var oldRatio *float64
	if old != nil {
		oldRatio = old.PausedReleaseRatio
	}
	detail := struct {
		Old        *float64 `json:"old"`
		New        *float64 `json:"new"`
		Bumped     int64    `json:"bumped"`
		Pushed     int      `json:"pushed"`
		PushFailed int      `json:"push_failed"`
	}{Old: oldRatio, New: ratio, Bumped: prop.Bumped, Pushed: prop.Pushed, PushFailed: prop.Failed}
	if err := svc.recordOperation(ctx, store.ClusterQuotaSentinelID, model.OpSetClusterQuota, operator, model.MustJSON(&detail)); err != nil {
		logging.G(ctx).Warnf("nodemgmt: record set-cluster-quota operation failed: %v", err)
	}
}

// OpsAgentPusher forwards a quota spec to the node-local ops-agent. Nil pusher
// means ops-agent integration is disabled; SetNodeQuota then still persists
// the spec and reports the push as skipped.
type OpsAgentPusher interface {
	// PushQuota posts the spec to the agent on hostIP and returns the
	// agent-applied flag or an error (unreachable / rejected).
	PushQuota(ctx context.Context, hostIP string, req model.OpsAgentPushRequest) (bool, error)
}

// SetOpsAgentPusher wires the ops-agent push client (server assembly step).
func (svc *NodeService) SetOpsAgentPusher(p OpsAgentPusher) {
	svc.opsAgentPusher = p
}

// GetNodeQuotaView returns the desired spec, the heartbeat-reported actual,
// and the drift between them.
func (svc *NodeService) GetNodeQuotaView(ctx context.Context, nodeID string) (*model.QuotaView, error) {
	snap, err := svc.getNodeFromRedisOrDB(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	view := &model.QuotaView{
		NodeID: nodeID,
		Actual: model.QuotaActual{
			MilliCPU:            snap.QuotaCPU,
			MemMB:               snap.QuotaMemMB,
			MaxMvmNum:           snap.MaxMvmNum,
			CreateConcurrentNum: snap.CreateConcurrentNum,
			PausedReleaseRatio:  snap.PausedReleaseRatio,
		},
		Drift: model.DriftNoSpec,
	}
	spec, err := svc.store.GetQuotaSpec(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			view.Message = "no quota spec; node runs on cubelet defaults"
			return view, nil
		}
		return nil, err
	}
	clusterDefault, err := svc.clusterQuotaDefault(ctx)
	if err != nil {
		return nil, err
	}
	view.Spec = specModelToAPI(spec)
	view.EffectivePausedRatio, view.PausedRatioInherited = resolvePausedRatio(spec.PausedReleaseRatio, clusterDefault)
	view.Drift, view.Message = quotaDrift(view.Spec, view.Actual)
	if a := view.Actual.PausedReleaseRatio; a != nil && *a != view.EffectivePausedRatio {
		view.Drift = model.DriftDetected
		if view.Message != "" {
			view.Message += "; "
		}
		view.Message += fmt.Sprintf("pausedRatio: spec=%v actual=%v", view.EffectivePausedRatio, *a)
	}
	return view, nil
}

// SetNodeQuota validates and persists the desired spec, records an audit
// entry, then pushes it to ops-agent. The spec is persisted regardless of the
// push outcome: a failed push is retried by the agent's pull reconcile.
func (svc *NodeService) SetNodeQuota(ctx context.Context, nodeID string, spec *model.QuotaSpec, operator string) (*model.QuotaView, *model.PushResult, error) {
	if spec == nil {
		return nil, nil, fmt.Errorf("spec is required")
	}
	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	physical, err := physicalFromRegistration(reg)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrQuotaValidation, err)
	}

	old, err := svc.store.GetQuotaSpec(ctx, nodeID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}

	if err := validateQuotaSpec(spec, physical); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrQuotaValidation, err)
	}

	row := specAPIToModel(nodeID, spec, old, operator)
	if old != nil {
		expected := old.Revision
		if spec.ExpectedRevision != 0 {
			expected = spec.ExpectedRevision
		}
		applied, err := svc.store.UpdateQuotaSpecIfRevision(ctx, row, expected)
		if err != nil {
			return nil, nil, err
		}
		if !applied {
			return nil, nil, fmt.Errorf("%w: expected %d, stored %d", ErrQuotaRevisionConflict, expected, old.Revision)
		}
	} else {
		if spec.ExpectedRevision != 0 {
			return nil, nil, fmt.Errorf("%w: expected %d, stored none", ErrQuotaRevisionConflict, spec.ExpectedRevision)
		}
		if err := svc.store.CreateQuotaSpecIfAbsent(ctx, row); err != nil {
			// A concurrent writer created the row first: surface the same
			// conflict the CAS update path reports, rather than overwriting.
			if errors.Is(err, store.ErrQuotaSpecExists) {
				return nil, nil, fmt.Errorf("%w: expected none, stored a row", ErrQuotaRevisionConflict)
			}
			return nil, nil, err
		}
	}

	if err := svc.recordOperation(ctx, nodeID, model.OpSetQuota, operator, quotaAuditDetail(old, row)); err != nil {
		logging.G(ctx).Warnf("nodemgmt: record set-quota operation failed: node=%s: %v", nodeID, err)
	}
	logging.G(ctx).Infof("nodemgmt: quota spec saved: node=%s revision=%d operator=%s", nodeID, row.Revision, operator)

	push := &model.PushResult{}
	clusterDefault, err := svc.clusterQuotaDefault(ctx)
	if err != nil {
		return nil, nil, err
	}
	if reason := svc.pushQuotaRow(ctx, reg, row, clusterDefault); reason != "" {
		push.SkipReason = reason
	} else {
		push.Applied = true
	}

	view, err := svc.GetNodeQuotaView(ctx, nodeID)
	if err != nil {
		return nil, push, err
	}
	return view, push, nil
}

// pushQuotaRow forwards a row to the node-local agent: node-managed rows
// carry the full spec, cluster-managed rows the ratio only. Returns "" on
// success, else a skip reason.
func (svc *NodeService) pushQuotaRow(ctx context.Context, reg *store.NodeRegistration, row *store.NodeQuotaSpec, clusterDefault *float64) string {
	if svc.opsAgentPusher == nil {
		return "ops-agent integration disabled"
	}
	if err := validatePushHostIP(reg.HostIP); err != nil {
		return err.Error()
	}
	effectiveRatio, _ := resolvePausedRatio(row.PausedReleaseRatio, clusterDefault)
	spec := specModelToAPI(row)
	spec.PausedReleaseRatio = &effectiveRatio
	// Physical stays zero: the agent measures its own host capacity.
	req := model.OpsAgentPushRequest{
		RequestID: fmt.Sprintf("quota-%s-%d", row.NodeID, row.Revision),
		Revision:  row.Revision,
		Spec:      spec,
	}
	if !row.NodeManaged {
		req.OnlyPausedRatio = true
		spec.MCpuLimit, spec.MemLimit = 0, ""
		spec.MvmLimit, spec.CreationConcurrentNum = 0, 0
	}
	applied, err := svc.opsAgentPusher.PushQuota(ctx, reg.HostIP, req)
	switch {
	case err != nil:
		return fmt.Sprintf("push failed: %v", err)
	case !applied:
		return "agent rejected the spec (validation)"
	}
	return ""
}

// GetOpsAgentSpec serves the pull reconcile endpoint: the desired spec plus
// the node's physical totals for agent-side re-validation. Managed=false
// means no spec row exists and the agent must leave the file untouched.
func (svc *NodeService) GetOpsAgentSpec(ctx context.Context, nodeID string) (*model.OpsAgentSpecResponse, error) {
	if _, err := svc.store.GetRegistration(ctx, nodeID); err != nil {
		return nil, err
	}
	// Physical stays zero: the agent measures its own host capacity.
	resp := &model.OpsAgentSpecResponse{
		NodeID: nodeID,
	}
	spec, err := svc.store.GetQuotaSpec(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return resp, nil
		}
		return nil, err
	}
	clusterDefault, err := svc.clusterQuotaDefault(ctx)
	if err != nil {
		return nil, err
	}
	resp.Managed = true
	resp.Revision = spec.Revision
	resp.Spec = specModelToAPI(spec)
	effectiveRatio, _ := resolvePausedRatio(spec.PausedReleaseRatio, clusterDefault)
	resp.Spec.PausedReleaseRatio = &effectiveRatio
	if !spec.NodeManaged {
		resp.OnlyPausedRatio = true
		resp.Spec.MCpuLimit, resp.Spec.MemLimit = 0, ""
		resp.Spec.MvmLimit, resp.Spec.CreationConcurrentNum = 0, 0
	}
	return resp, nil
}

// quotaDrift compares set spec fields against the heartbeat actual (unset
// fields are unmanaged); the paused ratio is checked by the caller.
func quotaDrift(spec *model.QuotaSpec, actual model.QuotaActual) (model.DriftState, string) {
	var diffs []string
	if spec.MCpuLimit > 0 && actual.MilliCPU != spec.MCpuLimit {
		diffs = append(diffs, fmt.Sprintf("mcpu: spec=%d actual=%d", spec.MCpuLimit, actual.MilliCPU))
	}
	if spec.MemLimit != "" {
		if memMB, err := parseMemLimitMBFloor(spec.MemLimit); err == nil && actual.MemMB != memMB {
			diffs = append(diffs, fmt.Sprintf("mem: spec=%s(%dMB) actual=%dMB", spec.MemLimit, memMB, actual.MemMB))
		}
	}
	if spec.MvmLimit > 0 && actual.MaxMvmNum != spec.MvmLimit {
		diffs = append(diffs, fmt.Sprintf("mvm: spec=%d actual=%d", spec.MvmLimit, actual.MaxMvmNum))
	}
	if spec.CreationConcurrentNum > 0 && actual.CreateConcurrentNum != spec.CreationConcurrentNum {
		diffs = append(diffs, fmt.Sprintf("createConcurrent: spec=%d actual=%d", spec.CreationConcurrentNum, actual.CreateConcurrentNum))
	}
	if len(diffs) == 0 {
		return model.DriftNone, ""
	}
	return model.DriftDetected, strings.Join(diffs, "; ")
}

// physicalFromRegistration returns the overcommit-guard machine totals: the
// cubelet-reported physical capacity, falling back to the capacity snapshot.
func physicalFromRegistration(reg *store.NodeRegistration) (model.PhysicalTotals, error) {
	if reg.CPUCount > 0 || reg.MemTotalMB > 0 {
		return model.PhysicalTotals{CpuTotal: reg.CPUCount, MemMBTotal: reg.MemTotalMB}, nil
	}
	var cap model.ResourceSnapshot
	if err := json.Unmarshal([]byte(reg.CapacityJSON), &cap); err != nil {
		return model.PhysicalTotals{}, fmt.Errorf("parse capacity snapshot: %w", err)
	}
	return model.PhysicalTotals{CpuTotal: cap.MilliCPU / 1000, MemMBTotal: cap.MemoryMB}, nil
}

// validatePushHostIP guards the push target against SSRF: it must be an IPv4
// unicast literal (loopback, unspecified, link-local and multicast rejected).
func validatePushHostIP(host string) error {
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid push host %q: not an IPv4 literal", host)
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return fmt.Errorf("disallowed push host %q", host)
	}
	return nil
}

func validateQuotaSpec(spec *model.QuotaSpec, physical model.PhysicalTotals) error {
	if spec.MCpuLimit < 0 || spec.MvmLimit < 0 || spec.CreationConcurrentNum < 0 {
		return fmt.Errorf("negative quota values are not allowed")
	}
	if r := spec.PausedReleaseRatio; r != nil && (*r < 0 || *r > 1) {
		return fmt.Errorf("paused_resource_release_ratio must be in [0,1], got %v", *r)
	}
	var memMB int64
	if spec.MemLimit != "" {
		var err error
		if memMB, err = parseMemLimitMB(spec.MemLimit); err != nil {
			return err
		}
	}
	if physical.CpuTotal > 0 && spec.MCpuLimit > 0 {
		if limit := int64(float64(physical.CpuTotal) * 1000 * maxCPUOvercommit); spec.MCpuLimit > limit {
			return fmt.Errorf("mcpu_limit %d exceeds guard %d (cpu=%d x %v)", spec.MCpuLimit, limit, physical.CpuTotal, maxCPUOvercommit)
		}
	}
	if physical.MemMBTotal > 0 && memMB > 0 {
		if limit := int64(float64(physical.MemMBTotal) * maxMemOvercommit); memMB > limit {
			return fmt.Errorf("mem_limit %dMB exceeds guard %dMB (mem=%dMB x %v)", memMB, limit, physical.MemMBTotal, maxMemOvercommit)
		}
	}
	return nil
}

// memLimitRe accepts only binary suffixes (Ki/Mi/Gi/Ti); anything else
// silently parses as bytes in cubelet and desyncs spec from actual.
var memLimitRe = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?)(Ti|Gi|Mi|Ki)$`)

// memLimitBytes converts a binary-suffix quantity to bytes.
func memLimitBytes(v string) (float64, error) {
	m := memLimitRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, fmt.Errorf("invalid mem_limit %q: want a binary-suffix quantity, e.g. 256Gi / 262144Mi", v)
	}
	num, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid mem_limit %q: %w", v, err)
	}
	switch m[3] {
	case "Ki":
		num *= 1024
	case "Mi":
		num *= 1024 * 1024
	case "Gi":
		num *= 1024 * 1024 * 1024
	case "Ti":
		num *= 1024 * 1024 * 1024 * 1024
	}
	return num, nil
}

// parseMemLimitMB rounds up to whole MiB so the guard never under-counts.
func parseMemLimitMB(v string) (int64, error) {
	num, err := memLimitBytes(v)
	if err != nil {
		return 0, err
	}
	return int64(num/(1024*1024) + 0.9999), nil
}

// parseMemLimitMBFloor truncates to whole MiB, mirroring cubelet's heartbeat
// reporting (bytes / 1MiB); the drift check must compare against this value.
func parseMemLimitMBFloor(v string) (int64, error) {
	num, err := memLimitBytes(v)
	if err != nil {
		return 0, err
	}
	return int64(num / (1024 * 1024)), nil
}

func specModelToAPI(row *store.NodeQuotaSpec) *model.QuotaSpec {
	return &model.QuotaSpec{
		MCpuLimit:             row.MCpuLimit,
		MemLimit:              row.MemLimit,
		MvmLimit:              row.MvmLimit,
		CreationConcurrentNum: row.CreationConcurrentNum,
		PausedReleaseRatio:    row.PausedReleaseRatio,
		NodeManaged:           row.NodeManaged,
		Revision:              row.Revision,
		UpdatedBy:             row.UpdatedBy,
		UpdatedAt:             row.UpdatedAt,
	}
}

func specAPIToModel(nodeID string, spec *model.QuotaSpec, old *store.NodeQuotaSpec, operator string) *store.NodeQuotaSpec {
	row := &store.NodeQuotaSpec{
		NodeID:                nodeID,
		MCpuLimit:             spec.MCpuLimit,
		MemLimit:              spec.MemLimit,
		MvmLimit:              spec.MvmLimit,
		CreationConcurrentNum: spec.CreationConcurrentNum,
		PausedReleaseRatio:    spec.PausedReleaseRatio,
		Revision:              1,
		UpdatedBy:             operator,
		// Rows written via the node-level API manage all fields.
		NodeManaged: true,
	}
	// The audit detail records this; the store sets its own time.Now()
	// when persisting, so stamp the in-memory row here.
	row.UpdatedAt = time.Now()
	if old != nil {
		row.Revision = old.Revision + 1
	}
	return row
}

func quotaAuditDetail(old *store.NodeQuotaSpec, new *store.NodeQuotaSpec) string {
	type snap struct {
		MCpuLimit             int64    `json:"mcpu_limit"`
		MemLimit              string   `json:"mem_limit"`
		MvmLimit              int64    `json:"mvm_limit"`
		CreationConcurrentNum int64    `json:"creation_concurrent_num"`
		PausedReleaseRatio    *float64 `json:"paused_release_ratio"`
		NodeManaged           bool     `json:"node_managed"`
		Revision              int64    `json:"revision"`
		UpdatedBy             string   `json:"updated_by"`
		UpdatedAt             string   `json:"updated_at,omitempty"`
	}
	detail := struct {
		Old *snap `json:"old,omitempty"`
		New snap  `json:"new"`
	}{New: snap{
		MCpuLimit: new.MCpuLimit, MemLimit: new.MemLimit, MvmLimit: new.MvmLimit,
		CreationConcurrentNum: new.CreationConcurrentNum, PausedReleaseRatio: new.PausedReleaseRatio,
		NodeManaged: new.NodeManaged, Revision: new.Revision, UpdatedBy: new.UpdatedBy,
		UpdatedAt: new.UpdatedAt.Format(time.RFC3339),
	}}
	if old != nil {
		detail.Old = &snap{
			MCpuLimit: old.MCpuLimit, MemLimit: old.MemLimit, MvmLimit: old.MvmLimit,
			CreationConcurrentNum: old.CreationConcurrentNum, PausedReleaseRatio: old.PausedReleaseRatio,
			NodeManaged: old.NodeManaged, Revision: old.Revision, UpdatedBy: old.UpdatedBy,
			UpdatedAt: old.UpdatedAt.Format(time.RFC3339),
		}
	}
	return model.MustJSON(&detail)
}
