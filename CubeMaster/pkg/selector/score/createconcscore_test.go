// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func TestNewCreateConcurrencyScoreFromProfileConfig(t *testing.T) {
	t.Run("uses default values", func(t *testing.T) {
		scorer, err := NewCreateConcurrencyScore(
			config.SchedulerProfilePluginConf{},
		)
		if err != nil {
			t.Fatalf("NewCreateConcurrencyScore returned an error: %v", err)
		}
		if scorer.Weight() != defaultCreateConcurrencyWeight {
			t.Fatalf("unexpected default weight: %f", scorer.Weight())
		}
		if !scorer.includeLocalCreate {
			t.Fatal("include_local_create should default to true")
		}
	})

	t.Run("uses configured values", func(t *testing.T) {
		scorer, err := NewCreateConcurrencyScore(
			config.SchedulerProfilePluginConf{
				Weight: 0.6,
				Args:   map[string]any{"include_local_create": false},
			},
		)
		if err != nil {
			t.Fatalf("NewCreateConcurrencyScore returned an error: %v", err)
		}
		if scorer.Weight() != 0.6 {
			t.Fatalf("unexpected configured weight: %f", scorer.Weight())
		}
		if scorer.includeLocalCreate {
			t.Fatal("include_local_create should follow the configured value")
		}
	})
}

func TestNewCreateConcurrencyScoreRejectsInvalidProfileConfig(t *testing.T) {
	tests := []struct {
		name string
		conf config.SchedulerProfilePluginConf
	}{
		{
			name: "negative weight",
			conf: config.SchedulerProfilePluginConf{Weight: -1},
		},
		{
			name: "non-boolean include_local_create",
			conf: config.SchedulerProfilePluginConf{
				Args: map[string]any{"include_local_create": "yes"},
			},
		},
		{
			name: "unknown argument",
			conf: config.SchedulerProfilePluginConf{
				Args: map[string]any{"include_local_creat": true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCreateConcurrencyScore(tt.conf); err == nil {
				t.Fatal("NewCreateConcurrencyScore should reject invalid configuration")
			}
		})
	}
}

func TestCreateConcurrencyScoreMetadata(t *testing.T) {
	scorer, err := NewCreateConcurrencyScore(
		config.SchedulerProfilePluginConf{Weight: 0.8},
	)
	if err != nil {
		t.Fatalf("NewCreateConcurrencyScore returned an error: %v", err)
	}
	if scorer.ID() != "Score/create_concurrency_score" {
		t.Fatalf("unexpected score ID: %s", scorer.ID())
	}
	if scorer.String() != scorer.ID() {
		t.Fatalf("String should return ID: string=%s id=%s", scorer.String(), scorer.ID())
	}
	if scorer.Disable() {
		t.Fatal("create concurrency scorer should be enabled")
	}
}

// TestCreateConcurrencyScorePrefersLowerInflight 在途创建越少的节点得分越高
func TestCreateConcurrencyScorePrefersLowerInflight(t *testing.T) {
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{
		{InsID: "busy", CreateConcurrentNum: 100, RealTimeCreateNum: 80},
		{InsID: "idle", CreateConcurrentNum: 100, RealTimeCreateNum: 0},
	})

	scorer, err := NewCreateConcurrencyScore(config.SchedulerProfilePluginConf{})
	if err != nil {
		t.Fatalf("NewCreateConcurrencyScore returned an error: %v", err)
	}

	scores, err := scorer.Select(selCtx)
	if err != nil {
		t.Fatalf("Select returned an unexpected error: %v", err)
	}
	if scores.Len() != 2 {
		t.Fatalf("expected 2 node scores, got %d", scores.Len())
	}

	byNode := make(map[string]float64, scores.Len())
	for _, s := range scores {
		byNode[s.InsID] = s.Score
	}
	if byNode["idle"] <= byNode["busy"] {
		t.Fatalf("idle node should score higher: idle=%f busy=%f",
			byNode["idle"], byNode["busy"])
	}
	// Profile 流水线对 fail-closed 插件强制 [0,100]
	for id, s := range byNode {
		if s < 0 || s > 100 {
			t.Fatalf("score for %s out of range: %f", id, s)
		}
	}
}

// TestCreateConcurrencyScoreIncludesLocalCreate 本 Master 侧在途创建计入压力
func TestCreateConcurrencyScoreIncludesLocalCreate(t *testing.T) {
	busyLocal := &node.Node{InsID: "busy-local", CreateConcurrentNum: 100}
	busyLocal.LocalCreateNumIncrBy(40)

	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{
		busyLocal,
		{InsID: "quiet", CreateConcurrentNum: 100},
	})

	withLocal, err := NewCreateConcurrencyScore(config.SchedulerProfilePluginConf{})
	if err != nil {
		t.Fatalf("NewCreateConcurrencyScore returned an error: %v", err)
	}
	scores, err := withLocal.Select(selCtx)
	if err != nil {
		t.Fatalf("Select returned an unexpected error: %v", err)
	}
	byNode := make(map[string]float64, scores.Len())
	for _, s := range scores {
		byNode[s.InsID] = s.Score
	}
	if byNode["quiet"] <= byNode["busy-local"] {
		t.Fatalf("node without local in-flight creates should score higher: %+v", byNode)
	}

	// 关闭本地计数后两个节点得分一致
	withoutLocal, err := NewCreateConcurrencyScore(config.SchedulerProfilePluginConf{
		Args: map[string]any{"include_local_create": false},
	})
	if err != nil {
		t.Fatalf("NewCreateConcurrencyScore returned an error: %v", err)
	}
	scores, err = withoutLocal.Select(selCtx)
	if err != nil {
		t.Fatalf("Select returned an unexpected error: %v", err)
	}
	for _, s := range scores {
		if s.Score != 100 {
			t.Fatalf("node %s should score 100 without local create factor, got %f",
				s.InsID, s.Score)
		}
	}
}

func TestCreateConcurrencyScoreSelectSkipsWhenDisabled(t *testing.T) {
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{{InsID: "node-1", CreateConcurrentNum: 100}})

	scorer, err := NewCreateConcurrencyScore(config.SchedulerProfilePluginConf{})
	if err != nil {
		t.Fatalf("NewCreateConcurrencyScore returned an error: %v", err)
	}
	scorer.disable = true

	scores, err := scorer.Select(selCtx)
	if err != nil {
		t.Fatalf("disabled scorer returned an error: %v", err)
	}
	if scores != nil {
		t.Fatalf("disabled scorer should return nil scores, got %v", scores)
	}
}
