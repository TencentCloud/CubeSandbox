// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package types common definitions of common types of cube master.
package types

import (
	"bytes"
	"encoding/json"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

type Request struct {
	RequestID string `json:"requestID" p:"requestID"  v:"required"`
}

type Res struct {
	RequestID string `json:"requestID,omitempty"`
	Ret       *Ret   `json:"ret,omitempty"`
}

type Ret struct {
	RetCode int    `json:"ret_code"`
	RetMsg  string `json:"ret_msg"`
}

type HostChangeEvent struct {
	*Request
	HostIDs   []string `p:"hostIDs"  v:"required"`
	EventType string   `p:"eventType"  v:"required"`
}

type CreateCubeSandboxReq struct {
	*Request

	Timeout       int            `json:"timeout,omitempty" d:"60"`
	SnapshotDir   string         `json:"snapshot_dir,omitempty"`
	InsId         string         `json:"ins_id,omitempty"`
	InsIp         string         `json:"ins_ip,omitempty"`
	Volumes       []*Volume      `json:"volumes,omitempty"`
	CubeVSContext *CubeVSContext `json:"cubevs_context,omitempty"`

	Containers []*Container `json:"containers,omitempty"`

	Annotations       map[string]string `json:"annotations,omitempty" `
	Labels            map[string]string `json:"labels,omitempty" `
	DistributionScope []string          `json:"distribution_scope,omitempty"`
	InstanceType      string            `json:"instance_type,omitempty"`
	NetworkType       string            `json:"network_type,omitempty"`

	RuntimeHandler string `json:"runtime_handler,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
}

type CreateCubeSandboxRes struct {
	RequestID string
	Ret       *Ret              `json:"ret,omitempty"`
	SandboxID string            `json:"sandbox_id,omitempty"`
	SandboxIP string            `json:"sandbox_ip,omitempty"`
	HostID    string            `json:"host_id,omitempty"`
	HostIP    string            `json:"host_ip,omitempty"`
	ExtInfo   map[string]string `json:"ext_info,omitempty"`
}

type Resource struct {
	Cpu string `json:"cpu,omitempty"`

	Mem string `json:"mem,omitempty"`

	Limit *RequestLimit `json:"limit,omitempty"`
}

type RequestLimit struct {
	Cpu string `json:"cpu,omitempty"`

	Mem string `json:"mem,omitempty"`
}

type CubeVSContext struct {
	AllowInternetAccess *bool    `json:"allowInternetAccess,omitempty"`
	AllowOut            []string `json:"allowOut,omitempty"`
	DenyOut             []string `json:"denyOut,omitempty"`
}

type Volume struct {
	Name string `json:"name,omitempty"`

	VolumeSource *VolumeSource `json:"volume_source,omitempty"`
}

type VolumeSource struct {
	EmptyDir             *EmptyDirVolumeSource    `json:"empty_dir,omitempty"`
	SandboxPath          *SandboxPathVolumeSource `json:"sandbox_path,omitempty"`
	HostDirVolumeSources *HostDirVolumeSources    `json:"host_dir_volumes,omitempty"`

	Image *imagev1.ImageVolumeSource `protobuf:"bytes,9,opt,name=image,proto3" json:"image,omitempty"`
}

type HostDirVolumeSources struct {
	VolumeSources []*HostDirSource `json:"volume_sources,omitempty"`
}

type HostDirSource struct {
	Name string `json:"name,omitempty"`

	HostPath string `json:"host_path,omitempty"`
}

type EmptyDirVolumeSource struct {
	Medium int32 `json:"medium,omitempty"`

	SizeLimit string `json:"size_limit,omitempty"`
}

type SandboxPathVolumeSource struct {
	Path string `json:"path,omitempty"`

	Type string `json:"type,omitempty"`
}

type Container struct {
	Name string `json:"name,omitempty"`

	Image *ImageSpec `json:"image,omitempty"`

	Command []string `json:"command,omitempty"`

	Args []string `json:"args,omitempty"`

	WorkingDir string `json:"working_dir,omitempty"`

	Envs []*KeyValue `json:"envs,omitempty"`

	VolumeMounts []*cubeboxv1.VolumeMounts `json:"volume_mounts,omitempty"`

	RLimit *RLimit `json:"r_limit,omitempty"`

	Resources *Resource `json:"resources,omitempty"`

	SecurityContext *ContainerSecurityContext `json:"security_context,omitempty"`

	Probe *Probe `json:"probe,omitempty"`

	Sysctls map[string]string `json:"sysctls,omitempty" `

	Syscalls []*SysCall `json:"syscalls,omitempty"`

	DnsConfig *DNSConfig `json:"dns_config,omitempty"`

	Annotations map[string]string `json:"annotations,omitempty" `
	HostAliases []*HostAlias      `json:"host_aliases,omitempty"`
	Prestop     *PreStop          `json:"prestop,omitempty"`
	Poststop    *PostStop         `json:"poststop,omitempty"`

	Id    string `json:"id,omitempty"`
	Hooks *Hooks `json:"hooks,omitempty"`
}

type Hooks struct {
	Prestart []*Hook `json:"prestart,omitempty"`
}

type Hook struct {
	Path    string   `json:"path,omitempty"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	Timeout *int32   `json:"timeout,omitempty"`
}

type PreStop struct {
	TerminationGracePeriodMs int32 `json:"termination_grace_period_ms,omitempty"`

	LifecyleHandler *LifecycleHandler `json:"lifecyle_handler,omitempty"`
}

type PostStop struct {
	TimeoutMs int32 `json:"timeout_ms,omitempty"`

	LifecyleHandler *LifecycleHandler `json:"lifecyle_handler,omitempty"`
}

type LifecycleHandler struct {
	HttpGet *HTTPGetAction `json:"http_get,omitempty"`
}

type HostAlias struct {
	Hostnames []string `json:"hostnames,omitempty"`
	Ip        string   `json:"ip,omitempty"`
}

type ImageSpec struct {
	Image             string            `json:"image,omitempty" v:"required"`
	Name              string            `json:"name,omitempty"`
	Token             string            `json:"token,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty" `
	StorageMedia      string            `json:"storage_media,omitempty"`
	WritableLayerSize string            `json:"writable_layer_size,omitempty"`
}

type KeyValue struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type RLimit struct {
	NoFile uint64 `json:"no_file,omitempty"`
}

type ContainerSecurityContext struct {
	Capabilities *Capability `json:"capabilities,omitempty"`

	Privileged bool `json:"privileged,omitempty"`

	RunAsUser *Int64Value `json:"run_as_user,omitempty"`

	NoNewPrivs bool `json:"no_new_privs,omitempty"`

	RunAsGroup *Int64Value `json:"run_as_group,omitempty"`

	RunAsUsername string `json:"run_as_username,omitempty"`

	ReadonlyRootfs bool `json:"readonly_rootfs,omitempty"`
}

type Int64Value struct {
	Value int64 `json:"value,omitempty"`
}

type Capability struct {
	AddCapabilities []string `json:"add_capabilities,omitempty"`

	DropCapabilities []string `json:"drop_capabilities,omitempty"`

	AddAmbientCapabilities []string `json:"add_ambient_capabilities,omitempty"`
}

type Probe struct {
	ProbeHandler *ProbeHandler `json:"probe_handler,omitempty"`

	InitialDelaySeconds int32 `json:"initial_delay_seconds,omitempty"`

	TimeoutSeconds int32 `json:"timeout_seconds,omitempty"`

	InitialDelayMs int32 `json:"initial_delay_ms,omitempty"`

	TimeoutMs int32 `json:"timeout_ms,omitempty"`

	PeriodMs int32 `json:"period_ms,omitempty"`

	SuccessThreshold int32 `json:"success_threshold,omitempty"`

	FailureThreshold int32 `json:"failure_threshold,omitempty"`

	ProbeTimeoutMs int32 `json:"probe_timeout_ms,omitempty"`
}

type ProbeHandler struct {
	TCPSocket *TCPSocketAction `json:"tcp_socket,omitempty"`
	Ping      *PingAction      `json:"ping,omitempty"`
	HttpGet   *HTTPGetAction   `json:"http_get,omitempty"`
}

type HTTPGetAction struct {
	Path *string `json:"path,omitempty"`

	Port int32 `json:"port,omitempty"`

	Host *string `json:"host,omitempty"`

	HttpHeaders []*HTTPHeader `json:"http_headers,omitempty"`
}

type HTTPHeader struct {
	Name *string `json:"name,omitempty"`

	Value *string `json:"value,omitempty"`
}

type TCPSocketAction struct {
	Port int32  `json:"port,omitempty"`
	Host string `json:"host,omitempty"`
}

type PingAction struct {
	Udp bool `json:"udp,omitempty"`
}

type SysCall struct {
	Names []string `json:"names,omitempty"`

	Action string `json:"action,omitempty"`
	Errno  uint32 `json:"errno,omitempty"`

	Args []*LinuxSeccompArg `json:"args,omitempty"`
}

type LinuxSeccompArg struct {
	Index    uint32 `json:"index,omitempty"`
	Value    uint64 `json:"value,omitempty"`
	ValueTwo uint64 `json:"value_two,omitempty"`

	Op string `json:"op,omitempty"`
}

type DNSConfig struct {
	Servers []string `json:"servers,omitempty"`

	Searches []string `json:"searches,omitempty"`

	Options []string `json:"options,omitempty"`
}

type CubeSandboxFilter struct {
	LabelSelector map[string]string `json:"label_selector,omitempty"`
}

type DeleteCubeSandboxRes struct {
	RequestID string `json:"requestID,omitempty"`

	Ret *Ret `json:"ret,omitempty"`

	SandboxID string            `json:"sandbox_id,omitempty"`
	ExtInfo   map[string]string `json:"ext_info,omitempty"`
}

type DeleteCubeSandboxReq struct {
	RequestID   string            `json:"requestID,omitempty" p:"requestID"  v:"required"`
	SandboxID   string            `json:"sandbox_id,omitempty"`
	HostIP      string            `json:"host_ip,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" `

	Filter       *CubeSandboxFilter `json:"filter,omitempty"`
	InstanceType string             `json:"instance_type,omitempty"`

	Sync bool `json:"sync,omitempty"`
}

type ListCubeSandboxReq struct {
	RequestID string `json:"requestID,omitempty" p:"requestID"  v:"required"`
	StartIdx  int    `json:"start_idx,omitempty"`
	Size      int    `json:"size,omitempty"`

	HostID string `json:"host_id,omitempty"`

	Filter       *CubeSandboxFilter `json:"filter,omitempty"`
	InstanceType string             `json:"instance_type,omitempty"`
}

type ListCubeSandboxRes struct {
	RequestID string              `json:"requestID,omitempty"`
	Ret       *Ret                `json:"ret,omitempty"`
	EndIdx    int                 `json:"end_idx,omitempty"`
	Size      int                 `json:"size,omitempty"`
	Total     int                 `json:"total,omitempty"`
	Data      []*SandboxBriefData `json:"data,omitempty"`
}

type SandboxBriefData struct {
	SandboxID   string            `json:"sandbox_id,omitempty"`
	Status      int32             `json:"status,omitempty"`
	HostID      string            `json:"host_id,omitempty"`
	HostIP      string            `json:"host_ip,omitempty"`
	TemplateID  string            `json:"template_id,omitempty"`
	CpuCount    int32             `json:"cpu_count,omitempty"`
	MemoryMB    int32             `json:"memory_mb,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	NameSpace   string            `json:"namespace,omitempty"`
	CreateAt    int64             `json:"create_at,omitempty"`
	PauseAt     int64             `json:"pause_at,omitempty"`
}

type GetCubeSandboxReq struct {
	RequestID     string `json:"requestID,omitempty"`
	SandboxID     string `json:"sandbox_id,omitempty" v:"required-without:host_id#sandbox_id or host_id is required"`
	HostID        string `json:"host_id,omitempty" v:"required-without:sandbox_id#sandbox_id or host_id is required"`
	InstanceType  string `json:"instance_type,omitempty"`
	ContainerPort int32  `json:"container_port,omitempty"`
}

type GetCubeSandboxRes struct {
	RequestID string         `json:"requestID,omitempty"`
	Ret       *Ret           `json:"ret,omitempty"`
	Data      []*SandboxData `json:"data,omitempty"`
}

type SandboxData struct {
	SandboxID              string            `json:"sandbox_id,omitempty"`
	Status                 int32             `json:"status,omitempty"`
	HostID                 string            `json:"host_id,omitempty"`
	HostIP                 string            `json:"host_ip,omitempty"`
	SandboxIP              string            `json:"sandbox_ip,omitempty"`
	TemplateID             string            `json:"template_id,omitempty"`
	Annotations            map[string]string `json:"annotations,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
	Containers             []*ContainerInfo  `json:"containers,omitempty"`
	NameSpace              string            `json:"namespace,omitempty"`
	ExposedPortEndpoint    string            `json:"exposed_port_endpoint,omitempty"`
	ExposedPortMode        string            `json:"exposed_port_mode,omitempty"`
	RequestedContainerPort int32             `json:"requested_container_port,omitempty"`
}

type ContainerInfo struct {
	Name        string `json:"name,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	Status      int32  `json:"status,omitempty"`
	Image       string `json:"image,omitempty"`
	CreateAt    int64  `json:"create_at,omitempty"`
	Cpu         string `json:"cpu,omitempty"`
	Mem         string `json:"mem,omitempty"`
	Type        string `json:"type,omitempty"`
	PauseAt     int64  `json:"pause_at,omitempty"`
}

type CreateImageReq struct {
	RequestID         string            `json:"requestID,omitempty" p:"requestID"  v:"required"`
	Image             string            `json:"image,omitempty" p:"image"  v:"required"`
	Username          string            `json:"username,omitempty"`
	Token             string            `json:"token,omitempty"`
	StorageMedia      string            `json:"storage_media,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	WritableLayerSize string            `json:"writable_layer_size,omitempty"`
	InstanceType      string            `json:"instance_type,omitempty"`
}

type ContainerOverrides struct {
	Command         []string                  `json:"command,omitempty"`
	Args            []string                  `json:"args,omitempty"`
	WorkingDir      string                    `json:"working_dir,omitempty"`
	Envs            []*KeyValue               `json:"envs,omitempty"`
	DnsConfig       *DNSConfig                `json:"dns_config,omitempty"`
	Resources       *Resource                 `json:"resources,omitempty"`
	SecurityContext *ContainerSecurityContext `json:"security_context,omitempty"`
	Probe           *Probe                    `json:"probe,omitempty"`
	Annotations     map[string]string         `json:"annotations,omitempty"`
	VolumeMounts    []*cubeboxv1.VolumeMounts `json:"volume_mounts,omitempty"`
	RLimit          *RLimit                   `json:"r_limit,omitempty"`
}

type CreateTemplateFromImageReq struct {
	*Request
	SourceImageRef     string              `json:"source_image_ref,omitempty" p:"source_image_ref" v:"required"`
	RegistryUsername   string              `json:"registry_username,omitempty"`
	RegistryPassword   string              `json:"registry_password,omitempty"`
	TemplateID         string              `json:"template_id,omitempty" p:"template_id"`
	InstanceType       string              `json:"instance_type,omitempty"`
	NetworkType        string              `json:"network_type,omitempty"`
	CubeVSContext      *CubeVSContext      `json:"cubevs_context,omitempty"`
	WritableLayerSize  string              `json:"writable_layer_size,omitempty" p:"writable_layer_size" v:"required"`
	ExposedPorts       []int32             `json:"exposed_ports,omitempty"`
	DistributionScope  []string            `json:"distribution_scope,omitempty"`
	ContainerOverrides *ContainerOverrides `json:"container_overrides,omitempty"`
	Wait               bool                `json:"wait,omitempty"`
}

type RedoTemplateFromImageReq struct {
	*Request
	TemplateID        string   `json:"template_id,omitempty" p:"template_id" v:"required"`
	DistributionScope []string `json:"distribution_scope,omitempty"`
	FailedOnly        bool     `json:"failed_only,omitempty"`
	Wait              bool     `json:"wait,omitempty"`
}

type RootfsArtifactInfo struct {
	ArtifactID              string `json:"artifact_id,omitempty"`
	TemplateSpecFingerprint string `json:"template_spec_fingerprint,omitempty"`
	SourceImageRef          string `json:"source_image_ref,omitempty"`
	SourceImageDigest       string `json:"source_image_digest,omitempty"`
	MasterNodeID            string `json:"master_node_id,omitempty"`
	MasterNodeIP            string `json:"master_node_ip,omitempty"`
	Ext4Path                string `json:"ext4_path,omitempty"`
	Ext4SHA256              string `json:"ext4_sha256,omitempty"`
	Ext4SizeBytes           int64  `json:"ext4_size_bytes,omitempty"`
	WritableLayerSize       string `json:"writable_layer_size,omitempty"`
	Status                  string `json:"status,omitempty"`
	LastError               string `json:"last_error,omitempty"`
}

type TemplateImageJobInfo struct {
	JobID                   string              `json:"job_id,omitempty"`
	TemplateID              string              `json:"template_id,omitempty"`
	AttemptNo               int32               `json:"attempt_no,omitempty"`
	RetryOfJobID            string              `json:"retry_of_job_id,omitempty"`
	Operation               string              `json:"operation,omitempty"`
	RedoMode                string              `json:"redo_mode,omitempty"`
	RedoScope               []string            `json:"redo_scope,omitempty"`
	ResumePhase             string              `json:"resume_phase,omitempty"`
	ArtifactID              string              `json:"artifact_id,omitempty"`
	TemplateSpecFingerprint string              `json:"template_spec_fingerprint,omitempty"`
	Status                  string              `json:"status,omitempty"`
	Phase                   string              `json:"phase,omitempty"`
	Progress                int32               `json:"progress,omitempty"`
	ErrorMessage            string              `json:"error_message,omitempty"`
	ExpectedNodeCount       int32               `json:"expected_node_count,omitempty"`
	ReadyNodeCount          int32               `json:"ready_node_count,omitempty"`
	FailedNodeCount         int32               `json:"failed_node_count,omitempty"`
	TemplateStatus          string              `json:"template_status,omitempty"`
	ArtifactStatus          string              `json:"artifact_status,omitempty"`
	Artifact                *RootfsArtifactInfo `json:"artifact,omitempty"`
	Template                *Res                `json:"template,omitempty"`
}

type CreateTemplateFromImageRes struct {
	RequestID string                `json:"requestID,omitempty"`
	Ret       *Ret                  `json:"ret,omitempty"`
	Job       *TemplateImageJobInfo `json:"job,omitempty"`
}

type DeleteImageReq struct {
	RequestID    string `json:"requestID,omitempty" p:"requestID"  v:"required"`
	Image        string `json:"image,omitempty" p:"image"  v:"required"`
	StorageMedia string `json:"storage_media,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
}

type GetNodeReq struct {
	RequestID    string `json:"requestID,omitempty"`
	HostID       string `json:"host_id,omitempty" v:"required-without:sandbox_id#sandbox_id or host_id is required"`
	ScoreOnly    bool   `json:"score_only,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
}

type GetNodeRes struct {
	Ret  *Ret         `json:"ret,omitempty"`
	Data []*node.Node `json:"data,omitempty"`
}

type FakeCreateSandboxRes struct {
	RequestID  string `json:"requestID,omitempty"`
	Ret        *Ret   `json:"ret,omitempty"`
	HostID     string `json:"host_id,omitempty"`
	HostIP     string `json:"host_ip,omitempty"`
	SelectCost int64  `json:"select_cost,omitempty"`
	AllCost    int64  `json:"all_cost,omitempty"`
}

type ExecRequest struct {
	RequestID    string   `json:"requestID,omitempty"`
	SandboxID    string   `json:"sandbox_id"`
	ContainerID  string   `json:"container_id"`
	Terminal     bool     `json:"terminal,omitempty"`
	Args         []string `json:"args"`
	Env          []string `json:"env,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
	InstanceType string   `json:"instance_type,omitempty"`
}

type contextKey string

const (
	StartTime contextKey = "startTime"
)

type stdJSONTool struct{}

func (stdJSONTool) Marshal(v interface{}) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := b.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

func (stdJSONTool) Unmarshal(data []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

var FastestJsoniter = stdJSONTool{}

type UpdateRequest struct {
	RequestID    string `json:"requestID" p:"requestID"  v:"required"`
	SandboxID    string `json:"sandbox_id" p:"sandbox_id"  v:"required"`
	InstanceType string `json:"instance_type" p:"instance_type"  v:"required"`
	Action       string `json:"action" p:"action"  v:"required"`
}

type ListInventoryReq struct {
	RequestID    string        `json:"requestID,omitempty" p:"requestID"  v:"required"`
	Filters      []*FilterItem `json:"filters,omitempty" p:"filters"`
	InstanceType string        `json:"instance_type,omitempty"`
}

type FilterItem struct {
	Name   string   `json:"name,omitempty"`
	Values []string `json:"values,omitempty"`
}

type ListInventoryRes struct {
	RequestID string                   `json:"requestID,omitempty"`
	Ret       *Ret                     `json:"ret,omitempty"`
	Data      []*InstanceTypeQuotaItem `json:"data,omitempty"`
}

type InstanceTypeQuotaItem struct {
	Zone    string `json:"zone,omitempty"`
	CPUType string `json:"cpu_type,omitempty"`
	CPU     int64  `json:"cpu,omitempty"`
	Memory  int64  `json:"memory,omitempty"`
}
