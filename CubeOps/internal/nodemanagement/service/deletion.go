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

// DeleteNode removes the node's control-plane metadata. The node must be
// isolated and — unless force — have zero sandboxes. Deletes DB rows in one
// transaction and drops the Redis snapshot/metric.
func (svc *NodeService) DeleteNode(ctx context.Context, nodeID string, force bool) (*model.NodeSnapshot, error) {
	if nodeID == "" {
		return nil, ErrNodeIDRequired
	}

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

	before, err := svc.getNodeFromRedisOrDB(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		return nil, err
	}

	if !force {
		if !before.Healthy {
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

	nodemetric.DeleteNodeSnapshot(nodeID)
	if err := nodemetric.DeleteNodeMetric(nodeID); err != nil {
		logging.G(ctx).Warnf("nodemgmt: delete node metric failed: node=%s: %v", nodeID, err)
	}

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

func schedulingDisabledFromLabels(labels map[string]string) bool {
	return labels[model.LabelSchedulingDisabled] == model.LabelSchedulingDisabledValue
}

var defaultSandboxInventoryChecker SandboxInventoryChecker

func (svc *NodeService) SetSandboxInventoryChecker(checker SandboxInventoryChecker) {
	svc.sandboxCheckerFn = checker
}

func (svc *NodeService) sandboxChecker() SandboxInventoryChecker {
	if svc.sandboxCheckerFn != nil {
		return svc.sandboxCheckerFn
	}
	return defaultSandboxInventoryChecker
}
