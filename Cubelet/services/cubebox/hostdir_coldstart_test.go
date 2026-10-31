// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func coldStartTestStorageInfo() *storage.StorageInfo {
	return &storage.StorageInfo{
		HostDirBackendInfos: map[string]*storage.HostDirBackendInfo{
			"cfs-data/probe": {
				VolumeName: "cfs-data",
				ShareDir:   "/data/cubelet/hostdir/sb/rw",
				BindPath:   "/data/cubelet/hostdir/sb/rw/probe",
			},
			"skill-docs/docs": {
				VolumeName: "skill-docs",
				ShareDir:   "/data/cubelet/hostdir/sb/ro",
				BindPath:   "/data/cubelet/hostdir/sb/ro/docs",
				ReadOnly:   true,
			},
		},
	}
}

func coldStartTestContainer() *cubebox.ContainerConfig {
	return &cubebox.ContainerConfig{VolumeMounts: []*cubebox.VolumeMounts{
		{Name: "rootfs", ContainerPath: "/"},
		{Name: "skill-docs", ContainerPath: "/opt/skill-docs", Readonly: true},
		{Name: "cfs-data", ContainerPath: "/data"},
	}}
}

func TestColdStartHostDirMountsColdStart(t *testing.T) {
	t.Parallel()

	flowOpts := &workflow.CreateContext{
		ReqInfo:     &cubebox.RunCubeSandboxRequest{InstanceType: cubebox.InstanceType_cubebox.String()},
		StorageInfo: coldStartTestStorageInfo(),
	}

	mounts := coldStartHostDirMounts(context.Background(), flowOpts, coldStartTestContainer())

	require.Equal(t, []virtiofs.CubeRootfsMount{
		{
			VirtiofsID:     constants.PropagationVirtioRw,
			VirtiofsSource: "probe",
			ContainerDest:  "/data",
			Type:           constants.MountTypeBind,
			Options:        []string{constants.MountOptBindRO, constants.MountOptReadWrite},
		},
		{
			VirtiofsID:     constants.PropagationVirtioRo,
			VirtiofsSource: "docs",
			ContainerDest:  "/opt/skill-docs",
			Type:           constants.MountTypeBind,
			Options:        []string{constants.MountOptBindRO, constants.MountOptReadOnly},
		},
	}, mounts)

	// The backends ride on the sandbox virtio channels, so they must not be
	// added to the container cube.fs share.
	info := &virtiofs.CubeRootfsInfo{Mounts: mounts}
	require.Empty(t, info.ShareDirs())
}

func TestColdStartHostDirMountsSkipsNonColdStart(t *testing.T) {
	t.Parallel()

	cubeboxType := cubebox.InstanceType_cubebox.String()
	tests := []struct {
		name string
		req  *cubebox.RunCubeSandboxRequest
	}{
		{
			name: "template restore",
			req: &cubebox.RunCubeSandboxRequest{InstanceType: cubeboxType, Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-1",
			}},
		},
		{
			name: "create snapshot",
			req: &cubebox.RunCubeSandboxRequest{InstanceType: cubeboxType, Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-1",
				constants.MasterAnnotationsAppSnapshotCreate:    "true",
			}},
		},
		{
			name: "runtime snapshot restore",
			req: &cubebox.RunCubeSandboxRequest{InstanceType: cubeboxType, Annotations: map[string]string{
				constants.MasterAnnotationRuntimeSnapshotID: "snap-1",
			}},
		},
		{
			name: "non cubebox instance",
			req:  &cubebox.RunCubeSandboxRequest{InstanceType: "runc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			flowOpts := &workflow.CreateContext{ReqInfo: tt.req, StorageInfo: coldStartTestStorageInfo()}
			require.Empty(t, coldStartHostDirMounts(context.Background(), flowOpts, coldStartTestContainer()))
		})
	}
}

func TestColdStartHostDirMountsRestartColdBoots(t *testing.T) {
	t.Parallel()

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations:  map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: "tpl-1"},
		},
		StorageInfo: coldStartTestStorageInfo(),
		IsRestart:   true,
	}

	mounts := coldStartHostDirMounts(context.Background(), flowOpts, coldStartTestContainer())
	require.Len(t, mounts, 2)
}

func TestColdStartHostDirMountsOrderAndSkips(t *testing.T) {
	t.Parallel()

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{InstanceType: cubebox.InstanceType_cubebox.String()},
		StorageInfo: &storage.StorageInfo{HostDirBackendInfos: map[string]*storage.HostDirBackendInfo{
			"child/out":   {VolumeName: "child", BindPath: "/data/cubelet/hostdir/sb/rw/out"},
			"parent/team": {VolumeName: "parent", BindPath: "/data/cubelet/hostdir/sb/ro/team", ReadOnly: true},
			"empty/e":     {VolumeName: "empty", BindPath: "/data/cubelet/hostdir/sb/rw/e"},
			"other/o":     {VolumeName: "unmounted", BindPath: "/data/cubelet/hostdir/sb/rw/o"},
		}},
	}
	containerReq := &cubebox.ContainerConfig{VolumeMounts: []*cubebox.VolumeMounts{
		{Name: "child", ContainerPath: "/workspace/team/members/a"},
		{Name: "parent", ContainerPath: "/workspace/team", Readonly: true},
		{Name: "empty"},
	}}

	for range 32 {
		mounts := coldStartHostDirMounts(context.Background(), flowOpts, containerReq)
		require.Len(t, mounts, 2)
		require.Equal(t, "/workspace/team", mounts[0].ContainerDest)
		require.Equal(t, constants.PropagationVirtioRo, mounts[0].VirtiofsID)
		require.Equal(t, "/workspace/team/members/a", mounts[1].ContainerDest)
		require.Equal(t, constants.PropagationVirtioRw, mounts[1].VirtiofsID)
	}
}

func TestColdStartHostDirMountsNoHostDir(t *testing.T) {
	t.Parallel()

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{InstanceType: cubebox.InstanceType_cubebox.String()},
	}
	require.Empty(t, coldStartHostDirMounts(context.Background(), flowOpts, coldStartTestContainer()))
	require.Empty(t, coldStartHostDirMounts(context.Background(), nil, coldStartTestContainer()))
}
