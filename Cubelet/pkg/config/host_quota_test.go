// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import "testing"

// These functions are the single source of truth for both node registration
// and resume admission, so pin the fallback order: explicit limit, then
// caller fallbacks (allocatable/capacity), then the host-derived default.
func TestCPUQuotaMilliFallbackOrder(t *testing.T) {
	cpuCount := func() int { return 2 }
	if got := CPUQuotaMilli(HostConfigQuota{Cpu: 3000}, cpuCount, 9000); got != 3000 {
		t.Fatalf("configured mcpu_limit must win, got %d", got)
	}
	if got := CPUQuotaMilli(HostConfigQuota{}, cpuCount, 0, 9000); got != 9000 {
		t.Fatalf("first positive fallback must win, got %d", got)
	}
	if got := CPUQuotaMilli(HostConfigQuota{}, cpuCount); got != 4000 {
		t.Fatalf("host default should be 2 CPUs x 1000m x 2, got %d", got)
	}
	if got := CPUQuotaMilli(HostConfigQuota{}, func() int { return 0 }); got != 0 {
		t.Fatalf("an unknown host CPU count leaves the dimension unbounded, got %d", got)
	}
}

func TestMemQuotaMBFallbackOrder(t *testing.T) {
	hostMem := func() (int64, error) { return 8192, nil }
	if got := MemQuotaMB(HostConfigQuota{Mem: "6Gi"}, hostMem, 9000); got != 6144 {
		t.Fatalf("configured mem_limit must win, got %d", got)
	}
	if got := MemQuotaMB(HostConfigQuota{}, hostMem, 0, 9000); got != 9000 {
		t.Fatalf("first positive fallback must win, got %d", got)
	}
	if got := MemQuotaMB(HostConfigQuota{}, hostMem); got != 10240 {
		t.Fatalf("host default should be 8192MB x 5/4, got %d", got)
	}
	if got := MemQuotaMB(HostConfigQuota{}, func() (int64, error) { return 0, nil }); got != 0 {
		t.Fatalf("unreadable host memory leaves the dimension unbounded, got %d", got)
	}
}

func TestResolvedHostQuotaUsesProductionHostFallbacks(t *testing.T) {
	oldCPU := HostCPUCount
	oldMem := ReadHostMemoryTotalMB
	t.Cleanup(func() {
		HostCPUCount = oldCPU
		ReadHostMemoryTotalMB = oldMem
	})
	HostCPUCount = func() int { return 64 }
	ReadHostMemoryTotalMB = func() (int64, error) { return 262144, nil }

	if got := ResolvedHostQuotaCPUMilli(HostConfigQuota{}); got != 128000 {
		t.Fatalf("unexpected resolved CPU quota %d", got)
	}
	if got := ResolvedHostQuotaMemMB(HostConfigQuota{}); got != 327680 {
		t.Fatalf("unexpected resolved memory quota %d", got)
	}
}

func TestParseMemMBAndRoundUpFraction(t *testing.T) {
	if got := ParseMemMB("8Gi"); got != 8192 {
		t.Fatalf("ParseMemMB(8Gi) = %d", got)
	}
	for _, v := range []string{"", "not-a-quantity"} {
		if got := ParseMemMB(v); got != 0 {
			t.Fatalf("ParseMemMB(%q) = %d, want 0", v, got)
		}
	}
	if got := RoundUpFraction(8192, 5, 4); got != 10240 {
		t.Fatalf("RoundUpFraction = %d", got)
	}
	if got := RoundUpFraction(0, 5, 4); got != 0 {
		t.Fatalf("RoundUpFraction(0) = %d, want 0", got)
	}
}
