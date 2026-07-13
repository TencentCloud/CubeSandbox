// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestSnapshotRuntimeRefFromAnnotationMapParsesLogicalFields(t *testing.T) {
	ref := snapshotRuntimeRefFromAnnotationMap("sb-1", "node-a", "10.0.0.1", map[string]string{
		constants.CubeAnnotationRuntimeSnapshotID:         "snap-1",
		constants.CubeAnnotationRuntimeSnapshotAttachedAt: "2026-05-10T09:00:00Z",
	})

	if ref.SnapshotID != "snap-1" {
		t.Fatalf("SnapshotID=%q, want snap-1", ref.SnapshotID)
	}
	// v5: master no longer carries physical memory_vol on the annotation
	// map; the ref's MemoryVol comes solely from rollback RPC responses.
	if ref.MemoryVol != "" {
		t.Fatalf("MemoryVol=%q, want empty (catalog-owned)", ref.MemoryVol)
	}
	if ref.MemoryDev != "" {
		t.Fatalf("MemoryDev=%q, want empty", ref.MemoryDev)
	}
	if ref.AttachedAt.IsZero() {
		t.Fatal("AttachedAt should be parsed")
	}
}

func TestSnapshotRuntimeActiveModelToInfoUsesActiveStatus(t *testing.T) {
	seen := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	info := snapshotRuntimeActiveModelToInfo(models.SnapshotRuntimeActive{
		SnapshotID:  " snap-1 ",
		SandboxID:   " sb-1 ",
		NodeID:      " node-a ",
		NodeIP:      " 10.0.0.1 ",
		BindingType: " memory_backing ",
		MemoryVol:   " mem-1 ",
		RootfsVol:   " rootfs-1 ",
		SandboxGen:  7,
		AttachedAt:  seen,
		LastSeenAt:  &seen,
		LastError:   " ",
	})

	if info.Status != SnapshotRuntimeRefStatusActive {
		t.Fatalf("Status=%q, want ACTIVE", info.Status)
	}
	if info.SandboxID != "sb-1" || info.SnapshotID != "snap-1" {
		t.Fatalf("unexpected binding identity: %#v", info)
	}
	if info.BindingType != SnapshotRuntimeBindingMemoryBacking {
		t.Fatalf("BindingType=%q, want %q", info.BindingType, SnapshotRuntimeBindingMemoryBacking)
	}
	if info.SandboxGen != 7 {
		t.Fatalf("SandboxGen=%d, want 7", info.SandboxGen)
	}
}

func TestSnapshotRuntimeBindingKeyDefaultsBindingType(t *testing.T) {
	got := snapshotRuntimeBindingKey(" sb-1 ", "")
	want := "sb-1\x00" + SnapshotRuntimeBindingMemoryBacking
	if got != want {
		t.Fatalf("binding key=%q, want %q", got, want)
	}
}
