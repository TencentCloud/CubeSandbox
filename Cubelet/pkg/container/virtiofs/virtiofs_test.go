// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package virtiofs

import (
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVv(t *testing.T) {
	d1 := "/data/cubelet/root/io.cubelet.internal.v1.images/cfsrootfs/9.0.224.155-/lam-480nm3z7/12e957dbcefaef9d4bc0375c2b22af21190a373a9de3506920a38b39c108abbf/fs"
	sharePath := strings.TrimSuffix(d1, "/fs")
	assert.Equal(t, "/data/cubelet/root/io.cubelet.internal.v1.images/cfsrootfs/9.0.224.155-/lam-480nm3z7/12e957dbcefaef9d4bc0375c2b22af21190a373a9de3506920a38b39c108abbf", sharePath)
	d2 := "ddf/fs"
	sharePath = strings.TrimRight(d2, "/fs")
	assert.Equal(t, "dd", sharePath)
}

func TestGenVirtiofsConfig(t *testing.T) {
	overlay := []ShareDirMapping{
		{
			SharePath: "/data/cubelet/a/overlay1",
		},
		{
			SharePath: "/data/cubelet/b/overlay2",
		},
	}
	mounts := []specs.Mount{
		{
			Source: "/data/cubelet/mount1",
		},
		{
			Source: "/data/cubelet/mount2",
		},
	}

	config := &CubeRootfsInfo{}

	mountsConfig, err := GenMountConfig(mounts)
	require.NoError(t, err)
	require.NotNil(t, mountsConfig)

	config.Mounts = mountsConfig
	config.Overlay = GenOverlayMountConfig(overlay)

	virtiofsConfig, err := GenVirtiofsConfig(config.ShareDirs())

	assert.NoError(t, err)
	assert.NotNil(t, virtiofsConfig)
	assert.Equal(t, []string{
		"/data/cubelet/mount1",
		"/data/cubelet/mount2",
		"/data/cubelet/a/overlay1",
		"/data/cubelet/b/overlay2",
	}, virtiofsConfig.VirtioBackendFsConfig.AllowedDirs)
}

func TestGenVirtiofsConfig_Error(t *testing.T) {
	shared := []string{"/data/cubelet/overlay1", "/data/cubelet/overlay1"}
	virtiofsConfig, err := GenVirtiofsConfig(shared)
	require.NoError(t, err)
	require.NotNil(t, virtiofsConfig)
	assert.Equal(t, []string{"/data/cubelet/overlay1"}, virtiofsConfig.VirtioBackendFsConfig.AllowedDirs)

	shared = []string{"/overlay1", "/overlay2"}
	virtiofsConfig, err = GenVirtiofsConfig(shared)

	assert.Error(t, err)
	assert.Nil(t, virtiofsConfig)
}

func TestGenMountConfig(t *testing.T) {
	overlay := []ShareDirMapping{
		{
			MountPath: "/a/overlay1",
		},
		{
			MountPath: "/b/overlay2",
		},
	}
	mounts := []specs.Mount{
		{
			Source:      "/data/cubelet/mount1",
			Destination: "/dest1",
			Type:        "bind",
			Options:     []string{"rw"},
		},
		{
			Source:      "/data/cubelet/mount2",
			Destination: "/dest2",
			Type:        "bind",
			Options:     []string{"rw"},
		},
	}

	rootfsInfo := &CubeRootfsInfo{}

	mountsConfig, err := GenMountConfig(mounts)
	assert.NoError(t, err)
	rootfsInfo.Mounts = mountsConfig
	rootfsInfo.Overlay = GenOverlayMountConfig(overlay)

	assert.NoError(t, err)
	assert.NotNil(t, rootfsInfo)
	assert.Equal(t, 2, len(rootfsInfo.Overlay.VirtiofsLowerDir))
	assert.Equal(t, "/a/overlay1", rootfsInfo.Overlay.VirtiofsLowerDir[0])
	assert.Equal(t, "/b/overlay2", rootfsInfo.Overlay.VirtiofsLowerDir[1])
	assert.Equal(t, 2, len(rootfsInfo.Mounts))
	assert.Equal(t, "mount1", rootfsInfo.Mounts[0].VirtiofsSource)
	assert.Equal(t, "/dest1", rootfsInfo.Mounts[0].ContainerDest)
	assert.Equal(t, "bind", rootfsInfo.Mounts[0].Type)
	assert.Equal(t, []string{"rw"}, rootfsInfo.Mounts[1].Options)
	assert.Equal(t, "mount2", rootfsInfo.Mounts[1].VirtiofsSource)
	assert.Equal(t, "/dest2", rootfsInfo.Mounts[1].ContainerDest)
	assert.Equal(t, "bind", rootfsInfo.Mounts[1].Type)
	assert.Equal(t, []string{"rw"}, rootfsInfo.Mounts[1].Options)
}

func TestGenMountConfig_InvalidSharePath(t *testing.T) {
	mounts := []specs.Mount{
		{
			Source:      "/invalid/mount",
			Destination: "/dest1",
			Type:        "bind",
			Options:     []string{"rw"},
		},
	}

	rootfsInfo, err := GenMountConfig(mounts)

	assert.Error(t, err)
	assert.Nil(t, rootfsInfo)
}

func TestCheckVmRelativePath(t *testing.T) {

	t.Run("relative path", func(t *testing.T) {
		path := virtioFsSharePath + "/relative/path"
		result := CheckVmRelativePath(path)
		assert.True(t, result, "Expected the path to be recognized as relative")
	})

	t.Run("non-relative path", func(t *testing.T) {
		path := "/non/relative/path"
		result := CheckVmRelativePath(path)
		assert.False(t, result, "Expected the path to be recognized as non-relative")
	})

	t.Run("exact path", func(t *testing.T) {
		path := virtioFsSharePath
		result := CheckVmRelativePath(path)
		assert.True(t, result, "Expected the path to be recognized as relative")
	})
}

func TestPropagationDir(t *testing.T) {

	got := GenPropagationVirtioDirs()
	t.Logf("got: %s", got)
	expect := `[{"name":"virtio_ro"},{"name":"virtio_rw"}]`
	assert.Equal(t, got, expect)

	got = GenPropagationContainerDirs()
	t.Logf("got: %s", got)
	expect = `[{"name":"virtio_ro","container_dir":"/.container_ro"},{"name":"virtio_rw","container_dir":"/.container_rw"}]`
	assert.Equal(t, got, expect)
}
