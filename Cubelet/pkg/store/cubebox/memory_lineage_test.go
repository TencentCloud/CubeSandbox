// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"
)

func TestBindLaunchAncestorOnceDoesNotOverwrite(t *testing.T) {
	cb := &CubeBox{}
	cb.BindLaunchAncestorOnce("snap-customer")
	cb.BindLaunchAncestorOnce("tpl-later")
	if got := cb.LaunchAncestorID(); got != "snap-customer" {
		t.Fatalf("ancestor = %q, want snap-customer", got)
	}
}

func TestClearRuntimeLineageKeepsAncestor(t *testing.T) {
	cb := &CubeBox{}
	cb.BindRuntimeBase("snap-runtime")
	cb.BindRestoreBase("snap-restore")
	cb.BindLaunchAncestorOnce("tpl-T")
	cb.ClearRuntimeLineage()
	if cb.RuntimeBaseID() != "" || cb.RestoreBaseID() != "" {
		t.Fatalf("runtime bases = %q %q, want empty", cb.RuntimeBaseID(), cb.RestoreBaseID())
	}
	if got := cb.LaunchAncestorID(); got != "tpl-T" {
		t.Fatalf("ancestor = %q, want tpl-T", got)
	}
	if cb.MemoryLineage == nil {
		t.Fatal("cleared lineage must stay non-nil so it remains authoritative")
	}
}

// An empty lineage and a nil lineage both mean "no base": annotations must
// never be consulted, so a restarted sandbox cannot inherit pre-restart memory.
func TestLineageIgnoresCreateAnnotations(t *testing.T) {
	for _, lineage := range []*MemoryLineage{nil, {}} {
		cb := &CubeBox{MemoryLineage: lineage}
		cb.AddAnnotations(map[string]string{
			"cube.master.runtime.snapshot.id":     "snap-old",
			"cube.master.appsnapshot.template.id": "tpl-T",
		})
		if got := cb.RuntimeBaseID(); got != "" {
			t.Fatalf("runtime = %q, want empty", got)
		}
		if got := cb.RestoreBaseID(); got != "" {
			t.Fatalf("restore = %q, want empty", got)
		}
		if got := cb.LaunchAncestorID(); got != "" {
			t.Fatalf("ancestor = %q, want empty", got)
		}
	}
}

func TestMemoryLineageDeepCopyIsDetached(t *testing.T) {
	cb := &CubeBox{}
	cb.BindRuntimeBase("snap-1")
	copied := cb.DeepCopy()
	copied.BindRuntimeBase("snap-2")
	if got := cb.RuntimeBaseID(); got != "snap-1" {
		t.Fatalf("original runtime base = %q, want snap-1", got)
	}
}

func TestNilAndEmptyLineageResolveEmpty(t *testing.T) {
	var nilBox *CubeBox
	if nilBox.RuntimeBaseID() != "" || nilBox.RestoreBaseID() != "" || nilBox.LaunchAncestorID() != "" {
		t.Fatal("nil cubebox must resolve no bases")
	}
	empty := &CubeBox{MemoryLineage: &MemoryLineage{}}
	if empty.RuntimeBaseID() != "" || empty.RestoreBaseID() != "" || empty.LaunchAncestorID() != "" {
		t.Fatal("empty lineage must resolve no bases")
	}
}
