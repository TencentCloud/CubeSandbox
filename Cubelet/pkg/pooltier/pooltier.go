// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package pooltier selects cgroup and TAP pool sizes from host CPU and memory.
package pooltier

import (
	"bufio"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
)

// Tier is one machine class plus the pool sizes it defaults to.
type Tier struct {
	// Name is the stable identifier used in startup logs.
	Name           string
	MinCPUs        int
	MinMemMB       int64
	CgroupPoolSize int
	TapInitNum     int
}

// tiers is ordered largest-first; Select returns the first tier whose CPU and
// memory bounds are both met.
var tiers = []Tier{
	{Name: "xlarge", MinCPUs: 128, MinMemMB: 690 * 1024, CgroupPoolSize: 5000, TapInitNum: 1000},
	{Name: "large", MinCPUs: 96, MinMemMB: 345 * 1024, CgroupPoolSize: 3000, TapInitNum: 500},
	{Name: "medium", MinCPUs: 32, MinMemMB: 115 * 1024, CgroupPoolSize: 1800, TapInitNum: 400},
	{Name: "small", MinCPUs: 16, MinMemMB: 28 * 1024, CgroupPoolSize: 1000, TapInitNum: 300},
	// minimal is the fallback tier. TapInitNum must stay nonzero.
	{Name: "minimal", CgroupPoolSize: 500, TapInitNum: 200},
}

// HostSpec is the measured capacity of the machine the pools are sized for.
type HostSpec struct {
	CPUs       int
	MemTotalMB int64
}

// Select returns the tier matching the given host capacity.
func Select(cpus int, memTotalMB int64) Tier {
	for _, t := range tiers {
		if cpus >= t.MinCPUs && memTotalMB >= t.MinMemMB {
			return t
		}
	}
	return tiers[len(tiers)-1]
}

// SelectForHost measures the host and returns the selected tier with its
// capacity. On failure it returns the floor tier and the error.
func SelectForHost() (Tier, HostSpec, error) {
	spec := HostSpec{CPUs: hostCPUCount()}
	memMB, err := readMemTotalMB()
	if err != nil {
		return Select(spec.CPUs, 0), spec, fmt.Errorf("detect host memory: %w", err)
	}
	spec.MemTotalMB = memMB
	return Select(spec.CPUs, memMB), spec, nil
}

// hostCPUCount and readMemTotalMB are test seams.
var (
	hostCPUCount = func() int {
		return goruntime.NumCPU()
	}
	readMemTotalMB = DetectMemTotalMB
)

// DetectMemTotalMB returns /proc/meminfo MemTotal in MiB, rounded up.
func DetectMemTotalMB() (int64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return parseMemTotalMB(file)
}

// parseMemTotalMB extracts the MemTotal line from a /proc/meminfo stream.
func parseMemTotalMB(r io.Reader) (int64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected MemTotal format: %q", line)
		}
		memKB, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal failed: %w", err)
		}
		return (memKB + 1023) / 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
