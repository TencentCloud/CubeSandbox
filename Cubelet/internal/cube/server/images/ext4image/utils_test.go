// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ext4image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
)

func writeTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s error=%v", path, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile %s error=%v", path, err)
	}
}

func writeSharedKernelFile(t *testing.T, content []byte) {
	t.Helper()
	writeTestFile(t, pmem.GetSharedKernelFilePath(), content)
}

func writeSharedVersionFile(t *testing.T, version string) {
	t.Helper()
	writeTestFile(t, pmem.GetSharedImageVersionFilePath(), []byte(version))
}

func writeRawImageFile(t *testing.T, instanceType, imageRef string, content []byte) {
	t.Helper()
	writeTestFile(t, pmem.GetRawImageFilePath(instanceType, imageRef), content)
}

func writeRawKernelFile(t *testing.T, instanceType, imageRef string, content []byte) {
	t.Helper()
	writeTestFile(t, pmem.GetRawKernelFilePath(instanceType, imageRef), content)
}

func writeRawVersionFile(t *testing.T, instanceType, imageRef, version string) {
	t.Helper()
	writeTestFile(t, pmem.GetRawImageVersionFilePath(instanceType, imageRef), []byte(version))
}

func TestRefreshArtifactRuntimeFilesRefreshesKernelWhenSharedKernelChanges(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	kernelV1 := bytes.Repeat([]byte("a"), 2048)
	writeSharedKernelFile(t, kernelV1)
	writeSharedVersionFile(t, "2.2.0-20251010\n")

	if err := RefreshArtifactRuntimeFiles(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("RefreshArtifactRuntimeFiles error=%v", err)
	}

	targetKernelPath := pmem.GetRawKernelFilePath("cubebox", "artifact-1")
	got, err := os.ReadFile(targetKernelPath)
	if err != nil {
		t.Fatalf("ReadFile target kernel error=%v", err)
	}
	if !bytes.Equal(got, kernelV1) {
		t.Fatal("target kernel content mismatch after first copy")
	}

	kernelV2 := bytes.Repeat([]byte("b"), 4096)
	writeSharedKernelFile(t, kernelV2)
	if err := RefreshArtifactRuntimeFiles(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("RefreshArtifactRuntimeFiles second call error=%v", err)
	}

	got, err = os.ReadFile(targetKernelPath)
	if err != nil {
		t.Fatalf("ReadFile target kernel after second call error=%v", err)
	}
	if !bytes.Equal(got, kernelV2) {
		t.Fatal("target kernel should refresh to latest shared content")
	}
}

func TestEnsurePmemFilePreservesExistingRuntimeFiles(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	writeSharedKernelFile(t, bytes.Repeat([]byte("s"), 3072))
	writeSharedVersionFile(t, "2.2.0-20251010\n")
	writeRawImageFile(t, "cubebox", "artifact-2", bytes.Repeat([]byte("e"), 2048))
	targetKernelPath := pmem.GetRawKernelFilePath("cubebox", "artifact-2")
	oldKernel := bytes.Repeat([]byte("o"), 3072)
	writeRawKernelFile(t, "cubebox", "artifact-2", oldKernel)
	ctx := constants.WithImageSpec(context.Background(), &cubeimages.ImageSpec{
		Annotations: map[string]string{
			constants.MasterAnnotationRootfsArtifactURL:    "http://unused.example/artifact.ext4",
			constants.MasterAnnotationRootfsArtifactSHA256: "deadbeef",
		},
	})
	targetVersionPath := pmem.GetRawImageVersionFilePath("cubebox", "artifact-2")
	writeRawVersionFile(t, "cubebox", "artifact-2", "2.2.0-20251010\n")

	if err := EnsurePmemFile(ctx, "cubebox", "artifact-2"); err != nil {
		t.Fatalf("EnsurePmemFile error=%v", err)
	}

	got, err := os.ReadFile(targetKernelPath)
	if err != nil {
		t.Fatalf("ReadFile target kernel error=%v", err)
	}
	if !bytes.Equal(got, oldKernel) {
		t.Fatal("target kernel should stay unchanged when file already exists")
	}
	gotVersion, err := os.ReadFile(targetVersionPath)
	if err != nil {
		t.Fatalf("ReadFile target version error=%v", err)
	}
	if !bytes.Equal(gotVersion, []byte("2.2.0-20251010\n")) {
		t.Fatal("target version should stay unchanged when file already exists")
	}
}

