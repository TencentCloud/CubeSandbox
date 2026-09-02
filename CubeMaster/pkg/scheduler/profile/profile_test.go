// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package profile

import (
	"context"
	"io"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/expr"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

type trackingCloser struct{ closed bool }

func (c *trackingCloser) Close() error {
	c.closed = true
	return nil
}

var _ io.Closer = (*trackingCloser)(nil)

type noopScore struct{}

func (noopScore) ID() string      { return "score/go/noop_score" }
func (noopScore) Weight() float64 { return 1 }
func (noopScore) Disable() bool   { return false }
func (noopScore) Select(*selctx.SelectorCtx) (node.NodeScoreList, error) {
	return node.NodeScoreList{}, nil
}

type zeroWeightScore struct{ noopScore }

func (zeroWeightScore) ID() string      { return "score/go/zero_weight_score" }
func (zeroWeightScore) Weight() float64 { return 0 }

func profileRegistry(t *testing.T) *plugin.Registry {
	t.Helper()
	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterFilterProvider(plugin.TypeExpression, expr.NewFilter); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterScoreProvider(plugin.TypeExpression, expr.NewScore); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestProfileRoutingAndLegacyFallback(t *testing.T) {
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		PrioritySelectNum:     3,
		ProfileRouteLabelKeys: []string{"workload"},
		Profiles: []config.SchedulerProfileConf{{
			Name:      "burst",
			Route:     config.SchedulerProfileRouteConf{InstanceTypes: []string{"S.*"}, Labels: map[string]string{"workload": "burst"}},
			Scores:    []config.SchedulerProfilePluginConf{{Name: "idle", Type: "expr", Expr: "100.0 - node.cpu_util", Weight: 2}},
			Selection: config.SchedulerSelectionConf{TopN: 5, Method: "spread"},
		}},
	}}}
	set, err := Compile(context.Background(), cfg, profileRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })

	matched := set.Match(&selctx.SelectorCtx{InstanceType: "S2", RequestLabels: map[string]string{"workload": "burst"}})
	if matched.Name != "burst" || matched.TopN != 5 || len(matched.Guards) != len(mandatoryGuardNames) {
		t.Fatalf("matched pipeline = %+v", matched)
	}
	fallback := set.Match(&selctx.SelectorCtx{InstanceType: "L1"})
	if fallback.Name != "default" || fallback.TopN != 3 || len(fallback.Guards) != 0 {
		t.Fatalf("fallback pipeline = %+v", fallback)
	}
}

func TestProfileValidationRejectsUncontrolledLabelsAndUnknownPlugins(t *testing.T) {
	tests := []struct {
		name    string
		profile config.SchedulerProfileConf
	}{
		{
			name: "uncontrolled label",
			profile: config.SchedulerProfileConf{
				Name: "bad", Route: config.SchedulerProfileRouteConf{Labels: map[string]string{"tenant": "x"}},
			},
		},
		{
			name: "unknown plugin",
			profile: config.SchedulerProfileConf{
				Name: "bad", Route: config.SchedulerProfileRouteConf{InstanceTypes: []string{".*"}},
				Filters: []config.SchedulerProfilePluginConf{{Name: "missing"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{Profiles: []config.SchedulerProfileConf{test.profile}}}}
			if _, err := Compile(context.Background(), cfg, profileRegistry(t)); err == nil {
				t.Fatal("invalid profile must be rejected")
			}
		})
	}
}

func TestProfileAllowsEmptyOnlyForBuiltinScores(t *testing.T) {
	registry := profileRegistry(t)
	if err := registry.RegisterScore(plugin.TypeGo, "noop_score", func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
		return noopScore{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		Profiles: []config.SchedulerProfileConf{{
			Name: "mixed", Default: true,
			Scores: []config.SchedulerProfilePluginConf{
				{Name: "noop_score"},
				{Name: "idle", Type: "expr", Expr: "100.0 - node.cpu_util"},
			},
		}},
	}}}
	set, err := Compile(context.Background(), cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	pipeline := set.Match(&selctx.SelectorCtx{})
	if len(pipeline.Scores) != 2 {
		t.Fatalf("scores = %+v", pipeline.Scores)
	}
	if !pipeline.Scores[0].AllowEmpty {
		t.Fatal("built-in go score must allow empty (not applicable) results")
	}
	if pipeline.Scores[1].AllowEmpty {
		t.Fatal("expr score must not allow empty results")
	}
}

func TestCompileLegacySkipsInvalidWeightScorer(t *testing.T) {
	registry := profileRegistry(t)
	if err := registry.RegisterScore(plugin.TypeGo, "zero_weight_score", func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
		return zeroWeightScore{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterScore(plugin.TypeGo, "noop_score", func(context.Context, config.SchedulerProfilePluginConf) (score.Selector, error) {
		return noopScore{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		Score: &config.SchedulerScoreConf{
			EnableScorers:   []string{"zero_weight_score", "noop_score"},
			ResourceWeights: map[string]float64{"cpu": 1},
		},
	}}}
	set, err := Compile(context.Background(), cfg, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	pipeline := set.Match(&selctx.SelectorCtx{})
	if pipeline == nil || pipeline.Name != "default" {
		t.Fatalf("pipeline = %+v", pipeline)
	}
	if len(pipeline.Scores) != 1 || pipeline.Scores[0].Name != "noop_score" {
		t.Fatalf("zero-weight scorer must be skipped, scores = %+v", pipeline.Scores)
	}
}

func TestCustomDefaultDoesNotCompileUnusedLegacyPlugins(t *testing.T) {
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		Filter: &config.SchedulerFilterConf{EnableFilters: []string{"removed-legacy-plugin"}},
		Profiles: []config.SchedulerProfileConf{{
			Name: "custom-default", Default: true,
		}},
	}}}
	set, err := Compile(context.Background(), cfg, profileRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	if got := set.Match(&selctx.SelectorCtx{}); got.Name != "custom-default" {
		t.Fatalf("default profile = %q", got.Name)
	}
}

func TestProfileSetDefersCloseUntilLeaseRelease(t *testing.T) {
	closer := &trackingCloser{}
	set := &Set{fallback: &Pipeline{Name: "default"}, closers: []io.Closer{closer}}
	pipeline, release, ok := set.Acquire(&selctx.SelectorCtx{})
	if !ok || pipeline == nil || pipeline.Name != "default" {
		t.Fatalf("acquire = (%+v, %v)", pipeline, ok)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if closer.closed {
		t.Fatal("active lease must keep plugin connection open")
	}
	if _, _, ok := set.Acquire(&selctx.SelectorCtx{}); ok {
		t.Fatal("retired profile set accepted a new lease")
	}
	release()
	if !closer.closed {
		t.Fatal("last lease release must close plugin connection")
	}
}
