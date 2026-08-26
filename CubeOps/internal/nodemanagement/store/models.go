// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"time"

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
	HostFactsJSON       string `gorm:"column:host_facts_json"`
	// Redundant columns for QueryHostFactCandidates filtering
	CPUIDHash         string `gorm:"column:cpuid_hash"`
	HostKernelRelease string `gorm:"column:host_kernel_release"`
}

func (NodeRegistration) TableName() string {
	return "t_cube_node_registration"
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
	return "t_cube_node_status"
}

// HostInfo mirrors the legacy host registry (t_cube_host_info), read-only.
type HostInfo struct {
	ID                  uint   `gorm:"column:id"`
	InsID               string `gorm:"column:ins_id"`
	IP                  string `gorm:"column:ip"`
	Region              string `gorm:"column:region"`
	Zone                string `gorm:"column:zone"`
	UUID                string `gorm:"column:uuid"`
	InstanceType        string `gorm:"column:instance_type"`
	ClusterLabel        string `gorm:"column:cube_cluster_label"`
	OssClusterLabel     string `gorm:"column:oss_cluster_label"`
	HostStatus          string `gorm:"column:host_status"`
	QuotaCPU            int64  `gorm:"column:quota_cpu"`
	QuotaMemMB          int64  `gorm:"column:quota_mem_mb"`
	CpuTotal            int64  `gorm:"column:cpu_total"`
	MemMBTotal          int64  `gorm:"column:mem_mb_total"`
	DataDiskGB          int64  `gorm:"column:data_disk_gb"`
	SysDiskGB           int64  `gorm:"column:sys_disk_gb"`
	CreateConcurrentNum int64  `gorm:"column:create_concurrent_num"`
	MaxMvmNum           int64  `gorm:"column:max_mvm_num"`
}

func (HostInfo) TableName() string { return "t_cube_host_info" }

// HostTypeInfo mirrors the legacy instance_type→cpu_type mapping table.
type HostTypeInfo struct {
	ID           uint   `gorm:"column:id"`
	InstanceType string `gorm:"column:instance_type"`
	CPUType      string `gorm:"column:cpu_type"`
}

func (HostTypeInfo) TableName() string { return "t_cube_host_type" }

// SubHostInfo mirrors the legacy machine sub-info table (t_cube_sub_host_info).
type SubHostInfo struct {
	ID                 uint   `gorm:"column:id"`
	InsID              string `gorm:"column:ins_id"`
	HostIP             string `gorm:"column:host_ip"`
	DeviceClass        string `gorm:"column:device_class"`
	DeviceID           int64  `gorm:"column:device_id"`
	InstanceFamily     string `gorm:"column:instance_family"`
	DedicatedClusterID string `gorm:"column:dedicated_cluster_id"`
	VirtualNodeQuota   string `gorm:"column:virtual_node_quota"`
}

func (SubHostInfo) TableName() string { return "t_cube_sub_host_info" }

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
	return "t_cube_node_component_version"
}

type NodeOperation struct {
	ID        uint      `gorm:"primarykey"`
	NodeID    string    `gorm:"column:node_id"`
	Type      string    `gorm:"column:type"`
	Operator  string    `gorm:"column:operator"`
	Detail    string    `gorm:"column:detail"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (NodeOperation) TableName() string {
	return "t_cube_node_operation"
}
