// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package models contains database schema and ORM model definitions for Cube Master project.
package models

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

type HostInfo struct {
	gorm.Model
	InsID        string `json:"InstanceID" gorm:"column:ins_id"`
	IP           string `json:"HostIP" gorm:"column:ip"`
	CpuTotal     int    `json:"Cpu" gorm:"column:cpu_total"`
	MemMBTotal   int64  `json:"Mem" gorm:"column:mem_mb_total"`
	Zone         string `json:"Zone" gorm:"column:zone"`
	Region       string `json:"Region" gorm:"column:region"`
	DataDiskGB   int64  `json:"DataDiskGB" gorm:"column:data_disk_gb"`
	SysDiskGB    int64  `json:"SysDiskGB" gorm:"column:sys_disk_gb"`
	InstanceType string `json:"InstanceType" gorm:"column:instance_type"`
	ClusterLabel string `json:"ClusterLabel" gorm:"column:cube_cluster_label"`
	UUID         string `json:"UUID" gorm:"column:uuid"`

	LiveStatus          string `json:"LiveStatus" gorm:"column:live_status"`
	HostStatus          string `json:"HostStatus" gorm:"column:host_status"`
	QuotaCpu            int64  `json:"QuotaCpu" gorm:"column:quota_cpu"`
	QuotaMem            int64  `json:"QuotaMem" gorm:"column:quota_mem_mb"`
	CreateConcurrentNum int64  `json:"CreateConcurrentNum" gorm:"column:create_concurrent_num"`
	MaxMvmNum           int64  `json:"MaxMvmNum" gorm:"column:max_mvm_num"`
	OssClusterLabel     string `json:"OssClusterLabel" gorm:"column:oss_cluster_label"`
}

func (HostInfo) TableName() string {
	return constants.MetadataTableName
}

type HostTypeInfo struct {
	gorm.Model
	InstanceType string `json:"InstanceType" gorm:"column:instance_type"`
	CPUType      string `json:"CPUType" gorm:"column:cpu_type"`
}

func (HostTypeInfo) TableName() string {
	return constants.HostTypeTableName
}

type MachineInfo struct {
	gorm.Model
	InsID              string `json:"InstanceID,omitempty" gorm:"column:ins_id"`
	DeviceClass        string `json:"DeviceClass,omitempty" gorm:"column:device_class"`
	DeviceID           int64  `json:"DeviceId,omitempty" gorm:"column:device_id"`
	HostIP             string `json:"HostIp,omitempty" gorm:"column:host_ip"`
	InstanceFamily     string `json:"InstanceFamily,omitempty" gorm:"column:instance_family"`
	DedicatedClusterId string `json:"DedicatedClusterId,omitempty" gorm:"column:dedicated_cluster_id"`
	VirtualNodeQuota   string `json:"VirtualNodeQuota,omitempty" gorm:"column:virtual_node_quota"`
}

func (MachineInfo) TableName() string {
	return constants.HostSubInfoTableName
}

type InstanceInfo struct {
	gorm.Model
	InsID         string `json:"InstanceID" gorm:"column:ins_id"`
	UUID          string `json:"Uuid" gorm:"column:uuid"`
	HostIP        string `json:"HostIP" gorm:"column:host_ip"`
	HostID        string `json:"HostId" gorm:"column:host_id"`
	InstanceState string `json:"InstanceState" gorm:"column:ins_state"`

	CPU     int64  `json:"Cpu" gorm:"column:cpu"`
	Mem     int64  `json:"Mem" gorm:"column:mem"`
	CPUType string `json:"CpuType" gorm:"column:cpu_type"`
	Zone    string `json:"Zone" gorm:"column:zone"`
	Region  string `json:"Region" gorm:"column:region"`

	ImageID                 string `json:"ImageID" gorm:"column:image_id"`
	SystemDisk              string `json:"SystemDisk" gorm:"column:system_disk"`
	DataDisks               string `json:"DataDisks" gorm:"column:data_disks"`
	PrivateIPAddresses      string `json:"PrivateIpAddresses" gorm:"column:private_ip_addresses"`
	PrivateIP               string `json:"PrivateIp" gorm:"column:private_ip"`
	PrirvateIPCnt           int64  `json:"PrivateIpCnt" gorm:"column:private_ip_cnt"`
	SecurityGroupIDS        string `json:"SecurityGroupIds" gorm:"column:security_ids"`
	VpcID                   string `json:"VpcId" gorm:"column:vpc_id"`
	MacAddress              string `json:"MacAddress" gorm:"column:mac_address"`
	SubnetID                string `json:"SubnetId" gorm:"column:subnet_id"`
	DiskState               string `json:"DiskState" gorm:"column:disk_state"`
	FailMsg                 string `json:"FailMsg" gorm:"column:fail_msg"`
	CamRoleName             string `json:"CamRoleName" gorm:"column:cam_role_name"`
	DisasterRecoverGroupIds string `json:"DisasterRecoverGroupIds" gorm:"column:disaster_recover_group_ids"`
}

func (InstanceInfo) TableName() string {
	return constants.InstanceInfoTableName
}

type InstanceUserData struct {
	gorm.Model
	InsID    string `json:"InsID" gorm:"column:ins_id"`
	UserData string `json:"UserData" gorm:"column:user_data"`
}

func (InstanceUserData) TableName() string {
	return constants.InstanceUserDataTableName
}
