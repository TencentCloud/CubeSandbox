// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"bufio"
	"fmt"
	"math"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Host quota fallbacks. Node registration reports these as the node's
// Capacity/Allocatable, and resume admission must compare against the very
// same numbers. Keeping the resolution here (rather than in one of its two
// consumers) is what stops the two paths from drifting when the operator
// leaves mcpu_limit/mem_limit unset.
const (
	DefaultHostQuotaCPUOvercommitFactor int64 = 2
	DefaultHostQuotaMemNumerator        int64 = 5
	DefaultHostQuotaMemDenominator      int64 = 4
)

// Production sources for the host-derived fallbacks. They are injectable so
// node-registration tests can use deterministic host sizes.
var (
	HostCPUCount          = func() int { return goruntime.NumCPU() }
	ReadHostMemoryTotalMB = DetectHostMemoryTotalMB
)

// ParseMemMB parses a Kubernetes quantity into whole MiB. An empty or
// unparsable value means "unset" (0), not "unbounded" by itself: callers
// decide whether to fall back to host capacity.
func ParseMemMB(value string) int64 {
	if value == "" {
		return 0
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	return q.Value() / (1024 * 1024)
}

// RoundUpFraction returns ceil(value*numerator/denominator), or 0 for
// non-positive inputs.
func RoundUpFraction(value, numerator, denominator int64) int64 {
	if value <= 0 || numerator <= 0 || denominator <= 0 {
		return 0
	}
	return (value*numerator + denominator - 1) / denominator
}

// ScaleCeilMB computes ceil(memMB * factor) without the float overshoot that
// can bump the result by 1MiB for decimal factors (e.g. 102400 * 1.1).
func ScaleCeilMB(memMB int64, factor float64) int64 {
	const eps = 1e-9
	return int64(math.Ceil(float64(memMB)*factor - eps))
}

// EffectiveCPUOvercommitFactor is the CPU multiplier used when the quota is
// derived from the host CPU count. Unset / non-positive keeps the historical
// 2.0x default; an explicit mcpu_limit bypasses it entirely.
func EffectiveCPUOvercommitFactor(q HostConfigQuota) float64 {
	if q.CpuOvercommitFactor > 0 {
		return q.CpuOvercommitFactor
	}
	return float64(DefaultHostQuotaCPUOvercommitFactor)
}

// EffectiveMemOvercommitFactor is the memory multiplier used when the quota is
// derived from host MemTotal. Unset / non-positive keeps the historical 1.25x
// (5/4) default; an explicit mem_limit bypasses it entirely.
func EffectiveMemOvercommitFactor(q HostConfigQuota) float64 {
	if q.MemOvercommitFactor > 0 {
		return q.MemOvercommitFactor
	}
	return float64(DefaultHostQuotaMemNumerator) / float64(DefaultHostQuotaMemDenominator)
}

// DetectHostMemoryTotalMB reads MemTotal from /proc/meminfo. It is Linux-only
// in practice; on other platforms the error simply leaves the memory quota
// unbounded, which matches the pre-existing behaviour.
func DetectHostMemoryTotalMB() (int64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
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
		return RoundUpFraction(memKB, 1, 1024), nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// CPUQuotaMilli resolves the node CPU quota in this order: configured
// mcpu_limit, then the caller's fallbacks (allocatable/capacity), then a
// host-CPU-derived default scaled by the effective overcommit factor. A
// non-positive result means "unbounded".
func CPUQuotaMilli(q HostConfigQuota, cpuCount func() int, fallbacks ...int64) int64 {
	if q.Cpu > 0 {
		return int64(q.Cpu)
	}
	for _, fallback := range fallbacks {
		if fallback > 0 {
			return fallback
		}
	}
	if cpuCount == nil {
		return 0
	}
	n := cpuCount()
	if n <= 0 {
		return 0
	}
	return int64(math.Round(float64(n) * 1000 * EffectiveCPUOvercommitFactor(q)))
}

// MemQuotaMB resolves the node memory quota in MiB in this order: configured
// mem_limit, then the caller's fallbacks, then a host-RAM-derived default
// scaled by the effective overcommit factor. A non-positive result means
// "unbounded".
func MemQuotaMB(q HostConfigQuota, hostMem func() (int64, error), fallbacks ...int64) int64 {
	if memMB := ParseMemMB(q.Mem); memMB > 0 {
		return memMB
	}
	for _, fallback := range fallbacks {
		if fallback > 0 {
			return fallback
		}
	}
	if hostMem == nil {
		return 0
	}
	memMB, err := hostMem()
	if err != nil || memMB <= 0 {
		return 0
	}
	return ScaleCeilMB(memMB, EffectiveMemOvercommitFactor(q))
}

// ResolvedHostQuotaCPUMilli / ResolvedHostQuotaMemMB are the no-fallback
// variants used outside node registration: they resolve to the same values
// applyHostQuota writes into Capacity/Allocatable.
func ResolvedHostQuotaCPUMilli(q HostConfigQuota) int64 {
	return CPUQuotaMilli(q, HostCPUCount)
}

func ResolvedHostQuotaMemMB(q HostConfigQuota) int64 {
	return MemQuotaMB(q, ReadHostMemoryTotalMB)
}
