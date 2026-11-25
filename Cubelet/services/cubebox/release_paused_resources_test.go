// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
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
		{"fits with headroom", 2048, 4096, 8192, 1000, 2000, 8000, false},
		{"mem exceeds quota", 6000, 4096, 8192, 0, 0, 0, true},
		{"cpu exceeds quota", 0, 0, 0, 7000, 2000, 8000, true},
		// The boundary matches the scheduler filters (memfilter.go /
		// cpufilter.go reject when free <= need): filling the node exactly is
		// rejected, so an allocation an ordinary create is refused for can
		// never be admitted as a resume (#2042's baseline: the 4th create of
		// 6144+2048 vs quota 8192 is rejected, and so is the resume).
		{"exact fill is rejected (mem)", 4096, 4096, 8192, 6000, 1000, 8000, true},
		{"exact fill is rejected (cpu)", 1000, 1000, 8192, 6000, 2000, 8000, true},
		{"issue #2042 boundary: 6144 + 2048 vs 8192", 6144, 2048, 8192, 0, 0, 0, true},
		{"one MB under the boundary still fits", 6143, 2048, 8192, 0, 0, 0, false},
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
	// Callers must pass a clamped factor: int64(v*NaN) is architecture-specific
	// (MinInt64 on amd64, 0 on arm64), so no cross-arch assertion is possible
	// here. The contract is guarded by TestClampRatio's NaN/Inf cases instead.
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

	release, rejected := s.admitResume(context.Background(), sb)
	require.Nil(t, rejected)
	require.NotNil(t, release, "admitResume must always return a safe-to-defer release")
	release()
	// Policy off registers nothing, so the ledger stays empty.
	memMB, cpuMilli := s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB)
	assert.Zero(t, cpuMilli)
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
	assert.Equal(t, "need 4096MB + used 6144MB >= mem quota 8192MB", mem)

	cpu := resumeQuotaRejection(resumeQuotaCheck{
		usedCPUMilli:  7000,
		needCPUMilli:  2000,
		cpuQuotaMilli: 8000,
	})
	assert.Equal(t, "need 2000m + used 7000m >= cpu quota 8000m", cpu)
}

func TestAdmitResumeRejectsWhenResourceMetadataIsMissing(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	defer func() { hostConf.Quota.PausedResourceReleaseRatio = 0 }()

	sb := newCubeboxWithStatusForTest("sb-no-resource", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	release, ret := s.admitResume(context.Background(), sb)

	require.NotNil(t, ret)
	assert.Equal(t, errorcode.ErrorCode_Conflict, ret.RetCode)
	assert.Contains(t, ret.RetMsg, "missing resource metadata")
	require.NotNil(t, release)
	release() // no-op on rejection; must be safe to call
}

func TestAdmitResumeRejectsWhenCapacityExceeded(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	sb := sandboxWithResourceForTest("sb-overcommit", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	release, ret := s.admitResume(context.Background(), sb)

	require.NotNil(t, ret, "admitResume must reject when post-resume demand exceeds quota")
	assert.Equal(t, errorcode.ErrorCode_Conflict, ret.RetCode)
	release()
	// A rejected resume must not leave a phantom reservation behind.
	memMB, _ := s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "rejection must not register a reservation")
}

// pausedReplaceRequest builds the thin Create-from-pause request CubeMaster
// sends for a same-node XFS resume: desired sandbox id + pause snapshot id.
func pausedReplaceRequest(desired string) *cubebox.RunCubeSandboxRequest {
	req := &cubebox.RunCubeSandboxRequest{RequestID: "resume-create"}
	if desired != "" {
		req.Annotations = map[string]string{
			constants.MasterAnnotationDesiredSandboxID: desired,
			constants.MasterAnnotationPauseSnapshotID:  "snap-test",
		}
	}
	return req
}

// TestAdmitPausedReplaceRejectsResumeCreateOverQuota locks the fix for the
// same-node XFS resume quota bypass: Resume now arrives as Create-from-pause
// (not Update(resume)), so without the Create-entry admission a resume would
// re-add the released quota share even though an identical ordinary create
// would be rejected.
func TestAdmitPausedReplaceRejectsResumeCreateOverQuota(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// PAUSED tombstone of an 8Gi sandbox on a 4Gi-quota node: the paused
	// record contributes nothing (ratio 1), and resuming needs the full 8Gi.
	sb := sandboxWithResourceForTest("sb-resume-create", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	sb.SandboxID = "sb-resume-create"
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	release, rejected := s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-resume-create"))

	require.NotNil(t, rejected, "resume Create must be rejected when it exceeds the node quota")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected.RetCode)
	assert.Contains(t, rejected.RetMsg, "mem quota")
	release()
	memMB, _ := s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "a rejected resume must not register a reservation")
}

