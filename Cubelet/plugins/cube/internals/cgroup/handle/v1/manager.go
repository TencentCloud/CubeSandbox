// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package v1

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/hashicorp/go-multierror"
	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"
)

type handler struct {
	hierarchy cgroup1.Hierarchy
	root      string
	baseGroup string
	usePoolV2 bool
}

var _ handle.Interface = &handler{}

func NewV1Handle(poolVersion int) handle.Interface {
	path := ""
	usePoolV2 := poolVersion == 2
	if poolVersion == 1 {
		path = handle.DefaultPathPoolV1
	} else if poolVersion == 2 {
		path = handle.DefaultPathPoolV2
	} else {
		return nil
	}
	return &handler{
		hierarchy: cgroup1.Default,
		root:      handle.RootMountPoint,
		baseGroup: path,
		usePoolV2: usePoolV2,
	}
}

func (h *handler) IsExist(ctx context.Context, group string) bool {
	filepath := path.Join(h.root, "cpu", group)
	exist, _ := utils.DenExist(filepath)
	return exist
}

func (h *handler) Create(ctx context.Context, group string) error {
	var (
		hierarchy     cgroup1.Hierarchy
		linuxResource *specs.LinuxResources
	)
	if h.usePoolV2 {
		hierarchy = SingleSubsystem(h.hierarchy, []cgroup1.Name{cgroup1.Cpu, cgroup1.Memory, cgroup1.Cpuset})
		linuxResource = &specs.LinuxResources{
			CPU:    &specs.LinuxCPU{},
			Memory: &specs.LinuxMemory{},
		}
	} else {
		hierarchy = SingleSubsystem(h.hierarchy, []cgroup1.Name{cgroup1.Cpu, cgroup1.Memory})
		linuxResource = &specs.LinuxResources{
			CPU:    &specs.LinuxCPU{},
			Memory: &specs.LinuxMemory{},
		}
	}
	_, err := cgroup1.New(cgroup1.StaticPath(group), linuxResource,
		cgroup1.WithHierarchy(hierarchy),
	)
	return err
}

func (h *handler) CreateWithCpuSet(ctx context.Context, group string, cpuset string, numaId int) error {
	var (
		hierarchy     cgroup1.Hierarchy
		linuxResource *specs.LinuxResources
	)
	if h.usePoolV2 {
		hierarchy = SingleSubsystem(h.hierarchy, []cgroup1.Name{cgroup1.Cpu, cgroup1.Memory, cgroup1.Cpuset})
	} else {
		hierarchy = SingleSubsystem(h.hierarchy, []cgroup1.Name{cgroup1.Cpu, cgroup1.Memory})
	}
	linuxResource = &specs.LinuxResources{
		CPU: &specs.LinuxCPU{
			Cpus: cpuset,
			Mems: fmt.Sprintf("%d", numaId),
		},
		Memory: &specs.LinuxMemory{},
	}
	_, err := cgroup1.New(cgroup1.StaticPath(group), linuxResource,
		cgroup1.WithHierarchy(hierarchy),
	)
	return err
}

func SingleSubsystem(baseHierarchy cgroup1.Hierarchy, needSystemName []cgroup1.Name) cgroup1.Hierarchy {
	return func() ([]cgroup1.Subsystem, error) {
		subsystems, err := baseHierarchy()
		if err != nil {
			return nil, err
		}
		var res []cgroup1.Subsystem
		for _, s := range subsystems {
			for _, n := range needSystemName {
				if s.Name() == n {
					res = append(res, s)
				}
			}

		}
		if len(res) == 0 {
			return nil, fmt.Errorf("unable to find subsystem %s", needSystemName)
		}
		return res, nil
	}
}

func (h *handler) Update(ctx context.Context, group string, cpu, mem resource.Quantity) error {
	cpuQuota := cpu.MilliValue() * 100
	memMax := mem.Value()
	res := &specs.LinuxResources{
		CPU: &specs.LinuxCPU{
			Quota:  &cpuQuota,
			Period: &handle.DefaultCPUPeriod,
		}, Memory: &specs.LinuxMemory{
			Limit: &memMax,
		},
	}
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHierarchy(h.hierarchy))
	if err != nil {
		if err != cgroup1.ErrCgroupDeleted {
			return err
		} else {
			cubelog.Infof("cgroup %s not exist while update,create new cgroup ")
			_, err = cgroup1.New(cgroup1.StaticPath(group), res, cgroup1.WithHierarchy(h.hierarchy))
			if err != nil {
				return err
			}
		}
	}
	err = m.Update(res)
	if err != nil {
		return err
	}
	return nil
}

