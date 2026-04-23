// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func TestCreateSandboxCreateSnapshotRefreshesArtifactKernel(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	artifactID := "artifact-1"
	sharedKernelPath := filepath.Join(plugin.config.BasePath, "cube-kernel-scf", "vmlinux")
	targetKernelPath := plugin.getKernelFilePath(artifactID)
	writeTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 4096))
	writeTestFile(t, targetKernelPath, bytes.Repeat([]byte("o"), 2048))

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationsAppSnapshotCreate:    "true",
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-1",
			},
			Containers: []*cubebox.ContainerConfig{
				{
					Resources: &cubebox.Resource{Cpu: "2000m", Mem: "2000Mi"},
					Image: &cubeimages.ImageSpec{
						Image:        artifactID,
						StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
					},
				},
			},
		},
	}
	ctx := constants.WithAppImageID(context.Background(), artifactID)

	specOpts, err := plugin.CreateSandbox(ctx, flowOpts)
	require.NoError(t, err)

	gotKernel, err := os.ReadFile(targetKernelPath)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte("s"), 4096), gotKernel)

	spec := applySpecOpts(t, ctx, specOpts)
	require.Equal(t, targetKernelPath, spec.Annotations[constants.AnnotationsVMKernelPath])
}

func TestCreateSandboxRestoreDoesNotRefreshArtifactKernel(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	artifactID := "artifact-2"
	sharedKernelPath := filepath.Join(plugin.config.BasePath, "cube-kernel-scf", "vmlinux")
	targetKernelPath := plugin.getKernelFilePath(artifactID)
	oldKernel := bytes.Repeat([]byte("o"), 2048)
	writeTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 4096))
	writeTestFile(t, targetKernelPath, oldKernel)

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-restore",
				constants.MasterAnnotationAppSnapshotVersion:    "v2",
			},
			Containers: []*cubebox.ContainerConfig{
				{
					Resources: &cubebox.Resource{Cpu: "2000m", Mem: "2000Mi"},
					Image: &cubeimages.ImageSpec{
						Image:        artifactID,
						StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
					},
				},
			},
		},
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Componts: map[string]templatetypes.LocalComponent{
				templatetypes.CubeComponentCubeKernel: {
					Component: templatetypes.MachineComponent{Path: "/template/kernel.vm"},
				},
				templatetypes.CubeComponentCubeImage: {
					Component: templatetypes.MachineComponent{Path: "/template/image.ext4"},
				},
			},
		},
	}
	ctx := constants.WithAppImageID(context.Background(), artifactID)

	specOpts, err := plugin.CreateSandbox(ctx, flowOpts)
	require.NoError(t, err)

	gotKernel, err := os.ReadFile(targetKernelPath)
	require.NoError(t, err)
	require.Equal(t, oldKernel, gotKernel)

	spec := applySpecOpts(t, ctx, specOpts)
	require.Equal(t, "/template/kernel.vm", spec.Annotations[constants.AnnotationsVMKernelPath])
}

func newTestCubeboxPlugin(t *testing.T) *cubeboxInstancePlugin {
	t.Helper()

	basePath := t.TempDir()
	return &cubeboxInstancePlugin{
		config: &cubeboxInstancePluginConfig{
			BasePath:         basePath,
			ImageBasePath:    filepath.Join(basePath, "cubebox_os_image"),
			KernelBasePath:   filepath.Join(basePath, "cubebox_os_image"),
			SnapShotBasePath: filepath.Join(basePath, "cube-snapshot"),
			instanceType:     cubebox.InstanceType_cubebox.String(),
		},
	}
}

func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}

func applySpecOpts(t *testing.T, ctx context.Context, specOpts []oci.SpecOpts) *specs.Spec {
	t.Helper()

	spec := &specs.Spec{
		Annotations: map[string]string{},
		Linux:       &specs.Linux{},
		Process:     &specs.Process{},
	}
	for _, specOpt := range specOpts {
		require.NoError(t, specOpt(ctx, nil, &containers.Container{}, spec))
	}
	return spec
}
