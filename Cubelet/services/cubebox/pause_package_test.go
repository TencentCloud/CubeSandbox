// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestResumeFromPauseSandboxID(t *testing.T) {
	assert.Empty(t, resumeFromPauseSandboxID(nil))
	assert.Empty(t, resumeFromPauseSandboxID(&cubebox.RunCubeSandboxRequest{}))
	assert.Empty(t, resumeFromPauseSandboxID(&cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationDesiredSandboxID: "sb-1",
		},
	}))
	assert.Equal(t, "sb-1", resumeFromPauseSandboxID(&cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationPauseSnapshotID:  "snap-pause-1",
			constants.MasterAnnotationDesiredSandboxID: "sb-1",
		},
	}))
}

func TestBuildPauseSandboxSpecIncludesNetworkRecreateFields(t *testing.T) {
	allow := false
	sb := &cubeboxstore.CubeBox{
		Metadata: cubeboxstore.Metadata{
			ID:           "sb-1",
			SandboxID:    "sb-1",
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations:  map[string]string{"k": "v"},
			Labels:       map[string]string{"l": "1"},
		},
		Namespace:      "default",
		NetworkType:    cubebox.NetworkType_tap.String(),
		RuntimeHandler: "cube",
		ExposedPorts:   []int64{8080, 443},
		CubeNetworkConfig: &cubebox.CubeNetworkConfig{
			AllowInternetAccess: &allow,
			AllowOut:            []string{"10.0.0.0/8"},
			DenyOut:             []string{"0.0.0.0/0"},
			Rules: []*cubebox.EgressRule{{
				Name: "deny-evil",
			}},
		},
		FirstContainerName: "c1",
		ContainersMap: &cubeboxstore.ContainersMap{
			ContainerMap: map[string]*cubeboxstore.Container{
				"c1": {
					Metadata: cubeboxstore.Metadata{
						ID:   "c1",
						Name: "main",
						Config: &cubebox.ContainerConfig{
							Id:   "c1",
							Name: "main",
						},
					},
				},
			},
		},
	}

	got := buildPauseSandboxSpec(sb, "req-1")
	require.NotNil(t, got)
	assert.Equal(t, "req-1", got.RequestID)
	assert.Equal(t, cubebox.NetworkType_tap.String(), got.NetworkType)
	assert.Equal(t, "cube", got.RuntimeHandler)
	assert.Equal(t, []int64{8080, 443}, got.ExposedPorts)
	require.NotNil(t, got.CubeNetworkConfig)
	require.NotNil(t, got.CubeNetworkConfig.AllowInternetAccess)
	assert.False(t, *got.CubeNetworkConfig.AllowInternetAccess)
	assert.Equal(t, []string{"10.0.0.0/8"}, got.CubeNetworkConfig.AllowOut)
	assert.Equal(t, []string{"0.0.0.0/0"}, got.CubeNetworkConfig.DenyOut)
	require.Len(t, got.CubeNetworkConfig.Rules, 1)
	assert.Equal(t, "deny-evil", got.CubeNetworkConfig.Rules[0].Name)
	// Packed config must be a clone.
	got.CubeNetworkConfig.AllowOut[0] = "mutated"
	assert.Equal(t, "10.0.0.0/8", sb.CubeNetworkConfig.AllowOut[0])
}

func TestMergeThinNetworkFieldsOntoPackedPauseSpec(t *testing.T) {
	allow := false
	thin := &cubebox.RunCubeSandboxRequest{
		CubeNetworkConfig: &cubebox.CubeNetworkConfig{
			AllowInternetAccess: &allow,
			DenyOut:             []string{"0.0.0.0/0"},
		},
		NetworkType:    "tap",
		RuntimeHandler: "cube",
		ExposedPorts:   []int64{8080},
	}
	packed := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{{Id: "c1", Name: "main"}},
		Annotations: map[string]string{
			"kept": "yes",
		},
	}

	// Simulate expandPauseSnapshotPackage merge logic for legacy packages.
	if packed.GetCubeNetworkConfig() == nil {
		packed.CubeNetworkConfig = thin.CubeNetworkConfig
	}
	if packed.GetNetworkType() == "" {
		packed.NetworkType = thin.NetworkType
	}
	if packed.GetRuntimeHandler() == "" {
		packed.RuntimeHandler = thin.RuntimeHandler
	}
	if len(packed.GetExposedPorts()) == 0 {
		packed.ExposedPorts = append([]int64(nil), thin.ExposedPorts...)
	}

	require.NotNil(t, packed.CubeNetworkConfig)
	assert.Equal(t, []string{"0.0.0.0/0"}, packed.CubeNetworkConfig.DenyOut)
	assert.Equal(t, "tap", packed.NetworkType)
	assert.Equal(t, "cube", packed.RuntimeHandler)
	assert.Equal(t, []int64{8080}, packed.ExposedPorts)
}
