// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import "testing"

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
}

func TestIsRetainedRootfs(t *testing.T) {
	if isRetainedRootfs(nil) {
		t.Fatal("nil volume")
	}
	if isRetainedRootfs(&BackendFileInfo{VolumeName: "sb-1-rootfs-gen0"}) {
		t.Fatal("missing path")
	}
	if !isRetainedRootfs(&BackendFileInfo{VolumeName: "sb-1-rootfs-gen0", FilePath: "/dev/mapper/sb-1-rootfs-gen0"}) {
		t.Fatal("rootfs volume")
	}
	if !isRetainedRootfs(&BackendFileInfo{FilePath: "/data/volumes/sb-1-rootfs-gen0"}) {
		t.Fatal("rootfs path")
	}
	if isRetainedRootfs(&BackendFileInfo{VolumeName: "memory", FilePath: "/dev/mapper/tpl-memory"}) {
		t.Fatal("memory volume")
	}
}
