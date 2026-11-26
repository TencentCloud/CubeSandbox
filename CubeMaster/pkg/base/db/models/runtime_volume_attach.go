// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	RuntimeAttachPending    = "pending"
	RuntimeAttachPublished  = "published"
	RuntimeAttachNotStarted = "not_started"
	RuntimeAttachRolledBack = "rolled_back"
)

// RuntimeVolumeAttach retains one request's original binding and outcome.
// Pending requests also own a reservation and the exact sandbox spec marker.
type RuntimeVolumeAttach struct {
	RequestKey     string `gorm:"column:request_key;size:64;primaryKey"`
	SandboxID      string `gorm:"column:sandbox_id;size:128;not null"`
	RequestID      string `gorm:"column:request_id;size:256;not null"`
	ExecutionID    string `gorm:"column:execution_id;size:256;not null"`
	VolumeID       string `gorm:"column:volume_id;size:128;not null"`
	ContainerPath  string `gorm:"column:container_path;size:4096;not null"`
	Readonly       bool   `gorm:"column:readonly;not null"`
	SpecRowID      uint   `gorm:"column:spec_row_id;not null"`
	VolumeRecordID uint   `gorm:"column:volume_record_id;not null"`
	ClaimToken     string `gorm:"column:claim_token;size:36;not null"`
	Outcome        string `gorm:"column:outcome;size:16;not null"`
}

func (RuntimeVolumeAttach) TableName() string {
	return "t_cube_runtime_volume_attach"
}

// RuntimeVolumeAttachKey preserves case-sensitive request identity in SQL.
// Attach request validation rejects NUL in either input.
func RuntimeVolumeAttachKey(sandboxID, requestID string) string {
	hash := sha256.Sum256([]byte(sandboxID + "\x00" + requestID))
	return hex.EncodeToString(hash[:])
}
