// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"syscall"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/resourcesource"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	metrictype "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric/types"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (l *local) RegisterMetrics(register *metrictype.CollectRegister) error {
	register.AddCollector(metrictype.MetricTypeCLS, func() (any, error) {
		var traces []*CubeLog.RequestTrace
		sbs := l.cubeboxManger.List()
		traces = append(traces, &CubeLog.RequestTrace{
			Action:  "MvmTotal",
			Callee:  constants.CubeboxID.ID(),
			RetCode: int64(len(sbs)),
		})

		traces = append(traces, &CubeLog.RequestTrace{
			Action:  "MvmDead",
			Callee:  constants.CubeboxID.ID(),
			RetCode: int64(deadContainerCount),
		})

		hostConf := config.GetHostConf()

		allocatePercent := -1
		if hostConf.Quota.MvmLimit > 0 {
			allocatePercent = len(sbs) * 100 / hostConf.Quota.MvmLimit
		}

		traces = append(traces, &CubeLog.RequestTrace{
			Action:  "MvmAllocatePercent",
			Callee:  constants.CubeboxID.ID(),
			RetCode: int64(allocatePercent),
		})

		cpuUsage := resource.MustParse("0")
		memUsage := resource.MustParse("0")
		nicQueues := int64(0)
		for _, sb := range sbs {
			if sb.GetStatus() == nil || !isContainerInGoodState(sb.GetStatus().Get().State()) {
				continue
			}

			if sb.ResourceWithOverHead != nil {
				cpuUsage.Add(sb.ResourceWithOverHead.HostCpuQ)
				memUsage.Add(sb.ResourceWithOverHead.HostMemQ)
			}
			nicQueues += sb.Queues
		}

		if cpuQuota := hostConf.Quota.Cpu; cpuQuota > 0 {
			cpuRate := float64(cpuUsage.MilliValue()) / float64(cpuQuota) * 100
			traces = append(traces, &CubeLog.RequestTrace{
				Action:  "CpuUsagePercent",
				Callee:  constants.CgroupID.ID(),
				RetCode: int64(cpuRate),
			})
		}

		memQuota, err := resource.ParseQuantity(hostConf.Quota.Mem)
		if err == nil {
			memRate := float64(memUsage.Value()) / float64(memQuota.Value()) * 100
			traces = append(traces, &CubeLog.RequestTrace{
				Action:  "MemUsagePercent",
				Callee:  constants.CgroupID.ID(),
				RetCode: int64(memRate),
			})
		}

		traces = append(traces, &CubeLog.RequestTrace{
			Action:  "NicQueues",
			Callee:  constants.CubeboxID.ID(),
			RetCode: nicQueues,
		})
		return traces, nil
	})
	register.AddCollector(metrictype.MetricTypeOSS, func() (any, error) {
		return l.collectOSSMetrics(), nil
	})
	return nil
}

func (l *local) collectOSSMetrics() map[string]any {
	alloc := l.aggregateAllocated()
	return map[string]any{
		"quota_cpu_usage":    int(alloc.MilliCPU),
		"quota_mem_mb_usage": alloc.MemoryMB,
		"mvm_num":            int(alloc.MvmNum),
		"mvm_running_num":    int(alloc.MvmRunningNum),
		"nic_queues":         alloc.NicQueues,
	}
}

// aggregatedSandboxView is the shared kernel between collectOSSMetrics and
// CollectAllocated: both need exactly the same accounting rules so the OSS
// trace pipeline and the cubemaster heartbeat report cannot disagree about
// what is "allocated" on this node.
type aggregatedSandboxView struct {
	MilliCPU      int64
	MemoryMB      int64
	MvmNum        int64
	MvmRunningNum int64
	NicQueues     int64
	DataDiskMB    int64
	StorageDiskMB int64
}

