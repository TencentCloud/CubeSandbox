// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// upsertArtifactNodePlacement records that artifactID is physically present on
// the given node. It is idempotent: re-distribution refreshes node_ip without
// failing on the (artifact_id, node_id) primary key. nodeID is required (it is
// part of the primary key); calls with an empty nodeID are ignored.
func upsertArtifactNodePlacement(ctx context.Context, artifactID, nodeID, nodeIP string) error {
	artifactID = strings.TrimSpace(artifactID)
	nodeID = strings.TrimSpace(nodeID)
	if artifactID == "" || nodeID == "" {
		return nil
	}
	row := &models.ArtifactNodePlacement{
		ArtifactID: artifactID,
		NodeID:     nodeID,
		NodeIP:     strings.TrimSpace(nodeIP),
		CreatedAt:  time.Now().Unix(),
	}
	return store.db.WithContext(ctx).Table(constants.ArtifactNodePlacementTableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "artifact_id"}, {Name: "node_id"}},
			// deferred_at resets: a re-distribution makes the row live again.
			// AssignmentColumns emits `col = excluded.col`, so the zero value in
			// row still overwrites; Assignments() would drop it and leak the old one.
			DoUpdates: clause.AssignmentColumns([]string{"node_ip", "deferred_at"}),
		}).Create(row).Error
}

// listArtifactNodePlacementsTx enumerates every node that holds (or held)
// artifactID, using the supplied transaction so it observes the same snapshot
// as the surrounding locked decision.
func listArtifactNodePlacementsTx(tx *gorm.DB, artifactID string) ([]models.ArtifactNodePlacement, error) {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil, nil
	}
	var rows []models.ArtifactNodePlacement
	err := tx.Table(constants.ArtifactNodePlacementTableName).
		Where("artifact_id = ?", artifactID).Find(&rows).Error
	return rows, err
}

// listArtifactNodePlacements is listArtifactNodePlacementsTx outside a
// transaction, for callers that only need the current coverage (the jfs
// replica-backfill sweep computes its push set from these rows).
func listArtifactNodePlacements(ctx context.Context, artifactID string) ([]models.ArtifactNodePlacement, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	return listArtifactNodePlacementsTx(store.db.WithContext(ctx), artifactID)
}

// deleteArtifactNodePlacementsTx removes artifactID's placement rows except
// keepNodeIDs (deferred ones); call it in the finalising transaction.
func deleteArtifactNodePlacementsTx(tx *gorm.DB, artifactID string, keepNodeIDs []string) error {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil
	}
	q := tx.Table(constants.ArtifactNodePlacementTableName).Where("artifact_id = ?", artifactID)
	if len(keepNodeIDs) > 0 {
		q = q.Where("node_id NOT IN ?", keepNodeIDs)
	}
	return q.Delete(&models.ArtifactNodePlacement{}).Error
}

// markArtifactNodePlacementsDeferredTx stamps deferred_at on the given rows.
// Rows already deferred keep their original timestamp.
func markArtifactNodePlacementsDeferredTx(tx *gorm.DB, artifactID string, nodeIDs []string, now int64) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	return tx.Table(constants.ArtifactNodePlacementTableName).
		Where("artifact_id = ? AND node_id IN ? AND deferred_at = 0", strings.TrimSpace(artifactID), nodeIDs).
		Update("deferred_at", now).Error
}

const (
	// legacyPlacementNodeIDPrefix marks placement ids synthesised by the
	// placement-table backfill for replica rows that only carried an IP.
	legacyPlacementNodeIDPrefix = "ip:"

	// placementAbsenceGrace is how long a node must stay absent before cleanup
	// is deferred; equals the async-operation timeout (> one GC period/RPC).
	placementAbsenceGrace = snapshotOperationTimeout
)

// placementNodeAbsentLongEnough reports whether the node has been missing for
// the full grace period; legacy "ip:" ids and unhealthy-but-present do not count.
func placementNodeAbsentLongEnough(p *models.ArtifactNodePlacement, now time.Time) bool {
	if p == nil || p.NodeID == "" || strings.HasPrefix(p.NodeID, legacyPlacementNodeIDPrefix) {
		return false
	}
	since, ok := localcache.AbsentSince(p.NodeID)
	return ok && now.Sub(since) >= placementAbsenceGrace
}

// deferredScanCursor is the last (artifact_id, node_id) scanned. Pages rotate
// from it so rows that keep failing or being skipped cannot pin the LIMIT window.
var deferredScanCursor struct {
	mu                 sync.Mutex
	artifactID, nodeID string
}

