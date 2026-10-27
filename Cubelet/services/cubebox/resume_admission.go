// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//

package cubebox

import (
	"context"
	"errors"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type resumeReservation struct {
	memBytes int64
	cpuMilli int64
}

// admitAndReserveResume enforces the node CPU/memory quota on the restore
// (Create-from-pause) path and reserves the admitted demand until the caller
// releases it. The returned release func is always non-nil and safe to call
// once, whether the following Create succeeds or fails.
func (l *local) admitAndReserveResume(ctx context.Context, sandboxID string) (func(), *errorcode.Ret) {
	noop := func() {}

	// Read the host quota exactly once and thread it through every decision
	// below. A config reload between two reads would make the accounting
	// fraction, the demand and the reservations disagree with each other.
	hostQuota := config.GetHostConf().Quota
	releaseRatio := clampRatio(hostQuota.PausedResourceReleaseRatio)
	if releaseRatio <= 0 {
		// Policy off: paused sandboxes kept their full quota, so resume is
		// guaranteed and needs no admission (legacy behaviour).
		return noop, nil
	}

	l.resumeAdmitMu.Lock()
	defer l.resumeAdmitMu.Unlock()

	sb, err := l.cubeboxManger.Get(ctx, sandboxID)
	// A missing bucket means "no local record" here, exactly as the rest of
	// this package treats it (CleanUp, IsCubeboxExists): this node simply has
	// nothing to size the demand from, so let the admission fall through to
	// "no local record" instead of failing a cross-node restore.
	if err != nil && !errors.Is(err, utils.ErrorKeyNotFound) && !errors.Is(err, utils.ErrorBucketNotFound) {
		log.G(ctx).Warnf("admitAndReserveResume reject sandbox=%s: read local record: %v", sandboxID, err)
		// A store failure is not a capacity conflict. Conflict maps to HTTP 409
		// and the WebUI's "free up capacity and retry" banner; neither applies
		// when the node simply cannot read its own metadata.
		return noop, &errorcode.Ret{
			RetCode: errorcode.ErrorCode_UpdateLocalMetaDataFailed,
			RetMsg:  "resume admission unavailable: cannot read sandbox state: " + err.Error(),
		}
	}
	if sb == nil || sb.SandboxID != sandboxID {
		return noop, nil
	}
	if st := sb.GetStatus(); st == nil || st.Get().State() != cubebox.ContainerState_CONTAINER_PAUSED {
		return noop, nil
	}

	in, state := resumeAdmissionInputs(sb, hostQuota, releaseRatio)
	switch state {
	case resumeAdmissionOff:
		return noop, nil
	case resumeAdmissionUnsizable:
		log.G(ctx).Warnf("admitAndReserveResume reject sandbox=%s: missing resource metadata", sandboxID)
		return noop, &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Conflict,
			RetMsg:  "resume rejected by paused_resource_release_ratio policy: missing resource metadata",
		}
	}

	// One store snapshot under the admission lock, shared by the allocation
	// aggregate and the reservation top-up. Create flows mutate rows outside
	// this mutex, so sampling List() twice could observe the same sandbox in
	// two different states and count it in neither.
	sbs := l.cubeboxManger.List()
	alloc := aggregateSandboxResources(sbs, releaseRatio)
	reservedMem, reservedCPU := l.reservedResumeDemandLocked(sbs, sandboxID, releaseRatio)

	if reason := resumeQuotaRejection(resumeQuotaCheck{
		usedMemMB:     alloc.MemoryMB + reservedMem,
		needMemMB:     in.needMemMB,
		memQuotaMB:    in.memQuotaMB,
		usedCPUMilli:  alloc.MilliCPU + reservedCPU,
		needCPUMilli:  in.needCPUMilli,
		cpuQuotaMilli: in.cpuQuotaMilli,
	}); reason != "" {
		// Conflict (HTTP 409 at the edge): an expected, retriable capacity
		// state, not a backend failure. The exact RetMsg wording is a
		// cross-language contract with the WebUI banner — see
		// resumeQuotaRejection.
		log.G(ctx).Warnf("admitAndReserveResume reject sandbox=%s: %s", sandboxID, reason)
		return noop, &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Conflict,
			RetMsg:  "resume rejected by paused_resource_release_ratio policy: " + reason,
		}
	}

	if l.resumeReserved == nil {
		l.resumeReserved = make(map[string]resumeReservation)
	}
	l.resumeReserved[sandboxID] = resumeReservation{
		memBytes: sb.ResourceWithOverHead.HostMemQ.Value(),
		cpuMilli: sb.ResourceWithOverHead.HostCpuQ.MilliValue(),
	}

	released := false
	return func() {
		l.resumeAdmitMu.Lock()
		defer l.resumeAdmitMu.Unlock()
		if released {
			return
		}
		released = true
		delete(l.resumeReserved, sandboxID)
	}, nil
}

// reservedResumeDemandLocked tops each still-in-flight reservation up to the
// sandbox's full demand, using the same store snapshot the caller used for the
// allocation aggregate. The caller must hold resumeAdmitMu and pass the sbs it
// sampled under that lock.
func (l *local) reservedResumeDemandLocked(sbs []*cubeboxstore.CubeBox, excludeSandboxID string, releaseRatio float64) (memMB, cpuMilli int64) {
	if len(l.resumeReserved) == 0 {
		return 0, 0
	}
	byID := make(map[string]*cubeboxstore.CubeBox, len(l.resumeReserved)+1)
	for _, sb := range sbs {
		if sb == nil {
			continue
		}
		byID[sb.ID] = sb
		if sb.SandboxID != "" {
			byID[sb.SandboxID] = sb
		}
	}

	memBytes := int64(0)
	for id, r := range l.resumeReserved {
		if id == excludeSandboxID {
			continue
		}
		countedMem, countedCPU := storeCountedResumeDemand(byID[id], releaseRatio)
		if r.memBytes > countedMem {
			memBytes += r.memBytes - countedMem
		}
		if r.cpuMilli > countedCPU {
			cpuMilli += r.cpuMilli - countedCPU
		}
	}
	return bytesToMB(memBytes), cpuMilli
}

func storeCountedResumeDemand(sb *cubeboxstore.CubeBox, releaseRatio float64) (memBytes, cpuMilli int64) {
	if sb == nil || sb.ResourceWithOverHead == nil {
		return 0, 0
	}
	st := sb.GetStatus()
	if st == nil {
		return 0, 0
	}
	state := st.Get().State()
	if !isContainerInGoodState(state) {
		return 0, 0
	}
	if state == cubebox.ContainerState_CONTAINER_PAUSED || state == cubebox.ContainerState_CONTAINER_PAUSING {
		factor := 1 - clampRatio(releaseRatio)
		return scaleInt64(sb.ResourceWithOverHead.HostMemQ.Value(), factor),
			scaleInt64(sb.ResourceWithOverHead.HostCpuQ.MilliValue(), factor)
	}
	return sb.ResourceWithOverHead.HostMemQ.Value(), sb.ResourceWithOverHead.HostCpuQ.MilliValue()
}
