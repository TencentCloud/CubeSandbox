// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//

package cubebox

import (
	"context"
	"errors"

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

	l.resumeAdmitMu.Lock()
	defer l.resumeAdmitMu.Unlock()

	sb, err := l.cubeboxManger.Get(ctx, sandboxID)
	if err != nil && !errors.Is(err, utils.ErrorKeyNotFound) {
		log.G(ctx).Warnf("admitAndReserveResume reject sandbox=%s: read local record: %v", sandboxID, err)
		return noop, &errorcode.Ret{
			RetCode: errorcode.ErrorCode_Conflict,
			RetMsg:  "resume rejected by paused_resource_release_ratio policy: cannot read sandbox state",
		}
	}
	if sb == nil || sb.SandboxID != sandboxID {
		return noop, nil
	}
	if st := sb.GetStatus(); st == nil || st.Get().State() != cubebox.ContainerState_CONTAINER_PAUSED {
		return noop, nil
	}

	in, state := resumeAdmissionInputs(sb)
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

	// Live read: this paused record already counts at the reserved (1-ratio)
	// fraction, and other in-flight reservations go on top so a burst cannot
	// all pass against the same snapshot.
	alloc := l.aggregateAllocated()
	reservedMem, reservedCPU := l.reservedResumeDemandLocked(sandboxID, in.releaseRatio)

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

func (l *local) reservedResumeDemandLocked(excludeSandboxID string, releaseRatio float64) (memMB, cpuMilli int64) {
	if len(l.resumeReserved) == 0 {
		return 0, 0
	}
	byID := make(map[string]*cubeboxstore.CubeBox, len(l.resumeReserved)+1)
	for _, sb := range l.cubeboxManger.List() {
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
		factor := 1 - pausedReleaseRatioFor(sb, releaseRatio)
		return scaleInt64(sb.ResourceWithOverHead.HostMemQ.Value(), factor),
			scaleInt64(sb.ResourceWithOverHead.HostCpuQ.MilliValue(), factor)
	}
	return sb.ResourceWithOverHead.HostMemQ.Value(), sb.ResourceWithOverHead.HostCpuQ.MilliValue()
}
