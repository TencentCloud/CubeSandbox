// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

// Sentinel errors for node deletion; the handler maps them to HTTP status
// codes via errors.Is.
var (
	ErrNodeNotIsolated    = errors.New("node must be isolated before deletion")
	ErrNodeNotFound       = errors.New("node not found")
	ErrNodeHasSandboxes   = errors.New("node still has sandboxes")
	ErrSandboxCheckFailed = errors.New("verify node sandboxes failed")
)

// SandboxInventoryChecker reports sandboxes on a node via CubeMaster HTTP.
type SandboxInventoryChecker interface {
	CountNodeSandboxes(ctx context.Context, hostID string) (int, error)
}

// DeleteNode removes the node's control-plane metadata. Preconditions:
// the node must be isolated and — unless force — have zero sandboxes.
// Deletes DB rows in one transaction, drops the in-memory snapshot and
// Redis metric, and holds the per-node label lock throughout.
func (svc *NodeService) DeleteNode(ctx context.Context, nodeID string, force bool) (*model.NodeSnapshot, error) {
	if nodeID == "" {
		return nil, ErrNodeIDRequired
	}

	unlock := svc.lockNodeLabels(nodeID)
	defer unlock()

	// Re-read under the lock to observe the latest labels.
	reg, err := svc.store.GetRegistration(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		logging.G(ctx).Errorf("nodemgmt: delete get registration failed: node=%s: %v", nodeID, err)
		return nil, err
	}
	labels, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		logging.G(ctx).Errorf("nodemgmt: delete labels corrupt: node=%s: %v", nodeID, err)
		return nil, fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}
	if !schedulingDisabledFromLabels(labels) {
		return nil, ErrNodeNotIsolated
	}

	// Snapshot the in-memory view before mutation to return to the caller.
	var before *model.NodeSnapshot
	svc.mu.RLock()
	if snap, ok := svc.nodes[nodeID]; ok {
		before = cloneSnapshotWithCurrentHealth(snap)
	}
	svc.mu.RUnlock()

	// Fail closed: any checker error means "cannot verify; refuse to delete".
	// "No sandboxes" is only trustworthy when the cubelet has a recent heartbeat.
	if !force {
		if before == nil || !before.Healthy {
			logging.G(ctx).Warnf("nodemgmt: delete sandbox check skipped: node=%s unhealthy", nodeID)
			return nil, fmt.Errorf("%w: node %s is unhealthy or unreachable; restore Cubelet connectivity and retry, or retry with force=true", ErrSandboxCheckFailed, nodeID)
		}
		if checker := svc.sandboxChecker(); checker != nil {
			count, err := checker.CountNodeSandboxes(ctx, nodeID)
			if err != nil {
				logging.G(ctx).Warnf("nodemgmt: delete sandbox check failed: node=%s: %v", nodeID, err)
				return nil, fmt.Errorf("%w: %v; restore CubeMaster connectivity and retry, or retry with force=true", ErrSandboxCheckFailed, err)
			}
			if count != 0 {
				return nil, fmt.Errorf("%w: %d found; remove them first and retry, or use --force to bypass the inventory check", ErrNodeHasSandboxes, count)
			}
		}
	} else {
		logging.G(ctx).Warnf("nodemgmt: force deleting node without sandbox inventory verification: node=%s", nodeID)
	}

	if err := svc.store.DeleteNode(ctx, nodeID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		logging.G(ctx).Errorf("nodemgmt: delete store failed: node=%s: %v", nodeID, err)
		return nil, err
	}

	// Drop the in-memory snapshot and Redis metric (best-effort; a stale
	// metric expires via TTL or is overwritten by a later registration).
	svc.mu.Lock()
	delete(svc.nodes, nodeID)
	svc.mu.Unlock()
	if err := nodemetric.DeleteNodeMetric(nodeID); err != nil {
		logging.G(ctx).Warnf("nodemgmt: delete node metric failed: node=%s: %v", nodeID, err)
	}

	// Record the deletion as an audit-trail operation (best-effort).
	detail := "force=false"
	if force {
		detail = "force=true"
	}
	if err := svc.recordOperation(ctx, nodeID, model.OpDelete, "internal", detail); err != nil {
		logging.G(ctx).Warnf("nodemgmt: delete record operation failed: node=%s: %v", nodeID, err)
	}

	logging.G(ctx).Infof("nodemgmt: node deleted: node=%s force=%t", nodeID, force)
	return before, nil
}

// schedulingDisabledFromLabels reports whether the node is cordoned,
// read from the DB labels (not the possibly-stale in-memory snapshot).
func schedulingDisabledFromLabels(labels map[string]string) bool {
	return labels[model.LabelSchedulingDisabled] == model.LabelSchedulingDisabledValue
}

// defaultSandboxInventoryChecker is the fallback checker when the service
// has none configured; nil skips the sandbox inventory check in tests.
var defaultSandboxInventoryChecker SandboxInventoryChecker

// SetSandboxInventoryChecker installs the checker used by DeleteNode.
func (svc *NodeService) SetSandboxInventoryChecker(checker SandboxInventoryChecker) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.sandboxCheckerFn = checker
}

// sandboxChecker returns the per-service checker, falling back to the
// package-level default; either may be nil to skip the check.
func (svc *NodeService) sandboxChecker() SandboxInventoryChecker {
	svc.mu.RLock()
	fn := svc.sandboxCheckerFn
	svc.mu.RUnlock()
	if fn != nil {
		return fn
	}
	return defaultSandboxInventoryChecker
}
