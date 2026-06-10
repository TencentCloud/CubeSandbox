// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

const (
	snapshotReconcilerInterval  = 5 * time.Minute
	snapshotOperationTimeout    = 15 * time.Minute
	snapshotMetricsTTL          = 10 * time.Minute
	snapshotWarnThreshold       = 70
	snapshotRejectThreshold     = 85
	snapshotDeleteOnlyThreshold = 95
)

func SnapshotOperationTimeout() time.Duration {
	return snapshotOperationTimeout
}

// requiredSnapshotMetricKeys mirrors the keys cubecow's reflink backend
// emits via cubecow_get_metrics(). The legacy dm-thin pool_* keys are
// gone — reflink has no metadata pool and no notion of multiple pools,
// so we derive a single usage_pct from total_bytes/used_bytes (which
// come from statvfs() on the FS hosting cubecow's reflink root_dir).
var requiredSnapshotMetricKeys = []string{
	"total_bytes",
	"used_bytes",
	"volume_count",
	"snapshot_count",
}

type snapshotStorageMode string

const (
	snapshotStorageModeHealthy    snapshotStorageMode = "healthy"
	snapshotStorageModeWarn       snapshotStorageMode = "warn"
	snapshotStorageModeReject     snapshotStorageMode = "reject"
	snapshotStorageModeDeleteOnly snapshotStorageMode = "delete_only"
	snapshotStorageModeUnknown    snapshotStorageMode = "unknown"
)

type snapshotStorageState struct {
	NodeID        string
	NodeIP        string
	UsagePct      uint64
	Mode          snapshotStorageMode
	LastError     string
	LastUpdatedAt time.Time
}

func failedReplicaStatus(replica models.TemplateReplica, message string) ReplicaStatus {
	status := replicaModelToStatus(replica)
	status.Status = ReplicaStatusFailed
	status.Phase = ReplicaPhaseFailed
	status.CleanupRequired = true
	status.ErrorMessage = message
	return status
}

var (
	snapshotReconcilerOnce sync.Once
	snapshotStorageCache   = struct {
		sync.RWMutex
		byNode map[string]snapshotStorageState
	}{
		byNode: make(map[string]snapshotStorageState),
	}
)

func startSnapshotReconciler(ctx context.Context) {
	snapshotReconcilerOnce.Do(func() {
		go func() {
			runSnapshotReconcilerPass(detachTemplateImageJobContext(ctx, "snapshot_reconciler", nil))
			ticker := time.NewTicker(snapshotReconcilerInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runSnapshotReconcilerPass(detachTemplateImageJobContext(ctx, "snapshot_reconciler", nil))
				}
			}
		}()
	})
}

func runSnapshotReconcilerPass(ctx context.Context) {
	if !isReady() {
		return
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "snapshot_reconciler"})
	if err := reconcileSnapshotDefinitionTimeouts(ctx); err != nil {
		logger.Warnf("reconcile snapshot definition timeouts failed: %v", err)
	}
	if err := reconcileSnapshotReplicaPresence(ctx); err != nil {
		logger.Warnf("reconcile snapshot replica presence failed: %v", err)
	}
	if err := reconcileSnapshotRuntimeRefs(ctx); err != nil {
		logger.Warnf("reconcile snapshot runtime refs failed: %v", err)
	}
	if err := refreshSnapshotStorageMetrics(ctx); err != nil {
		logger.Warnf("refresh snapshot storage metrics failed: %v", err)
	}
}

