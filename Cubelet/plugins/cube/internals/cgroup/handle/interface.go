// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package handle

import (
	"context"

	"k8s.io/apimachinery/pkg/api/resource"
)

type Interface interface {
	IsExist(ctx context.Context, group string) bool
	Create(ctx context.Context, group string) error
	CreateWithCpuSet(ctx context.Context, group string, cpuset string, numaId int) error
	Update(ctx context.Context, group string, cpu, mem resource.Quantity) error
	Delete(ctx context.Context, group string) error
	List() ([]string, error)
	ListSubdir(subdir string) ([]string, error)
	AddProc(group string, pid uint64) error
	RemoveLimit(ctx context.Context, group string) error
	CleanForReuse(ctx context.Context, group string) error
	GetAllocatedCpuNum(group string) int
	GetAllocatedMem(group string) int64
	UsageSnapshot(ctx context.Context, group string) (UsageSnapshot, error)
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
