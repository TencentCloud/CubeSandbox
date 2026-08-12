// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func seedWlayerCatalog(t *testing.T, id, wlayerSubdir string) {
	t.Helper()
	snapDir := t.TempDir()
	require.NoError(t, storage.WriteSnapshotCatalog(&storage.SnapshotCatalogEntry{
		SnapshotID:   id,
		InstanceType: "cubebox",
		SpecDir:      "1C1M",
		SnapshotPath: snapDir,
		MetaDir:      snapDir,
		RootfsVol:    "tpl-" + id + "-rootfs",
		RootfsKind:   storage.CowKindSnapshot,
		WlayerSubdir: wlayerSubdir,
		Kind:         storage.CatalogKindTemplate,
	}))
	t.Cleanup(func() { storage.DeleteSnapshotCatalog(id) })
}

func wlayerFlowOpts(templateID, memoryVolURL string) *workflow.CreateContext {
	annotations := map[string]string{}
	if templateID != "" {
		annotations[constants.MasterAnnotationAppSnapshotTemplateID] = templateID
	}
	return &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: "cubebox",
			Annotations:  annotations,
		},
		StorageInfo: &storage.StorageInfo{
			RestoreMemoryVolURL: memoryVolURL,
			RestoreDiskOnly:     memoryVolURL == "",
			Volumes: map[string]*storage.BackendFileInfo{
				"tmp": {Name: "tmp", FilePath: "/dev/mapper/sb-rootfs"},
			},
		},
	}
}

func wlayerContainerReq() *cubebox.ContainerConfig {
	return &cubebox.ContainerConfig{
		Id: "newctr",
		VolumeMounts: []*cubebox.VolumeMounts{
			{Name: "tmp", ContainerPath: "/"},
		},
	}
}

func wlayerAnnotation(t *testing.T, opt oci.SpecOpts) string {
	t.Helper()
	spec := &oci.Spec{}
	require.NoError(t, opt(context.Background(), nil, &containers.Container{}, spec))
	return spec.Annotations[constants.AnnotationsRootfsWlayerSubdir]
}

func TestPrepareWritableRootfsWlayerSubdir(t *testing.T) {
	l := &local{}

	t.Run("disk-only restore uses catalog override", func(t *testing.T) {
		seedWlayerCatalog(t, "snap-override", "disk/oldctr")
		opt, err := l.prepareWritableRootfs(context.Background(), wlayerFlowOpts("snap-override", ""), wlayerContainerReq())
		require.NoError(t, err)
		require.Equal(t, "disk/oldctr", wlayerAnnotation(t, opt))
	})

	t.Run("disk-only restore fails on missing catalog", func(t *testing.T) {
		_, err := l.prepareWritableRootfs(context.Background(), wlayerFlowOpts("snap-absent", ""), wlayerContainerReq())
		require.ErrorContains(t, err, "read snapshot catalog")
	})

	t.Run("disk-only restore fails on empty subdir", func(t *testing.T) {
		seedWlayerCatalog(t, "snap-empty", "")
		_, err := l.prepareWritableRootfs(context.Background(), wlayerFlowOpts("snap-empty", ""), wlayerContainerReq())
		require.ErrorContains(t, err, "records no wlayer_subdir")
	})

	t.Run("disk-only restore rejects traversal subdir", func(t *testing.T) {
		seedWlayerCatalog(t, "snap-evil", "../../etc")
		_, err := l.prepareWritableRootfs(context.Background(), wlayerFlowOpts("snap-evil", ""), wlayerContainerReq())
		require.ErrorContains(t, err, "invalid snapshot wlayer_subdir")
	})

	t.Run("disk-only restore accepts a segment containing dots", func(t *testing.T) {
		seedWlayerCatalog(t, "snap-dots", "disk/a..b")
		opt, err := l.prepareWritableRootfs(context.Background(), wlayerFlowOpts("snap-dots", ""), wlayerContainerReq())
		require.NoError(t, err)
		require.Equal(t, "disk/a..b", wlayerAnnotation(t, opt))
	})

	t.Run("memory-resume restore keeps default without catalog", func(t *testing.T) {
		opt, err := l.prepareWritableRootfs(context.Background(), wlayerFlowOpts("snap-mem", "file:///dev/mapper/mem"), wlayerContainerReq())
		require.NoError(t, err)
		require.Equal(t, "disk/newctr", wlayerAnnotation(t, opt))
	})

	t.Run("normal create keeps default", func(t *testing.T) {
		opt, err := l.prepareWritableRootfs(context.Background(), wlayerFlowOpts("", ""), wlayerContainerReq())
		require.NoError(t, err)
		require.Equal(t, "disk/newctr", wlayerAnnotation(t, opt))
	})
}
