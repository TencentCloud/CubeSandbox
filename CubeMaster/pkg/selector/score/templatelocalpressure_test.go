// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"fmt"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func TestNewTemplateLocalPressureScoreFromProfileConfig(t *testing.T) {
	t.Run("uses default weight", func(t *testing.T) {
		scorer, err := NewTemplateLocalPressureScore(
			config.SchedulerProfilePluginConf{},
		)
		if err != nil {
			t.Fatalf("NewTemplateLocalPressureScore returned an error: %v", err)
		}
		if scorer.Weight() != defaultTemplateLocalPressureWeight {
			t.Fatalf("unexpected default weight: %f", scorer.Weight())
		}
	})

	t.Run("uses configured weight", func(t *testing.T) {
		scorer, err := NewTemplateLocalPressureScore(
			config.SchedulerProfilePluginConf{Weight: 0.4},
		)
		if err != nil {
			t.Fatalf("NewTemplateLocalPressureScore returned an error: %v", err)
		}
		if scorer.Weight() != 0.4 {
			t.Fatalf("unexpected configured weight: %f", scorer.Weight())
		}
	})

	t.Run("rejects invalid configuration", func(t *testing.T) {
		if _, err := NewTemplateLocalPressureScore(
			config.SchedulerProfilePluginConf{Weight: -1},
		); err == nil {
			t.Fatal("negative weight should be rejected")
		}
		if _, err := NewTemplateLocalPressureScore(
			config.SchedulerProfilePluginConf{
				Args: map[string]any{"pressure": 1},
			},
		); err == nil {
			t.Fatal("unknown argument should be rejected")
		}
	})
}

func TestTemplateLocalPressureScoreMetadata(t *testing.T) {
	scorer, err := NewTemplateLocalPressureScore(
		config.SchedulerProfilePluginConf{},
	)
	if err != nil {
		t.Fatalf("NewTemplateLocalPressureScore returned an error: %v", err)
	}
	if scorer.ID() != "Score/template_local_pressure" {
		t.Fatalf("unexpected score ID: %s", scorer.ID())
	}
	if scorer.String() != scorer.ID() {
		t.Fatalf("String should return ID: string=%s id=%s", scorer.String(), scorer.ID())
	}
	if scorer.Disable() {
		t.Fatal("template local pressure scorer should be enabled")
	}
}

// TestTemplateLocalPressureScorePrefersLowerSameTemplateInflight
// 同模板在途创建越少的节点得分越高，不同模板的在途创建不影响分数
func TestTemplateLocalPressureScorePrefersLowerSameTemplateInflight(t *testing.T) {
	const templateID = "tpl-pressure-test"
	t.Cleanup(func() {
		localcache.DecrNodeTemplateCreate("hot", templateID)
		localcache.DecrNodeTemplateCreate("hot", templateID)
		localcache.DecrNodeTemplateCreate("hot", templateID)
		localcache.DecrNodeTemplateCreate("hot", "tpl-other")
	})

	localcache.IncrNodeTemplateCreate("hot", templateID)
	localcache.IncrNodeTemplateCreate("hot", templateID)
	localcache.IncrNodeTemplateCreate("hot", templateID)
	// 其他模板的在途创建不计入本模板的分数
	localcache.IncrNodeTemplateCreate("hot", "tpl-other")

	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.ReqRes = &selctx.RequestResource{TemplateID: templateID}
	selCtx.SetNodes(node.NodeList{
		{InsID: "hot", CreateConcurrentNum: 100},
		{InsID: "cold", CreateConcurrentNum: 100},
	})

	scorer, err := NewTemplateLocalPressureScore(config.SchedulerProfilePluginConf{})
	if err != nil {
		t.Fatalf("NewTemplateLocalPressureScore returned an error: %v", err)
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
	if byNode["cold"] != 100 {
		t.Fatalf("node without same-template in-flight creates should score 100, got %f",
			byNode["cold"])
	}
	if byNode["hot"] >= byNode["cold"] {
		t.Fatalf("hot node should score lower: hot=%f cold=%f",
			byNode["hot"], byNode["cold"])
	}
	if byNode["hot"] < 0 {
		t.Fatalf("score out of range: %f", byNode["hot"])
	}
}

// TestTemplateLocalPressureScoreSkipsWithoutTemplate
// 请求不带模板 ID 时插件不参与打分
func TestTemplateLocalPressureScoreSkipsWithoutTemplate(t *testing.T) {
	scorer, err := NewTemplateLocalPressureScore(config.SchedulerProfilePluginConf{})
	if err != nil {
		t.Fatalf("NewTemplateLocalPressureScore returned an error: %v", err)
	}

	for _, reqRes := range []*selctx.RequestResource{
		nil,
		{TemplateID: ""},
	} {
		selCtx := selctx.New("random")
		selCtx.Ctx = context.Background()
		selCtx.ReqRes = reqRes
		selCtx.SetNodes(node.NodeList{{InsID: "node-1", CreateConcurrentNum: 100}})

		scores, err := scorer.Select(selCtx)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		if scores != nil {
			t.Fatalf("scorer should skip when template id is missing, got %v", scores)
		}
	}
}

// TestTemplateLocalPressureScoreCountsDownAfterRelease
// 计数归还后节点分数恢复，验证创建路径 +1/-1 闭环对打分可见
func TestTemplateLocalPressureScoreCountsDownAfterRelease(t *testing.T) {
	const templateID = "tpl-release-test"

	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.ReqRes = &selctx.RequestResource{TemplateID: templateID}
	selCtx.SetNodes(node.NodeList{{InsID: "node-release", CreateConcurrentNum: 100}})

	scorer, err := NewTemplateLocalPressureScore(config.SchedulerProfilePluginConf{})
	if err != nil {
		t.Fatalf("NewTemplateLocalPressureScore returned an error: %v", err)
	}

	scoreOf := func() float64 {
		scores, err := scorer.Select(selCtx)
		if err != nil {
			t.Fatalf("Select returned an unexpected error: %v", err)
		}
		if scores.Len() != 1 {
			t.Fatalf("expected 1 node score, got %d", scores.Len())
		}
		return scores[0].Score
	}

	before := scoreOf()
	localcache.IncrNodeTemplateCreate("node-release", templateID)
	during := scoreOf()
	localcache.DecrNodeTemplateCreate("node-release", templateID)
	after := scoreOf()

	if during >= before {
		t.Fatalf("score should drop while a create is in flight: before=%f during=%f",
			before, during)
	}
	if after != before {
		t.Fatalf("score should recover after release: before=%f after=%f", before, after)
	}
}

func ExampleNewTemplateLocalPressureScore() {
	scorer, _ := NewTemplateLocalPressureScore(config.SchedulerProfilePluginConf{})
	fmt.Println(scorer.ID())
	// Output: Score/template_local_pressure
}
