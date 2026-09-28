// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package v2

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3/cgroup2"
	cgroup2stats "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/hashicorp/go-multierror"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const defaultDirPerm = 0755
const subtreeControl = "cgroup.subtree_control"

var defaultFilePerm = os.FileMode(0)

type handler struct {
	root      string
	baseGroup string
}

var _ handle.Interface = &handler{}

func NewV2Handle(poolVersion int) handle.Interface {
	path := ""
	if poolVersion == 1 {
		path = handle.DefaultPathPoolV1
	} else if poolVersion == 2 {
		path = handle.DefaultPathPoolV2
	} else {
		return nil
	}

	return &handler{
		root:      handle.RootMountPoint,
		baseGroup: path,
	}
}

func (h *handler) IsExist(ctx context.Context, group string) bool {
	filepath := path.Join(h.root, group)
	exist, _ := utils.DenExist(filepath)
	return exist
}

func (h *handler) Create(ctx context.Context, group string) error {
	return h.createWithResource(ctx, group, &cgroup2.Resources{
		CPU:    &cgroup2.CPU{},
		Memory: &cgroup2.Memory{},
	})
}

func (h *handler) CreateWithCpuSet(ctx context.Context, group string, cpuset string, numaId int) error {
	cpuResource := &cgroup2.CPU{}
	if cpuset != "" {

		cpuResource.Cpus = cpuset
		cpuResource.Mems = fmt.Sprintf("%d", numaId)
	}
	return h.createWithResource(ctx, group, &cgroup2.Resources{
		CPU:    cpuResource,
		Memory: &cgroup2.Memory{},
	})
}

func (h *handler) createWithResource(ctx context.Context, group string, res *cgroup2.Resources) error {

	if res == nil {
		return errors.New("resources reference is nil")
	}

	if err := cgroup2.VerifyGroupPath(group); err != nil {
		return fmt.Errorf("invalid group path %s: %v", group, err)
	}
	path := filepath.Join(h.root, group)
	if err := os.MkdirAll(path, defaultDirPerm); err != nil {
		return fmt.Errorf("failed to create directory for cgroup %s: %v", path, err)
	}

	controllers := cubeEnabledControllers(res)
	if err := toggleControllers(h.root, group, controllers, cgroup2.Enable); err != nil {
		return fmt.Errorf("failed to toggle controllers for group %s: %v", group, err)
	}

	m, err := cgroup2.Load(group, cgroup2.WithMountpoint(h.root))
	if err != nil {
		return err
	}

	if err := m.Update(res); err != nil {
		os.Remove(path)
		return fmt.Errorf("failed to update cgroup %s: %v", path, err)
	}

	return nil
}

func cubeEnabledControllers(r *cgroup2.Resources) (c []string) {
	if r.CPU != nil {
		c = append(c, "cpu")
		if r.CPU.Cpus != "" {
			c = append(c, "cpuset")
		}

	}
	if r.Memory != nil {
		c = append(c, "memory")
	}
	if r.Pids != nil {
		c = append(c, "pids")
	}
	if r.IO != nil {
		c = append(c, "io")
	}
	if r.RDMA != nil {
		c = append(c, "rdma")
	}
	if r.HugeTlb != nil {
		c = append(c, "hugetlb")
	}
	return
}

func toggleControllers(root string, group string, controllers []string, t cgroup2.ControllerToggle) error {

	path := filepath.Join(root, group)
	split := strings.Split(path, "/")
	var lastErr error
	for i := range split {
		f := strings.Join(split[:i], "/")
		if !strings.HasPrefix(f, root) || f == path {
			continue
		}
		crtls := controllers

		if t == cgroup2.Disable && f == root {

			for i, controller := range crtls {
				if controller == "cpuset" {
					crtls = append(crtls[:i], crtls[i+1:]...)
					break
				}
			}
		}
		filePath := filepath.Join(f, subtreeControl)
		if err := writeSubtreeControl(filePath, crtls, t); err != nil {

			lastErr = fmt.Errorf("failed to write subtree controllers %+v to %q: %w", crtls, filePath, err)
		} else {
			lastErr = nil
		}
	}
	return lastErr
}

