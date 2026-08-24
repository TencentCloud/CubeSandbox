// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"time"
)

type ResourceSnapshot struct {
	MilliCPU int64 `json:"milli_cpu,omitempty"`
	MemoryMB int64 `json:"memory_mb,omitempty"`
}

type ComponentVersion struct {
	Component string `json:"component"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	Source    string `json:"source,omitempty"`
	Variant   string `json:"variant,omitempty"`
}

// HostFacts carries the static host identity (CPU, kernel, KVM ABI) used to
// judge cross-node snapshot restore compatibility.
type HostFacts struct {
	CPUVendor             string `json:"cpu_vendor,omitempty"`
	CPUModel              string `json:"cpu_model,omitempty"`
	CPUIDHash             string `json:"cpuid_hash,omitempty"`
	HostKernelRelease     string `json:"host_kernel_release,omitempty"`
	HostKernelFingerprint string `json:"host_kernel_fingerprint,omitempty"`
	KVMAPIVersion         int    `json:"kvm_api_version,omitempty"`
	KVMModuleFingerprint  string `json:"kvm_module_fingerprint,omitempty"`
	KVMModuleTaint        string `json:"kvm_module_taint,omitempty"`
	// KVMModuleScanned is a transient signal (not persisted): mergeIncomingHostFacts
	// uses it to distinguish "module unloaded" from "read gap".
	KVMModuleScanned bool `json:"kvm_module_scanned,omitempty"`
}

// IsZero reports whether no meaningful host fact was collected.
func (f *HostFacts) IsZero() bool {
	if f == nil {
		return true
	}
	return f.CPUVendor == "" && f.CPUModel == "" && f.CPUIDHash == "" &&
		f.HostKernelRelease == "" && f.HostKernelFingerprint == "" && f.KVMAPIVersion == 0 &&
		f.KVMModuleFingerprint == "" && f.KVMModuleTaint == ""
}

type ContainerImage struct {
	Names     []string `json:"names,omitempty"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	MediaType string   `json:"media_type,omitempty"`
}

type LocalTemplate struct {
	TemplateID string `json:"template_id,omitempty"`
	ID         string `json:"id,omitempty"`
	Media      string `json:"media,omitempty"`
	Path       string `json:"path,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
}

type NodeCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	LastHeartbeatTime  *time.Time `json:"lastHeartbeatTime,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
	Reason             string     `json:"reason,omitempty"`
	Message            string     `json:"message,omitempty"`
}

// RegisterNodeRequest is sent by cubelet on first registration.
type RegisterNodeRequest struct {
	RequestID           string             `json:"requestID,omitempty"`
	NodeID              string             `json:"node_id,omitempty"`
	HostIP              string             `json:"host_ip,omitempty"`
	GRPCPort            int                `json:"grpc_port,omitempty"`
	Labels              map[string]string  `json:"labels,omitempty"`
	Capacity            ResourceSnapshot   `json:"capacity,omitempty"`
	Allocatable         ResourceSnapshot   `json:"allocatable,omitempty"`
	InstanceType        string             `json:"instance_type,omitempty"`
	ClusterLabel        string             `json:"cluster_label,omitempty"`
	QuotaCPU            int64              `json:"quota_cpu,omitempty"`
	QuotaMemMB          int64              `json:"quota_mem_mb,omitempty"`
	CreateConcurrentNum int64              `json:"create_concurrent_num,omitempty"`
	MaxMvmNum           int64              `json:"max_mvm_num,omitempty"`
	Versions            []ComponentVersion `json:"versions,omitempty"`
	InventoryIncomplete bool               `json:"inventory_incomplete,omitempty"`
	HostFacts           *HostFacts         `json:"host_facts,omitempty"`
}

// UpdateNodeStatusRequest is the cubelet heartbeat payload.
type UpdateNodeStatusRequest struct {
	RequestID           string              `json:"requestID,omitempty"`
	Conditions          []NodeCondition     `json:"conditions,omitempty"`
	Images              []ContainerImage    `json:"images,omitempty"`
	LocalTemplates      []LocalTemplate     `json:"local_templates,omitempty"`
	HeartbeatTime       time.Time           `json:"heartbeat_time,omitempty"`
	Allocated           *AllocatedResources `json:"allocated,omitempty"`
	DiskUsage           *DiskUsage          `json:"disk_usage,omitempty"`
	MetricTime          time.Time           `json:"metric_time,omitempty"`
	Versions            []ComponentVersion  `json:"versions,omitempty"`
	InventoryIncomplete bool                `json:"inventory_incomplete,omitempty"`
	HostFacts           *HostFacts          `json:"host_facts,omitempty"`
}

