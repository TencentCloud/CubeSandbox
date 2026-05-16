// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

type RootfsArtifact struct {
	gorm.Model
	ArtifactID              string `json:"artifact_id" gorm:"column:artifact_id"`
	TemplateSpecFingerprint string `json:"template_spec_fingerprint" gorm:"column:template_spec_fingerprint"`
	SourceImageRef          string `json:"source_image_ref" gorm:"column:source_image_ref"`
	SourceImageDigest       string `json:"source_image_digest" gorm:"column:source_image_digest"`
	MasterNodeID            string `json:"master_node_id" gorm:"column:master_node_id"`
	MasterNodeIP            string `json:"master_node_ip" gorm:"column:master_node_ip"`
	Ext4Path                string `json:"ext4_path" gorm:"column:ext4_path"`
	Ext4SHA256              string `json:"ext4_sha256" gorm:"column:ext4_sha256"`
	Ext4SizeBytes           int64  `json:"ext4_size_bytes" gorm:"column:ext4_size_bytes"`
	ImageConfigJSON         string `json:"image_config_json" gorm:"column:image_config_json"`
	GeneratedRequestJSON    string `json:"generated_request_json" gorm:"column:generated_request_json"`
	WritableLayerSize       string `json:"writable_layer_size" gorm:"column:writable_layer_size"`
	DownloadToken           string `json:"download_token" gorm:"column:download_token"`
	Status                  string `json:"status" gorm:"column:status"`
	LastError               string `json:"last_error" gorm:"column:last_error"`
	GCDeadline              int64  `json:"gc_deadline" gorm:"column:gc_deadline"`
}

func (RootfsArtifact) TableName() string {
	return constants.RootfsArtifactTableName
}

type TemplateImageJob struct {
	gorm.Model
	JobID                   string `json:"job_id" gorm:"column:job_id"`
	TemplateID              string `json:"template_id" gorm:"column:template_id"`
	RequestID               string `json:"request_id" gorm:"column:request_id"`
	AttemptNo               int32  `json:"attempt_no" gorm:"column:attempt_no"`
	RetryOfJobID            string `json:"retry_of_job_id" gorm:"column:retry_of_job_id"`
	Operation               string `json:"operation" gorm:"column:operation"`
	RedoMode                string `json:"redo_mode" gorm:"column:redo_mode"`
	RedoScopeJSON           string `json:"redo_scope_json" gorm:"column:redo_scope_json"`
	ResumePhase             string `json:"resume_phase" gorm:"column:resume_phase"`
	NodeID                  string `json:"node_id" gorm:"column:node_id"`
	NodeIP                  string `json:"node_ip" gorm:"column:node_ip"`
	SnapshotPath            string `json:"snapshot_path" gorm:"column:snapshot_path"`
	ArtifactID              string `json:"artifact_id" gorm:"column:artifact_id"`
	TemplateSpecFingerprint string `json:"template_spec_fingerprint" gorm:"column:template_spec_fingerprint"`
	SourceImageRef          string `json:"source_image_ref" gorm:"column:source_image_ref"`
	SourceImageDigest       string `json:"source_image_digest" gorm:"column:source_image_digest"`
	WritableLayerSize       string `json:"writable_layer_size" gorm:"column:writable_layer_size"`
	InstanceType            string `json:"instance_type" gorm:"column:instance_type"`
	NetworkType             string `json:"network_type" gorm:"column:network_type"`
	Status                  string `json:"status" gorm:"column:status"`
	Phase                   string `json:"phase" gorm:"column:phase"`
	Progress                int32  `json:"progress" gorm:"column:progress"`
	ErrorMessage            string `json:"error_message" gorm:"column:error_message"`
	ExpectedNodeCount       int32  `json:"expected_node_count" gorm:"column:expected_node_count"`
	ReadyNodeCount          int32  `json:"ready_node_count" gorm:"column:ready_node_count"`
	FailedNodeCount         int32  `json:"failed_node_count" gorm:"column:failed_node_count"`
	TemplateStatus          string `json:"template_status" gorm:"column:template_status"`
	ArtifactStatus          string `json:"artifact_status" gorm:"column:artifact_status"`
	RequestJSON             string `json:"request_json" gorm:"column:request_json"`
	ResultJSON              string `json:"result_json" gorm:"column:result_json"`
}

func (TemplateImageJob) TableName() string {
	return constants.TemplateImageJobTableName
}
