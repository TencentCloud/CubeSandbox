// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package score

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCalculateResourceFitScorePrefersTighterBalancedFit(t *testing.T) {
	// 节点 A：8 核/16 GB，已使用 4 核/8 GB。
	// 放入 2 核/4 GB 请求后，CPU 和内存都剩余 25%。
	tightFit := calculateResourceFitScore(
		8000, 4000, 2000,
		16384, 8192, 4096,
		0.5,
	)

	// 节点 B：8 核/16 GB，当前完全空闲。
	// 放入相同请求后，CPU 和内存都剩余 75%。
	looseFit := calculateResourceFitScore(
		8000, 0, 2000,
		16384, 0, 4096,
		0.5,
	)

	if tightFit <= looseFit {
		t.Fatalf(
			"tighter fit should receive a higher score: tight=%f loose=%f",
			tightFit,
			looseFit,
		)
	}
}

func TestCalculateResourceFitScorePenalizesImbalance(t *testing.T) {
	// 放入请求后，CPU 和内存都剩余 25%。
	balanced := calculateResourceFitScore(
		8000, 4000, 2000,
		16384, 8192, 4096,
		0.5,
	)

	// 放入请求后，CPU 剩余 0%，内存剩余 50%。
	// 总体剩余资源与 balanced 相近，但资源比例严重失衡。
	imbalanced := calculateResourceFitScore(
		8000, 6000, 2000,
		16384, 4096, 4096,
		0.5,
	)

	if balanced <= imbalanced {
		t.Fatalf(
			"balanced fit should receive a higher score: balanced=%f imbalanced=%f",
			balanced,
			imbalanced,
		)
	}
}

func TestCalculateResourceFitScoreRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string

		cpuCapacity int64
		cpuUsed     int64
		cpuRequest  int64

		memCapacity int64
		memUsed     int64
		memRequest  int64

		imbalancePenalty float64
	}{
		{
			name:             "zero cpu capacity",
			cpuCapacity:      0,
			memCapacity:      16384,
			imbalancePenalty: 0.5,
		},
		{
			name:             "negative usage",
			cpuCapacity:      8000,
			cpuUsed:          -1,
			memCapacity:      16384,
			imbalancePenalty: 0.5,
		},
		{
			name:             "cpu request exceeds remaining capacity",
			cpuCapacity:      8000,
			cpuUsed:          7000,
			cpuRequest:       2000,
			memCapacity:      16384,
			memUsed:          4096,
			memRequest:       4096,
			imbalancePenalty: 0.5,
		},
		{
			name:             "memory request exceeds remaining capacity",
			cpuCapacity:      8000,
			cpuUsed:          2000,
			cpuRequest:       2000,
			memCapacity:      16384,
			memUsed:          15000,
			memRequest:       4096,
			imbalancePenalty: 0.5,
		},
		{
			name:             "negative imbalance penalty",
			cpuCapacity:      8000,
			cpuRequest:       2000,
			memCapacity:      16384,
			memRequest:       4096,
			imbalancePenalty: -0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateResourceFitScore(
				tt.cpuCapacity,
				tt.cpuUsed,
				tt.cpuRequest,
				tt.memCapacity,
				tt.memUsed,
				tt.memRequest,
				tt.imbalancePenalty,
			)

			if got != 0 {
				t.Fatalf("invalid input should return zero score, got=%f", got)
			}
		})
	}
}

func TestResourceFitScoreMetadata(t *testing.T) {
	scorer := newResourceFitScore(0.8, 0.5, false)

	if scorer.ID() != "Score/resource_fit_score" {
		t.Fatalf("unexpected score ID: %s", scorer.ID())
	}

	if scorer.String() != scorer.ID() {
		t.Fatalf(
			"String should return ID: string=%s id=%s",
			scorer.String(),
			scorer.ID(),
		)
	}

	if scorer.Weight() != 0.8 {
		t.Fatalf("unexpected weight: %f", scorer.Weight())
	}

	if scorer.Disable() {
		t.Fatal("resource fit scorer should be enabled")
	}
}

func TestResourceFitScoreSelectPrefersTighterFit(t *testing.T) {
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.ReqRes = &selctx.RequestResource{
		Cpu: resource.MustParse("2"),
		Mem: resource.MustParse("4Gi"),
	}

	selCtx.SetNodes(node.NodeList{
		{
			InsID:         "tight-fit",
			QuotaCpu:      8000,
			QuotaMem:      16384,
			QuotaCpuUsage: 4000,
			QuotaMemUsage: 8192,
		},
		{
			InsID:         "loose-fit",
			QuotaCpu:      8000,
			QuotaMem:      16384,
			QuotaCpuUsage: 0,
			QuotaMemUsage: 0,
		},
	})

	scorer := newResourceFitScore(1.0, 0.5, false)

	scores, err := scorer.Select(selCtx)
	if err != nil {
		t.Fatalf("Select returned an unexpected error: %v", err)
	}

	if scores.Len() != 2 {
		t.Fatalf("expected 2 node scores, got %d", scores.Len())
	}

	if scores[0].InsID != "tight-fit" {
		t.Fatalf("unexpected first node: %s", scores[0].InsID)
	}

	if scores[0].Score <= scores[1].Score {
		t.Fatalf(
			"tighter node should receive a higher score: tight=%f loose=%f",
			scores[0].Score,
			scores[1].Score,
		)
	}
}