func ensureSnapshotNodeWritable(ctx context.Context, nodeID, nodeIP string, allowDelete bool) error {
	state, err := getOrRefreshSnapshotStorageState(ctx, nodeID, nodeIP)
	if err != nil {
		if allowDelete {
			return nil
		}
		return fmt.Errorf("%w: snapshot storage metrics unavailable for node %s: %v", ErrTemplateAttemptInProgress, firstNonEmpty(nodeID, nodeIP), err)
	}
	if localcache.IsSnapshotStorageWriteAllowed(string(state.Mode)) {
		return nil
	}
	switch state.Mode {
	case snapshotStorageModeReject, snapshotStorageModeDeleteOnly:
		if allowDelete {
			return nil
		}
		return fmt.Errorf("%w: snapshot operations are blocked on node %s (usage=%d%%)", ErrTemplateAttemptInProgress, firstNonEmpty(nodeID, nodeIP), state.UsagePct)
	default:
		if allowDelete {
			return nil
		}
		return fmt.Errorf("%w: snapshot storage state is unknown for node %s", ErrTemplateAttemptInProgress, firstNonEmpty(nodeID, nodeIP))
	}
}

func getOrRefreshSnapshotStorageState(ctx context.Context, nodeID, nodeIP string) (snapshotStorageState, error) {
	key := firstNonEmpty(nodeID, nodeIP)
	if key == "" {
		return snapshotStorageState{}, fmt.Errorf("node id or ip is required")
	}
	snapshotStorageCache.RLock()
	state, ok := snapshotStorageCache.byNode[key]
	snapshotStorageCache.RUnlock()
	// unknown mode bypasses TTL: every access forces a synchronous refresh so that
	// transient cold-start failures (e.g. grpc dial timeout) are not cached for 10min,
	// which would otherwise block createSnapshot/rollback for too long (issue 130409).
	if ok && state.Mode != snapshotStorageModeUnknown && time.Since(state.LastUpdatedAt) <= snapshotMetricsTTL {
		return state, nil
	}
	return refreshSingleSnapshotStorageState(ctx, nodeID, nodeIP)
}

func reconcileSnapshotDefinitionTimeouts(ctx context.Context) error {
	var defs []models.TemplateDefinition
	if err := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("kind = ? AND status IN ? AND updated_at < ?", TemplateKindSnapshot, []string{StatusCreating, StatusDeleting}, time.Now().Add(-snapshotOperationTimeout)).
		Find(&defs).Error; err != nil {
		return err
	}
	for _, def := range defs {
		active, err := getActiveSnapshotJobByResourceID(ctx, def.TemplateID)
		if err == nil && active != nil {
			continue
		}
		if err != nil && !errorsIsRecordNotFound(err) {
			return err
		}
		lastError := fmt.Sprintf("snapshot %s remained in %s beyond %s", def.TemplateID, def.Status, snapshotOperationTimeout)
		if err := updateDefinitionFields(ctx, def.TemplateID, map[string]any{
			"status":     StatusFailed,
			"last_error": lastError,
		}); err != nil {
			return err
		}
	}
	return nil
}