func TestEnsurePmemFileRejectsMissingKernelFile(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	sharedKernel := bytes.Repeat([]byte("s"), 3072)
	writeSharedKernelFile(t, sharedKernel)
	writeSharedVersionFile(t, "2.2.0-20251010\n")
	writeRawImageFile(t, "cubebox", "artifact-3", bytes.Repeat([]byte("e"), 2048))

	err := EnsurePmemFile(context.Background(), "cubebox", "artifact-3")
	if err == nil {
		t.Fatal("EnsurePmemFile error=nil, want non-nil")
	}
}

func TestEnsurePmemRootfsDoesNotRequireKernelFile(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	writeRawImageFile(t, "cubebox", "artifact-4", bytes.Repeat([]byte("e"), 2048))

	if err := EnsurePmemRootfs(context.Background(), "cubebox", "artifact-4"); err != nil {
		t.Fatalf("EnsurePmemRootfs error=%v", err)
	}
}

func TestEnsurePmemFileRejectsMissingImageVersionFile(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	writeSharedKernelFile(t, bytes.Repeat([]byte("s"), 3072))
	writeSharedVersionFile(t, "2.2.0-20251010\n")
	writeRawImageFile(t, "cubebox", "artifact-5", bytes.Repeat([]byte("e"), 2048))
	writeRawKernelFile(t, "cubebox", "artifact-5", bytes.Repeat([]byte("k"), 3072))

	err := EnsurePmemFile(context.Background(), "cubebox", "artifact-5")
	if err == nil {
		t.Fatal("EnsurePmemFile error=nil, want non-nil")
	}
}

func TestEnsureKernelFilePresentRequiresSharedKernel(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	err := ensureKernelFilePresent(context.Background(), "cubebox", "artifact-2")
	if err == nil {
		t.Fatal("ensureKernelFilePresent error=nil, want non-nil")
	}
}

func TestRefreshImageVersionFileRefreshesSharedVersion(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	versionV1 := []byte("2.2.0-20251010\n")
	writeSharedVersionFile(t, string(versionV1))

	if err := refreshImageVersionFile(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("refreshImageVersionFile error=%v", err)
	}

	targetVersionPath := pmem.GetRawImageVersionFilePath("cubebox", "artifact-1")
	got, err := os.ReadFile(targetVersionPath)
	if err != nil {
		t.Fatalf("ReadFile target version error=%v", err)
	}
	if !bytes.Equal(got, versionV1) {
		t.Fatal("target version content mismatch after first copy")
	}

	versionV2 := []byte("2.2.0-20251011\n")
	writeSharedVersionFile(t, string(versionV2))
	if err := refreshImageVersionFile(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("refreshImageVersionFile second call error=%v", err)
	}

	got, err = os.ReadFile(targetVersionPath)
	if err != nil {
		t.Fatalf("ReadFile target version after second call error=%v", err)
	}
	if !bytes.Equal(got, versionV2) {
		t.Fatal("target version should refresh from shared version")
	}
}

func TestRefreshImageVersionFileRequiresSharedVersion(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	err := refreshImageVersionFile(context.Background(), "cubebox", "artifact-2")
	if err == nil {
		t.Fatal("refreshImageVersionFile error=nil, want non-nil")
	}
}

func TestValidateImageVersionFilePresentRequiresArtifactVersion(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	err := validateImageVersionFilePresent(context.Background(), "cubebox", "artifact-2")
	if err == nil {
		t.Fatal("validateImageVersionFilePresent error=nil, want non-nil")
	}

	writeRawVersionFile(t, "cubebox", "artifact-2", "2.2.0-20251010\n")
	if err := validateImageVersionFilePresent(context.Background(), "cubebox", "artifact-2"); err != nil {
		t.Fatalf("validateImageVersionFilePresent error=%v", err)
	}
}
