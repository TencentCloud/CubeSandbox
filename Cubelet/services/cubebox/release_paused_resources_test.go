// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

// sandboxWithResourceForTest builds a CubeBox carrying both a lifecycle status
// and a per-sandbox resource footprint, so the accounting kernel can be driven
// with realistic running/paused mixes.
func sandboxWithResourceForTest(id string, status cubeboxstore.Status, cpu, mem string, queues, dataDiskMB, storageDiskMB int64) *cubeboxstore.CubeBox {
	cb := newCubeboxWithStatusForTest(id, status)
	cb.ResourceWithOverHead = &cubeboxstore.ResourceWithOverHead{
		HostCpuQ:          resource.MustParse(cpu),
		HostMemQ:          resource.MustParse(mem),
		HostDataDiskMB:    dataDiskMB,
		HostStorageDiskMB: storageDiskMB,
	}
	cb.Queues = queues
	return cb
}

func TestAggregateSandboxResourcesRatioZeroCountsPausedQuota(t *testing.T) {
	now := time.Now().UnixNano()
	sbs := []*cubeboxstore.CubeBox{
		sandboxWithResourceForTest("run", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 4, 10, 20),
		sandboxWithResourceForTest("paused", cubeboxstore.Status{PausedAt: now}, "2000m", "4Gi", 8, 30, 40),
	}

	// releaseRatio 0 = legacy: a paused sandbox keeps reserving its full quota
	// and counts as running, so the node still looks fully committed and resume
	// is guaranteed.
	got := aggregateSandboxResources(sbs, 0)

	assert.Equal(t, int64(3000), got.MilliCPU)
	assert.Equal(t, int64(6144), got.MemoryMB)
	assert.Equal(t, int64(2), got.MvmNum)
	assert.Equal(t, int64(2), got.MvmRunningNum)
	assert.Equal(t, int64(12), got.NicQueues)
	assert.Equal(t, int64(40), got.DataDiskMB)
	assert.Equal(t, int64(60), got.StorageDiskMB)
}

func TestAggregateSandboxResourcesRatioOneReleasesPausedCPUAndMem(t *testing.T) {
	now := time.Now().UnixNano()
	sbs := []*cubeboxstore.CubeBox{
		sandboxWithResourceForTest("run", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 4, 10, 20),
		sandboxWithResourceForTest("paused", cubeboxstore.Status{PausedAt: now}, "2000m", "4Gi", 8, 30, 40),
	}

	// releaseRatio 1 = release everything: paused sandbox no longer reserves
	// CPU/RAM/NIC queues nor counts as running, freeing scheduling capacity...
	got := aggregateSandboxResources(sbs, 1)

	assert.Equal(t, int64(1000), got.MilliCPU, "paused CPU quota must be released")
	assert.Equal(t, int64(2048), got.MemoryMB, "paused mem quota must be released")
	assert.Equal(t, int64(1), got.MvmRunningNum, "paused sandbox is not running")
	assert.Equal(t, int64(4), got.NicQueues, "paused NIC queues must be released")
	// ...but the sandbox object still exists and its pause snapshot occupies
	// disk, so MvmNum and disk accounting are unchanged.
	assert.Equal(t, int64(2), got.MvmNum, "paused sandbox object still counts")
	assert.Equal(t, int64(40), got.DataDiskMB, "disk still counts under the policy")
	assert.Equal(t, int64(60), got.StorageDiskMB, "disk still counts under the policy")
}

func TestAggregateSandboxResourcesRatioOneAlsoReleasesPausingQuota(t *testing.T) {
	now := time.Now().UnixNano()
	sbs := []*cubeboxstore.CubeBox{
		// PAUSING is the in-flight pause transient; it must release quota too so
		// the freed capacity is visible the moment the pause begins committing.
		sandboxWithResourceForTest("pausing", cubeboxstore.Status{PausingAt: now}, "2000m", "4Gi", 8, 30, 40),
	}

	got := aggregateSandboxResources(sbs, 1)

	assert.Equal(t, int64(0), got.MilliCPU)
	assert.Equal(t, int64(0), got.MemoryMB)
	assert.Equal(t, int64(0), got.MvmRunningNum)
	assert.Equal(t, int64(1), got.MvmNum)
}

