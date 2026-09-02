// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/expr"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

type testFilter struct{}

func (*testFilter) ID() string { return "filter/test" }
func (*testFilter) Select(ctx *selctx.SelectorCtx) (node.NodeList, error) {
	return ctx.Nodes(), nil
}

type testScore struct{}

func (*testScore) ID() string      { return "score/test" }
func (*testScore) Weight() float64 { return 1 }
func (*testScore) Disable() bool   { return false }
func (*testScore) Select(ctx *selctx.SelectorCtx) (node.NodeScoreList, error) {
	return nil, nil
}

func TestRegistryRejectsDuplicateAndUnknownPlugins(t *testing.T) {
	registry := NewRegistry()
	factory := func(context.Context, config.SchedulerProfilePluginConf) (filter.Selector, error) {
		return &testFilter{}, nil
	}
	if err := registry.RegisterFilter(TypeGo, "test", factory); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterFilter(TypeGo, "test", factory); !errors.Is(err, ErrDuplicateRegistration) {
		t.Fatalf("duplicate registration error = %v", err)
	}
	if _, err := registry.BuildFilter(context.Background(), config.SchedulerProfilePluginConf{Name: "missing"}); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("unknown plugin error = %v", err)
	}
	if _, err := registry.BuildFilter(context.Background(), config.SchedulerProfilePluginConf{Name: "x", Type: "wasm"}); !errors.Is(err, ErrUnknownPluginType) {
		t.Fatalf("unknown plugin type error = %v", err)
	}
}

func TestRegistryKnownTypeSpansPhases(t *testing.T) {
	registry := NewRegistry()
	scoreFactory := func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
		return &testScore{}, nil
	}
	if err := registry.RegisterScore(TypeGo, "test", scoreFactory); err != nil {
		t.Fatal(err)
	}
	// "go" is registered for scores only: a typo'd filter name must surface
	// as ErrUnknownPlugin, not the misleading ErrUnknownPluginType.
	if _, err := registry.BuildFilter(context.Background(), config.SchedulerProfilePluginConf{Name: "tset"}); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("cross-phase unknown plugin error = %v", err)
	}
	if _, err := registry.BuildScore(context.Background(), config.SchedulerProfilePluginConf{Name: "tset"}); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("same-phase unknown plugin error = %v", err)
	}
}

func TestRegistryProviderBuildsDynamicPlugin(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterFilterProvider(TypeExpression, func(_ context.Context, conf config.SchedulerProfilePluginConf) (filter.Selector, error) {
		if conf.Name != "dynamic" {
			t.Fatalf("name = %q", conf.Name)
		}
		return &testFilter{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	selector, err := registry.BuildFilter(context.Background(), config.SchedulerProfilePluginConf{Name: "dynamic", Type: TypeExpression})
	if err != nil {
		t.Fatal(err)
	}
	if selector.ID() != "filter/test" {
		t.Fatalf("selector ID = %q", selector.ID())
	}
}

// TestRegistryBuildsExprPluginsEndToEnd exercises the provider path the
// integration half relies on: register the real CEL providers, build plugins
// through BuildFilter/BuildScore, and run them against a frozen selection.
func TestRegistryBuildsExprPluginsEndToEnd(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterFilterProvider(TypeExpression, expr.NewFilter); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterScoreProvider(TypeExpression, expr.NewScore); err != nil {
		t.Fatal(err)
	}
	exprFilter, err := registry.BuildFilter(context.Background(), config.SchedulerProfilePluginConf{
		Name: "healthy_only", Type: TypeExpression, Expr: "node.healthy",
	})
	if err != nil {
		t.Fatal(err)
	}
	exprScore, err := registry.BuildScore(context.Background(), config.SchedulerProfilePluginConf{
		Name: "cpu_util", Type: TypeExpression, Expr: "node.cpu_util",
	})
	if err != nil {
		t.Fatal(err)
	}

	filterSelection := selctx.New("")
	filterSelection.SetNodes(node.NodeList{
		{InsID: "healthy", Healthy: true, CpuUtil: 10},
		{InsID: "unhealthy", Healthy: false, CpuUtil: 90},
	})
	filterSelection.FreezeSnapshot()
	kept, err := exprFilter.Select(filterSelection)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Len() != 1 || kept[0].ID() != "healthy" {
		t.Fatalf("expr filter kept %v", kept)
	}

	scoreSelection := selctx.New("")
	scoreSelection.SetNodes(node.NodeList{
		{InsID: "idle", Healthy: true, CpuUtil: 10},
		{InsID: "busy", Healthy: true, CpuUtil: 90},
	})
	scoreSelection.FreezeSnapshot()
	scored, err := exprScore.Select(scoreSelection)
	if err != nil {
		t.Fatal(err)
	}
	if scored.Len() != 2 || scored[0].Score != 10 || scored[1].Score != 90 {
		t.Fatalf("expr scores = %+v", scored)
	}
}
