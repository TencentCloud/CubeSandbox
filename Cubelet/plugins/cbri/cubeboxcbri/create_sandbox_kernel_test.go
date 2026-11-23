// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images/rootfsimage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func TestCreateSandboxCreateSnapshotRefreshesArtifactKernel(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	artifactID := "artifact-1"
	rootfsPath, err := plugin.artifactPaths.RootfsFile(plugin.config.instanceType, artifactID, "ext4")
	require.NoError(t, err)
	writeTestFile(t, rootfsPath, bytes.Repeat([]byte("rootfs"), 1024))
	sharedKernelPath := plugin.artifactPaths.SharedKernelPath
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
	require.Equal(t, kernelVersionForContent(gotKernel), readKernelVersion(t, targetKernelPath))

	spec := applySpecOpts(t, ctx, specOpts)
	require.Equal(t, targetKernelPath, spec.Annotations[constants.AnnotationsVMKernelPath])
}

func TestSyncLatestKernelForImageWaitsForArtifactCleanup(t *testing.T) {
	plugin := newTestCubeboxPlugin(t)
	previous := pmem.CurrentPaths()
	pmem.InitPaths(plugin.artifactPaths)
	t.Cleanup(func() { pmem.InitPaths(previous) })
	artifactID := "artifact-cleanup"
	shared := bytes.Repeat([]byte("s"), 4096)
	old := bytes.Repeat([]byte("o"), 2048)
	writeTestFile(t, plugin.artifactPaths.SharedKernelPath, shared)
	target := plugin.getKernelFilePath(artifactID)
	writeTestFile(t, target, old)

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var destroyErr error
	go func() {
		destroyErr = rootfsimage.DestroyPmemArtifact(context.Background(), plugin.config.instanceType, artifactID, func(string) (bool, error) {
			close(locked)
			<-release
			return true, nil
		})
		close(done)
	}()
	t.Cleanup(func() { unblock(); <-done })
	<-locked

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, plugin.syncLatestKernelForImage(ctx, artifactID), context.DeadlineExceeded)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, old, got, "a blocked refresh must not change the kernel")

	unblock()
	<-done
	require.ErrorIs(t, destroyErr, rootfsimage.ErrArtifactInUse)
	require.NoError(t, plugin.syncLatestKernelForImage(context.Background(), artifactID))
	got, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, shared, got)
}

func TestCreateSandboxRestoreDoesNotRefreshArtifactKernel(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	artifactID := "artifact-2"
	rootfsPath, err := plugin.artifactPaths.RootfsFile(plugin.config.instanceType, artifactID, "ext4")
	require.NoError(t, err)
	writeTestFile(t, rootfsPath, bytes.Repeat([]byte("rootfs"), 1024))
	sharedKernelPath := plugin.artifactPaths.SharedKernelPath
	targetKernelPath := plugin.getKernelFilePath(artifactID)
	oldKernel := bytes.Repeat([]byte("o"), 2048)
	writeTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 4096))
	writeTestFile(t, targetKernelPath, oldKernel)
	writeKernelVersion(t, targetKernelPath, oldKernel)

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
	require.Equal(t, kernelVersionForContent(oldKernel), readKernelVersion(t, targetKernelPath))

	spec := applySpecOpts(t, ctx, specOpts)
	require.Equal(t, "/template/kernel.vm", spec.Annotations[constants.AnnotationsVMKernelPath])
}

func TestCreateSandboxNormalStartDoesNotRefreshArtifactKernel(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	artifactID := "artifact-3"
	rootfsPath, err := plugin.artifactPaths.RootfsFile(plugin.config.instanceType, artifactID, "ext4")
	require.NoError(t, err)
	writeTestFile(t, rootfsPath, bytes.Repeat([]byte("rootfs"), 1024))
	sharedKernelPath := plugin.artifactPaths.SharedKernelPath
	targetKernelPath := plugin.getKernelFilePath(artifactID)
	oldKernel := bytes.Repeat([]byte("o"), 2048)
	writeTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 4096))
	writeTestFile(t, targetKernelPath, oldKernel)
	writeKernelVersion(t, targetKernelPath, oldKernel)

	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
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
	require.Equal(t, oldKernel, gotKernel)
	require.Equal(t, kernelVersionForContent(oldKernel), readKernelVersion(t, targetKernelPath))

	spec := applySpecOpts(t, ctx, specOpts)
	require.Equal(t, targetKernelPath, spec.Annotations[constants.AnnotationsVMKernelPath])
}

