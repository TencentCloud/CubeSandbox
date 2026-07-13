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
			reason := resumeQuotaRejection(resumeQuotaCheck{
				usedMemMB:     tc.usedMemMB,
				needMemMB:     tc.needMemMB,
				memQuotaMB:    tc.memQuotaMB,
				usedCPUMilli:  tc.usedCPUMilli,
				needCPUMilli:  tc.needCPUMilli,
				cpuQuotaMilli: tc.cpuQuota,
			})
			if tc.wantReject {
				assert.NotEmpty(t, reason)
			} else {
				assert.Empty(t, reason)
			}
		})
	}
}

func TestScaleInt64(t *testing.T) {
	// Normal multiplication and truncation toward zero.
	assert.Equal(t, int64(2048), scaleInt64(4096, 0.5))
	assert.Equal(t, int64(0), scaleInt64(1000, 0))
	assert.Equal(t, int64(1000), scaleInt64(1000, 1))
	assert.Equal(t, int64(7), scaleInt64(10, 0.75), "truncates toward zero")
	// Guard the load-bearing contract that callers must pass a clamped factor:
	// a non-finite factor collapses int64(v*NaN) to math.MinInt64, which is why
	// clampRatio sanitises the ratio before it ever reaches scaleInt64.
	assert.Equal(t, int64(math.MinInt64), scaleInt64(4096, math.NaN()))
}

func TestAdmitResumeNoOpWhenPolicyDisabled(t *testing.T) {
	// With no config loaded, GetHostConf() returns defaults where the policy is
	// off, so resume must always be admitted regardless of node pressure.
	//
	// Inject a real (empty) cubebox manager rather than leaving cubeboxMgr nil:
	// the no-op must hold because the policy is off, not because admitResume
	// happens to early-return before touching s.cubeboxMgr. A future refactor
	// that reorders the policy-off check would otherwise nil-panic here instead
	// of failing loudly on the actual behaviour.
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{}}}
	sb := sandboxWithResourceForTest("sb", cubeboxstore.Status{PausedAt: time.Now().UnixNano()}, "1000m", "2Gi", 1, 0, 0)

	require.Nil(t, s.admitResume(context.Background(), sb))
}

// TestResumeDemandUsesBytePrecision locks the fix for the admission/accounting
// precision mismatch: resumeDemand must scale at byte precision and truncate to
// MB only at the end, exactly like aggregateSandboxResources, so the released
// fraction (need) plus the reserved fraction equals the sandbox's full quota
// with no off-by-one drift.
func TestResumeDemandUsesBytePrecision(t *testing.T) {
	// A HostMemQ deliberately NOT MiB-aligned (~101.9 MiB in raw bytes) so the
	// legacy truncate-then-scale ordering and the byte-precision
	// scale-then-truncate ordering diverge by 1 MB at this ratio.
	r := &cubeboxstore.ResourceWithOverHead{
		HostMemQ: resource.MustParse("106850713"),
		HostCpuQ: resource.MustParse("2000m"),
	}

	needMemMB, needCPU := resumeDemand(r, 0.9)

	// Byte precision: floor(floor(106850713 * 0.9) / 1MiB) = 91.
	assert.Equal(t, int64(91), needMemMB)
	assert.Equal(t, int64(1800), needCPU)

	// The old truncate-then-scale path would have produced 90; assert the fix
	// no longer matches it. Crucially this keeps need + reserved == full:
	// reserved = bytesToMB(scaleInt64(bytes, 1-0.9)) = 10, full = 101.
	legacyNeedMemMB := scaleInt64(r.HostMemQ.Value()/1024/1024, 0.9)
	assert.Equal(t, int64(90), legacyNeedMemMB)
	assert.NotEqual(t, legacyNeedMemMB, needMemMB)

	reservedMemMB := bytesToMB(scaleInt64(r.HostMemQ.Value(), 1-0.9))
	fullMemMB := bytesToMB(r.HostMemQ.Value())
	assert.Equal(t, fullMemMB, needMemMB+reservedMemMB, "need + reserved must equal full quota")
}

// TestResumeQuotaRejectionMessageFormat locks the exact reason wording, which
// is a cross-language contract parsed by regex in
// web/src/lib/sandboxActionError.ts. If either format string changes, the
// WebUI regex must change in lockstep or it silently falls back to a generic
// message; this test fails first to flag the drift.
func TestResumeQuotaRejectionMessageFormat(t *testing.T) {
	mem := resumeQuotaRejection(resumeQuotaCheck{
		usedMemMB:  6144,
		needMemMB:  4096,
		memQuotaMB: 8192,
	})
	assert.Equal(t, "need 4096MB + used 6144MB > mem quota 8192MB", mem)

	cpu := resumeQuotaRejection(resumeQuotaCheck{
		usedCPUMilli:  7000,
		needCPUMilli:  2000,
		cpuQuotaMilli: 8000,
	})
	assert.Equal(t, "need 2000m + used 7000m > cpu quota 8000m", cpu)
}
