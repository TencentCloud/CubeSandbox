// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

func TestDefaultTemplateImageSpecSetsExt4InstanceType(t *testing.T) {
	spec := defaultTemplateImageSpec("test-ns", &templatetypes.TemplateImage{
		Image:        "artifact-1",
		StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
	})
	require.Equal(t, cubebox.InstanceType_cubebox.String(), spec.Annotations[constants.MasterAnnotationInstanceType])
}

func TestMaterializeDistributedTemplateRuntimeFilesRefreshesKernel(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	template := &templatetypes.TemplateImage{
		Image:        "artifact-1",
		StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
	}
	sharedKernelPath := pmem.GetSharedKernelFilePath()
	require.NoError(t, os.MkdirAll(filepath.Dir(sharedKernelPath), 0o755))
	require.NoError(t, os.WriteFile(sharedKernelPath, bytes.Repeat([]byte("s"), 4096), 0o644))

	sharedVersionPath := pmem.GetSharedImageVersionFilePath()
	require.NoError(t, os.MkdirAll(filepath.Dir(sharedVersionPath), 0o755))
	require.NoError(t, os.WriteFile(sharedVersionPath, []byte("2.2.0-20251010\n"), 0o644))

	targetKernelPath := pmem.GetRawKernelFilePath(cubebox.InstanceType_cubebox.String(), template.Image)
	require.NoError(t, os.MkdirAll(filepath.Dir(targetKernelPath), 0o755))
	require.NoError(t, os.WriteFile(targetKernelPath, bytes.Repeat([]byte("o"), 2048), 0o644))

	err := materializeDistributedTemplateRuntimeFiles(context.Background(), template)
	require.NoError(t, err)

	gotKernel, err := os.ReadFile(targetKernelPath)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte("s"), 4096), gotKernel)
}

func TestMaterializeDistributedTemplateRuntimeFilesSkipsNonExt4(t *testing.T) {
	err := materializeDistributedTemplateRuntimeFiles(context.Background(), &templatetypes.TemplateImage{
		Image:        "artifact-1",
		StorageMedia: "overlayfs",
	})
	require.NoError(t, err)
}
