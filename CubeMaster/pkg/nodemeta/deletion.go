// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"errors"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNodeNotIsolated = errors.New("node must be isolated before deletion")
	ErrNodeNotFound    = errors.New("node not found")
)

// DeleteNode removes the node's current control-plane metadata. A later
// Cubelet restart may register the same node ID again. The node must first be
// cordoned. The service layer is responsible for confirming that the Cubelet
// has no sandboxes before calling this function.
func DeleteNode(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return fmt.Errorf("node_id is required")
	}
	unlock := global.lockNodeLabels(nodeID)
	defer unlock()

	var reg models.NodeRegistration
	if err := global.db.WithContext(ctx).Where("node_id = ?", nodeID).Take(&reg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		return err
	}
	labels, err := parseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}
	if !schedulingDisabledFromLabels(labels) {
		return ErrNodeNotIsolated
	}

	if err := global.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var reg models.NodeRegistration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("node_id = ?", nodeID).Take(&reg).Error; err != nil {
			return err
		}
		lockedLabels, err := parseLabelsJSON(reg.LabelsJSON)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
		}
		if !schedulingDisabledFromLabels(lockedLabels) {
			return ErrNodeNotIsolated
		}
		if err := tx.Unscoped().Where("node_id = ?", nodeID).Delete(&models.NodeStatus{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", nodeID).Delete(&models.NodeComponentVersion{}).Error; err != nil {
			return err
		}
		return tx.Unscoped().Where("node_id = ?", nodeID).Delete(&models.NodeRegistration{}).Error
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
		}
		return err
	}

	global.mu.Lock()
	delete(global.nodes, nodeID)
	global.mu.Unlock()
	evictDeletedNode(ctx, nodeID)
	log.G(ctx).Infof("node deleted node_id=%s", nodeID)
	return nil
}

func schedulingDisabledFromLabels(labels map[string]string) bool {
	return labels[constants.LabelSchedulingDisabled] == constants.LabelSchedulingDisabledValue
}

func evictDeletedNode(ctx context.Context, nodeID string) {
	localcache.EvictNode(nodeID)
	if err := localcache.DeleteNodeMetric(ctx, nodeID); err != nil {
		log.G(ctx).Warnf("node metric cleanup failed after deletion node_id=%s err=%v", nodeID, err)
	}
}
