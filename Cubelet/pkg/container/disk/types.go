// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package disk

type CubeDiskConfig struct {
	HostPath    string       `json:"path"`
	Type        string       `json:"fs_type"`
	SourcePath  string       `json:"source_dir"`
	Size        int64        `json:"size"`
	FSQuota     int64        `json:"fs_quota,omitempty"`
	RateLimiter *RateLimiter `json:"rate_limiter_config,omitempty"`
	Options     []string     `json:"options,omitempty"`

	VolumeSourceName string `json:"volume_source,omitempty"`
}

type RateLimiter struct {
	Bandwidth *struct {
		Size         *int64 `json:"size,omitempty"`
		OneTimeBurst *int64 `json:"one_time_burst,omitempty"`
		RefillTime   *int64 `json:"refill_time,omitempty"`
	} `json:"bandwidth,omitempty"`
	Ops *struct {
		Size         *int64 `json:"size,omitempty"`
		OneTimeBurst *int64 `json:"one_time_burst,omitempty"`
		RefillTime   *int64 `json:"refill_time,omitempty"`
	} `json:"ops,omitempty"`
}

type CubePCIDiskInfo struct {
	PCIDisks      []CubePCIDisk `json:"pci_disks"`
	NumaNode      int32         `json:"numa_node"`
	Queues        int64         `json:"queues"`
	AppID         int64         `json:"AppId,omitempty"`
	Uin           string        `json:"Uin,omitempty"`
	SubAccountUin string        `json:"SubAccountUin,omitempty"`
	CubeHostInfo  *CubeHostInfo `json:"cube_host_info,omitempty"`
	PCIMode       string        `json:"pci_mode,omitempty"`
}

type CubePCISystemDiskInfo struct {
	PCISystemDisk CubePCIDisk `json:"pci_system_disk"`
	NumaNode      int32       `json:"numa_node"`
	Queues        int64       `json:"queues"`
	AppID         int64       `json:"AppId,omitempty"`
	Uin           string      `json:"Uin,omitempty"`
	SubAccountUin string      `json:"SubAccountUin,omitempty"`
}

type CubePCIDisk struct {
	DiskUuid    string `json:"DiskUuid,omitempty"`
	Name        string `json:"name,omitempty"`
	BDF         string `json:"bdf,omitempty"`
	SysfsDevice string `json:"sysfs_dev,omitempty"`
	ID          string `json:"id,omitempty"`

	Platform bool `json:"platform,omitempty"`

	SourceDir string `json:"source_dir,omitempty"`
	FSType    string `json:"fs_type,omitempty"`

	NeedFormat bool `json:"need_format,omitempty"`

	FSQuota int64 `json:"fs_quota,omitempty"`

	NeedResize bool `json:"need_resize,omitempty"`
}

type CloudDiskV3 struct {
	DiskType string `json:"DiskType,omitempty"`
	DiskSize int    `json:"DiskSize,omitempty"`
	DiskID   string `json:"DiskId,omitempty"`
	DiskUuid string `json:"DiskUuid,omitempty"`
}

type CubeHostInfo struct {
	Region                string  `json:"region,omitempty"`
	VirtualCpu            uint64  `json:"virtual_cpu,omitempty"`
	UUID                  string  `json:"uuid,omitempty"`
	DeviceClass           string  `json:"DeviceClass,omitempty"`
	DeviceID              int64   `json:"DeviceId,omitempty"`
	MachineHostIP         string  `json:"MachineHostIP,omitempty"`
	InstanceFamily        string  `json:"InstanceFamily,omitempty"`
	DedicatedClusterId    string  `json:"DedicatedClusterId,omitempty"`
	VirtualNodeQuotaArray []int64 `json:"VirtualNodeQuotaArray,omitempty" `
}