func reconcileSnapshotReplicaPresence(ctx context.Context) error {
	var reconcileErr error
	orphanCount := 0
	defer setSnapshotOrphanGauge(orphanCount)
	var defs []models.TemplateDefinition
	if err := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("kind = ? AND status IN ?", TemplateKindSnapshot, []string{StatusReady, StatusFailed, StatusDeleting}).
		Find(&defs).Error; err != nil {
		return err
	}
	for _, def := range defs {
		replicas, err := ListReplicas(ctx, def.TemplateID)
		if err != nil {
			return err
		}
		for _, model := range replicas {
			replica := replicaModelToStatus(model)
			hostIP := resolveNodeIP(replica.NodeID, replica.NodeIP)
			if hostIP == "" {
				err := fmt.Errorf("snapshot %s replica on node %s has no reachable node address", def.TemplateID, firstNonEmpty(replica.NodeID, replica.NodeIP))
				reconcileErr = errors.Join(reconcileErr, err)
				continue
			}
			// Authoritative existence check now happens via cubelet's local
			// snapshot catalog. Master no longer carries physical refs on
			// snapshot replicas, so we ask the node directly.
			rsp, err := cubelet.GetLocalSnapshot(ctx, cubelet.GetCubeletAddr(hostIP), &cubeboxv1.GetLocalSnapshotRequest{
				RequestID:  uuid.NewString(),
				SnapshotID: def.TemplateID,
			})
			if err != nil {
				msg := "get local snapshot failed"
				if strings.TrimSpace(err.Error()) != "" {
					msg = err.Error()
				}
				cacheSnapshotStorageState(replica.NodeID, hostIP, snapshotStorageState{
					NodeID:        replica.NodeID,
					NodeIP:        hostIP,
					Mode:          snapshotStorageModeUnknown,
					LastError:     msg,
					LastUpdatedAt: time.Now(),
				})
				reconcileErr = errors.Join(reconcileErr, fmt.Errorf("snapshot %s reconcile on node %s failed: %s", def.TemplateID, firstNonEmpty(replica.NodeID, hostIP), msg))
				continue
			}
			retCode := cubeleterrorcode.ErrorCode_Success
			retMsg := ""
			if rsp.GetRet() != nil {
				retCode = rsp.GetRet().GetRetCode()
				retMsg = rsp.GetRet().GetRetMsg()
			}
			switch retCode {
			case cubeleterrorcode.ErrorCode_Success:
				if rsp.GetSnapshot() == nil || strings.TrimSpace(rsp.GetSnapshot().GetSnapshotID()) == "" {
					orphanCount++
					msg := fmt.Sprintf("snapshot %s missing from local catalog on node %s", def.TemplateID, firstNonEmpty(replica.NodeID, hostIP))
					_ = UpsertReplica(ctx, def.TemplateID, model.InstanceType, failedReplicaStatus(model, msg))
					_ = updateDefinitionFields(ctx, def.TemplateID, map[string]any{
						"status":     StatusFailed,
						"last_error": msg,
					})
				}
			case cubeleterrorcode.ErrorCode_PreConditionFailed:
				orphanCount++
				msg := fmt.Sprintf("snapshot %s missing from local catalog on node %s: %s", def.TemplateID, firstNonEmpty(replica.NodeID, hostIP), retMsg)
				_ = cleanupTemplateReplicasWithLocators(ctx, def.TemplateID, []templateCleanupLocator{{
					NodeID: model.NodeID,
					NodeIP: model.NodeIP,
				}})
				_ = UpsertReplica(ctx, def.TemplateID, model.InstanceType, failedReplicaStatus(model, msg))
				_ = updateDefinitionFields(ctx, def.TemplateID, map[string]any{
					"status":     StatusFailed,
					"last_error": msg,
				})
			default:
				reconcileErr = errors.Join(reconcileErr, fmt.Errorf("snapshot %s reconcile on node %s returned ret=%d %s", def.TemplateID, firstNonEmpty(replica.NodeID, hostIP), retCode, retMsg))
			}
		}
	}
	return reconcileErr
}

