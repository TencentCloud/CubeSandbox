// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestRejectUnsupportedXFSS3Resources(t *testing.T) {
	cubeletReq := func(backend string) *cubebox.RunCubeSandboxRequest {
		return &cubebox.RunCubeSandboxRequest{Annotations: map[string]string{constants.CubeAnnotationStorageBackend: backend}}
	}
	rootfs := &types.Volume{Name: "cube_rootfs_rw", VolumeSource: &types.VolumeSource{EmptyDir: &types.EmptyDirVolumeSource{SizeLimit: "32Gi"}}}
	plugin := &types.Volume{Name: "data", VolumeSource: &types.VolumeSource{PluginVolume: &types.PluginVolumeSource{}}}
	hostDir := &types.Volume{Name: "host", VolumeSource: &types.VolumeSource{HostDirVolumeSources: &types.HostDirVolumeSources{
		VolumeSources: []*types.HostDirSource{{Name: "h", HostPath: "/data/x"}}}}}
	hostMount := map[string]string{AnnotationHostDirMount: `[{"hostPath":"/data/x","mountPath":"/x"}]`}

	require.NoError(t, rejectUnsupportedXFSS3Resources(&types.CreateCubeSandboxReq{Volumes: []*types.Volume{rootfs}}, cubeletReq("xfs-s3")))
	require.Error(t, rejectUnsupportedXFSS3Resources(&types.CreateCubeSandboxReq{Volumes: []*types.Volume{rootfs, plugin}}, cubeletReq("xfs-s3")))
	require.Error(t, rejectUnsupportedXFSS3Resources(&types.CreateCubeSandboxReq{Volumes: []*types.Volume{rootfs, hostDir}}, cubeletReq("xfs-s3")))
	require.Error(t, rejectUnsupportedXFSS3Resources(&types.CreateCubeSandboxReq{Annotations: hostMount}, cubeletReq("xfs-s3")))

	// Other backends keep accepting what they accepted before.
	for _, b := range []string{"xfs", "s3", "jfs", ""} {
		require.NoError(t, rejectUnsupportedXFSS3Resources(&types.CreateCubeSandboxReq{
			Annotations: hostMount, Volumes: []*types.Volume{rootfs, plugin, hostDir}}, cubeletReq(b)), b)
	}
}
