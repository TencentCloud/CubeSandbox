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
	// CpusetDir returns the directory that carries cpuset.cpus / cpuset.mems
	// for group: the controller mount on cgroup v1, the cgroup itself on the
	// unified hierarchy. An error means that directory cannot be resolved at
	// all (no cpuset mount on v1); a backend without a per-controller mount
	// returns the cgroup path and lets the caller's read of the files be what
	// fails, which is the only check that can see a controller that is not
	// enabled for the subtree.
	CpusetDir(group string) (string, error)
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
