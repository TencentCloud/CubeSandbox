// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package shimtypes

type CubeShimDevice struct {
	ID        string `json:"id,omitempty"`
	SysfsDev  string `json:"sysfs_dev,omitempty"`
	Platform  bool   `json:"platform,omitempty"`
	FsType    string `json:"fs_type,omitempty"`
	SourceDir string `json:"source_dir,omitempty"`
}

type DeviceType string

const (
	DeviceTypeNetworkDevice      DeviceType = "NetworkDevice"
	DeviceTypeBlockDevice        DeviceType = "BlockDevice"
	DeviceTypeVhostUserNetwork   DeviceType = "VhostUserNetwork"
	DeviceTypeVhostUserBlkDevice DeviceType = "VhostUserBlkDevice"
	DeviceTypeShareFs            DeviceType = "ShareFs"
	DeviceTypeVfioDevice         DeviceType = "VfioDevice"
)

type CubeDeviceType struct{}

type ChAddDiskRequest struct {
	Path     string `json:"path"`
	Serial   string `json:"serial"`
	Readonly bool   `json:"readonly"`
}

type ChAddDiskResponse struct {
	Bdf string `json:"bdf"`
	ID  string `json:"id"`
}

type ChDiskDevice struct {
	ChAddDiskRequest
	ChAddDiskResponse
}

type ChDelDiskRequest struct {
	ID string `json:"id"`
}
