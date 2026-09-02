// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"math"
	"sort"
)

// CoefficientOfVariation returns stddev/mean over xs using the population
// standard deviation. An empty input or a zero mean (e.g. cluster completely
// idle) yields 0 — "no spread" — rather than NaN.
func CoefficientOfVariation(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(n)
	if mean == 0 {
		return 0
	}
	var sq float64
	for _, x := range xs {
		d := x - mean
		sq += d * d
	}
	return math.Sqrt(sq/float64(n)) / mean
}

// JainIndex returns Jain's fairness index (Σx)²/(n·Σx²) over per-node load
// rates: 1.0 means perfectly balanced, 1/n means maximally concentrated. An
// empty input or an all-zero input (nothing placed anywhere) is defined as
// perfectly balanced, i.e. 1.
func JainIndex(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 1
	}
	var sum, sq float64
	for _, x := range xs {
		sum += x
		sq += x * x
	}
	if sum == 0 || sq == 0 {
		return 1
	}
	return sum * sum / (float64(n) * sq)
}

// FragmentationRatio measures how much of the cluster's free CPU (or memory —
// the engine applies it per resource) is stranded in pieces too small for the
// largest request shape in the trace.
//
// Definition: let free_i be node i's schedulable free quantity (effective
// quota after overcommit minus allocated usage, the same quantity the
// cpu/mem filters test) and maxShape the largest request size over all trace
// requests. A node "cannot fit" the shape when free_i <= maxShape (mirroring
// the filter's strict free > req admission check). The ratio is
//
//	Σ free_i over unfit nodes / Σ free_i over all nodes
//
// 0 means every free unit sits on a node that could still take the biggest
// request; 1 means no free unit is usable for it. Total free of 0 (cluster
// full or empty quota) is defined as 0.
func FragmentationRatio(free []float64, maxShape float64) float64 {
	var total, stranded float64
	for _, f := range free {
		if f <= 0 {
			continue
		}
		total += f
		if f <= maxShape {
			stranded += f
		}
	}
	if total == 0 {
		return 0
	}
	return stranded / total
}

// Percentile returns the nearest-rank percentile of values: the smallest value
// v such that at least p% of samples are <= v. p is clamped to (0, 100].
// An empty input yields 0.
func Percentile(values []float64, p float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		p = 1
	}
	if p > 100 {
		p = 100
	}
	sorted := make([]float64, n)
	copy(sorted, values)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// Snapshot is the instantaneous cluster state sampled on the virtual clock
// after each event. All fields are ratios/counts that can be time-averaged
// linearly.
type Snapshot struct {
	CPUAllocRate       float64 // Σ used cpu / Σ raw cpu quota
	MemAllocRate       float64 // Σ used mem / Σ raw mem quota
	LoadCVCPU          float64 // CV of per-node cpu usage rate (used/raw quota)
	LoadCVMem          float64 // CV of per-node mem usage rate
	JainCPU            float64 // Jain index of per-node cpu usage rate
	JainMem            float64 // Jain index of per-node mem usage rate
	FragmentationRatio float64 // free CPU stranded on nodes that cannot fit the largest shape
	// FragmentationRatioMem is the memory-side analog: tracked separately
	// because either resource can bind first depending on the overcommit
	// ratios and the trace shape.
	FragmentationRatioMem float64
	ActiveNodes           float64 // nodes running >= 1 sandbox
	EmptyNodes            float64 // nodes running 0 sandboxes
}

// TimeWeightedAvg accumulates snapshots against the virtual clock: the state
// installed at time T is weighted by how long it persisted (until the next
// event), so a burst that lasted 1ms counts far less than a plateau that
// lasted an hour of simulated time.
type TimeWeightedAvg struct {
	lastTimeMs int64
	last       Snapshot
	hasLast    bool
	acc        Snapshot
	totalMs    int64
}

// Advance moves the virtual clock to nowMs, crediting the previously installed
// snapshot for the elapsed interval, and installs s as the state going forward.
// The first call only installs s (no time has elapsed yet). nowMs must be
// non-decreasing across calls.
func (t *TimeWeightedAvg) Advance(nowMs int64, s Snapshot) {
	if t.hasLast && nowMs > t.lastTimeMs {
		dt := float64(nowMs - t.lastTimeMs)
		t.acc.addScaled(t.last, dt)
		t.totalMs += nowMs - t.lastTimeMs
	}
	t.lastTimeMs = nowMs
	t.last = s
	t.hasLast = true
}

// Mean returns the time-weighted average snapshot. If no time elapsed between
// samples (totalMs == 0) the last installed snapshot is returned instead of a
// zero struct.
func (t *TimeWeightedAvg) Mean() Snapshot {
	if t.totalMs == 0 {
		return t.last
	}
	m := t.acc
	m.scale(1 / float64(t.totalMs))
	return m
}

func (s *Snapshot) addScaled(o Snapshot, k float64) {
	s.CPUAllocRate += o.CPUAllocRate * k
	s.MemAllocRate += o.MemAllocRate * k
	s.LoadCVCPU += o.LoadCVCPU * k
	s.LoadCVMem += o.LoadCVMem * k
	s.JainCPU += o.JainCPU * k
	s.JainMem += o.JainMem * k
	s.FragmentationRatio += o.FragmentationRatio * k
	s.FragmentationRatioMem += o.FragmentationRatioMem * k
	s.ActiveNodes += o.ActiveNodes * k
	s.EmptyNodes += o.EmptyNodes * k
}

func (s *Snapshot) scale(k float64) {
	s.CPUAllocRate *= k
	s.MemAllocRate *= k
	s.LoadCVCPU *= k
	s.LoadCVMem *= k
	s.JainCPU *= k
	s.JainMem *= k
	s.FragmentationRatio *= k
	s.FragmentationRatioMem *= k
	s.ActiveNodes *= k
	s.EmptyNodes *= k
}