func writeSubtreeControl(filePath string, controllers []string, t cgroup2.ControllerToggle) error {
	f, err := os.OpenFile(filePath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	switch t {
	case cgroup2.Enable:
		controllers = toggleFunc(controllers, "+")
	case cgroup2.Disable:
		controllers = toggleFunc(controllers, "-")
	}
	_, err = f.WriteString(strings.Join(controllers, " "))
	return err
}

func toggleFunc(controllers []string, prefix string) []string {
	out := make([]string, len(controllers))
	for i, c := range controllers {
		out[i] = prefix + c
	}
	return out
}

func (h *handler) Update(ctx context.Context, group string, cpu, mem resource.Quantity) error {
	cpuQuota := cpu.MilliValue() * 100
	memMax := mem.Value()
	cpuMax := cgroup2.NewCPUMax(&cpuQuota, &handle.DefaultCPUPeriod)

	res := &cgroup2.Resources{
		CPU:    &cgroup2.CPU{Max: cpuMax},
		Memory: &cgroup2.Memory{Max: &memMax},
	}
	m, err := cgroup2.Load(group, cgroup2.WithMountpoint(h.root))
	if err != nil {
		return err
	}
	cgroupv2Path := path.Join(h.root, group)
	if exist, _ := utils.DenExist(cgroupv2Path); exist {
		return m.Update(res)
	}

	log.G(ctx).Infof("cgroup %s not exist while update, create one", group)
	return h.createWithResource(ctx, group, res)
}

func (h *handler) Delete(ctx context.Context, group string) error {
	if _, err := os.Stat(path.Join(h.root, group)); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	m, err := cgroup2.Load(group, cgroup2.WithMountpoint(h.root))
	if err != nil {
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

func tryKillProc(m *cgroup2.Manager) error {
	procs, err := m.Procs(true)
	if err != nil {
		return err
	}

	var result *multierror.Error
	for _, proc := range procs {
		if err = syscall.Kill(int(proc), syscall.SIGKILL); err != nil {
			result = multierror.Append(result, err)
		}
	}
	return result.ErrorOrNil()
}

func (h *handler) List() ([]string, error) {
	dirname := path.Join(h.root, h.baseGroup)
	return utils.GetAllDirname(dirname)
}
func (h *handler) ListSubdir(subdir string) ([]string, error) {
	dirname := path.Join(h.root, h.baseGroup, subdir)
	return utils.GetAllDirname(dirname)
}

func (h *handler) AddProc(group string, pid uint64) error {
	m, err := cgroup2.Load(group, cgroup2.WithMountpoint(h.root))
	if err != nil {
		return err
	}
	return m.AddProc(pid)
}

func (h *handler) RemoveLimit(ctx context.Context, group string) error {
	cpuLimitFilePath := path.Join(h.root, group, "cpu.max")
	memLimitFilePath := path.Join(h.root, group, "memory.max")
	if exist, _ := utils.DenExist(cpuLimitFilePath); exist {
		err := os.WriteFile(cpuLimitFilePath, []byte("max 100000"), 0644)
		if err != nil {
			log.G(ctx).Errorf("remove cpu limit error %v", err)
			return err
		}
	}
	if exist, _ := utils.DenExist(memLimitFilePath); exist {
		err := os.WriteFile(memLimitFilePath, []byte("max"), 0644)
		if err != nil {
			log.G(ctx).Errorf("remove memory limit error %v", err)
			return err
		}
	}
	return nil
}

func (h *handler) cleanProc(ctx context.Context, group string) error {
	m, err := cgroup2.Load(group, cgroup2.WithMountpoint(h.root))
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
	filepath := path.Join(h.root, group, "cpu.max")

	b, err := os.ReadFile(filepath)
	content := strings.TrimSpace(string(b))
	if err != nil {
		cubelog.Errorf("read cpu.max err:%s", err)
		return 0
	}
	cpuMax := strings.Split(string(content), " ")
	if cpuMax[0] == "max" {
		return 0
	}
	cpuQuota, err := strconv.Atoi(cpuMax[0])
	if err != nil {
		cubelog.Errorf("parse cpu.max err:%s", err)
		return 0
	}
	return int(uint64(cpuQuota) / handle.DefaultMCPUPeriod)
}

func (h *handler) GetAllocatedMem(group string) int64 {
	filepath := path.Join(h.root, group, "memory.max")

	b, err := os.ReadFile(filepath)
	memMax := strings.TrimSpace(string(b))
	if err != nil {
		cubelog.Errorf("read memory.max err:%s", err)
		return 0
	}
	if memMax == "max" {
		return 0
	}
	mem, err := strconv.Atoi(memMax)
	if err != nil {
		cubelog.Errorf("parse memory.max err:%s", err)
		return 0
	}
	return int64(mem)
}

func (h *handler) UsageSnapshot(ctx context.Context, group string) (handle.UsageSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return handle.UsageSnapshot{}, err
	}
	m, err := cgroup2.Load(group, cgroup2.WithMountpoint(h.root))
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("load cgroup v2 %q: %w", group, err)
	}
	metrics, err := m.Stat()
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("read cgroup v2 usage for %q: %w", group, err)
	}
	cpuLimit, err := h.readCPULimit(group)
	if err != nil {
		return handle.UsageSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return handle.UsageSnapshot{}, err
	}
	return usageSnapshotFromMetrics(metrics, cpuLimit)
}

