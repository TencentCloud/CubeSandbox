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
