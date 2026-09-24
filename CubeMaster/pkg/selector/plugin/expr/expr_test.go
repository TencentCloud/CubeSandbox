// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package expr

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

func expressionContext() *selctx.SelectorCtx {
	selection := selctx.New("random")
	selection.Ctx = context.Background()
	selection.InstanceType = "small"
	selection.SetNodes(node.NodeList{
		{InsID: "n1", CpuUtil: 20, RealTimeCreateNum: 1},
		{InsID: "n2", CpuUtil: 90, RealTimeCreateNum: 9},
	})
	selection.FreezeSnapshot()
	return selection
}

func TestExpressionFilterAndScore(t *testing.T) {
	filter, err := NewFilter(context.Background(), config.SchedulerProfilePluginConf{
		Name: "idle", Expr: "node.cpu_util < 50.0 && node.creating < 4",
	})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := filter.Select(expressionContext())
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ID() != "n1" {
		t.Fatalf("kept = %v", kept)
	}

	scorer, err := NewScore(context.Background(), config.SchedulerProfilePluginConf{
		Name: "prefer-idle", Expr: "100.0 - node.cpu_util", Weight: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	scores, err := scorer.Select(expressionContext())
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0].Score != 80 || scores[1].Score != 10 {
		t.Fatalf("scores = %+v", scores)
	}
}

func TestExpressionValidation(t *testing.T) {
	if _, err := NewFilter(context.Background(), config.SchedulerProfilePluginConf{Name: "bad", Expr: "node.unknown == 1"}); err == nil {
		t.Fatal("unknown field must be rejected")
	}
	if _, err := NewFilter(context.Background(), config.SchedulerProfilePluginConf{Name: "bad", Expr: "1.0"}); err == nil {
		t.Fatal("non-bool filter must be rejected")
	}
	scorer, err := NewScore(context.Background(), config.SchedulerProfilePluginConf{Name: "bad-range", Expr: "101.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scorer.Select(expressionContext()); err == nil {
		t.Fatal("out-of-range score must be rejected")
	}
}

func TestExpressionRejectsInvalidTypedOperationAtCompileTime(t *testing.T) {
	_, err := NewScore(context.Background(), config.SchedulerProfilePluginConf{
		Name: "bad-operation", Expr: `node.cpu_util + "not-a-number"`,
	})
	if err == nil {
		t.Fatal("invalid typed operation must fail profile compilation")
	}
	_, err = NewFilter(context.Background(), config.SchedulerProfilePluginConf{
		Name: "unknown-index", Expr: `node["does_not_exist"] == 1`,
	})
	if err == nil {
		t.Fatal("unknown object index must fail profile compilation")
	}
}
