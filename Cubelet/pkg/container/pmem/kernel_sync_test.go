// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pmem

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshKernelFileVerifiesCopiedContent(t *testing.T) {
	baseDir := t.TempDir()
	sharedKernelPath := filepath.Join(baseDir, "shared", "vmlinux")
	targetKernelPath := filepath.Join(baseDir, "target", "artifact.vm")

	writeKernelTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 4096))
	writeKernelTestFile(t, targetKernelPath, bytes.Repeat([]byte("o"), 2048))

	err := RefreshKernelFile(context.Background(), sharedKernelPath, targetKernelPath)
	if err != nil {
		t.Fatalf("RefreshKernelFile error=%v", err)
	}

	got, err := os.ReadFile(targetKernelPath)
	if err != nil {
		t.Fatalf("ReadFile target kernel error=%v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("s"), 4096)) {
		t.Fatal("target kernel should match shared kernel after refresh")
	}
}

func TestRefreshKernelFileCleansTargetOnVerificationFailure(t *testing.T) {
	baseDir := t.TempDir()
	sharedKernelPath := filepath.Join(baseDir, "shared", "vmlinux")
	targetKernelPath := filepath.Join(baseDir, "target", "artifact.vm")

	writeKernelTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 4096))
	writeKernelTestFile(t, targetKernelPath, bytes.Repeat([]byte("o"), 2048))

	originalCompare := compareKernelFiles
	compareKernelFiles = func(pathA, pathB string) (bool, error) {
		return false, nil
	}
	defer func() {
		compareKernelFiles = originalCompare
	}()

	err := RefreshKernelFile(context.Background(), sharedKernelPath, targetKernelPath)
	if err == nil {
		t.Fatal("RefreshKernelFile error=nil, want non-nil")
	}
	if _, statErr := os.Stat(targetKernelPath); !os.IsNotExist(statErr) {
		t.Fatalf("target kernel should be removed after verification failure, statErr=%v", statErr)
	}
}

func TestEnsureKernelFilePresentRejectsMissingTarget(t *testing.T) {
	baseDir := t.TempDir()
	sharedKernelPath := filepath.Join(baseDir, "shared", "vmlinux")
	targetKernelPath := filepath.Join(baseDir, "target", "artifact.vm")

	sharedKernel := bytes.Repeat([]byte("s"), 4096)
	writeKernelTestFile(t, sharedKernelPath, sharedKernel)

	err := EnsureKernelFilePresent(context.Background(), sharedKernelPath, targetKernelPath)
	if err == nil {
		t.Fatal("EnsureKernelFilePresent error=nil, want non-nil")
	}
}

func TestEnsureKernelFilePresentRejectsSmallTarget(t *testing.T) {
	baseDir := t.TempDir()
	sharedKernelPath := filepath.Join(baseDir, "shared", "vmlinux")
	targetKernelPath := filepath.Join(baseDir, "target", "artifact.vm")

	sharedKernel := bytes.Repeat([]byte("s"), 4096)
	writeKernelTestFile(t, sharedKernelPath, sharedKernel)
	writeKernelTestFile(t, targetKernelPath, bytes.Repeat([]byte("o"), 128))

	err := EnsureKernelFilePresent(context.Background(), sharedKernelPath, targetKernelPath)
	if err == nil {
		t.Fatal("EnsureKernelFilePresent error=nil, want non-nil")
	}
}

func TestEnsureKernelFilePresentRejectsDirectoryTarget(t *testing.T) {
	baseDir := t.TempDir()
	sharedKernelPath := filepath.Join(baseDir, "shared", "vmlinux")
	targetKernelPath := filepath.Join(baseDir, "target", "artifact.vm")

	sharedKernel := bytes.Repeat([]byte("s"), 4096)
	writeKernelTestFile(t, sharedKernelPath, sharedKernel)
	if err := os.MkdirAll(targetKernelPath, 0o755); err != nil {
		t.Fatalf("MkdirAll target kernel dir error=%v", err)
	}

	err := EnsureKernelFilePresent(context.Background(), sharedKernelPath, targetKernelPath)
	if err == nil {
		t.Fatal("EnsureKernelFilePresent error=nil, want non-nil")
	}
}

func TestEnsureKernelFilePresentRequiresValidSharedKernel(t *testing.T) {
	baseDir := t.TempDir()
	sharedKernelPath := filepath.Join(baseDir, "shared", "vmlinux")
	targetKernelPath := filepath.Join(baseDir, "target", "artifact.vm")

	err := EnsureKernelFilePresent(context.Background(), sharedKernelPath, targetKernelPath)
	if err == nil {
		t.Fatal("EnsureKernelFilePresent error=nil for missing shared kernel")
	}

	writeKernelTestFile(t, sharedKernelPath, bytes.Repeat([]byte("s"), 128))
	err = EnsureKernelFilePresent(context.Background(), sharedKernelPath, targetKernelPath)
	if err == nil {
		t.Fatal("EnsureKernelFilePresent error=nil for invalid shared kernel")
	}
}

func writeKernelTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll error=%v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile error=%v", err)
	}
}
