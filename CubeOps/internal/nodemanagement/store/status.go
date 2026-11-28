// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type nodeInventory struct {
	StatusID           uint `gorm:"primaryKey;autoIncrement:false"`
	Digest             string
	ImagesJSON         string
	LocalTemplatesJSON string
}

func (nodeInventory) TableName() string { return "t_cube_node_inventory" }

// UpsertStatus leaves the input unchanged, including its ID and timestamps.
// Split writers lock the status row before publishing inventory in the same transaction.
func (s *gormNodeStore) UpsertStatus(ctx context.Context, status *NodeStatus) error {
	conflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"conditions_json", "images_json", "local_templates_json",
			"heartbeat_unix", "heartbeat_received_unix", "healthy", "updated_at",
		}),
	}
	row := *status
	if !s.splitInventory {
		return s.db.WithContext(ctx).Clauses(conflict).Create(&row).Error
	}

	// A NUL byte cannot occur in JSON text.
	sum := sha256.Sum256([]byte(status.ImagesJSON + "\x00" + status.LocalTemplatesJSON))
	digest := hex.EncodeToString(sum[:])

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The omitted columns default to SQL NULL in the incoming row.
		// Assigning those defaults on conflict also clears old large values.
		write := tx.Clauses(conflict).Omit("ImagesJSON", "LocalTemplatesJSON")
		if err := write.Create(&row).Error; err != nil {
			return err
		}

		var current struct {
			ID     uint
			Digest string
		}
		if err := tx.Model(&NodeStatus{}).
			Select("t_cube_node_status.id, COALESCE(inv.digest, '') AS digest").
			Joins(inventoryJoin).Where("t_cube_node_status.node_id = ?", row.NodeID).
			Take(&current).Error; err != nil {
			return err
		}

		if current.Digest == digest {
			return nil
		}
		inventory := nodeInventory{
			StatusID: current.ID, Digest: digest,
			ImagesJSON: status.ImagesJSON, LocalTemplatesJSON: status.LocalTemplatesJSON,
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "status_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"digest", "images_json", "local_templates_json"}),
		}).Create(&inventory).Error
	})
}

const inventoryJoin = "LEFT JOIN t_cube_node_inventory AS inv ON inv.status_id = t_cube_node_status.id"

func (s *gormNodeStore) statuses(ctx context.Context) *gorm.DB {
	// Legacy writers update only the inline columns during a rolling upgrade.
	// Prefer those whenever present, independently of heartbeat clock skew.
	return s.db.WithContext(ctx).Model(&NodeStatus{}).Joins(inventoryJoin).
		Select(`t_cube_node_status.id, t_cube_node_status.node_id, t_cube_node_status.created_at,
            t_cube_node_status.updated_at, t_cube_node_status.deleted_at, t_cube_node_status.conditions_json,
            t_cube_node_status.heartbeat_unix, t_cube_node_status.heartbeat_received_unix, t_cube_node_status.healthy,
            COALESCE(t_cube_node_status.images_json, inv.images_json) AS images_json,
            COALESCE(t_cube_node_status.local_templates_json, inv.local_templates_json) AS local_templates_json`)
}

func (s *gormNodeStore) GetStatus(ctx context.Context, nodeID string) (*NodeStatus, error) {
	var st NodeStatus
	if err := s.statuses(ctx).Where("t_cube_node_status.node_id = ?", nodeID).First(&st).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &st, nil
}

func (s *gormNodeStore) ListStatuses(ctx context.Context) ([]NodeStatus, error) {
	var sts []NodeStatus
	if err := s.statuses(ctx).Find(&sts).Error; err != nil {
		return nil, err
	}
	return sts, nil
}
