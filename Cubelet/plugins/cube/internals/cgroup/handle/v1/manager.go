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
	"sync"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/hashicorp/go-multierror"
	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type handler struct {
	hierarchy cgroup1.Hierarchy
	root      string
	baseGroup string
	usePoolV2 bool

	// subsystems resolved once; the mount topology does not change at
	// runtime.
	subsysOnce sync.Once
	subsys     []cgroup1.Subsystem
	subsysErr  error
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
		cgroup1.WithHiearchy(hierarchy),
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
		cgroup1.WithHiearchy(hierarchy),
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
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHiearchy(h.hierarchy))
	if err != nil {
		if err != cgroup1.ErrCgroupDeleted {
			return err
		} else {
			cubelog.Infof("cgroup %s not exist while update,create new cgroup ")
			_, err = cgroup1.New(cgroup1.StaticPath(group), res, cgroup1.WithHiearchy(h.hierarchy))
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
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHiearchy(h.hierarchy))
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
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHiearchy(h.hierarchy))
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
	m, err := cgroup1.Load(cgroup1.StaticPath(group), cgroup1.WithHiearchy(h.hierarchy))
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

// UsageSnapshot reads only the files the snapshot consumes; cpuacct is
// optional and reads as zero when absent.
func (h *handler) UsageSnapshot(ctx context.Context, group string) (handle.UsageSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return handle.UsageSnapshot{}, err
	}
	cpuPath, err := h.subsystemPath(cgroup1.Cpu, group)
	if err != nil {
		return handle.UsageSnapshot{}, err
	}
	memPath, err := h.subsystemPath(cgroup1.Memory, group)
	if err != nil {
		return handle.UsageSnapshot{}, err
	}

	var usageTotal, usageUser, usageSystem uint64
	if cpuacctPath, acctErr := h.subsystemPath(cgroup1.Cpuacct, group); acctErr == nil {
		statKV, err := handle.ReadKVFile(cpuacctPath, "cpuacct.stat")
		if err != nil {
			return handle.UsageSnapshot{}, fmt.Errorf("read cgroup v1 usage for %q: %w", group, err)
		}
		usageUser = handle.CpuacctTicksToNanoseconds(statKV["user"])
		usageSystem = handle.CpuacctTicksToNanoseconds(statKV["system"])
		usageTotal, err = handle.ReadUintFile(cpuacctPath, "cpuacct.usage")
		if err != nil {
			return handle.UsageSnapshot{}, fmt.Errorf("read cgroup v1 usage for %q: %w", group, err)
		}
	}

	throttleKV, err := handle.ReadKVFile(cpuPath, "cpu.stat")
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("read cgroup v1 usage for %q: %w", group, err)
	}
	memUsage, err := handle.ReadUintFile(memPath, "memory.usage_in_bytes")
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("read cgroup v1 usage for %q: %w", group, err)
	}
	memLimit, err := handle.ReadUintFile(memPath, "memory.limit_in_bytes")
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("read cgroup v1 usage for %q: %w", group, err)
	}
	memFailcnt, err := handle.ReadUintFile(memPath, "memory.failcnt")
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("read cgroup v1 usage for %q: %w", group, err)
	}
	cpuLimit, err := h.readCPULimit(group)
	if err != nil {
		return handle.UsageSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return handle.UsageSnapshot{}, err
	}
	return usageSnapshotFromRaw(usageTotal, usageUser, usageSystem, throttleKV, memUsage, memLimit, memFailcnt, cpuLimit), nil
}

func (h *handler) readCPULimit(group string) (handle.CPULimit, error) {
	cpuPath, err := h.subsystemPath(cgroup1.Cpu, group)
	if err != nil {
		return handle.CPULimit{}, err
	}
	quotaBytes, err := os.ReadFile(path.Join(cpuPath, "cpu.cfs_quota_us"))
	if err != nil {
		return handle.CPULimit{}, fmt.Errorf("read cgroup v1 CPU quota for %q: %w", group, err)
	}
	periodBytes, err := os.ReadFile(path.Join(cpuPath, "cpu.cfs_period_us"))
	if err != nil {
		return handle.CPULimit{}, fmt.Errorf("read cgroup v1 CPU period for %q: %w", group, err)
	}
	period, err := strconv.ParseUint(strings.TrimSpace(string(periodBytes)), 10, 64)
	if err != nil || period == 0 {
		return handle.CPULimit{}, fmt.Errorf("parse cgroup v1 CPU period for %q: %q", group, strings.TrimSpace(string(periodBytes)))
	}
	quotaText := strings.TrimSpace(string(quotaBytes))
	if quotaText == "-1" {
		return handle.CPULimit{PeriodUS: period, Unlimited: true}, nil
	}
	quota, err := strconv.ParseUint(quotaText, 10, 64)
	if err != nil {
		return handle.CPULimit{}, fmt.Errorf("parse cgroup v1 CPU quota for %q: %q", group, quotaText)
	}
	return handle.CPULimit{QuotaUS: quota, PeriodUS: period}, nil
}

func (h *handler) resolveSubsystems() ([]cgroup1.Subsystem, error) {
	h.subsysOnce.Do(func() {
		h.subsys, h.subsysErr = h.hierarchy()
	})
	return h.subsys, h.subsysErr
}

func (h *handler) subsystemPath(name cgroup1.Name, group string) (string, error) {
	subsystems, err := h.resolveSubsystems()
	if err != nil {
		return "", fmt.Errorf("load cgroup v1 hierarchy: %w", err)
	}
	for _, subsystem := range subsystems {
		if subsystem.Name() != name {
			continue
		}
		pather, ok := subsystem.(interface{ Path(string) string })
		if !ok {
			return "", fmt.Errorf("cgroup v1 subsystem %q does not expose its path", name)
		}
		return pather.Path(group), nil
	}
	return "", fmt.Errorf("cgroup v1 subsystem %q is unavailable", name)
}

func usageSnapshotFromRaw(usageTotal, usageUser, usageSystem uint64, throttleKV map[string]uint64, memUsage, memLimit, memFailcnt uint64, cpuLimit handle.CPULimit) handle.UsageSnapshot {
	limit := handle.ResourceLimit{Value: memLimit}
	if handle.IsCgroupV1UnlimitedMemoryLimit(limit.Value) {
		limit.Value = 0
		limit.Unlimited = true
	}
	return handle.UsageSnapshot{
		CPUUsageTotalNS:          usageTotal,
		CPUUserTotalNS:           usageUser,
		CPUSystemTotalNS:         usageSystem,
		CPUThrottledTotalNS:      throttleKV["throttled_time"],
		CPUPeriodsTotal:          throttleKV["nr_periods"],
		CPUThrottledPeriodsTotal: throttleKV["nr_throttled"],
		CPULimit:                 cpuLimit,
		MemoryCurrentBytes:       memUsage,
		MemoryLimit:              limit,
		MemoryFailuresTotal:      memFailcnt,
	}
}
