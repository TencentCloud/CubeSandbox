// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestInjectHostDirMountsAcceptsCanonicalAndLegacyAnnotations(t *testing.T) {
	raw := `[
		{"hostPath":"/tmp/rw","mountPath":"/mnt/rw","readOnly":false},
		{"hostPath":"/tmp/ro","mountPath":"/mnt/ro","readOnly":true}
	]`

	for _, tt := range []struct {
		name string
		key  string
	}{
		{name: "canonical", key: AnnotationHostDirMount},
		{name: "legacy", key: AnnotationHostDirMountLegacy},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{tt.key: raw},
				Containers:  []*types.Container{{Name: "c0"}},
			}

			require.NoError(t, injectHostDirMounts(context.Background(), req))
			require.Len(t, req.Volumes, 2)
			require.Equal(t, "hostdir-0", req.Volumes[0].Name)
			require.Equal(t, "/tmp/rw", req.Volumes[0].VolumeSource.HostDirVolumeSources.VolumeSources[0].HostPath)
			require.Equal(t, "hostdir-1", req.Volumes[1].Name)
			require.Equal(t, "/tmp/ro", req.Volumes[1].VolumeSource.HostDirVolumeSources.VolumeSources[0].HostPath)

			require.Len(t, req.Containers[0].VolumeMounts, 2)
			require.Equal(t, "/mnt/rw", req.Containers[0].VolumeMounts[0].ContainerPath)
			require.False(t, req.Containers[0].VolumeMounts[0].Readonly)
			require.Equal(t, "/mnt/ro", req.Containers[0].VolumeMounts[1].ContainerPath)
			require.True(t, req.Containers[0].VolumeMounts[1].Readonly)
		})
	}
}

func TestInjectHostDirMountsPrefersCanonicalAnnotation(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationHostDirMountLegacy: `[{"hostPath":"/legacy","mountPath":"/mnt/legacy"}]`,
			AnnotationHostDirMount:       `[{"hostPath":"/canonical","mountPath":"/mnt/canonical"}]`,
		},
		Containers: []*types.Container{{Name: "c0"}},
	}

	require.NoError(t, injectHostDirMounts(context.Background(), req))
	require.Len(t, req.Volumes, 1)
	require.Equal(t, "/canonical", req.Volumes[0].VolumeSource.HostDirVolumeSources.VolumeSources[0].HostPath)
	require.Equal(t, "/mnt/canonical", req.Containers[0].VolumeMounts[0].ContainerPath)
}
