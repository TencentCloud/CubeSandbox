// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"gorm.io/gorm"
)

var (
	// ErrLabelsJSONCorrupt: isolation must not overwrite corrupt labels_json.
	ErrLabelsJSONCorrupt = errors.New("node labels_json is corrupt")
	// ErrSchedulingLabelRejected: cubelet must not set the control-plane cordon key.
	ErrSchedulingLabelRejected = errors.New("cubelet must not set scheduling-disabled label")
)

func (s *service) lockNodeLabels(nodeID string) func() {
	v, _ := s.labelWriteLocks.LoadOrStore(nodeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// countUserLabels excludes the control-plane cordon key from the per-node quota.
func countUserLabels(labels map[string]string) int {
	n := len(labels)
	if _, ok := labels[constants.LabelSchedulingDisabled]; ok {
		n--
	}
	return n
}

// SetNodeSchedulingDisabled cordons (disabled=true) or uncordons a node.
// Idempotent; always republishes localcache so a stale local view is repaired.
func SetNodeSchedulingDisabled(ctx context.Context, nodeID string, disabled bool) (*NodeSnapshot, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("node_id is required")
	}
	unlock := global.lockNodeLabels(nodeID)
	defer unlock()

	var nodeLabels map[string]string
	var changed bool
	if err := global.db.Transaction(func(tx *gorm.DB) error {
		existing, err := readLabelsJSONForUpdate(tx, nodeID)
		if err != nil {
			return err
		}
		_, has := existing[constants.LabelSchedulingDisabled]
		switch {
		case disabled && (!has || existing[constants.LabelSchedulingDisabled] != constants.LabelSchedulingDisabledValue):
			existing[constants.LabelSchedulingDisabled] = constants.LabelSchedulingDisabledValue
			changed = true
		case !disabled && has:
			delete(existing, constants.LabelSchedulingDisabled)
			changed = true
		}
		if changed {
			if err := tx.Table(constants.NodeMetaRegistrationTable).
				Where("node_id = ?", nodeID).
				Updates(map[string]interface{}{
					"labels_json": mustJSON(existing),
					"updated_at":  time.Now(),
				}).Error; err != nil {
				return err
			}
		}
		nodeLabels = existing
		return nil
	}); err != nil {
		log.G(ctx).Warnf("node isolation write failed node_id=%s disabled=%v err=%v", nodeID, disabled, err)
		return nil, err
	}

	snap := global.ensureNode(nodeID)
	global.mu.Lock()
	snap.Labels = cloneStringMap(nodeLabels)
	snap.labelsJSONCorrupt = false
	global.mu.Unlock()
	syncLocalcache(snap)

	out := cloneSnapshotWithCurrentHealth(snap, time.Now())
	log.G(ctx).Infof("node isolation write node_id=%s disabled=%v changed=%v scheduling_disabled=%v",
		nodeID, disabled, changed, out.SchedulingDisabled)
	return out, nil
}

// stripAndPreserveSchedulingLabel merges cubelet labels while keeping the
// control-plane cordon key from DB (cubelet cannot create/overwrite/delete it).
func stripAndPreserveSchedulingLabel(existing, cubeletLabels map[string]string) map[string]string {
	ctrlVal, hasCtrl := existing[constants.LabelSchedulingDisabled]
	for k, v := range cubeletLabels {
		if k == constants.LabelSchedulingDisabled {
			continue
		}
		existing[k] = v
	}
	if hasCtrl {
		existing[constants.LabelSchedulingDisabled] = ctrlVal
	} else {
		delete(existing, constants.LabelSchedulingDisabled)
	}
	return existing
}

func snapshotSchedulingDisabled(snap *NodeSnapshot) bool {
	if snap == nil || snap.labelsJSONCorrupt {
		return true
	}
	return node.DecodeSchedulingDisabled(snap.Labels)
}
