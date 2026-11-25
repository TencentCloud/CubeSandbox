// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"math"
	"sync"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/resourcesource"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	metrictype "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric/types"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
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
	return aggregateSandboxResources(
		l.cubeboxManger.List(),
		config.GetHostConf().Quota.PausedResourceReleaseRatio,
	)
}

// aggregateCacheTTL bounds how stale the cached node aggregate may be for the
// resume-admission fast path. The scan behind it is O(rows) — 58ms p99 at
// N=5000 measured on a production-shaped store — and admission used to run it
// while holding the node-wide reservation ledger lock, serializing every
// resume on the node behind that scan. A burst of concurrent resumes now
// shares one scan per TTL window; the ledger still makes check-and-register
// atomic, and in-flight resume demand is exact through the reservations, so
// the only staleness is other flows' Saves/Deletes landing within the window
// — bounded, and in the common direction conservative (a reservation covers
// its own Save until released).
const aggregateCacheTTL = 10 * time.Millisecond

// aggregateCache memoizes one aggregateAllocated view for aggregateCacheTTL.
// Its mutex is a leaf: admitResume takes it outside the ledger lock and no
// path takes the ledger lock while holding it, so the two can never deadlock.
//
// A resume release invalidates the cache (invalidateAggregateCache) BEFORE
// dropping its ledger entry: by release time the flow's Save has folded the
// demand into the store, so a view cached before that Save (still showing the
// sandbox as a T-fraction tombstone) must never be served to an admission
// that no longer sees the reservation either — that pair would under-count
// the node by r·F. gen counts the invalidations so admitResume, which reads
// the view outside the ledger lock, can revalidate under it: a view whose gen
// is stale is re-read inside the critical section, closing the interleaving
// where the release lands between the two reads.
type aggregateCache struct {
	mu    sync.Mutex
	view  aggregatedSandboxView
	at    time.Time
	valid bool
	gen   uint64
}

// aggregateCacheGeneration is the invalidation counter admitResume
// revalidates against under the ledger lock.
func (l *local) aggregateCacheGeneration() uint64 {
	l.aggregateCache.mu.Lock()
	defer l.aggregateCache.mu.Unlock()
	return l.aggregateCache.gen
}

// invalidateAggregateCache drops the cached view so the next read rescans the
// store. Called by the resume release closure; see aggregateCache.
func (l *local) invalidateAggregateCache() {
	c := &l.aggregateCache
	c.mu.Lock()
	c.valid = false
	c.gen++
	c.mu.Unlock()
}

// aggregateAllocatedCached is aggregateAllocated with a TTL cache, for the
// resume-admission critical section. It also returns the cache's invalidation
// generation: a caller that reads the view outside a lock can revalidate
// under it (see admitResume). Heartbeat and metric collectors keep calling
// aggregateAllocated directly: they run once a second and report to
// CubeMaster, where a fresher view is worth the scan.
func (l *local) aggregateAllocatedCached() (aggregatedSandboxView, uint64) {
	c := &l.aggregateCache
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.valid && now.Sub(c.at) < aggregateCacheTTL {
		return c.view, c.gen
	}
	view := l.aggregateAllocated()
	c.view = view
	c.at = now
	c.valid = true
	return view, c.gen
}

// clampRatio bounds the release ratio to [0,1], the operator's
// density<->resume-headroom dial: 0 reserves everything (legacy, resume
// guaranteed), 1 releases everything (max density, resume best-effort), and a
// value in (0,1) releases that fraction while reserving the rest as headroom.
// Out-of-range and non-finite inputs are clamped to the safe extremes
// (NaN/-Inf -> 0, +Inf -> 1). The NaN guard is load-bearing: a malformed config
// value (e.g. ".nan" from a templating error) would otherwise flow into
// scaleInt64, where int64(v*NaN) collapses to math.MinInt64 and corrupts both
// the reported quota and the resume admission decision.
func clampRatio(r float64) float64 {
	if math.IsNaN(r) || r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

func scaleInt64(v int64, factor float64) int64 {
	return int64(float64(v) * factor)
}

// bytesToMB is the single byte->MiB conversion shared by the accounting side
// (aggregateSandboxResources) and the resume-admission side (admitResume), so
// both truncate to MB at exactly the same point. Keeping it in one place
// prevents the two paths from drifting on "scale-then-truncate" vs
// "truncate-then-scale" ordering.
func bytesToMB(b int64) int64 {
	return b / 1024 / 1024
}

// aggregateSandboxResources is the pure accounting kernel behind
// aggregateAllocated, split out so the release-ratio policy can be exercised
// directly in tests without standing up the full config/store stack.
//
// releaseRatio drives how paused/pausing sandboxes are accounted. When it is >0
// the policy is active: a paused sandbox counts only (1-releaseRatio) of its
// CPU/memory quota and never as running / NIC queues, because it has been
// snapshotted to disk and its MicroVM shut down, so it holds no live host
// CPU/RAM. Lowering its reported quota lets cubemaster's scheduler reuse the
// freed capacity, while a ratio in (0,1) keeps partial headroom so resume can
// still land and pause-heavy nodes stay visible to the quota scoring factors.
// Disk always counts (the pause snapshot occupies storage) and MvmNum always
// counts them (the sandbox object lives on). When releaseRatio is 0 the result
// is identical to the legacy behaviour: paused sandboxes keep their full quota
// and count as running, so resume is guaranteed.
func aggregateSandboxResources(sbs []*cubeboxstore.CubeBox, releaseRatio float64) aggregatedSandboxView {
	// Resolve the ratio once: factor is the paused-quota fraction still counted
	// (1-ratio), and the policy is active only for a strictly positive ratio.
	ratio := clampRatio(releaseRatio)
	factor := 1 - ratio
	policyActive := ratio > 0
	cpuMilli := int64(0)
	memBytes := int64(0)
	runningBox := int64(0)
	nicQueues := int64(0)
	dataDiskMB := int64(0)
	storageDiskMB := int64(0)
	for _, sb := range sbs {
		status := sb.GetStatus()
		if status == nil {
			continue
		}
		// Snapshot the status once (single RLock + copy) and reuse it for both
		// the good-state gate and the paused check below.
		state := status.Get().State()
		if !isContainerInGoodState(state) {
			continue
		}
		// A paused sandbox under the policy keeps only `factor` of its quota.
		paused := policyActive &&
			(state == cubebox.ContainerState_CONTAINER_PAUSED ||
				state == cubebox.ContainerState_CONTAINER_PAUSING)

		if sb.ResourceWithOverHead != nil {
			if paused {
				cpuMilli += scaleInt64(sb.ResourceWithOverHead.HostCpuQ.MilliValue(), factor)
				memBytes += scaleInt64(sb.ResourceWithOverHead.HostMemQ.Value(), factor)
			} else {
				cpuMilli += sb.ResourceWithOverHead.HostCpuQ.MilliValue()
				memBytes += sb.ResourceWithOverHead.HostMemQ.Value()
			}
			dataDiskMB += sb.ResourceWithOverHead.HostDataDiskMB
			storageDiskMB += sb.ResourceWithOverHead.HostStorageDiskMB
		}
		if paused {
			// Not running: a paused VM holds no live vCPU / NIC queues
			// regardless of reserveRatio (the ratio only governs quota
			// headroom, not liveness).
			continue
		}
		runningBox++
		nicQueues += sb.Queues
	}
	return aggregatedSandboxView{
		MilliCPU:      cpuMilli,
		MemoryMB:      bytesToMB(memBytes),
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
	metrics, err := storage.ObjectMetrics(context.Background())
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
