// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func TestResolveSnapshotRuntimeArtifactsFallsBackToSnapshotConfig(t *testing.T) {
	t.Parallel()

	snapshotPath := t.TempDir()
	configDir := filepath.Join(snapshotPath, "snapshot")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll error=%v", err)
	}

	configJSON := `{
		"payload": {
			"kernel": "/opt/cube/kernel/image.vm"
		},
		"pmem": [
			{
				"id": "_pmem0",
				"file": "/opt/cube/guest-image.ext4"
			},
			{
				"id": "pmem-cubebox-image-0",
				"file": "/opt/cube/app-image.ext4"
			}
		]
	}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("WriteFile error=%v", err)
	}

	localTemplate := &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{
			templatetypes.CubeComponentCubeShim: {
				Component: templatetypes.MachineComponent{
					Path: "/opt/cube/shim/containerd-shim-cube-rs",
				},
			},
		},
	}

	p := &cubeboxInstancePlugin{}
	kernelPath, imagePath, err := p.resolveSnapshotRuntimeArtifacts(snapshotPath, localTemplate)
	if err != nil {
		t.Fatalf("resolveSnapshotRuntimeArtifacts error=%v", err)
	}
	if kernelPath != "/opt/cube/kernel/image.vm" {
		t.Fatalf("kernelPath=%q, want %q", kernelPath, "/opt/cube/kernel/image.vm")
	}
	if imagePath != "/opt/cube/app-image.ext4" {
		t.Fatalf("imagePath=%q, want %q", imagePath, "/opt/cube/app-image.ext4")
	}
}

func TestResolveSnapshotRuntimeArtifactsPrefersTemplateComponents(t *testing.T) {
	t.Parallel()

	p := &cubeboxInstancePlugin{}
	localTemplate := &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{
			templatetypes.CubeComponentCubeKernel: {
				Component: templatetypes.MachineComponent{
					Path: "/template/kernel.vm",
				},
			},
			templatetypes.CubeComponentCubeImage: {
				Component: templatetypes.MachineComponent{
					Path: "/template/image.ext4",
				},
			},
		},
	}

	kernelPath, imagePath, err := p.resolveSnapshotRuntimeArtifacts(t.TempDir(), localTemplate)
	if err != nil {
		t.Fatalf("resolveSnapshotRuntimeArtifacts error=%v", err)
	}
	if kernelPath != "/template/kernel.vm" {
		t.Fatalf("kernelPath=%q, want %q", kernelPath, "/template/kernel.vm")
	}
	if imagePath != "/template/image.ext4" {
		t.Fatalf("imagePath=%q, want %q", imagePath, "/template/image.ext4")
	}
}

func TestInferSnapshotResDirFromRequest(t *testing.T) {
	t.Parallel()

	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			{
				Resources: &cubebox.Resource{
					Cpu: "2000m",
					Mem: "2000Mi",
				},
			},
		},
	}

	resDir, err := inferSnapshotResDirFromRequest(req)
	if err != nil {
		t.Fatalf("inferSnapshotResDirFromRequest error=%v", err)
	}
	if resDir != "2C2000M" {
		t.Fatalf("resDir=%q, want %q", resDir, "2C2000M")
	}
}

func TestResolveSnapshotPathsEmptyRawPathUsesTemplateBase(t *testing.T) {
	t.Parallel()

	p := &cubeboxInstancePlugin{
		config: &cubeboxInstancePluginConfig{
			SnapShotBasePath: "/snapshots",
			instanceType:     cubebox.InstanceType_cubebox.String(),
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			{
				Resources: &cubebox.Resource{
					Cpu: "2000m",
					Mem: "2000Mi",
				},
			},
		},
	}

	paths, err := p.resolveSnapshotPaths("tpl-1", "", req)
	if err != nil {
		t.Fatalf("resolveSnapshotPaths error=%v", err)
	}
	if paths.Base != "/snapshots/cubebox/tpl-1" {
		t.Fatalf("Base=%q, want %q", paths.Base, "/snapshots/cubebox/tpl-1")
	}
	if paths.Spec != "/snapshots/cubebox/tpl-1/2C2000M" {
		t.Fatalf("Spec=%q, want %q", paths.Spec, "/snapshots/cubebox/tpl-1/2C2000M")
	}
}

func TestResolveSnapshotPathsNormalizesTemporarySpecPath(t *testing.T) {
	t.Parallel()

	p := &cubeboxInstancePlugin{
		config: &cubeboxInstancePluginConfig{
			SnapShotBasePath: "/snapshots",
			instanceType:     cubebox.InstanceType_cubebox.String(),
		},
	}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{
			{
				Resources: &cubebox.Resource{
					Cpu: "2000m",
					Mem: "2000Mi",
				},
			},
		},
	}

	paths, err := p.resolveSnapshotPaths("tpl-1", "/snapshots/cubebox/tpl-1/2C2000M.tmp", req)
	if err != nil {
		t.Fatalf("resolveSnapshotPaths error=%v", err)
	}
	if paths.Base != "/snapshots/cubebox/tpl-1" {
		t.Fatalf("Base=%q, want %q", paths.Base, "/snapshots/cubebox/tpl-1")
	}
	if paths.Spec != "/snapshots/cubebox/tpl-1/2C2000M" {
		t.Fatalf("Spec=%q, want %q", paths.Spec, "/snapshots/cubebox/tpl-1/2C2000M")
	}
}

func TestSnapshotRestoreContainerIDUsesMetadataWhenPresent(t *testing.T) {
	t.Parallel()

	snapshotPath := t.TempDir()
	metadataJSON := `{"app_snapshot_container_id":"tpl-1e0d677b60a0499c80f49e55_0"}`
	if err := os.WriteFile(filepath.Join(snapshotPath, "metadata.json"), []byte(metadataJSON), 0o644); err != nil {
		t.Fatalf("WriteFile error=%v", err)
	}

	got := snapshotRestoreContainerID("snap-123", snapshotPath)
	if got != "tpl-1e0d677b60a0499c80f49e55_0" {
		t.Fatalf("snapshotRestoreContainerID=%q, want %q", got, "tpl-1e0d677b60a0499c80f49e55_0")
	}
}

func TestSnapshotRestoreContainerIDFallsBackToSnapshotID(t *testing.T) {
	t.Parallel()

	got := snapshotRestoreContainerID("snap-123", t.TempDir())
	if got != "snap-123_0" {
		t.Fatalf("snapshotRestoreContainerID=%q, want %q", got, "snap-123_0")
	}
}

func TestCreateSandboxDiskOnlyRestoreColdBoots(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	artifactID := "artifact-diskonly"
	targetKernelPath := plugin.getKernelFilePath(artifactID)
	writeTestFile(t, targetKernelPath, []byte("kernel"))
	writeKernelVersion(t, targetKernelPath, []byte("kernel"))

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "snap-imported",
				constants.MasterAnnotationAppSnapshotVersion:    "v2",
			},
			Containers: []*cubebox.ContainerConfig{
				{
					Resources: &cubebox.Resource{Cpu: "1000m", Mem: "1000Mi"},
					Image: &cubeimages.ImageSpec{
						Image:        artifactID,
						StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
					},
				},
			},
		},
		StorageInfo: &storage.StorageInfo{RestoreDiskOnly: true},
	}
	ctx := constants.WithAppImageID(context.Background(), artifactID)

	specOpts, err := plugin.CreateSandbox(ctx, flowOpts)
	require.NoError(t, err)

	spec := applySpecOpts(t, ctx, specOpts)
	require.Equal(t, "true", spec.Annotations[constants.AnnotationAppSnapshotRestore])
	require.Equal(t, "true", spec.Annotations[constants.AnnotationVMSnapshotDiskOnly])
	require.NotContains(t, spec.Annotations, constants.AnnotationVMSnapshotPath)
	require.NotContains(t, spec.Annotations, constants.AnnotationVMSnapshotMemoryVolURL)
	require.NotContains(t, spec.Annotations, constants.AnnotationAppSnapshotContainerID)
	require.Equal(t, targetKernelPath, spec.Annotations[constants.AnnotationsVMKernelPath])
}

func TestCreateSandboxRestoreWithoutStorageInfoFails(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	artifactID := "artifact-unresolved"
	targetKernelPath := plugin.getKernelFilePath(artifactID)
	writeTestFile(t, targetKernelPath, []byte("kernel"))
	writeKernelVersion(t, targetKernelPath, []byte("kernel"))

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "snap-unresolved",
				constants.MasterAnnotationAppSnapshotVersion:    "v2",
			},
			Containers: []*cubebox.ContainerConfig{
				{
					Resources: &cubebox.Resource{Cpu: "1000m", Mem: "1000Mi"},
					Image: &cubeimages.ImageSpec{
						Image:        artifactID,
						StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
					},
				},
			},
		},
	}
	ctx := constants.WithAppImageID(context.Background(), artifactID)

	_, err := plugin.CreateSandbox(ctx, flowOpts)
	require.ErrorContains(t, err, "missing prefetched memory volume")
}