func TestAdmitPausedReplaceAllowsWhenResumeFits(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 8000
	hostConf.Quota.Mem = "16Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	sb := sandboxWithResourceForTest("sb-fits", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	sb.SandboxID = "sb-fits"
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	release, rejected := s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-fits"))
	assert.Nil(t, rejected, "a resume that fits the node quota must be admitted")
	require.NotNil(t, release)

	// While the reservation is held, the admitted demand is visible to the
	// ledger, so a second resume of the same size no longer fits the 16Gi node
	// that already carries this 8Gi reservation.
	memMB, cpuMilli := s.cubeboxMgr.reservations.totals()
	assert.Equal(t, int64(8192), memMB, "admission must reserve the sandbox's full memory quota")
	assert.Equal(t, int64(4000), cpuMilli, "admission must reserve the sandbox's full cpu quota")

	release()
	memMB, cpuMilli = s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "release must drop the reservation")
	assert.Zero(t, cpuMilli)
	// release is idempotent: a second call must not panic or go negative.
	release()
	memMB, _ = s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB)
}

func TestAdmitPausedReplaceNoOpWhenPolicyDisabled(t *testing.T) {
	// Policy off (default config): resume keeps the legacy guaranteed
	// behaviour, even when the tombstone alone exceeds the quota.
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	sb := sandboxWithResourceForTest("sb-policy-off", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	sb.SandboxID = "sb-policy-off"
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	release, rejected := s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-policy-off"))
	assert.Nil(t, rejected)
	require.NotNil(t, release)
	release()
	memMB, _ := s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "policy off must not reserve anything")
}

func TestAdmitPausedReplaceIgnoresEverythingButAPausedReplacement(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// An over-quota tombstone proves the no-op cases below are not skipping
	// the check by accident.
	paused := sandboxWithResourceForTest("sb-paused", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	paused.SandboxID = "sb-paused"
	running := sandboxWithResourceForTest("sb-running", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	running.SandboxID = "sb-running"
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: paused}}}

	release, rejected := s.admitPausedReplace(context.Background(), pausedReplaceRequest(""))
	assert.Nil(t, rejected, "an ordinary create without the desired-id annotation is not a replacement")
	release()
	release, rejected = s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-elsewhere"))
	assert.Nil(t, rejected, "a desired id this node does not hold is a cross-node or fresh create")
	release()

	sRunning := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: running}}}
	release, rejected = sRunning.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-running"))
	assert.Nil(t, rejected,
		"a live sandbox collision is rejected downstream as already exists, never here")
	release()
	memMB, _ := sRunning.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "non-replacements must not reserve anything")
}

// TestAdmitPausedReplaceFailsClosedOnStoreError covers the fail-closed
// requirement: with the release-ratio policy on, a real store error (not
// not-found) leaves the local tombstone state UNKNOWN, and admission must
// reject rather than silently skip the quota gate. Not-found must keep the
// no-op behaviour (cross-node restore / fresh create).
func TestAdmitPausedReplaceFailsClosedOnStoreError(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// A store I/O failure must fail closed: the tombstone may or may not
	// exist, and admitting blindly is exactly the silent bypass #2042 asks to
	// prevent.
	storeErr := errors.New("boltdb: unexpected EOF")
	sErr := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{getErr: storeErr}}}
	release, rejected := sErr.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-unknown"))
	require.NotNil(t, rejected, "a store error under the policy must reject, not no-op")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected.RetCode)
	assert.Contains(t, rejected.RetMsg, "cannot read the local sandbox state")
	release()

	// Not-found is not an error state: cross-node restore and fresh creates
	// keep their no-op path.
	sNotFound := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{
		getErr: utils.ErrorKeyNotFound,
	}}}
	release, rejected = sNotFound.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-gone"))
	assert.Nil(t, rejected, "not-found must keep the no-op (cross-node restore) behaviour")
	release()
	memMB, _ := sNotFound.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB)
}

