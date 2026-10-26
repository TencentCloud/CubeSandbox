// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package tmpfs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestGenMountDefault(t *testing.T) {
	ctx := context.Background()
	c := &cubebox.ContainerConfig{}
	mounts := GenMount(ctx, c)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, "/dev/shm", mounts[0].Destination)
	assert.Equal(t, "shm", mounts[0].Source)
	require.Equal(t, 5, len(mounts[0].Options))
	assert.Equal(t, "size=65536k", mounts[0].Options[4])
}

func TestGenMount(t *testing.T) {
	ctx := context.Background()
	c := &cubebox.ContainerConfig{
		Annotations: map[string]string{
			"dev_shm_size_in_bytes": "1024",
		},
	}
	mounts := GenMount(ctx, c)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, "/dev/shm", mounts[0].Destination)
	assert.Equal(t, "shm", mounts[0].Source)
	require.Equal(t, 5, len(mounts[0].Options))
	assert.Equal(t, "size=1024", mounts[0].Options[4])
}

func TestGenNscdMount(t *testing.T) {
	mount := GenNscdMount()
	assert.Equal(t, constants.MountTypeTmpfs, mount.Type)
	assert.Equal(t, "/var/run/nscd/", mount.Destination)
	assert.Equal(t, 5, len(mount.Options))
}

func TestGenSizeMountDefault(t *testing.T) {
	ctx := context.Background()
	target := "/test"
	size := int64(0)
	mounts := GenSizeMount(ctx, target, size)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	require.Equal(t, 5, len(mounts[0].Options))
	assert.Equal(t, "size=65536k", mounts[0].Options[4])
}

func TestGenSizeMount(t *testing.T) {
	ctx := context.Background()
	target := "/test"
	size := int64(1024)
	mounts := GenSizeMount(ctx, target, size)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	assert.Equal(t, 5, len(mounts[0].Options))
	assert.Equal(t, "size=1024", mounts[0].Options[4])
}

func TestGenSizeMountWithVolumeMount_Nil(t *testing.T) {
	ctx := context.Background()
	target := "/test"
	size := int64(1024)
	mounts := GenSizeMountWithVolumeMount(ctx, target, size, nil)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	require.Equal(t, 5, len(mounts[0].Options))
	assert.Contains(t, mounts[0].Options, "nosuid")
	assert.Contains(t, mounts[0].Options, "noexec")
	assert.Contains(t, mounts[0].Options, "nodev")
	assert.Contains(t, mounts[0].Options, "mode=1777")
	assert.Contains(t, mounts[0].Options, "size=1024")
}

func TestGenSizeMountWithVolumeMount_WithExec(t *testing.T) {
	ctx := context.Background()
	target := "/test"
	size := int64(2048)
	vm := &cubebox.VolumeMounts{
		Exec: true,
	}
	mounts := GenSizeMountWithVolumeMount(ctx, target, size, vm)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	require.Equal(t, 4, len(mounts[0].Options))
	assert.Contains(t, mounts[0].Options, "nosuid")
	assert.Contains(t, mounts[0].Options, "nodev")
	assert.Contains(t, mounts[0].Options, "mode=1777")
	assert.Contains(t, mounts[0].Options, "size=2048")

	assert.NotContains(t, mounts[0].Options, "noexec")
}

func TestGenSizeMountWithVolumeMount_WithUid(t *testing.T) {
	ctx := context.Background()
	target := "/test"
	size := int64(0)
	vm := &cubebox.VolumeMounts{
		Uid: "1300",
	}
	mounts := GenSizeMountWithVolumeMount(ctx, target, size, vm)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	require.Equal(t, 6, len(mounts[0].Options))
	assert.Contains(t, mounts[0].Options, "nosuid")
	assert.Contains(t, mounts[0].Options, "noexec")
	assert.Contains(t, mounts[0].Options, "nodev")
	assert.Contains(t, mounts[0].Options, "mode=1777")
	assert.Contains(t, mounts[0].Options, "uid=1300")
	assert.Contains(t, mounts[0].Options, "size=65536k")
}

func TestGenSizeMountWithVolumeMount_WithGid(t *testing.T) {
	ctx := context.Background()
	target := "/test"
	size := int64(4096)
	vm := &cubebox.VolumeMounts{
		Gid: "1301",
	}
	mounts := GenSizeMountWithVolumeMount(ctx, target, size, vm)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	require.Equal(t, 6, len(mounts[0].Options))
	assert.Contains(t, mounts[0].Options, "nosuid")
	assert.Contains(t, mounts[0].Options, "noexec")
	assert.Contains(t, mounts[0].Options, "nodev")
	assert.Contains(t, mounts[0].Options, "mode=1777")
	assert.Contains(t, mounts[0].Options, "gid=1301")
	assert.Contains(t, mounts[0].Options, "size=4096")
}

func TestGenSizeMountWithVolumeMount_WithUidGidExec(t *testing.T) {
	ctx := context.Background()
	target := "/home/androidusr"
	size := int64(1073741824)
	vm := &cubebox.VolumeMounts{
		Uid:  "1300",
		Gid:  "1301",
		Exec: true,
	}
	mounts := GenSizeMountWithVolumeMount(ctx, target, size, vm)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	require.Equal(t, 6, len(mounts[0].Options))
	assert.Contains(t, mounts[0].Options, "nosuid")
	assert.Contains(t, mounts[0].Options, "nodev")
	assert.Contains(t, mounts[0].Options, "mode=1777")
	assert.Contains(t, mounts[0].Options, "uid=1300")
	assert.Contains(t, mounts[0].Options, "gid=1301")
	assert.Contains(t, mounts[0].Options, "size=1073741824")

	assert.NotContains(t, mounts[0].Options, "noexec")
}

func TestGenSizeMountWithVolumeMount_EmptyUidGid(t *testing.T) {
	ctx := context.Background()
	target := "/test"
	size := int64(1024)
	vm := &cubebox.VolumeMounts{
		Uid:  "",
		Gid:  "",
		Exec: false,
	}
	mounts := GenSizeMountWithVolumeMount(ctx, target, size, vm)
	assert.Equal(t, 1, len(mounts))
	assert.Equal(t, constants.MountTypeTmpfs, mounts[0].Type)
	assert.Equal(t, target, mounts[0].Destination)
	require.Equal(t, 5, len(mounts[0].Options))
	assert.Contains(t, mounts[0].Options, "nosuid")
	assert.Contains(t, mounts[0].Options, "noexec")
	assert.Contains(t, mounts[0].Options, "nodev")
	assert.Contains(t, mounts[0].Options, "mode=1777")
	assert.Contains(t, mounts[0].Options, "size=1024")

	assert.NotContains(t, mounts[0].Options, "uid=")
	assert.NotContains(t, mounts[0].Options, "gid=")
}
