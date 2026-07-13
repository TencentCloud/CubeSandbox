// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

type TemplateDefinition struct {
	gorm.Model
	TemplateID                string `json:"template_id" gorm:"column:template_id"`
	InstanceType              string `json:"instance_type" gorm:"column:instance_type"`
	Version                   string `json:"version" gorm:"column:version"`
	Status                    string `json:"status" gorm:"column:status"`
	Kind                      string `json:"kind" gorm:"column:kind"`
	OriginSandboxID           string `json:"origin_sandbox_id" gorm:"column:origin_sandbox_id"`
	OriginNodeID              string `json:"origin_node_id" gorm:"column:origin_node_id"`
	DisplayName               string `json:"display_name" gorm:"column:display_name"`
	StorageBackend            string `json:"storage_backend" gorm:"column:storage_backend"`
	Retain                    bool   `json:"retain" gorm:"column:retain"`
	RootfsSizeBytesAtSnapshot uint64 `json:"rootfs_size_bytes_at_snapshot" gorm:"column:rootfs_size_bytes_at_snapshot"`
	RootfsArtifactID          string `json:"rootfs_artifact_id" gorm:"column:rootfs_artifact_id"`
	RequestJSON               string `json:"request_json" gorm:"column:request_json"`
	LastError                 string `json:"last_error" gorm:"column:last_error"`
}

func (TemplateDefinition) TableName() string {
	return constants.TemplateDefinitionTableName
}

// TemplateReplica is the master-side row that tracks where a template
// definition has been materialized. v5: physical columns
// (snapshot_path, rootfs_vol, memory_vol, rootfs_kind, memory_kind,
// rootfs_dev, memory_dev, meta_dir, build_rootfs_vol) were removed from
// both the struct and the DB schema (see migrations). Cubelet's local
// snapshot catalog is the single source of truth for physical layout,
// keyed by templateID/snapshotID.
type TemplateReplica struct {
	gorm.Model
	TemplateID        string `json:"template_id" gorm:"column:template_id"`
	NodeID            string `json:"node_id" gorm:"column:node_id"`
	NodeIP            string `json:"node_ip" gorm:"column:node_ip"`
	InstanceType      string `json:"instance_type" gorm:"column:instance_type"`
	Spec              string `json:"spec" gorm:"column:spec"`
	Status            string `json:"status" gorm:"column:status"`
	Phase             string `json:"phase" gorm:"column:phase"`
	ArtifactID        string `json:"artifact_id" gorm:"column:artifact_id"`
	LastJobID         string `json:"last_job_id" gorm:"column:last_job_id"`
	LastErrorPhase    string `json:"last_error_phase" gorm:"column:last_error_phase"`
	CleanupRequired   bool   `json:"cleanup_required" gorm:"column:cleanup_required"`
	ErrorMessage      string `json:"error_message" gorm:"column:error_message"`
	GuestImageVersion string `json:"guest_image_version" gorm:"column:guest_image_version"`
	AgentVersion      string `json:"agent_version" gorm:"column:agent_version"`
	KernelVersion     string `json:"kernel_version" gorm:"column:kernel_version"`
	CompatStatus      string `json:"compat_status" gorm:"column:compat_status"`
	CompatPolicy      string `json:"compat_policy" gorm:"column:compat_policy"`
	CompatCheckedUnix int64  `json:"compat_checked_unix" gorm:"column:compat_checked_unix"`
}

func (TemplateReplica) TableName() string {
	return constants.TemplateReplicaTableName
}
