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

func writeImageTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}

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
	writeImageTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 4096))

	sharedVersionPath := pmem.GetSharedImageVersionFilePath()
	writeImageTestFile(t, sharedVersionPath, []byte("2.2.0-20251010\n"))

	targetKernelPath := pmem.GetRawKernelFilePath(cubebox.InstanceType_cubebox.String(), template.Image)
	writeImageTestFile(t, targetKernelPath, bytes.Repeat([]byte("o"), 2048))

	err := materializeDistributedTemplateRuntimeFiles(context.Background(), template)
	require.NoError(t, err)

	gotKernel, err := os.ReadFile(targetKernelPath)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte("s"), 4096), gotKernel)

	targetVersionPath := pmem.GetRawImageVersionFilePath(cubebox.InstanceType_cubebox.String(), template.Image)
	gotVersion, err := os.ReadFile(targetVersionPath)
	require.NoError(t, err)
	require.Equal(t, []byte("2.2.0-20251010\n"), gotVersion)
}

func TestMaterializeDistributedTemplateRuntimeFilesSkipsNonExt4(t *testing.T) {
	err := materializeDistributedTemplateRuntimeFiles(context.Background(), &templatetypes.TemplateImage{
		Image:        "artifact-1",
		StorageMedia: "overlayfs",
	})
	require.NoError(t, err)
}

func TestEnsureDistributedTemplateImageExt4DoesNotRequireKernelFile(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	imagePath := pmem.GetRawImageFilePath(cubebox.InstanceType_cubebox.String(), "artifact-2")
	writeImageTestFile(t, imagePath, bytes.Repeat([]byte("e"), 4096))

	err := ensureDistributedTemplateImage(context.Background(), nil, &templatetypes.TemplateImage{
		Image:        "artifact-2",
		StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
	})
	require.NoError(t, err)
}