func (l *local) aggregateAllocated() aggregatedSandboxView {
	cpuUsage := resource.MustParse("0")
	memUsage := resource.MustParse("0")
	sbs := l.cubeboxManger.List()
	runningBox := int64(0)
	nicQueues := int64(0)
	dataDiskMB := int64(0)
	storageDiskMB := int64(0)
	for _, sb := range sbs {
		if sb.GetStatus() == nil || !isContainerInGoodState(sb.GetStatus().Get().State()) {
			continue
		}
		runningBox++

		if sb.ResourceWithOverHead != nil {
			cpuUsage.Add(sb.ResourceWithOverHead.HostCpuQ)
			memUsage.Add(sb.ResourceWithOverHead.HostMemQ)
			dataDiskMB += sb.ResourceWithOverHead.HostDataDiskMB
			storageDiskMB += sb.ResourceWithOverHead.HostStorageDiskMB
		}
		nicQueues += sb.Queues
	}
	return aggregatedSandboxView{
		MilliCPU:      cpuUsage.MilliValue(),
		MemoryMB:      memUsage.Value() / 1024 / 1024,
		MvmNum:        int64(len(sbs)),
		MvmRunningNum: runningBox,
		NicQueues:     nicQueues,
		DataDiskMB:    dataDiskMB,
		StorageDiskMB: storageDiskMB,
	}
}

// CollectAllocated implements resourcesource.Collector. The heartbeat path
// invokes this on the static node_status_update_frequency configured for
// the cubelet controller plugin. Returning a non-nil value even when no
// sandboxes are resident is intentional: cubemaster needs to know "this
// cubelet has 0 committed resources" to keep MetricUpdate fresh and avoid
// scheduler timeouts.
func (l *local) CollectAllocated() *resourcesource.AllocatedResources {
	v := l.aggregateAllocated()
	return &resourcesource.AllocatedResources{
		MilliCPU:      v.MilliCPU,
		MemoryMB:      v.MemoryMB,
		MvmNum:        v.MvmNum,
		MvmRunningNum: v.MvmRunningNum,
		NicQueues:     v.NicQueues,
		DataDiskMB:    v.DataDiskMB,
		StorageDiskMB: v.StorageDiskMB,
	}
}

// CollectDiskUsage produces filesystem-level fill ratios for the dimensions
// the scheduler filters care about. The cubecow pool feeds storage_disk;
// the data and system filesystems are observed via statfs on canonical
// paths chosen to match the cubelet config defaults. Per-dimension errors
// are logged but never propagated, because a missing mount must not stop
// CPU/Memory accounting from reaching cubemaster.
func (l *local) CollectDiskUsage() *resourcesource.DiskUsage {
	out := &resourcesource.DiskUsage{}
	out.SysDiskUsagePer = filesystemFillPercent("/")
	out.DataDiskUsagePer = filesystemFillPercent(cubeletDataDiskPath)
	if storage.IsCowBackend() {
		if pct, ok := cubecowFillPercent(); ok {
			out.StorageDiskUsagePer = pct
		} else {
			out.StorageDiskUsagePer = filesystemFillPercent(cubeletStorageDiskPath)
		}
	} else {
		out.StorageDiskUsagePer = filesystemFillPercent(cubeletStorageDiskPath)
	}
	return out
}

// Canonical fallback paths. These mirror the values shipped in
// Cubelet/config/config.toml (root="/data/cubelet/root",
// storage data_path="/data/cubelet/storage"). When the operator overrides
// them the statfs simply falls back to the parent mount, which is still a
// reasonable proxy.
const (
	cubeletDataDiskPath    = "/data/cubelet"
	cubeletStorageDiskPath = "/data/cubelet/storage"
)

func filesystemFillPercent(path string) float64 {
	if path == "" {
		return 0
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		log.G(context.Background()).Debugf("statfs %s failed: %v", path, err)
		return 0
	}
	total := st.Blocks * uint64(st.Bsize)
	if total == 0 {
		return 0
	}
	free := st.Bavail * uint64(st.Bsize)
	if free > total {
		return 0
	}
	used := total - free
	return float64(used) * 100 / float64(total)
}

func cubecowFillPercent() (float64, bool) {
	metrics, err := storage.GetCowMetrics(context.Background())
	if err != nil {
		log.G(context.Background()).Debugf("get cubecow metrics failed: %v", err)
		return 0, false
	}
	total, ok := metrics["total_bytes"]
	if !ok || total == 0 {
		return 0, false
	}
	used := metrics["used_bytes"]
	if used > total {
		return 100, true
	}
	return float64(used) * 100 / float64(total), true
}

func isContainerInGoodState(state cubebox.ContainerState) bool {
	if state == cubebox.ContainerState_CONTAINER_RUNNING ||
		state == cubebox.ContainerState_CONTAINER_PAUSED ||
		state == cubebox.ContainerState_CONTAINER_CREATED ||
		state == cubebox.ContainerState_CONTAINER_PAUSING {
		return true
	}
	return false
}
