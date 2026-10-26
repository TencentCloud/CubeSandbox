// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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

// Expanding a Resume replaces the whole request with the packed spec, which
// knows nothing about where this restore reads its storage from. Losing
// remote_uuids sends the rootfs path down the clone branch, and cross-node
// there is no snapshot on this node to clone from.
func TestPauseRestoreBindingSurvivesPackedSpecSwap(t *testing.T) {
	thin := &cubebox.RunCubeSandboxRequest{
		RequestID: "req-1",
		Backend:   "s3",
		Annotations: map[string]string{
			constants.MasterAnnotationDesiredSandboxID:    "sb-1",
			constants.MasterAnnotationSnapshotRemoteUUIDs: `{"rootfs":"r1","memory":"m1","metadata":"d1"}`,
			constants.MasterAnnotationSnapshotCrossNode:   "true",
		},
	}
	binding := pauseRestoreBindingFrom(thin, "snap-1", "s3")

	packed := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{{Id: "c1", Name: "main"}},
		Annotations: map[string]string{
			"kept": "yes",
			constants.MasterAnnotationLaunchMemorySnapshotID: "tpl-T",
		},
	}
	req := &cubebox.RunCubeSandboxRequest{}
	*req = *packed
	binding.applyTo(req)

	assert.Equal(t, "yes", req.Annotations["kept"])
	assert.Equal(t, "req-1", req.RequestID)
	assert.Equal(t, "sb-1", req.Annotations[constants.MasterAnnotationDesiredSandboxID])
	assert.Equal(t, "snap-1", req.Annotations[constants.MasterAnnotationPauseSnapshotID])
	assert.Equal(t, "snap-1", req.Annotations[constants.MasterAnnotationRuntimeSnapshotID])
	assert.Equal(t, "s3", req.Annotations[constants.MasterAnnotationStorageBackend])
	assert.Equal(t, "s3", req.Backend)
	assert.Equal(t, `{"rootfs":"r1","memory":"m1","metadata":"d1"}`,
		req.Annotations[constants.MasterAnnotationSnapshotRemoteUUIDs])
	assert.Equal(t, "true", req.Annotations[constants.MasterAnnotationSnapshotCrossNode])
	assert.Equal(t, "tpl-T", req.Annotations[constants.MasterAnnotationLaunchMemorySnapshotID])
	assert.NotEmpty(t, req.Annotations[constants.MasterAnnotationRuntimeSnapshotAttachedAt])
}

// A same-node Resume must not come out looking cross-node, or the rootfs
// path would attach the package instead of cloning it.
func TestPauseRestoreBindingKeepsSameNodeResumeLocal(t *testing.T) {
	thin := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{
			constants.MasterAnnotationSnapshotRemoteUUIDs: `{"rootfs":"r1"}`,
		},
	}
	req := &cubebox.RunCubeSandboxRequest{}
	pauseRestoreBindingFrom(thin, "snap-1", "s3").applyTo(req)
	assert.Empty(t, req.Annotations[constants.MasterAnnotationSnapshotCrossNode])
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
