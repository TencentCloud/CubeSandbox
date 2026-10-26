// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
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
			Columns:   []clause.Column{{Name: "artifact_id"}, {Name: "node_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"node_ip"}),
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

// deleteArtifactNodePlacementsTx removes all placement rows for artifactID. It
// must run in the same transaction (TX2) that deletes the artifact row.
func deleteArtifactNodePlacementsTx(tx *gorm.DB, artifactID string) error {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil
	}
	return tx.Table(constants.ArtifactNodePlacementTableName).
		Where("artifact_id = ?", artifactID).Delete(&models.ArtifactNodePlacement{}).Error
}
