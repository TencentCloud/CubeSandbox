// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

// Orphan replica cleanup sweep.
//
// A template delete now proceeds even when a replica-holding node is
// unreachable: the failed locator's replica row is marked
// cleanup_required=true and survives metadata cleanup (see delete.go). That
// row is the only durable record that the node may still hold the template's
// data. This sweep is the retry loop that finishes the job: it replays the
// cubelet-side cleanup for every such orphan row (its template's definition
// AND snapshot records are already gone) and deletes the row once the node
// confirms the data is gone.
//
// Node identity vs. address: the stored NodeIP can be STALE -- a node that
// was re-deployed (e.g. the cluster moved into Kubernetes) re-registers with
// a new IP under the same node id, and RPCs to the old address time out
// forever (that is exactly how the stuck deletes this fixes were reported).
// The sweep therefore re-resolves the address from the live node cache by
// node id first, so the retry converges once the node is back under ANY
// address; only when the node is absent from the cache does it fall back to
// the stored IP (the node may be briefly unregistered yet reachable).
//
// A row whose node never comes back simply stays: it is small, visible to
// operators, and retried every tick at negligible cost -- the same "tombstone
// until cleaned" discipline the artifact placements follow.

// orphanCleanupReplica is the projection the sweep needs.
type orphanCleanupReplica struct {
	ID         uint   `gorm:"column:id"`
	TemplateID string `gorm:"column:template_id"`
	NodeID     string `gorm:"column:node_id"`
	NodeIP     string `gorm:"column:node_ip"`
}

// Seams, so the sweep can be tested without a database or a live cubelet.
var (
	listOrphanCleanupReplicas = func(ctx context.Context, limit int) ([]orphanCleanupReplica, error) {
		var rows []orphanCleanupReplica
		err := store.db.WithContext(ctx).
			Table(constants.TemplateReplicaTableName+" AS r").
			Select("r.id, r.template_id, r.node_id, r.node_ip").
			Where("r.cleanup_required = ?", true).
			Where("NOT EXISTS (SELECT 1 FROM " + constants.TemplateDefinitionTableName + " d WHERE d.template_id = r.template_id AND d.deleted_at IS NULL)").
			Where("NOT EXISTS (SELECT 1 FROM " + constants.SnapshotTableName + " s WHERE s.snapshot_id = r.template_id AND s.deleted_at IS NULL)").
			Limit(limit).
			Find(&rows).Error
		return rows, err
	}
	deleteReplicaRowByID = func(ctx context.Context, id uint) error {
		return store.db.WithContext(ctx).Unscoped().
			Table(constants.TemplateReplicaTableName).
			Where("id = ?", id).
			Delete(nil).Error
	}
	// cleanupOrphanReplicaOnNode retries the cubelet cleanup for one locator.
	// It goes through cleanupTemplateReplicasWithLocators (single-element
	// batch) so the ignorable-error classification (already-gone data counts
	// as converged) stays in exactly one place.
	cleanupOrphanReplicaOnNode = func(ctx context.Context, templateID string, locator templateCleanupLocator, backend string) error {
		return cleanupTemplateReplicasWithLocators(ctx, templateID, []templateCleanupLocator{locator}, backend)
	}
)

// reconcileOrphanReplicaCleanups replays the cubelet-side template cleanup
// for every replica row a finished delete left behind. Runs inside the
// image-job reconcile pass (advisory lock held by the caller).
func reconcileOrphanReplicaCleanups(ctx context.Context) error {
	if !isReady() {
		return nil
	}
	rows, err := listOrphanCleanupReplicas(ctx, imageJobReconcileBatch)
	if err != nil {
		return fmt.Errorf("scan orphan cleanup replicas: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "image_job_reconcile"})
	logger.Warnf("found %d orphan replica cleanup(s) left by earlier deletes; retrying cubelet cleanup", len(rows))

	for _, row := range rows {
		locator := templateCleanupLocator{NodeID: row.NodeID, NodeIP: row.NodeIP}
		// Re-resolve the address from the live node cache: the stored IP may
		// predate a re-deployment, while the node id is the stable identity.
		if row.NodeID != "" {
			if n, ok := localcache.GetNode(row.NodeID); ok && n != nil && n.HostIP() != "" {
				locator.NodeIP = n.HostIP()
			}
		}
		// Backend hint is empty: the definition row (which carried it) is
		// gone by construction, and cubelet derives the physical layout from
		// its local catalog keyed by templateID (v4/v5 contract).
		if err := cleanupOrphanReplicaOnNode(ctx, row.TemplateID, locator, ""); err != nil {
			// Leave the row: the next tick retries. Warn (not Error) because
			// a dead node is an expected, possibly long-lived condition.
			logger.Warnf("orphan replica cleanup still failing: template=%s node=%s err=%v", row.TemplateID, row.NodeID, err)
			continue
		}
		if err := deleteReplicaRowByID(ctx, row.ID); err != nil {
			logger.Warnf("orphan replica cleanup succeeded on node but row delete failed: id=%d template=%s err=%v", row.ID, row.TemplateID, err)
			continue
		}
		logger.Infof("orphan replica cleanup converged: template=%s node=%s", row.TemplateID, row.NodeID)
	}
	return nil
}
