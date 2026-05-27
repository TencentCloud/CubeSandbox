// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
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