// TestCreateRejectsResumeOverQuotaBeforeTheFlowRuns is the regression test for
// the same-node XFS resume bypass: Create-from-pause must refuse an over-quota
// resume with Conflict before the workflow allocates anything, and must not
// destroy the paused tombstone (Conflict skips create-flow failover).
func TestCreateRejectsResumeOverQuotaBeforeTheFlowRuns(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	seedPausePackageForTest(t, "snap-test", resumePackedSpecForTest())

	// A PAUSED tombstone whose shim is provably gone, so gatePausedReplace
	// passes and only the quota admission can stop the resume.
	sb := sandboxWithResourceForTest("sb-resume-create", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	sb.SandboxID = "sb-resume-create"
	flow := &fakeCreateFlow{}
	s := createServiceForTest(sb, flow, time.Minute)

	rsp, err := s.Create(context.Background(), resumeReplaceRequest("sb-resume-create"))
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rsp.Ret.RetCode,
		"an over-quota resume must be rejected with the retriable Conflict code")
	assert.Zero(t, flow.createCalls, "admission must run before the flow allocates anything")
	assert.Zero(t, flow.destroyCalls, "a rejected resume must not roll the paused tombstone back")
}

// seedPausePackageForTest lays down the on-disk pause package a real Resume
// reads: a catalog entry of kind pause_snapshot whose metadata dir holds the
// packed sandbox_spec.json. It returns the entry so the caller can assert
// against the resolved MetaDir.
func seedPausePackageForTest(t *testing.T, snapID string, packed *cubebox.RunCubeSandboxRequest) *storage.SnapshotCatalogEntry {
	t.Helper()
	entry := &storage.SnapshotCatalogEntry{
		SnapshotID: snapID,
		Kind:       storage.CatalogKindPauseSnapshot,
	}
	seedCatalogForTest(t, entry) // fills SnapshotPath/MetaDir, writes catalog, registers cleanup
	require.NoError(t, writePauseSandboxSpec(entry.MetaDir, packed))
	return entry
}

