// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatetypes

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
)

func TestLocalRunTemplateCloneNil(t *testing.T) {
	var local *LocalRunTemplate
	assert.Nil(t, local.Clone())
}

func TestLocalRunTemplateCloneCompontsIndependent(t *testing.T) {
	src := &LocalRunTemplate{
		DistributionReference: DistributionReference{TemplateID: "tpl-1"},
		Componts: map[string]LocalComponent{
			CubeComponentCubeShim: {Component: MachineComponent{Name: CubeComponentCubeShim, Version: "v1"}},
		},
		Volumes: map[string]LocalBaseVolume{
			"root": {
				VolumeID: "vol-1",
				Volume: VolumeSource{BaseBlockSource: BaseBlockVolumeSource{
					RemoteCos: &CosFileInfo{URL: "cos://x", Annotations: map[string]string{"a": "b"}},
				}},
			},
		},
		Images: []LocalDistributionImage{{
			Image: imagestore.Image{
				ID:         "img-1",
				References: []string{"ref"},
				Annotation: map[string]string{"k": "v"},
			},
		}},
	}

	got := src.Clone()
	require.NotNil(t, got)
	assert.Equal(t, "tpl-1", got.TemplateID)
	assert.NotSame(t, src, got)

	got.Componts[CubeComponentCubeShim] = LocalComponent{Component: MachineComponent{Version: "mutated"}}
	assert.Equal(t, "v1", src.Componts[CubeComponentCubeShim].Component.Version)

	vol := got.Volumes["root"]
	vol.VolumeID = "mutated"
	got.Volumes["root"] = vol
	assert.Equal(t, "vol-1", src.Volumes["root"].VolumeID)
	got.Volumes["root"].Volume.BaseBlockSource.RemoteCos.URL = "mutated"
	assert.Equal(t, "cos://x", src.Volumes["root"].Volume.BaseBlockSource.RemoteCos.URL)

	got.Images[0].Image.Annotation["k"] = "mutated"
	assert.Equal(t, "v", src.Images[0].Image.Annotation["k"])
	got.Images[0].Image.References[0] = "mutated"
	assert.Equal(t, "ref", src.Images[0].Image.References[0])
}
