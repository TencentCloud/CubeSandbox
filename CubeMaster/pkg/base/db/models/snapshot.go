// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

// SnapshotRecord is a user Commit snapshot. Independent of
// t_cube_template_definition — templates stay on that table unchanged.
type SnapshotRecord struct {
	gorm.Model
	SnapshotID                string `json:"snapshot_id" gorm:"column:snapshot_id"`
	OriginSandboxID           string `json:"origin_sandbox_id" gorm:"column:origin_sandbox_id"`
	OriginNodeID              string `json:"origin_node_id" gorm:"column:origin_node_id"`
	OriginNodeIP              string `json:"origin_node_ip" gorm:"column:origin_node_ip"`
	DisplayName               string `json:"display_name" gorm:"column:display_name"`
	InstanceType              string `json:"instance_type" gorm:"column:instance_type"`
	Version                   string `json:"version" gorm:"column:version"`
	Status                    string `json:"status" gorm:"column:status"`
	LastError                 string `json:"last_error" gorm:"column:last_error"`
	RootfsSizeBytesAtSnapshot uint64 `json:"rootfs_size_bytes_at_snapshot" gorm:"column:rootfs_size_bytes_at_snapshot"`
	OriginHostFactsJSON       string `json:"origin_host_facts_json" gorm:"column:origin_host_facts_json"`
	RequestJSON               string `json:"request_json" gorm:"column:request_json"`
	Backend                   string `json:"backend" gorm:"column:backend"`
	RemoteStatus              string `json:"remote_status" gorm:"column:remote_status"`
	ExportUUIDs               string `json:"export_uuids" gorm:"column:export_uuids"`
}

func (SnapshotRecord) TableName() string {
	return constants.SnapshotTableName
}

// PauseSnapshotRecord is the thin control-plane binding
// sandbox ↔ pause-snap ↔ node. Hard-deleted after Resume or Delete.
type PauseSnapshotRecord struct {
	ID                  uint      `gorm:"primaryKey"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
	SnapshotID          string    `json:"snapshot_id" gorm:"column:snapshot_id"`
	SandboxID           string    `json:"sandbox_id" gorm:"column:sandbox_id"`
	NodeID              string    `json:"node_id" gorm:"column:node_id"`
	NodeIP              string    `json:"node_ip" gorm:"column:node_ip"`
	InstanceType        string    `json:"instance_type" gorm:"column:instance_type"`
	Status              string    `json:"status" gorm:"column:status"`
	LastError           string    `json:"last_error" gorm:"column:last_error"`
	PluginVolumeIDs     string    `json:"plugin_volume_ids" gorm:"column:plugin_volume_ids"`
	Backend             string    `json:"backend" gorm:"column:backend"`
	RemoteStatus        string    `json:"remote_status" gorm:"column:remote_status"`
	OriginHostFactsJSON string    `json:"origin_host_facts_json" gorm:"column:origin_host_facts_json"`
	ExportUUIDs         string    `json:"export_uuids" gorm:"column:export_uuids"`
}

func (PauseSnapshotRecord) TableName() string {
	return constants.PauseSnapshotTableName
}