// TestCreateRejectsResumeOverQuotaAfterPausePackageExpansion pins the real
// Create-from-pause path, which the annotation-only test above cannot reach:
// a thin Create carries cube.master.pause.snapshot.id, so Create runs
// expandPauseSnapshotPackage, which does *req = *packed and then relies on
// pauseRestoreBinding.applyTo to re-stamp the desired-id annotation the quota
// gate reads. If that re-stamp ever regresses, the gate silently no-ops (an
// empty desired-id is "not a replacement") and an over-quota resume slips
// through — this test fails first, because the packed spec deliberately omits
// the annotation so only applyTo can restore it.
func TestCreateRejectsResumeOverQuotaAfterPausePackageExpansion(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 4000
	hostConf.Quota.Mem = "4Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// The packed spec is the sandbox as it ran: containers and resources, but
	// no resume binding (no desired-id, no snapshot-id). Those only exist on the
	// thin Create and must be re-stamped by applyTo after expansion.
	seedPausePackageForTest(t, "snap-test", &cubebox.RunCubeSandboxRequest{
		RequestID: "original-create",
		Containers: []*cubebox.ContainerConfig{{
			Name:      "main",
			Resources: &cubebox.Resource{Cpu: "4000m", Mem: "8Gi"},
		}},
	})

	// PAUSED tombstone whose shim is provably gone, so gatePausedReplace passes
	// and only the quota admission can stop the resume.
	sb := sandboxWithResourceForTest("sb-resume-expand", cubeboxstore.Status{
		StartedAt: 1,
		PausedAt:  time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	sb.SandboxID = "sb-resume-expand"
	flow := &fakeCreateFlow{}
	s := createServiceForTest(sb, flow, time.Minute)

	// pausedReplaceRequest is the production thin Create: desired-id + snapshot-id
	// and no containers, exactly what CubeMaster sends for a same-node resume.
	thinReq := pausedReplaceRequest("sb-resume-expand")
	rsp, err := s.Create(context.Background(), thinReq)
	require.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rsp.Ret.RetCode,
		"an over-quota resume must be rejected with the retriable Conflict code")
	assert.Zero(t, flow.createCalls, "admission must run before the flow allocates anything")
	assert.Zero(t, flow.destroyCalls, "a rejected resume must not roll the paused tombstone back")

	// The gate fired because the desired-id survived expansion. Assert the
	// post-expansion request directly: containers came from the packed spec and
	// the resume binding was re-stamped onto them.
	require.Len(t, thinReq.GetContainers(), 1, "expansion must pull containers from the packed spec")
	assert.Equal(t, "main", thinReq.GetContainers()[0].GetName())
	assert.Equal(t, "sb-resume-expand",
		thinReq.GetAnnotations()[constants.MasterAnnotationDesiredSandboxID],
		"applyTo must re-stamp the desired-id the quota gate reads")
	assert.Equal(t, "snap-test",
		thinReq.GetAnnotations()[constants.MasterAnnotationPauseSnapshotID],
		"applyTo must re-stamp the pause snapshot id")
}

// TestConcurrentResumesDoNotBothPassQuota is the direct regression for the
// #2042 concurrency repro: two resumes of DIFFERENT sandboxes hold different
// per-sandbox lifecycle locks, so they reach admission in parallel. Each reads
// the same aggregateAllocated snapshot (the two PAUSED tombstones contribute
// 0 at ratio 1.0), and without a node-level reservation both would compute
// 0+4096 < 8192 and pass, ending with the node filled to exactly its 8 GiB
// quota — an allocation an ordinary create is rejected for. The ledger makes
// check-and-register atomic, so exactly one may pass regardless of
// interleaving.
//
// The goroutines call admitResume directly (the shared core admitPausedReplace
// delegates to) so every touch of the shared fixtures — the store scan and the
// ledger — happens inside the ledger critical section; GetStatus mutates the
// record it is called on, and this keeps each goroutine's lazy-init writes to
// its own tombstone. The sequential
// TestResumeReservationBlocksSecondUntilReleased covers the full
// admitPausedReplace path.
func TestConcurrentResumesDoNotBothPassQuota(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 0 // isolate the memory dimension
	hostConf.Quota.Mem = "8Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// Two paused tombstones each wanting the full 4 GiB back on resume
	// (ratio 1.0, tombstones contribute nothing to used). One fits
	// (0+4096 < 8192); the second only fits if it cannot see the first's
	// reservation (0+4096+4096 == 8192 >= 8192).
	tombA := sandboxWithResourceForTest("sb-a", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tombB := sandboxWithResourceForTest("sb-b", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{
		list: []*cubeboxstore.CubeBox{tombA, tombB},
	}}}

	const rounds = 64
	for round := 0; round < rounds; round++ {
		results := make([]*errorcode.Ret, 2)
		releases := make([]func(), 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, tomb := range []*cubeboxstore.CubeBox{tombA, tombB} {
			wg.Add(1)
			go func(i int, tomb *cubeboxstore.CubeBox) {
				defer wg.Done()
				<-start // maximize the chance both hit the gate together
				releases[i], results[i] = s.admitResume(context.Background(), tomb)
			}(i, tomb)
		}
		close(start)
		wg.Wait()

		passed := 0
		for i := range results {
			if results[i] == nil {
				passed++
				require.NotNil(t, releases[i])
				releases[i]()
			}
		}
		assert.Equal(t, 1, passed,
			"round %d: exactly one of the two over-subscription resumes may pass", round)

		memMB, _ := s.cubeboxMgr.reservations.totals()
		require.Zero(t, memMB, "round %d: reservations must be empty after release", round)
	}
}

// TestResumeReservationBlocksSecondUntilReleased is the deterministic companion
// to the race above: it pins the ledger semantics without relying on goroutine
// interleaving — while one resume holds its reservation, a second identical
// resume that would over-subscribe the node is rejected, and once released it is
// admitted.
func TestResumeReservationBlocksSecondUntilReleased(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 0
	hostConf.Quota.Mem = "8Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	running := sandboxWithResourceForTest("sb-running", cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(),
	}, "0m", "5Gi", 0, 0, 0)
	running.SandboxID = "sb-running"
	tombA := sandboxWithResourceForTest("sb-a", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "0m", "2Gi", 0, 0, 0)
	tombA.SandboxID = "sb-a"
	tombB := sandboxWithResourceForTest("sb-b", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "0m", "2Gi", 0, 0, 0)
	tombB.SandboxID = "sb-b"
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{
		list: []*cubeboxstore.CubeBox{running, tombA, tombB},
	}}}

	// A fits on its own: 5120 + 2048 == 7168 < 8192.
	releaseA, rejectedA := s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-a"))
	require.Nil(t, rejectedA)
	memMB, _ := s.cubeboxMgr.reservations.totals()
	assert.Equal(t, int64(2048), memMB, "the admitted resume holds its full 2 GiB")

	// With A's 2048 reserved, B sees used 5120 + reserved 2048 + need 2048 ==
	// 9216 >= 8192 and is rejected — the concurrent repro cannot double-spend.
	releaseB, rejectedB := s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-b"))
	require.NotNil(t, rejectedB, "the second resume must see the first's reservation")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejectedB.RetCode)
	releaseB()

	// Once A releases (its create failed, say), B fits again.
	releaseA()
	memMB, _ = s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB, "releasing A frees its reservation")

	releaseB2, rejectedB2 := s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-b"))
	assert.Nil(t, rejectedB2, "after A's release the same resume fits")
	releaseB2()
}

