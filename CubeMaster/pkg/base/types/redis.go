// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

type InstanceInfoMap struct {
	InsID     string `json:"InsID" redis:"ins_id"`
	HostInsID string `json:"HostInsID" redis:"host_ins_id"`
	HostIP    string `json:"HostIP" redis:"host_ip"`
	SandboxID string `json:"SandboxID" redis:"sandbox_id"`
	SandboxIP string `json:"SandboxIP,omitempty" redis:"sandbox_ip"`

	CreatedAt           string `json:"CreatedAt,omitempty" redis:"created_at"`
	RunningCallbackFlag string `json:"RunningCallbackFlag,omitempty" redis:"running_callback_flag"`
	RunningFailMsg      string `json:"RunningFailMsg,omitempty" redis:"running_fail_msg"`
	NumaNode            uint64 `json:"NumaNode" redis:"numa_node"`
	NICQueue            uint64 `json:"NICQueue,omitempty" redis:"nic_queue"`

	InstanceState string `json:"InstanceState" redis:"ins_state"`

	CPU     int64  `json:"Cpu" redis:"cpu"`
	Mem     int64  `json:"Mem" redis:"mem"`
	CPUType string `json:"CpuType" redis:"cpu_type"`
	Zone    string `json:"Zone" redis:"zone"`
	Region  string `json:"Region" redis:"region"`

	ImageID                 string `json:"ImageID" redis:"image_id"`
	SystemDisk              string `json:"SystemDisk" redis:"system_disk"`
	DataDisks               string `json:"DataDisks" redis:"data_disks"`
	PrivateIPAddresses      string `json:"PrivateIpAddresses" redis:"private_ip_addresses"`
	PrivateIP               string `json:"PrivateIp" redis:"private_ip"`
	PrirvateIPCnt           int64  `json:"PrivateIpCnt" redis:"private_ip_cnt"`
	SecurityGroupIDS        string `json:"SecurityGroupIds" redis:"security_ids"`
	VpcID                   string `json:"VpcId" redis:"vpc_id"`
	SubnetID                string `json:"SubnetId" redis:"subnet_id"`
	MacAddress              string `json:"MacAddress" redis:"mac_address"`
	DiskState               string `json:"DiskState" redis:"disk_state"`
	FailMsg                 string `json:"FailMsg" redis:"fail_msg"`
	CamRoleName             string `json:"CamRoleName" redis:"cam_role_name"`
	DisasterRecoverGroupIds string `json:"DisasterRecoverGroupIds" redis:"disaster_recover_group_ids"`
}

type DescribeTaskMap struct {
	ErrorCode    int64  `json:"ErrorCode,omitempty" redis:"error_code"`
	ErrorMessage string `json:"ErrorMessage,omitempty" redis:"error_message"`
	Status       string `json:"Status,omitempty" redis:"status"`
	TaskID       string `json:"TaskId,omitempty" redis:"task_id"`
}

type TemplateImageJobPullProgressMap struct {
	JobID               string `json:"JobID,omitempty" redis:"job_id"`
	PullTotalBytes      int64  `json:"PullTotalBytes,omitempty" redis:"pull_total_bytes"`
	PullDownloadedBytes int64  `json:"PullDownloadedBytes,omitempty" redis:"pull_downloaded_bytes"`
	PullTotalLayers     int32  `json:"PullTotalLayers,omitempty" redis:"pull_total_layers"`
	PullCompletedLayers int32  `json:"PullCompletedLayers,omitempty" redis:"pull_completed_layers"`
	PullSpeedBPS        int64  `json:"PullSpeedBPS,omitempty" redis:"pull_speed_bps"`
	UpdatedAtMs         int64  `json:"UpdatedAtMs,omitempty" redis:"updated_at_ms"`
}
