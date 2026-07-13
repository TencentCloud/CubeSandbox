// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

type SnapshotRuntimeRef struct {
	gorm.Model
	SnapshotID  string     `json:"snapshot_id" gorm:"column:snapshot_id"`
	SandboxID   string     `json:"sandbox_id" gorm:"column:sandbox_id"`
	NodeID      string     `json:"node_id" gorm:"column:node_id"`
	NodeIP      string     `json:"node_ip" gorm:"column:node_ip"`
	BindingType string     `json:"binding_type" gorm:"column:binding_type"`
	MemoryVol   string     `json:"memory_vol" gorm:"column:memory_vol"`
	MemoryDev   string     `json:"memory_dev" gorm:"column:memory_dev"`
	RootfsVol   string     `json:"rootfs_vol" gorm:"column:rootfs_vol"`
	SandboxGen  uint32     `json:"sandbox_gen" gorm:"column:sandbox_gen"`
	Status      string     `json:"status" gorm:"column:status"`
	AttachedAt  time.Time  `json:"attached_at" gorm:"column:attached_at"`
	ReleasedAt  *time.Time `json:"released_at" gorm:"column:released_at"`
	LastSeenAt  *time.Time `json:"last_seen_at" gorm:"column:last_seen_at"`
	LastError   string     `json:"last_error" gorm:"column:last_error"`
}

func (SnapshotRuntimeRef) TableName() string {
	return constants.SnapshotRuntimeRefTableName
}

type SnapshotRuntimeActive struct {
	SandboxID   string     `json:"sandbox_id" gorm:"column:sandbox_id;primaryKey"`
	BindingType string     `json:"binding_type" gorm:"column:binding_type;primaryKey"`
	SnapshotID  string     `json:"snapshot_id" gorm:"column:snapshot_id"`
	NodeID      string     `json:"node_id" gorm:"column:node_id"`
	NodeIP      string     `json:"node_ip" gorm:"column:node_ip"`
	MemoryVol   string     `json:"memory_vol" gorm:"column:memory_vol"`
	RootfsVol   string     `json:"rootfs_vol" gorm:"column:rootfs_vol"`
	SandboxGen  uint32     `json:"sandbox_gen" gorm:"column:sandbox_gen"`
	AttachedAt  time.Time  `json:"attached_at" gorm:"column:attached_at"`
	LastSeenAt  *time.Time `json:"last_seen_at" gorm:"column:last_seen_at"`
	LastError   string     `json:"last_error" gorm:"column:last_error"`
	CreatedAt   time.Time  `json:"created_at" gorm:"column:created_at"`
	UpdatedAt   time.Time  `json:"updated_at" gorm:"column:updated_at"`
}

func (SnapshotRuntimeActive) TableName() string {
	return constants.SnapshotRuntimeActiveTableName
}