// TestAdmitResumeUsesHostDerivedQuotaWhenConfigUnset pins the B1 fix: the
// default deployment configures no quota, but node status reports one derived
// from the host (cpuCount×2.0, MemTotal×1.25). Resume admission must derive
// the same quota — a node that reports a capacity to Master must gate resumes
// against it, or every default deployment would bypass the check entirely.
func TestAdmitResumeUsesHostDerivedQuotaWhenConfigUnset(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	// Policy on, quota unset: exactly the default-config shape.
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	hostConf.Quota.Cpu = 0
	hostConf.Quota.Mem = ""
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	// A demand far beyond any real host: the derived quota is nonzero on any
	// machine that can run the test (cpu count and MemTotal read from the
	// kernel), so the resume must be rejected — proving the check used the
	// host derivation rather than skipping on the unset config.
	sb := sandboxWithResourceForTest("sb-derived", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "1000000", "100Pi", 0, 0, 0)
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	release, rejected := s.admitResume(context.Background(), sb)
	defer release()
	require.NotNil(t, rejected,
		"with the policy on, an unset config quota must fall back to the host derivation, not skip the check")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected.RetCode)
	assert.Contains(t, rejected.RetMsg, "mem quota",
		"the rejection must name the derived dimension it exceeded")
}

// TestConcurrentResumesReserveOnlyTheReleasedFraction is the I4 pin at a
// partial ratio: admission reserves r·F (the released fraction), not F. With
// r=0.5 on an 8 GiB node, two 4 GiB tombstones each still hold 2 GiB in the
// store aggregate and each need 2 GiB back — 4096 used + 2048 + 2048 == 8192
// fills the node exactly, and the >= boundary rejects the second. A full-F
// reservation would have rejected the second even though the pair genuinely
// fits, stranding a resumable sandbox the node has room for.
func TestConcurrentResumesReserveOnlyTheReleasedFraction(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 0.5
	hostConf.Quota.Cpu = 0 // isolate the memory dimension
	hostConf.Quota.Mem = "8Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	tombA := sandboxWithResourceForTest("sb-half-a", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tombA.SandboxID = "sb-half-a"
	tombB := sandboxWithResourceForTest("sb-half-b", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tombB.SandboxID = "sb-half-b"
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{
		list: []*cubeboxstore.CubeBox{tombA, tombB},
	}}}

	// The store aggregate counts the retained half: 2048 + 2048 == 4096 MB.
	alloc := s.cubeboxMgr.aggregateAllocated()
	require.Equal(t, int64(4096), alloc.MemoryMB,
		"at r=0.5 each tombstone keeps half its quota in the aggregate")

	const rounds = 32
	for round := 0; round < rounds; round++ {
		results := make([]*errorcode.Ret, 2)
		releases := make([]func(), 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, tomb := range []*cubeboxstore.CubeBox{tombA, tombB} {
			wg.Add(1)
			go func(i int, tomb *cubeboxstore.CubeBox) {
				defer wg.Done()
				<-start
				releases[i], results[i] = s.admitResume(context.Background(), tomb)
			}(i, tomb)
		}
		close(start)
		wg.Wait()

		// First admit sees 4096 + 2048 == 6144 < 8192 and passes, reserving
		// only its r·F == 2048; the second then sees 4096 + 2048 + 2048 ==
		// 8192 >= 8192 and is rejected. Exactly one passes per round.
		passed := 0
		for i := range results {
			if results[i] == nil {
				passed++
				require.NotNil(t, releases[i])
				releases[i]()
			} else {
				assert.Equal(t, errorcode.ErrorCode_Conflict, results[i].RetCode)
				releases[i]()
			}
		}
		assert.Equal(t, 1, passed,
			"round %d: at r=0.5 exactly one of the two 2 GiB resumes fits under the >= boundary", round)

		memMB, _ := s.cubeboxMgr.reservations.totals()
		require.Zero(t, memMB, "round %d: reservations must be empty after release", round)
	}
}