// listDeferredArtifactNodePlacements returns the next page of up to limit
// deferred rows whose node is in the view, wrapping to the start after the tail.
func listDeferredArtifactNodePlacements(ctx context.Context, limit int) ([]models.ArtifactNodePlacement, error) {
	nodeIDs := localcache.NodeIDs()
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	deferredScanCursor.mu.Lock()
	afterArtifact, afterNode := deferredScanCursor.artifactID, deferredScanCursor.nodeID
	deferredScanCursor.mu.Unlock()

	rows, err := listDeferredPageAfter(ctx, nodeIDs, afterArtifact, afterNode, limit)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 && afterArtifact != "" {
		if rows, err = listDeferredPageAfter(ctx, nodeIDs, "", "", limit); err != nil {
			return nil, err
		}
	}
	deferredScanCursor.mu.Lock()
	defer deferredScanCursor.mu.Unlock()
	if len(rows) < limit {
		deferredScanCursor.artifactID, deferredScanCursor.nodeID = "", ""
	} else {
		last := rows[len(rows)-1]
		deferredScanCursor.artifactID, deferredScanCursor.nodeID = last.ArtifactID, last.NodeID
	}
	return rows, nil
}

// listDeferredPageAfter reads one page strictly after the (artifact_id, node_id)
// cursor; an empty artifact id starts from the beginning.
func listDeferredPageAfter(ctx context.Context, nodeIDs []string, afterArtifact, afterNode string, limit int) ([]models.ArtifactNodePlacement, error) {
	q := store.db.WithContext(ctx).Table(constants.ArtifactNodePlacementTableName).
		Where("deferred_at > 0 AND node_id IN ?", nodeIDs)
	if afterArtifact != "" {
		q = q.Where("(artifact_id > ? OR (artifact_id = ? AND node_id > ?))", afterArtifact, afterArtifact, afterNode)
	}
	var rows []models.ArtifactNodePlacement
	err := q.Order("artifact_id asc, node_id asc").Limit(limit).Find(&rows).Error
	return rows, err
}

// artifactRowIsLive reports whether the artifact row exists outside a cleanup
// state, so its files must not be touched.
func artifactRowIsLive(ctx context.Context, artifactID string) (bool, error) {
	var status string
	res := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Select("status").Where("artifact_id = ?", artifactID).Limit(1).Scan(&status)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	switch status {
	case ArtifactStatusCleanupPending, ArtifactStatusDeleting, ArtifactStatusFailed, ArtifactStatusOrphaned:
		return false, nil
	}
	return true, nil
}

// deleteDeferredArtifactNodePlacement drops one deferred row after the node
// confirmed the artifact is gone; the deferred_at guard spares re-registered rows.
func deleteDeferredArtifactNodePlacement(ctx context.Context, artifactID, nodeID string) error {
	return store.db.WithContext(ctx).Table(constants.ArtifactNodePlacementTableName).
		Where("artifact_id = ? AND node_id = ? AND deferred_at > 0", artifactID, nodeID).
		Delete(&models.ArtifactNodePlacement{}).Error
}

// reconcileDeferredArtifactPlacements retries the node-side cleanup that
// cleanupArtifactFully postponed once the node is back; success deletes the row.
func reconcileDeferredArtifactPlacements(ctx context.Context) {
	rows, err := listDeferredArtifactNodePlacements(ctx, artifactGCMaxPerPass)
	if err != nil {
		log.G(ctx).Warnf("artifact gc: scan deferred placements failed: %v", err)
		return
	}
	logger := log.G(ctx).WithFields(map[string]any{"component": "artifact_gc"})
	for i := range rows {
		p := &rows[i]
		cached, ok := localcache.GetNode(p.NodeID)
		if !ok || cached == nil || cached.HostIP() == "" {
			continue
		}
		if live, err := artifactRowIsLive(ctx, p.ArtifactID); err != nil {
			logger.Warnf("deferred placement cleanup: liveness check failed, skipping: artifact=%s node=%s err=%v", p.ArtifactID, p.NodeID, err)
			continue
		} else if live {
			continue // resurrected by a rebuild: do not touch its files
		}
		target := &node.Node{InsID: p.NodeID, IP: cached.HostIP()}
		inUse, err := destroyArtifactOnNode(ctx, p.ArtifactID, "", target)
		if inUse || err != nil {
			logger.Warnf("deferred placement cleanup pending: artifact=%s node=%s inUse=%v err=%v", p.ArtifactID, p.NodeID, inUse, err)
			continue
		}
		if err := deleteDeferredArtifactNodePlacement(ctx, p.ArtifactID, p.NodeID); err != nil {
			logger.Warnf("deferred placement cleanup: row delete failed: artifact=%s node=%s err=%v", p.ArtifactID, p.NodeID, err)
			continue
		}
		logger.Infof("deferred placement cleanup converged: artifact=%s node=%s", p.ArtifactID, p.NodeID)
	}
}
