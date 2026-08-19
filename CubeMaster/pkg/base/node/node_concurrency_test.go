// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package node

import (
	"sync"
	"testing"
)

func TestLabelsCacheInitIsRaceFree(t *testing.T) {
	n := &Node{Zone: "z1", ClusterLabel: "c1", CPUType: "amd64", QuotaMem: 1024, QuotaCpu: 2000}

	const readers = 32
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := n.Labels(); got == nil {
					t.Error("Labels() returned nil")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestLabelsCacheInvalidateIsRaceFree(t *testing.T) {
	n := &Node{Zone: "z1", ClusterLabel: "c1"}

	var wg sync.WaitGroup
	wg.Add(16)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				n.Labels()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				n.InvalidateLabelsCache()
			}
		}()
	}
	wg.Wait()
}

func TestCloneCarriesSchedulingDisabled(t *testing.T) {
	n := &Node{InsID: "n1", NodeLabels: map[string]string{"a": "b"}}
	n.SetSchedulingDisabled(true)

	c := n.Clone()
	if !c.SchedulingDisabled() {
		t.Fatal("clone lost the cordon flag")
	}
	if c.SchedulingAllowed() {
		t.Fatal("clone reports scheduling allowed while cordoned")
	}

	c.SetSchedulingDisabled(false)
	if !n.SchedulingDisabled() {
		t.Fatal("mutating the clone changed the original cordon flag")
	}
	if c.SchedulingDisabled() {
		t.Fatal("clone did not accept the new cordon value")
	}
}

func BenchmarkLabels(b *testing.B) {
	n := &Node{Zone: "z1", ClusterLabel: "c1", CPUType: "amd64", QuotaMem: 1024, QuotaCpu: 2000}
	n.Labels()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n.Labels()
	}
}

func BenchmarkLabelsParallel(b *testing.B) {
	n := &Node{Zone: "z1", ClusterLabel: "c1", CPUType: "amd64", QuotaMem: 1024, QuotaCpu: 2000}
	n.Labels()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n.Labels()
		}
	})
}