// TestAdmitResumeReservesReleasedFractionNotFullQuota is the deterministic
// companion to the race above: at r=0.5 a 4 GiB tombstone on an empty-
// aggregate node reserves exactly 2048 MB — the released fraction — so a
// reservation never over-counts a demand the store aggregate still carries.
func TestAdmitResumeReservesReleasedFractionNotFullQuota(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 0.5
	hostConf.Quota.Cpu = 0
	hostConf.Quota.Mem = "8Gi"
	defer func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	}()

	tomb := sandboxWithResourceForTest("sb-frac", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "0m", "4Gi", 0, 0, 0)
	tomb.SandboxID = "sb-frac"
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{list: []*cubeboxstore.CubeBox{tomb}}}}

	release, rejected := s.admitResume(context.Background(), tomb)
	require.Nil(t, rejected)

	memMB, _ := s.cubeboxMgr.reservations.totals()
	assert.Equal(t, int64(2048), memMB,
		"the reservation must be the released fraction r·F, not the full quota F")
	release()
	memMB, _ = s.cubeboxMgr.reservations.totals()
	assert.Zero(t, memMB)
}

// raceSafeListManager serves List() from a fixed slice so the concurrent
// readers of the race test never touch the fake's unsynchronized call
// counters. Every other cubes.CubeboxAPI method is inherited from the fake.
type raceSafeListManager struct {
	*fakeCubeboxAPI
	rows []*cubeboxstore.CubeBox
}

func (m *raceSafeListManager) List() []*cubeboxstore.CubeBox { return m.rows }

// TestGetStatusConcurrentWithAggregateScanIsRaceFree pins the B2/C fix: the
// heartbeat's aggregate scan walks every row calling GetStatus without holding
// the row's write lock, and an RPC handler can concurrently Update the same
// row's status. GetStatus used to mirror its resolution into cb.Status on
// every call — a write on the hottest read path — which -race flagged the
// moment both touched one row. Now that GetStatus is strictly read-only, the
// concurrent scan and update must run clean under -race.
func TestGetStatusConcurrentWithAggregateScanIsRaceFree(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	defer func() { hostConf.Quota.PausedResourceReleaseRatio = 0 }()

	rows := make([]*cubeboxstore.CubeBox, 0, 16)
	for i := 0; i < 16; i++ {
		sb := sandboxWithResourceForTest(fmt.Sprintf("sb-race-%d", i), cubeboxstore.Status{
			PausedAt: time.Now().UnixNano(),
		}, "1000m", "1Gi", 0, 0, 0)
		sb.SandboxID = sb.ID
		rows = append(rows, sb)
	}
	// The shared fake counts calls on unsynchronized fields; the race this
	// test hunts is in the rows themselves (GetStatus vs the aggregate scan),
	// so List() is served from an immutable slice here.
	s := &service{cubeboxMgr: &local{cubeboxManger: &raceSafeListManager{
		fakeCubeboxAPI: &fakeCubeboxAPI{},
		rows:           rows,
	}}}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Readers: the aggregate scan (calls GetStatus on every row) plus direct
	// GetStatus on the first row, mimicking the heartbeat and an RPC handler.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.cubeboxMgr.aggregateAllocated()
				_ = rows[0].GetStatus().Get().State()
			}
		}()
	}
	// Writer: an RPC handler updating the same rows' status under its own lock.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, sb := range rows {
					_ = sb.GetStatus().Update(func(st cubeboxstore.Status) (cubeboxstore.Status, error) {
						st.PausedAt = time.Now().UnixNano()
						return st, nil
					})
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
