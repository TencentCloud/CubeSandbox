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

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
)

func TestEnsureKernelFileRefreshesWhenSharedKernelChanges(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	sharedKernelPath := pmem.GetSharedKernelFilePath()
	if err := os.MkdirAll(filepath.Dir(sharedKernelPath), 0o755); err != nil {
		t.Fatalf("MkdirAll error=%v", err)
	}
	kernelV1 := bytes.Repeat([]byte("a"), 2048)
	if err := os.WriteFile(sharedKernelPath, kernelV1, 0o644); err != nil {
		t.Fatalf("WriteFile shared kernel error=%v", err)
	}

	if err := ensureKernelFile(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("ensureKernelFile error=%v", err)
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
	if err := os.WriteFile(sharedKernelPath, kernelV2, 0o644); err != nil {
		t.Fatalf("WriteFile updated shared kernel error=%v", err)
	}
	if err := ensureKernelFile(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("ensureKernelFile second call error=%v", err)
	}

	got, err = os.ReadFile(targetKernelPath)
	if err != nil {
		t.Fatalf("ReadFile target kernel after second call error=%v", err)
	}
	if !bytes.Equal(got, kernelV2) {
		t.Fatal("target kernel should refresh to latest shared content")
	}
}

func TestEnsureKernelFileRefreshesExistingTargetKernel(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	sharedKernelPath := pmem.GetSharedKernelFilePath()
	if err := os.MkdirAll(filepath.Dir(sharedKernelPath), 0o755); err != nil {
		t.Fatalf("MkdirAll error=%v", err)
	}
	sharedKernel := bytes.Repeat([]byte("s"), 3072)
	if err := os.WriteFile(sharedKernelPath, sharedKernel, 0o644); err != nil {
		t.Fatalf("WriteFile shared kernel error=%v", err)
	}

	targetKernelPath := pmem.GetRawKernelFilePath("cubebox", "artifact-2")
	if err := os.MkdirAll(filepath.Dir(targetKernelPath), 0o755); err != nil {
		t.Fatalf("MkdirAll target dir error=%v", err)
	}
	oldKernel := bytes.Repeat([]byte("o"), 3072)
	if err := os.WriteFile(targetKernelPath, oldKernel, 0o644); err != nil {
		t.Fatalf("WriteFile target kernel error=%v", err)
	}

	if err := ensureKernelFile(context.Background(), "cubebox", "artifact-2"); err != nil {
		t.Fatalf("ensureKernelFile error=%v", err)
	}

	got, err := os.ReadFile(targetKernelPath)
	if err != nil {
		t.Fatalf("ReadFile target kernel error=%v", err)
	}
	if !bytes.Equal(got, sharedKernel) {
		t.Fatal("target kernel should refresh from shared kernel")
	}
}

func TestEnsureKernelFileRequiresSharedKernel(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	err := ensureKernelFile(context.Background(), "cubebox", "artifact-2")
	if err == nil {
		t.Fatal("ensureKernelFile error=nil, want non-nil")
	}
}

func TestEnsureImageVersionFileCopiesSharedVersionOnce(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	sharedVersionPath := pmem.GetSharedImageVersionFilePath()
	if err := os.MkdirAll(filepath.Dir(sharedVersionPath), 0o755); err != nil {
		t.Fatalf("MkdirAll error=%v", err)
	}
	versionV1 := []byte("2.2.0-20251010\n")
	if err := os.WriteFile(sharedVersionPath, versionV1, 0o644); err != nil {
		t.Fatalf("WriteFile shared version error=%v", err)
	}

	if err := ensureImageVersionFile(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("ensureImageVersionFile error=%v", err)
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
	if err := os.WriteFile(sharedVersionPath, versionV2, 0o644); err != nil {
		t.Fatalf("WriteFile updated shared version error=%v", err)
	}
	if err := ensureImageVersionFile(context.Background(), "cubebox", "artifact-1"); err != nil {
		t.Fatalf("ensureImageVersionFile second call error=%v", err)
	}

	got, err = os.ReadFile(targetVersionPath)
	if err != nil {
		t.Fatalf("ReadFile target version after second call error=%v", err)
	}
	if !bytes.Equal(got, versionV1) {
		t.Fatal("target version should keep first copied content")
	}
}

func TestEnsureImageVersionFileRequiresSharedVersion(t *testing.T) {
	baseDir := t.TempDir()
	pmem.Init(baseDir)

	err := ensureImageVersionFile(context.Background(), "cubebox", "artifact-2")
	if err == nil {
		t.Fatal("ensureImageVersionFile error=nil, want non-nil")
	}
}
