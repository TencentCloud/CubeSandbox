// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package resourcesource

import "testing"

type fakeCollector struct {
	alloc *AllocatedResources
	disk  *DiskUsage
}

func (f *fakeCollector) CollectAllocated() *AllocatedResources { return f.alloc }
func (f *fakeCollector) CollectDiskUsage() *DiskUsage          { return f.disk }

func TestSetAndGet(t *testing.T) {
	t.Cleanup(func() { Set(nil) })
	if Get() != nil {
		t.Fatal("expected nil before Set")
	}
	want := &fakeCollector{alloc: &AllocatedResources{MilliCPU: 12}}
	Set(want)
	got := Get()
	if got == nil {
		t.Fatal("expected collector after Set")
	}
	a := got.CollectAllocated()
	if a == nil || a.MilliCPU != 12 {
		t.Fatalf("collector returned wrong allocated: %+v", a)
	}
	Set(nil)
	if Get() != nil {
		t.Fatal("expected nil after Set(nil)")
	}
}
