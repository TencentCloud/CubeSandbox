// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func stubMachineInfo(t *testing.T, cpus int, memMB int64) {
	t.Helper()
	oldCPU := hostCPUCount
	oldMem := readHostMemoryTotalMB
	hostCPUCount = func() int { return cpus }
	readHostMemoryTotalMB = func() (int64, error) { return memMB, nil }
	t.Cleanup(func() {
		hostCPUCount = oldCPU
		readHostMemoryTotalMB = oldMem
	})
}

func TestBuildQuotaReportPrefersConfiguredQuota(t *testing.T) {
	stubMachineInfo(t, 64, 262144)
	got := buildQuotaReport(&config.HostConf{
		Quota: config.HostConfigQuota{
			Cpu:                   128000,
			Mem:                   "256Gi",
			MvmLimit:              500,
			CreationConcurrentNum: 32,
		},
	})
	want := masterclient.QuotaReport{MilliCPU: 128000, MemMB: 262144, MaxMvmNum: 500, CreateConcurrentNum: 32}
	if got == nil || got.MilliCPU != want.MilliCPU || got.MemMB != want.MemMB ||
		got.MaxMvmNum != want.MaxMvmNum || got.CreateConcurrentNum != want.CreateConcurrentNum {
		t.Fatalf("buildQuotaReport = %+v, want %+v", got, want)
	}
	if got.PausedReleaseRatio == nil || *got.PausedReleaseRatio != 0 {
		t.Fatalf("paused ratio = %+v, want 0 (zero-value config)", got.PausedReleaseRatio)
	}
}

func TestBuildQuotaReportUsesMachineDefaults(t *testing.T) {
	stubMachineInfo(t, 2, 8192)
	// Defaults: cpu = 2*1000*2, mem = 8192*5/4, mvm = 10240/512.
	got := buildQuotaReport(&config.HostConf{})
	want := masterclient.QuotaReport{MilliCPU: 4000, MemMB: 10240, MaxMvmNum: 20}
	if got == nil || got.MilliCPU != want.MilliCPU || got.MemMB != want.MemMB ||
		got.MaxMvmNum != want.MaxMvmNum || got.CreateConcurrentNum != want.CreateConcurrentNum {
		t.Fatalf("buildQuotaReport = %+v, want %+v", got, want)
	}
	if got.PausedReleaseRatio == nil || *got.PausedReleaseRatio != 0 {
		t.Fatalf("paused ratio = %+v, want 0 (zero-value config)", got.PausedReleaseRatio)
	}
}

func TestBuildQuotaReportNilWhenUnresolvable(t *testing.T) {
	oldCPU := hostCPUCount
	oldMem := readHostMemoryTotalMB
	hostCPUCount = func() int { return 0 }
	readHostMemoryTotalMB = func() (int64, error) { return 0, errors.New("unreadable") }
	t.Cleanup(func() {
		hostCPUCount = oldCPU
		readHostMemoryTotalMB = oldMem
	})

	if got := buildQuotaReport(&config.HostConf{}); got != nil {
		t.Fatalf("expected nil report when nothing resolves, got %+v", got)
	}
}

// Register and heartbeat quotas must come from the same resolution chain.
func TestBuildQuotaReportMatchesRegisterResolution(t *testing.T) {
	stubMachineInfo(t, 4, 16384)

	cases := []struct {
		name    string
		hostCfg *config.HostConf
	}{
		{name: "machine defaults", hostCfg: &config.HostConf{}},
		{name: "configured quota", hostCfg: &config.HostConf{
			Quota: config.HostConfigQuota{Cpu: 9000, Mem: "9Gi", MvmLimit: 9, CreationConcurrentNum: 7},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &cubeletnodemeta.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
			applyHostQuotaWithConfig(node, tc.hostCfg)
			kl := &Cubelet{instanceType: "test"}
			reg := kl.buildRegisterRequest(node, tc.hostCfg)
			report := buildQuotaReport(tc.hostCfg)

			if report == nil {
				t.Fatal("expected non-nil quota report")
			}
			if reg.QuotaCPU != report.MilliCPU {
				t.Errorf("cpu: register=%d heartbeat=%d", reg.QuotaCPU, report.MilliCPU)
			}
			if reg.QuotaMemMB != report.MemMB {
				t.Errorf("memMB: register=%d heartbeat=%d", reg.QuotaMemMB, report.MemMB)
			}
			if reg.MaxMvmNum != report.MaxMvmNum {
				t.Errorf("maxMvmNum: register=%d heartbeat=%d", reg.MaxMvmNum, report.MaxMvmNum)
			}
			if reg.CreateConcurrentNum != report.CreateConcurrentNum {
				t.Errorf("createConcurrentNum: register=%d heartbeat=%d", reg.CreateConcurrentNum, report.CreateConcurrentNum)
			}
		})
	}
}
