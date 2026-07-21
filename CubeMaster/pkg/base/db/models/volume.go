// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"time"
)

// VolumeTableName is the canonical MySQL table name for volume records.
const VolumeTableName = "t_cube_volume"

// MaxVolumeNameLen is the maximum length of a customer-supplied volume name.
// Must match the varchar(128) columns for volume_id and name in t_cube_volume.
const MaxVolumeNameLen = 128

// VolumeRecord persists a single managed volume.
//
// Field layout:
//   - VolumeID    : stable business key (same as Name when the caller supplies a name)
//   - Name        : human-readable label; UNIQUE, cannot be reused while the row exists
//   - Driver      : plugin name (ControllerPlugin.Name()) used to create the volume
//   - Token       : per-volume credential returned by the plugin; may be empty
//   - RefCount    : number of nodes currently referencing (mounting) the volume
//
// Deletion is hard-delete (row removed). There is no deleted_at column.
type VolumeRecord struct {
	ID        uint      `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
	VolumeID  string    `gorm:"column:volume_id;uniqueIndex:uniq_volume_id;not null;size:128"`
	Name      string    `gorm:"column:name;uniqueIndex:uniq_volume_name;not null;default:'';size:128"`
	Driver    string    `gorm:"column:driver;not null;default:''"`
	Token     string    `gorm:"column:token;not null;default:''"`
	// RefCount tracks how many nodes currently have the volume attached
	// (mounted by at least one sandbox). It is maintained from the node-level
	// 0→1 / 1→0 transitions Cubelet reports on sandbox create/destroy. A volume
	// with RefCount > 0 must not be deleted.
	RefCount int64 `gorm:"column:refcount;not null;default:0"`
}

// TableName implements schema.Tabler so GORM uses the canonical table name.
func (VolumeRecord) TableName() string {
	return VolumeTableName
}