func reconcileSnapshotRuntimeRefs(ctx context.Context) error {
	nodes := localcache.GetHealthyNodesByInstanceType(-1, cubeboxv1.InstanceType_cubebox.String())
	var reconcileErr error
	for i := range nodes {
		nodeID := strings.TrimSpace(nodes[i].ID())
		nodeIP := strings.TrimSpace(nodes[i].HostIP())
		if nodeID == "" && nodeIP == "" {
			continue
		}
		rsp := sandbox.ListSandbox(ctx, &sandboxtypes.ListCubeSandboxReq{
			RequestID:    uuid.NewString(),
			HostID:       nodeID,
			InstanceType: cubeboxv1.InstanceType_cubebox.String(),
			StartIdx:     1,
			Size:         1000,
		})
		if rsp == nil || rsp.Ret == nil {
			err := fmt.Errorf("list sandbox returned empty response for node %s", firstNonEmpty(nodeID, nodeIP))
			_ = UpdateSnapshotRuntimeRefsNodeError(ctx, nodeID, nodeIP, err.Error())
			reconcileErr = err
			continue
		}
		if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
			err := fmt.Errorf("list sandbox failed for node %s: %s", firstNonEmpty(nodeID, nodeIP), rsp.Ret.RetMsg)
			_ = UpdateSnapshotRuntimeRefsNodeError(ctx, nodeID, nodeIP, err.Error())
			reconcileErr = err
			continue
		}
		observed := make([]SnapshotRuntimeRefInfo, 0, len(rsp.Data))
		for _, item := range rsp.Data {
			if ref, ok := SnapshotRuntimeRefFromSandboxBriefData(item); ok {
				observed = append(observed, ref)
				continue
			}
			templateID := ""
			if item != nil {
				templateID = strings.TrimSpace(item.TemplateID)
			}
			if templateID == "" {
				continue
			}
			kind, err := GetTemplateKind(ctx, templateID)
			if err != nil || !strings.EqualFold(kind, TemplateKindSnapshot) {
				continue
			}
			observed = append(observed, SnapshotRuntimeRefInfo{
				SnapshotID: templateID,
				SandboxID:  item.SandboxID,
				NodeID:     item.HostID,
				NodeIP:     item.HostIP,
			})
		}
		if err := RefreshSnapshotRuntimeRefsFromNode(ctx, nodeID, nodeIP, observed); err != nil {
			reconcileErr = err
		}
	}
	return reconcileErr
}

func refreshSnapshotStorageMetrics(ctx context.Context) error {
	type refreshTarget struct {
		nodeID string
		nodeIP string
	}
	targets := make([]refreshTarget, 0)
	seen := make(map[string]struct{})

	addTarget := func(nodeID, nodeIP string) {
		nodeID = strings.TrimSpace(nodeID)
		nodeIP = strings.TrimSpace(nodeIP)
		if nodeID == "" && nodeIP == "" {
			return
		}
		key := firstNonEmpty(nodeID, nodeIP)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, refreshTarget{nodeID: nodeID, nodeIP: nodeIP})
	}

	nodes := localcache.GetHealthyNodesByInstanceType(-1, cubeboxv1.InstanceType_cubebox.String())
	for i := range nodes {
		addTarget(nodes[i].ID(), nodes[i].HostIP())
	}
	// Include already-cached nodes in the refresh set so that during cold start,
	// when the healthy node table (populated via redis) is temporarily empty and the
	// reconciler would otherwise spin harmlessly, unknown nodes still get a chance to
	// self-heal. We iterate all cached nodes (not just unknown ones) so that healthy/warn
	// entries are also periodically updated.
	for _, cached := range localcache.ListSnapshotStorageStates() {
		addTarget(cached.NodeID, cached.NodeIP)
	}

	var refreshErr error
	for _, t := range targets {
		if _, err := refreshSingleSnapshotStorageState(ctx, t.nodeID, t.nodeIP); err != nil {
			refreshErr = err
		}
	}
	return refreshErr
}

func refreshSingleSnapshotStorageState(ctx context.Context, nodeID, nodeIP string) (snapshotStorageState, error) {
	hostIP := resolveNodeIP(nodeID, nodeIP)
	if hostIP == "" {
		return snapshotStorageState{}, fmt.Errorf("missing node address")
	}
	rsp, err := cubelet.GetStorageMetrics(ctx, cubelet.GetCubeletAddr(hostIP), &cubeboxv1.GetStorageMetricsRequest{
		RequestID: uuid.NewString(),
	})
	if err != nil {
		state := newUnknownSnapshotStorageState(nodeID, hostIP, err.Error())
		cacheSnapshotStorageState(nodeID, hostIP, state)
		return state, err
	}
	if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		msg := "get storage metrics failed"
		if rsp.GetRet() != nil {
			msg = rsp.GetRet().GetRetMsg()
		}
		state := newUnknownSnapshotStorageState(firstNonEmpty(nodeID, rsp.GetNodeId()), hostIP, msg)
		cacheSnapshotStorageState(nodeID, hostIP, state)
		return state, fmt.Errorf("%s", msg)
	}
	if err := validateSnapshotMetrics(rsp.GetMetrics()); err != nil {
		state := newUnknownSnapshotStorageState(firstNonEmpty(nodeID, rsp.GetNodeId()), hostIP, err.Error())
		cacheSnapshotStorageState(nodeID, hostIP, state)
		return state, err
	}
	usagePct := derivedUsagePct(rsp.GetMetrics())
	state := snapshotStorageState{
		NodeID:        firstNonEmpty(nodeID, rsp.GetNodeId()),
		NodeIP:        hostIP,
		UsagePct:      usagePct,
		Mode:          classifySnapshotStorageMode(usagePct),
		LastUpdatedAt: time.Now(),
	}
	cacheSnapshotStorageState(nodeID, hostIP, state)
	return state, nil
}

