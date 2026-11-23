// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package warmup decides how many cgroup slots the sandbox pool pre-creates.
//
// The pool is a latency optimisation: cgPool.Get falls back to creating a slot
// on demand, so pre-creating fewer slots only moves work from startup to the
// first sandbox that needs one. Pre-creating more than the host can afford is
// therefore never worth it.
//
// What a slot costs is not constant. On the unified hierarchy every cgroup in a
// subtree whose parent enabled the cpu controller carries per-CPU accounting
// structures, so the cost of one cgroup scales with the host's CPU count.
// Measured on a 384-CPU aarch64 host: five cgroups under a subtree with
// cpu+memory enabled took 327 MiB of kernel per-CPU memory (~65 MiB each),
// while the same five under a subtree with no controller enabled took none. A
// 3000-slot warm pool therefore reserves on the order of 200 GiB there, and
// because percpu pages are not returned to the buddy allocator when the objects
// are freed, the host stays short of memory until it is rebooted.
//
// The pool size used to follow max_mvm_num, i.e. the host's capacity
// (MemTotal * overcommit / 512 MiB). On a large-memory node that is thousands
// of slots, which is where the blow-up came from. Capacity and warm pool size
// are different questions: capacity is how many sandboxes may run, the pool is
// how many slots are worth keeping hot. This package answers the second one by
// measuring the host instead of assuming it.
package warmup

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

const (
	// DefaultMax bounds the warm pool when Max is not configured, so a large
	// max_mvm_num cannot translate into a large pool on a many-core host.
	// Concurrency above this is still served, just through the on-demand path.
	DefaultMax = 256
	// DefaultMemBudgetMB is how much kernel memory the warm pool may reserve by
	// default. It is deliberately small: the pool buys microseconds on create,
	// and anything that costs hundreds of megabytes at startup is a bad trade.
	DefaultMemBudgetMB = 256
	// DefaultMin keeps a few slots even when the measured cost is alarming, so
	// the common path of "start one sandbox" stays warm.
	DefaultMin = 2

	// ProcMeminfoPath is where the kernel reports its per-CPU footprint.
	ProcMeminfoPath = "/proc/meminfo"
)

// Limits are the operator-facing bounds on the warm pool.
type Limits struct {
	// Requested is what the pool would like to pre-create: pool_size, or
	// max_mvm_num when the scheduler reports one.
	Requested int
	// Max is a hard ceiling. A non-positive value disables it.
	Max int
	// Min is a floor. A non-positive value disables it.
	Min int
	// BudgetMB is the kernel-memory budget for the whole warm pool. A
	// non-positive value disables both the budget and cost-based scaling.
	BudgetMB int
}

// Unset reports whether no bound at all is configured. Callers then keep the
// historical behaviour, which also keeps hand-built pools in unit tests
// working unchanged.
func (l Limits) Unset() bool {
	return l.Max <= 0 && l.BudgetMB <= 0 && l.Min <= 0
}

// PercpuUsageBytes reads the kernel per-CPU allocator footprint from the
// "Percpu:" line of a meminfo file. ok is false on kernels that do not export
// the field (added in 5.13), in which case callers must fall back to the
// configured bounds only.
func PercpuUsageBytes(path string) (value uint64, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "Percpu:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false, nil
		}
		kb, perr := strconv.ParseUint(fields[1], 10, 64)
		if perr != nil {
			return 0, false, nil
		}
		return kb * 1024, true, nil
	}
	if serr := sc.Err(); serr != nil {
		return 0, false, serr
	}
	return 0, false, nil
}

// Resolve returns how many cgroups to pre-create and a short reason suitable
// for a log line. costPerCgroupBytes is what one slot measured on this host;
// costKnown is false when the kernel does not report Percpu and the budget
// therefore cannot be applied.
func Resolve(l Limits, costPerCgroupBytes uint64, costKnown bool) (size int, reason string) {
	size = l.Requested
	if size < 1 {
		size = 1
	}
	if l.Unset() {
		return size, "no bound configured"
	}

	if l.Max > 0 && size > l.Max {
		size = l.Max
		reason = "warm_pool_max"
	}

	if costKnown && l.BudgetMB > 0 && costPerCgroupBytes > 0 {
		budget := uint64(l.BudgetMB) * 1024 * 1024
		allowed := int(budget / costPerCgroupBytes)
		if allowed < 1 {
			// A single slot already exceeds the budget. Still keep one: the
			// first create is on the hot path of every host.
			allowed = 1
		}
		if size > allowed {
			size = allowed
			reason = "warm_pool_mem_budget_mb"
		}
	}

	if l.Min > 0 && size < l.Min {
		size = l.Min
		reason = "warm_pool_min"
	}

	if reason == "" {
		reason = "within bounds"
	}
	return size, reason
}