func newTestCubeboxPlugin(t *testing.T) *cubeboxInstancePlugin {
	t.Helper()

	basePath := t.TempDir()
	paths, err := pmem.ResolvePaths(basePath, filepath.Join(t.TempDir(), "artifacts"), filepath.Join(t.TempDir(), "installed", "vmlinux"))
	require.NoError(t, err)
	return &cubeboxInstancePlugin{
		artifactPaths: paths,
		config: &cubeboxInstancePluginConfig{
			BasePath:         basePath,
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

func writeKernelVersion(t *testing.T, kernelPath string, kernel []byte) {
	t.Helper()
	writeTestFile(t, filepath.Join(filepath.Dir(kernelPath), "version"), []byte(kernelVersionForContent(kernel)+"\n"))
}

func readKernelVersion(t *testing.T, kernelPath string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(filepath.Dir(kernelPath), "version"))
	require.NoError(t, err)
	return strings.TrimSpace(string(got))
}

func kernelVersionForContent(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
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

func TestCreateSandboxWithoutTemplateKeepsInstalledKernel(t *testing.T) {
	t.Parallel()
	plugin := newTestCubeboxPlugin(t)
	flow := &workflow.CreateContext{ReqInfo: &cubebox.RunCubeSandboxRequest{InstanceType: "cubebox"}}
	opts, err := plugin.CreateSandbox(context.Background(), flow)
	require.NoError(t, err)
	spec := applySpecOpts(t, context.Background(), opts)
	require.Equal(t, plugin.artifactPaths.SharedKernelPath, spec.Annotations[constants.AnnotationsVMKernelPath])
}

// A restart cold-boots on its retained disk and never reaches the template or
// restore branches, so it has to re-apply the guest component pin itself. If it
// does not, the shim boots the live toolbox artifacts and stamps their versions
// into the next snapshot's metadata while the catalog keeps the sandbox pin.
func TestCreateSandboxRestartPinsGuestArtifacts(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	inventory := t.TempDir()

	pinned := map[templatetypes.CubeComponent]string{
		templatetypes.CubeComponentCubeKernel: filepath.Join(inventory, "cube-kernel-scf", "pin-k", "vmlinux"),
		templatetypes.CubeComponentCubeImage:  filepath.Join(inventory, "cube-image", "pin-i", "cube-guest-image-cpu.img"),
		templatetypes.CubeComponentCubeAgent:  filepath.Join(inventory, "cube-agent", "pin-a", "cube-agent.ext4"),
	}
	localTemplate := &templatetypes.LocalRunTemplate{
		Componts: map[string]templatetypes.LocalComponent{},
	}
	for component, componentPath := range pinned {
		localTemplate.Componts[string(component)] = templatetypes.LocalComponent{
			Component: templatetypes.MachineComponent{
				Name:    component,
				Version: "pin-" + string(component),
				Path:    componentPath,
			},
		}
	}

	flow := &workflow.CreateContext{
		ReqInfo:          &cubebox.RunCubeSandboxRequest{InstanceType: "cubebox"},
		IsRestart:        true,
		LocalRunTemplate: localTemplate,
	}
	opts, err := plugin.CreateSandbox(context.Background(), flow)
	require.NoError(t, err)
	spec := applySpecOpts(t, context.Background(), opts)

	require.Equal(t, pinned[templatetypes.CubeComponentCubeKernel], spec.Annotations[constants.AnnotationsVMKernelPath])
	require.Equal(t, pinned[templatetypes.CubeComponentCubeImage], spec.Annotations[constants.AnnotationsVMOSImagePath])
	require.Equal(t, pinned[templatetypes.CubeComponentCubeAgent], spec.Annotations[constants.AnnotationsVMAgentPath])
	require.NotEqual(t,
		filepath.Join(plugin.config.BasePath, "cube-agent", "cube-agent.ext4"),
		spec.Annotations[constants.AnnotationsVMAgentPath],
		"restart must not fall back to the live toolbox agent",
	)
}

// Without a pin the restart keeps the toolbox default, so behaviour for
// sandboxes created outside a template is unchanged.
func TestCreateSandboxRestartWithoutPinFallsBackToToolbox(t *testing.T) {
	t.Parallel()

	plugin := newTestCubeboxPlugin(t)
	flow := &workflow.CreateContext{
		ReqInfo:   &cubebox.RunCubeSandboxRequest{InstanceType: "cubebox"},
		IsRestart: true,
	}
	opts, err := plugin.CreateSandbox(context.Background(), flow)
	require.NoError(t, err)
	spec := applySpecOpts(t, context.Background(), opts)

	require.Equal(t,
		filepath.Join(plugin.config.BasePath, "cube-agent", "cube-agent.ext4"),
		spec.Annotations[constants.AnnotationsVMAgentPath],
	)
	require.Equal(t,
		filepath.Join(plugin.config.BasePath, "cube-image", "cube-guest-image-cpu.img"),
		spec.Annotations[constants.AnnotationsVMOSImagePath],
	)
	require.Equal(t, plugin.artifactPaths.SharedKernelPath, spec.Annotations[constants.AnnotationsVMKernelPath])
}

func TestGenPmemOptValidatesArtifactBeforeAddingDevice(t *testing.T) {
	plugin := newTestCubeboxPlugin(t)
	path, err := plugin.artifactPaths.RootfsFile(plugin.config.instanceType, "rfs-device", "ext4")
	require.NoError(t, err)
	opt := plugin.genPmemOpt(context.Background(), "rfs-device", "ext4")
	spec := &specs.Spec{}
	require.Error(t, opt(context.Background(), nil, nil, spec))
	require.Empty(t, spec.Annotations)
	writeTestFile(t, path, []byte("rootfs"))
	require.NoError(t, opt(context.Background(), nil, nil, spec))
	var devices []pmem.CubePmem
	require.NoError(t, json.Unmarshal([]byte(spec.Annotations[constants.AnnotationPmem]), &devices))
	require.Len(t, devices, 1)
	require.Equal(t, "ext4", devices[0].FsType)
	require.Equal(t, int64(6), devices[0].Size)
	require.NoError(t, os.Truncate(path, 0))
	require.Error(t, opt(context.Background(), nil, nil, &specs.Spec{}))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	require.Error(t, opt(context.Background(), nil, nil, &specs.Spec{}))
}
