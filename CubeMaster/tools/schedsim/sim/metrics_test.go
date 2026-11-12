// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"math"
	"testing"
)

func almostEq(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCoefficientOfVariation(t *testing.T) {
	// Constant input: no spread.
	almostEq(t, CoefficientOfVariation([]float64{2, 2, 2, 2}), 0)
	// Empty and all-zero inputs are defined as 0.
	almostEq(t, CoefficientOfVariation(nil), 0)
	almostEq(t, CoefficientOfVariation([]float64{0, 0}), 0)
	// [1,3]: mean=2, population stddev=1 -> cv=0.5.
	almostEq(t, CoefficientOfVariation([]float64{1, 3}), 0.5)
	// [1,2,3]: mean=2, var=(1+0+1)/3=2/3 -> cv=sqrt(2/3)/2.
	almostEq(t, CoefficientOfVariation([]float64{1, 2, 3}), math.Sqrt(2.0/3.0)/2)
}

func TestJainIndex(t *testing.T) {
	// Degenerate inputs are defined as perfectly balanced.
	almostEq(t, JainIndex(nil), 1)
	almostEq(t, JainIndex([]float64{0, 0, 0}), 1)
	// Perfect balance.
	almostEq(t, JainIndex([]float64{1, 1, 1, 1}), 1)
	// Single node always balances against itself.
	almostEq(t, JainIndex([]float64{5}), 1)
	// Full concentration on one of four nodes: (1)^2/(4*1)=0.25.
	almostEq(t, JainIndex([]float64{1, 0, 0, 0}), 0.25)
	// [1,2]: (3)^2/(2*5)=0.9.
	almostEq(t, JainIndex([]float64{1, 2}), 0.9)
}

func TestFragmentationRatio(t *testing.T) {
	// All nodes fit the biggest shape -> nothing stranded.
	almostEq(t, FragmentationRatio([]float64{10, 10, 10}, 8), 0)
	// free=[5,5,20], shape=8: unfit free=10 out of 30 -> 1/3.
	almostEq(t, FragmentationRatio([]float64{5, 5, 20}, 8), 1.0/3.0)
	// free == shape cannot fit (the cpu filter needs free > req).
	almostEq(t, FragmentationRatio([]float64{8}, 8), 1)
	// No free CPU at all -> defined as 0.
	almostEq(t, FragmentationRatio([]float64{0, 0}, 8), 0)
	almostEq(t, FragmentationRatio(nil, 8), 0)
	// Negative (over-allocated) free is ignored; [10] vs shape 20 -> all stranded.
	almostEq(t, FragmentationRatio([]float64{-5, 10}, 20), 1)
}

func TestPercentile(t *testing.T) {
	almostEq(t, Percentile(nil, 50), 0)
	v := []float64{4, 1, 3, 2}
	// Nearest rank over [1,2,3,4]: p50 -> rank ceil(2)=2 -> 2.
	almostEq(t, Percentile(v, 50), 2)
	// p95 -> rank ceil(3.8)=4 -> 4.
	almostEq(t, Percentile(v, 95), 4)
	almostEq(t, Percentile(v, 99), 4)
	almostEq(t, Percentile(v, 100), 4)
	// p is clamped away from 0.
	almostEq(t, Percentile(v, 0), 1)
	// Single sample is every percentile.
	almostEq(t, Percentile([]float64{7}, 50), 7)
	almostEq(t, Percentile([]float64{7}, 99), 7)
}

func TestTimeWeightedAvg(t *testing.T) {
	var tw TimeWeightedAvg
	// No samples at all: zero snapshot.
	almostEq(t, tw.Mean().CPUAllocRate, 0)

	// Install 0.5 at t=0, 1.0 at t=10, 0.0 at t=30:
	// mean cpu = (0.5*10 + 1.0*20) / 30 = 25/30.
	tw.Advance(0, Snapshot{CPUAllocRate: 0.5, ActiveNodes: 1})
	tw.Advance(10, Snapshot{CPUAllocRate: 1.0, ActiveNodes: 2})
	tw.Advance(30, Snapshot{CPUAllocRate: 0.0, ActiveNodes: 0})
	m := tw.Mean()
	almostEq(t, m.CPUAllocRate, 25.0/30.0)
	almostEq(t, m.ActiveNodes, (1.0*10+2.0*20)/30.0)

	// A state installed at the final timestamp has zero duration and does not
	// count; with no elapsed time at all, Mean falls back to the last state.
	var tw2 TimeWeightedAvg
	tw2.Advance(5, Snapshot{CPUAllocRate: 0.7})
	almostEq(t, tw2.Mean().CPUAllocRate, 0.7)
}