func TestResourceFitScoreSelectRejectsMissingRequest(t *testing.T) {
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{
		{
			InsID:    "node-1",
			QuotaCpu: 8000,
			QuotaMem: 16384,
		},
	})

	scorer := newResourceFitScore(1.0, 0.5, false)

	_, err := scorer.Select(selCtx)
	if err == nil {
		t.Fatal("Select should reject a missing resource request")
	}
}

func TestResourceFitScoreSelectSkipsWhenDisabled(t *testing.T) {
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.ReqRes = &selctx.RequestResource{
		Cpu: resource.MustParse("2"),
		Mem: resource.MustParse("4Gi"),
	}

	selCtx.SetNodes(node.NodeList{
		{
			InsID:    "node-1",
			QuotaCpu: 8000,
			QuotaMem: 16384,
		},
	})

	scorer := newResourceFitScore(1.0, 0.5, true)

	scores, err := scorer.Select(selCtx)
	if err != nil {
		t.Fatalf("disabled scorer returned an error: %v", err)
	}

	if scores != nil {
		t.Fatalf("disabled scorer should return nil scores, got %v", scores)
	}
}

func TestNewResourceFitScoreFromProfileConfig(t *testing.T) {
	t.Run("uses default values", func(t *testing.T) {
		scorer, err := NewResourceFitScore(
			config.SchedulerProfilePluginConf{},
		)
		if err != nil {
			t.Fatalf("NewResourceFitScore returned an error: %v", err)
		}

		if scorer.Weight() != defaultResourceFitWeight {
			t.Fatalf(
				"unexpected default weight: got=%f want=%f",
				scorer.Weight(),
				defaultResourceFitWeight,
			)
		}

		if scorer.imbalancePenalty !=
			defaultResourceFitImbalancePenalty {
			t.Fatalf(
				"unexpected default imbalance penalty: got=%f want=%f",
				scorer.imbalancePenalty,
				defaultResourceFitImbalancePenalty,
			)
		}
	})

	t.Run("uses configured values", func(t *testing.T) {
		scorer, err := NewResourceFitScore(
			config.SchedulerProfilePluginConf{
				Weight: 0.8,
				Args: map[string]any{
					"imbalance_penalty": 0.25,
				},
			},
		)
		if err != nil {
			t.Fatalf("NewResourceFitScore returned an error: %v", err)
		}

		if scorer.Weight() != 0.8 {
			t.Fatalf(
				"unexpected configured weight: %f",
				scorer.Weight(),
			)
		}

		if scorer.imbalancePenalty != 0.25 {
			t.Fatalf(
				"unexpected configured imbalance penalty: %f",
				scorer.imbalancePenalty,
			)
		}
	})

	t.Run("accepts integer penalty", func(t *testing.T) {
		scorer, err := NewResourceFitScore(
			config.SchedulerProfilePluginConf{
				Args: map[string]any{
					"imbalance_penalty": 1,
				},
			},
		)
		if err != nil {
			t.Fatalf("NewResourceFitScore returned an error: %v", err)
		}

		if scorer.imbalancePenalty != 1 {
			t.Fatalf(
				"unexpected integer imbalance penalty: %f",
				scorer.imbalancePenalty,
			)
		}
	})
}

func TestNewResourceFitScoreRejectsInvalidProfileConfig(t *testing.T) {
	tests := []struct {
		name string
		conf config.SchedulerProfilePluginConf
	}{
		{
			name: "negative weight",
			conf: config.SchedulerProfilePluginConf{
				Weight: -1,
			},
		},
		{
			name: "negative imbalance penalty",
			conf: config.SchedulerProfilePluginConf{
				Args: map[string]any{
					"imbalance_penalty": -0.5,
				},
			},
		},
		{
			name: "non-numeric imbalance penalty",
			conf: config.SchedulerProfilePluginConf{
				Args: map[string]any{
					"imbalance_penalty": "high",
				},
			},
		},
		{
			name: "unknown argument",
			conf: config.SchedulerProfilePluginConf{
				Args: map[string]any{
					"imbalance_penality": 0.5,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewResourceFitScore(tt.conf)
			if err == nil {
				t.Fatal(
					"NewResourceFitScore should reject invalid configuration",
				)
			}
		})
	}
}