func validateSnapshotMetrics(metrics map[string]uint64) error {
	for _, key := range requiredSnapshotMetricKeys {
		if _, ok := metrics[key]; !ok {
			return fmt.Errorf("snapshot storage metrics missing key %s", key)
		}
	}
	return nil
}

func cacheSnapshotStorageState(nodeID, nodeIP string, state snapshotStorageState) {
	snapshotStorageCache.Lock()
	defer snapshotStorageCache.Unlock()
	if nodeID != "" {
		snapshotStorageCache.byNode[nodeID] = state
	}
	if nodeIP != "" {
		snapshotStorageCache.byNode[nodeIP] = state
	}
	localcache.SetSnapshotStorageState(localcache.SnapshotStorageState{
		NodeID:        firstNonEmpty(state.NodeID, nodeID),
		NodeIP:        firstNonEmpty(state.NodeIP, nodeIP),
		UsagePct:      state.UsagePct,
		Mode:          string(state.Mode),
		LastError:     state.LastError,
		LastUpdatedAt: state.LastUpdatedAt.Unix(),
	})
	setSnapshotStorageModeMetric(state)
}

func newUnknownSnapshotStorageState(nodeID, nodeIP, lastError string) snapshotStorageState {
	return snapshotStorageState{
		NodeID:        nodeID,
		NodeIP:        nodeIP,
		Mode:          snapshotStorageModeUnknown,
		LastError:     lastError,
		LastUpdatedAt: time.Now(),
	}
}

func classifySnapshotStorageMode(usagePct uint64) snapshotStorageMode {
	switch {
	case usagePct >= snapshotDeleteOnlyThreshold:
		return snapshotStorageModeDeleteOnly
	case usagePct >= snapshotRejectThreshold:
		return snapshotStorageModeReject
	case usagePct >= snapshotWarnThreshold:
		return snapshotStorageModeWarn
	default:
		return snapshotStorageModeHealthy
	}
}

// derivedUsagePct converts cubecow's reflink filesystem statvfs output
// into a 0-100 usage percent. Returns 0 if total_bytes is missing or
// zero so we never divide by zero or amplify a temporary metric gap
// into a synthetic 100%-full reading.
func derivedUsagePct(metrics map[string]uint64) uint64 {
	total := metrics["total_bytes"]
	if total == 0 {
		return 0
	}
	used := metrics["used_bytes"]
	if used > total {
		used = total
	}
	return used * 100 / total
}

func resolveNodeIP(nodeID, nodeIP string) string {
	if strings.TrimSpace(nodeIP) != "" {
		return strings.TrimSpace(nodeIP)
	}
	if strings.TrimSpace(nodeID) == "" {
		return ""
	}
	if node, ok := localcache.GetNode(nodeID); ok && node != nil {
		return strings.TrimSpace(node.HostIP())
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func errorsIsRecordNotFound(err error) bool {
	return err != nil && errors.Is(err, gorm.ErrRecordNotFound)
}