// AllocatedResources carries quota usage reported by cubelet.
type AllocatedResources struct {
	MilliCPU      int64 `json:"milli_cpu,omitempty"`
	MemoryMB      int64 `json:"memory_mb,omitempty"`
	MvmNum        int64 `json:"mvm_num,omitempty"`
	MvmRunningNum int64 `json:"mvm_running_num,omitempty"`
	NicQueues     int64 `json:"nic_queues,omitempty"`
	DataDiskMB    int64 `json:"data_disk_mb,omitempty"`
	StorageDiskMB int64 `json:"storage_disk_mb,omitempty"`
}

// DiskUsage carries filesystem fill ratios (0~100).
type DiskUsage struct {
	DataDiskUsagePer    float64 `json:"data_disk_usage_per,omitempty"`
	StorageDiskUsagePer float64 `json:"storage_disk_usage_per,omitempty"`
	SysDiskUsagePer     float64 `json:"sys_disk_usage_per,omitempty"`
}

// NodeSnapshot is the authoritative per-node view owned by CubeOps.
type NodeSnapshot struct {
	NodeID              string             `json:"node_id,omitempty"`
	HostIP              string             `json:"host_ip,omitempty"`
	GRPCPort            int                `json:"grpc_port,omitempty"`
	Labels              map[string]string  `json:"labels,omitempty"`
	Capacity            ResourceSnapshot   `json:"capacity,omitempty"`
	Allocatable         ResourceSnapshot   `json:"allocatable,omitempty"`
	InstanceType        string             `json:"instance_type,omitempty"`
	ClusterLabel        string             `json:"cluster_label,omitempty"`
	QuotaCPU            int64              `json:"quota_cpu,omitempty"`
	QuotaMemMB          int64              `json:"quota_mem_mb,omitempty"`
	CreateConcurrentNum int64              `json:"create_concurrent_num,omitempty"`
	MaxMvmNum           int64              `json:"max_mvm_num,omitempty"`
	Conditions          []NodeCondition    `json:"conditions,omitempty"`
	Images              []ContainerImage   `json:"images,omitempty"`
	LocalTemplates      []LocalTemplate    `json:"local_templates,omitempty"`
	Versions            []ComponentVersion `json:"versions,omitempty"`
	HeartbeatTime       time.Time          `json:"heartbeat_time,omitempty"`
	HostFacts           *HostFacts         `json:"host_facts,omitempty"`
	ReportedReady       bool               `json:"reported_ready,omitempty"`
	Healthy             bool               `json:"healthy"`
	UnhealthyReason     string             `json:"unhealthy_reason,omitempty"`
	SchedulingDisabled  bool               `json:"scheduling_disabled"`
	Score               float64            `json:"score,omitempty"`
	MetricUpdate        time.Time          `json:"metric_update,omitempty"`
	MetricLocalUpdateAt time.Time          `json:"metric_local_update_at,omitempty"`

	// Static scheduler fields recovered from the legacy host registry tables.
	Zone                  string  `json:"zone,omitempty"`
	Region                string  `json:"region,omitempty"`
	CPUType               string  `json:"cpu_type,omitempty"`
	DeviceClass           string  `json:"device_class,omitempty"`
	DeviceID              int64   `json:"device_id,omitempty"`
	MachineHostIP         string  `json:"machine_host_ip,omitempty"`
	InstanceFamily        string  `json:"instance_family,omitempty"`
	DedicatedClusterID    string  `json:"dedicated_cluster_id,omitempty"`
	VirtualNodeQuotaArray []int64 `json:"virtual_node_quota_array,omitempty"`
	SystemDiskSize        int64   `json:"system_disk_size,omitempty"`
	DataDiskSize          int64   `json:"data_disk_size,omitempty"`

	// Resource usage observed by cubelet and mirrored into the snapshot so that
	// the node view API exposes the same values as CubeMaster's Redis-driven
	// metric path.
	QuotaCpuUsage       int64   `json:"quota_cpu_usage,omitempty"`
	QuotaMemUsage       int64   `json:"quota_mem_mb_usage,omitempty"`
	MvmNum              int64   `json:"mvm_num,omitempty"`
	DataDiskUsagePer    float64 `json:"data_disk_usage_per,omitempty"`
	StorageDiskUsagePer float64 `json:"storage_disk_usage_per,omitempty"`
	SysDiskUsagePer     float64 `json:"sys_disk_usage_per,omitempty"`
	NicQueues           int64   `json:"nic_queues,omitempty"`

	VersionsHash      string `json:"versions_hash,omitempty"`
	LabelsJSONCorrupt bool   `json:"labels_json_corrupt,omitempty"`
}

// UpdateNodeLabelsRequest is used by the admin label API.
type UpdateNodeLabelsRequest struct {
	Labels map[string]string `json:"labels"`
}

// VersionMatrix is the aggregated component version report.
type VersionMatrix struct {
	ControlPlane map[string]string      `json:"control_plane"`
	Components   []ComponentMatrixEntry `json:"components"`
	Nodes        []NodeVersionEntry     `json:"nodes"`
}