func TestAggregateSandboxResourcesPartialRelease(t *testing.T) {
	now := time.Now().UnixNano()
	sbs := []*cubeboxstore.CubeBox{
		sandboxWithResourceForTest("run", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 4, 10, 20),
		sandboxWithResourceForTest("paused", cubeboxstore.Status{PausedAt: now}, "2000m", "4Gi", 8, 30, 40),
	}

	// Release 50% of the paused sandbox's CPU/mem quota (reserve the other 50%).
	got := aggregateSandboxResources(sbs, 0.5)

	// running 1000m/2Gi + reserved 50% of paused 2000m/4Gi = 2000m/4096MB.
	assert.Equal(t, int64(2000), got.MilliCPU, "running + reserved half of paused CPU")
	assert.Equal(t, int64(4096), got.MemoryMB, "running + reserved half of paused mem")
	// Liveness is unaffected by the ratio: paused is still not running and
	// holds no NIC queues; the object and its disk snapshot still count.
	assert.Equal(t, int64(1), got.MvmRunningNum)
	assert.Equal(t, int64(4), got.NicQueues)
	assert.Equal(t, int64(2), got.MvmNum)
	assert.Equal(t, int64(40), got.DataDiskMB)
	assert.Equal(t, int64(60), got.StorageDiskMB)
}

func TestClampRatio(t *testing.T) {
	// In-range values pass through; out-of-range and non-finite inputs clamp to
	// the safe extremes so a malformed config can never corrupt the accounting
	// or bypass resume admission.
	assert.Equal(t, 0.0, clampRatio(0))
	assert.Equal(t, 1.0, clampRatio(1))
	assert.Equal(t, 0.25, clampRatio(0.25))
	assert.Equal(t, 0.0, clampRatio(-1), "negative clamps to 0")
	assert.Equal(t, 1.0, clampRatio(2), "above 1 clamps to 1")
	assert.Equal(t, 0.0, clampRatio(math.NaN()), "NaN clamps to 0 (no resume-admission bypass)")
	assert.Equal(t, 1.0, clampRatio(math.Inf(1)), "+Inf clamps to 1")
	assert.Equal(t, 0.0, clampRatio(math.Inf(-1)), "-Inf clamps to 0")
}

func TestResumeQuotaRejection(t *testing.T) {
	cases := []struct {
		name                                                                   string
		usedMemMB, needMemMB, memQuotaMB, usedCPUMilli, needCPUMilli, cpuQuota int64
		wantReject                                                             bool
	}{
		{"fits", 2048, 4096, 8192, 1000, 2000, 8000, false},
		{"mem exceeds quota", 6000, 4096, 8192, 0, 0, 0, true},
		{"cpu exceeds quota", 0, 0, 0, 7000, 2000, 8000, true},
		{"exact fit is allowed", 4096, 4096, 8192, 6000, 2000, 8000, false},
		{"unbounded quota never rejects", 999999, 999999, 0, 999999, 999999, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := resumeQuotaRejection(
				tc.usedMemMB, tc.needMemMB, tc.memQuotaMB,
				tc.usedCPUMilli, tc.needCPUMilli, tc.cpuQuota)
			if tc.wantReject {
				assert.NotEmpty(t, reason)
			} else {
				assert.Empty(t, reason)
			}
		})
	}
}

func TestAdmitResumeNoOpWhenPolicyDisabled(t *testing.T) {
	// With no config loaded, GetHostConf() returns defaults where the policy is
	// off, so resume must always be admitted regardless of node pressure.
	s := &service{}
	sb := sandboxWithResourceForTest("sb", cubeboxstore.Status{PausedAt: time.Now().UnixNano()}, "1000m", "2Gi", 1, 0, 0)
	rsp := &cubebox.UpdateCubeSandboxResponse{Ret: &errorcode.Ret{RetCode: errorcode.ErrorCode_Success}}

	require.Nil(t, s.admitResume(context.Background(), sb, rsp))
	assert.Equal(t, errorcode.ErrorCode_Success, rsp.Ret.RetCode)
}
