// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"

	"github.com/containerd/continuity/fs"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	corev1 "k8s.io/api/core/v1"
)

func (cb *CubeBox) GetOrCreatePodConfig() *PodConfig {
	if cb.PodConfig == nil {
		cb.PodConfig = &PodConfig{}
	}
	return cb.PodConfig
}

type ExternConfig struct {
}

type PodConfig struct {
	ImageStorageQuota int64 `json:"image_storage_quota,omitempty"`
	ImageStorageUsed  int64 `json:"image_storage_used,omitempty"`

	HostSharedLayers map[string]*cubehost.LayerMount `json:"host_layers,omitempty"`
	HostedImageMap   map[string]*HostImageIndex      `json:"hosted_image_map,omitempty"`

	IsParsedUserData bool          `json:"is_parsed_user_data,omitempty"`
	K8sPod           *corev1.Pod   `json:"k8s_pod,omitempty"`
	ExternConfig     *ExternConfig `json:"extern_config,omitempty"`
}

func (cb *PodConfig) SetK8sPod(ctx context.Context, pod *corev1.Pod) {
	cb.K8sPod = pod
}

func (cb *PodConfig) AppendHostedImage(hostImage *cubehost.HostImage) error {
	if err := cb.appendImageLayer(hostImage.LayerMounts); err != nil {
		return err
	}

	hii := &HostImageIndex{
		Image: hostImage.LessCopy(),
	}
	for _, l := range hostImage.LayerMounts {
		hii.Layers = append(hii.Layers, l.Name)
	}
	if cb.HostedImageMap == nil {
		cb.HostedImageMap = make(map[string]*HostImageIndex)
	}
	cb.HostedImageMap[hostImage.Id] = hii
	return nil
}

func (cb *PodConfig) GetHostedImageList() []string {
	if cb.HostedImageMap == nil {
		return nil
	}
	var list []string
	for _, hii := range cb.HostedImageMap {
		list = append(list, hii.Image.Id)
	}
	return list
}

func (cb *PodConfig) appendImageLayer(layers []*cubehost.LayerMount) error {
	if cb.HostSharedLayers == nil {
		cb.HostSharedLayers = make(map[string]*cubehost.LayerMount)
	}

	var (
		newSize        = cb.ImageStorageUsed
		toAppendLayers []*cubehost.LayerMount
	)

	for _, l := range layers {
		if _, ok := cb.HostSharedLayers[l.Name]; !ok {
			if l.Usage != "" {
				usage := &fs.Usage{}
				err := jsoniter.Unmarshal([]byte(l.Usage), usage)
				if err == nil {
					newSize = newSize + usage.Size
					if newSize > cb.ImageStorageQuota && cb.ImageStorageQuota > 0 {
						return errors.New("image storage quota exceeded")
					}

				}
			}
			toAppendLayers = append(toAppendLayers, l)
		}
	}

	for _, l := range toAppendLayers {
		cb.HostSharedLayers[l.Name] = l
	}
	cb.ImageStorageUsed = newSize
	return nil
}

func (cb *PodConfig) DeleteImageLayer(name string) {
	if cb.HostSharedLayers == nil {
		return
	}

	if l, ok := cb.HostSharedLayers[name]; ok {
		if l.Usage != "" {
			usage := &fs.Usage{}
			err := jsoniter.Unmarshal([]byte(l.Usage), usage)
			if err == nil {
				cb.ImageStorageUsed -= usage.Size
				if cb.ImageStorageUsed < 0 {
					cb.ImageStorageUsed = 0
				}
			}
		}
		delete(cb.HostSharedLayers, name)
	}
}

func (cb *PodConfig) SetImageQuota(defaultQuota int64) error {
	cb.ImageStorageQuota = defaultQuota
	return nil
}

type HostImageIndex struct {
	Image  *cubehost.HostImage `json:"image,omitempty"`
	Layers []string            `json:"layers,omitempty"`
}
