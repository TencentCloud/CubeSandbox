// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"math"
	"testing"
)

func perNodeCreateLimit(totalLimitCreate, healthyNodes int64) int64 {
	return int64(math.Ceil(float64(totalLimitCreate) / float64(healthyNodes)))
}

func clusterLimit(healthyNodes, perNode, masterNodes int64) int64 {
	return int64(math.Ceil(float64(healthyNodes*perNode) / float64(masterNodes)))
}

func TestPerNodeCreateLimitRoundsUp(t *testing.T) {
	cases := []struct {
		total, nodes, want int64
	}{
		{10, 3, 4},
		{10, 5, 2},
		{9, 3, 3},
		{1, 3, 1},
		{100, 7, 15},
	}
	for _, tc := range cases {
		if got := perNodeCreateLimit(tc.total, tc.nodes); got != tc.want {
			t.Errorf("perNodeCreateLimit(%d, %d) = %d, want %d", tc.total, tc.nodes, got, tc.want)
		}
	}
}

func TestClusterLimitRoundsUp(t *testing.T) {
	cases := []struct {
		nodes, perNode, masters, want int64
	}{
		{4, 5, 3, 7},
		{3, 5, 3, 5},
		{1, 1, 3, 1},
		{10, 10, 3, 34},
	}
	for _, tc := range cases {
		got := clusterLimit(tc.nodes, tc.perNode, tc.masters)
		if got != tc.want {
			t.Errorf("clusterLimit(%d, %d, %d) = %d, want %d",
				tc.nodes, tc.perNode, tc.masters, got, tc.want)
		}
	}
}
