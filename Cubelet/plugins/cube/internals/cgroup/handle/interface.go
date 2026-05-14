// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package handle

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

type Interface interface {
	IsExist(ctx context.Context, group string) bool
	Create(ctx context.Context, group string) error
	CreateWithCpuSet(ctx context.Context, group string, cpuset string, numaId int) error
	Update(ctx context.Context, group string, cpu, mem, cpuBurst resource.Quantity) error
	Delete(ctx context.Context, group string) error
	List() ([]string, error)
	ListSubdir(subdir string) ([]string, error)
	AddProc(group string, pid uint64) error
	RemoveLimit(ctx context.Context, group string) error
	CleanForReuse(ctx context.Context, group string) error
	GetAllocatedCpuNum(group string) int
	GetAllocatedMem(group string) int64
}

const (
	RootMountPoint = "/sys/fs/cgroup"

	DefaultPathPoolV1 = "/cube_sandbox/sandbox"

	DefaultPathPoolV2                 = "/cube_sandbox_v2/sandbox"
	DefaultPoolV1SubtreeControlPrefix = "/cube_sandbox"
	DefaultPoolV2SubtreeControlPrefix = "/cube_sandbox_v2/sandbox/numa"
)

var (
	DefaultCPUPeriod  uint64 = 100000
	DefaultMCPUPeriod uint64 = 100
)

func CPUBurstQuota(cpu, cpuBurst resource.Quantity) (uint64, bool, error) {
	if cpuBurst.IsZero() {
		return 0, false, nil
	}
	if cpuBurst.Cmp(cpu) < 0 {
		return 0, false, fmt.Errorf("cpu burst %s must be >= cpu quota %s", cpuBurst.String(), cpu.String())
	}

	extra := cpuBurst.DeepCopy()
	extra.Sub(cpu)
	if extra.Cmp(cpu) > 0 {
		return 0, false, fmt.Errorf("cpu burst extra %s must be <= cpu quota %s", extra.String(), cpu.String())
	}
	return uint64(extra.MilliValue()) * DefaultMCPUPeriod, true, nil
}
