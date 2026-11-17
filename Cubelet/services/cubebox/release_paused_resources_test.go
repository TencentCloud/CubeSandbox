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

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
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

func TestAdmitResumeRejectsWhenResourceMetadataIsMissing(t *testing.T) {
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = 1.0
	defer func() { hostConf.Quota.PausedResourceReleaseRatio = 0 }()

	sb := newCubeboxWithStatusForTest("sb-no-resource", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	ret := s.admitResume(context.Background(), sb)

	require.NotNil(t, ret)
	assert.Equal(t, errorcode.ErrorCode_Conflict, ret.RetCode)
	assert.Contains(t, ret.RetMsg, "missing resource metadata")
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

	ret := s.admitResume(context.Background(), sb)

	require.NotNil(t, ret, "admitResume must reject when post-resume demand exceeds quota")
	assert.Equal(t, errorcode.ErrorCode_Conflict, ret.RetCode)
}

func TestResumeLockedSkipsAdmissionWhenFlagSet(t *testing.T) {
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

	sb := sandboxWithResourceForTest("sb-skip-admit", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	task := &fakeResumeTask{}
	sb.FirstContainer().Container = &fakeDestroyContainer{task: task}
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	now := time.Now()
	result := s.resumeLocked(context.Background(), sb, resumeOptions{
		taskDeadline:      now.Add(5 * time.Second),
		reconcileDeadline: now.Add(5 * time.Second),
		persist:           false,
		skipAdmission:     true,
	})

	require.True(t, result.running,
		"resumeLocked with skipAdmission must proceed despite capacity pressure")
	assert.Equal(t, 1, task.resumeCalls)
	assert.Equal(t, int64(0), sb.GetStatus().Get().PausedAt)
	assert.NotZero(t, sb.GetStatus().Get().StartedAt)
}

func TestResumeLockedRejectsWithoutSkipAdmissionUnderCapacityPressure(t *testing.T) {
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

	sb := sandboxWithResourceForTest("sb-no-skip-admit", cubeboxstore.Status{
		PausedAt: time.Now().UnixNano(),
	}, "4000m", "8Gi", 4, 0, 0)
	task := &fakeResumeTask{}
	sb.FirstContainer().Container = &fakeDestroyContainer{task: task}
	s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}

	now := time.Now()
	result := s.resumeLocked(context.Background(), sb, resumeOptions{
		taskDeadline:      now.Add(5 * time.Second),
		reconcileDeadline: now.Add(5 * time.Second),
		persist:           false,
		skipAdmission:     false,
	})

	require.False(t, result.running,
		"resumeLocked without skipAdmission must reject under capacity pressure")
	require.NotNil(t, result.ret)
	assert.Equal(t, errorcode.ErrorCode_Conflict, result.ret.RetCode)
	assert.Zero(t, task.resumeCalls, "task.Resume must not be called when admission rejects")
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

	rejected := s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-resume-create"))

	require.NotNil(t, rejected, "resume Create must be rejected when it exceeds the node quota")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected.RetCode)
	assert.Contains(t, rejected.RetMsg, "mem quota")
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

	assert.Nil(t, s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-fits")),
		"a resume that fits the node quota must be admitted")
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

	assert.Nil(t, s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-policy-off")))
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

	assert.Nil(t, s.admitPausedReplace(context.Background(), pausedReplaceRequest("")),
		"an ordinary create without the desired-id annotation is not a replacement")
	assert.Nil(t, s.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-elsewhere")),
		"a desired id this node does not hold is a cross-node or fresh create")

	sRunning := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: running}}}
	assert.Nil(t, sRunning.admitPausedReplace(context.Background(), pausedReplaceRequest("sb-running")),
		"a live sandbox collision is rejected downstream as already exists, never here")
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
