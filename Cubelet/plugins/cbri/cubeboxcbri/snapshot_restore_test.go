// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubeboxcbri

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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

func TestSnapshotRestoreMemoryVolURLFromStorageInfo(t *testing.T) {
	t.Parallel()

	flowOpts := &workflow.CreateContext{
		StorageInfo: &storage.StorageInfo{
			RestoreMemoryVolURL: "file:///dev/mapper/prefetched-memory",
		},
	}

	got := snapshotRestoreMemoryVolURLFromStorageInfo(flowOpts)
	if got != "file:///dev/mapper/prefetched-memory" {
		t.Fatalf("snapshotRestoreMemoryVolURLFromStorageInfo=%q", got)
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

func TestResolveSnapshotPathsAcceptsMetadataHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	meta := filepath.Join(home, "metadata")
	if err := os.MkdirAll(filepath.Join(meta, "snapshot"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(meta, "snapshot", "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(meta, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	p := &cubeboxInstancePlugin{}
	req := &cubebox.RunCubeSandboxRequest{
		Containers: []*cubebox.ContainerConfig{{
			Resources: &cubebox.Resource{Cpu: "2000m", Mem: "2000Mi"},
		}},
	}
	paths, err := p.resolveSnapshotPaths("snap-1", meta, req)
	if err != nil {
		t.Fatalf("resolveSnapshotPaths: %v", err)
	}
	if paths.Spec != meta {
		t.Fatalf("Spec=%q want metadata home %q", paths.Spec, meta)
	}
	if paths.Base != home {
		t.Fatalf("Base=%q want snapshot home %q", paths.Base, home)
	}
	if paths.ResDir != "2C2000M" {
		t.Fatalf("ResDir=%q", paths.ResDir)
	}
}

func TestSnapshotRestoreRawPathPauseResumeUsesCatalogMetaDir(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	meta := filepath.Join(home, "metadata")
	if err := os.MkdirAll(filepath.Join(meta, "snapshot"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entry := &storage.SnapshotCatalogEntry{
		SnapshotID:   "snap-pause-1",
		SnapshotPath: home,
		MetaDir:      meta,
		Kind:         storage.CatalogKindPauseSnapshot,
		Backend:      "xfs",
	}
	if err := storage.WriteSnapshotCatalogFor("xfs", entry); err != nil {
		t.Fatalf("WriteSnapshotCatalogFor: %v", err)
	}

	tplPath := filepath.Join(t.TempDir(), "tpl-990ea6", "2C2000M")
	flowOpts := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			InstanceType: cubebox.InstanceType_cubebox.String(),
			Annotations: map[string]string{
				constants.MasterAnnotationPauseSnapshotID:       "snap-pause-1",
				constants.MasterAnnotationRuntimeSnapshotID:     "snap-pause-1",
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-990ea6",
				constants.MasterAnnotationStorageBackend:        "xfs",
			},
		},
		LocalRunTemplate: &templatetypes.LocalRunTemplate{
			Snapshot: templatetypes.LocalSnapshot{
				Snapshot: templatetypes.Snapshot{Path: tplPath},
			},
		},
	}
	got := snapshotRestoreRawPath(context.Background(), flowOpts)
	if got != meta {
		t.Fatalf("snapshotRestoreRawPath=%q want pause meta %q (not template %q)", got, meta, tplPath)
	}

	fromTpl := &workflow.CreateContext{
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			Annotations: map[string]string{
				constants.MasterAnnotationAppSnapshotTemplateID: "tpl-990ea6",
			},
		},
		LocalRunTemplate: flowOpts.LocalRunTemplate,
	}
	if got := snapshotRestoreRawPath(context.Background(), fromTpl); got != tplPath {
		t.Fatalf("non-pause snapshotRestoreRawPath=%q want template path %q", got, tplPath)
	}
}

func TestRestorePathsFromMetadataMountUsesReturnedDir(t *testing.T) {
	t.Parallel()
	paths := &snapshotPaths{
		Base:   "/data/cubelet/storage/s3/snapshots/tpl-parent",
		Spec:   "/data/cubelet/storage/s3/snapshots/tpl-parent/2C2048M",
		ResDir: "2C2048M",
	}
	mounted := "/data/cubelet/storage/s3/snapshots/sb-child/metadata"
	base, spec := restorePathsFromMetadataMount(mounted, paths)
	if base != "/data/cubelet/storage/s3/snapshots/sb-child" {
		t.Fatalf("base=%q", base)
	}
	if spec != "/data/cubelet/storage/s3/snapshots/sb-child/2C2048M" {
		t.Fatalf("spec=%q", spec)
	}

	fallbackBase, fallbackSpec := restorePathsFromMetadataMount("", paths)
	if fallbackBase != paths.Base || fallbackSpec != paths.Spec {
		t.Fatalf("empty mount must keep template paths, got %q %q", fallbackBase, fallbackSpec)
	}
}
