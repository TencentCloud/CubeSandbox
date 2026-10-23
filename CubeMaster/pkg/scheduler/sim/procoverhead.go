// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// procoverhead.go samples the schedsim process's own CPU and memory cost so
// performance mode reports the scheduling core's process-level overhead next
// to per-decision latencies. CPU and peak RSS come from procfs
// (/proc/self/stat and /proc/self/status, the same runtime procfs-read
// convention as pkg/templatecenter/image/util.go): on non-Linux builds or
// when procfs is unreadable the reads fail and the corresponding fields stay
// zero (omitted from JSON), while the Go heap gauges — runtime.ReadMemStats —
// work on every platform.

// ProcessOverhead is the schedsim process's resource cost over one round's
// event replay — the same window PerfSummary.WallSeconds measures.
type ProcessOverhead struct {
	// CPUSeconds is the process CPU time (user + system, /proc/self/stat
	// utime+stime) consumed during the replay window.
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	// AvgCPUCores is CPUSeconds / WallSeconds: the average number of CPU
	// cores the process kept busy over the replay (×100 = percent of one
	// core).
	AvgCPUCores float64 `json:"avg_cpu_cores,omitempty"`
	// PeakRSSMiB is the process peak resident set size (/proc/self/status
	// VmHWM) at the end of the replay, in MiB. VmHWM is a process-lifetime
	// high-water mark, so it upper-bounds the window's peak.
	PeakRSSMiB float64 `json:"peak_rss_mib,omitempty"`
	// HeapAllocMiB is the Go heap in use (runtime.MemStats.HeapAlloc) at the
	// end of the replay, in MiB.
	HeapAllocMiB float64 `json:"heap_alloc_mib"`
	// HeapSysMiB is the Go heap obtained from the OS
	// (runtime.MemStats.HeapSys) at the end of the replay, in MiB.
	HeapSysMiB float64 `json:"heap_sys_mib"`
}

// procUsageSample is one instantaneous read of the process counters. CPU time
// is a monotonic process-lifetime counter, so a window's cost is the delta
// between two samples.
type procUsageSample struct {
	cpuSeconds float64
	cpuOK      bool
	peakRSSKiB float64
	rssOK      bool
}

func sampleProcUsage() procUsageSample {
	var s procUsageSample
	s.cpuSeconds, s.cpuOK = readProcSelfCPUSeconds()
	s.peakRSSKiB, s.rssOK = readProcSelfPeakRSSKiB()
	return s
}

// overheadBetween folds the begin/end samples into a ProcessOverhead spanning
// the given wall-clock window.
func overheadBetween(begin, end procUsageSample, wall time.Duration) *ProcessOverhead {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	oh := &ProcessOverhead{
		HeapAllocMiB: float64(ms.HeapAlloc) / (1 << 20),
		HeapSysMiB:   float64(ms.HeapSys) / (1 << 20),
	}
	if begin.cpuOK && end.cpuOK {
		oh.CPUSeconds = end.cpuSeconds - begin.cpuSeconds
		if wall > 0 {
			oh.AvgCPUCores = oh.CPUSeconds / wall.Seconds()
		}
	}
	if end.rssOK {
		oh.PeakRSSMiB = end.peakRSSKiB / 1024
	}
	return oh
}

// linuxUserHZ is the unit of /proc/self/stat utime/stime. USER_HZ is 100 on
// every architecture CubeSandbox targets (x86_64, aarch64).
const linuxUserHZ = 100

// readProcSelfCPUSeconds parses /proc/self/stat for the process's cumulative
// user+system CPU time. The comm field may contain spaces and parentheses, so
// parsing resumes after the final ')'.
func readProcSelfCPUSeconds() (float64, bool) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	rest := string(data)
	if i := strings.LastIndex(rest, ")"); i >= 0 {
		rest = rest[i+1:]
	}
	fields := strings.Fields(rest)
	// fields[0] is state (stat field 3), so utime (field 14) is fields[11]
	// and stime (field 15) is fields[12].
	if len(fields) < 13 {
		return 0, false
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, false
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(utime+stime) / linuxUserHZ, true
}

// readProcSelfPeakRSSKiB parses VmHWM from /proc/self/status — the process's
// peak resident set size in KiB.
func readProcSelfPeakRSSKiB() (float64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return float64(kib), true
	}
	return 0, false
}
