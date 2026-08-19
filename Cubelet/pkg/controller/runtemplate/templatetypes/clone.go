// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatetypes

import (
	"maps"
	"slices"
)

// Clone returns a caller-owned copy. Maps and slices are independent of the
// original so Create/List can mutate Componts without racing the template cache.
func (t *LocalRunTemplate) Clone() *LocalRunTemplate {
	if t == nil {
		return nil
	}
	out := *t
	out.Componts = maps.Clone(t.Componts)
	out.Volumes = cloneVolumes(t.Volumes)
	out.Images = cloneImages(t.Images)
	return &out
}

func cloneVolumes(in map[string]LocalBaseVolume) map[string]LocalBaseVolume {
	out := maps.Clone(in)
	for k, v := range out {
		if v.Volume.BaseBlockSource.RemoteCos == nil {
			continue
		}
		cos := *v.Volume.BaseBlockSource.RemoteCos
		cos.Annotations = maps.Clone(cos.Annotations)
		v.Volume.BaseBlockSource.RemoteCos = &cos
		out[k] = v
	}
	return out
}

func cloneImages(in []LocalDistributionImage) []LocalDistributionImage {
	out := slices.Clone(in)
	for i := range out {
		img := &out[i].Image
		img.References = slices.Clone(img.References)
		img.Snapshots = slices.Clone(img.Snapshots)
		img.HostLayers = slices.Clone(img.HostLayers)
		img.Annotation = maps.Clone(img.Annotation)
	}
	return out
}
