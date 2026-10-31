// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/hostquota"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

var errAdmissionDuplicate = errors.New("sandbox already exists during admission")

// Missing runtime metadata belongs to the conservative MicroVM path. Both
// runtimes populate OciRuntime; only an explicit non-Cube type opts out.
func isOCISandbox(sb *cubeboxstore.CubeBox) bool {
	return sb != nil && sb.OciRuntime != nil && sb.OciRuntime.Type != "" &&
		!strings.HasPrefix(sb.OciRuntime.Type, "io.containerd.cube")
}

func (l *local) admitAndSave(ctx context.Context, sb *cubeboxstore.CubeBox, replacing ...bool) error {
	// OCI/runc containers do not use the MicroVM overhead/admission contract.
	if isOCISandbox(sb) {
		if err := l.cubeboxManger.Save(ctx, sb, cubes.WithNoEvent); err != nil {
			return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
		}
		return nil
	}
	host := config.GetHostConf()
	cpu := hostquota.CPU(host, runtime.NumCPU())
	mem, err := hostquota.Memory(host, hostquota.DetectMemoryTotalMB)
	if err != nil || cpu <= 0 || mem <= 0 {
		return ret.Errorf(errorcode.ErrorCode_HostResourcesNotEnough, "cannot determine host quota: cpu=%d memory=%d error=%v", cpu, mem, err)
	}
	return l.admitAndSaveWithQuota(ctx, sb, host, cpu, mem, replacing...)
}

func (l *local) admitAndSaveWithQuota(ctx context.Context, sb *cubeboxstore.CubeBox, host *config.HostConf, cpu, mem int64, replacing ...bool) error {
	l.admissionMu.Lock()
	defer l.admissionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	existing := l.cubeboxManger.List()
	if sb != nil && len(replacing) > 0 && replacing[0] {
		// Restart retains this sandbox's metadata after its old VM is destroyed.
		// Replace its allocation once while still checking every other sandbox.
		remaining := make([]*cubeboxstore.CubeBox, 0, len(existing))
		for _, current := range existing {
			if current.ID != sb.ID {
				remaining = append(remaining, current)
			}
		}
		existing = remaining
	}
	if host == nil {
		host = &config.HostConf{}
	}
	if err := checkCreateQuota(existing, sb, host, cpu, mem); err != nil {
		if errors.Is(err, errAdmissionDuplicate) {
			return ret.Err(errorcode.ErrorCode_Conflict, err.Error())
		}
		return ret.Err(errorcode.ErrorCode_HostResourcesNotEnough, err.Error())
	}
	// CREATED records count as allocated before any VM starts. Failed creates use
	// the ordinary workflow cleanup, so no separate reservation can leak.
	if err := l.cubeboxManger.Save(ctx, sb, cubes.WithNoEvent); err != nil {
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}
	return nil
}

func checkCreateQuota(existing []*cubeboxstore.CubeBox, incoming *cubeboxstore.CubeBox, host *config.HostConf, cpu, memMB int64) error {
	if incoming == nil || incoming.ResourceWithOverHead == nil {
		return fmt.Errorf("sandbox host resource demand is missing")
	}
	r := incoming.ResourceWithOverHead
	wantCPU, wantMem := r.HostCpuQ.MilliValue(), r.HostMemQ.Value()
	if wantCPU <= 0 || wantMem <= 0 {
		return fmt.Errorf("sandbox host resource demand must be positive")
	}
	// Missing metadata must not silently make a live sandbox free of charge.
	for _, sb := range existing {
		if sb.ID == incoming.ID {
			return fmt.Errorf("%w: %s", errAdmissionDuplicate, incoming.ID)
		}
		if status := sb.GetStatus(); status != nil && isContainerInGoodState(status.Get().State()) && sb.ResourceWithOverHead == nil && !isOCISandbox(sb) {
			return fmt.Errorf("existing sandbox %s has no host resource demand", sb.ID)
		}
	}
	used := aggregateSandboxResources(existing, host.Quota.PausedResourceReleaseRatio)
	// Compare bytes before conversion so fractional MiB cannot escape the fence.
	wantMemMB := (wantMem + 1024*1024 - 1) / (1024 * 1024)
	if wantCPU > cpu-used.MilliCPU || wantMem > memMB*1024*1024-used.MemoryBytes ||
		(host.Quota.MvmLimit > 0 && int64(len(existing)) >= int64(host.Quota.MvmLimit)) {
		return fmt.Errorf("host quota exhausted: allocated_cpu=%d requested_cpu=%d quota_cpu=%d allocated_memory_mib=%d requested_memory_mib=%d quota_memory_mib=%d instances=%d limit=%d",
			used.MilliCPU, wantCPU, cpu, used.MemoryMB, wantMemMB, memMB, len(existing), host.Quota.MvmLimit)
	}
	return nil
}
