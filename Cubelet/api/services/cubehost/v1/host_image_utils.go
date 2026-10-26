// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubehost

func (hi *HostImage) LessCopy() *HostImage {
	hiCopy := &HostImage{}
	hiCopy.Id = hi.Id
	hiCopy.RepoTags = hi.RepoTags
	hiCopy.RepoDigests = hi.RepoDigests
	hiCopy.Size = hi.Size
	hiCopy.Spec = hi.Spec
	hiCopy.LayerMounts = hi.LayerMounts
	hiCopy.ImageDevs = hi.ImageDevs
	return hiCopy
}

func (hi *HostImage) GetLayerString() []string {
	layers := make([]string, 0, len(hi.LayerMounts))
	for _, layer := range hi.LayerMounts {
		layers = append(layers, layer.Name)
	}
	return layers
}
