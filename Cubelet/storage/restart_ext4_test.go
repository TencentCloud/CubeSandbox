// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"os"
	"path/filepath"
	"testing"

	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestExt4RepairAccepted(t *testing.T) {
	for _, code := range []int{0, 1, 2} {
		if !ext4RepairAccepted(code) {
			t.Fatalf("exit %d should be a repaired disk", code)
		}
	}
	for _, code := range []int{-1, 4, 8, 16} {
		if ext4RepairAccepted(code) {
			t.Fatalf("exit %d is not a repaired disk", code)
		}
	}
	if !ext4RepairUnrecoverable(4) || !ext4RepairUnrecoverable(8) {
		t.Fatal("exit 4 and 8 stop the restart")
	}
	if ext4RepairUnrecoverable(16) {
		t.Fatal("exit 16 stays retryable")
	}
}

func TestRetainedRootfsTargets(t *testing.T) {
	cow := &BackendFileInfo{VolumeName: "sb-1-rootfs-gen0", FilePath: "/dev/mapper/sb-1-rootfs-gen0"}
	other := &BackendFileInfo{VolumeName: "sb-2-rootfs-gen0", FilePath: "/dev/mapper/sb-2-rootfs-gen0"}
	plain := &BackendFileInfo{Type: "ext4", Medium: cubebox.StorageMedium_StorageMediumDefault, FilePath: "/dev/disk/by-id/pool"}
	pathOnly := &BackendFileInfo{FilePath: "/data/volumes/sb-1-rootfs-gen0"}
	memory := &BackendFileInfo{VolumeName: "memory", FilePath: "/dev/mapper/tpl-memory"}

	got, err := retainedRootfsTargets("1", map[string]*BackendFileInfo{
		"cow": cow, "other": other, "plain": plain, "path": pathOnly, "mem": memory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != cow {
		t.Fatalf("cubecow candidate = %+v", got)
	}

	got, err = retainedRootfsTargets("9", map[string]*BackendFileInfo{"plain": plain, "path": pathOnly, "mem": memory})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != plain {
		t.Fatalf("default ext4 = %+v", got)
	}

	_, err = retainedRootfsTargets("1", map[string]*BackendFileInfo{
		"a": cow,
		"b": {VolumeName: "sb-1-rootfs-gen1", FilePath: "/dev/mapper/other"},
	})
	if err == nil {
		t.Fatal("multiple cubecow candidates")
	}
}

func TestHasExt4Superblock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	buf := make([]byte, 0x438+2)
	buf[0x438] = 0x53
	buf[0x439] = 0xEF
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := hasExt4Superblock(path)
	if err != nil || !ok {
		t.Fatalf("magic ok=%v err=%v", ok, err)
	}
	buf[0x438] = 0
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = hasExt4Superblock(path)
	if err != nil || ok {
		t.Fatalf("non-ext4 ok=%v err=%v", ok, err)
	}
}