func (h *handler) readCPULimit(group string) (handle.CPULimit, error) {
	raw, err := os.ReadFile(path.Join(h.root, group, "cpu.max"))
	if err != nil {
		return handle.CPULimit{}, fmt.Errorf("read cgroup v2 CPU limit for %q: %w", group, err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return handle.CPULimit{}, fmt.Errorf("parse cgroup v2 CPU limit for %q: %q", group, strings.TrimSpace(string(raw)))
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || period == 0 {
		return handle.CPULimit{}, fmt.Errorf("parse cgroup v2 CPU period for %q: %q", group, fields[1])
	}
	if fields[0] == "max" {
		return handle.CPULimit{PeriodUS: period, Unlimited: true}, nil
	}
	quota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return handle.CPULimit{}, fmt.Errorf("parse cgroup v2 CPU quota for %q: %q", group, fields[0])
	}
	return handle.CPULimit{QuotaUS: quota, PeriodUS: period}, nil
}

func usageSnapshotFromMetrics(metrics *cgroup2stats.Metrics, cpuLimit handle.CPULimit) (handle.UsageSnapshot, error) {
	if metrics == nil || metrics.GetCPU() == nil {
		return handle.UsageSnapshot{}, fmt.Errorf("cgroup v2 CPU usage is incomplete")
	}
	if metrics.GetMemory() == nil {
		return handle.UsageSnapshot{}, fmt.Errorf("cgroup v2 memory usage is incomplete")
	}
	cpu := metrics.GetCPU()
	usage, err := microsecondsToNanoseconds(cpu.GetUsageUsec())
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("convert cgroup v2 CPU usage: %w", err)
	}
	user, err := microsecondsToNanoseconds(cpu.GetUserUsec())
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("convert cgroup v2 CPU user usage: %w", err)
	}
	system, err := microsecondsToNanoseconds(cpu.GetSystemUsec())
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("convert cgroup v2 CPU system usage: %w", err)
	}
	throttled, err := microsecondsToNanoseconds(cpu.GetThrottledUsec())
	if err != nil {
		return handle.UsageSnapshot{}, fmt.Errorf("convert cgroup v2 CPU throttled time: %w", err)
	}
	memory := metrics.GetMemory()
	limit := handle.ResourceLimit{Value: memory.GetUsageLimit()}
	if limit.Value == math.MaxUint64 {
		limit.Value = 0
		limit.Unlimited = true
	}
	events := metrics.GetMemoryEvents()
	return handle.UsageSnapshot{
		CPUUsageTotalNS:          usage,
		CPUUserTotalNS:           user,
		CPUSystemTotalNS:         system,
		CPUThrottledTotalNS:      throttled,
		CPUPeriodsTotal:          cpu.GetNrPeriods(),
		CPUThrottledPeriodsTotal: cpu.GetNrThrottled(),
		CPULimit:                 cpuLimit,
		MemoryCurrentBytes:       memory.GetUsage(),
		MemoryLimit:              limit,
		MemoryFailuresTotal:      events.GetMax(),
	}, nil
}

func microsecondsToNanoseconds(value uint64) (uint64, error) {
	if value > math.MaxUint64/1000 {
		return 0, fmt.Errorf("microsecond value %d overflows nanoseconds", value)
	}
	return value * 1000, nil
}
