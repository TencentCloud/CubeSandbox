// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatetypes

import (
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
)

type CubeComponent = string

const (
	CubeComponentCubeShim   = "cube-shim"
	CubeComponentCubeKernel = "cube-agent"
	CubeComponentCubeImage  = "cube-image"

	StorageMediumErofs = "erofs"
)

type DistributionReference struct {
	Namespace          string `protobuf:"bytes,1,opt,name=namespace" json:"namespace,omitempty"`
	Name               string `protobuf:"bytes,2,opt,name=name" json:"name,omitempty"`
	DistributionName   string `protobuf:"bytes,3,opt,name=distributionName" json:"distributionName,omitempty"`
	DistributionTaskID string `protobuf:"bytes,4,opt,name=distributionTaskID" json:"distributionTaskID,omitempty"`
	TemplateID         string `protobuf:"bytes,5,opt,name=templateID" json:"templateID,omitempty"`
}

type LocalRunTemplate struct {
	DistributionReference `protobuf:"bytes,1,opt,name=distributionReference" json:"distributionReference,omitempty"`
	Images                []LocalDistributionImage   `protobuf:"bytes,3,rep,name=images" json:"images,omitempty"`
	Volumes               map[string]LocalBaseVolume `protobuf:"bytes,4,rep,name=volumes" json:"volumes,omitempty"`

	Components map[string]LocalComponent `protobuf:"bytes,5,rep,name=components" json:"components,omitempty" protobuf_key:"bytes,1,opt,name=key" protobuf_val:"bytes,2,opt,name=value"`

	Snapshot LocalSnapshot `protobuf:"bytes,6,opt,name=snapshot" json:"snapshot,omitempty"`
}

type LocalDistributionImage struct {
	DistributionReference `protobuf:"bytes,1,opt,name=distributionReference" json:"distributionReference,omitempty"`
	TemplateImage         TemplateImage    `protobuf:"bytes,2,opt,name=templateImage" json:"templateImage,omitempty"`
	Image                 imagestore.Image `protobuf:"bytes,3,opt,name=image" json:"image,omitempty"`
}

type LocalBaseVolume struct {
	DistributionReference `protobuf:"bytes,1,opt,name=distributionReference" json:"distributionReference,omitempty"`
	VolumeID              string       `protobuf:"bytes,2,opt,name=volumeID" json:"volumeID,omitempty"`
	Volume                VolumeSource `protobuf:"bytes,3,opt,name=volume" json:"volume,omitempty"`
	LocalPath             string       `protobuf:"bytes,4,opt,name=localPath" json:"localPath,omitempty"`
	SnapshotID            string       `protobuf:"bytes,5,opt,name=snapshotID" json:"snapshotID,omitempty"`
}

type LocalComponent struct {
	DistributionReference `protobuf:"bytes,1,opt,name=distributionReference" json:"distributionReference,omitempty"`
	Component             MachineComponent `protobuf:"bytes,2,opt,name=component" json:"component,omitempty"`
}

type LocalSnapshot struct {
	DistributionReference `protobuf:"bytes,1,opt,name=distributionReference" json:"distributionReference,omitempty"`
	Snapshot              Snapshot `protobuf:"bytes,2,opt,name=snapshot" json:"snapshot,omitempty"`
}

type TemplateImage struct {
	Name         string `protobuf:"bytes,1,opt,name=name" json:"name,omitempty"`
	Namespace    string `protobuf:"bytes,2,opt,name=namespace" json:"namespace,omitempty"`
	Image        string `protobuf:"bytes,3,opt,name=image" json:"image,omitempty"`
	StorageMedia string `protobuf:"bytes,4,opt,name=storageMedia" json:"storage_media,omitempty"`
}

type BaseBlockVolumeSource struct {
	ID        string       `protobuf:"bytes,1,opt,name=id" json:"id,omitempty"`
	Medium    string       `protobuf:"bytes,2,opt,name=medium" json:"medium,omitempty"`
	SizeLimit string       `protobuf:"bytes,3,opt,name=sizeLimit" json:"size_limit,omitempty"`
	BlockType string       `protobuf:"bytes,4,opt,name=blockType" json:"block_type,omitempty"`
	RemoteCos *CosFileInfo `protobuf:"bytes,5,opt,name=remoteCos" json:"remote_cos,omitempty"`
}

type VolumeSource struct {
	BaseBlockSource BaseBlockVolumeSource `protobuf:"bytes,1,opt,name=baseBlockSource" json:"base_block_source,omitempty"`
}

type MachineComponent struct {
	Name    CubeComponent `protobuf:"bytes,1,opt,name=name" json:"name,omitempty"`
	Version string        `protobuf:"bytes,2,opt,name=version" json:"version,omitempty"`
	Path    string        `protobuf:"bytes,3,opt,name=path" json:"path,omitempty"`
}

type Snapshot struct {
	ID    string `protobuf:"bytes,1,opt,name=id" json:"id,omitempty"`
	Media string `protobuf:"bytes,2,opt,name=media" json:"media,omitempty"`
	Path  string `protobuf:"bytes,3,opt,name=path" json:"path,omitempty"`
}

type CosFileInfo struct {
	SecretId    string            `json:"secret_id,omitempty"`
	SecretKey   string            `json:"secret_key,omitempty"`
	Token       string            `json:"token,omitempty"`
	URL         string            `json:"url,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	LocalPath   string            `json:"local_path,omitempty"`
}
