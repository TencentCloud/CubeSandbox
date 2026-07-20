// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pmem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitWithPathsSeparatesToolboxAndOsImage(t *testing.T) {
	toolbox := t.TempDir()
	osParent := t.TempDir()
	if err := InitWithPaths(toolbox, osParent); err != nil {
		t.Fatalf("InitWithPaths: %v", err)
	}

	shared := GetSharedKernelFilePath()
	wantShared := filepath.Join(toolbox, "cube-kernel-scf", "vmlinux")
	if shared != wantShared {
		t.Fatalf("GetSharedKernelFilePath=%q, want %q", shared, wantShared)
	}

	base := GetPmemBasePath("cubebox")
	wantBase := filepath.Join(osParent, "cubebox_os_image")
	if base != wantBase {
		t.Fatalf("GetPmemBasePath=%q, want %q", base, wantBase)
	}

	img := GetRawImageFilePath("cubebox", "img-1")
	wantImg := filepath.Join(wantBase, "img-1", "img-1.ext4")
	if img != wantImg {
		t.Fatalf("GetRawImageFilePath=%q, want %q", img, wantImg)
	}
}

func TestInitKeepsLegacySingleRoot(t *testing.T) {
	root := t.TempDir()
	Init(root)

	if got, want := GetSharedKernelFilePath(), filepath.Join(root, "cube-kernel-scf", "vmlinux"); got != want {
		t.Fatalf("GetSharedKernelFilePath=%q, want %q", got, want)
	}
	if got, want := GetPmemBasePath("cubebox"), filepath.Join(root, "cubebox_os_image"); got != want {
		t.Fatalf("GetPmemBasePath=%q, want %q", got, want)
	}
}

func TestDefaultOsImagePaths(t *testing.T) {
	if DefaultOsImageParentDir != "/data" {
		t.Fatalf("DefaultOsImageParentDir=%q, want /data", DefaultOsImageParentDir)
	}
	want := "/data/cubebox_os_image"
	if got := DefaultCubeboxOsImageDir(); got != want {
		t.Fatalf("DefaultCubeboxOsImageDir=%q, want %q", got, want)
	}
}

func TestResolveOsImageParentDir(t *testing.T) {
	if got := ResolveOsImageParentDir(""); got != DefaultOsImageParentDir {
		t.Fatalf("empty ResolveOsImageParentDir=%q, want %q", got, DefaultOsImageParentDir)
	}
	if got := ResolveOsImageParentDir("/custom"); got != "/custom" {
		t.Fatalf("custom ResolveOsImageParentDir=%q, want /custom", got)
	}
}

func TestInitWithPathsReturnsMkdirError(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InitWithPaths(root, blocker); err == nil {
		t.Fatal("InitWithPaths: want error when os-image parent is a file")
	}
}
