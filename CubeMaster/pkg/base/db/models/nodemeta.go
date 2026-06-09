// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

type NodeRegistration struct {
	gorm.Model
	NodeID              string `gorm:"column:node_id"`
	HostIP              string `gorm:"column:host_ip"`
	GRPCPort            int    `gorm:"column:grpc_port"`
	LabelsJSON          string `gorm:"column:labels_json"`
	CapacityJSON        string `gorm:"column:capacity_json"`
	AllocatableJSON     string `gorm:"column:allocatable_json"`
	InstanceType        string `gorm:"column:instance_type"`
	ClusterLabel        string `gorm:"column:cluster_label"`
	QuotaCPU            int64  `gorm:"column:quota_cpu"`
	QuotaMemMB          int64  `gorm:"column:quota_mem_mb"`
	CreateConcurrentNum int64  `gorm:"column:create_concurrent_num"`
	MaxMvmNum           int64  `gorm:"column:max_mvm_num"`
}

func (NodeRegistration) TableName() string {
	return constants.NodeMetaRegistrationTable
}

type NodeStatus struct {
	gorm.Model
	NodeID             string `gorm:"column:node_id"`
	ConditionsJSON     string `gorm:"column:conditions_json"`
	ImagesJSON         string `gorm:"column:images_json"`
	LocalTemplatesJSON string `gorm:"column:local_templates_json"`
	HeartbeatUnix      int64  `gorm:"column:heartbeat_unix"`
	Healthy            bool   `gorm:"column:healthy"`
}

func (NodeStatus) TableName() string {
	return constants.NodeMetaStatusTable
}

// NodeComponentVersion records the real version of a single component on a
// single node. It intentionally does NOT embed gorm.Model: the table uses a
// (node_id, component) unique key and the writer physically deletes stale
// rows, so a soft-delete column would resurrect tombstoned rows on the next
// upsert and hide them from Find().
type NodeComponentVersion struct {
	ID           uint   `gorm:"primarykey"`
	NodeID       string `gorm:"column:node_id"`
	Component    string `gorm:"column:component"`
	Version      string `gorm:"column:version"`
	Commit       string `gorm:"column:commit"`
	BuildTime    string `gorm:"column:build_time"`
	Source       string `gorm:"column:source"`
	ReportedUnix int64  `gorm:"column:reported_unix"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (NodeComponentVersion) TableName() string {
	return constants.NodeComponentVersionTable
}