type ComponentMatrixEntry struct {
	Component        string             `json:"component"`
	DeclaredVersion  string             `json:"declared_version"`
	DeclaredVersions []string           `json:"declared_versions"`
	Consistent       bool               `json:"consistent"`
	Versions         []VersionNodeGroup `json:"versions"`
}

type VersionNodeGroup struct {
	Version string   `json:"version"`
	Nodes   []string `json:"nodes"`
}

type NodeComponentVersion struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Declared  bool   `json:"declared"`
}

type NodeVersionEntry struct {
	NodeID     string                 `json:"node_id"`
	Healthy    bool                   `json:"healthy"`
	Components []NodeComponentVersion `json:"components"`
}

type NodeOperation struct {
	ID        int64     `json:"id,omitempty"`
	NodeID    string    `json:"node_id"`
	Type      string    `json:"type"`
	Operator  string    `json:"operator,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SchedulerNode is the shape exported to CubeMaster (matches CubeMaster pkg/base/node.Node).
type SchedulerNode struct {
	Index                 int                `json:"Index,omitempty"`
	InsID                 string             `json:"InstanceID,omitempty"`
	UUID                  string             `json:"uuid,omitempty"`
	IP                    string             `json:"IP,omitempty"`
	CpuTotal              int                `json:"CpuTotal,omitempty"`
	MemMBTotal            int64              `json:"MemMBTotal,omitempty"`
	Zone                  string             `json:"Zone,omitempty"`
	Region                string             `json:"Region,omitempty"`
	SystemDiskSize        int64              `json:"SystemDiskSize,omitempty"`
	DataDiskSize          int64              `json:"DataDiskSize,omitempty"`
	CPUType               string             `json:"CpuType,omitempty"`
	ClusterLabel          string             `json:"ClusterLabel,omitempty"`
	InstanceType          string             `json:"InstanceType,omitempty"`
	OssClusterLabel       string             `json:"OssClusterLabel,omitempty"`
	DeviceClass           string             `json:"DeviceClass,omitempty"`
	DeviceID              int64              `json:"DeviceId,omitempty"`
	MachineHostIP         string             `json:"MachineHostIP,omitempty"`
	InstanceFamily        string             `json:"InstanceFamily,omitempty"`
	DedicatedClusterId    string             `json:"DedicatedClusterId,omitempty"`
	VirtualNodeQuotaArray []int64            `json:"VirtualNodeQuotaArray,omitempty"`
	HostStatus            string             `json:"HostStatus,omitempty"`
	CreateConcurrentNum   int64              `json:"CreateConcurrentNum,omitempty"`
	MaxMvmLimit           int64              `json:"MaxMvmLimit,omitempty"`
	QuotaCpu              int64              `json:"QuotaCpu,omitempty"`
	QuotaMem              int64              `json:"QuotaMem,omitempty"`
	MetaDataUpdateAt      time.Time          `json:"MetaDataUpdateAt,omitempty"`
	ReportedReady         bool               `json:"ReportedReady,omitempty"`
	Healthy               bool               `json:"Healthy"`
	UnhealthyReason       string             `json:"UnhealthyReason,omitempty"`
	Score                 float64            `json:"Score,omitempty"`
	QuotaCpuUsage         int64              `json:"QuotaCpuUsage,omitempty"`
	QuotaMemUsage         int64              `json:"QuotaMemUsage,omitempty"`
	CpuUtil               float64            `json:"CpuUtil,omitempty"`
	CpuLoadUsage          float64            `json:"CpuLoadUsage,omitempty"`
	MemUsage              int64              `json:"MemUsage,omitempty"`
	DataDiskUsagePer      float64            `json:"DataDiskUsagePer,omitempty"`
	StorageDiskUsagePer   float64            `json:"StorageDiskUsagePer,omitempty"`
	SysDiskUsagePer       float64            `json:"SysDiskUsagePer,omitempty"`
	MvmNum                int64              `json:"mvm_num,omitempty"`
	MetricUpdate          time.Time          `json:"MetricUpdateAt,omitempty"`
	MetricLocalUpdateAt   time.Time          `json:"MetricLocalUpdateAt,omitempty"`
	RealTimeCreateNum     int64              `json:"RealTimeCreateNum,omitempty"`
	LocalCreateNum        int64              `json:"LocalCreateNum,omitempty"`
	NicQueues             int64              `json:"nic_queues,omitempty"`
	NodeLabels            map[string]string  `json:"NodeLabels,omitempty"`
	SchedulingDisabled    bool               `json:"SchedulingDisabled"`
	LocalTemplates        []string           `json:"LocalTemplates,omitempty"`
	Versions              []ComponentVersion `json:"Versions,omitempty"`
	HostFacts             *HostFacts         `json:"HostFacts,omitempty"`
}

func (n *SchedulerNode) ID() string {
	if n == nil {
		return ""
	}
	if n.InsID == "" {
		return n.IP
	}
	return n.InsID
}

func (n *SchedulerNode) HostIP() string {
	if n == nil {
		return ""
	}
	return n.IP
}

func MustJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}