func (h *handler) Delete(ctx context.Context, group string) error {
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHierarchy(h.hierarchy))
	if err != nil {
		if err == cgroup1.ErrCgroupDeleted {
			return nil
		}
		return err
	}

	delay := 10 * time.Millisecond
	for i := 0; i < 3; i++ {
		if i != 0 {
			time.Sleep(delay)
			delay *= 2
		}

		if err = m.Delete(); err == nil {
			return nil
		}

		_ = tryKillProc(m)
	}

	return fmt.Errorf("unable to delete group %q: %w", group, err)
}

func tryKillProc(m cgroup1.Cgroup) error {
	procs, err := m.Processes(cgroup1.Cpu, true)
	if err != nil {
		return err
	}

	var result *multierror.Error
	for _, proc := range procs {
		if err = syscall.Kill(proc.Pid, syscall.SIGKILL); err != nil {
			result = multierror.Append(result, err)
		}
	}
	return result.ErrorOrNil()
}

func (h *handler) List() ([]string, error) {
	dirname := path.Join(h.root, "/memory", h.baseGroup)
	return utils.GetAllDirname(dirname)
}

func (h *handler) ListSubdir(subdir string) ([]string, error) {
	dirname := path.Join(h.root, "/memory", h.baseGroup, subdir)
	return utils.GetAllDirname(dirname)
}

func (h *handler) AddProc(group string, pid uint64) error {
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHierarchy(h.hierarchy))
	if err != nil {
		return err
	}
	return m.AddProc(pid)
}

func (h *handler) RemoveLimit(ctx context.Context, group string) error {
	cpuLimitFilePath := path.Join(h.root, "cpu", group, "cpu.cfs_quota_us")
	memLimitFilePath := path.Join(h.root, "memory", group, "memory.limit_in_bytes")
	if exist, _ := utils.DenExist(cpuLimitFilePath); exist {
		err := os.WriteFile(cpuLimitFilePath, []byte("-1"), 0644)
		if err != nil {
			log.G(ctx).Errorf("remove cpu limit error %v", err)
			return err
		}
	}
	if exist, _ := utils.DenExist(memLimitFilePath); exist {
		err := os.WriteFile(memLimitFilePath, []byte("-1"), 0644)
		if err != nil {
			log.G(ctx).Errorf("remove memory limit error %v", err)
			return err
		}
	}
	return nil
}

func (h *handler) cleanProc(ctx context.Context, group string) error {
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHierarchy(h.hierarchy))
	if err != nil {
		return err
	}

	delay := 10 * time.Millisecond
	for i := 0; i < 3; i++ {
		if i != 0 {
			time.Sleep(delay)
			delay *= 2
		}

		if err = tryKillProc(m); err == nil {
			return nil
		}
	}

	return fmt.Errorf("unable to kill proc of group %q: %w", group, err)
}

func (h *handler) CleanForReuse(ctx context.Context, group string) error {
	exists := h.IsExist(ctx, group)
	if !exists {
		err := h.Create(ctx, group)
		if err != nil {
			return err
		}
		return nil
	}
	err := h.cleanProc(ctx, group)
	if err != nil {
		return err
	}
	err = h.RemoveLimit(ctx, group)
	if err != nil {
		return err
	}
	return nil
}

func (h *handler) GetAllocatedCpuNum(group string) int {
	filepath := path.Join(h.root, "cpu", group, "cpu.cfs_quota_us")

	b, err := os.ReadFile(filepath)
	content := strings.TrimSpace(string(b))
	if err != nil {
		cubelog.Errorf("read cpu.cfs_quota_us err:%s", err)
		return 0
	}
	cpuQuota, err := strconv.Atoi(content)
	if err != nil {
		cubelog.Errorf("parse cpu.cfs_quota_us err:%s", err)
		return 0
	}
	if cpuQuota == -1 {
		return 0
	}
	return int(uint64(cpuQuota) / handle.DefaultMCPUPeriod)
}

func (h *handler) GetAllocatedMem(group string) int64 {
	filepath := path.Join(h.root, "memory", group, "memory.limit_in_bytes")

	b, err := os.ReadFile(filepath)
	memMax := strings.TrimSpace(string(b))
	if err != nil {
		cubelog.Errorf("read memory.limit_in_bytes err:%s", err)
		return 0
	}
	mem, err := strconv.Atoi(memMax)
	if err != nil {
		cubelog.Errorf("parse memory.limit_in_bytes err:%s", err)
		return 0
	}
	if mem == -1 {
		return 0
	}
	return int64(mem)
}
